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

// TestRules_ADayKeywordWithAPartOfTheDay (issue #532, point 2): a word of a
// part of the day right behind a day keyword in its clause gives the
// keyword's day at the part's hour ("gestern Abend", "morgen früh",
// "yesterday evening"); "heute morgen" is this morning; "last night" and
// "letzte Nacht" without an article before them are yesterday's night,
// "tonight" today's; every weekday of the question stays a date, in the
// order of the question, and the weights are those of a day keyword.
func TestRules_ADayKeywordWithAPartOfTheDay(t *testing.T) {
	check517(t, now517, want517{
		"Was ist gestern Abend schiefgegangen?":                             "2026-11-21/past@19",
		"Wer hatte gestern Nacht Rufbereitschaft?":                          "2026-11-21/past@23",
		"Was ist heute Morgen passiert?":                                    "2026-11-22/today@8",
		"was haben wir heute morgen deployed?":                              "2026-11-22/today@8",
		"Was steht morgen früh an?":                                         "2026-11-23/future@8",
		"Wer übernimmt morgen Abend?":                                       "2026-11-23/future@19",
		"Was war vorgestern Nachmittag?":                                    "2026-11-20/past@15",
		"What broke yesterday evening?":                                     "2026-11-21/past@19",
		"What is planned for tomorrow morning?":                             "2026-11-23/future@8",
		"What happened last night?":                                         "2026-11-21/past@23",
		"Gab es letzte Nacht Alarme?":                                       "2026-11-21/past@23",
		"Anything on tonight?":                                              "2026-11-22/today@23",
		"Was ist heute Abend?":                                              "2026-11-22/today@19",
		"Was lief gestern Abend schief, nachdem am Freitag deployed wurde?": "2026-11-21/past@19 2026-11-20/past",
		"Was ist am Freitag passiert und was steht morgen früh an?":         "2026-11-20/past 2026-11-23/future@8",
		// counter-cases: no day keyword with a part of the day
		"Montag morgen ist Release, ist alles bereit?":    "2026-11-23/future@8",
		"Wir müssen morgen früher anfangen, oder?":        "2026-11-23/future",
		"Ist heute Abendessen im Büro?":                   "2026-11-22/today",
		"Die Last Nacht für Nacht steigt – warum?":        "nil",
		"Was planen wir für die letzte Nacht des Jahres?": "nil",
		"War der Build heute früh grün?":                  "2026-11-22/today@8",
		"Was steht heute an? Morgen ist Feiertag.":        "2026-11-22/today 2026-11-23/future",
		"Wer schreibt morgen den Abend-Bericht?":          "2026-11-23/future",
	})
	if r := NormalizeTemporalRules("Was ist gestern Abend schiefgegangen?", now517); r == nil || len(r.DimensionWeights) != 1 || r.DimensionWeights["linear"] != 1 {
		t.Errorf("a day keyword with a part of the day: weights %v, want linear 1", r)
	}
}

// TestRules_AnEnglishSimplePastBehindANoun (issue #532, point 3): a form on
// -ed behind a noun phrase without a pronoun that opens its clause ("The
// rollout stalled", "Mrs. Monday phoned", "Tuesday merged") or right before a
// date ("Which nodes rebooted Thursday?") is a simple past; the irregular
// past ("met", "sent", "froze") reads as one where a simple past stands; a
// subject pronoun behind a noun opens a relative clause without its pronoun
// ("the bug we had on Monday"). A form on -ed between the word that opens a
// noun phrase and its noun is an attribute ("the failed jobs"), a participle
// of an arrangement names the date arranged ("Deploy scheduled for Friday?"),
// and a question of a noun, the form and a date asks for a state as often
// ("Office closed on Monday?").
func TestRules_AnEnglishSimplePastBehindANoun(t *testing.T) {
	check517(t, now517, want517{
		"The rollout stalled on Thursday":                             "2026-11-19/past",
		"Disk usage spiked on Tuesday night":                          "2026-11-17/past@23",
		"Mrs. Monday phoned on Wednesday":                             "2026-11-18/past",
		"Which nodes rebooted Thursday?":                              "2026-11-19/past",
		"We met the auditors on Tuesday":                              "2026-11-17/past",
		"Who sent the report on Monday?":                              "2026-11-16/past",
		"The server froze on Saturday":                                "2026-11-21/past",
		"Let's talk on Friday about the bug we had on Monday":         "2026-11-27/future 2026-11-16/past",
		"Tuesday merged, Wednesday reverted – why?":                   "2026-11-17/past 2026-11-18/past",
		"The release slipped on Thursday and the new date is Tuesday": "2026-11-19/past 2026-11-24/future",
		// counter-cases
		"Can you rerun the failed jobs on Monday?": "2026-11-23/future",
		"Which updated docs are due on Thursday?":  "2026-11-26/future",
		"The deploy is scheduled for Friday":       "2026-11-27/future",
		"Deploy scheduled for Friday?":             "2026-11-27/future",
		"Office closed on Monday?":                 "2026-11-23/future",
		"Do we need a hotfix on Friday?":           "2026-11-27/future",
		"Who will read the report on Friday?":      "2026-11-27/future",
		"Is the left panel fixed on Monday?":       "2026-11-23/future",
		"Let's cut the release on Thursday":        "2026-11-26/future",
	})
}

// TestRules_TwoReportsOfParticiplesAreAPerfekt (issue #532, point 4, the
// ellipsis of participles): two clauses side by side, each a date with a
// participle at its end and no finite verb, are a Perfekt without its
// auxiliary where the pair asks for a reason or one of the two ends the
// question with a question mark; a participle of an arrangement or a state,
// and a question mark behind a clause of its own, leave the pair unread.
func TestRules_TwoReportsOfParticiplesAreAPerfekt(t *testing.T) {
	check517(t, now517, want517{
		"Am Montag gepatcht, am Dienstag wieder gecrasht – wieso?": "2026-11-16/past 2026-11-17/past",
		"Am Dienstag ausgerollt, am Mittwoch zurückgerollt?":       "2026-11-17/past 2026-11-18/past",
		"Montag migriert, Dienstag gelöscht – warum?":              "2026-11-16/past 2026-11-17/past",
		// counter-cases
		"Montag Büro geschlossen, Dienstag Homeoffice angeordnet":                    "2026-11-23/future 2026-11-24/future",
		"Am Freitag Server abgeschaltet, am Montag wieder hochgefahren – passt das?": "2026-11-27/future 2026-11-23/future",
		"Montag Deployment geplant, Dienstag Rollout vorgesehen":                     "2026-11-23/future 2026-11-24/future",
	})
}

// TestRules_ThePresentOfAStemOnT (issue #532, point 4): the present of the
// verbs whose stem ends in -t after a consonant and that no weak verb without
// that -t shares ("punkten", "belasten", "sichten") is no simple past; their
// simple past on -eten, and "rasten", the past of "rasen", stay pasts.
func TestRules_ThePresentOfAStemOnT(t *testing.T) {
	check517(t, now517, want517{
		"Welche Teams punkten am Freitag?":                  "2026-11-27/future",
		"Welche Jobs belasten am Montag die Datenbank?":     "2026-11-23/future",
		"Welche Kollegen sichten am Dienstag die Logs?":     "2026-11-24/future",
		"Welche Server crashten am Freitag?":                "2026-11-20/past",
		"Welche Jobs belasteten am Dienstag die Datenbank?": "2026-11-17/past",
		"Welche Autos rasten am Montag über den Hof?":       "2026-11-16/past",
	})
}

// TestRules_TheImperativeOfARequest (issue #532, point 4): a clause opened by
// the imperative of a verb of a request (imperatives) has a verb of its own
// that names no past, as "Lass uns" has, so that the question's past is the
// clause's that has it; "vom" before one of several weekdays names the day
// that has been, as before a single one; "hing" is a simple past. Adverbs
// before "mal" and a simple past at the start of a question are no
// imperatives.
func TestRules_TheImperativeOfARequest(t *testing.T) {
	check517(t, now517, want517{
		"Erinnere mich am Freitag an den Bug, den wir am Montag hatten": "2026-11-27/future 2026-11-16/past",
		"Schau dir am Montag an, was am Freitag kaputtging":             "2026-11-23/future 2026-11-20/past",
		"Frag ihn am Dienstag, was am Samstag los war":                  "2026-11-24/future 2026-11-21/past",
		"Teste am Donnerstag, ob der Fix vom Dienstag hält":             "2026-11-26/future 2026-11-17/past",
		"Starte am Dienstag den Job neu, der am Montag hing":            "2026-11-24/future 2026-11-16/past",
		// counter-cases
		"Wieder mal am Freitag, als alles kaputt war?":             "2026-11-20/past",
		"Lief am Montag mal was schief?":                           "2026-11-16/past",
		"Schon mal am Montag, wo der Build rot war, nachgeschaut?": "2026-11-16/past",
	})
}
