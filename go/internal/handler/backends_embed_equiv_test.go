package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
)

// equivTestPool seeds a snapshot with ONE local reference embed backend.
func equivTestPool(extra ...backends.Backend) *backends.Pool {
	p := backends.NewPool(nil, nil)
	bs := append([]backends.Backend{{
		ID: "ref-id", Name: "ref-embed", Host: "http://ref:8081",
		Protocol: backends.ProtocolOpenAI, Locality: "lan", Enabled: true,
		Priority: 98, Roles: []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "ref-model"}},
	}}, extra...)
	p.SeedSnapshotForTest(bs)
	return p
}

// stubEquivProbe routes deterministic vectors by host: the reference host gets
// base, any other host gets cand. Restores itself via t.Cleanup.
func stubEquivProbe(t *testing.T, base, cand []float32) {
	t.Helper()
	orig := embedEquivProbe
	embedEquivProbe = func(_ context.Context, b backends.Backend, _ string, _ embed.Prefix) ([]float32, error) {
		if b.Host == "http://ref:8081" {
			return base, nil
		}
		return cand, nil
	}
	t.Cleanup(func() { embedEquivProbe = orig })
}

func equivRequest(t *testing.T, bp *backends.Pool, body map[string]any) map[string]any {
	t.Helper()
	h := NewManageHandler(nil, nil, nil, bp, nil, nil, nil, nil)
	h.SetAdmitter(dispatch.New(nil, dispatch.DefaultSettings()))
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, adminAR()))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return resp
}

// unsavedEquivBody is the dialog case: NO id, the candidate lives only in the
// submitted spec (test-before-create).
func unsavedEquivBody() map[string]any {
	return map[string]any{
		"action": "backend-test",
		"data": map[string]any{
			"probe":     "embed-equivalence",
			"name":      "cand-embed",
			"base_url":  "https://cand:4000",
			"protocol":  "openai",
			"locality":  "external",
			"roles":     []string{"embed"},
			"model_map": map[string]any{"default": "cand-model"},
		},
	}
}

// TestEmbedEquivalenceVerified: identical vectors → cosine 1.0, verified, and
// the ready-to-merge metadata_patch carries the armed flag + proof.
func TestEmbedEquivalenceVerified(t *testing.T) {
	vec := []float32{0.6, 0.8, 0}
	stubEquivProbe(t, vec, vec)
	resp := equivRequest(t, equivTestPool(), unsavedEquivBody())

	if resp["verified"] != true {
		t.Fatalf("verified = %v, want true (resp %v)", resp["verified"], resp)
	}
	minCos, _ := resp["min_cosine"].(float64)
	if math.Abs(minCos-1.0) > 1e-9 {
		t.Errorf("min_cosine = %v, want 1.0", minCos)
	}
	patch, ok := resp["metadata_patch"].(map[string]any)
	if !ok || patch["embed_equivalence_verified"] != true {
		t.Fatalf("metadata_patch missing or unarmed: %v", resp["metadata_patch"])
	}
	proof, ok := patch["embed_equivalence_proof"].(map[string]any)
	if !ok || proof["reference"] != "ref-embed" || proof["model"] != "cand-model" {
		t.Errorf("proof = %v, want reference ref-embed + model cand-model", proof)
	}
	if got := len(resp["samples"].([]any)); got != len(embedEquivSamples) {
		t.Errorf("samples = %d, want %d", got, len(embedEquivSamples))
	}
}

// TestEmbedEquivalenceDivergent: a genuinely different direction fails the
// threshold — no verified, no metadata_patch.
func TestEmbedEquivalenceDivergent(t *testing.T) {
	stubEquivProbe(t, []float32{1, 0, 0}, []float32{0.7, 0.7, 0.14})
	resp := equivRequest(t, equivTestPool(), unsavedEquivBody())

	if resp["verified"] != false {
		t.Fatalf("verified = %v, want false", resp["verified"])
	}
	if _, present := resp["metadata_patch"]; present {
		t.Error("metadata_patch present on a failed probe — must never arm the flag")
	}
	minCos, _ := resp["min_cosine"].(float64)
	if minCos >= backends.EmbedEquivalenceThreshold {
		t.Errorf("min_cosine = %v, expected below threshold %v", minCos, backends.EmbedEquivalenceThreshold)
	}
}

// TestEmbedEquivalenceNoReference: an empty pool yields the config failure —
// verified:false with a failure message, HTTP 200 (verdict-in-body contract).
func TestEmbedEquivalenceNoReference(t *testing.T) {
	stubEquivProbe(t, []float32{1, 0}, []float32{1, 0})
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest(nil)
	resp := equivRequest(t, p, unsavedEquivBody())

	if resp["verified"] != false {
		t.Fatalf("verified = %v, want false", resp["verified"])
	}
	if resp["failure"] == nil || resp["failure"] == "" {
		t.Error("failure message missing for the no-reference case")
	}
}

// TestEmbedEquivalenceByID: the table case — candidate resolved from the pool
// row, reference excluded from being its own candidate.
func TestEmbedEquivalenceByID(t *testing.T) {
	vec := []float32{0, 1, 0}
	stubEquivProbe(t, vec, vec)
	cand := backends.Backend{
		ID: "cand-id", Name: "cand-embed", Host: "https://cand:4000",
		Protocol: backends.ProtocolOpenAI, Locality: "external", Enabled: true,
		Priority: 40, Roles: []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "cand-model"}},
	}
	resp := equivRequest(t, equivTestPool(cand), map[string]any{
		"action": "backend-test", "id": "cand-id",
		"data": map[string]any{"probe": "embed-equivalence"},
	})

	if resp["verified"] != true {
		t.Fatalf("verified = %v, want true (resp %v)", resp["verified"], resp)
	}
	ref, _ := resp["reference"].(map[string]any)
	if ref["name"] != "ref-embed" {
		t.Errorf("reference = %v, want ref-embed (the candidate must not reference itself)", ref)
	}
}
