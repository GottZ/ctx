package llm

import (
	"strings"
	"testing"
)

// The translation detector of the query path (#210). The step translates into
// English because the instance's corpus is English or German (the stance at
// handler/query.go), so: German and the Nordic languages are translated, a
// Nordic question whatever letters it is written with; English never is.

func checkNeedsTranslation(t *testing.T, want bool, queries ...string) {
	t.Helper()
	for _, q := range queries {
		if got := NeedsTranslation(q); got != want {
			t.Errorf("NeedsTranslation(%q) = %v, want %v", q, got, want)
		}
	}
}

// TestNeedsTranslationIssue210Table is the table of the issue comment of
// 2026-11-16 with the outcome the stance asks for. On eaa42622 (DetectGerman)
// three rows failed: the å-only Swedish question was not translated, and both
// English questions were (was + was, was + die).
func TestNeedsTranslationIssue210Table(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Vad är nästa steg för ctx?",
		"Vem skrev det här beslutet?",
		"Hur gick det med releasen i går?",
		"Vad sa Damien om forumet på lördag?",
	)
	checkNeedsTranslation(t, false,
		"What was the decision and who made it was unclear",
		"what was the die roll",
	)
}

// TestNeedsTranslationEnglishAmbiguousStopwords: was, die, den, hat, des and
// dem are English words too. In a question with English function words they
// are no German evidence (effect 2 of the issue: each of these paid a
// translation call).
func TestNeedsTranslationEnglishAmbiguousStopwords(t *testing.T) {
	checkNeedsTranslation(t, false,
		"Who was the den leader before Mark?",
		"Which Red Hat box in the den was the old mail server?",
		"Was DES ever used in the old site-to-site VPN?",
		"Did Des leave his hat in the den after the party?",
		"The DEM import was slow again, did the worker die?",
	)
}

// TestNeedsTranslationEnglishWithUmlautNames: a capitalised word with an
// umlaut (a name) is weak evidence; it does not outweigh the English words
// around it.
func TestNeedsTranslationEnglishWithUmlautNames(t *testing.T) {
	checkNeedsTranslation(t, false,
		"When is the call with Mr. Müller about the contract renewal?",
		"Which hotel did we book in Zürich for the conference?",
		"Is Dr. Köhler's practice open on Fridays?",
		"Is Jörg Müller in today?",
		"Are the Malmö servers still on the old kernel?",
	)
}

// TestNeedsTranslationEnglishQuotingGerman: a German title or saying inside an
// English question that does not outnumber the English around it.
func TestNeedsTranslationEnglishQuotingGerman(t *testing.T) {
	checkNeedsTranslation(t, false,
		`What does "Wer rastet, der rostet" mean?`,
		`Who directed "Das Boot"?`,
		`Where does the phrase "Das ist nicht mein Bier" come from?`,
		"Was Des at the meeting?",
	)
}

// TestNeedsTranslationGermanWithEnglishWords: German questions with an
// English word or phrase, or an abbreviation such as IT or US, stay German.
// The ambiguous stop words and capitalised nouns count half against the
// English words, an abbreviation not at all.
func TestNeedsTranslationGermanWithEnglishWords(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Was macht die IT?",
		"Wo stehen die US-Server?",
		"IT-Sicherheit Richtlinien",
		"Gibt es Änderungen in the schema?",
		"Wer hat Lord of the Rings geschrieben?",
		"Was ist the difference?",
		"Varför kraschar the worker?",
	)
}

// TestNeedsTranslationEnglishAbbreviationsAndNames: a Nordic word that is
// also an English word, abbreviation, name or keyword is weak evidence, and a
// word in capitals is looked up in no English or Nordic list.
func TestNeedsTranslationEnglishAbbreviationsAndNames(t *testing.T) {
	checkNeedsTranslation(t, false,
		"var vs let in JavaScript",
		"ER diagram tool",
		"PA system setup",
		"SOM clustering example",
		"Till Lindemann tour",
		"AV1 encoding settings",
		"med school notes",
		"till receipts",
		"MIG welding gas",
	)
}

// TestNeedsTranslationNordicWhateverTheSpelling (effect 1): the same question
// written with its letters, with å only, and in ASCII is translated each time.
// On eaa42622 only the spellings with ä or ö were.
func TestNeedsTranslationNordicWhateverTheSpelling(t *testing.T) {
	checkNeedsTranslation(t, true,
		// Swedish
		"Var finns anteckningarna från mötet i fredags?",
		"Vem tar hand om båten i sommar?",
		"Var finns anteckningarna fran motet i fredags?",
		"vad heter appen som skickar fakturorna",
		"Vem fixade buggen i inloggningen?",
		"påskefrokost",
		// Danish
		"Hvordan sætter jeg overvågning op for disken?",
		"Hvorfor ryger CPU-forbruget op på hundrede procent?",
		"Hvornaar er naeste release?",
		// Norwegian
		"Når kommer den nye laptopen, og hvem bestilte den?",
		"Hvor lang tid tar en full bygging?",
		// Finnish
		"Mikä oli viime perjantain katkon syy?",
		"Milloin on seuraava julkaisu?",
		"Kuinka usein tietokanta varmuuskopioidaan?",
	)
}

// TestNeedsTranslationGermanKept: what was translated before stays translated
// — umlauts, sharp s, capitalised umlaut nouns, two stop words (the ambiguous
// ones included), domain terms, and German around English terms and code.
func TestNeedsTranslationGermanKept(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Wie groß ist die Datenbank?",
		"Ärger",
		"Ärztehaus Öffnungszeiten",
		"Straße",
		"STRAẞE",
		"Pru\u0308fungsergebnis der Datei",
		"die ist gut",
		"Was hat die Firma gemacht?",
		"Welcher pfad ist korrekt?",
		"KONFIGURATION check",
		"ich bin der neue user",
		"Wie skaliert der Index bei vielen Tenants?",
		"Welche Tests prüfen `OrderService.Cancel`?",
		"Warum hängt `SELECT count(*) FROM events WHERE created_at > now()` so lange?",
		"Ist das Runbook für den Failover schon im Wiki?",
	)
	checkNeedsTranslation(t, false,
		"die hard movie",
		"What port does the service use?",
		"PostgreSQL runs on port 5432 with pgvector",
		"",
		"12345 67890",
	)
}

// TestNeedsTranslationCodeCarriesNoLanguage: a backtick span and a token with
// a path, identifier or URL character are skipped; a bare path is not a
// question in any language.
func TestNeedsTranslationCodeCarriesNoLanguage(t *testing.T) {
	checkNeedsTranslation(t, false,
		"/var/lib/postgresql/data",
		"/home/jörg/notes/übersicht.md",
		"`Größe`",
		"git log --oneline origin/main..HEAD",
		"feature/PAY-1432-refund-webhook",
	)
	checkNeedsTranslation(t, true, "Was steht in /var/log/nginx/error.log zum Upstream-Timeout?")
}

// TestNeedsTranslationOtherUmlautLanguages: Turkish and Hungarian questions
// with ö or ü are translated as before — the same reasoning as for the Nordic
// ones (the corpus is English or German).
func TestNeedsTranslationOtherUmlautLanguages(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Bir sonraki sürüm ne zaman çıkacak?",
		"Mi a különbség a teszt és az éles környezet között?",
	)
}

// TestNeedsTranslationLongInput: a long input is read to its end, an English
// one is not translated, and one word beyond the lower-case buffer still
// counts its letters.
func TestNeedsTranslationLongInput(t *testing.T) {
	checkNeedsTranslation(t, false,
		strings.Repeat("what was the decision about the backup schedule ", 20000),
		strings.Repeat("x", 1<<20),
	)
	checkNeedsTranslation(t, true,
		strings.Repeat("a", 100*1024)+"ä",
		strings.Repeat("what was the decision ", 2000)+strings.Repeat("über die Datenbank und das Backup ", 5000),
	)
}
