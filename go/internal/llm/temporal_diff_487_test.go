package llm

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestRules_DifferentialAgainstTheBase is the gate of issue #487's review
// round: over the question corpora of the independent review (four-digit
// numbers that are no year, years, the tense of German and English questions,
// German Perfekt questions, the edges of a window at year precision, the F
// bucket), the lead's own, the holdout of that round and the questions of
// issue #506, asked on the days the review asked them, no question whose
// answer the rules changed against 36b915f8 (v5.75.0, the "base" column of
// testdata/temporal_diff_487) may be one they now answer wrong. Every changed
// answer carries a verdict given by hand — fixed, or disputed where both
// readings stand — and the answer it was given for; an answer that changes
// again fails until it is judged again, and a verdict "broken" fails always.
// The base column is the rules' output at the base, byte for byte the
// review's own run of the base on the same corpora.
func TestRules_DifferentialAgainstTheBase(t *testing.T) {
	checkDifferential(t, "testdata/temporal_diff_487")
}

// TestRules_DifferentialAgainstTheSecondBase is the same gate against
// aa1354ea (v5.76.1, issue #506): the base column of
// testdata/temporal_diff_506 is the rules' output there, over the same
// corpora, so that no question the rules answer right since issue #487 is
// answered wrong now.
func TestRules_DifferentialAgainstTheSecondBase(t *testing.T) {
	checkDifferential(t, "testdata/temporal_diff_506")
}

// checkDifferential runs the differential gate over the corpora in dir. A
// gate that checks no question proves nothing: a directory without a corpus
// file, and a file without a question, fail it (issue #506).
func checkDifferential(t testing.TB, dir string) {
	t.Helper()
	files, err := filepath.Glob(dir + "/*.tsv")
	if err != nil || len(files) == 0 {
		t.Fatalf("no corpus in %s: %v", dir, err)
	}
	type counts struct{ checked, changed, fixed, broken, disputed int }
	var total counts
	var disputed []string
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".tsv")
		if len(name) < len("2006-01-02") {
			t.Fatalf("%s: no reference day in the name", file)
		}
		now, err := time.Parse("2006-01-02", name[len(name)-len("2006-01-02"):])
		if err != nil {
			t.Fatalf("%s: no reference day in the name: %v", file, err)
		}
		now = now.Add(12 * time.Hour)
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		var c counts
		sc := bufio.NewScanner(f)
		for line := 1; sc.Scan(); line++ {
			if strings.HasPrefix(sc.Text(), "#") {
				continue
			}
			col := strings.Split(sc.Text(), "\t")
			if len(col) != 2 && len(col) != 5 {
				t.Fatalf("%s:%d: %d columns, want 2 (unchanged) or 5 (changed)", file, line, len(col))
			}
			col = append(col, "", "", "")[:5]
			q, base, verdict, judged := col[0], col[1], col[2], col[3]
			got := rulesOutput(q, now)
			c.checked++
			if got == base {
				if verdict != "" {
					t.Errorf("%s:%d %q: the answer is the base's again, the verdict %q is stale", file, line, q, verdict)
				}
				continue
			}
			c.changed++
			switch {
			case judged != got:
				t.Errorf("%s:%d %q: changed against the base without a verdict for this answer\n  base %s\n  now  %s\n  judged %s",
					file, line, q, base, got, judged)
			case verdict == "fixed":
				c.fixed++
			case verdict == "disputed":
				c.disputed++
				disputed = append(disputed, name+": "+q)
			case verdict == "broken":
				c.broken++
				t.Errorf("%s:%d %q: answered right at the base, wrong now\n  base %s\n  now  %s", file, line, q, base, got)
			default:
				t.Errorf("%s:%d %q: verdict %q is none of fixed, disputed, broken", file, line, q, verdict)
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if c.checked == 0 {
			t.Errorf("%s: the corpus holds no question", file)
		}
		t.Logf("%s: checked %d, changed %d, fixed %d, broken %d, disputed %d", name, c.checked, c.changed, c.fixed, c.broken, c.disputed)
		total.checked += c.checked
		total.changed += c.changed
		total.fixed += c.fixed
		total.broken += c.broken
		total.disputed += c.disputed
	}
	if total.checked == 0 {
		t.Fatalf("%s: no question checked", dir)
	}
	t.Logf("total: checked %d, changed %d, fixed %d, broken %d, disputed %d", total.checked, total.changed, total.fixed, total.broken, total.disputed)
	t.Logf("disputed:\n  %s", strings.Join(disputed, "\n  "))
}

// TestCheckDifferential_AnEmptyCorpusFails: the gate fails on a directory
// without a corpus file and on a corpus file that holds only its header —
// before issue #506 both passed, checking nothing.
func TestCheckDifferential_AnEmptyCorpusFails(t *testing.T) {
	empty := t.TempDir()
	headerOnly := t.TempDir()
	if err := os.WriteFile(filepath.Join(headerOnly, "all-2026-11-22.tsv"), []byte("# header\n# columns\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{"no file": empty, "header only": headerOnly} {
		rec := &gateRecorder{TB: t}
		func() {
			defer func() {
				if r := recover(); r != nil {
					if err, ok := r.(error); !ok || !errors.Is(err, errGateStopped) {
						panic(r)
					}
				}
			}()
			checkDifferential(rec, dir)
		}()
		if len(rec.failures) == 0 {
			t.Errorf("%s: the differential gate passes without a question", name)
		}
	}
}

// gateRecorder records the failures a gate reports instead of failing the
// test that runs it; a fatal failure stops the gate (errGateStopped).
type gateRecorder struct {
	testing.TB
	failures []string
}

var errGateStopped = errors.New("gate stopped")

func (r *gateRecorder) Helper()                       {}
func (r *gateRecorder) Logf(string, ...any)           {}
func (r *gateRecorder) Errorf(format string, a ...any) { r.failures = append(r.failures, fmt.Sprintf(format, a...)) }
func (r *gateRecorder) Fatalf(format string, a ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, a...))
	panic(errGateStopped)
}
func (r *gateRecorder) Fatal(a ...any) {
	r.failures = append(r.failures, fmt.Sprint(a...))
	panic(errGateStopped)
}

// rulesOutput is the answer of the rules to q at now in the form of the
// review's probe: the dates as "ref|date..end|dir" (a JSON list) and the
// weights sorted by name, or "nil".
func rulesOutput(q string, now time.Time) string {
	r := NormalizeTemporalRules(q, now)
	if r == nil {
		return "nil"
	}
	parts := make([]string, 0, len(r.Dates))
	for _, d := range r.Dates {
		end := ""
		if d.End != nil {
			end = *d.End
		}
		parts = append(parts, fmt.Sprintf("%s|%s..%s|%s", d.Ref, d.Date, end, d.Dir))
	}
	keys := make([]string, 0, len(r.DimensionWeights))
	for k := range r.DimensionWeights {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	ws := make([]string, 0, len(keys))
	for _, k := range keys {
		ws = append(ws, fmt.Sprintf("%s=%.2f", k, r.DimensionWeights[k]))
	}
	b, _ := json.Marshal(parts)
	return string(b) + " " + strings.Join(ws, ",")
}

// BenchmarkNormalizeTemporalRules runs the rules over every question of the
// differential corpora (testdata/temporal_diff_487) at its reference day:
// the cost of the rules per question, before and after a change.
func BenchmarkNormalizeTemporalRules(b *testing.B) {
	type asked struct {
		q   string
		now time.Time
	}
	var qs []asked
	files, _ := filepath.Glob("testdata/temporal_diff_487/*.tsv")
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".tsv")
		now, err := time.Parse("2006-01-02", name[len(name)-len("2006-01-02"):])
		if err != nil {
			b.Fatal(err)
		}
		raw, err := os.ReadFile(file)
		if err != nil {
			b.Fatal(err)
		}
		for _, line := range strings.Split(string(raw), "\n") {
			if line != "" && !strings.HasPrefix(line, "#") {
				qs = append(qs, asked{strings.SplitN(line, "\t", 2)[0], now.Add(12 * time.Hour)})
			}
		}
	}
	if len(qs) == 0 {
		b.Fatal("no questions")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		for _, a := range qs {
			NormalizeTemporalRules(a.q, a.now)
		}
	}
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*len(qs)), "ns/question")
}
