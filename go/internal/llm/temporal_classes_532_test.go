package llm

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestRules_HoldoutOfIssue532 is the gate of the holdout of issue #532
// (testdata/temporal_classes_532): per class of the issue — a day keyword
// with a part of the day, two weekdays in a question, an English simple past
// behind a noun, the three readings #517 left out, the rest forms of the
// third holdout — and a control group, questions with their expectations
// written and hashed before any rule changed, a third or more of each class
// counter-cases, asked at noon UTC on several days — and the holdout of the
// review round (the files r1-*: English forms of the past and their
// homographs, imperatives, a past and a coming clause, names, participle
// clauses), written and hashed before the round's first edit, half of each
// flank counter-cases. No question the rules answered right at 5a28c475 (the
// last column) may be one they answer wrong now, and every file keeps at
// least the number of questions it had right when issue #532's pull request
// landed (holdout532Floor). Right is holdout532Right.
func TestRules_HoldoutOfIssue532(t *testing.T) {
	files, err := filepath.Glob("testdata/temporal_classes_532/*.tsv")
	if err != nil || len(files) == 0 {
		t.Fatalf("no holdout: %v", err)
	}
	type tally struct{ n, base, now int }
	var report strings.Builder
	report.WriteString("file/kind: questions / right at 5a28c475 / right now\n")
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".tsv")
		byKind := map[string]*tally{}
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
			if len(col) != 6 {
				t.Fatalf("%s:%d: %d columns, want 6", file, line, len(col))
			}
			day, err := time.Parse("2006-01-02", col[1])
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			now := canon532(NormalizeTemporalRules(col[3], day.Add(12*time.Hour)))
			baseOK, nowOK := holdout532Right(col[4], col[5]), holdout532Right(col[4], now)
			for _, k := range []string{col[2], "total"} {
				c := byKind[k]
				if c == nil {
					c = &tally{}
					byKind[k] = c
				}
				c.n++
				c.base += b2i(baseOK)
				c.now += b2i(nowOK)
			}
			if baseOK && !nowOK {
				t.Errorf("%s:%d %s %q: answered right at 5a28c475, wrong now: base %s, now %s, expected %s",
					file, line, col[0], col[3], col[5], now, col[4])
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if byKind["total"] == nil {
			t.Fatalf("%s: the holdout holds no question", file)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		sort.Slice(kinds, func(i, j int) bool { return kinds[j] == "total" || kinds[i] != "total" && kinds[i] < kinds[j] })
		for _, k := range kinds {
			c := byKind[k]
			fmt.Fprintf(&report, "%s/%s: %d / %d / %d\n", name, k, c.n, c.base, c.now)
		}
		floor, ok := holdout532Floor[name]
		if !ok {
			t.Errorf("%s: no floor in holdout532Floor", name)
		}
		if got := byKind["total"].now; got < floor {
			t.Errorf("%s: right now %d, fewer than the %d of issue #532's pull request", name, got, floor)
		}
	}
	t.Logf("holdout of #532:\n%s", report.String())
}

// holdout532Floor is, per class of the holdout of issue #532, the number of
// questions the rules answered right when the issue's pull request landed.
var holdout532Floor = map[string]int{
	"a-tageszeit":             90,
	"b-zwei-wochentage":       148,
	"c-englisch-praeteritum":  153,
	"d1-ellipse":              56,
	"d2-ten-praesens":         59,
	"d3-imperativ":            57,
	"e-restformen":            61,
	"f-kontrolle":             199,
	"r1-n1-englische-formen":  133,
	"r1-n2-imperative":        88,
	"r1-n3-vergangen-kommend": 44,
	"r1-n4-namen":             25,
	"r1-n5-partizip-ellipsen": 34,
}

// canon532 writes the dates of r as the holdout of issue #532 reads them:
// D:day, W:start..end, F:from, U:until, each with "@hour" where it carries
// one, joined by ";" in the order of the answer; NIL for no answer.
func canon532(r *TemporalResult) string {
	if r == nil || len(r.Dates) == 0 {
		return "NIL"
	}
	items := make([]string, 0, len(r.Dates))
	for _, d := range r.Dates {
		var s string
		switch {
		case d.End != nil:
			s = "W:" + d.Date + ".." + *d.End
		case d.Dir == DirUntil:
			s = "U:" + d.Date
		case d.Dir == DirFrom:
			s = "F:" + d.Date
		default:
			s = "D:" + d.Date
		}
		if d.Hour != nil {
			s += "@" + strconv.Itoa(*d.Hour)
		}
		items = append(items, s)
	}
	return strings.Join(items, ";")
}

// holdout532Right reports whether answer, written by canon532, fits the
// expectation spec: some alternative (split by " | ") names the same set of
// dates (a window of one day is that day), and every date of it with an hour
// mark carries that hour each time the answer names it ("@-": none); NONE
// fits only no answer.
func holdout532Right(spec, answer string) bool {
	got := map[string][]string{}
	if answer != "NIL" {
		for _, it := range strings.Split(answer, ";") {
			key, hour, _ := strings.Cut(it, "@")
			got[oneDay532(key)] = append(got[oneDay532(key)], hour)
		}
	}
	for _, alt := range strings.Split(spec, " | ") {
		if alt = strings.TrimSpace(alt); alt == "NONE" {
			if len(got) == 0 {
				return true
			}
			continue
		}
		items := strings.Split(alt, ";")
		if len(items) != len(got) {
			continue
		}
		fits := true
		for _, it := range items {
			key, want, marked := strings.Cut(it, "@")
			hours, ok := got[oneDay532(key)]
			if !ok {
				fits = false
				continue
			}
			if want == "-" {
				want = ""
			}
			for _, hour := range hours {
				fits = fits && (!marked || hour == want)
			}
		}
		if fits {
			return true
		}
	}
	return false
}

// TestHoldout532RightHoldsEveryHour: an expectation with an hour mark fits
// only an answer whose every hour of that date is the marked one, in either
// order; the same date twice with the same hour, and an expectation without a
// mark, fit as before.
func TestHoldout532RightHoldsEveryHour(t *testing.T) {
	for _, c := range []struct {
		spec, answer string
		want         bool
	}{
		{"D:2026-11-23@9", "D:2026-11-23@17;D:2026-11-23@9", false},
		{"D:2026-11-23@9", "D:2026-11-23@9;D:2026-11-23@17", false},
		{"D:2026-11-23@-", "D:2026-11-23;D:2026-11-23@9", false},
		{"D:2026-11-23@-", "D:2026-11-23@9;D:2026-11-23", false},
		{"D:2026-11-23@9", "D:2026-11-23@9;D:2026-11-23@9", true},
		{"D:2026-11-23@9", "W:2026-11-23..2026-11-23@9;D:2026-11-23@9", true},
		{"D:2026-11-23", "D:2026-11-23@17;D:2026-11-23@9", true},
		{"D:2026-11-23@-", "D:2026-11-23", true},
		{"D:2026-11-23@9;D:2026-11-24", "D:2026-11-24@19;D:2026-11-23@9", true},
		{"NONE", "NIL", true},
	} {
		if got := holdout532Right(c.spec, c.answer); got != c.want {
			t.Errorf("holdout532Right(%q, %q) = %v, want %v", c.spec, c.answer, got, c.want)
		}
	}
}

// oneDay532 writes a window of one day as that day.
func oneDay532(item string) string {
	if w, ok := strings.CutPrefix(item, "W:"); ok {
		if a, b, _ := strings.Cut(w, ".."); a == b {
			return "D:" + a
		}
	}
	return item
}
