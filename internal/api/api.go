// Package api exposes the sync store over HTTP.
//
// The surface is intentionally small. The server does not know what a watch position or a
// repository list is; it stores opaque values under (category, key) and tells a device what
// has changed since it last asked. Every decision about what to sync, when, and how to apply
// it belongs to the app.
package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/F-e-n-y-x/cloudstream-sync/internal/store"
)

// PairCodeTTL is how long a pairing code stays valid. Short, because the codes are short
// enough to be typed and therefore weak.
const PairCodeTTL = 10 * time.Minute

// MaxBodyBytes bounds request bodies. A sync push of a large library is still well under
// this; anything larger is a client bug or an attack.
const MaxBodyBytes = 8 << 20 // 8 MiB

// Server holds the dependencies for the HTTP handlers.
type Server struct {
	store *store.Store
	log   *slog.Logger
	// openRegistration allows anyone who can reach the server to create an account. Off by
	// default: a self-hosted instance is usually for one household, and an open endpoint
	// lets a stranger who finds the URL use it as free storage.
	openRegistration bool
}

func New(st *store.Store, log *slog.Logger, openRegistration bool) *Server {
	return &Server{store: st, log: log, openRegistration: openRegistration}
}

// Routes builds the HTTP handler.
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("POST /api/v1/account", s.handleCreateAccount)
	mux.HandleFunc("POST /api/v1/pair/redeem", s.handleRedeemPair)
	mux.HandleFunc("POST /api/v1/pair/setup-key/redeem", s.handleRedeemSetupKey)

	// Authenticated.
	mux.Handle("POST /api/v1/pair", s.authenticated(s.handleCreatePair))
	mux.Handle("POST /api/v1/pair/setup-key", s.authenticated(s.handleSetSetupKey))
	mux.Handle("DELETE /api/v1/pair/setup-key", s.authenticated(s.handleClearSetupKey))
	mux.Handle("GET /api/v1/devices", s.authenticated(s.handleListDevices))
	mux.Handle("DELETE /api/v1/devices/{id}", s.authenticated(s.handleRemoveDevice))
	mux.Handle("GET /api/v1/records", s.authenticated(s.handleGetRecords))
	mux.Handle("POST /api/v1/records", s.authenticated(s.handlePutRecords))
	mux.Handle("GET /api/v1/status", s.authenticated(s.handleStatus))
	mux.Handle("PUT /api/v1/presence", s.authenticated(s.handleSetPresence))
	mux.Handle("DELETE /api/v1/presence", s.authenticated(s.handleClearPresence))
	mux.Handle("GET /api/v1/presence", s.authenticated(s.handleGetPresence))

	return s.withRecovery(s.withLogging(mux))
}

// ---- middleware ----

func (s *Server) authenticated(next func(http.ResponseWriter, *http.Request, store.Device)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "missing bearer token")
			return
		}

		device, err := s.store.DeviceByToken(r.Context(), token)
		if errors.Is(err, store.ErrNotFound) {
			// Deliberately identical to a malformed token: a caller must not be able to
			// tell a revoked device from a wrong one.
			writeError(w, http.StatusUnauthorized, "invalid token")
			return
		}
		if err != nil {
			s.log.Error("resolve token", "err", err)
			writeError(w, http.StatusInternalServerError, "internal error")
			return
		}

		next(w, r, device)
	})
}

func (s *Server) withLogging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		s.log.Info("request",
			"method", r.Method,
			"path", r.URL.Path,
			"status", rec.status,
			"duration", time.Since(start).String(),
		)
	})
}

// withRecovery keeps one bad request from taking the whole server down, which matters for
// something meant to run unattended on a home server.
func (s *Server) withRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic serving request", "path", r.URL.Path, "panic", rec)
				writeError(w, http.StatusInternalServerError, "internal error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func bearerToken(r *http.Request) string {
	header := r.Header.Get("Authorization")
	if after, ok := strings.CutPrefix(header, "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// ---- handlers ----

func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

type createAccountRequest struct {
	DeviceName string `json:"deviceName"`
}

type credentialsResponse struct {
	AccountID  string `json:"accountId"`
	DeviceID   string `json:"deviceId"`
	DeviceName string `json:"deviceName"`
	Token      string `json:"token"`
}

func (s *Server) handleCreateAccount(w http.ResponseWriter, r *http.Request) {
	if !s.openRegistration {
		writeError(w, http.StatusForbidden,
			"registration is disabled; start the server with -open-registration to create the first account")
		return
	}

	var req createAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	accountID, device, token, err := s.store.CreateAccount(r.Context(), req.DeviceName)
	if err != nil {
		s.log.Error("create account", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create account")
		return
	}

	writeJSON(w, http.StatusCreated, credentialsResponse{
		AccountID:  accountID,
		DeviceID:   device.ID,
		DeviceName: device.Name,
		Token:      token,
	})
}

type pairResponse struct {
	Code      string `json:"code"`
	ExpiresAt int64  `json:"expiresAt"`
}

func (s *Server) handleCreatePair(w http.ResponseWriter, r *http.Request, device store.Device) {
	code, expires, err := s.store.CreatePairCode(r.Context(), device.AccountID, PairCodeTTL)
	if err != nil {
		s.log.Error("create pair code", "err", err)
		writeError(w, http.StatusInternalServerError, "could not create pairing code")
		return
	}
	writeJSON(w, http.StatusCreated, pairResponse{Code: code, ExpiresAt: expires.UnixMilli()})
}

type redeemPairRequest struct {
	Code       string `json:"code"`
	DeviceName string `json:"deviceName"`
}

func (s *Server) handleRedeemPair(w http.ResponseWriter, r *http.Request) {
	var req redeemPairRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	device, token, err := s.store.RedeemPairCode(r.Context(), req.Code, req.DeviceName)
	if errors.Is(err, store.ErrPairCodeInvalid) {
		writeError(w, http.StatusUnauthorized, "pairing code is invalid or has expired")
		return
	}
	if err != nil {
		s.log.Error("redeem pair code", "err", err)
		writeError(w, http.StatusInternalServerError, "could not redeem pairing code")
		return
	}

	writeJSON(w, http.StatusCreated, credentialsResponse{
		AccountID:  device.AccountID,
		DeviceID:   device.ID,
		DeviceName: device.Name,
		Token:      token,
	})
}

type setupKeyRequest struct {
	Key string `json:"key"`
}

func (s *Server) handleSetSetupKey(w http.ResponseWriter, r *http.Request, device store.Device) {
	var req setupKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req.Key) < store.MinSetupKeyLength {
		writeError(w, http.StatusBadRequest,
			fmt.Sprintf("key must be at least %d characters", store.MinSetupKeyLength))
		return
	}

	if err := s.store.SetSetupKey(r.Context(), device.AccountID, req.Key); err != nil {
		s.log.Error("set setup key", "err", err)
		writeError(w, http.StatusInternalServerError, "could not set pairing key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearSetupKey(w http.ResponseWriter, r *http.Request, device store.Device) {
	if err := s.store.ClearSetupKey(r.Context(), device.AccountID); err != nil {
		s.log.Error("clear setup key", "err", err)
		writeError(w, http.StatusInternalServerError, "could not clear pairing key")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type redeemSetupKeyRequest struct {
	Key        string `json:"key"`
	DeviceName string `json:"deviceName"`
}

func (s *Server) handleRedeemSetupKey(w http.ResponseWriter, r *http.Request) {
	var req redeemSetupKeyRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	device, token, err := s.store.RedeemSetupKey(r.Context(), req.Key, req.DeviceName)
	if errors.Is(err, store.ErrSetupKeyInvalid) {
		writeError(w, http.StatusUnauthorized, "pairing key is invalid")
		return
	}
	if err != nil {
		s.log.Error("redeem setup key", "err", err)
		writeError(w, http.StatusInternalServerError, "could not redeem pairing key")
		return
	}

	writeJSON(w, http.StatusCreated, credentialsResponse{
		AccountID:  device.AccountID,
		DeviceID:   device.ID,
		DeviceName: device.Name,
		Token:      token,
	})
}

func (s *Server) handleListDevices(w http.ResponseWriter, r *http.Request, device store.Device) {
	devices, err := s.store.ListDevices(r.Context(), device.AccountID)
	if err != nil {
		s.log.Error("list devices", "err", err)
		writeError(w, http.StatusInternalServerError, "could not list devices")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"devices":   devices,
		"thisId":    device.ID,
		"accountId": device.AccountID,
	})
}

func (s *Server) handleRemoveDevice(w http.ResponseWriter, r *http.Request, device store.Device) {
	id := r.PathValue("id")
	if err := s.store.RemoveDevice(r.Context(), device.AccountID, id); errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusNotFound, "no such device")
		return
	} else if err != nil {
		s.log.Error("remove device", "err", err)
		writeError(w, http.StatusInternalServerError, "could not remove device")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, device store.Device) {
	seq, err := s.store.AccountSeq(r.Context(), device.AccountID)
	if err != nil {
		s.log.Error("account seq", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read account")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"accountId":  device.AccountID,
		"deviceId":   device.ID,
		"deviceName": device.Name,
		"cursor":     seq,
	})
}

type setPresenceRequest struct {
	Status string `json:"status"`
}

func (s *Server) handleSetPresence(w http.ResponseWriter, r *http.Request, device store.Device) {
	var req setPresenceRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.store.SetPresence(r.Context(), device.AccountID, device.ID, req.Status); err != nil {
		s.log.Error("set presence", "err", err)
		writeError(w, http.StatusInternalServerError, "could not set presence")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleClearPresence(w http.ResponseWriter, r *http.Request, device store.Device) {
	if err := s.store.ClearPresence(r.Context(), device.ID); err != nil {
		s.log.Error("clear presence", "err", err)
		writeError(w, http.StatusInternalServerError, "could not clear presence")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleGetPresence(w http.ResponseWriter, r *http.Request, device store.Device) {
	presence, err := s.store.GetPresence(r.Context(), device.AccountID, device.ID)
	if err != nil {
		s.log.Error("get presence", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read presence")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"devices": presence})
}

type recordsResponse struct {
	Records []store.Record `json:"records"`
	Cursor  int64          `json:"cursor"`
	// HasMore tells the client to page again immediately rather than waiting for the next
	// sync, which matters on a first sync of a large library.
	HasMore bool `json:"hasMore"`
}

func (s *Server) handleGetRecords(w http.ResponseWriter, r *http.Request, device store.Device) {
	since, err := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if err != nil {
		since = 0
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}

	categories := r.URL.Query()["category"]

	records, cursor, err := s.store.GetRecords(r.Context(), device.AccountID, since, categories, limit)
	if err != nil {
		s.log.Error("get records", "err", err)
		writeError(w, http.StatusInternalServerError, "could not read records")
		return
	}

	writeJSON(w, http.StatusOK, recordsResponse{
		Records: records,
		Cursor:  cursor,
		HasMore: len(records) == limit,
	})
}

type putRecordsRequest struct {
	Records []store.Record `json:"records"`
}

func (s *Server) handlePutRecords(w http.ResponseWriter, r *http.Request, device store.Device) {
	var req putRecordsRequest
	if !decodeJSON(w, r, &req) {
		return
	}

	cursor, err := s.store.PutRecords(r.Context(), device.AccountID, device.ID, req.Records)
	if err != nil {
		s.log.Error("put records", "err", err)
		writeError(w, http.StatusInternalServerError, "could not store records")
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"cursor":   cursor,
		"accepted": len(req.Records),
	})
}

// ---- helpers ----

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, MaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return false
	}
	return true
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
