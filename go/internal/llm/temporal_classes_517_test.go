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

// TestRules_ClassCorporaOfIssue517 is the gate of the class corpora of issue
// #517 (testdata/temporal_classes_517): for each class of the issue — a
// weekday that is a surname, a weekday with a time of day, a coordinated
// clause with its own finite verb, an ellipsis of participles, a year before
// an English participle, a word on -ten at a finite verb's place and a clause
// of an imperative — and for the bot review's points, questions with their
// expectation written and hashed before any rule changed, half of them
// counter-cases in which the new reading would be wrong, asked at noon UTC of
// 2026-11-22 (a Sunday) and of 2026-03-18 (a Wednesday). No question the rules
// answered right at c367fd44 (the base column) may be one they answer wrong
// now, unless its expectation is marked as disputed; and every file keeps at
// least the number of questions it had right when issue #517's pull request
// landed (classCorpusFloor). Right is classRight. The counts per file and
// kind are logged.
func TestRules_ClassCorporaOfIssue517(t *testing.T) {
	files, err := filepath.Glob("testdata/temporal_classes_517/*.tsv")
	if err != nil || len(files) == 0 {
		t.Fatalf("no class corpus: %v", err)
	}
	type tally struct{ n, base, now, worse, better int }
	var report strings.Builder
	report.WriteString("file/kind: questions / right at c367fd44 / right now / wrong now, right at the base / right now, wrong at the base\n")
	checked := 0
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
			if len(col) != 11 {
				t.Fatalf("%s:%d: %d columns, want 11", file, line, len(col))
			}
			e := classCase{id: col[0], kind: col[2], rule: col[3], spec: col[4], hour: col[5], tense: col[6], disputed: col[7] == "s", q: col[8]}
			day, err := time.Parse("2006-01-02", col[1])
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			var baseDates []TemporalDate
			if col[9] != "nil" {
				if err := json.Unmarshal([]byte(col[9]), &baseDates); err != nil {
					t.Fatalf("%s:%d: the base column is no JSON list of dates: %v", file, line, err)
				}
			}
			baseOK, _, err := classRight(e, baseDates, col[10], day)
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			var nowDates []TemporalDate
			if r := NormalizeTemporalRules(e.q, day.Add(12*time.Hour)); r != nil {
				nowDates = r.Dates
			}
			nowOK, why, err := classRight(e, nowDates, DetectVerbTense(e.q), day)
			if err != nil {
				t.Fatalf("%s:%d: %v", file, line, err)
			}
			checked++
			if e.disputed {
				continue
			}
			for _, k := range []string{e.kind, "total"} {
				c := byKind[k]
				if c == nil {
					c = &tally{}
					byKind[k] = c
				}
				c.n++
				c.base += b2i(baseOK)
				c.now += b2i(nowOK)
				c.worse += b2i(baseOK && !nowOK)
				c.better += b2i(nowOK && !baseOK)
			}
			if baseOK && !nowOK {
				got, _ := json.Marshal(nowDates)
				t.Errorf("%s:%d %s %q: answered right at c367fd44, wrong now (%s): base %s, now %s, expected %s (hour %s, tense %s)",
					file, line, e.id, e.q, why, col[9], got, e.spec, e.hour, e.tense)
			}
		}
		if err := sc.Err(); err != nil {
			t.Fatal(err)
		}
		f.Close()
		if byKind["total"] == nil {
			t.Fatalf("%s: the corpus holds no undisputed question", file)
		}
		kinds := make([]string, 0, len(byKind))
		for k := range byKind {
			kinds = append(kinds, k)
		}
		sort.Slice(kinds, func(i, j int) bool { return kinds[j] == "total" || kinds[i] != "total" && kinds[i] < kinds[j] })
		for _, k := range kinds {
			c := byKind[k]
			fmt.Fprintf(&report, "%s/%s: %d / %d / %d / %d / %d\n", name, k, c.n, c.base, c.now, c.worse, c.better)
		}
		floor, ok := classCorpusFloor[name]
		if !ok {
			t.Errorf("%s: no floor in classCorpusFloor", name)
		}
		if got := byKind["total"].now; got < floor {
			t.Errorf("%s: right now %d, fewer than the %d of issue #517's pull request", name, got, floor)
		}
	}
	if checked == 0 {
		t.Fatal("the class corpora hold no question")
	}
	t.Logf("class corpora of #517, disputed questions left out:\n%s", report.String())
}

// classCorpusFloor is, per class corpus, the number of undisputed questions
// the rules answered right when issue #517's pull request landed: a floor
// that keeps its improvements counted.
var classCorpusFloor = map[string]int{
	"k1-nachname":      58,
	"k2-tageszeit":     71,
	"k3-klauseln":      69,
	"k4-ellipse":       26,
	"k5-jahr-partizip": 44,
	"k6-ten-imperativ": 51,
	"k7-bot":           36,
}

// classCase is a question of a class corpus of issue #517 with its
// expectation: the kind (F: the new reading is right, F2: the reading of a
// first name before a weekday, G: a counter-case), whether a rule should
// answer it (ja, nein, egal), the expected dates (holdout506Fits), the hour
// of the first date ("-": no date carries one, "*": not judged), the tense
// of the question (DetectVerbTense; "notpast": future or neutral, "any": not
// judged) and whether the expectation is disputed.
type classCase struct {
	id, kind, rule, spec, hour, tense string
	disputed                          bool
	q                                 string
}

// classRight reports whether dates and tense, the rules' answer to e asked on
// day, are right: a rule answers exactly when one should and its dates fit
// (holdout506Right), the first date carries the expected hour, and the
// question has the expected tense.
func classRight(e classCase, dates []TemporalDate, tense string, day time.Time) (bool, string, error) {
	ok, why, err := holdout506Right(e.rule, e.spec, dates, day)
	if err != nil || !ok {
		return ok, why, err
	}
	if e.hour != "*" && len(dates) > 0 {
		if e.hour == "-" {
			for _, d := range dates {
				if d.Hour != nil {
					return false, fmt.Sprintf("an hour %d", *d.Hour), nil
				}
			}
		} else {
			want, err := strconv.Atoi(e.hour)
			if err != nil {
				return false, "", fmt.Errorf("hour %q is no number", e.hour)
			}
			if dates[0].Hour == nil || *dates[0].Hour != want {
				return false, fmt.Sprintf("hour %v instead of %d", hourOf(dates[0]), want), nil
			}
		}
	}
	switch e.tense {
	case "any":
	case "notpast":
		if tense == "past" {
			return false, "tense past", nil
		}
	case "past", "future", "neutral":
		if tense != e.tense {
			return false, "tense " + tense + " instead of " + e.tense, nil
		}
	default:
		return false, "", fmt.Errorf("tense %q is none of any, notpast, past, future, neutral", e.tense)
	}
	return true, "", nil
}

// hourOf is the hour of d for a message, "none" without one.
func hourOf(d TemporalDate) string {
	if d.Hour == nil {
		return "none"
	}
	return strconv.Itoa(*d.Hour)
}

// TestHoldoutJudges_AnEmptyDateListIsNoRuleAnswer: a result without a date
// answers nothing — the query path takes any non-nil result as the rules'
// answer and skips the LLM fallback for it — so the holdout judges count it
// as no rule answer (issue #517, from the bot review of #516): before, a
// non-nil result with an empty date list passed where a rule should answer.
func TestHoldoutJudges_AnEmptyDateListIsNoRuleAnswer(t *testing.T) {
	day := time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)
	empty := &TemporalResult{}
	e := holdoutCase{class: "A", tense: "egal", rule: "ja", q: "Was ist da los?"}
	if ok, _ := holdoutJudge(e, empty, "neutral", day); ok {
		t.Error("#487's judge: an empty date list passes where a rule should answer")
	}
	e.rule = "nein"
	if ok, why := holdoutJudge(e, empty, "neutral", day); !ok {
		t.Errorf("#487's judge: an empty date list counts as a rule answer where none should answer: %s", why)
	}
	for _, spec := range []string{"*", "-", "2026-11-20"} {
		if ok, _, err := holdout506Right("ja", spec, []TemporalDate{}, day); err != nil || ok {
			t.Errorf("#506's judge, %q: an empty date list passes where a rule should answer (err %v)", spec, err)
		}
	}
	if ok, why, err := holdout506Right("nein", "-", []TemporalDate{}, day); err != nil || !ok {
		t.Errorf("#506's judge: an empty date list counts as a rule answer where none should answer: %s %v", why, err)
	}
	if ok, why, err := classRight(classCase{rule: "ja", spec: "*", hour: "*", tense: "any"}, nil, "neutral", day); err != nil || ok {
		t.Errorf("the class judge: no date passes where a rule should answer: %s %v", why, err)
	}
}
