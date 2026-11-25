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
// counter-cases, asked at noon UTC on several days. No question the rules
// answered right at 5a28c475 (the last column) may be one they answer wrong
// now, and every file keeps at least the number of questions it had right
// when issue #532's pull request landed (holdout532Floor). Right is
// holdout532Right.
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
	"a-tageszeit":            90,
	"b-zwei-wochentage":      148,
	"c-englisch-praeteritum": 153,
	"d1-ellipse":             53,
	"d2-ten-praesens":        59,
	"d3-imperativ":           57,
	"e-restformen":           61,
	"f-kontrolle":            199,
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
// mark carries that hour ("@-": none); NONE fits only no answer.
func holdout532Right(spec, answer string) bool {
	got := map[string]string{}
	if answer != "NIL" {
		for _, it := range strings.Split(answer, ";") {
			key, hour, _ := strings.Cut(it, "@")
			got[oneDay532(key)] = hour
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
			hour, ok := got[oneDay532(key)]
			switch {
			case !ok:
				fits = false
			case marked && want == "-":
				fits = fits && hour == ""
			case marked:
				fits = fits && hour == want
			}
		}
		if fits {
			return true
		}
	}
	return false
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
