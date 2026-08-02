// Wave H12 call-site probes (design/04 §7-H12 c/d/g): the budget is resolved
// over the RESOLVED chain, an undeclared context window refuses the prompt, and
// a cap that bit is visible in llmlog telemetry. Measured on the wire — the
// assertions read the body the backend actually received, not the intermediate
// values the code computed.
package llm

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/promptguard"
)

// budgetRecorder is a chat stub that keeps the last request body.
type budgetRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	body string
}

func newBudgetRecorder(t *testing.T) *budgetRecorder {
	t.Helper()
	rec := &budgetRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rec.mu.Lock()
		rec.body = string(b)
		rec.mu.Unlock()
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *budgetRecorder) lastBody() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.body
}

func budgetBackend(host string, numCtx int) backends.Backend {
	return backends.Backend{
		ID: "b", Name: "b", Host: host, Protocol: backends.ProtocolOllama,
		Model: "m", Trust: backends.TrustFull, Enabled: true, NumCtx: numCtx,
		Roles: []string{backends.RoleSynthesis},
	}
}

// budgetSources builds n sources whose content sits at the per-source cap, each
// carrying its own ordinal so survival is identifiable in the wire body.
func budgetSources(n int) []Source {
	out := make([]Source, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Source{
			ID: "id" + itoa(i), Title: "t", Category: "c", Score: 1.0 / float64(i),
			Content:     "MARKER" + itoa(i) + " " + strings.Repeat("z", MaxBlockChars),
			Sensitivity: backends.SensPublic,
		})
	}
	return out
}

func budgetSettings(fallback int) SynthesisSettings {
	// ConfidentThreshold LOW on purpose: budgetSources scores its top source at
	// 1.0, so the set classifies CONFIDENT and escapes the low-confidence
	// 2-source clamp (Synthesize step 4). With a high threshold the clamp fires
	// first and only two sources ever reach the prompt — the budget would then
	// never be the reason anything was dropped, and the case would pass for the
	// wrong reason. Found exactly that way while red-probing eviction order.
	return SynthesisSettings{
		ScoreThreshold: 0.001, ConfidentThreshold: 0.008,
		PromptVersion:          PromptVersionV52,
		ExternalNumCtxFallback: fallback,
	}
}

func runBudgetSynthesize(t *testing.T, b backends.Backend, s SynthesisSettings, sources []Source) (*SynthesisResult, error) {
	t.Helper()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{b})
	return Synthesize(testPrincipalCtx(), nil, bpool, nil, s, backends.SensPublic,
		"the question", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
}

// TestSynthesizeRefusesUndeclaredWindow is probe (d) at the call site: a
// synthesis chain whose member declares no context window and has no configured
// fallback must NOT build a prompt.
//
// Falsifying implementation: a call site that substitutes a rate value for the
// missing window (the model-level context_length, or a compiled-in 32768). That
// variant reaches the wire — this case requires zero wire contact.
func TestSynthesizeRefusesUndeclaredWindow(t *testing.T) {
	rec := newBudgetRecorder(t)
	_, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, 0), budgetSettings(0), budgetSources(3))
	if !errors.Is(err, promptguard.ErrUndeclaredWindow) {
		t.Errorf("err = %v, want promptguard.ErrUndeclaredWindow", err)
	}
	if body := rec.lastBody(); body != "" {
		t.Errorf("backend was contacted with %d bytes — the prompt must not be built at all", len(body))
	}
}

// TestSynthesizeFallbackUnlocksUndeclaredWindow is probe (c) at the call site:
// the SAME chain with pool.external_num_ctx_fallback configured resolves, and
// the budget follows the fallback rather than an invented window.
func TestSynthesizeFallbackUnlocksUndeclaredWindow(t *testing.T) {
	rec := newBudgetRecorder(t)
	res, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, 0), budgetSettings(32768), budgetSources(3))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want the stub answer", res.Answer)
	}
	if !strings.Contains(rec.lastBody(), "MARKER1 ") {
		t.Errorf("wire body lost source 1 — a 32768-token fallback holds three capped sources")
	}
}

// TestSynthesizeBudgetDropsFromBelowOnTheWire is probe (b) at the call site: a
// chain whose window cannot hold every source must reach the wire with FEWER
// sources, the first one surviving — not with a prompt the backend silently
// truncates from the front, where the security rule sits.
//
// Falsifying implementations: (1) no budget wiring at all — all ten markers
// reach the wire; (2) dropping from the top — MARKER1 is the one missing.
func TestSynthesizeBudgetDropsFromBelowOnTheWire(t *testing.T) {
	rec := newBudgetRecorder(t)
	// 2048 tokens => 3686 runes, well under ten sources at MaxBlockChars.
	res, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, 2048), budgetSettings(0), budgetSources(10))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if res.Answer != "ok" {
		t.Errorf("answer = %q, want the stub answer", res.Answer)
	}
	body := rec.lastBody()
	if !strings.Contains(body, "MARKER1 ") {
		t.Errorf("wire body lost source 1 — eviction ran from the top of the class, not the bottom")
	}
	if strings.Contains(body, "MARKER10 ") {
		t.Errorf("wire body kept source 10 while the budget was exceeded")
	}
	if got := strings.Count(body, "<source id="); got >= 10 {
		t.Errorf("wire body carries %d source elements — the budget never bit", got)
	}
	if !strings.Contains(body, promptguard.GuardTag) {
		t.Errorf("wire body lost its guard markers")
	}
}

// TestApplyBudgetTelemetry is probe (g): metadata.promptguard_dropped is
// present exactly when the budget cut something, and carries the count.
//
// Falsifying implementations, both caught: a stamper that never sets the key
// (case "cut"), and one that always sets it — a constant 0 on every row would
// make the interesting case unqueryable, so case "untouched" requires absence.
func TestApplyBudgetTelemetry(t *testing.T) {
	t.Run("cut", func(t *testing.T) {
		entry := llmlog.Entry{Pipeline: "query-synthesize"}
		applyBudgetTelemetry(&entry, promptguard.Report{Dropped: 2, Truncated: 1})
		if got := entry.Metadata["promptguard_dropped"]; got != 3 {
			t.Errorf("metadata.promptguard_dropped = %v, want 3", got)
		}
	})
	t.Run("untouched", func(t *testing.T) {
		entry := llmlog.Entry{Pipeline: "query-synthesize", Metadata: map[string]any{"chain": "x"}}
		applyBudgetTelemetry(&entry, promptguard.Report{})
		if _, ok := entry.Metadata["promptguard_dropped"]; ok {
			t.Errorf("metadata.promptguard_dropped set on an untouched prompt — an always-present key hides the real cases")
		}
	})
}

// itoa keeps the fixtures free of a strconv import in this file.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}
