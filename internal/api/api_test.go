package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/F-e-n-y-x/cloudstream-sync/internal/store"
)

func newTestServer(t *testing.T, openRegistration bool) (*httptest.Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(New(st, log, openRegistration).Routes())
	t.Cleanup(func() {
		srv.Close()
		_ = st.Close()
	})
	return srv, st
}

func do(t *testing.T, method, url, token string, body any) (*http.Response, map[string]any) {
	t.Helper()

	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	}

	req, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })

	decoded := map[string]any{}
	raw, _ := io.ReadAll(resp.Body)
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &decoded)
	}
	return resp, decoded
}

func createAccount(t *testing.T, srv *httptest.Server) (accountID, token string) {
	t.Helper()
	resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/account", "",
		map[string]string{"deviceName": "Phone"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create account: status %d body %v", resp.StatusCode, body)
	}
	return body["accountId"].(string), body["token"].(string)
}

func TestHealth(t *testing.T) {
	srv, _ := newTestServer(t, false)
	resp, body := do(t, http.MethodGet, srv.URL+"/healthz", "", nil)
	if resp.StatusCode != http.StatusOK || body["status"] != "ok" {
		t.Fatalf("unexpected health response: %d %v", resp.StatusCode, body)
	}
}

// Registration is closed by default, so a stranger who finds the URL cannot use the server.
func TestRegistrationClosedByDefault(t *testing.T) {
	srv, _ := newTestServer(t, false)
	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/account", "",
		map[string]string{"deviceName": "Phone"})
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 with registration closed, got %d", resp.StatusCode)
	}
}

func TestAuthenticationRequired(t *testing.T) {
	srv, _ := newTestServer(t, true)

	for _, path := range []string{"/api/v1/records", "/api/v1/devices", "/api/v1/status"} {
		resp, _ := do(t, http.MethodGet, srv.URL+path, "", nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("%s without a token returned %d, want 401", path, resp.StatusCode)
		}
	}

	resp, _ := do(t, http.MethodGet, srv.URL+"/api/v1/records", "bogus-token", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("bogus token returned %d, want 401", resp.StatusCode)
	}
}

func TestPairAndSyncBetweenDevices(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	// The phone issues a code; the TV redeems it.
	resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/pair", phoneToken, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create pair code: %d %v", resp.StatusCode, body)
	}
	code := body["code"].(string)

	resp, body = do(t, http.MethodPost, srv.URL+"/api/v1/pair/redeem", "",
		map[string]string{"code": code, "deviceName": "TV"})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem: %d %v", resp.StatusCode, body)
	}
	tvToken := body["token"].(string)

	// The phone pushes a watch position.
	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/records", phoneToken, map[string]any{
		"records": []map[string]any{
			{"category": "resume", "key": "show/1", "value": `{"pos":120}`, "updatedAt": 1000},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: %d", resp.StatusCode)
	}

	// The TV sees it.
	resp, body = do(t, http.MethodGet, srv.URL+"/api/v1/records?since=0", tvToken, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("pull: %d %v", resp.StatusCode, body)
	}
	records := body["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("expected the phone's record on the TV, got %v", records)
	}
	first := records[0].(map[string]any)
	if first["key"] != "show/1" || first["value"] != `{"pos":120}` {
		t.Fatalf("unexpected record: %v", first)
	}
}

// Two accounts must never see each other's data.
func TestAccountsAreIsolated(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, tokenA := createAccount(t, srv)
	_, tokenB := createAccount(t, srv)

	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/records", tokenA, map[string]any{
		"records": []map[string]any{
			{"category": "resume", "key": "secret", "value": "mine", "updatedAt": 1000},
		},
	})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("push: %d", resp.StatusCode)
	}

	_, body := do(t, http.MethodGet, srv.URL+"/api/v1/records?since=0", tokenB, nil)
	if records, ok := body["records"].([]any); ok && len(records) != 0 {
		t.Fatalf("account B can see account A's records: %v", records)
	}
}

func TestCategoryFilterOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, token := createAccount(t, srv)

	do(t, http.MethodPost, srv.URL+"/api/v1/records", token, map[string]any{
		"records": []map[string]any{
			{"category": "resume", "key": "k1", "value": "1", "updatedAt": 1000},
			{"category": "repos", "key": "k2", "value": "2", "updatedAt": 1000},
		},
	})

	_, body := do(t, http.MethodGet, srv.URL+"/api/v1/records?since=0&category=repos", token, nil)
	records := body["records"].([]any)
	if len(records) != 1 {
		t.Fatalf("expected only repos, got %v", records)
	}
	if records[0].(map[string]any)["category"] != "repos" {
		t.Fatalf("wrong category returned: %v", records[0])
	}
}

func TestDeviceListAndRevocation(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	_, body := do(t, http.MethodPost, srv.URL+"/api/v1/pair", phoneToken, nil)
	code := body["code"].(string)
	_, body = do(t, http.MethodPost, srv.URL+"/api/v1/pair/redeem", "",
		map[string]string{"code": code, "deviceName": "TV"})
	tvToken := body["token"].(string)
	tvID := body["deviceId"].(string)

	_, body = do(t, http.MethodGet, srv.URL+"/api/v1/devices", phoneToken, nil)
	if devices := body["devices"].([]any); len(devices) != 2 {
		t.Fatalf("expected two devices, got %v", devices)
	}

	resp, _ := do(t, http.MethodDelete, srv.URL+"/api/v1/devices/"+tvID, phoneToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("revoke: %d", resp.StatusCode)
	}

	resp, _ = do(t, http.MethodGet, srv.URL+"/api/v1/records", tvToken, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked device still authorised: %d", resp.StatusCode)
	}
}

func TestSetupKeyEndToEnd(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key", phoneToken, map[string]string{
		"key": "a-long-enough-key",
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set setup key: %d %v", resp.StatusCode, body)
	}

	resp, body = do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key/redeem", "", map[string]string{
		"key":        "a-long-enough-key",
		"deviceName": "TV",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem setup key: %d %v", resp.StatusCode, body)
	}

	// Not single-use: redeeming again for a second device must still work.
	resp, body = do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key/redeem", "", map[string]string{
		"key":        "a-long-enough-key",
		"deviceName": "Tablet",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("redeem setup key a second time: %d %v", resp.StatusCode, body)
	}

	resp, body = do(t, http.MethodGet, srv.URL+"/api/v1/devices", phoneToken, nil)
	if devices, ok := body["devices"].([]any); !ok || len(devices) != 3 {
		t.Fatalf("expected phone + TV + tablet, got %d %v", resp.StatusCode, body)
	}
}

func TestSetupKeyRequiresMinLength(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key", phoneToken, map[string]string{
		"key": "abc",
	})
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected a short key to be rejected, got %d", resp.StatusCode)
	}
}

func TestClearSetupKeyOverHTTP(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key", phoneToken, map[string]string{
		"key": "a-long-enough-key",
	})

	resp, _ := do(t, http.MethodDelete, srv.URL+"/api/v1/pair/setup-key", phoneToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear setup key: %d", resp.StatusCode)
	}

	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key/redeem", "", map[string]string{
		"key":        "a-long-enough-key",
		"deviceName": "TV",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected the cleared key to be rejected, got %d", resp.StatusCode)
	}
}

func TestSetupKeyRequiresAuthToSet(t *testing.T) {
	srv, _ := newTestServer(t, true)
	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/pair/setup-key", "", map[string]string{
		"key": "a-long-enough-key",
	})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected setting a key without auth to be rejected, got %d", resp.StatusCode)
	}
}

func TestPresenceEndToEnd(t *testing.T) {
	srv, _ := newTestServer(t, true)
	_, phoneToken := createAccount(t, srv)

	resp, body := do(t, http.MethodPost, srv.URL+"/api/v1/pair", phoneToken, nil)
	code := body["code"].(string)
	_, body = do(t, http.MethodPost, srv.URL+"/api/v1/pair/redeem", "",
		map[string]string{"code": code, "deviceName": "TV"})
	tvToken := body["token"].(string)

	resp, _ = do(t, http.MethodPost, srv.URL+"/api/v1/presence", phoneToken, map[string]string{
		"status": `{"title":"Show","playing":true}`,
	})
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("set presence: %d", resp.StatusCode)
	}

	_, body = do(t, http.MethodGet, srv.URL+"/api/v1/presence", tvToken, nil)
	devices, ok := body["devices"].([]any)
	if !ok || len(devices) != 1 {
		t.Fatalf("expected the TV to see the phone's presence, got %v", body)
	}
	entry := devices[0].(map[string]any)
	if entry["status"] != `{"title":"Show","playing":true}` {
		t.Fatalf("unexpected presence entry: %v", entry)
	}

	// The phone does not see its own presence.
	_, body = do(t, http.MethodGet, srv.URL+"/api/v1/presence", phoneToken, nil)
	if devices, ok := body["devices"].([]any); !ok || len(devices) != 0 {
		t.Fatalf("expected a device to be excluded from its own presence list, got %v", body)
	}

	resp, _ = do(t, http.MethodDelete, srv.URL+"/api/v1/presence", phoneToken, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("clear presence: %d", resp.StatusCode)
	}
	_, body = do(t, http.MethodGet, srv.URL+"/api/v1/presence", tvToken, nil)
	if devices, ok := body["devices"].([]any); !ok || len(devices) != 0 {
		t.Fatalf("expected presence to be gone after clearing, got %v", body)
	}
}

func TestPresenceRequiresAuth(t *testing.T) {
	srv, _ := newTestServer(t, true)

	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/presence", "", map[string]string{"status": "x"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected setting presence without auth to be rejected, got %d", resp.StatusCode)
	}

	resp, _ = do(t, http.MethodGet, srv.URL+"/api/v1/presence", "", nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected reading presence without auth to be rejected, got %d", resp.StatusCode)
	}
}

func TestInvalidPairCodeRejected(t *testing.T) {
	srv, _ := newTestServer(t, true)
	resp, _ := do(t, http.MethodPost, srv.URL+"/api/v1/pair/redeem", "",
		map[string]string{"code": "ZZZZZZZZ", "deviceName": "Attacker"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected an unknown code to be rejected, got %d", resp.StatusCode)
	}
}
