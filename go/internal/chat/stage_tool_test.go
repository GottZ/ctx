// D-W6b unit probes for the ctx_store staging tool: the tool is offered ONLY
// by an armed executor, and every outcome shape (staged / gate-reject /
// infrastructure error) is model-safe. The staging itself is integration-
// tested in handler (chat_confirm_integration_test.go) — here the runner is a
// scriptable fake (the QueryRunner test pattern).
package chat

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/llm"
)

type fakeStage struct {
	staged *StagedWrite
	reject string
	err    error
	calls  int
}

func (f *fakeStage) StageWrite(_ context.Context, _, _, _ string, _ []string, _ map[string]any) (*StagedWrite, string, error) {
	f.calls++
	return f.staged, f.reject, f.err
}

// unitFakeQuery is the no-op QueryRunner for these unit tests (the richer
// fakeQuery lives behind the integration build tag).
type unitFakeQuery struct{}

func (unitFakeQuery) RunQuery(context.Context, []string, string, int) (QueryResult, error) {
	return QueryResult{}, nil
}

func storeCall(args string) llm.ToolCall {
	return llm.ToolCall{ID: "c1", Function: llm.ToolCallFunction{Name: "ctx_store", Arguments: json.RawMessage(args)}}
}

func TestDefsOfferCtxStoreOnlyWhenArmed(t *testing.T) {
	ex := NewExecutor(nil, &unitFakeQuery{}, 0)
	for _, d := range ex.Defs() {
		if d.Function.Name == "ctx_store" {
			t.Fatalf("unarmed executor must not offer ctx_store")
		}
	}
	if ex.HasStage() {
		t.Fatalf("unarmed executor reports HasStage")
	}

	armed := NewExecutor(nil, &unitFakeQuery{}, 0).WithStage(&fakeStage{})
	found := false
	for _, d := range armed.Defs() {
		if d.Function.Name == "ctx_store" {
			found = true
		}
	}
	if !found || !armed.HasStage() {
		t.Fatalf("armed executor must offer ctx_store (found=%v, HasStage=%v)", found, armed.HasStage())
	}
	// The base def list stays untouched (read-only wiring unchanged, D3-m1).
	if len(toolDefs) != 4 {
		t.Fatalf("base toolDefs mutated: %d", len(toolDefs))
	}
}

func TestRunStoreStagesAndCarriesTheCardPayload(t *testing.T) {
	exp := time.Now().Add(10 * time.Minute).UTC()
	fs := &fakeStage{staged: &StagedWrite{
		PayloadHash: "abc123", Op: "store", Scope: "private", Category: "test",
		Title: "t", Sensitivity: "personal", ContentPreview: "p", ContentChars: 1, ExpiresAt: &exp,
	}}
	ex := NewExecutor(nil, &unitFakeQuery{}, 0).WithStage(fs)

	out := ex.Run(context.Background(), nil, "key1", storeCall(`{"category":"test","title":"t","content":"c"}`))
	if !out.OK {
		t.Fatalf("staged outcome must be OK=true (the turn continues): %s", out.Content)
	}
	if out.Staged == nil || out.Staged.PayloadHash != "abc123" {
		t.Fatalf("outcome carries no staged card payload: %+v", out.Staged)
	}
	if !strings.Contains(out.Content, "abc123") || !strings.Contains(out.Content, "NOT saved") {
		t.Fatalf("model content must carry hash + not-saved note: %s", out.Content)
	}
	if string(out.Sensitivity) != "personal" {
		t.Fatalf("outcome sensitivity = %q, want the resolved stage sensitivity", out.Sensitivity)
	}
	if fs.calls != 1 {
		t.Fatalf("stage runner calls = %d, want 1", fs.calls)
	}
}

func TestRunStoreGateRejectIsModelVisible(t *testing.T) {
	ex := NewExecutor(nil, &unitFakeQuery{}, 0).WithStage(&fakeStage{reject: "Cannot write to requested scope"})
	out := ex.Run(context.Background(), nil, "key1", storeCall(`{"category":"test","title":"t","content":"c"}`))
	if out.OK || !strings.Contains(out.Content, "Cannot write to requested scope") {
		t.Fatalf("gate reject must be an OK=false outcome carrying the reason: ok=%v content=%s", out.OK, out.Content)
	}
	if out.Staged != nil {
		t.Fatalf("rejected stage must not carry a card payload")
	}
}

func TestRunStoreWithoutRunnerFailsClosed(t *testing.T) {
	ex := NewExecutor(nil, &unitFakeQuery{}, 0)
	out := ex.Run(context.Background(), nil, "key1", storeCall(`{"category":"test","title":"t","content":"c"}`))
	if out.OK || !strings.Contains(out.Content, "not available") {
		t.Fatalf("unarmed ctx_store must fail closed: ok=%v content=%s", out.OK, out.Content)
	}
}
