package dream

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
)

// testChatB is the dream chat backend tuple the EvaluateRelationships tests
// pass in. Host/model never reach the wire — the chatJSON seam intercepts —
// but they flow into the llmlog entry fields.
var testChatB = backends.Backend{Host: "h", APIKey: "k", Model: "m"}

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

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), nil)
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

	_, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want error, got nil")
	}
	if !errorContains(err, "ollama exploded") || !errorContains(err, "evaluate") {
		t.Errorf("error not wrapped as expected: %v", err)
	}
}

func TestEvaluateRelationships_ParseError_PropagatesWrapped(t *testing.T) {
	mockChatJSON(t, constResp("not json at all"))

	_, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	if !errorContains(err, "parse links") {
		t.Errorf("error not wrapped as parse-error: %v", err)
	}
}

func TestEvaluateRelationships_HallucinatedUUID_BadFormat_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"not-a-uuid","type":"topical","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
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

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect hallucinated-but-not-candidate UUID filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_InvalidRelationship_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidB+`","type":"similar","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("expect invalid relationship filtered, got %+v", links)
	}
}

func TestEvaluateRelationships_ConfidenceAboveOne_Filtered(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidB+`","type":"topical","confidence":1.5}]`))

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
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

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
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

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{},
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
	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, source, candidates)
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

	links, err := EvaluateRelationships(context.Background(), nil, testChatB, llm.Options{}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("want 0 links, got %+v", links)
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
		{TargetID: "bad-format", Relationship: "topical", Confidence: 0.9},      // bad UUID
		{TargetID: uuidF, Relationship: "topical", Confidence: 0.9},             // not in candidates
		{TargetID: uuidA, Relationship: "similar", Confidence: 0.9},             // bad relationship
		{TargetID: uuidB, Relationship: "topical", Confidence: 1.5},             // > 1.0
		{TargetID: uuidC, Relationship: "topical", Confidence: math.NaN()},      // NaN
		{TargetID: uuidD, Relationship: "topical", Confidence: 0.65},            // below threshold
		{TargetID: uuidE, Relationship: "topical", Confidence: 0.85},            // valid
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
