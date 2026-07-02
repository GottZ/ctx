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

func (s staticConfigStore) Snapshot() *config.Config { return s.cfg }
func (s staticConfigStore) SnapshotForRequest(context.Context) *config.Config { return s.cfg }
func (s staticConfigStore) SnapshotForTenant(context.Context, string) *config.Config {
	return s.cfg
}

// gamingHandler wires a ManageHandler with a static config snapshot and a
// seeded pool (the names the disabled list is cross-checked against). No DB,
// no reload — the read and the 422-before-write paths never touch them.
func gamingHandler(active bool, disabled, poolNames []string) *ManageHandler {
	cfg := &config.Config{Pool: config.PoolConfig{
		GamingActive:           active,
		GamingDisabledBackends: disabled,
	}}
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

type gamingResp struct {
	Success bool `json:"success"`
	Gaming  struct {
		Active           bool     `json:"active"`
		DisabledBackends []string `json:"disabled_backends"`
		UnknownBackends  []string `json:"unknown_backends"`
		Note             string   `json:"note"`
	} `json:"gaming"`
}

// A status read ({} data) stays open to any valid key (design 03 §2.6 — only
// the mutating shape is admin-gated) and renders the live state. A name in the
// disabled list that matches no live backend surfaces as unknown_backends (a
// typo would otherwise make the toggle silently ineffective, risk 6.6).
func TestGamingMode_Read_NonAdmin_RendersStateAndUnknown(t *testing.T) {
	h := gamingHandler(true,
		[]string{"herbert-chat", "herbert-rerank", "herbert_typo"},
		[]string{"herbert-chat", "herbert-rerank", "llama-cpu"})
	rec := gamingReq(t, h, nonAdminAR(), "{}")
	if rec.Code == http.StatusForbidden {
		t.Fatalf("read got 403 — the status read must stay non-admin (design 03 §2.6)")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("read status = %d, want 200", rec.Code)
	}
	var resp gamingResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Success || !resp.Gaming.Active {
		t.Errorf("success=%v active=%v, want both true", resp.Success, resp.Gaming.Active)
	}
	if len(resp.Gaming.UnknownBackends) != 1 || resp.Gaming.UnknownBackends[0] != "herbert_typo" {
		t.Errorf("unknown_backends = %v, want [herbert_typo]", resp.Gaming.UnknownBackends)
	}
	if resp.Gaming.Note == "" {
		t.Error("note is empty — the in-flight advisory must always be present")
	}
}

// A clean disabled list (all names live) yields no unknown_backends.
func TestGamingMode_Read_NoUnknownWhenNamesLive(t *testing.T) {
	h := gamingHandler(false,
		[]string{"herbert-chat", "herbert-rerank"},
		[]string{"herbert-chat", "herbert-rerank", "llama-cpu"})
	rec := gamingReq(t, h, nonAdminAR(), "{}")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	var resp gamingResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Gaming.Active {
		t.Error("active = true, want false")
	}
	if len(resp.Gaming.UnknownBackends) != 0 {
		t.Errorf("unknown_backends = %v, want empty", resp.Gaming.UnknownBackends)
	}
}

// An admin passes the gate; an unknown mode is a 422 BEFORE any write — the
// nil pool/reload are never reached (the validation precedes them).
func TestGamingMode_BadMode_422(t *testing.T) {
	h := gamingHandler(false, nil, nil)
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
