package llm

import (
	"strings"
	"testing"
	"time"
)

// The tests below pin the review round of issue #532: forms a fresh review
// of the wave found read wrong where 5a28c475 read them right. Asked on
// Sunday 2026-11-22 (now517) unless a test names another day.

// TestRules_AHomographOfAnEnglishPastIsReadByItsPlace (issue #532, review
// round): a simple past that is another word as well — a name ("Drew"), a
// noun ("stole", "drove", "spoke"), an adjective ("woke"), a quantified
// noun ("Any thought") — is a past only where a simple past stands, behind
// its subject, and a word written with its capital inside its clause is a
// name and no verb. "fell" is the present of "to fell" as well and names no
// past at all; "found" and "saw" stay pasts wherever they stand, also
// before an object without an article.
func TestRules_AHomographOfAnEnglishPastIsReadByItsPlace(t *testing.T) {
	check517(t, now517, want517{
		"Drew presents the roadmap on Friday.":       "2026-11-27/future",
		"Will Drew join the retro on Monday?":        "2026-11-23/future",
		"Can Drew review the PR on Friday?":          "2026-11-27/future",
		"Meet Drew at the office on Monday?":         "2026-11-23/future",
		"Will the stole match the dress on Friday?":  "2026-11-27/future",
		"Woke politics debate is on Saturday?":       "2026-11-28/future",
		"Who chairs the drove auction on Friday?":    "2026-11-27/future",
		"Spoke wheels arrive on Friday.":             "2026-11-27/future",
		"The fell race is on Saturday.":              "2026-11-28/future",
		"Crews fell the dead trees on Monday.":       "2026-11-23/future",
		"Any thought on the plan for Friday?":        "2026-11-27/future",
		"Is the saw in the workshop free on Monday?": "2026-11-23/future",
		// counter-cases: the same forms where a simple past stands
		"The architect drew the topology on Monday, where is it?": "2026-11-16/past",
		"Who spoke with legal on Thursday?":                       "2026-11-19/past",
		"Somebody stole the disk on Saturday?":                    "2026-11-21/past",
		"The pager woke everyone on Thursday, why?":               "2026-11-19/past",
		"The pager woke Anna on Thursday.":                        "2026-11-19/past",
		"Who found the root cause on Monday?":                     "2026-11-16/past",
		"Who saw the alert on Saturday?":                          "2026-11-21/past",
		"The QA team found three bugs on Friday?":                 "2026-11-20/past",
		"Monitoring saw spikes on Monday?":                        "2026-11-16/past",
		"We chose the new region on Friday, right?":               "2026-11-20/past",
	})
}

// TestRules_AnImperativeOpensItsClauseOnlyWhereItCanBeOne (issue #532,
// review round): the German imperatives of a request give their clause a
// verb of its own only where they can be one — not in an English sentence
// ("such", "sage", "check" are English words), not written with their
// capital inside a sentence (a German noun: "Suche", "Check"), not in "sage
// und schreibe", not where another verb of the clause is the finite one, and
// only with something that says so — an object, a pronoun, "bitte", a
// particle at the end, a clause behind it ("Suche am Montag ebenfalls?" at
// the start of a sentence is the noun); an English "check" or "bring" at the
// start of the question or before its object is one.
func TestRules_AnImperativeOpensItsClauseOnlyWhereItCanBeOne(t *testing.T) {
	check517(t, now517, want517{
		"We broke prod on Friday, such a mess on Monday too.":                 "2026-11-20/past 2026-11-16/past",
		"The build broke on Friday, check on Monday too?":                     "2026-11-20/past 2026-11-16/past",
		"Prod went down on Friday, sage advice on Monday?":                    "2026-11-20/past 2026-11-16/past",
		"Am Freitag ging alles schief, Suche am Montag auch kaputt?":          "2026-11-20/past 2026-11-16/past",
		"Am Freitag gab es Ärger, sage und schreibe sieben Alerts am Montag?": "2026-11-20/past 2026-11-16/past",
		"Check am Montag ergab nichts – was lief am Freitag schief?":          "2026-11-16/past 2026-11-20/past",
		"Am Freitag fiel der Login aus. Suche am Montag ebenfalls?":           "2026-11-20/past 2026-11-16/past",
		"am montag kam ein ticket rein, frage am dienstag dazu?":              "2026-11-16/past 2026-11-17/past",
		"am freitag ging der build kaputt, check am montag auch rot?":         "2026-11-20/past 2026-11-16/past",
		// counter-cases: an imperative where it can be one
		"Schau dir am Freitag an, was am Montag schiefging.":              "2026-11-27/future 2026-11-16/past",
		"Suche am Montag die Mail heraus, die am Freitag kam.":            "2026-11-23/future 2026-11-20/past",
		"Der Build brach am Montag, schau dir das am Donnerstag an.":      "2026-11-16/past 2026-11-26/future",
		"The build broke on Monday, check the logs on Thursday.":          "2026-11-16/past 2026-11-26/future",
		"Check on Friday whether the job that failed on Monday was rerun": "2026-11-27/future 2026-11-16/past",
		"Am Montag kam die Lieferung, prüf am Freitag den Bestand.":       "2026-11-16/past 2026-11-27/future",
		"Am Montag brach der Build. Frag am Freitag nach, warum.":         "2026-11-16/past 2026-11-27/future",
	})
}

// TestRules_AnEnglishPresentGivesItsClauseATenseOfItsOwn (issue #532,
// review round): a clause whose verb is an English present of the third
// person behind its subject ("the vendor comes", "her successor starts",
// "Billing goes live") has a verb of its own and keeps its tense rather than
// taking the past a simple past elsewhere gives the question; "that" before a
// subject or an auxiliary opens a clause of its own ("the feature that we
// built on Monday"); "be" behind a relative clause is the verb of the main
// clause. A clause without a verb still takes the question's past, also
// where a plural noun ends like a present ("check runs").
func TestRules_AnEnglishPresentGivesItsClauseATenseOfItsOwn(t *testing.T) {
	check517(t, now517, want517{
		"The cluster died on Saturday; the vendor comes on Tuesday.":                         "2026-11-21/past 2026-11-24/future",
		"The cluster died on Saturday and the vendor comes on Tuesday.":                      "2026-11-21/past 2026-11-24/future",
		"The cluster died on Saturday. The vendor comes on Tuesday.":                         "2026-11-21/past 2026-11-24/future",
		"The bug came in on Monday, the fix on Tuesday, and the release goes out on Friday.": "2026-11-16/past 2026-11-17/past 2026-11-27/future",
		"On Friday we demo the feature that we built on Monday.":                             "2026-11-27/future 2026-11-16/past",
		"On Thursday the team presents what it built on Monday.":                             "2026-11-26/future 2026-11-16/past",
		"Anna left on Monday; her successor starts on Friday.":                               "2026-11-16/past 2026-11-27/future",
		"Billing goes live on Friday; the old system died on Tuesday.":                       "2026-11-27/future 2026-11-17/past",
		"Will the service that froze on Tuesday be restarted on Thursday?":                   "2026-11-17/past 2026-11-26/future",
		"The tickets that have to be closed on Monday were filed on Friday?":                 "2026-11-23/future 2026-11-20/past",
		"Which PRs that need to be merged on Friday were opened on Monday?":                  "2026-11-27/future 2026-11-16/past",
		"What broke on Friday, and the alerts on Saturday?":                                  "2026-11-20/past 2026-11-21/past",
		"The patch that crashed prod on Saturday is scheduled for Friday again?":             "2026-11-21/past 2026-11-27/future",
		"The build broke on Monday; everyone meets on Friday.":                               "2026-11-16/past 2026-11-27/future",
		// counter-cases: a clause without a verb of its own
		"Builds were red on Thursday, check runs on Friday as well?":               "2026-11-19/past 2026-11-20/past",
		"Builds were red on Thursday, on Friday check runs as well?":               "2026-11-19/past 2026-11-20/past",
		"Builds were red on Thursday, on Friday check runs on staging too?":        "2026-11-19/past 2026-11-20/past",
		"The deploy failed on Monday, the rollback on Tuesday too?":                "2026-11-16/past 2026-11-17/past",
		"The deploy failed on Monday, the web analytics dashboard on Tuesday too?": "2026-11-16/past 2026-11-17/past",
		"We deployed on Friday, the test runs on Monday all green?":                "2026-11-20/past 2026-11-16/past",
		"Latency spiked on Friday, the error rates on Monday too?":                 "2026-11-20/past 2026-11-16/past",
	})
	// A form on -ed with "get" behind it is the participle of its noun, no
	// simple past that makes the question one: the clause of "get" keeps the
	// coming Monday the base read.
	q := "Tickets closed on Friday get reopened on Monday."
	if got := fmt517(NormalizeTemporalRules(q, now517)); !strings.Contains(got, "2026-11-23/future") {
		t.Errorf("%q: got %s, want the coming Monday 2026-11-23/future among its dates", q, got)
	}
}

// TestRules_TonightInANameNamesNoDay (issue #532, review round): "tonight"
// written with its capital inside a sentence before another word written so
// in its clause, or behind a word of a kind of work or group ("Band",
// "Show", "Album"), is a word of a name and names no day; at the start of a
// sentence, on its own, behind a capital noun alone and in a question that
// capitalizes every word it is tonight.
func TestRules_TonightInANameNamesNoDay(t *testing.T) {
	check517(t, now517, want517{
		"Wer spielt in der Band Tonight Alive?":        "nil",
		"Wer singt bei Tonight Alive?":                 "nil",
		"Who sings in the band Tonight Tonight?":       "nil",
		"Wann spielt Tonight Alive morgen?":            "2026-11-23/future",
		"Was lief am Montag in der Show Tonight Live?": "2026-11-16/past",
		"Who sings in the band Tonight?":               "nil",
		// counter-cases: tonight
		"What's on Tonight?":                                "2026-11-22/today",
		"Is Tonight Alive playing tonight?":                 "2026-11-22/today",
		"Who Is Playing Tonight?":                           "2026-11-22/today",
		"Tonight, who is on call?":                          "2026-11-22/today",
		"Ready? Tonight Anna deploys the fix.":              "2026-11-22/today",
		"Is the Release Tonight?":                           "2026-11-22/today",
		"Are you free Tonight, Anna?":                       "2026-11-22/today",
		"Meeting with Anna Tonight?":                        "2026-11-22/today",
		"Welche Songs sind auf dem Album Midnight Tonight?": "nil",
	})
}

// TestRules_AParticipleOfAnArrangementMakesNoPairOfReports (issue #532,
// review round): two participle clauses side by side are a Perfekt without
// its auxiliary only where neither participle names an arrangement or a
// state; "anberaumt", "festgelegt", "verabredet", "avisiert", "abgesperrt"
// and the like name the date of what is arranged, as the base read them.
func TestRules_AParticipleOfAnArrangementMakesNoPairOfReports(t *testing.T) {
	check517(t, now517, want517{
		"Am Freitag anberaumt, am Montag festgelegt?":    "2026-11-27/future 2026-11-23/future",
		"Am Montag verabredet, am Donnerstag avisiert?":  "2026-11-23/future 2026-11-26/future",
		"Am Freitag abgesperrt, am Samstag umgeleitet?":  "2026-11-27/future 2026-11-28/future",
		"Am Donnerstag eingetaktet, am Freitag fixiert?": "2026-11-26/future 2026-11-27/future",
		// counter-cases: two reports of acts
		"Am Montag gemerged, am Dienstag zurückgerollt – warum?":         "2026-11-16/past 2026-11-17/past",
		"Am Freitag gekauft, am Samstag zurückgeschickt?":                "2026-11-20/past 2026-11-21/past",
		"Am Freitag angefragt, am Montag beantwortet?":                   "2026-11-20/past 2026-11-16/past",
		"Am Freitag den Traffic umgeleitet, am Montag zurückgeschaltet?": "2026-11-20/past 2026-11-16/past",
	})
}

// TestRules_TheEndOfARelativeClauseAndTheHolidayOfToday (issue #532, review
// round) pins two readings the tests left open: an English relative clause
// ends at the auxiliary of its main clause two words or more behind its
// pronoun, not at its own auxiliary right behind it ("Which PRs that are due
// on Friday were merged on Monday?"); and a holiday asked on its own day in
// the past is that day, not the one a year before.
func TestRules_TheEndOfARelativeClauseAndTheHolidayOfToday(t *testing.T) {
	check517(t, now517, want517{
		"Which PRs that are due on Friday were merged on Monday?":  "2026-11-27/future 2026-11-16/past",
		"The tickets that are open on Friday were filed on Monday": "2026-11-27/future 2026-11-16/past",
	})
	newYear := time.Date(2027, 1, 1, 12, 0, 0, 0, time.UTC)
	check517(t, newYear, want517{
		"Was ist an Neujahr passiert?":     "2027-01-01/range",
		"What happened on New Year's Day?": "2027-01-01/range",
	})
}
