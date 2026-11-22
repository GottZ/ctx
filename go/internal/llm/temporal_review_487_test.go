package llm

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"
)

// The review round of issue #487: the findings of the independent review of
// the first round, each pinned on questions of the review's corpora (the
// whole corpora run in TestRules_DifferentialAgainstTheBase). Asked on Sunday
// 2026-11-22: the last Monday is 2026-11-16, the next 2026-11-23.

var sunday487 = time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)

// TestRules_AGermanPerfektAsksForThePast (finding F1): the German Perfekt
// keeps the past without a list of its participles — the auxiliary in front
// and the participle at the end of a main clause, the participle right before
// the auxiliary at the end of a subordinate clause, with separable and
// inseparable prefixes; behind a form of "sein" a verb that takes "sein".
func TestRules_AGermanPerfektAsksForThePast(t *testing.T) {
	past := wantDate{"2026-11-16", "", "past"}
	checkRules(t, sunday487, map[string]wantDate{
		"Was haben wir am Montag festgelegt?":                   past,
		"Weißt du noch, was wir am Montag festgelegt haben?":    past,
		"Was haben wir am Montag übersehen?":                    past,
		"Weißt du noch, was wir am Montag vergessen haben?":     past,
		"Was haben wir am Montag verworfen?":                    past,
		"Was haben wir am Montag bemerkt?":                      past,
		"Was haben wir am Montag beantwortet?":                  past,
		"Was haben wir am Montag entfernt?":                     past,
		"Was haben wir am Montag erhalten?":                     past,
		"Was haben wir am Montag vorbereitet?":                  past,
		"Weißt du noch, was wir am Montag zurückgerollt haben?": past,
		"Was haben wir am Montag runtergefahren?":               past,
		"Was haben wir am Montag zusammengeführt?":              past,
		"Weißt du noch, was wir am Montag freigegeben haben?":   past,
		"Was haben wir am Montag fertiggestellt?":               past,
		"Was haben wir am Montag zerstört?":                     past,
		"was haben wir am montag festgelegt":                    past,
		"WAS HABEN WIR AM MONTAG FESTGELEGT?":                   past,
		"Was haben wir am Montag zur Migration festgelegt?":     past,
		"Was haben wir am Montag beim Deploy übersehen?":        past,
		"Zeig mir, was am Montag passiert ist.":                 past,
		"Zeig mir, was am Montag kaputtgegangen ist.":           past,
		"Zeig mir, was am Montag hängengeblieben ist.":          past,
		"Was ist am Montag schiefgelaufen?":                     past,
		"Was ist am Montag aufgetreten?":                        past,
		"Welche Tests sind am Freitag fehlgeschlagen?":          {"2026-11-20", "", "past"},
		"Warum ist der Build am Montag abgebrochen?":            past,
		"Was ist am Montag gemacht worden?":                     past,
		"Was hätten wir am Montag tun sollen?":                  past,
		"Was wäre am Montag passiert?":                          past,
		"Was haben wir von März bis Mai festgelegt?":            {"2026-03-01", "2026-05-31", "range"},
		"Was haben wir von Montag bis Mittwoch übersehen?":      {"2026-11-16", "2026-11-18", "range"},
		"Was haben wir am Wochenende vergessen?":                {"2026-11-21", "2026-11-22", "past"},
	})
}

// TestRules_AStateIsNoPast (finding F4): "haben" and "sein" as full verbs and
// the Zustandspassiv ask for the present: "sein" with the participle of a verb
// that does not take "sein" may name a state, which the form cannot tell from
// the Perfekt — no past, the bare weekday stays the next one. A noun at the
// end of the clause is no participle.
func TestRules_AStateIsNoPast(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Ist das Büro am Samstag geöffnet?":     {"2026-11-28", "", "future"},
		"Hat das Büro am Samstag geöffnet?":     {"2026-11-28", "", "future"},
		"Was ist am Montag geplant?":            {"2026-11-23", "", "future"},
		"Zeig mir, was am Montag geplant ist.":  {"2026-11-23", "", "future"},
		"Ist am Montag ein Termin eingetragen?": {"2026-11-23", "", "future"},
		"Ist der Raum am Freitag reserviert?":   {"2026-11-27", "", "future"},
		"Was haben wir am Montag vor?":          {"2026-11-23", "", "future"},
		"Haben wir am Montag einen Bericht?":    {"2026-11-23", "", "future"},
		"Wer hat am Montag Bereitschaft?":       {"2026-11-23", "", "future"},
		"wer hat am montag bereitschaft":        {"2026-11-23", "", "future"},
		"Hätten wir am Montag Zeit?":            {"2026-11-23", "", "future"},
		"Wird am Montag deployed?":              {"2026-11-23", "", "future"},
		"Was ist am Wochenende geplant?":        {"2026-11-28", "2026-11-29", "future"},
		"Haben wir am Montag Verlust?":          {"2026-11-23", "", "future"},
		"haben wir am montag einen bericht":     {"2026-11-23", "", "future"},
	})
}

// TestRules_AnEnglishWasIsPast (finding F3): "Was" before a name or a noun
// without an article is the English past of "to be" when the question is
// English: the language of the whole question decides, not the shape of the
// word behind "was".
func TestRules_AnEnglishWasIsPast(t *testing.T) {
	monday, friday := wantDate{"2026-11-16", "", "past"}, wantDate{"2026-11-20", "", "past"}
	checkRules(t, sunday487, map[string]wantDate{
		"Was Damien in the standup on Friday?":     friday,
		"Was Robert at the meeting on Monday?":     monday,
		"Was Jürgen there on Monday?":              monday,
		"Was Karsten in the office on Friday?":     friday,
		"Was Qwen slow on Monday?":                 monday,
		"Was herbert offline on Friday?":           friday,
		"Was Vault sealed on Monday?":              monday,
		"Was incident 42 resolved on Friday?":      friday,
		"Was rollout complete on Monday?":          monday,
		"Was deployment successful on Friday?":     friday,
		"Was content indexed on Monday?":           monday,
		"Was test coverage green on Friday?":       friday,
		"Was agent X running on Monday?":           monday,
		"Was output correct on Friday?":            friday,
		"Was token usage high on Monday?":          monday,
		"Was Damien off from Monday to Wednesday?": {"2026-11-16", "2026-11-18", "range"},
		// German: the pronoun.
		"Was kommt am Montag?":          {"2026-11-23", "", "future"},
		"Weißt du was am Dienstag ist?": {"2026-11-24", "", "future"},
	})
}

// TestRules_TheTenseOfTheDatesClause: a date counts by the tense of its own
// clause when that clause has a verb of its own, else by the question's
// ("Sie sagten, wir treffen uns am Montag" asks for the coming Monday).
func TestRules_TheTenseOfTheDatesClause(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Sie sagten, wir treffen uns am Montag.":       {"2026-11-23", "", "future"},
		"Wir hatten vor, am Montag zu deployen.":       {"2026-11-23", "", "future"},
		"Ich habe vergessen, was am Montag ansteht.":   {"2026-11-23", "", "future"},
		"Ich will wissen, was am Montag passiert ist.": {"2026-11-16", "", "past"},
		"Am Montag, was ist da passiert?":              {"2026-11-16", "", "past"},
		"Was war los, am Montag?":                      {"2026-11-16", "", "past"},
		"It was broken on Monday, why?":                {"2026-11-16", "", "past"},
	})
}

// TestRules_AYearNeedsProof (finding F2): a four-digit number is a window's
// edge only as a year in the window of the years a content date carries and
// at most two years ahead, without a word in its clause that makes it a
// count, a position or a name, and not in a question for a count — the end
// of its clause is no proof alone. Each question goes to the LLM fallback as
// at the base.
func TestRules_AYearNeedsProof(t *testing.T) {
	for _, q := range []string{
		"Welche Tokens kommen nach 2048?", "Läuft der Dienst auf Port ab 2024?", "Welche Ports sind zwischen 2020 und 2030?",
		"Welche Ports liegen zwischen 2020 und 2026?", "Werden Zeilen nach 2025 ignoriert?", "Zeig mir die Zeilen ab 2024.",
		"Zeig mir die Logzeilen nach 2025.", "Which issues are between 2024 and 2026?", "Show me the lines after 2024.",
		"Wie viel kostet es ab 2023?", "Zähle von 2020 bis 2026.", "Count from 2020 to 2026.", "Nach wie vielen Requests, ab 2024?",
		"Was ist mit den Requests ab 2024, werden die gedrosselt?", "Was passiert nach 2024 in der Queue?",
		"Ab 2022 und weniger ist alles ok.", "After 2025 or more requests it fails.", "Ist nach 2030 noch jemand im Büro?",
		"Skip until 2025.", "Überspring alles bis 2025.", "Was bedeutet Fehlercode ab 2024?", "Was kam in der Liste nach 2025?",
		"Welcher Wert folgt nach 2024?", "Welche IDs liegen zwischen 2021 und 2024?", "Does the firewall block ports above 2024 or after 2024?",
		"Ab 2025 im Kontext wird es langsam.", "Was steht bis 2025 in der Ausgabe?", "Läuft das Backup ab 2045?",
	} {
		if tr := NormalizeTemporalRules(q, sunday487); tr != nil {
			t.Errorf("%q: %+v, want no window and no date", q, tr.Dates)
		}
	}
	// The years of the question are edges.
	checkRules(t, sunday487, map[string]wantDate{
		"Was kam nach 2025?":                           {"2026-01-01", "", DirFrom},
		"since 2024, lines have grown":                 {"2024-01-01", "2026-11-22", "range"},
		"Seit 2024 im Einsatz?":                        {"2024-01-01", "2026-11-22", "range"},
		"Ab 2027 gilt die neue Lizenz.":                {"2027-01-01", "", DirFrom},
		"Welche Blöcke stammen aus der Zeit vor 2025?": {"2024-12-31", "", DirUntil},
	})
}

// TestRules_AYearAtTheEnd (finding F6): with a year as the end a month
// without its year at the start lies in that year, asked in any month; after
// "seit" only a month that has begun. A window with a year as either edge
// carries the linear weight alone.
func TestRules_AYearAtTheEnd(t *testing.T) {
	jan1 := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	checkRules(t, jan1, map[string]wantDate{
		"Was lief von Juni bis 2026?":        {"2026-06-01", "2026-12-31", "range"},
		"Was lief von Anfang März bis 2026?": {"2026-03-01", "2026-12-31", "range"},
		"Was steht von Oktober bis 2026 an?": {"2026-10-01", "2026-12-31", "range"},
		"Was lief seit Dezember bis 2026?":   {"2025-12-01", "2026-12-31", "range"},
	})
	for _, q := range []string{"Was lief von 28.12.2025 bis 2025?", "Was lief von Montag bis 2026?", "Was lief von Juni bis 2026?"} {
		tr := NormalizeTemporalRules(q, sunday487)
		if tr == nil || len(tr.DimensionWeights) != 1 || tr.DimensionWeights["linear"] != 1 {
			t.Errorf("%q: %+v, want the linear weight alone", q, tr)
		}
	}
}

// TestParseTemporalResponse_AnUnknownDirIsNoDirection (finding F6): a dir the
// temporal prompt does not ask for is no direction — "from", which the query
// path would read as a window open towards the future, among them.
func TestParseTemporalResponse_AnUnknownDirIsNoDirection(t *testing.T) {
	raw := `{"dates":[{"ref":"a","date":"2026-03-01","end":null,"dir":"from"},` +
		`{"ref":"b","date":"2026-03-02","end":null,"dir":"backwards"},` +
		`{"ref":"c","date":"2026-03-03","end":null,"dir":"until"},` +
		`{"ref":"d","date":"2026-03-04","end":null,"dir":"past"},` +
		`{"ref":"e","date":"2026-03-05","end":"2026-03-06","dir":"range"}],"query":"q"}`
	tr, err := parseTemporalResponse(raw, "q")
	if err != nil || tr == nil {
		t.Fatalf("parseTemporalResponse: %v, %v", tr, err)
	}
	want := []string{"", "", DirUntil, "past", "range"}
	for i, d := range tr.Dates {
		if d.Dir != want[i] {
			t.Errorf("date %s: dir %q, want %q", d.Ref, d.Dir, want[i])
		}
	}
}

// TestRules_TheReviewsSurvivingMutants (finding F7): the places the
// review's surviving mutants changed that still exist — an -iert participle
// as a word behind a year (M11), the colon as the end of a clause (M13), and
// the end of a clause as the end of the main clause of a Perfekt (M20). The
// places of M01, M05, M06 and M09 are gone (the year to come before a word,
// the shape of the word behind "was", the conjunctions before "was").
func TestRules_TheReviewsSurvivingMutants(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Was hat sich seit 2024 etabliert?":                  {"2024-01-01", "2026-11-22", "range"},
		"Ports: seit 2024 gibt es neue Regeln.":              {"2024-01-01", "2026-11-22", "range"},
		"Was haben wir am Montag festgelegt, und wer kommt?": {"2026-11-16", "", "past"},
	})
}

// TestRules_YearEdgesAgainstAnOracle: the year forms at random reference days
// from 2021 to 2030 and random years from 2020 to 2099 against an oracle
// written apart from the rules — the window of each form, none for a year
// more than two years ahead and for "seit" a year to come, the linear weight
// alone, and every window starting on or before the day it ends (the
// review's property test, taken over with the window of the years ahead).
func TestRules_YearEdgesAgainstAnOracle(t *testing.T) {
	rng := rand.New(rand.NewPCG(2026, 11))
	ymd := func(y int, m time.Month, d int) string { return fmt.Sprintf("%04d-%02d-%02d", y, m, d) }
	type form struct {
		q              string
		date, end, dir string // date "" = no window
	}
	rounds := 10000
	if testing.Short() {
		rounds = 2000
	}
	checked, failed := 0, 0
	for i := 0; i < rounds; i++ {
		now := time.Date(2021, 1, 1, 12, 0, 0, 0, time.UTC).AddDate(0, 0, rng.IntN(3650))
		today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		tf := today.Format("2006-01-02")
		y, y2 := 2020+rng.IntN(80), 2020+rng.IntN(80)
		edge := func(y int) bool { return y <= now.Year()+2 }
		jan1 := func(y int) time.Time { return time.Date(y, 1, 1, 0, 0, 0, 0, time.UTC) }
		var forms []form
		add := func(ok bool, f form) {
			if !ok {
				f.date, f.end, f.dir = "", "", ""
			}
			forms = append(forms, f)
		}
		add(edge(y), form{fmt.Sprintf("Was kam nach %d?", y), ymd(y+1, 1, 1), "", DirFrom})
		add(edge(y), form{fmt.Sprintf("Was gilt ab %d?", y), ymd(y, 1, 1), "", DirFrom})
		add(edge(y) && y <= now.Year(), form{fmt.Sprintf("Was hat sich seit %d geändert?", y), ymd(y, 1, 1), tf, "range"})
		if jan1(y).Before(today) {
			add(edge(y), form{fmt.Sprintf("Was galt bis %d?", y), ymd(y, 12, 31), "", DirUntil})
		} else {
			add(edge(y), form{fmt.Sprintf("Was galt bis %d?", y), tf, ymd(y, 12, 31), "range"})
		}
		if jan1(y).After(today) {
			add(edge(y), form{fmt.Sprintf("Was galt vor %d?", y), tf, ymd(y-1, 12, 31), "range"})
		} else {
			add(edge(y), form{fmt.Sprintf("Was galt vor %d?", y), ymd(y-1, 12, 31), "", DirUntil})
		}
		lo, hi := min(y, y2), max(y, y2)
		both := edge(y) && edge(y2)
		add(both, form{fmt.Sprintf("Was lief von %d bis %d?", y, y2), ymd(lo, 1, 1), ymd(hi, 12, 31), "range"})
		add(both, form{fmt.Sprintf("What happened between %d and %d?", y, y2), ymd(lo, 1, 1), ymd(hi, 12, 31), "range"})
		add(both, form{fmt.Sprintf("Was lief zwischen %d und %d?", y, y2), ymd(lo, 1, 1), ymd(hi, 12, 31), "range"})
		add(both, form{fmt.Sprintf("What happened from %d to %d?", y, y2), ymd(lo, 1, 1), ymd(hi, 12, 31), "range"})
		for _, f := range forms {
			checked++
			tr := NormalizeTemporalRules(f.q, now)
			got, gotEnd, gotDir := "", "", ""
			if tr != nil && len(tr.Dates) > 0 {
				d := tr.Dates[0]
				got, gotDir = d.Date, d.Dir
				if d.End != nil {
					gotEnd = *d.End
				}
				if gotEnd != "" && gotEnd < got {
					t.Errorf("now %s %q: window %s..%s starts after it ends", tf, f.q, got, gotEnd)
				}
				if len(tr.Dates) != 1 || len(tr.DimensionWeights) != 1 || tr.DimensionWeights["linear"] != 1 {
					t.Errorf("now %s %q: %+v, weights %v, want one window with the linear weight alone", tf, f.q, tr.Dates, tr.DimensionWeights)
				}
			}
			if got != f.date || gotEnd != f.end || gotDir != f.dir {
				failed++
				if failed <= 20 {
					t.Errorf("now %s %q: got %s..%s %s, want %s..%s %s", tf, f.q, got, gotEnd, gotDir, f.date, f.end, f.dir)
				}
			}
		}
	}
	t.Logf("checked %d, failed %d", checked, failed)
}
