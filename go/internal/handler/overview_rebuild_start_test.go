package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// kickRecorder is the OverviewController double: it records calls and plays
// back a scripted armed/coalesced answer.
type kickRecorder struct {
	calls int
	armed bool
}

func (k *kickRecorder) KickOverviewRebuild() bool { k.calls++; return k.armed }

// TestOverviewRebuildStartContract pins the handler contract of
// overview-rebuild-start: nil controller answers 503 (SetOverviewController
// pattern — an unwired surface must fail loudly, not pretend to kick), a
// wired controller reports armed=true, and a pending kick surfaces as
// success with armed=false (coalesced is not an error: the pending kick
// covers this request too).
func TestOverviewRebuildStartContract(t *testing.T) {
	t.Run("nil controller answers 503", func(t *testing.T) {
		h := &ManageHandler{}
		rec := httptest.NewRecorder()
		h.handleOverviewRebuildStart(rec)
		if rec.Code != 503 {
			t.Fatalf("status=%d, want 503", rec.Code)
		}
	})

	t.Run("wired controller kicks and reports armed", func(t *testing.T) {
		k := &kickRecorder{armed: true}
		h := &ManageHandler{overview: k}
		rec := httptest.NewRecorder()
		h.handleOverviewRebuildStart(rec)
		if rec.Code != 200 || k.calls != 1 {
			t.Fatalf("status=%d calls=%d, want 200/1", rec.Code, k.calls)
		}
		var body struct {
			Success bool `json:"success"`
			Armed   bool `json:"armed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		if !body.Success || !body.Armed {
			t.Fatalf("body=%+v, want success+armed", body)
		}
	})

	t.Run("coalesced kick is success with armed=false", func(t *testing.T) {
		k := &kickRecorder{armed: false}
		h := &ManageHandler{overview: k}
		rec := httptest.NewRecorder()
		h.handleOverviewRebuildStart(rec)
		var body struct {
			Success bool `json:"success"`
			Armed   bool `json:"armed"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("body decode: %v", err)
		}
		if rec.Code != 200 || !body.Success || body.Armed {
			t.Fatalf("status=%d body=%+v, want 200/success/armed=false", rec.Code, body)
		}
	})
}

// TestOverviewRebuildStartTier pins the admin gate: the action sits in the
// tierServerAdmin list — a manual rebuild is operator-scale compute spend
// (same reasoning as blocks-audit-start). Red against a fail-open dispatcher
// default: removing the tier entry would make this test fail, not the action
// silently open.
func TestOverviewRebuildStartTier(t *testing.T) {
	if got := actionTier(manageRequest{Action: "overview-rebuild-start"}); got != tierServerAdmin {
		t.Fatalf("actionTier(overview-rebuild-start)=%v, want tierServerAdmin", got)
	}
}
