// Wave H12 constants gate (design/04 §4.7, §7-H12 e/f): the per-pipeline
// budgets are only meaningful for as long as they still cover the item caps
// their pipelines actually apply. This test does the multiplication — item cap
// x item count + rule + question — for EVERY foreign-text pipeline and goes red
// the moment one of those constants grows without its budget growing with it.
//
// External test package on purpose: the gate imports the pipelines whose
// constants it checks (llm, rrf, dream), and every one of them imports
// promptguard. `package promptguard_test` is what makes that legal — a gate
// that had to hand-copy the numbers would assert nothing.
//
// The daily report is the one pipeline whose volume is NOT expressible in
// exported constants: three of its sections carry no SQL LIMIT at all. That
// half of the gate reads the source file, exactly like the H11 call-site test
// reads the module — and pins the number of unlimited sections, the same way
// H13d pins its two exec exceptions.
//
// No DB, no build tag: runs under `go test -short`.
package promptguard_test

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/promptguard"
	"github.com/GottZ/ctx/internal/rrf"
)

// Write-path caps that are NOT Go constants — they live in request validation
// and in the DDL, and the daily prompt inherits them by construction.
const (
	// blockTitleMaxRunes is the ingest title cap (internal/handler/ingest.go
	// maxTitleLen, and the 500 in handler/blob.go). The daily prompt prints one
	// title per new block.
	blockTitleMaxRunes = 500
	// blockCategoryMaxRunes is the category cap (context_blocks.category,
	// len <= 100 — the free-string field dream/evaluate.go's doc names).
	blockCategoryMaxRunes = 100
	// dailyItemMarkupRunes is the per-line framing of a daily-report item
	// ("- [category] title\n" plus head-room).
	dailyItemMarkupRunes = 32
)

// budgetCase is one foreign-text pipeline's worst case.
type budgetCase struct {
	pipeline string
	itemCap  int // runes of foreign text per item
	items    int // maximum number of items
	fixed    int // code-side reserve that is not per-item (e.g. the source block)
	question bool
	budget   int
}

// pipelines are the five prompt paths that carry foreign text. The two
// pipelines with no foreign text at all (query-translate, query-temporal —
// the caller's own query only) are absent by the same reasoning that keeps
// them out of the H11 call-site guard list.
func foreignTextPipelines() []budgetCase {
	return []budgetCase{
		{
			pipeline: "query-synthesize",
			itemCap:  llm.MaxBlockChars,
			items:    llm.MaxPromptSources,
			question: true,
			budget:   promptguard.BudgetSynthesis,
		},
		{
			pipeline: "query-rerank-judge",
			itemCap:  rrf.RerankContentLimit,
			items:    rrf.RerankMaxDocs,
			question: true,
			budget:   promptguard.BudgetRerankJudge,
		},
		{
			pipeline: "dream-eval",
			itemCap:  dream.MaxContentLen / 2, // candidates get half (buildEvalPrompt)
			items:    dream.MaxCandidatesPerKeyword * dream.MaxKeywords,
			fixed:    dream.MaxContentLen, // the source block, at the full cap
			budget:   promptguard.BudgetDreamEval,
		},
		{
			pipeline: "sensitivity-audit",
			itemCap:  llm.ClassifyContentLimit,
			items:    1, // exactly ONE block per audit question
			budget:   promptguard.BudgetClassifyAudit,
		},
	}
}

// TestPromptBudgetsCoverPipelineConstants is probe (e): every pipeline's worst
// case must fit its budget.
//
// Falsifying mutation, and the one the probe was measured against: raise
// dream.MaxCandidatesPerKeyword from 5 to 50. The dream-eval worst case goes
// from 10 800 to 100 800 runes against a 16 000-rune budget and this test goes
// red — WITHOUT a prompt ever being built, which is the point of a static gate:
// it fires at `go test`, not at the first oversized prompt in production.
func TestPromptBudgetsCoverPipelineConstants(t *testing.T) {
	for _, c := range foreignTextPipelines() {
		t.Run(c.pipeline, func(t *testing.T) {
			worst := c.itemCap*c.items + c.fixed + promptguard.RuleReserve
			if c.question {
				worst += promptguard.QuestionReserve
			}
			if worst > c.budget {
				t.Errorf("%s worst case = %d runes (%d x %d + %d fixed + rule%s), budget %d.\n"+
					"A cap grew without its budget: either lower the cap again, or raise the budget "+
					"AND re-check it against the smallest context window the role's chain can resolve to.",
					c.pipeline, worst, c.itemCap, c.items, c.fixed,
					map[bool]string{true: " + question", false: ""}[c.question], c.budget)
			}
		})
	}
}

// TestRuleReserveCoversRule keeps the gate's rule charge honest: RuleReserve is
// what every case above pays for the security sentence, so it must actually
// cover it. Red if Rule's wording ever grows past the reserve.
func TestRuleReserveCoversRule(t *testing.T) {
	got := utf8.RuneCountInString(promptguard.CanonicalRule())
	if got > promptguard.RuleReserve {
		t.Errorf("Rule() is %d runes, RuleReserve is %d — the gate under-charges every pipeline",
			got, promptguard.RuleReserve)
	}
}

// TestBudgetWiringCarriesItsTelemetry is the wiring half of probe (g): a
// production file that resolves a chain budget must also stamp the outcome onto
// its llmlog entry. Budget and telemetry travel together — a cap that fires
// invisibly is a cap nobody can hold to account, and the llm_log row is the only
// place a fired cap is ever observable in production.
//
// Source-level on purpose, in the H11 call-site idiom: llmlog.Record is
// fire-and-forget onto a *pgxpool.Pool, so a unit test can call the stamping
// helper directly (TestApplyBudgetTelemetry does) but cannot observe whether
// Synthesize still CALLS it. Measured: deleting that one call left every other
// probe green — this is the case that catches it.
var (
	telemetryCall = regexp.MustCompile(`\bapplyBudgetTelemetry\(`)
	telemetryDecl = regexp.MustCompile(`func applyBudgetTelemetry\(`)
)

func TestBudgetWiringCarriesItsTelemetry(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	var checked int
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			switch base := info.Name(); {
			case base == "vendor", base == ".git", base == "node_modules", strings.HasPrefix(base, ".gocache"):
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		text := string(src)
		if !strings.Contains(text, "promptguard.ChainRuneBudget(") {
			return nil
		}
		checked++
		// Declaration vs CALL, the H11 distinction: a file that only DEFINES the
		// stamping helper has not stamped anything. Measured — the first version
		// of this gate counted the `func` line and stayed green on the mutation
		// it exists to catch.
		if telemetryCall.FindAllString(text, -1) == nil ||
			len(telemetryCall.FindAllString(text, -1)) <= len(telemetryDecl.FindAllString(text, -1)) {
			rel, _ := filepath.Rel(root, path)
			t.Errorf("%s resolves a chain budget but never CALLS applyBudgetTelemetry — "+
				"metadata.promptguard_dropped stays unset and a cap that fires is invisible",
				filepath.ToSlash(rel))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if checked == 0 {
		t.Errorf("no production file resolves a chain budget — the H12 wiring is gone, not merely unstamped")
	}
}

// dailyFetchFunc matches a daily-report section fetcher that scans rows into a
// SLICE. The slice return is the discriminator that matters: an unbounded row
// count is what turns a section into an unbounded prompt. fetchDailyGuardReview
// returns *dailyGuardStat — one aggregate row, at most four printed lines — and
// is correctly NOT in scope.
var dailyFetchFunc = regexp.MustCompile(`(?m)^func (fetchDaily\w+)\([^)]*\) \(\[\]\w+, error\) \{`)

// dailySQLLimit matches an SQL LIMIT with a literal bound inside such a body.
var dailySQLLimit = regexp.MustCompile(`LIMIT (\d+)`)

// wantUnlimitedDailySections pins how many daily-report sections run WITHOUT an
// SQL LIMIT. Today: fetchDailyDecisions, fetchDailyDreamLinks and
// fetchDailyStructuralLinks — all three are GROUP BY aggregations whose row
// count is bounded by a vocabulary (decision labels, relationship classes,
// link_class x origin pairs), not by a clause. That is a real but SMALL bound,
// and it is the reason the three are tolerated rather than fixed.
//
// A FOURTH unlimited section is the drift this number exists to catch: the next
// aggregation added here may well not be vocabulary-bounded, and nothing else
// in the tree would notice.
const wantUnlimitedDailySections = 3

// TestDailyReportUnlimitedSectionCount is probe (f).
//
// Falsifying mutation, and the one the probe was measured against: add a fourth
// `func fetchDailyX(ctx …) ([]dailyXStat, error)` to synthesize_report.go whose
// query carries no LIMIT. The count moves to 4 and this test goes red.
func TestDailyReportUnlimitedSectionCount(t *testing.T) {
	src := readDailyReportSource(t)

	var unlimited, limited []string
	limits := map[string]int{}
	for _, m := range dailyFetchFunc.FindAllStringSubmatchIndex(src, -1) {
		name := src[m[2]:m[3]]
		body := funcBody(src, m[1])
		if lm := dailySQLLimit.FindStringSubmatch(body); lm != nil {
			limited = append(limited, name)
			limits[name] = atoi(lm[1])
			continue
		}
		unlimited = append(unlimited, name)
	}

	if len(unlimited) != wantUnlimitedDailySections {
		t.Errorf("daily report has %d section fetchers without an SQL LIMIT, want %d: %v\n"+
			"A NEW unlimited section needs either a LIMIT or a written reason why its row count is "+
			"bounded by a vocabulary — then raise wantUnlimitedDailySections with that reason.",
			len(unlimited), wantUnlimitedDailySections, unlimited)
	}
	if len(limited) == 0 {
		t.Fatalf("no LIMIT-bounded section found — the scan did not reach the fetchers")
	}

	// The bounded half feeds the budget check: ten new blocks, each a title +
	// a category on one line.
	worst := promptguard.RuleReserve
	for name, n := range limits {
		worst += n * (blockTitleMaxRunes + blockCategoryMaxRunes + dailyItemMarkupRunes)
		t.Logf("bounded section %s: LIMIT %d", name, n)
	}
	if worst > promptguard.BudgetDailyReport {
		t.Errorf("daily report worst case = %d runes, budget %d", worst, promptguard.BudgetDailyReport)
	}
}

// readDailyReportSource loads internal/dream/synthesize_report.go from the
// module root. Same cut as the H11 call-site test — and it verifies the root
// really is the module root, so a narrowed path fails loudly instead of
// silently scanning nothing.
func readDailyReportSource(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	if _, serr := os.Stat(filepath.Join(root, "go.mod")); serr != nil {
		t.Fatalf("scan root %s is not the module root (no go.mod): %v", root, serr)
	}
	src, err := os.ReadFile(filepath.Join(root, "internal", "dream", "synthesize_report.go"))
	if err != nil {
		t.Fatalf("read synthesize_report.go: %v", err)
	}
	return string(src)
}

// funcBody returns the source from a function's opening brace to the next
// closing brace at column 0. Sufficient here because gofmt guarantees the
// shape and every fetcher is a top-level function.
func funcBody(src string, from int) string {
	if end := strings.Index(src[from:], "\n}"); end >= 0 {
		return src[from : from+end]
	}
	return src[from:]
}

func atoi(s string) int {
	n := 0
	for _, r := range s {
		n = n*10 + int(r-'0')
	}
	return n
}
