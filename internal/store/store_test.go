package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestCreateAccountAndAuthenticate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, device, token, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if accountID == "" || device.ID == "" || token == "" {
		t.Fatal("expected account, device and token to be populated")
	}

	got, err := st.DeviceByToken(ctx, token)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if got.ID != device.ID || got.AccountID != accountID {
		t.Fatalf("resolved wrong device: %+v", got)
	}

	if _, err := st.DeviceByToken(ctx, "not-a-real-token"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound for a bogus token, got %v", err)
	}
}

// The token must not be recoverable from the database, only its hash.
func TestTokenIsStoredHashed(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, _, token, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	var stored string
	if err := st.db.QueryRow(`SELECT token_hash FROM devices LIMIT 1`).Scan(&stored); err != nil {
		t.Fatalf("read token hash: %v", err)
	}
	if stored == token {
		t.Fatal("token was stored in plaintext")
	}
	if stored != hashToken(token) {
		t.Fatal("stored value is not the token hash")
	}
}

func TestPairingFlow(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "TV")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	code, expires, err := st.CreatePairCode(ctx, accountID, time.Minute)
	if err != nil {
		t.Fatalf("create pair code: %v", err)
	}
	if time.Until(expires) <= 0 {
		t.Fatal("pairing code expired immediately")
	}

	device, token, err := st.RedeemPairCode(ctx, code, "Phone")
	if err != nil {
		t.Fatalf("redeem: %v", err)
	}
	if device.AccountID != accountID {
		t.Fatalf("paired device joined the wrong account: %s", device.AccountID)
	}
	if token == "" {
		t.Fatal("expected a token for the paired device")
	}

	// Single use: a leaked code must not keep working.
	if _, _, err := st.RedeemPairCode(ctx, code, "Attacker"); err != ErrPairCodeInvalid {
		t.Fatalf("expected reuse to be rejected, got %v", err)
	}
}

func TestExpiredPairCodeIsRejected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "TV")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	code, _, err := st.CreatePairCode(ctx, accountID, -time.Second)
	if err != nil {
		t.Fatalf("create pair code: %v", err)
	}
	if _, _, err := st.RedeemPairCode(ctx, code, "Phone"); err != ErrPairCodeInvalid {
		t.Fatalf("expected expired code to be rejected, got %v", err)
	}
}

// The core guarantee: the newest write for a key wins, regardless of arrival order.
func TestLastWriteWins(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, phone, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, _, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if _, err := st.PutRecords(ctx, accountID, phone.ID, []Record{
		{Category: "resume", Key: "show/1", Value: `{"pos":100}`, UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put from phone: %v", err)
	}

	// Newer write from another device replaces it.
	if _, err := st.PutRecords(ctx, accountID, tv.ID, []Record{
		{Category: "resume", Key: "show/1", Value: `{"pos":500}`, UpdatedAt: 2000},
	}); err != nil {
		t.Fatalf("put from tv: %v", err)
	}

	records, _, err := st.GetRecords(ctx, accountID, 0, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected the key to be merged, got %d records", len(records))
	}
	if records[0].Value != `{"pos":500}` {
		t.Fatalf("newer write did not win, got %s", records[0].Value)
	}

	// A late-arriving older write must not clobber the newer value. This is the case that
	// causes real data loss when a device syncs after being offline.
	if _, err := st.PutRecords(ctx, accountID, phone.ID, []Record{
		{Category: "resume", Key: "show/1", Value: `{"pos":50}`, UpdatedAt: 500},
	}); err != nil {
		t.Fatalf("put stale from phone: %v", err)
	}

	records, _, err = st.GetRecords(ctx, accountID, 0, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if records[0].Value != `{"pos":500}` {
		t.Fatalf("stale write overwrote a newer value, got %s", records[0].Value)
	}
}

// Two devices writing in the same millisecond must land on the same answer rather than
// flapping between values on every sync.
func TestEqualTimestampsResolveDeterministically(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, a, _, err := st.CreateAccount(ctx, "A")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	b, _, err := st.AddDevice(ctx, accountID, "B")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	first, second := a.ID, b.ID
	if first > second {
		first, second = second, first
	}

	if _, err := st.PutRecords(ctx, accountID, second, []Record{
		{Category: "c", Key: "k", Value: "higher-id", UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	// Same timestamp from the lower device id must not displace it.
	if _, err := st.PutRecords(ctx, accountID, first, []Record{
		{Category: "c", Key: "k", Value: "lower-id", UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	records, _, err := st.GetRecords(ctx, accountID, 0, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if records[0].Value != "higher-id" {
		t.Fatalf("tie-break was not deterministic, got %s", records[0].Value)
	}
}

// Deletions have to travel, otherwise an offline device resurrects removed items.
func TestDeletionsPropagate(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, device, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	if _, err := st.PutRecords(ctx, accountID, device.ID, []Record{
		{Category: "bookmarks", Key: "b/1", Value: "{}", UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}
	if _, err := st.PutRecords(ctx, accountID, device.ID, []Record{
		{Category: "bookmarks", Key: "b/1", Value: "", UpdatedAt: 2000, Deleted: true},
	}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	records, _, err := st.GetRecords(ctx, accountID, 0, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(records) != 1 || !records[0].Deleted {
		t.Fatalf("expected a tombstone, got %+v", records)
	}
}

func TestIncrementalPullByCursor(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, device, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	if _, err := st.PutRecords(ctx, accountID, device.ID, []Record{
		{Category: "c", Key: "k1", Value: "1", UpdatedAt: 1000},
		{Category: "c", Key: "k2", Value: "2", UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	_, cursor, err := st.GetRecords(ctx, accountID, 0, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}

	// Nothing new since the cursor.
	records, _, err := st.GetRecords(ctx, accountID, cursor, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("expected no changes after the cursor, got %d", len(records))
	}

	if _, err := st.PutRecords(ctx, accountID, device.ID, []Record{
		{Category: "c", Key: "k3", Value: "3", UpdatedAt: 2000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	records, _, err = st.GetRecords(ctx, accountID, cursor, nil, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(records) != 1 || records[0].Key != "k3" {
		t.Fatalf("expected only the new record, got %+v", records)
	}
}

// A device that syncs only some categories must not receive the others.
func TestCategoryFilter(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, device, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}

	if _, err := st.PutRecords(ctx, accountID, device.ID, []Record{
		{Category: "resume", Key: "k1", Value: "1", UpdatedAt: 1000},
		{Category: "repos", Key: "k2", Value: "2", UpdatedAt: 1000},
	}); err != nil {
		t.Fatalf("put: %v", err)
	}

	records, _, err := st.GetRecords(ctx, accountID, 0, []string{"repos"}, 100)
	if err != nil {
		t.Fatalf("get records: %v", err)
	}
	if len(records) != 1 || records[0].Category != "repos" {
		t.Fatalf("category filter leaked other data: %+v", records)
	}
}

// Revoking a device must immediately stop its token from working.
func TestRemoveDeviceRevokesToken(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, tvToken, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.RemoveDevice(ctx, accountID, tv.ID); err != nil {
		t.Fatalf("remove device: %v", err)
	}
	if _, err := st.DeviceByToken(ctx, tvToken); err != ErrNotFound {
		t.Fatalf("revoked token still works, got %v", err)
	}
}

// The core guarantee of a setup key over a pairing code: it is not consumed by use, so any
// number of devices can join with it.
func TestSetupKeyIsNotSingleUse(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountID, "correct horse battery"); err != nil {
		t.Fatalf("set setup key: %v", err)
	}

	tv, _, err := st.RedeemSetupKey(ctx, "correct horse battery", "TV")
	if err != nil {
		t.Fatalf("redeem for TV: %v", err)
	}
	if tv.AccountID != accountID {
		t.Fatalf("TV joined the wrong account: %s", tv.AccountID)
	}

	// Redeeming again with the same key must still work.
	tablet, _, err := st.RedeemSetupKey(ctx, "correct horse battery", "Tablet")
	if err != nil {
		t.Fatalf("redeem for Tablet: %v", err)
	}
	if tablet.AccountID != accountID {
		t.Fatalf("Tablet joined the wrong account: %s", tablet.AccountID)
	}
}

func TestSetupKeyRejectsShortKeys(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountID, "abc"); err == nil {
		t.Fatal("expected a key shorter than MinSetupKeyLength to be rejected")
	}
}

// Setting a new key must invalidate the old one - there is only ever one valid key.
func TestSetupKeyChangeInvalidatesThePrevious(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountID, "first-key-value"); err != nil {
		t.Fatalf("set first key: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountID, "second-key-value"); err != nil {
		t.Fatalf("set second key: %v", err)
	}

	if _, _, err := st.RedeemSetupKey(ctx, "first-key-value", "TV"); err != ErrSetupKeyInvalid {
		t.Fatalf("expected the replaced key to be rejected, got %v", err)
	}
	if _, _, err := st.RedeemSetupKey(ctx, "second-key-value", "TV"); err != nil {
		t.Fatalf("expected the new key to work, got %v", err)
	}
}

func TestClearSetupKeyDisablesRedemption(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, _, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountID, "a-perfectly-good-key"); err != nil {
		t.Fatalf("set setup key: %v", err)
	}
	if err := st.ClearSetupKey(ctx, accountID); err != nil {
		t.Fatalf("clear setup key: %v", err)
	}

	if _, _, err := st.RedeemSetupKey(ctx, "a-perfectly-good-key", "TV"); err != ErrSetupKeyInvalid {
		t.Fatalf("expected a cleared key to be rejected, got %v", err)
	}
}

func TestRedeemUnknownSetupKeyRejected(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	if _, _, err := st.RedeemSetupKey(ctx, "never-set-by-anyone", "TV"); err != ErrSetupKeyInvalid {
		t.Fatalf("expected an unknown key to be rejected, got %v", err)
	}
}

// Two accounts each setting their own key must not collide or leak into each other.
func TestSetupKeyIsScopedToItsAccount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountA, _, _, err := st.CreateAccount(ctx, "A-Phone")
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accountB, _, _, err := st.CreateAccount(ctx, "B-Phone")
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountA, "account-a-key-value"); err != nil {
		t.Fatalf("set key A: %v", err)
	}
	if err := st.SetSetupKey(ctx, accountB, "account-b-key-value"); err != nil {
		t.Fatalf("set key B: %v", err)
	}

	device, _, err := st.RedeemSetupKey(ctx, "account-b-key-value", "New Device")
	if err != nil {
		t.Fatalf("redeem key B: %v", err)
	}
	if device.AccountID != accountB {
		t.Fatalf("key B joined the wrong account: %s", device.AccountID)
	}
}

func TestPresenceSetAndGet(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, phone, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, _, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.SetPresence(ctx, accountID, phone.ID, `{"title":"Show","playing":true}`); err != nil {
		t.Fatalf("set presence: %v", err)
	}

	// The TV should see the phone playing something.
	fromTV, err := st.GetPresence(ctx, accountID, tv.ID)
	if err != nil {
		t.Fatalf("get presence from TV: %v", err)
	}
	if len(fromTV) != 1 || fromTV[0].DeviceID != phone.ID || fromTV[0].Status != `{"title":"Show","playing":true}` {
		t.Fatalf("expected TV to see the phone's presence, got %+v", fromTV)
	}

	// A device does not see its own presence in its own list.
	fromPhone, err := st.GetPresence(ctx, accountID, phone.ID)
	if err != nil {
		t.Fatalf("get presence from phone: %v", err)
	}
	if len(fromPhone) != 0 {
		t.Fatalf("expected a device to be excluded from its own presence list, got %+v", fromPhone)
	}
}

func TestPresenceUpsertOverwritesRatherThanDuplicating(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, phone, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, _, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.SetPresence(ctx, accountID, phone.ID, "first"); err != nil {
		t.Fatalf("set presence: %v", err)
	}
	if err := st.SetPresence(ctx, accountID, phone.ID, "second"); err != nil {
		t.Fatalf("set presence again: %v", err)
	}

	got, err := st.GetPresence(ctx, accountID, tv.ID)
	if err != nil {
		t.Fatalf("get presence: %v", err)
	}
	if len(got) != 1 || got[0].Status != "second" {
		t.Fatalf("expected exactly one row with the latest status, got %+v", got)
	}
}

func TestPresenceClear(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, phone, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, _, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.SetPresence(ctx, accountID, phone.ID, "playing"); err != nil {
		t.Fatalf("set presence: %v", err)
	}
	if err := st.ClearPresence(ctx, phone.ID); err != nil {
		t.Fatalf("clear presence: %v", err)
	}

	got, err := st.GetPresence(ctx, accountID, tv.ID)
	if err != nil {
		t.Fatalf("get presence: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no presence after clearing, got %+v", got)
	}
}

// A device that stopped updating its presence (closed, backgrounded, lost network) must not
// look like it is still playing something indefinitely.
func TestPresenceExcludesStaleEntries(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountID, phone, _, err := st.CreateAccount(ctx, "Phone")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	tv, _, err := st.AddDevice(ctx, accountID, "TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	staleTime := time.Now().Add(-2 * PresenceFreshness).UnixMilli()
	if _, err := st.db.ExecContext(ctx,
		`INSERT INTO presence (device_id, account_id, status, updated_at) VALUES (?, ?, ?, ?)`,
		phone.ID, accountID, "old", staleTime); err != nil {
		t.Fatalf("insert stale presence: %v", err)
	}

	got, err := st.GetPresence(ctx, accountID, tv.ID)
	if err != nil {
		t.Fatalf("get presence: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected a stale presence row to be excluded, got %+v", got)
	}
}

func TestPresenceIsScopedToItsAccount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	accountA, phoneA, _, err := st.CreateAccount(ctx, "A-Phone")
	if err != nil {
		t.Fatalf("create account A: %v", err)
	}
	accountB, _, _, err := st.CreateAccount(ctx, "B-Phone")
	if err != nil {
		t.Fatalf("create account B: %v", err)
	}
	bOther, _, err := st.AddDevice(ctx, accountB, "B-TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.SetPresence(ctx, accountA, phoneA.ID, "playing"); err != nil {
		t.Fatalf("set presence: %v", err)
	}

	got, err := st.GetPresence(ctx, accountB, bOther.ID)
	if err != nil {
		t.Fatalf("get presence: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("account B must not see account A's presence, got %+v", got)
	}
}

// A token from one account must not be able to touch another account's devices.
func TestRemoveDeviceIsScopedToAccount(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)

	_, mine, _, err := st.CreateAccount(ctx, "Mine")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	otherAccount, _, _, err := st.CreateAccount(ctx, "Theirs")
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	theirDevice, _, err := st.AddDevice(ctx, otherAccount, "Their TV")
	if err != nil {
		t.Fatalf("add device: %v", err)
	}

	if err := st.RemoveDevice(ctx, mine.AccountID, theirDevice.ID); err != ErrNotFound {
		t.Fatalf("cross-account removal was allowed, got %v", err)
	}
}
