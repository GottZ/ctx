package llm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRules_HoldoutExpectations runs the holdout of issue #487's review
// round (testdata/temporal_holdout_487: 488 questions written before that
// round's diff was read, and the 20 of its second probe) against the
// expectation the review wrote for each question, by the review's own
// criterion (holdoutJudge), asked on 2026-11-22. Each row carries whether
// the rules answered it right at 36b915f8 (v5.75.0) and at aa1354ea
// (v5.76.1); a question answered right at either base fails the test when it
// is answered wrong now, unless the review marked its expectation as
// disputed. The counts per class are logged (issue #506).
func TestRules_HoldoutExpectations(t *testing.T) {
	files, err := filepath.Glob("testdata/temporal_holdout_487/*.tsv")
	if err != nil || len(files) == 0 {
		t.Fatalf("no holdout corpus: %v", err)
	}
	type tally struct{ n, ok36, okAA, ok, right36, wrong36, rightAA, wrongAA int }
	byClass := map[string]*tally{}
	var classes []string
	checked := 0
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".tsv")
		day, err := time.Parse("2006-01-02", name[len(name)-len("2006-01-02"):])
		if err != nil {
			t.Fatalf("%s: no reference day in the name: %v", file, err)
		}
		now := day.Add(12 * time.Hour)
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
			e := holdoutCase{class: col[0], tense: col[1], rule: col[2], disputed: col[3] == "s", q: col[4]}
			ok36, okAA := col[5] == "1", col[6] == "1"
			ok, why := holdoutJudge(e, NormalizeTemporalRules(e.q, now), DetectVerbTense(e.q), day)
			checked++
			key := name + "/" + e.class
			c := byClass[key]
			if c == nil {
				c = &tally{}
				byClass[key] = c
				classes = append(classes, key)
			}
			c.n++
			c.ok36 += b2i(ok36)
			c.okAA += b2i(okAA)
			c.ok += b2i(ok)
			c.right36 += b2i(ok && !ok36)
			c.wrong36 += b2i(!ok && ok36)
			c.rightAA += b2i(ok && !okAA)
			c.wrongAA += b2i(!ok && okAA)
			if !ok && (ok36 || okAA) && !e.disputed {
				t.Errorf("%s:%d [%s] %q: answered right at a base (36b915f8 %v, aa1354ea %v), wrong now: %s",
					file, line, e.class, e.q, ok36, okAA, why)
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
	var sum tally
	var report strings.Builder
	report.WriteString("class: n, right at 36b915f8 / aa1354ea / now; became right/wrong against 36b915f8, against aa1354ea\n")
	for _, k := range classes {
		c := byClass[k]
		fmt.Fprintf(&report, "%s: %d, %d / %d / %d; +%d/-%d, +%d/-%d\n", k, c.n, c.ok36, c.okAA, c.ok, c.right36, c.wrong36, c.rightAA, c.wrongAA)
		sum.n, sum.ok36, sum.okAA, sum.ok = sum.n+c.n, sum.ok36+c.ok36, sum.okAA+c.okAA, sum.ok+c.ok
		sum.right36, sum.wrong36, sum.rightAA, sum.wrongAA = sum.right36+c.right36, sum.wrong36+c.wrong36, sum.rightAA+c.rightAA, sum.wrongAA+c.wrongAA
	}
	fmt.Fprintf(&report, "total: %d, %d / %d / %d; +%d/-%d, +%d/-%d", sum.n, sum.ok36, sum.okAA, sum.ok, sum.right36, sum.wrong36, sum.rightAA, sum.wrongAA)
	t.Logf("holdout of #487 by the review's criterion:\n%s", report.String())
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// holdoutCase is a question of the holdout with the review's expectation:
// its class (A–H, N for the second probe), the tense (past, future, neutral,
// egal), whether a rule should answer it (ja, nein, egal) and whether the
// review marked the expectation as disputed.
type holdoutCase struct {
	class, tense, rule string
	disputed           bool
	q                  string
}

// holdoutJudge is the criterion of the review's evaluation (auswertung.py of
// issue #487's review round), ported: the tense counts as past against not
// past (egal: any); a rule answers exactly when one should (egal: either);
// and when a rule should answer and does, every date points the expected way
// and one fits the day words of the question — a weekday named within 8 days
// of the reference day, a weekend (Friday to Sunday) within 8 days, a month
// named — or, for class G, the year named.
func holdoutJudge(e holdoutCase, r *TemporalResult, tense string, ref time.Time) (bool, string) {
	var why []string
	if e.tense == "past" && tense != "past" {
		why = append(why, "tense "+tense+" instead of past")
	}
	if (e.tense == "future" || e.tense == "neutral") && tense == "past" {
		why = append(why, "tense past instead of "+e.tense)
	}
	// A result without a date answers nothing (issue #517): the query path
	// would skip the LLM fallback for it and find no date to rank by.
	fired := r != nil && len(r.Dates) > 0
	if e.rule == "ja" && !fired {
		why = append(why, "no rule answers")
	}
	if e.rule == "nein" && fired {
		why = append(why, "a rule answers")
	}
	if e.rule == "ja" && fired {
		if ok, w := holdoutContent(e, r.Dates, ref); !ok {
			why = append(why, w)
		}
	}
	return len(why) == 0, strings.Join(why, ", ")
}

var (
	holdoutYearRe  = regexp.MustCompile(`\b(1[89]\d\d|20\d\d|2100)\b`)
	holdoutWordRe  = regexp.MustCompile(`[a-zäöüß]+`)
	holdoutDateRe  = regexp.MustCompile(`^(\d{4})-(\d{2})(?:-(\d{2}))?`)
	holdoutWeekday = map[string]int{"montag": 0, "monday": 0, "dienstag": 1, "tuesday": 1, "mittwoch": 2, "wednesday": 2,
		"donnerstag": 3, "thursday": 3, "freitag": 4, "friday": 4, "samstag": 5, "saturday": 5, "sonntag": 6, "sunday": 6}
	holdoutMonth = map[string]time.Month{"januar": 1, "january": 1, "februar": 2, "february": 2, "märz": 3, "march": 3,
		"april": 4, "mai": 5, "may": 5, "juni": 6, "june": 6, "juli": 7, "july": 7, "august": 8, "september": 9,
		"oktober": 10, "october": 10, "november": 11, "dezember": 12, "december": 12}
)

// holdoutDay parses the date form of auswertung.py's pdate (a month alone
// is its 1st).
func holdoutDay(s string) (time.Time, bool) {
	m := holdoutDateRe.FindStringSubmatch(s)
	if m == nil {
		return time.Time{}, false
	}
	y, _ := strconv.Atoi(m[1])
	mo, _ := strconv.Atoi(m[2])
	d := 1
	if m[3] != "" {
		d, _ = strconv.Atoi(m[3])
	}
	t := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, time.UTC)
	if t.Month() != time.Month(mo) {
		return time.Time{}, false
	}
	return t, true
}

func holdoutContent(e holdoutCase, dates []TemporalDate, ref time.Time) (bool, string) {
	edges := func(d TemporalDate) []string {
		out := []string{d.Date}
		if d.End != nil {
			out = append(out, *d.End)
		}
		return out
	}
	if e.class == "G" {
		var years []int
		for _, y := range holdoutYearRe.FindAllString(e.q, -1) {
			n, _ := strconv.Atoi(y)
			years = append(years, n)
		}
		for _, d := range dates {
			for _, s := range edges(d) {
				p, ok := holdoutDay(s)
				if !ok {
					continue
				}
				for _, y := range years {
					if p.Year() == y || p.Year() == y-1 && p.Month() == 12 && p.Day() == 31 || p.Year() == y+1 && p.Month() == 1 && p.Day() == 1 {
						return true, ""
					}
				}
			}
		}
		return false, fmt.Sprintf("year %v not in the dates", years)
	}
	ref = time.Date(ref.Year(), ref.Month(), ref.Day(), 0, 0, 0, 0, time.UTC)
	var wds []int
	var mos []time.Month
	weekend := false
	for _, w := range holdoutWordRe.FindAllString(strings.ToLower(e.q), -1) {
		if wd, ok := holdoutWeekday[w]; ok {
			wds = append(wds, wd)
		}
		if mo, ok := holdoutMonth[w]; ok {
			mos = append(mos, mo)
		}
		weekend = weekend || w == "wochenende" || w == "weekend"
	}
	for _, d := range dates {
		a, ok := holdoutDay(d.Date)
		if !ok {
			return false, "date unreadable"
		}
		end := a
		if d.End != nil {
			if p, ok := holdoutDay(*d.End); ok {
				end = p
			}
		}
		switch e.tense {
		case "past":
			if !a.Before(ref) || end.After(ref) {
				return false, fmt.Sprintf("direction: %s..%s is not past", fmtDate(a), fmtDate(end))
			}
		case "future":
			last := a
			if end.After(a) {
				last = end
			}
			if a.Before(ref) || last.Before(ref.AddDate(0, 0, 1)) {
				return false, fmt.Sprintf("direction: %s..%s is not to come", fmtDate(a), fmtDate(end))
			}
		}
	}
	if len(wds) == 0 && len(mos) == 0 && !weekend {
		return true, ""
	}
	near := func(p time.Time) bool { return p.Sub(ref) <= 8*24*time.Hour && ref.Sub(p) <= 8*24*time.Hour }
	for _, d := range dates {
		for _, s := range edges(d) {
			p, ok := holdoutDay(s)
			if !ok {
				continue
			}
			wd := (int(p.Weekday()) + 6) % 7
			for _, w := range wds {
				if wd == w && near(p) {
					return true, ""
				}
			}
			if weekend && wd >= 4 && near(p) {
				return true, ""
			}
			for _, m := range mos {
				if p.Month() == m {
					return true, ""
				}
			}
		}
	}
	return false, "the day words do not fit the dates"
}
