package llm

import "testing"

// The questions below are asked on Sunday 2026-11-22 (now517) unless a test
// names another day; the expectations are written as check517 reads them.

// TestRules_AnInterrogativeWithoutAVerbTakesTheTenseOfTheClauseBefore (issue
// #532, point 1): an interrogative without a verb of its own ("was am
// Dienstag", "and what on Thursday") shares the verb of the clause before it
// and with it its tense — not a neutral one of its own, and not the whole
// question's, which a third clause may turn ("…, was wird am Freitag, was am
// Samstag?"). Behind "und"/"and" it is a clause of its own. A clause that
// puts a verb last ("was am Montag ansteht") or leaves a separable particle
// there ("und was am Freitag vor?") has a verb of its own.
func TestRules_AnInterrogativeWithoutAVerbTakesTheTenseOfTheClauseBefore(t *testing.T) {
	check517(t, now517, want517{
		"Was war am Montag, was am Dienstag?":                      "2026-11-16/past 2026-11-17/past",
		"Was ist am Montag passiert und was am Mittwoch?":          "2026-11-16/past 2026-11-18/past",
		"Was haben wir am Dienstag gemerged und was am Freitag?":   "2026-11-17/past 2026-11-20/past",
		"What broke on Monday, and what on Thursday?":              "2026-11-16/past 2026-11-19/past",
		"Who deployed on Tuesday and who on Friday?":               "2026-11-17/past 2026-11-20/past",
		"Wer hatte am Montag Dienst, wer am Mittwoch?":             "2026-11-16/past 2026-11-18/past",
		"Was steht am Montag an, was am Dienstag?":                 "2026-11-23/future 2026-11-24/future",
		"Was war am Montag, was wird am Freitag, was am Samstag?":  "2026-11-16/past 2026-11-27/future 2026-11-28/future",
		"Was haben wir am Montag gemacht, und was am Freitag vor?": "2026-11-16/past 2026-11-27/future",
		"Ich habe vergessen, was am Montag ansteht.":               "2026-11-23/future",
	})
}

// TestRules_ButAndAberStartAClause (issue #532, point 1): "but" and "aber"
// without a comma start a clause where both sides have a verb of their own,
// and a side whose verb the rules read only as a past ("The job crashed …")
// has one; between phrases they join them.
func TestRules_ButAndAberStartAClause(t *testing.T) {
	check517(t, now517, want517{
		"The job crashed on Saturday but it will run again on Tuesday": "2026-11-21/past 2026-11-24/future",
		"It failed on Friday but it will be fixed on Monday":           "2026-11-20/past 2026-11-23/future",
		"Der Build lief am Montag aber am Dienstag wird er neu gebaut": "2026-11-16/past 2026-11-24/future",
		// counter-cases: no verb of its own behind the conjunction
		"Did it fail on Monday but succeed on Tuesday?":         "2026-11-16/past 2026-11-17/past",
		"Lief der Job am Montag aber nicht am Dienstag?":        "2026-11-16/past 2026-11-17/past",
		"Kommt der Patch am Montag aber erst am Mittwoch live?": "2026-11-23/future 2026-11-25/future",
	})
}

// TestRules_AClauseThatPutsItsVerbLastReadsItsLastWord (issue #532, point
// 1): a clause that an interrogative opens at the start of a sentence, or a
// relative pronoun inside one, before a word that is no verb puts its verb
// last; a last word of the shape of the simple past is that past ("Was am
// Freitag kaputtging, …", "…, die am Montag auftraten, …").
func TestRules_AClauseThatPutsItsVerbLastReadsItsLastWord(t *testing.T) {
	check517(t, now517, want517{
		"Was am Freitag kaputtging, reparieren wir am Montag":             "2026-11-20/past 2026-11-23/future",
		"Die Fehler, die am Montag auftraten, werden am Freitag gefixt?":  "2026-11-16/past 2026-11-27/future",
		"Der Server, der am Dienstag ausfiel, wird am Donnerstag ersetzt": "2026-11-17/past 2026-11-26/future",
		// counter-cases: a present at the end, an article before a noun
		"Was am Montag ansteht, besprechen wir am Freitag": "2026-11-23/future 2026-11-27/future",
		"Die Jobs, die am Montag laufen, sind wichtig":     "2026-11-23/future",
	})
}

// TestRules_ACoordinatedEnglishParticipleSharesItsAuxiliaryAcrossADate (issue
// #532, point 1): a participle coordinated with one behind "be" shares that
// auxiliary also with a date between them.
func TestRules_ACoordinatedEnglishParticipleSharesItsAuxiliaryAcrossADate(t *testing.T) {
	check517(t, now517, want517{
		"Will it be fixed on Monday and deployed on Tuesday?":               "2026-11-23/future 2026-11-24/future",
		"Will the patch be reviewed on Tuesday and merged on Thursday?":     "2026-11-24/future 2026-11-26/future",
		"It was fixed on Monday and we deployed on Tuesday":                 "2026-11-16/past 2026-11-17/past",
		"The fix was reviewed on Monday and the team deployed it on Friday": "2026-11-16/past 2026-11-20/past",
	})
}

// TestRules_AnEnglishRelativeClauseEndsAtTheAuxiliaryOfItsMainClause (issue
// #532, point 1): a relative clause opened by "that", "which" or "who"
// without a comma ends at an auxiliary or modal two words or more behind its
// opener that follows no subject.
func TestRules_AnEnglishRelativeClauseEndsAtTheAuxiliaryOfItsMainClause(t *testing.T) {
	check517(t, now517, want517{
		"Which PRs that we merged on Monday will ship on Friday?":    "2026-11-16/past 2026-11-27/future",
		"Which bugs that we found on Tuesday are fixed by Thursday?": "2026-11-17/past 2026-11-26/future",
		"The jobs that failed on Monday will rerun on Wednesday":     "2026-11-16/past 2026-11-25/future",
		// counter-cases: the auxiliary is the relative clause's own
		"Is it true that we will ship on Friday?": "2026-11-27/future",
	})
}

// TestRules_LetsOpensAClauseOfAProposal (issue #532, point 1): "Let's" opens a
// clause with a verb of its own that names no past, as "Lass uns" does.
func TestRules_LetsOpensAClauseOfAProposal(t *testing.T) {
	check517(t, now517, want517{
		"Let's talk on Monday about what happened on Friday": "2026-11-23/future 2026-11-20/past",
		"Let’s review on Tuesday what broke on Saturday":     "2026-11-24/future 2026-11-21/past",
	})
}
