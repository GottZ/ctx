package dream

import (
	"context"
	"errors"
	"fmt"
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

// --- Output-cap detection and the bounded retry (issue #26) ---.

// truncatedObjectMap is a real-shaped dream-eval answer cut off mid-JSON: the
// object-map drift form (form 2) with the second key started but never closed,
// exactly how a cap hit presents itself to parseLinks.
const truncatedObjectMap = `{"` + uuidB + `":{"target_id":"` + uuidB + `","type":"topical","confidence":0.9}, "` + uuidC

// validArray is a well-formed answer in the shape the prompt asks for — what
// the retry is supposed to get once the cap is wide enough.
const validArray = `[{"target_id":"` + uuidB + `","type":"topical","confidence":0.85}]`

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

// scriptedAnswer is one entry of a per-call script: what the backend answers
// on attempt n.
type scriptedAnswer struct {
	content      string
	evalCount    int
	finishReason string
}

// scriptedChat installs a chatJSON seam that walks answers in order and
// records the Options of every call. The recorded Options are the ones the
// chain walk RESOLVED (llm.ChatChainVia hands applyModelParams' output to the
// seam), so a test can assert the effective output cap of each attempt rather
// than the caller's intent.
func scriptedChat(t *testing.T, answers ...scriptedAnswer) *[]llm.Options {
	t.Helper()
	seen := make([]llm.Options, 0, len(answers))
	mockChatJSON(t, func(_ context.Context, _, _, _ string, _ *bool, _, _ string, opts llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		seen = append(seen, opts)
		a := answers[len(answers)-1]
		if len(seen) <= len(answers) {
			a = answers[len(seen)-1]
		}
		return &llm.ChatResponse{
			Message:      llm.Message{Role: "assistant", Content: a.content},
			EvalCount:    a.evalCount,
			PromptTokens: 100,
			FinishReason: a.finishReason,
		}, nil
	})
	return &seen
}

// retryRouter is newTestRouter with the cap-hit retry armed at factor.
func retryRouter(factor float64) *Router {
	r := newTestRouter()
	r.CapRetryFactor = factor
	return r
}

// TestEvaluateRelationships_CapHitRetriesAtScaledCap is spec test 1: a
// truncated first answer is re-asked ONCE at factor x the cap, and the second
// answer's links are returned.
//
// The cap assertion is on opts.NumPredict at the seam — 600 on call 1, 1200 on
// call 2 — not on NumPredictScale. The scale field only proves it was copied;
// the number the backend is actually told is the behaviour, and it exists only
// after applyModelParams ran inside the chain walk.
func TestEvaluateRelationships_CapHitRetriesAtScaledCap(t *testing.T) {
	opts := llm.Options{NumPredict: 600}
	seen := scriptedChat(t,
		scriptedAnswer{truncatedObjectMap, 600, "length"},
		scriptedAnswer{validArray, 300, "stop"},
	)

	links, err := EvaluateRelationships(context.Background(), nil, retryRouter(2), opts,
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("the retry answer parses — want no error, got %v", err)
	}
	if len(links) != 1 {
		t.Fatalf("want the retry's 1 link, got %+v", links)
	}
	if len(*seen) != 2 {
		t.Fatalf("want exactly 2 wire calls, got %d", len(*seen))
	}
	if got := (*seen)[0].NumPredict; got != 600 {
		t.Errorf("call 1 sent num_predict %d, want the base 600", got)
	}
	if got := (*seen)[1].NumPredict; got != 1200 {
		t.Errorf("call 2 sent num_predict %d, want the scaled 1200", got)
	}
}

// TestEvaluateRelationships_DoubleCapHit_Sentinel is spec test 2: the retry
// hits the cap again, so the error carries ErrOutputCapHit — the signal
// RunDreamCycle books as a completed-but-inert eval. Exactly two calls: the
// retry is bounded, never a loop.
func TestEvaluateRelationships_DoubleCapHit_Sentinel(t *testing.T) {
	seen := scriptedChat(t,
		scriptedAnswer{truncatedObjectMap, 600, "length"},
		scriptedAnswer{truncatedObjectMap, 1200, "length"},
	)

	links, err := EvaluateRelationships(context.Background(), nil, retryRouter(2),
		llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want an error after two cap hits, got nil")
	}
	if !errors.Is(err, ErrOutputCapHit) {
		t.Errorf("second cap hit must carry ErrOutputCapHit, got %v", err)
	}
	if !errorContains(err, "parse links") {
		t.Errorf("the underlying parse error must survive the wrap: %v", err)
	}
	if len(links) != 0 {
		t.Errorf("want no links, got %+v", links)
	}
	if len(*seen) != 2 {
		t.Errorf("want exactly 2 wire calls (bounded retry), got %d", len(*seen))
	}
}

// TestEvaluateRelationships_RetryDisabled_PlainParseError is spec test 3 and
// the kill-switch pin: with dream.eval_cap_retry_factor <= 1 the function
// behaves exactly as it did before the retry existed — ONE call, the plain
// parse error, no sentinel, so RunDreamCycle books the 5-minute transient
// cooldown and re-picks. It also covers the zero value, i.e. every Router
// built without config wiring.
func TestEvaluateRelationships_RetryDisabled_PlainParseError(t *testing.T) {
	for _, factor := range []float64{0, 1} {
		t.Run(fmt.Sprintf("factor-%g", factor), func(t *testing.T) {
			seen := scriptedChat(t, scriptedAnswer{truncatedObjectMap, 600, "length"})

			links, err := EvaluateRelationships(context.Background(), nil, retryRouter(factor),
				llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
			if err == nil {
				t.Fatal("want parse error for truncated object-map JSON, got nil")
			}
			if !errorContains(err, "parse links") {
				t.Errorf("error not wrapped as parse-error: %v", err)
			}
			if errors.Is(err, ErrOutputCapHit) {
				t.Error("a cap hit that was never retried must not escalate to the inert booking")
			}
			if len(links) != 0 {
				t.Errorf("want no links from a truncated answer, got %+v", links)
			}
			if len(*seen) != 1 {
				t.Errorf("want exactly 1 wire call with the retry off, got %d", len(*seen))
			}
		})
	}
}

// TestEvaluateRelationships_OrdinaryParseFailure_NoRetry is spec test 4: a
// malformed answer well under the cap, with the provider reporting a natural
// stop, is ordinary drift — one call, plain parse error, no sentinel. Pins
// that the retry does not double the cost of every parse failure.
func TestEvaluateRelationships_OrdinaryParseFailure_NoRetry(t *testing.T) {
	seen := scriptedChat(t, scriptedAnswer{`not json at all`, 42, "stop"})

	_, err := EvaluateRelationships(context.Background(), nil, retryRouter(2),
		llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want a parse error, got nil")
	}
	if errors.Is(err, ErrOutputCapHit) {
		t.Errorf("ordinary drift must not be classified as a cap hit: %v", err)
	}
	if len(*seen) != 1 {
		t.Errorf("want exactly 1 wire call, got %d", len(*seen))
	}
}

// TestEvaluateRelationships_RetryFailsBelowScaledCap_NoSentinel is spec test
// 4's twin for the RETRY attempt, and the regression guard for the heuristic's
// reference cap. The retry answers malformed with finish_reason "stop" and 900
// completion tokens — above the base 600, below the scaled 1200. Compared
// against the BASE cap (which is what opts.NumPredict still holds on this
// side of the chain walk) that would read as a second cap hit and book the
// block inert for an ordinary parse failure. It must stay a plain parse error.
func TestEvaluateRelationships_RetryFailsBelowScaledCap_NoSentinel(t *testing.T) {
	seen := scriptedChat(t,
		scriptedAnswer{truncatedObjectMap, 600, "length"},
		scriptedAnswer{`{"garbage": `, 900, "stop"},
	)

	_, err := EvaluateRelationships(context.Background(), nil, retryRouter(2),
		llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err == nil {
		t.Fatal("want a parse error from the malformed retry answer, got nil")
	}
	if errors.Is(err, ErrOutputCapHit) {
		t.Errorf("a retry failure between the base and the scaled cap is not a second cap hit: %v", err)
	}
	if len(*seen) != 2 {
		t.Errorf("want exactly 2 wire calls, got %d", len(*seen))
	}
}

// TestEvaluateRelationships_NoStopReason_RetriesOnTokenHeuristic is spec test
// 5: the provider reports no stop reason at all (the common OpenAI-compatible
// case), so the token heuristic carries the verdict and the retry still fires.
func TestEvaluateRelationships_NoStopReason_RetriesOnTokenHeuristic(t *testing.T) {
	seen := scriptedChat(t,
		scriptedAnswer{truncatedObjectMap, 600, ""},
		scriptedAnswer{validArray, 300, ""},
	)

	links, err := EvaluateRelationships(context.Background(), nil, retryRouter(2),
		llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(links) != 1 || len(*seen) != 2 {
		t.Fatalf("want 1 link from 2 calls, got %d links from %d calls", len(links), len(*seen))
	}
	// The source of that verdict is only observable on the helper: the llmlog
	// entry is function-local and llmlog.Record is a no-op on a nil pool (see
	// TestCapHit).
	entry := &llmlog.Entry{Pipeline: "dream-eval"}
	if !capHit(entry, &llm.ChatResponse{EvalCount: 600}, llm.Options{NumPredict: 600}) {
		t.Fatal("a silent provider at the budget must still read as a cap hit")
	}
	if entry.Metadata["cap_hit_source"] != "tokens" {
		t.Errorf("cap_hit_source = %v, want tokens", entry.Metadata["cap_hit_source"])
	}
}

// TestEvaluateRelationships_ExtraBodyMaxTokensRow_SkipsRetry pins verifier
// correction 2: on a row whose extra_body pins max_tokens, the scaled
// Options.NumPredict never reaches the wire (extra_body is merged last), so
// the retry would send the identical cap, hit it again and book the block
// inert for a row setting. The retry is skipped and the plain parse error
// returned — one call, no sentinel.
func TestEvaluateRelationships_ExtraBodyMaxTokensRow_SkipsRetry(t *testing.T) {
	for _, pinned := range []any{float64(600), 600} {
		t.Run(fmt.Sprintf("%T", pinned), func(t *testing.T) {
			r := retryRouter(2)
			p := backends.NewPool(nil, nil)
			p.SeedSnapshotForTest([]backends.Backend{{
				ID: "pinned-backend-id", Name: "pinned-backend",
				Host: "h", APIKey: "k",
				Trust: backends.TrustFull, Locality: "lan",
				Roles:     []string{backends.RoleDream},
				ModelMap:  map[string]backends.ModelSpec{"default": {Model: "m"}},
				ExtraBody: map[string]any{"max_tokens": pinned},
				Priority:  100, Enabled: true,
			}})
			r.Pool = p
			seen := scriptedChat(t, scriptedAnswer{truncatedObjectMap, 600, "length"})

			_, err := EvaluateRelationships(context.Background(), nil, r,
				llm.Options{NumPredict: 600}, srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
			if err == nil {
				t.Fatal("want the plain parse error, got nil")
			}
			if errors.Is(err, ErrOutputCapHit) {
				t.Error("a retry that cannot take effect must not escalate to the inert booking")
			}
			if len(*seen) != 1 {
				t.Errorf("want exactly 1 wire call (retry skipped), got %d", len(*seen))
			}
		})
	}
}

// TestCapHit drives the verdict itself. It runs against the helper rather
// than through EvaluateRelationships because the llmlog entry that carries the
// metadata is function-local and llmlog.Record is a no-op on a nil pool — the
// package has no in-process recorder seam, only the DB-backed waitLlmlogRows
// helper in the integration tests.
func TestCapHit(t *testing.T) {
	tests := []struct {
		name       string
		evalCount  int
		numPredict int
		scale      float64
		finish     string
		want       bool
		wantSource string
	}{
		// The provider states it outright — token count irrelevant.
		{"finish-reason-length", 12, 600, 0, "length", true, "finish_reason"},
		{"finish-reason-length-cased", 12, 600, 0, "LENGTH", true, "finish_reason"},
		// A stated natural stop is an answer: the token count must not
		// overrule it, or the retry attempt below would misfire.
		{"finish-reason-stop-at-cap", 600, 600, 0, "stop", false, ""},
		{"finish-reason-tool-calls", 900, 600, 0, "tool_calls", false, ""},
		// Provider silent: the heuristic carries it.
		{"silent-exactly-at-cap", 600, 600, 0, "", true, "tokens"},
		// Some backends count the stop token in, so > is a hit too.
		{"silent-over-cap", 601, 600, 0, "", true, "tokens"},
		// Malformed but well short of the budget: a real parse failure.
		{"silent-below-cap", 42, 600, 0, "", false, ""},
		{"silent-one-below-cap", 599, 600, 0, "", false, ""},
		// Uncapped request (dailySynthesisOptions shape): nothing to hit.
		{"silent-uncapped", 900, 0, 0, "", false, ""},
		// THE retry rows: opts.NumPredict is still the base cap on this side
		// of the chain walk, so the comparison must scale it. 900 tokens under
		// a 600x2 cap is not a cap hit; 1200 is.
		{"retry-below-scaled-cap", 900, 600, 2, "", false, ""},
		{"retry-at-scaled-cap", 1200, 600, 2, "", true, "tokens"},
		// A scale <= 1 leaves the reference cap alone.
		{"scale-one-is-base-cap", 600, 600, 1, "", true, "tokens"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			resp := &llm.ChatResponse{EvalCount: tt.evalCount, FinishReason: tt.finish}
			got := capHit(entry, resp, llm.Options{NumPredict: tt.numPredict, NumPredictScale: tt.scale})
			if got != tt.want {
				t.Fatalf("capHit = %v, want %v", got, tt.want)
			}
			if tt.want {
				if entry.Metadata["cap_hit"] != true {
					t.Errorf("metadata cap_hit = %v, want true", entry.Metadata["cap_hit"])
				}
				if entry.Metadata["cap_hit_source"] != tt.wantSource {
					t.Errorf("cap_hit_source = %v, want %q", entry.Metadata["cap_hit_source"], tt.wantSource)
				}
			} else if _, ok := entry.Metadata["cap_hit"]; ok {
				t.Errorf("cap_hit must not be set here, got %+v", entry.Metadata)
			}
		})
	}
}

// TestCapHit_NilInputs pins the guards: a nil entry or a nil response (the
// LLM-error path, where resp is nil) must never be a cap hit and never panic.
func TestCapHit_NilInputs(t *testing.T) {
	if capHit(nil, &llm.ChatResponse{EvalCount: 600, FinishReason: "length"}, llm.Options{NumPredict: 600}) {
		t.Error("nil entry must not report a cap hit")
	}
	if capHit(&llmlog.Entry{Pipeline: "dream-eval"}, nil, llm.Options{NumPredict: 600}) {
		t.Error("nil response must not report a cap hit")
	}
}

// TestEffectiveCap pins the reference cap the token heuristic compares
// against: the base cap times a scale above 1, the base cap otherwise.
func TestEffectiveCap(t *testing.T) {
	tests := []struct {
		name string
		opts llm.Options
		want int
	}{
		{"unscaled", llm.Options{NumPredict: 600}, 600},
		{"scale-below-one", llm.Options{NumPredict: 600, NumPredictScale: 0.5}, 600},
		{"scale-one", llm.Options{NumPredict: 600, NumPredictScale: 1}, 600},
		{"scale-two", llm.Options{NumPredict: 600, NumPredictScale: 2}, 1200},
		{"scale-fractional", llm.Options{NumPredict: 600, NumPredictScale: 1.5}, 900},
		{"uncapped", llm.Options{NumPredictScale: 2}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := effectiveCap(tt.opts); got != tt.want {
				t.Errorf("effectiveCap = %d, want %d", got, tt.want)
			}
		})
	}
}

// TestRowPinsMaxTokens pins the retry-suppression predicate: only a POSITIVE
// numeric extra_body.max_tokens (both JSONB round-trip shapes) suppresses the
// retry. Anything else falls through to the retry, which is the conservative
// branch.
func TestRowPinsMaxTokens(t *testing.T) {
	if rowPinsMaxTokens(nil) {
		t.Error("a nil served backend must not suppress the retry")
	}
	tests := []struct {
		name string
		body map[string]any
		want bool
	}{
		{"no-extra-body", map[string]any{}, false},
		{"float-shape", map[string]any{"max_tokens": float64(4096)}, true},
		{"int-shape", map[string]any{"max_tokens": 4096}, true},
		{"zero-is-no-pin", map[string]any{"max_tokens": float64(0)}, false},
		{"negative-is-no-pin", map[string]any{"max_tokens": -1}, false},
		{"string-is-not-a-cap-we-can-read", map[string]any{"max_tokens": "4096"}, false},
		{"other-keys-ignored", map[string]any{"provider": map[string]any{"zdr": true}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := rowPinsMaxTokens(&backends.Backend{ExtraBody: tt.body}); got != tt.want {
				t.Errorf("rowPinsMaxTokens = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestNoteCandidatesCapped pins the aggregate-candidate-cap counter that
// searchByKeywords hands to evaluateRelationships (PR #36 hardening). Same
// reachability constraint as TestCapHit: the entry never leaves
// evaluateRelationships, so the stamp is tested as its own function.
func TestNoteCandidatesCapped(t *testing.T) {
	tests := []struct {
		name   string
		capped int
		want   bool // key present on the entry
	}{
		// The cycle collected fewer than the cap allows — the overwhelming
		// majority, and the reason 0 must NOT be written.
		{"cap-did-not-bind", 0, false},
		// The production worst case the PR reported: 29 collected, 25 kept.
		{"worst-case-overshoot", 4, true},
		{"single-drop", 1, true},
		// Defensive: a negative count is a caller bug, never a metadata row.
		{"negative-is-ignored", -1, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			noteCandidatesCapped(entry, tt.capped)
			got, ok := entry.Metadata["candidates_capped"]
			if ok != tt.want {
				t.Fatalf("candidates_capped present = %v, want %v (metadata %+v)", ok, tt.want, entry.Metadata)
			}
			if tt.want && got != tt.capped {
				t.Errorf("candidates_capped = %v, want %d", got, tt.capped)
			}
		})
	}
}

// TestNoteCandidatesCapped_KeepsSiblingMetadata pins that the stamp lands
// beside the keys evaluateRelationships writes later (parse_format,
// links_parsed, links_capped) instead of replacing the map, and that a nil
// entry — the shape every other note* helper tolerates — does not panic.
func TestNoteCandidatesCapped_KeepsSiblingMetadata(t *testing.T) {
	entry := &llmlog.Entry{Pipeline: "dream-eval", Metadata: map[string]any{"parse_format": "array"}}
	noteCandidatesCapped(entry, 3)
	if entry.Metadata["parse_format"] != "array" {
		t.Errorf("sibling metadata lost: %+v", entry.Metadata)
	}
	if entry.Metadata["candidates_capped"] != 3 {
		t.Errorf("candidates_capped = %v, want 3", entry.Metadata["candidates_capped"])
	}
	noteCandidatesCapped(nil, 3) // must not panic
}

// TestEvaluateRelationships_LinksParsedRecorded checks the counted-verdict side
// of the same wave through the helper's sibling metadata key. The count itself
// is asserted on the returned links (the entry is not reachable, see
// TestCapHit) — this pins that a well-formed answer under the cap takes the
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
			// The same inputs the success path hands capHit must NOT
			// read as a cap hit — 120 tokens is far under the 600 budget.
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			if capHit(entry, &llm.ChatResponse{EvalCount: 120}, opts) {
				t.Error("a well-formed answer under the cap must not be flagged cap_hit")
			}
		})
	}
}

// --- Candidate-filter drop counter (noteDroppedInvalid, issue #27) ---.

// TestNoteDroppedInvalid pins the counter that makes filterValidCandidates'
// five bare continues countable. Driven as its own function for the
// reachability reason TestCapHit documents: the llmlog entry never leaves
// evaluateRelationships.
func TestNoteDroppedInvalid(t *testing.T) {
	tests := []struct {
		name        string
		parsed      int
		valid       int
		wantWritten bool
		wantValue   int
	}{
		// The healthy majority — every parsed link survived, so no key.
		{"nothing-dropped", 3, 3, false, 0},
		// The silent-inert signature: the model answered, nothing survived.
		{"all-dropped", 2, 0, true, 2},
		{"partial-drop", 5, 3, true, 2},
		// Zero-link verdict ("[]"): nothing parsed, nothing dropped.
		{"zero-link-verdict", 0, 0, false, 0},
		// Defensive: more survivors than parsed is a caller bug, not a row.
		{"negative-is-ignored", 1, 2, false, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			entry := &llmlog.Entry{Pipeline: "dream-eval"}
			written := noteDroppedInvalid(entry, tt.parsed, tt.valid)
			if written != tt.wantWritten {
				t.Fatalf("noteDroppedInvalid = %v, want %v", written, tt.wantWritten)
			}
			got, ok := entry.Metadata["links_dropped_invalid"]
			if ok != tt.wantWritten {
				t.Fatalf("links_dropped_invalid present = %v, want %v (metadata %+v)", ok, tt.wantWritten, entry.Metadata)
			}
			if tt.wantWritten && got != tt.wantValue {
				t.Errorf("links_dropped_invalid = %v, want %d", got, tt.wantValue)
			}
		})
	}
}

// TestNoteDroppedInvalid_KeepsSiblingMetadata pins that the stamp lands beside
// the keys evaluateRelationships writes before it (parse_format, links_parsed)
// instead of replacing the map, and that a nil entry does not panic.
func TestNoteDroppedInvalid_KeepsSiblingMetadata(t *testing.T) {
	entry := &llmlog.Entry{Pipeline: "dream-eval", Metadata: map[string]any{"links_parsed": 4}}
	noteDroppedInvalid(entry, 4, 1)
	if entry.Metadata["links_parsed"] != 4 {
		t.Errorf("sibling metadata lost: %+v", entry.Metadata)
	}
	if entry.Metadata["links_dropped_invalid"] != 3 {
		t.Errorf("links_dropped_invalid = %v, want 3", entry.Metadata["links_dropped_invalid"])
	}
	if noteDroppedInvalid(nil, 4, 1) {
		t.Error("nil entry must not report a write")
	}
}

// TestEvaluateRelationships_AllLinksDropped_CountedNotErrored drives the whole
// function with an answer whose single UUID is well-formed but outside the
// candidate set — the hallucination case that ends as a zero-link success and
// books the inert back-off. Two things are pinned: the behaviour does NOT
// change (0 links, nil error), and the numbers that reach the counter on this
// path produce the metadata key. The stamp is asserted on the helper because
// the entry is function-local and llmlog.Record is a no-op on a nil pool (see
// TestCapHit) — the same split TestEvaluateRelationships_LinksParsedRecorded
// uses.
func TestEvaluateRelationships_AllLinksDropped_CountedNotErrored(t *testing.T) {
	mockChatJSON(t, constResp(`[{"target_id":"`+uuidG+`","type":"topical","confidence":0.9}]`))

	links, err := EvaluateRelationships(context.Background(), nil, newTestRouter(), llm.Options{},
		srcBlock(uuidA), []BlockInfo{candBlock(uuidB)})
	if err != nil {
		t.Fatalf("a fully filtered answer must stay a zero-link success: %v", err)
	}
	if len(links) != 0 {
		t.Fatalf("want 0 links, got %+v", links)
	}

	// Same parsed/valid pair the filtered path hands the counter: 1 parsed,
	// 0 survivors.
	entry := &llmlog.Entry{Pipeline: "dream-eval", Metadata: map[string]any{"links_parsed": 1}}
	if !noteDroppedInvalid(entry, 1, len(links)) {
		t.Fatal("a fully dropped parse must be counted")
	}
	if entry.Metadata["links_dropped_invalid"] != 1 {
		t.Errorf("links_dropped_invalid = %v, want 1", entry.Metadata["links_dropped_invalid"])
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
