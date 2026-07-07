package handler

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeDreamController is a minimal DreamController for the handler-level
// dream-mode response test (no scheduler stood up).
type fakeDreamController struct {
	mode     int32
	interval time.Duration
}

func (f *fakeDreamController) SetDreamMode(mode int32, throttleInterval time.Duration) {
	f.mode = mode
	f.interval = throttleInterval
}

func (f *fakeDreamController) GetDreamMode() (int32, time.Duration) {
	return f.mode, f.interval
}

// TestDreamModeMutationCarriesAsOf is the U01-W7 gate for §4.5-4: the dream-mode
// MUTATION answer must carry an as_of timestamp so the DreamTile can splice it
// into the held status against the client's asOfFloor (instead of a stale
// reload). Structurally red against the pre-W7 handler, whose mutation body was
// {success, mode, interval} with NO as_of.
func TestDreamModeMutationCarriesAsOf(t *testing.T) {
	h := &ManageHandler{dreamController: &fakeDreamController{}}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/manage", nil)
	h.handleDreamMode(rec, req, manageRequest{Action: "dream-mode", Data: json.RawMessage(`{"mode":"off"}`)})

	if rec.Code != 200 {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body["mode"] != "off" {
		t.Errorf("mode = %v, want off", body["mode"])
	}
	asOf, ok := body["as_of"].(string)
	if !ok || asOf == "" {
		t.Fatalf("dream-mode mutation must carry a non-empty as_of, got %v (§4.5-4)", body["as_of"])
	}
	if _, err := time.Parse(time.RFC3339Nano, asOf); err != nil {
		t.Errorf("as_of %q is not RFC3339Nano: %v", asOf, err)
	}
}
