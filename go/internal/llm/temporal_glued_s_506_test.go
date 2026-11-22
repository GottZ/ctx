package llm

import (
	"sort"
	"testing"
)

// TestRules_AnImperativeOfLassenIsNoPast (issue #506, the holdout of its pull
// request): "lass" is the imperative of "lassen", not "las" with "es" glued to
// it. A sentence that asks with it — "lass uns …", "lass mal …", "lasst uns
// …" — makes a plan and asks for the coming day, wherever the imperative
// stands and however it is written.
func TestRules_AnImperativeOfLassenIsNoPast(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Lass uns am Freitag releasen":        next506["freitag"],
		"Lass uns am Freitag deployen":        next506["freitag"],
		"Lass uns am Freitag ausrollen":       next506["freitag"],
		"Lass uns am Freitag reden":           next506["freitag"],
		"lass mal am mittwoch pairen":         next506["mittwoch"],
		"lass am mittwoch pairen":             next506["mittwoch"],
		"lass uns am mittwoch treffen":        next506["mittwoch"],
		"Lass den Job am Mittwoch laufen":     next506["mittwoch"],
		"Bitte lass den Job am Montag laufen": next506["montag"],
		"Dann lass uns am Dienstag mergen":    next506["dienstag"],
		"Lasst uns am Freitag releasen":       next506["freitag"],
		// The spoken forms the cut of a glued "s" is for still ask for the
		// past (holdout of the pull request, question 58).
		"Samstag hats den Spark zerlegt": last506["samstag"],
	})
}

// TestRules_ANounOnSIsNoVerbInTheSimplePast (issue #506): a noun whose
// genitive or plural is a simple past the rules know followed by "s" stays
// the noun — "Stands" is no "stand", "Lags" no "lag" — and the question asks
// for the coming day.
func TestRules_ANounOnSIsNoVerbInTheSimplePast(t *testing.T) {
	checkRules(t, sunday487, map[string]wantDate{
		"Welche Stands bauen wir am Freitag auf?": next506["freitag"],
		"Welche Lags erwarten wir am Montag?":     next506["montag"],
	})
}

// sWordsOfTheirOwn are, by this test's own reading, the words "<verb>s" of
// a finite verb the rules know that are German words of their own: the
// imperative of "lassen"; the genitive or plural of a noun ("des Stands",
// "des Lags", "des Stiegs", "des Solls"); the genitive of an infinitive used
// as a noun ("des Könnens"); the participle as an adjective ("ein passiertes
// Problem").
var sWordsOfTheirOwn = []string{
	"lass",
	"stands", "lags", "stiegs", "solls",
	"habens", "werdens", "müssens", "sollens", "könnens", "dürfens", "wollens", "brauchens", "kriegens",
	"passiertes", "ereignetes",
}

// TestColloquial_AGluedSIsCutOnlyFromNoWordOfItsOwn (issue #506): in a
// German sentence colloquial cuts the "s" a question glues to a known finite
// verb ("gabs", "wars", "gings", "gibts", "hats") — but only when the word
// with its "s" is no German word of its own. Every verb the rules know is
// checked with an "s" glued to it, so a verb added to their sets is checked
// as well; the words of their own are this test's own list, not the rules'.
func TestColloquial_AGluedSIsCutOnlyFromNoWordOfItsOwn(t *testing.T) {
	own := toWordSet(sWordsOfTheirOwn...)
	verbs := map[string]bool{}
	for _, set := range []map[string]bool{germanPastVerbs, perfektAux, germanFiniteWords} {
		for v := range set {
			verbs[v] = true
		}
	}
	forms := make([]string, 0, len(verbs))
	for v := range verbs {
		forms = append(forms, v)
	}
	sort.Strings(forms)
	read := func(w string) string {
		c := clauseTokens("Und " + w + " am Freitag noch?")
		return c.words[1]
	}
	seen := map[string]bool{}
	for _, v := range forms {
		w := v + "s"
		want := v
		if own[w] {
			want, seen[w] = w, true
		}
		if got := read(w); got != want {
			t.Errorf("%q in a German sentence reads as %q, want %q", w, got, want)
		}
	}
	for _, w := range sWordsOfTheirOwn {
		if !seen[w] {
			t.Errorf("%q is no finite verb of the rules with an \"s\" glued to it: the list is stale", w)
		}
	}
	// The spoken forms the cut is for, by name.
	for w, want := range map[string]string{"gabs": "gab", "wars": "war", "gings": "ging", "gibts": "gibt", "hats": "hat", "ists": "ist"} {
		if got := read(w); got != want {
			t.Errorf("%q in a German sentence reads as %q, want %q", w, got, want)
		}
	}
	// Other words on "s" whose rest is no verb of the rules stay as written.
	for _, w := range []string{"iss", "miss", "riss", "biss", "wies", "lies", "blies", "stieß", "stiess", "aß", "ass", "hass", "pass",
		"muss", "bis", "das", "dass", "was", "eins", "uns", "es", "als", "aus", "los", "gras", "glas", "haus", "tags", "abends", "montags"} {
		if got := read(w); got != w {
			t.Errorf("%q in a German sentence reads as %q, want it as written", w, got)
		}
	}
}
