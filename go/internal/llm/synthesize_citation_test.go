// Wave V-W1 (design/05 §2.2 seam S1, §7 row V-W1): the citation ordinal a
// source carried in the rendered synthesis prompt is resolvable from the API
// response.
//
// The prompt order is NOT the response order. Three stages sit between them —
// the low-confidence cap (Synthesize step 4), LostInMiddleReorder (step 5) and
// the H12 prompt-budget pass (step 7) — so for n >= 3 only [1] coincides.
// TestSynthesize_CitationOffsetIsReal pins that offset as a test value; it is
// green BEFORE and AFTER the wave, because the wave makes the offset
// resolvable, it does not remove it (LostInMiddleReorder stays untouched: the
// lost-in-the-middle effect is deliberate).
package llm

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// promptRecorder is a chat stub that keeps the USER message of the last
// request — the rendered prompt document, unescaped, so the assertions read
// the bytes the backend actually received rather than a re-derivation.
type promptRecorder struct {
	srv  *httptest.Server
	mu   sync.Mutex
	user string
}

func newPromptRecorder(t *testing.T) *promptRecorder {
	t.Helper()
	rec := &promptRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(b, &req)
		rec.mu.Lock()
		for _, m := range req.Messages {
			if m.Role == "user" {
				rec.user = m.Content
			}
		}
		rec.mu.Unlock()
		_, _ = io.WriteString(w, `{"message":{"role":"assistant","content":"ok"},"eval_count":1,"prompt_eval_count":1}`)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *promptRecorder) lastUser() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.user
}

// citationSources builds n sources with descending scores and a distinct title
// per source, so a prompt ordinal can be read back to the source that produced
// it. Sensitivity is explicit: the zero value counts as credentials in the
// trust gate (fail-closed) and would lock the stub backend out.
func citationSources(n int, topScore float64) []Source {
	out := make([]Source, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Source{
			ID:          "id" + itoa(i),
			Title:       "T" + itoa(i),
			Category:    "c",
			Content:     "body of source " + itoa(i),
			Score:       topScore - float64(i-1)*0.0001,
			Sensitivity: backends.SensPublic,
		})
	}
	return out
}

// citationSettings classifies CONFIDENT for a topScore >= 0.008 and LOW for a
// topScore in [0.001, 0.008) — the two branches of Synthesize step 4.
var citationSettings = SynthesisSettings{
	ScoreThreshold:     0.001,
	ConfidentThreshold: 0.008,
	PromptVersion:      PromptVersionV52,
}

// runCitationSynthesize walks the real pipeline against a local stub backend.
// numCtx is generous by default so the budget pass does not bite; the
// budget-drop probe passes a small one on purpose.
func runCitationSynthesize(t *testing.T, rec *promptRecorder, numCtx int, sources []Source) *SynthesisResult {
	t.Helper()
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "b", Name: "b", Host: rec.srv.URL, Protocol: backends.ProtocolOllama,
		Model: "m", Trust: backends.TrustFull, Enabled: true, NumCtx: numCtx,
		Roles: []string{backends.RoleSynthesis},
	}})
	res, err := Synthesize(testPrincipalCtx(), nil, bpool, nil, citationSettings, backends.SensPublic,
		"the question", sources, nil, "", "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	return res
}

var sourceElementRe = regexp.MustCompile(`<source id="(\d+)" title="([^"]*)"`)

// promptOrdinals maps the rendered id="N" to the title carried at that
// position, read off the prompt document itself.
func promptOrdinals(t *testing.T, prompt string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, m := range sourceElementRe.FindAllStringSubmatch(prompt, -1) {
		out[m[1]] = m[2]
	}
	if len(out) == 0 {
		t.Fatalf("no <source id=…> element in the prompt: %s", prompt)
	}
	return out
}

// TestSynthesize_CitationOffsetIsReal is the second red probe of V-W1: it
// fixes the offset as a test value BEFORE the wave removes the ambiguity
// around it. With five confident sources the response keeps filtered order,
// while the prompt renders id="2" over filtered[2] — so a client resolving
// "[2]" as sources[1] reads the wrong block.
//
// This case must stay green after the wave: V-W1 adds the mapping, it does not
// move LostInMiddleReorder. A variant that "fixed" the offset by dropping the
// reorder would make this test red.
func TestSynthesize_CitationOffsetIsReal(t *testing.T) {
	rec := newPromptRecorder(t)
	sources := citationSources(5, 0.9)
	res := runCitationSynthesize(t, rec, 32768, sources)

	if len(res.Confidence) == 0 || res.Confidence != ConfidenceConfident {
		t.Fatalf("confidence = %q, want %q — the low-confidence cap must not fire here", res.Confidence, ConfidenceConfident)
	}
	if len(res.Sources) != 5 {
		t.Fatalf("len(sources) = %d, want 5", len(res.Sources))
	}
	// Response order == filtered order (unchanged by the wave, additive field).
	for i, want := range []string{"id1", "id2", "id3", "id4", "id5"} {
		if res.Sources[i].ID != want {
			t.Errorf("sources[%d].ID = %q, want %q", i, res.Sources[i].ID, want)
		}
	}
	ord := promptOrdinals(t, rec.lastUser())
	// The offset itself: sources[1] is filtered[1] ("T2"), but the prompt's
	// id="2" carries filtered[2] ("T3").
	if res.Sources[1].Title != "T2" {
		t.Errorf("sources[1].Title = %q, want %q", res.Sources[1].Title, "T2")
	}
	if ord["2"] != "T3" {
		t.Errorf(`prompt id="2" carries %q, want %q — the reorder is gone`, ord["2"], "T3")
	}
	// The full permutation LostInMiddleReorder produces for n=5.
	want := map[string]string{"1": "T1", "2": "T3", "3": "T4", "4": "T5", "5": "T2"}
	for id, title := range want {
		if ord[id] != title {
			t.Errorf(`prompt id=%q carries %q, want %q`, id, ord[id], title)
		}
	}
	if len(ord) != 5 {
		t.Errorf("prompt carries %d source elements, want 5", len(ord))
	}
}

// TestBuildPrompt_OrdinalFollowsSliceOrder isolates the rendering half of the
// offset from the pipeline: BuildPrompt numbers id="N" strictly by slice
// position, so feeding it the reordered list is what produces the mismatch.
func TestBuildPrompt_OrdinalFollowsSliceOrder(t *testing.T) {
	sources := citationSources(5, 0.9)
	_, user := BuildPrompt("q", LostInMiddleReorder(sources), nil, citationSettings)
	ord := promptOrdinals(t, user)
	for id, title := range map[string]string{"1": "T1", "2": "T3", "3": "T4", "4": "T5", "5": "T2"} {
		if ord[id] != title {
			t.Errorf(`id=%q carries %q, want %q`, id, ord[id], title)
		}
	}
	if strings.Count(user, "<source id=") != 5 {
		t.Errorf("want 5 source elements, got %d", strings.Count(user, "<source id="))
	}
}

// citationBulkSources builds n sources whose content sits at the per-source
// cap, each with a distinct title — large enough that a small context window
// forces the H12 budget pass to drop from the bottom.
func citationBulkSources(n int) []Source {
	out := citationSources(n, 0.9)
	for i := range out {
		out[i].Content = "body " + itoa(i+1) + " " + strings.Repeat("z", MaxBlockChars)
	}
	return out
}

// citationMap reduces a response source set to title → citation ordinal, and
// reports how many sources carry no ordinal at all.
func citationMap(sources []Source) (map[string]int, int) {
	got := map[string]int{}
	nils := 0
	for _, s := range sources {
		if s.CitationIndex == nil {
			nils++
			continue
		}
		got[s.Title] = *s.CitationIndex
	}
	return got, nils
}

// TestSynthesize_CitationIndexResolvesPromptOrdinal is the primary V-W1 gate:
// the source carrying citation_index == 2 is the one the prompt rendered on
// id="2" — filtered[2], not sources[1].
//
// Red before the wave: the field does not exist (compile error). Falsifying
// implementations, both caught here: (1) numbering by response position — T2
// would get 2; (2) numbering by the pre-reorder prompt candidate list — same
// result.
func TestSynthesize_CitationIndexResolvesPromptOrdinal(t *testing.T) {
	rec := newPromptRecorder(t)
	res := runCitationSynthesize(t, rec, 32768, citationSources(5, 0.9))

	if len(res.Sources) != 5 {
		t.Fatalf("len(sources) = %d, want 5 — the response set must stay unchanged", len(res.Sources))
	}
	got, nils := citationMap(res.Sources)
	if nils != 0 {
		t.Errorf("%d sources without a citation index — all five entered the prompt", nils)
	}
	// Assertion A, stated as the gate does: index 2 belongs to filtered[2].
	var atTwo string
	for _, s := range res.Sources {
		if s.CitationIndex != nil && *s.CitationIndex == 2 {
			atTwo = s.ID
		}
	}
	if atTwo != "id3" {
		t.Errorf("citation_index == 2 sits on %q, want %q (= filtered[2])", atTwo, "id3")
	}
	// The full mapping for n=5, as LostInMiddleReorder produces it:
	// filtered[0]→1, filtered[2]→2, filtered[3]→3, filtered[4]→4, filtered[1]→5.
	want := map[string]int{"T1": 1, "T3": 2, "T4": 3, "T5": 4, "T2": 5}
	for title, n := range want {
		if got[title] != n {
			t.Errorf("source %q: citation_index = %d, want %d", title, got[title], n)
		}
	}
	// Bound to the prompt document itself, not to a re-derivation of the
	// permutation: whatever the builder rendered on id="N" must be the source
	// that claims N.
	ord := promptOrdinals(t, rec.lastUser())
	for title, n := range got {
		if ord[itoa(n)] != title {
			t.Errorf(`source %q claims id=%d, but the prompt rendered %q there`, title, n, ord[itoa(n)])
		}
	}
}

// TestSynthesize_CitationIndexNilForCappedSources is the low-confidence probe:
// the cap keeps three of five sources out of the prompt, and a source the
// model never saw must not carry an ordinal — otherwise a citation checker
// would resolve [3] onto a block that was never citable.
func TestSynthesize_CitationIndexNilForCappedSources(t *testing.T) {
	rec := newPromptRecorder(t)
	res := runCitationSynthesize(t, rec, 32768, citationSources(5, 0.007))

	if res.Confidence != ConfidenceLow {
		t.Fatalf("confidence = %q, want %q", res.Confidence, ConfidenceLow)
	}
	if len(res.Sources) != 5 {
		t.Fatalf("len(sources) = %d, want 5 — the cap trims the PROMPT, not the response", len(res.Sources))
	}
	if got := len(promptOrdinals(t, rec.lastUser())); got != LowConfidenceMaxSources {
		t.Fatalf("prompt carries %d source elements, want %d", got, LowConfidenceMaxSources)
	}
	got, nils := citationMap(res.Sources)
	if nils != 3 {
		t.Errorf("%d sources without a citation index, want 3", nils)
	}
	if len(got) != 2 || got["T1"] != 1 || got["T2"] != 2 {
		t.Errorf("citation map = %v, want {T1:1, T2:2}", got)
	}
}

// TestSynthesize_CitationIndexNilForBudgetDroppedSource closes the third stage
// between response order and prompt order: the H12 budget pass (Synthesize
// step 7) runs AFTER the reorder and removes sources from the bottom of the
// prompt. An implementation that derives the ordinals from
// LostInMiddleReorder(llmSources) — i.e. before the budget pass — hands out
// ordinals for sources that are not in the document and numbers the survivors
// wrong; this case is red for it.
func TestSynthesize_CitationIndexNilForBudgetDroppedSource(t *testing.T) {
	rec := newPromptRecorder(t)
	// 2048 tokens => 3686 runes, far below ten sources at MaxBlockChars.
	res := runCitationSynthesize(t, rec, 2048, citationBulkSources(10))

	if len(res.Sources) != 10 {
		t.Fatalf("len(sources) = %d, want 10", len(res.Sources))
	}
	ord := promptOrdinals(t, rec.lastUser())
	if len(ord) >= 10 {
		t.Fatalf("prompt carries %d source elements — the budget never bit", len(ord))
	}
	got, nils := citationMap(res.Sources)
	if len(got) != len(ord) {
		t.Errorf("%d sources carry an ordinal, but the prompt rendered %d elements", len(got), len(ord))
	}
	if nils != 10-len(ord) {
		t.Errorf("%d sources without an ordinal, want %d", nils, 10-len(ord))
	}
	// Every claimed ordinal resolves to the title the prompt rendered there,
	// and the ordinals are the contiguous run 1..len(ord).
	seen := map[int]bool{}
	for title, n := range got {
		if ord[itoa(n)] != title {
			t.Errorf(`source %q claims id=%d, but the prompt rendered %q there`, title, n, ord[itoa(n)])
		}
		if n < 1 || n > len(ord) {
			t.Errorf("source %q claims id=%d, outside 1..%d", title, n, len(ord))
		}
		if seen[n] {
			t.Errorf("id=%d claimed twice", n)
		}
		seen[n] = true
	}
}
