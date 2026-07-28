package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
)

func gateAuth() *auth.AuthResult {
	return &auth.AuthResult{
		ApiKeyID:      "00000000-0000-0000-0000-000000000001",
		HomeScope:     "private",
		AllowedScopes: []string{"private", "shared"},
		IsValid:       true,
	}
}

func gateReq() storeRequest {
	return storeRequest{Category: "learnings", Title: "t", Content: "harmless content"}
}

// run wraps runStageWriteGates with the common test wiring: nil pool + rate
// limit 0 (the only DB-touching gate stays off), nil blocktype set.
func run(t *testing.T, req storeRequest, set *blocktype.Set) (*stageWriteGateResult, *writeReject) {
	t.Helper()
	return runStageWriteGates(context.Background(), nil, set, gateAuth(), req, backends.SensInternal, 0, "test-req")
}

func TestStageGatesHappyPath(t *testing.T) {
	res, rej := run(t, gateReq(), nil)
	if rej != nil {
		t.Fatalf("unexpected reject: %+v", rej)
	}
	if res.WriteScope != "private" || res.ScopeExplicit {
		t.Fatalf("want implicit home scope, got %+v", res)
	}
	if res.Sens.Value != backends.SensInternal || res.Sens.Manual || res.Sens.Detector {
		t.Fatalf("want plain default sensitivity, got %+v", res.Sens)
	}
}

func TestStageGatesRequiredFields(t *testing.T) {
	req := gateReq()
	req.Content = ""
	_, rej := run(t, req, nil)
	if rej == nil || rej.Status != http.StatusBadRequest {
		t.Fatalf("want 400 on missing content, got %+v", rej)
	}
}

// Oversize content is rejected BEFORE the card (D1-M2): the stage must never
// promise a write the execute-time upsert would refuse.
func TestStageGatesOversizeRejectsPreCard(t *testing.T) {
	req := gateReq()
	req.Content = strings.Repeat("x", 50*1024+1)
	_, rej := run(t, req, nil)
	if rej == nil || rej.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 pre-card, got %+v", rej)
	}
}

func TestStageGatesInvalidSensitivity(t *testing.T) {
	req := gateReq()
	req.Sensitivity = "totally-bogus"
	_, rej := run(t, req, nil)
	if rej == nil || rej.Status != http.StatusBadRequest {
		t.Fatalf("want 400 on invalid sensitivity, got %+v", rej)
	}
}

// G40 detector: a credentials pattern in staged content forces the resolved
// sensitivity to credentials (upgrade-only) and records the secret-free
// reason — identical to the direct path.
func TestStageGatesDetectorForcesCredentials(t *testing.T) {
	req := gateReq()
	req.Content = "aws key: AKIA" + strings.Repeat("Z", 16)
	res, rej := run(t, req, nil)
	if rej != nil {
		t.Fatalf("unexpected reject: %+v", rej)
	}
	if res.Sens.Value != backends.SensCredentials || !res.Sens.Detector {
		t.Fatalf("detector did not force credentials: %+v", res.Sens)
	}
	if res.Metadata["sensitivity_detector"] == nil {
		t.Fatal("detector reason missing from metadata")
	}
}

func TestStageGatesScopeGate(t *testing.T) {
	req := gateReq()
	req.Scope = "work" // not writable: neither home nor shared-allowed nor write_scopes
	_, rej := run(t, req, nil)
	if rej == nil || rej.Status != http.StatusForbidden {
		t.Fatalf("want 403 on unwritable scope, got %+v", rej)
	}

	req.Scope = "shared" // allowed_scopes contains shared ⇒ writable
	res, rej := run(t, req, nil)
	if rej != nil {
		t.Fatalf("unexpected reject for shared: %+v", rej)
	}
	if res.WriteScope != "shared" || !res.ScopeExplicit {
		t.Fatalf("want explicit shared scope, got %+v", res)
	}
}

// Explicit type: nil set fails closed (422); a set that knows the name passes.
func TestStageGatesTypeValidation(t *testing.T) {
	req := gateReq()
	req.Type = "note"
	_, rej := run(t, req, nil)
	if rej == nil || rej.Status != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 fail-closed on nil set, got %+v", rej)
	}

	set, err := blocktype.NewSet([]blocktype.Policy{{Name: "note", IsDefault: true}})
	if err != nil {
		t.Fatal(err)
	}
	if _, rej := run(t, req, set); rej != nil {
		t.Fatalf("unexpected reject with registered type: %+v", rej)
	}

	req.Type = "unknown-type"
	if _, rej := run(t, req, set); rej == nil || rej.Status != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 on unknown type, got %+v", rej)
	}
}
