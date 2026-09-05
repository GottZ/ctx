//go:build integration

// T03-4b probes: the three properties of the converted handler transactions that
// the existing suite did NOT carry.
//
//  1. ATTRIBUTION. Seven of the sixteen converted openings stamp the request id
//     inside the transaction (store.SetTxRequestID), and the 092 audit trigger
//     reads it back out of the transaction-local GUC. Nothing asserted that the
//     stamp survives — a SetTxRequestID that moved OUT of the bracket, or a
//     bracket that opened a second transaction for the write, would still return
//     200 and still write the audit row, only with a NULL request_id.
//     The production RequestID middleware has to be mounted for that to mean
//     anything: the package's other manage tests build their request by hand, so
//     RequestIDFromContext is empty there and the stamp would be trivially
//     satisfied by nothing at all.
//
//  2. handleDisableProfileUpdate — no test in the tree drives it (only the tier
//     table names the action).
//
//  3. handleBackendDelete — same gap: the action appears only in admin-403
//     lists, never against a real row.
//
// Run with:
//
//	cd go && GOTMPDIR=/compose/n8n/.gotmp GOCACHE=/compose/n8n/.gotmp/gocache-t03-4b \
//	  go test -tags=integration -p 1 ./internal/handler/ \
//	  -run 'TestDisableProfileAudit|TestBackendDelete' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// t034bRequestID is 44 chars of hex + dashes, the shape isValidRequestID accepts
// verbatim (middleware.go) — a malformed one would be replaced by a generated id
// and the assertion below would test the generator instead of the stamp.
const t034bRequestID = "aaaabbbbccccdddd-1111-2222-3333-444455556666"

// t034bRouter puts the PRODUCTION RequestID middleware in front of HandleManage,
// then injects the auth result the way the real auth middleware does.
func t034bRouter(ph *profileHarness, ar *auth.AuthResult) http.Handler {
	r := chi.NewRouter()
	r.Use(RequestID)
	r.Post("/api/manage", func(w http.ResponseWriter, req *http.Request) {
		ph.h.HandleManage(w, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
	})
	return r
}

// t034bDo sends one manage request through that router with the pinned header.
func t034bDo(t *testing.T, h http.Handler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Request-ID", t034bRequestID)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// t034bLastAudit reads the newest audit row of one entity.
func t034bLastAudit(t *testing.T, pool *pgxpool.Pool, entityType, entityKey string) (apiKeyID, requestID, action string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT COALESCE(api_key_id::text,''), COALESCE(metadata->>'request_id',''), action
		   FROM context_settings_audit
		  WHERE entity_type = $1 AND entity_key = $2
		  ORDER BY created_at DESC, id DESC LIMIT 1`,
		entityType, entityKey).Scan(&apiKeyID, &requestID, &action)
	if err != nil {
		t.Fatalf("audit read %s/%s: %v", entityType, entityKey, err)
	}
	return apiKeyID, requestID, action
}

func t034bCheckStamp(t *testing.T, ph *profileHarness, entityKey, wantAction string) {
	t.Helper()
	gotKey, gotReq, gotAction := t034bLastAudit(t, ph.pool, "disable_profile", entityKey)
	if gotAction != wantAction {
		t.Errorf("audit action = %q, want %q", gotAction, wantAction)
	}
	if gotKey != ph.admin.ApiKeyID {
		t.Errorf("audit api_key_id = %q, want %q", gotKey, ph.admin.ApiKeyID)
	}
	if gotReq != t034bRequestID {
		t.Errorf("audit request_id = %q, want %q — the request id must be stamped INSIDE the write transaction",
			gotReq, t034bRequestID)
	}
}

// TestDisableProfileAudit_RequestIDStamp drives create then update through the
// mounted middleware and pins the audit stamp of both. The update leg is also
// the only coverage handleDisableProfileUpdate has.
func TestDisableProfileAudit_RequestIDStamp(t *testing.T) {
	ph := setupProfileHarness(t)
	h := t034bRouter(ph, ph.admin)

	rec := t034bDo(t, h, map[string]any{
		"action": "disable-profile-create",
		"data":   map[string]any{"name": "t034b", "label": "before", "members": []string{"chat-a"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("create: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	t034bCheckStamp(t, ph, "t034b", "create")

	rec = t034bDo(t, h, map[string]any{
		"action": "disable-profile-update",
		"data":   map[string]any{"name": "t034b", "label": "after", "members": []string{"chat-a", "chat-b"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("update: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	t034bCheckStamp(t, ph, "t034b", "update")

	// The update must have COMMITTED, not just answered 200.
	var label string
	var members int
	if err := ph.pool.QueryRow(ph.ctx,
		`SELECT p.label, (SELECT count(*) FROM context_disable_profile_backends m WHERE m.profile_id = p.id)
		   FROM context_disable_profiles p WHERE p.scope = '_global' AND p.name = 't034b'`,
	).Scan(&label, &members); err != nil {
		t.Fatalf("reread profile: %v", err)
	}
	if label != "after" || members != 2 {
		t.Errorf("after update: label = %q members = %d, want \"after\" / 2", label, members)
	}
}

// TestBackendDelete_CommitsAndAudits covers handleBackendDelete: the row is gone
// after the response, the audit trail carries the delete, and the pool snapshot
// no longer serves it.
func TestBackendDelete_CommitsAndAudits(t *testing.T) {
	ph := setupProfileHarness(t)

	rec := ph.doID(ph.admin, "backend-delete", ph.byID["chat-b"], nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: code = %d, body = %s", rec.Code, rec.Body.String())
	}
	if got := decode(t, rec)["deleted"]; got != "chat-b" {
		t.Errorf("deleted = %v, want chat-b", got)
	}

	var rows int
	if err := ph.pool.QueryRow(ph.ctx,
		`SELECT count(*) FROM context_backends WHERE id = $1`, ph.byID["chat-b"]).Scan(&rows); err != nil {
		t.Fatalf("count backend: %v", err)
	}
	if rows != 0 {
		t.Errorf("backend row still present after delete (count = %d)", rows)
	}
	if _, _, action := t034bLastAudit(t, ph.pool, "backend", "chat-b"); action != "delete" {
		t.Errorf("audit action = %q, want delete", action)
	}
	for _, b := range ph.bp.Snapshot() {
		if b.ID == ph.byID["chat-b"] {
			t.Error("deleted backend still in the pool snapshot — reloadAfterMutation did not run")
		}
	}

	// A second delete of the same id is a uniform 404, not a 500.
	if rec := ph.doID(ph.admin, "backend-delete", ph.byID["chat-b"], nil); rec.Code != http.StatusNotFound {
		t.Errorf("second delete: code = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}
