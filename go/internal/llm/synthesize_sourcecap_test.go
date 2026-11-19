// Issue #208: the per-source cut of a synthesis prompt is the settings key
// query.max_source_runes, bounded so that no prompt carries more source text
// than today's worst case (MaxPromptSources sources at MaxBlockChars each) —
// the volume the H12 static gate (promptguard.TestPromptBudgetsCoverPipeline-
// Constants) already proves against BudgetSynthesis.
package llm

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/promptguard"
)

// TestSourceRuneCapDefaultIsTodaysCut pins byte-equality: without the key (0)
// and at its default (MaxBlockChars) every source is cut at MaxBlockChars,
// whatever the source count — including counts above the handler's clamp,
// which direct callers of BuildPrompt can pass.
func TestSourceRuneCapDefaultIsTodaysCut(t *testing.T) {
	for _, configured := range []int{-5, 0, 1, MaxBlockChars - 1, MaxBlockChars} {
		for n := 0; n <= 3*MaxPromptSources; n++ {
			if got := SourceRuneCap(configured, n); got != MaxBlockChars {
				t.Fatalf("SourceRuneCap(%d, %d) = %d, want MaxBlockChars %d", configured, n, got, MaxBlockChars)
			}
		}
	}
}

// TestSourceRuneCapNeverGrowsTheWorstCase: for every configured value and
// every source count the handler can send, the source text of one prompt stays
// at or under today's worst case, and no source is cut shorter than today.
//
// Falsifying implementation: the configured value used as the cut without the
// division by n (option A of the issue without its validator) — 20 sources at
// 6000 runes is 120 000 runes against a 30 000-rune worst case.
func TestSourceRuneCapNeverGrowsTheWorstCase(t *testing.T) {
	if SourceRunesShare != MaxPromptSources*MaxBlockChars {
		t.Fatalf("SourceRunesShare = %d, want MaxPromptSources x MaxBlockChars = %d — the share is today's worst case, the volume the H12 gate proves",
			SourceRunesShare, MaxPromptSources*MaxBlockChars)
	}
	for _, configured := range []int{MaxBlockChars + 1, 3000, 6000, 10000, SourceRunesShare / 2, SourceRunesShare, 10 * SourceRunesShare} {
		for n := 1; n <= MaxPromptSources; n++ {
			got := SourceRuneCap(configured, n)
			if got < MaxBlockChars {
				t.Errorf("SourceRuneCap(%d, %d) = %d, below today's cut %d", configured, n, got, MaxBlockChars)
			}
			if got > configured {
				t.Errorf("SourceRuneCap(%d, %d) = %d, above the configured cap", configured, n, got)
			}
			if n*got > SourceRunesShare {
				t.Errorf("SourceRuneCap(%d, %d) = %d: %d sources carry %d runes, above the %d-rune worst case",
					configured, n, got, n, n*got, SourceRunesShare)
			}
		}
	}
	// The room a query with fewer sources uses: the default limit of 5.
	if got := SourceRuneCap(10000, 5); got != SourceRunesShare/5 {
		t.Errorf("SourceRuneCap(10000, 5) = %d, want %d (the share of five sources)", got, SourceRunesShare/5)
	}
	if got := SourceRuneCap(4000, 5); got != 4000 {
		t.Errorf("SourceRuneCap(4000, 5) = %d, want the configured 4000", got)
	}
}

// sourceCapSources builds n confident sources with a long body made of one
// letter each, so the rendered length of every body is countable in the wire
// body.
func sourceCapSources(n, runes int) []Source {
	out := make([]Source, 0, n)
	for i := 1; i <= n; i++ {
		out = append(out, Source{
			ID: "id" + itoa(i), Title: "t", Category: "c", Score: 1.0 / float64(i),
			Content:     "MARKER" + itoa(i) + " " + strings.Repeat("q", runes),
			Sensitivity: backends.SensPublic,
		})
	}
	return out
}

// bodyRun counts the run of 'q' that follows marker in the wire body.
func bodyRun(body, marker string) int {
	i := strings.Index(body, marker)
	if i < 0 {
		return -1
	}
	rest := body[i+len(marker):]
	n := 0
	for n < len(rest) && rest[n] == 'q' {
		n++
	}
	return n
}

// TestSynthesizeSourceCutFollowsSetting reads the cut on the wire: three
// sources of 9000 runes each reach the backend at MaxBlockChars without the
// key, at the configured value with it, and whole once the share of three
// sources holds them.
func TestSynthesizeSourceCutFollowsSetting(t *testing.T) {
	const body = 9000
	prefix := len("MARKER1 ")
	cases := []struct {
		name       string
		configured int
		wantQ      int // run of 'q' per source on the wire
	}{
		{"no key keeps today's cut", 0, MaxBlockChars - prefix},
		{"default keeps today's cut", MaxBlockChars, MaxBlockChars - prefix},
		{"raised cap", 6000, 6000 - prefix},
		{"share of three holds the whole body", SourceRunesShare, body},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := newBudgetRecorder(t)
			s := budgetSettings(0)
			s.MaxSourceRunes = tc.configured
			if _, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, 262144), s, sourceCapSources(3, body)); err != nil {
				t.Fatalf("Synthesize: %v", err)
			}
			wire := rec.lastBody()
			for i := 1; i <= 3; i++ {
				if got := bodyRun(wire, "MARKER"+itoa(i)+" "); got != tc.wantQ {
					t.Errorf("source %d: %d runes of body on the wire, want %d", i, got, tc.wantQ)
				}
			}
		})
	}
}

// TestBuildPromptSourceCutFollowsSetting: the exported builder (bench
// harnesses, goldbench) applies the same cut as Synthesize, from the same
// settings field, and a literal SynthesisSettings keeps today's cut.
func TestBuildPromptSourceCutFollowsSetting(t *testing.T) {
	sources := sourceCapSources(2, 9000)
	_, user := BuildPrompt("q", sources, nil, SynthesisSettings{})
	if got := bodyRun(user, "MARKER1 "); got != MaxBlockChars-len("MARKER1 ") {
		t.Errorf("literal settings: %d runes of body, want today's %d", got, MaxBlockChars-len("MARKER1 "))
	}
	_, user = BuildPrompt("q", sources, nil, SynthesisSettings{MaxSourceRunes: 5000})
	if got := bodyRun(user, "MARKER1 "); got != 5000-len("MARKER1 ") {
		t.Errorf("max_source_runes 5000: %d runes of body, want %d", got, 5000-len("MARKER1 "))
	}
}

// TestFitSourcesToBudgetChargesTheGivenCut: the budget pass charges each
// source at the cut the builder will render it at. Three 9000-rune sources at a
// 6000-rune cut do not fit a 10 080-rune budget, so the pass must cut — read off
// the surviving content, which is what the builder renders.
//
// Falsifying implementation: fitSourcesToBudget charging MaxBlockChars while
// the builder renders the raised cut — the pass sees three 1500-rune sources,
// finds them fitting, and hands back 3 x 9000 runes for a 6000-rune render.
func TestFitSourcesToBudgetChargesTheGivenCut(t *testing.T) {
	budget := promptguard.RuneBudget(5600)
	sys, _ := selectSystemPrompt(budgetSettings(0))
	kept, rep := fitSourcesToBudget(sys, "q", sourceCapSources(3, 9000), budget, 6000)
	if rep.Err != nil {
		t.Fatalf("budget pass: %v", rep.Err)
	}
	total := 0
	for _, s := range kept {
		total += min(len([]rune(s.Content)), 6000)
	}
	if total > budget {
		t.Errorf("sources the builder would render carry %d runes against a %d-rune budget — the pass measured a shorter cut than the builder renders", total, budget)
	}
}

// TestSynthesizeRaisedCutNeverCostsASource: on a chain whose window holds the
// default cut for every source but not the raised one, the raised cut shrinks
// to the window's share instead of letting the budget pass drop sources — the
// second-best one first, since the lost-in-the-middle order puts it last.
// Every source reaches the wire, at one common length no shorter than the
// default's. On a window that holds the raised cut it applies in full.
//
// Falsifying implementation: no fitSourceCutToBudget — at 8192 tokens sources
// 2 and 5 go missing, at 16384 source 2 is shortened below the others.
func TestSynthesizeRaisedCutNeverCostsASource(t *testing.T) {
	for _, tc := range []struct {
		window int
		full   bool // the raised cut fits whole
	}{{8192, false}, {16384, false}, {65536, true}} {
		rec := newBudgetRecorder(t)
		s := budgetSettings(0)
		s.MaxSourceRunes = 6000
		if _, err := runBudgetSynthesize(t, budgetBackend(rec.srv.URL, tc.window), s, sourceCapSources(5, 9000)); err != nil {
			t.Fatalf("window %d: Synthesize: %v", tc.window, err)
		}
		wire := rec.lastBody()
		first := bodyRun(wire, "MARKER1 ")
		for i := 1; i <= 5; i++ {
			got := bodyRun(wire, "MARKER"+itoa(i)+" ")
			if got < 0 {
				t.Errorf("window %d: source %d missing from the wire — the raised cut cost a source the default keeps", tc.window, i)
				continue
			}
			if got != first {
				t.Errorf("window %d: source %d carries %d runes of body, source 1 carries %d — the budget pass shortened one", tc.window, i, got, first)
			}
		}
		if first < MaxBlockChars-len("MARKER1 ") {
			t.Errorf("window %d: %d runes of body, below the default's %d", tc.window, first, MaxBlockChars-len("MARKER1 "))
		}
		if tc.full && first != 6000-len("MARKER1 ") {
			t.Errorf("window %d: %d runes of body, want the raised cut whole (%d)", tc.window, first, 6000-len("MARKER1 "))
		}
	}
}

// TestFitSourceCutToBudgetLeavesTheDefaultAlone: a cut at or below
// MaxBlockChars passes unchanged whatever the budget (the default path is the
// budget pass's own business, as before the key), and a raised cut never
// drops below MaxBlockChars.
func TestFitSourceCutToBudgetLeavesTheDefaultAlone(t *testing.T) {
	src := sourceCapSources(5, 9000)
	for _, budget := range []int{100, 5000, 40000} {
		if got := fitSourceCutToBudget(MaxBlockChars, budget, "sys", "q", src); got != MaxBlockChars {
			t.Errorf("budget %d: default cut became %d", budget, got)
		}
		if got := fitSourceCutToBudget(6000, budget, "sys", "q", src); got < MaxBlockChars || got > 6000 {
			t.Errorf("budget %d: raised cut became %d, want within [%d, 6000]", budget, got, MaxBlockChars)
		}
	}
	if got := fitSourceCutToBudget(6000, 40000, "sys", "q", nil); got != 6000 {
		t.Errorf("no sources: cut %d, want 6000 unchanged", got)
	}
}
