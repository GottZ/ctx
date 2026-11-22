package llm

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRules_SecondHoldoutAgainstTheBase is the gate of the holdout of issue
// #506's pull request (testdata/temporal_holdout_506: 482 questions, the
// expectation of each written and hashed before the diff was read), asked on
// 2026-11-22 at noon UTC: no question the rules answered right at aa1354ea
// (v5.76.1, the base column) may be one they answer wrong now, unless the
// holdout marked its expectation as disputed. Right is the holdout's own
// criterion (holdout506Right). The counts per class are logged.
func TestRules_SecondHoldoutAgainstTheBase(t *testing.T) {
	files, err := filepath.Glob("testdata/temporal_holdout_506/*.tsv")
	if err != nil || len(files) == 0 {
		t.Fatalf("no holdout corpus: %v", err)
	}
	type tally struct{ n, base, now, worse, better int }
	byClass := map[string]*tally{}
	checked := 0
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".tsv")
		day, err := time.Parse("2006-01-02", name[len(name)-len("2006-01-02"):])
		if err != nil {
			t.Fatalf("%s: no reference day in the name: %v", file, err)
		}
		if day.Format("2006-01-02") != "2026-11-22" {
			t.Fatalf("%s: the macros of the expected dates (holdout506Macro) are those of 2026-11-22", file)
		}
		f, err := os.Open(file)
		if err != nil {
			t.Fatal(err)
		}
		sc := bufio.NewScanner(f)
		for line := 1; sc.Scan(); line++ {
			if strings.HasPrefix(sc.Text(), "#") || strings.TrimSpace(sc.Text()) == "" {
				continue
			}
			col := strings.Split(sc.Text(), "\t")
			if len(col) != 7 {
				t.Fatalf("%s:%d: %d columns, want 7", file, line, len(col))
			}
			class, rule, spec, disputed, q := col[0], col[2], col[3], col[4] == "s", col[5]
			baseFired, baseDates := col[6] != "nil", []TemporalDate(nil)
			if baseFired {
				if err := json.Unmarshal([]byte(col[6]), &baseDates); err != nil {
					t.Fatalf("%s:%d: the base column is no JSON list of dates: %v", file, line, err)
				}
			}
			baseOK, _, err := holdout506Right(rule, spec, baseFired, baseDates, day)
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			r := NormalizeTemporalRules(q, day.Add(12*time.Hour))
			var nowDates []TemporalDate
			if r != nil {
				nowDates = r.Dates
			}
			nowOK, why, err := holdout506Right(rule, spec, r != nil, nowDates, day)
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			checked++
			if disputed {
				continue
			}
			for _, k := range []string{class, "total"} {
				c := byClass[k]
				if c == nil {
					c = &tally{}
					byClass[k] = c
				}
				c.n++
				c.base += b2i(baseOK)
				c.now += b2i(nowOK)
				c.worse += b2i(baseOK && !nowOK)
				c.better += b2i(nowOK && !baseOK)
			}
			if baseOK && !nowOK {
				got, _ := json.Marshal(nowDates)
				t.Errorf("%s:%d [%s] %q: answered right at aa1354ea, wrong now (%s): base %s, now %s, expected %s",
					file, line, class, q, why, col[6], got, spec)
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
	}
	if checked == 0 {
		t.Fatal("the holdout holds no question")
	}
	keys := make([]string, 0, len(byClass))
	for k := range byClass {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[j] == "total" || keys[i] != "total" && keys[i] < keys[j] })
	var report strings.Builder
	report.WriteString("class: questions / right at aa1354ea / right now / wrong now, right at the base / right now, wrong at the base\n")
	for _, k := range keys {
		c := byClass[k]
		fmt.Fprintf(&report, "%s: %d / %d / %d / %d / %d\n", k, c.n, c.base, c.now, c.worse, c.better)
	}
	t.Logf("holdout of #506 by its own criterion, disputed questions left out:\n%s", report.String())
	// The base column judged by the port is what evaluate.py counted for it:
	// a port that judges more leniently fails here, not silently.
	if got := byClass["total"].base; got != holdout506RightAtTheBase {
		t.Errorf("right at aa1354ea by the port: %d, evaluate.py counted %d — the criterion drifted", got, holdout506RightAtTheBase)
	}
	if got := byClass["total"].now; got < holdout506RightAtThePR {
		t.Errorf("right now: %d, fewer than the %d of issue #506's pull request", got, holdout506RightAtThePR)
	}
}

// holdout506RightAtTheBase is the number of undisputed questions of the
// holdout that evaluate.py counts right at aa1354ea without its tense part
// (275 of 481); holdout506RightAtThePR the number right when issue #506's
// pull request landed (303), a floor that keeps its improvements counted.
const (
	holdout506RightAtTheBase = 275
	holdout506RightAtThePR   = 303
)

// holdout506Right is the criterion of the holdout's evaluation (evaluate.py
// of issue #506's holdout), ported without its tense part — the tense of a
// whole question (DetectVerbTense) has no production caller, the tense a date
// is read by shows in its date: a rule answers exactly when one should
// (rule "ja": it answers and its dates fit; "nein": none answers; "egal":
// none answers, or its dates fit). The dates fit the expectation spec when
// some alternative of it covers the same days as the dates of the answer
// (holdout506Fits).
func holdout506Right(rule, spec string, fired bool, dates []TemporalDate, day time.Time) (bool, string, error) {
	switch rule {
	case "ja":
		if !fired {
			return false, "no rule answers", nil
		}
	case "nein":
		if fired {
			return false, "a rule answers", nil
		}
		return true, "", nil
	case "egal":
		if !fired {
			return true, "", nil
		}
	default:
		return false, "", fmt.Errorf("expected rule %q is none of ja, nein, egal", rule)
	}
	ok, err := holdout506Fits(spec, dates, day)
	if err != nil {
		return false, "", err
	}
	if !ok {
		return false, "the dates do not fit", nil
	}
	return true, "", nil
}

// holdout506Inf is an open edge of a span of days.
const holdout506Inf = int64(1) << 40

// holdout506Span is a span of days [s, e] as day numbers; within: every
// date of the answer has to lie inside it ("~A..B").
type holdout506Span struct {
	s, e   int64
	within bool
}

func holdout506DayNumber(s string) (int64, error) {
	t, err := time.Parse("2006-01-02", s)
	if err != nil {
		return 0, err
	}
	return t.Unix() / 86400, nil
}

// holdout506Macro expands a macro of the spec into its readings at the
// reference day day (expand_macro of evaluate.py).
func holdout506Macro(tok string, day time.Time) []string {
	today, yesterday := day.Format("2006-01-02"), day.AddDate(0, 0, -1).Format("2006-01-02")
	digits := func(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }
	switch {
	case len(tok) == 8 && tok[0] == 'M' && digits(tok[1:5]):
		y, _ := strconv.Atoi(tok[1:5])
		m, _ := strconv.Atoi(tok[6:8])
		last := time.Date(y, time.Month(m)+1, 0, 0, 0, 0, 0, time.UTC).Day()
		return []string{fmt.Sprintf("%04d-%02d-01..%04d-%02d-%02d", y, m, y, m, last)}
	case len(tok) == 5 && tok[0] == 'Y' && digits(tok[1:]):
		return []string{tok[1:] + "-01-01.." + tok[1:] + "-12-31"}
	case tok == "WEP":
		return []string{"2026-11-21..2026-11-22", "2026-11-14..2026-11-15"}
	case tok == "WEF":
		return []string{"2026-11-28..2026-11-29"}
	case tok == "WEA":
		return []string{"2026-11-21..2026-11-22", "2026-11-28..2026-11-29"}
	case tok == "LW":
		return []string{"2026-11-09..2026-11-15", "2026-11-15..2026-11-21", "2026-11-15..2026-11-22", "2026-11-16..2026-11-22"}
	case tok == "NW":
		return []string{"2026-11-23..2026-11-29"}
	case tok == "NOV":
		return []string{"2026-11-01..2026-11-30", "2026-11-01..2026-11-22", "2026-11-01..2026-11-21"}
	case strings.HasPrefix(tok, "U+"):
		x := tok[2:]
		return []string{"<=" + x, x, today + ".." + x, "2026-11-23.." + x}
	case strings.HasPrefix(tok, "U-"):
		x := tok[2:]
		return []string{"<=" + x, x}
	case strings.HasPrefix(tok, "S-"):
		x := tok[2:]
		return []string{">=" + x, x + ".." + today, x + ".." + yesterday}
	}
	return []string{tok}
}

// holdout506Atom parses one date of the spec (atom of evaluate.py).
func holdout506Atom(a string) (holdout506Span, error) {
	a = strings.TrimSpace(a)
	var sp holdout506Span
	var err error
	edge := func(s string) int64 {
		n, e := holdout506DayNumber(s)
		if e != nil && err == nil {
			err = e
		}
		return n
	}
	switch {
	case strings.HasPrefix(a, "~"):
		s, e, _ := strings.Cut(a[1:], "..")
		sp = holdout506Span{edge(s), edge(e), true}
	case strings.HasPrefix(a, ">="):
		sp = holdout506Span{edge(a[2:]), holdout506Inf, false}
	case strings.HasPrefix(a, "<="):
		sp = holdout506Span{-holdout506Inf, edge(a[2:]), false}
	case strings.Contains(a, ".."):
		s, e, _ := strings.Cut(a, "..")
		sp = holdout506Span{edge(s), edge(e), false}
	default:
		n := edge(a)
		sp = holdout506Span{n, n, false}
	}
	return sp, err
}

// holdout506Fits reports whether the dates of an answer fit the expectation
// spec (dates_match of evaluate.py): "*" fits any, "-" only no date; else
// some alternative fits — a single "~A..B" when every date lies inside it,
// any other when its days, merged where they touch, are the days of the
// answer merged the same way (which a set of the same spans also is).
func holdout506Fits(spec string, dates []TemporalDate, day time.Time) (bool, error) {
	switch spec = strings.TrimSpace(spec); spec {
	case "*":
		return true, nil
	case "-":
		return len(dates) == 0, nil
	}
	var answer []holdout506Span
	for _, d := range dates {
		s, err := holdout506DayNumber(d.Date)
		if err != nil {
			return false, fmt.Errorf("a date of the answer: %w", err)
		}
		sp := holdout506Span{s: s, e: s}
		switch {
		case d.End != nil && *d.End != "":
			if sp.e, err = holdout506DayNumber(*d.End); err != nil {
				return false, fmt.Errorf("an end of the answer: %w", err)
			}
		case d.Dir == DirFrom:
			sp.e = holdout506Inf
		case d.Dir == DirUntil:
			sp.s = -holdout506Inf
		}
		answer = append(answer, sp)
	}
	if len(answer) == 0 {
		return false, nil
	}
	for _, alt := range strings.Split(spec, " OR ") {
		// Each date of the alternative has its readings; every combination
		// of one reading per date is a candidate.
		cands := [][]holdout506Span{nil}
		for _, item := range strings.Split(alt, ";") {
			var readings []holdout506Span
			for _, a := range strings.Split(item, "/") {
				for _, x := range holdout506Macro(strings.TrimSpace(a), day) {
					sp, err := holdout506Atom(x)
					if err != nil {
						return false, fmt.Errorf("expected dates %q: %w", spec, err)
					}
					readings = append(readings, sp)
				}
			}
			var next [][]holdout506Span
			for _, c := range cands {
				for _, r := range readings {
					next = append(next, append(append([]holdout506Span(nil), c...), r))
				}
			}
			cands = next
		}
		for _, c := range cands {
			if len(c) == 1 && c[0].within {
				inside := true
				for _, a := range answer {
					inside = inside && c[0].s <= a.s && a.e <= c[0].e
				}
				if inside {
					return true, nil
				}
				continue
			}
			if holdout506Merged(answer) == holdout506Merged(c) {
				return true, nil
			}
		}
	}
	return false, nil
}

// holdout506Merged is the days of the spans, sorted and merged where they
// overlap or touch, written out for comparison (union of evaluate.py).
func holdout506Merged(spans []holdout506Span) string {
	s := append([]holdout506Span(nil), spans...)
	sort.Slice(s, func(i, j int) bool { return s[i].s < s[j].s || s[i].s == s[j].s && s[i].e < s[j].e })
	var out []holdout506Span
	for _, sp := range s {
		if n := len(out); n > 0 && sp.s <= out[n-1].e+1 {
			out[n-1].e = max(out[n-1].e, sp.e)
			continue
		}
		out = append(out, holdout506Span{s: sp.s, e: sp.e})
	}
	return fmt.Sprint(out)
}
