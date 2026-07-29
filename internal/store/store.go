// Package store persists sync accounts, devices and records in SQLite.
//
// The server is deliberately a dumb relay: it never interprets a record's value, which is an
// opaque blob owned by the app. Its only real responsibility is deciding which of two
// competing writes for the same key wins, and handing back everything a device has not seen.
package store

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var (
	// ErrNotFound is returned when a lookup finds nothing.
	ErrNotFound = errors.New("not found")
	// ErrPairCodeInvalid covers unknown, expired and already-redeemed codes alike, so a
	// caller cannot probe for which codes exist.
	ErrPairCodeInvalid = errors.New("pairing code is invalid or has expired")
	// ErrSetupKeyInvalid covers an unknown key or one that was never set, so a caller cannot
	// probe for which accounts have one.
	ErrSetupKeyInvalid = errors.New("pairing key is invalid")
)

// MinSetupKeyLength is enforced when a key is set, not when it is redeemed: rejecting a short
// key later would just leak that a longer one exists somewhere.
const MinSetupKeyLength = 4

// Store owns the database handle.
type Store struct {
	db *sql.DB
}

// Device is one app installation attached to an account.
type Device struct {
	ID        string    `json:"id"`
	AccountID string    `json:"-"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LastSeen  time.Time `json:"lastSeen"`
}

// Record is a single synced item, identified by (account, category, key).
//
// Value is opaque. Category exists so a device can sync some kinds of data and not others,
// which is a per-device user choice.
type Record struct {
	Category string `json:"category"`
	Key      string `json:"key"`
	Value    string `json:"value"`
	// UpdatedAt is client-supplied Unix milliseconds and decides which write wins.
	UpdatedAt int64 `json:"updatedAt"`
	// Deleted marks a tombstone. Deletions have to travel like any other change,
	// otherwise a device that was offline would resurrect what another device removed.
	Deleted bool `json:"deleted"`
	// Seq is assigned by the server and is what clients page through. Never sent by clients.
	Seq int64 `json:"seq"`
	// DeviceID records the writer, used only to break ties deterministically.
	DeviceID string `json:"deviceId,omitempty"`
}

const schema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS accounts (
    id         TEXT PRIMARY KEY,
    created_at INTEGER NOT NULL,
    -- Monotonic per-account counter handed out to records, so a device can ask for
    -- "everything after seq N" without relying on wall-clock time being consistent
    -- across devices.
    seq        INTEGER NOT NULL DEFAULT 0,
    -- A persistent, user-chosen alternative to a pairing code: unlike one, it is not
    -- single-use or time-limited, so it works for adding devices whenever needed without the
    -- account's first device having to be online to issue a fresh code each time. Only the
    -- hash is stored, same reasoning as token_hash below. NULL means none is set.
    setup_key_hash TEXT
);

-- Enforces one account per key without preventing multiple accounts from having no key set:
-- a plain UNIQUE constraint on a nullable column would only ever allow one NULL row in older
-- SQLite versions, but a partial index is unambiguous about excluding NULLs entirely.
CREATE UNIQUE INDEX IF NOT EXISTS idx_accounts_setup_key
    ON accounts(setup_key_hash) WHERE setup_key_hash IS NOT NULL;

CREATE TABLE IF NOT EXISTS devices (
    id         TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    -- Only the hash is stored, so a database leak does not hand over working credentials.
    token_hash TEXT NOT NULL UNIQUE,
    created_at INTEGER NOT NULL,
    last_seen  INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_devices_account ON devices(account_id);

CREATE TABLE IF NOT EXISTS records (
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    category   TEXT NOT NULL,
    key        TEXT NOT NULL,
    value      TEXT NOT NULL,
    updated_at INTEGER NOT NULL,
    deleted    INTEGER NOT NULL DEFAULT 0,
    seq        INTEGER NOT NULL,
    device_id  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (account_id, category, key)
);

-- Pulling changes is always "for this account, ordered by seq", so index exactly that.
CREATE INDEX IF NOT EXISTS idx_records_pull ON records(account_id, seq);

CREATE TABLE IF NOT EXISTS pair_codes (
    code       TEXT PRIMARY KEY,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    expires_at INTEGER NOT NULL,
    redeemed   INTEGER NOT NULL DEFAULT 0
);

-- What a device is playing right now, if anything - not a synced record, deliberately not
-- part of the records table above: this is live, ephemeral status, not history a device
-- should ever page through or need to resolve a conflict on. One row per device, always
-- overwritten in place, and expected to go stale within a couple of minutes of that device
-- closing or losing network, which GetPresence's freshness window (not this schema) is what
-- enforces.
CREATE TABLE IF NOT EXISTS presence (
    device_id  TEXT PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
    account_id TEXT NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    status     TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_presence_account ON presence(account_id);
`

// Open opens the database at path and applies the schema.
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	// SQLite handles one writer at a time. Letting the pool open several connections
	// converts that into SQLITE_BUSY errors under concurrent writes rather than queuing,
	// so the writer is serialised here instead.
	db.SetMaxOpenConns(1)

	// CREATE TABLE IF NOT EXISTS does not add columns to a table that already exists, so a
	// database from before setup_key_hash existed needs it added explicitly before the index
	// on it (in schema below) can be created.
	if err := addSetupKeyColumnIfMissing(db); err != nil {
		return nil, fmt.Errorf("migrate schema: %w", err)
	}

	if _, err := db.Exec(schema); err != nil {
		return nil, fmt.Errorf("apply schema: %w", err)
	}
	return &Store{db: db}, nil
}

func addSetupKeyColumnIfMissing(db *sql.DB) error {
	// PRAGMA table_info on a table that does not exist yet returns zero rows rather than an
	// error, so tableExists tracks that case explicitly: a brand new database has no accounts
	// table at all, and schema below creates one with the column already in place, so there
	// is nothing to migrate.
	rows, err := db.Query(`PRAGMA table_info(accounts)`)
	if err != nil {
		return err
	}
	defer rows.Close()

	tableExists := false
	hasColumn := false
	for rows.Next() {
		tableExists = true
		var (
			cid, notNull, pk int
			name, colType    string
			dfltValue        sql.NullString
		)
		if err := rows.Scan(&cid, &name, &colType, &notNull, &dfltValue, &pk); err != nil {
			return err
		}
		if name == "setup_key_hash" {
			hasColumn = true
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}

	if !tableExists || hasColumn {
		return nil
	}

	_, err = db.Exec(`ALTER TABLE accounts ADD COLUMN setup_key_hash TEXT`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func newID() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func newToken() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// CreateAccount creates an account together with its first device.
//
// The returned token is the only time it exists in plaintext; only its hash is stored.
func (s *Store) CreateAccount(ctx context.Context, deviceName string) (accountID string, device Device, token string, err error) {
	accountID, err = newID()
	if err != nil {
		return "", Device{}, "", err
	}

	now := time.Now()
	if _, err = s.db.ExecContext(ctx,
		`INSERT INTO accounts (id, created_at, seq) VALUES (?, ?, 0)`,
		accountID, now.UnixMilli()); err != nil {
		return "", Device{}, "", fmt.Errorf("create account: %w", err)
	}

	device, token, err = s.AddDevice(ctx, accountID, deviceName)
	if err != nil {
		return "", Device{}, "", err
	}
	return accountID, device, token, nil
}

// AddDevice attaches a new device to an account and returns its token.
func (s *Store) AddDevice(ctx context.Context, accountID, name string) (Device, string, error) {
	id, err := newID()
	if err != nil {
		return Device{}, "", err
	}
	token, err := newToken()
	if err != nil {
		return Device{}, "", err
	}

	if strings.TrimSpace(name) == "" {
		name = "Device"
	}

	now := time.Now()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO devices (id, account_id, name, token_hash, created_at, last_seen)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		id, accountID, name, hashToken(token), now.UnixMilli(), now.UnixMilli()); err != nil {
		return Device{}, "", fmt.Errorf("add device: %w", err)
	}

	return Device{
		ID:        id,
		AccountID: accountID,
		Name:      name,
		CreatedAt: now,
		LastSeen:  now,
	}, token, nil
}

// DeviceByToken resolves a bearer token and refreshes the device's last-seen time.
func (s *Store) DeviceByToken(ctx context.Context, token string) (Device, error) {
	var (
		d                   Device
		createdAt, lastSeen int64
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, account_id, name, created_at, last_seen FROM devices WHERE token_hash = ?`,
		hashToken(token)).Scan(&d.ID, &d.AccountID, &d.Name, &createdAt, &lastSeen)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, ErrNotFound
	}
	if err != nil {
		return Device{}, err
	}
	d.CreatedAt = time.UnixMilli(createdAt)
	d.LastSeen = time.UnixMilli(lastSeen)

	// Best effort; a failure to record activity must not fail the request.
	_, _ = s.db.ExecContext(ctx,
		`UPDATE devices SET last_seen = ? WHERE id = ?`, time.Now().UnixMilli(), d.ID)

	return d, nil
}

// ListDevices returns every device on an account, newest first.
func (s *Store) ListDevices(ctx context.Context, accountID string) ([]Device, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, created_at, last_seen FROM devices
		 WHERE account_id = ? ORDER BY created_at DESC`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Device
	for rows.Next() {
		var (
			d                   Device
			createdAt, lastSeen int64
		)
		if err := rows.Scan(&d.ID, &d.Name, &createdAt, &lastSeen); err != nil {
			return nil, err
		}
		d.AccountID = accountID
		d.CreatedAt = time.UnixMilli(createdAt)
		d.LastSeen = time.UnixMilli(lastSeen)
		out = append(out, d)
	}
	return out, rows.Err()
}

// RemoveDevice revokes a device's access. Scoped to the account so a token cannot be used
// to remove devices belonging to somebody else.
func (s *Store) RemoveDevice(ctx context.Context, accountID, deviceID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM devices WHERE id = ? AND account_id = ?`, deviceID, accountID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// PresenceFreshness is how long ago a device's presence must have been set for GetPresence to
// still return it. A device that closed, backgrounded the player, or lost its network
// connection stops updating this, and without a window like this it would look like it was
// still playing whatever it last reported forever.
const PresenceFreshness = 90 * time.Second

// DevicePresence is what another device on the account is doing right now, if anything.
type DevicePresence struct {
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	// Status is opaque, the same way a Record's Value is: whatever the app wants to report
	// (title, position, whether it is actually playing or paused), which this server never
	// parses.
	Status    string `json:"status"`
	UpdatedAt int64  `json:"updatedAt"`
}

// SetPresence records what a device is playing right now, overwriting whatever it last
// reported. There is no history here, unlike records - only ever the current status.
func (s *Store) SetPresence(ctx context.Context, accountID, deviceID, status string) error {
	now := time.Now().UnixMilli()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO presence (device_id, account_id, status, updated_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(device_id) DO UPDATE SET status = excluded.status, updated_at = excluded.updated_at`,
		deviceID, accountID, status, now)
	return err
}

// ClearPresence removes a device's status, e.g. when it stops playing. Distinct from an
// empty-string SetPresence so "explicitly stopped" and "reported an empty status string" are
// not the same on-disk state, even though GetPresence's caller sees them identically.
func (s *Store) ClearPresence(ctx context.Context, deviceID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM presence WHERE device_id = ?`, deviceID)
	return err
}

// GetPresence returns what every other device on the account is playing right now, excluding
// the caller's own and anything older than PresenceFreshness.
func (s *Store) GetPresence(ctx context.Context, accountID, excludeDeviceID string) ([]DevicePresence, error) {
	cutoff := time.Now().Add(-PresenceFreshness).UnixMilli()
	rows, err := s.db.QueryContext(ctx,
		`SELECT presence.device_id, devices.name, presence.status, presence.updated_at
		 FROM presence
		 JOIN devices ON devices.id = presence.device_id
		 WHERE presence.account_id = ? AND presence.device_id != ? AND presence.updated_at >= ?
		 ORDER BY presence.updated_at DESC`,
		accountID, excludeDeviceID, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]DevicePresence, 0, 4)
	for rows.Next() {
		var p DevicePresence
		if err := rows.Scan(&p.DeviceID, &p.DeviceName, &p.Status, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreatePairCode issues a short-lived code that another device can redeem to join.
//
// Codes are short enough to read off a TV screen and type on a phone, which means they are
// guessable by brute force; the short lifetime and single use are what make that acceptable.
// Four characters matches the persistent setup key's own minimum length - both are meant to be
// typed once on a second device, not memorised, so there is no reason for one to be harder to
// get right than the other.
func (s *Store) CreatePairCode(ctx context.Context, accountID string, ttl time.Duration) (string, time.Time, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // no I/O/0/1, which are misread
	const codeLength = 4
	buf := make([]byte, codeLength)
	if _, err := rand.Read(buf); err != nil {
		return "", time.Time{}, err
	}
	code := make([]byte, codeLength)
	for i, b := range buf {
		code[i] = alphabet[int(b)%len(alphabet)]
	}

	expires := time.Now().Add(ttl)
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO pair_codes (code, account_id, expires_at, redeemed) VALUES (?, ?, ?, 0)`,
		string(code), accountID, expires.UnixMilli()); err != nil {
		return "", time.Time{}, fmt.Errorf("create pair code: %w", err)
	}
	return string(code), expires, nil
}

// RedeemPairCode consumes a code and creates a device on its account.
func (s *Store) RedeemPairCode(ctx context.Context, code, deviceName string) (Device, string, error) {
	code = strings.ToUpper(strings.TrimSpace(code))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Device{}, "", err
	}
	defer func() { _ = tx.Rollback() }()

	var (
		accountID string
		expiresAt int64
		redeemed  int
	)
	err = tx.QueryRowContext(ctx,
		`SELECT account_id, expires_at, redeemed FROM pair_codes WHERE code = ?`, code).
		Scan(&accountID, &expiresAt, &redeemed)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, "", ErrPairCodeInvalid
	}
	if err != nil {
		return Device{}, "", err
	}
	if redeemed != 0 || time.Now().UnixMilli() > expiresAt {
		return Device{}, "", ErrPairCodeInvalid
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE pair_codes SET redeemed = 1 WHERE code = ?`, code); err != nil {
		return Device{}, "", err
	}
	if err := tx.Commit(); err != nil {
		return Device{}, "", err
	}

	device, token, err := s.AddDevice(ctx, accountID, deviceName)
	if err != nil {
		return Device{}, "", err
	}
	return device, token, nil
}

// SetSetupKey sets or replaces the account's persistent pairing key. Overwriting an existing
// key immediately invalidates it, since only the hash of the newest one is kept - there is no
// way to have two valid keys at once.
func (s *Store) SetSetupKey(ctx context.Context, accountID, key string) error {
	if len(key) < MinSetupKeyLength {
		return fmt.Errorf("setup key must be at least %d characters", MinSetupKeyLength)
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET setup_key_hash = ? WHERE id = ?`, hashToken(key), accountID)
	if err != nil {
		return fmt.Errorf("set setup key: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ClearSetupKey disables the account's persistent pairing key. Existing devices are
// unaffected; only future redemptions of the key stop working.
func (s *Store) ClearSetupKey(ctx context.Context, accountID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE accounts SET setup_key_hash = NULL WHERE id = ?`, accountID)
	return err
}

// RedeemSetupKey creates a new device on whichever account the key belongs to.
//
// Unlike a pairing code, redeeming does not consume the key: it keeps working, for any number
// of devices, until the account holder changes or clears it. That is the point of it over a
// code - joining a device does not require the first device to be online to issue one.
func (s *Store) RedeemSetupKey(ctx context.Context, key, deviceName string) (Device, string, error) {
	var accountID string
	err := s.db.QueryRowContext(ctx,
		`SELECT id FROM accounts WHERE setup_key_hash = ?`, hashToken(key)).Scan(&accountID)
	if errors.Is(err, sql.ErrNoRows) {
		return Device{}, "", ErrSetupKeyInvalid
	}
	if err != nil {
		return Device{}, "", err
	}
	return s.AddDevice(ctx, accountID, deviceName)
}

// PurgeExpiredPairCodes drops codes that can no longer be redeemed.
func (s *Store) PurgeExpiredPairCodes(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM pair_codes WHERE redeemed = 1 OR expires_at < ?`, time.Now().UnixMilli())
	return err
}

// PutRecords merges records into an account, last write wins per key.
//
// A record is only stored when its UpdatedAt is newer than what is already held. Equal
// timestamps are broken by comparing device ids, which is arbitrary but consistent, so two
// devices that write the same key in the same millisecond still converge on the same value
// rather than flapping.
//
// Returns the account's sequence number after the merge.
func (s *Store) PutRecords(ctx context.Context, accountID, deviceID string, records []Record) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	var seq int64
	if err := tx.QueryRowContext(ctx,
		`SELECT seq FROM accounts WHERE id = ?`, accountID).Scan(&seq); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, ErrNotFound
		}
		return 0, err
	}

	for _, r := range records {
		if r.Category == "" || r.Key == "" {
			continue
		}

		var (
			existingUpdated int64
			existingDevice  string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT updated_at, device_id FROM records
			 WHERE account_id = ? AND category = ? AND key = ?`,
			accountID, r.Category, r.Key).Scan(&existingUpdated, &existingDevice)

		switch {
		case errors.Is(err, sql.ErrNoRows):
			// New key, always accepted.
		case err != nil:
			return 0, err
		default:
			if r.UpdatedAt < existingUpdated {
				continue
			}
			if r.UpdatedAt == existingUpdated && deviceID <= existingDevice {
				continue
			}
		}

		seq++
		deleted := 0
		if r.Deleted {
			deleted = 1
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO records (account_id, category, key, value, updated_at, deleted, seq, device_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(account_id, category, key) DO UPDATE SET
			     value = excluded.value,
			     updated_at = excluded.updated_at,
			     deleted = excluded.deleted,
			     seq = excluded.seq,
			     device_id = excluded.device_id`,
			accountID, r.Category, r.Key, r.Value, r.UpdatedAt, deleted, seq, deviceID); err != nil {
			return 0, err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE accounts SET seq = ? WHERE id = ?`, seq, accountID); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// GetRecords returns records with seq greater than since, oldest first.
//
// categories filters to the kinds of data this device syncs; empty means everything. limit
// bounds the page so a first sync on a large library cannot produce an unbounded response.
func (s *Store) GetRecords(ctx context.Context, accountID string, since int64, categories []string, limit int) ([]Record, int64, error) {
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}

	query := `SELECT category, key, value, updated_at, deleted, seq, device_id
	          FROM records WHERE account_id = ? AND seq > ?`
	args := []any{accountID, since}

	if len(categories) > 0 {
		query += " AND category IN (" + strings.TrimSuffix(strings.Repeat("?,", len(categories)), ",") + ")"
		for _, c := range categories {
			args = append(args, c)
		}
	}

	query += " ORDER BY seq ASC LIMIT ?"
	args = append(args, limit)

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := make([]Record, 0, 16)
	cursor := since
	for rows.Next() {
		var (
			r       Record
			deleted int
		)
		if err := rows.Scan(&r.Category, &r.Key, &r.Value, &r.UpdatedAt, &deleted, &r.Seq, &r.DeviceID); err != nil {
			return nil, 0, err
		}
		r.Deleted = deleted != 0
		out = append(out, r)
		cursor = r.Seq
	}
	return out, cursor, rows.Err()
}

// AccountSeq returns the current sequence number, which a client can use as a starting
// cursor when it wants only future changes.
func (s *Store) AccountSeq(ctx context.Context, accountID string) (int64, error) {
	var seq int64
	err := s.db.QueryRowContext(ctx, `SELECT seq FROM accounts WHERE id = ?`, accountID).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return seq, err
}
