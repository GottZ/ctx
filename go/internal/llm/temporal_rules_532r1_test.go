package llm

import "testing"

// The tests below pin the review round of issue #532: forms a fresh review
// of the wave found read wrong where 5a28c475 read them right. Asked on
// Sunday 2026-11-22 (now517) unless a test names another day.

// TestRules_AHomographOfAnEnglishPastIsReadByItsPlace (issue #532, review
// round): a simple past that is another word as well — a name ("Drew"), a
// noun ("stole", "drove", "spoke", "saw"), an adjective ("woke"), a
// quantified noun ("Any thought") — is a past only where a simple past
// stands, behind its subject, and a word written with its capital inside
// its clause is a name and no verb. "fell" is the present of "to fell" as
// well and names no past at all.
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
		"Lost and found opens on Saturday?":          "2026-11-28/future",
		"Is the saw in the workshop free on Monday?": "2026-11-23/future",
		// counter-cases: the same forms where a simple past stands
		"The architect drew the topology on Monday, where is it?": "2026-11-16/past",
		"Who spoke with legal on Thursday?":                       "2026-11-19/past",
		"Somebody stole the disk on Saturday?":                    "2026-11-21/past",
		"The pager woke everyone on Thursday, why?":               "2026-11-19/past",
		"The pager woke Anna on Thursday.":                        "2026-11-19/past",
		"Who found the root cause on Monday?":                     "2026-11-16/past",
		"Who saw the alert on Saturday?":                          "2026-11-21/past",
		"We chose the new region on Friday, right?":               "2026-11-20/past",
	})
}

// TestRules_AnImperativeOpensItsClauseOnlyWhereItCanBeOne (issue #532,
// review round): the German imperatives of a request give their clause a
// verb of its own only where they can be one — not in an English sentence
// ("such", "sage", "check" are English words), not written with their
// capital inside a sentence (a German noun: "Suche", "Check"), not in "sage
// und schreibe", not where another verb of the clause is the finite one; an
// English "check" or "bring" at the start of the question or before its
// object is one.
func TestRules_AnImperativeOpensItsClauseOnlyWhereItCanBeOne(t *testing.T) {
	check517(t, now517, want517{
		"We broke prod on Friday, such a mess on Monday too.":                 "2026-11-20/past 2026-11-16/past",
		"The build broke on Friday, check on Monday too?":                     "2026-11-20/past 2026-11-16/past",
		"Prod went down on Friday, sage advice on Monday?":                    "2026-11-20/past 2026-11-16/past",
		"Am Freitag ging alles schief, Suche am Montag auch kaputt?":          "2026-11-20/past 2026-11-16/past",
		"Am Freitag gab es Ärger, sage und schreibe sieben Alerts am Montag?": "2026-11-20/past 2026-11-16/past",
		"Check am Montag ergab nichts – was lief am Freitag schief?":          "2026-11-16/past 2026-11-20/past",
		// counter-cases: an imperative where it can be one
		"Schau dir am Freitag an, was am Montag schiefging.":              "2026-11-27/future 2026-11-16/past",
		"Suche am Montag die Mail heraus, die am Freitag kam.":            "2026-11-23/future 2026-11-20/past",
		"Der Build brach am Montag, schau dir das am Donnerstag an.":      "2026-11-16/past 2026-11-26/future",
		"The build broke on Monday, check the logs on Thursday.":          "2026-11-16/past 2026-11-26/future",
		"Check on Friday whether the job that failed on Monday was rerun": "2026-11-27/future 2026-11-16/past",
	})
}
