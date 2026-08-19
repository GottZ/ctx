package dream

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
)

// newTestRouter routes the dream tests through a seeded single-backend pool
// (G28). Host/model never reach the wire — the chatJSON seam intercepts —
// but they flow through the chain resolution into the llmlog entry fields.
// full-trust so the zero-value sensitivity of the test fixtures (acting as
// credentials, fail-closed) still resolves a chain.
func newTestRouter() *Router {
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{{
		ID: "test-backend-id", Name: "test-backend",
		Host: "h", APIKey: "k",
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream, backends.RoleDigest},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
		Priority: 100, Enabled: true,
	}})
	return &Router{Pool: p, Admit: testAdmit()}
}

// mockChatJSON installs fn as the package-level chatJSON seam for the duration
// of the test. The original implementation is restored on cleanup.
func mockChatJSON(t *testing.T, fn func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error)) {
	t.Helper()
	saved := chatJSON
	chatJSON = fn
	t.Cleanup(func() { chatJSON = saved })
}

// constResp returns a chatJSON stub that ignores its inputs and returns a
// fixed Message.Content (and nil error). Used for happy-path JSON injection.
func constResp(content string) func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
	return func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: content},
			EvalCount:    10,
			PromptTokens: 100,
		}, nil
	}
}

func srcBlock(id string) BlockInfo {
	return BlockInfo{
		ID:        id,
		Title:     "src",
		Category:  "decisions",
		Content:   "source content",
		Scope:     "private",
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func candBlock(id string) BlockInfo {
	return BlockInfo{
		ID:        id,
		Title:     "cand",
		Category:  "decisions",
		Content:   "candidate content",
		Scope:     "private",
		UpdatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
		CreatedAt: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
	}
}

const (
	uuidA = "019d0000-0000-7000-8000-000000000001"
	uuidB = "019d0000-0000-7000-8000-000000000002"
	uuidC = "019d0000-0000-7000-8000-000000000003"
	uuidD = "019d0000-0000-7000-8000-000000000004"
	uuidE = "019d0000-0000-7000-8000-000000000005"
	uuidF = "019d0000-0000-7000-8000-000000000006"
	uuidG = "019d0000-0000-7000-8000-000000000007"
)

// --- EvaluateRelationships Mock-LLM Tests (S26 Welle 2) ---.

func TestEvaluateRelationships_EmptyCandidates_NoLLMCall(t *testing.T) {
	called := false
	mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		called = true
		return nil, nil
	})

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if links != nil {
		t.Errorf("want nil, got %+v", links)
	}
	if called {
		t.Error("LLM must not be invoked when candidates is empty")
	}
}

func TestEvaluateRelationships_LLMError_PropagatesWrapped(t *testing.T) {
	mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return nil, errors.New("ollama exploded")
	})

	_, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errorContains(err, "ollama exploded") || !errorContains(err, "evaluate") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}

func TestEvaluateRelationships_ParseError_PropagatesWrapped(t *testing.T) {
	mockChatJSON(t, constResp("not json at all"))

	_, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	if !errorContains(err, "parse links") {
		t.Errorf("error not wrapped as parse-error: %v", err)
	}
}

func TestEvaluateRelationships_HallucinatedUUID_BadFormat_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"not-a-uuid","type":"topical","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect bad-format UUID filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_HallucinatedUUID_NotInCandidateSet_Filtered(t *testing.T) {
	// LLM emits a syntactically valid UUID that is NOT in the candidate set.
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidG+`","type":"topical","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect hallucinated-but-not-candidate UUID filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_InvalidRelationship_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidB+`","type":"similar","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect invalid relationship filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_ConfidenceAboveOne_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidB+`","type":"topical","confidence":1.5}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect confidence>1.0 filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_BelowThreshold_Filtered(t *testing.T) {
	// minRawConfidence is 0.7 across all types (S25 audit-confirmed).
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidB+`","type":"topical","confidence":0.65}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect below-threshold filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_HappyPath_PassesThrough(t *testing.T) {
	mockChatJSON(t, constResp(`[
		{"target_id":"`+uuidB+`","type":"topical","confidence":0.85},
		{"target_id":"`+uuidC+`","type":"factual","confidence":0.9}
	]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{},
		srcBlock(uuidA),
		[]BlockInfo{candBlock(uuidB), candBlock(uuidC)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 2 {
		t.Fatalf("want 2 links, got %d: %+v", len(links), links)
	}
}

func TestEvaluateRelationships_HardCapEngaged_TrimsToFive(t *testing.T) {
	// 7 candidates, all at conf 0.9 — hard-cap of 5 must drop 2.
	mockChatJSON(t, constResp(`[
		{"target_id":"`+uuidA+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidB+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidC+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidD+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidE+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidF+`","type":"topical","confidence":0.9},
		{"target_id":"`+uuidG+`","type":"topical","confidence":0.9}
	]`))

	source := srcBlock("019d0000-0000-7000-8000-000000000000")
	candidates := []BlockInfo{
		candBlock(uuidA), candBlock(uuidB), candBlock(uuidC), candBlock(uuidD),
		candBlock(uuidE), candBlock(uuidF), candBlock(uuidG),
	}
	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, source, candidates)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != MaxLinksPerCycle {
		t.Errorf("hard-cap not engaged: got %d links, want %d", len(links), MaxLinksPerCycle)
	}
}

func TestEvaluateRelationships_AllCandidatesNone_EmptyResult(t *testing.T) {
	// LLM legitimately returns "[]" for unrelated blocks — must not error.
	mockChatJSON(t, constResp(`[]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("want 0 links, got %+v", links)
	}
}

// --- Output-cap detection (noteCapHit) ---.

// truncatedObjectMap is a real-shaped dream-eval answer cut off mid-JSON: the
// object-map drift form (form 2) with the second key started but never closed,
// exactly how a cap hit presents itself to parseLinks.
const truncatedObjectMap = `{"` + uuidB + `":{"target_id":"` + uuidB + `","type":"topical","confidence":0.9}, "` + uuidC

// capRespFunc returns a chatJSON stub that answers with content and reports
// evalCount completion tokens, so a test can put the backend's own token count
// exactly at (or below) the cap it was handed.
func capRespFunc(content string, evalCount int) func(ctx context.Context, host, apiKey, model string, think *bool, systemPrompt, userPrompt string, opts llm.Options, timeout time.Duration) (*llm.ChatResponse, error) {
	return func(_ context.Context, _, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: content},
			EvalCount:    evalCount,
			PromptTokens: 100,
		}, nil
	}
}

// TestEvaluateRelationships_CapHitStillErrors pins that the cap-hit flag is
// OBSERVABILITY ONLY: a truncated answer that consumed the whole budget still
// returns the parse error, so the cooldown/re-pick path is untouched.
func TestEvaluateRelationships_CapHitStillErrors(t *testing.T) {
	opts := llm.Options{NumPredict: 600}
	mockChatJSON(t, capRespFunc(truncatedObjectMap, opts.NumPredict))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), opts, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want parse error for truncated object-map JSON, got nil")
	}
	if !errorContains(err, "parse links") {
		t.Errorf("error not wrapped as parse-error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("want no links from a truncated answer, got %+v", links)
	}
}

// TestNoteCapHit drives the verdict itself. It runs against the helper rather
// than through EvaluateRelationships because the llmlog entry that carries the
// metadata is function-local and llmlog.Record is a no-op on a nil pool — the
// package has no in-process recorder seam, only the DB-backed waitLlmlogRows
// helper in the integration tests.
func TestNoteCapHit(t *testing.T) {
	tests := []struct {
		name       string
		evalCount  int
		numPredict int
		want       bool
	}{
		// Generation stopped exactly at the budget — the cap hit.
		{"exactly-at-cap", 600, 600, true},
		// Some backends count the stop token in, so > is a hit too.
		{"over-cap", 601, 600, true},
		// Malformed but well short of the budget: a real parse failure.
		{"below-cap", 42, 600, false},
		{"one-below-cap", 599, 600, false},
		// Uncapped request (dailySynthesisOptions shape): nothing to hit.
		{"uncapped", 900, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			resp := &llm.ChatResponse{EvalCount: tt.evalCount}
			got := noteCapHit(entry, resp, llm.Options{NumPredict: tt.numPredict})
			if got != tt.want {
				t.Fatalf("noteCapHit = %v, want %v", got, tt.want)
			}
			if tt.want {
				if entry.Metadata["cap_hit"] != true {
					t.Errorf("metadata cap_hit = %v, want true", entry.Metadata["cap_hit"])
				}
			} else if _, ok := entry.Metadata["cap_hit"]; ok {
				t.Errorf("cap_hit must not be set below the cap, got %+v", entry.Metadata)
			}
		})
	}
}

// TestNoteCapHit_NilInputs pins the guards: a nil entry or a nil response (the
// LLM-error path, where resp is nil) must never be a cap hit and never panic.
func TestNoteCapHit_NilInputs(t *testing.T) {
	if noteCapHit(nil, &llm.ChatResponse{EvalCount: 600}, llm.Options{NumPredict: 600}) {
		t.Error("nil entry must not report a cap hit")
	}
	if noteCapHit(&llmlog.Entry{Pipeline: "dream-eval"}, nil, llm.Options{NumPredict: 600}) {
		t.Error("nil response must not report a cap hit")
	}
}

// TestEvaluateRelationships_LinksParsedRecorded checks the counted-verdict side
// of the same wave through the helper's sibling metadata key. The count itself
// is asserted on the returned links (the entry is not reachable, see
// TestNoteCapHit) — this pins that a well-formed answer under the cap takes the
// success path and yields the parsed links.
func TestEvaluateRelationships_LinksParsedRecorded(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    int
	}{
		{"two-links", `[{"target_id":"` + uuidB + `","type":"topical","confidence":0.85},{"target_id":"` + uuidC + `","type":"factual","confidence":0.9}]`, 2},
		{"zero-link-verdict", `[]`, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := llm.Options{NumPredict: 600}
			mockChatJSON(t, capRespFunc(tt.content, 120))

			links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), opts,
				srcBlock(uuidA), []BlockInfo{candBlock(uuidB), candBlock(uuidC)})
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if len(links) != tt.want {
				t.Fatalf("want %d links, got %d: %+v", tt.want, len(links), links)
			}
			// The same inputs the success path hands noteCapHit must NOT
			// read as a cap hit — 120 tokens is far under the 600 budget.
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			if noteCapHit(entry, &llm.ChatResponse{EvalCount: 120}, opts) {
				t.Error("a well-formed answer under the cap must not be flagged cap_hit")
			}
		})
	}
}

// --- filterValidCandidates Pure-Function Tests (covers NaN/Inf branches) ---.

func TestFilterValidCandidates_NaNConfidence_Dropped(t *testing.T) {
	cands := map[string]bool{uuidA: true}
	in := []Link{{TargetID: uuidA, Relationship: "topical", Confidence: math.NaN()}}
	out := filterValidCandidates(in, cands)
	if len(out) != 0 {
		t.Errorf("NaN must be dropped, got %+v", out)
	}
}

func TestFilterValidCandidates_PosInfConfidence_Dropped(t *testing.T) {
	cands := map[string]bool{uuidA: true}
	in := []Link{{TargetID: uuidA, Relationship: "topical", Confidence: math.Inf(1)}}
	out := filterValidCandidates(in, cands)
	if len(out) != 0 {
		t.Errorf("+Inf must be dropped, got %+v", out)
	}
}

func TestFilterValidCandidates_NegInfConfidence_Dropped(t *testing.T) {
	cands := map[string]bool{uuidA: true}
	in := []Link{{TargetID: uuidA, Relationship: "topical", Confidence: math.Inf(-1)}}
	out := filterValidCandidates(in, cands)
	if len(out) != 0 {
		t.Errorf("-Inf must be dropped, got %+v", out)
	}
}

func TestFilterValidCandidates_AllPathsCovered(t *testing.T) {
	cands := map[string]bool{uuidA: true, uuidB: true, uuidC: true, uuidD: true, uuidE: true}
	in := []Link{
		{TargetID: "bad-format", Relationship: "topical", Confidence: 0.9}, // bad UUID
		{TargetID: uuidF, Relationship: "topical", Confidence: 0.9},        // not in candidates
		{TargetID: uuidA, Relationship: "similar", Confidence: 0.9},        // bad relationship
		{TargetID: uuidB, Relationship: "topical", Confidence: 1.5},        // > 1.0
		{TargetID: uuidC, Relationship: "topical", Confidence: math.NaN()}, // NaN
		{TargetID: uuidD, Relationship: "topical", Confidence: 0.65},       // below threshold
		{TargetID: uuidE, Relationship: "topical", Confidence: 0.85},       // valid
	}
	out := filterValidCandidates(in, cands)
	if len(out) != 1 || out[0].TargetID != uuidE {
		t.Fatalf("want only uuidE survives, got %+v", out)
	}
}

// --- helpers ---.

func errorContains(err error, substr string) bool {
	if err == nil {
		return false
	}
	return containsStr(err.Error(), substr)
}
