package topiclabel

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

// TestChainCallKeepsLabelBudget pins the label call's output cap on the wire:
// the digest role's model_map max_tokens is sized for the daily report and
// must not widen the 128-token budget of a one-object label answer.
func TestChainCallKeepsLabelBudget(t *testing.T) {
	var sent map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &sent)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"{\"label\":\"Retrieval\"}"},"finish_reason":"stop"}],"usage":{"completion_tokens":5,"prompt_tokens":5}}`))
	}))
	defer srv.Close()

	pool := backends.NewPool(nil, nil)
	pool.SeedSnapshotForTest([]backends.Backend{{
		ID: "d", Name: "d", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Locality: "lan",
		Roles: []string{backends.RoleDigest},
		ModelMap: map[string]backends.ModelSpec{
			backends.RoleDigest: {Model: "m", Params: map[string]any{"max_tokens": float64(16000)}},
		},
		Priority: 100, Enabled: true,
	}})
	disp := dispatch.New(nil, dispatch.DefaultSettings())
	defer disp.Close()

	d := Deps{Backends: pool, Adm: llm.Admission{Admitter: disp, Class: dispatch.ClassBackground}}
	answer, _, err := ChainCall(context.Background(), d, backends.SensPublic, "sys", "user", nil)
	if err != nil {
		t.Fatalf("ChainCall: %v", err)
	}
	if answer == "" {
		t.Fatal("empty answer from stub")
	}
	if sent["max_tokens"] != float64(128) {
		t.Errorf("max_tokens on the wire = %v, want 128 (model_map 16000 must not apply)", sent["max_tokens"])
	}
}
