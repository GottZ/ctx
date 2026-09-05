package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
)

// staticConfigStore is a ConfigStore returning a fixed snapshot (no DB) for the
// gaming-mode read/validation paths.
type staticConfigStore struct{ cfg *config.Config }

func (s staticConfigStore) Snapshot() *config.Config                          { return s.cfg }
func (s staticConfigStore) SnapshotForRequest(context.Context) *config.Config { return s.cfg }
func (s staticConfigStore) SnapshotForTenant(context.Context, string) *config.Config {
	return s.cfg
}

// gamingHandler wires a ManageHandler with a static (empty) config snapshot and
// a seeded pool. No DB, no reload — the 422-before-write validation path never
// touches them. Since U01-W5 the exclusion state lives in the eject profile
// (DB), not config, so the handler carries no gaming config fields.
func gamingHandler(poolNames []string) *ManageHandler {
	cfg := &config.Config{}
	bp := backends.NewPool(nil, nil)
	bs := make([]backends.Backend, 0, len(poolNames))
	for _, n := range poolNames {
		bs = append(bs, backends.Backend{Name: n})
	}
	bp.SeedSnapshotForTest(bs)
	return NewManageHandler(nil, staticConfigStore{cfg}, nil, bp, nil, nil, nil, nil)
}

func gamingReq(t *testing.T, h *ManageHandler, ar *auth.AuthResult, data string) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"action": "gaming-mode", "data": json.RawMessage(data)})
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

// NOTE (U01-W3, AM-7): the eject/gaming-mode READ now renders the '_global'
// 'eject' profile's live state (active + member names), not the old
// config-derived list — so unknown_backends is gone structurally (FK membership
// cannot dangle) and the read touches the DB. The read + shape assertions moved
// to context_disable_profiles_integration_test.go (DB-backed); the byte-identical
// eject-mode==gaming-mode shape is pinned there too (gate f). The DB-FREE
// mode-validation path stays a unit test below.

// An admin passes the gate; an unknown mode is a 422 BEFORE any write — the
// nil pool/reload are never reached (the validation precedes them).
func TestGamingMode_BadMode_422(t *testing.T) {
	h := gamingHandler(nil)
	rec := gamingReq(t, h, adminAR(), `{"mode":"bogus"}`)
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("bad mode status = %d, want 422", rec.Code)
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if ok, _ := resp["success"].(bool); ok {
		t.Error("success = true on 422")
	}
}
