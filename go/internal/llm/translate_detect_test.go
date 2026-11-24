package llm

import (
	"math/rand/v2"
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

// TestNeedsTranslationQuotedTextIsMaterial: a quote is what the question asks
// about, not the language it is asked in. A German or Nordic question that
// quotes an English error message, mail subject or talk title is translated
// (the quoted English words are no counterweight), and an English question
// that quotes a German title stays English (the quoted German words are no
// evidence). Typographic marks count like the ASCII ones.
func TestNeedsTranslationQuotedTextIsMaterial(t *testing.T) {
	checkNeedsTranslation(t, true,
		`Was bedeutet der Fehler "the server is not responding"?`,
		`Warum kommt beim Deploy der Fehler "the container is not running"?`,
		`Wie behebe ich "permission denied: the file is not writable"?`,
		`Gibt es Neuigkeiten zum Thema "why the build is slow"?`,
		`Übersicht zu "what is new in the release"`,
		`Lösung für "the certificate has expired"`,
		`Kündigung: "your subscription has been cancelled" was nun?`,
		`Er hat gesagt "this is a breaking change", stimmt das?`,
		`Was hat der Reviewer mit "this is not what we want" gemeint?`,
		`Wann kam die Mail "your invoice is ready" von Hetzner?`,
		`Was steht im Log bei 'the job has been cancelled'?`,
		`Präsentation "How we scaled Postgres" fertig?`,
		`Folien für den Talk "Why your database is slow"`,
		"Was bedeutet der Fehler „the server is not responding“?",
		"Was bedeutet der Fehler „the server is not responding”?",
		"Was bedeutet der Fehler “the server is not responding”?",
		"Was heißt «the build is green» in der Pipeline?",
		"Was heißt »the build is green« in der Pipeline?",
		"Was bedeutet der Fehler « the server is not responding »?",
		"Wie übersetze ich ‚the best of both worlds‘ hier?",
		"Vad betyder felet ”the disk is full” på servern?",
		`Hvad betyder "the build is green" egentlig?`,
		`Was bedeutet "Schreibschutz"?`,
		`Was bedeutet der Fehler "the server is not responding "?`,
		`Was bedeutet der Fehler "the users' data is not in the table"?`,
	)
	checkNeedsTranslation(t, false,
		`Who composed "Der Ring des Nibelungen"?`,
		`What does "Wer rastet, der rostet, und das ist gut so" mean?`,
		"Who wrote „Im Westen nichts Neues“ and when?",
		"Who wrote 'Im Westen nichts Neues' and when?",
		`What does "Größe" mean in Jörg's notes?`,
		"What does 'Wie geht's dem Jörg und der Familie' mean?",
	)
}

// TestNeedsTranslationQuoteOnly: a question that carries no evidence outside
// its quotes is read from the quote — a pasted German question in quotes is
// translated, a pasted English one is not.
func TestNeedsTranslationQuoteOnly(t *testing.T) {
	checkNeedsTranslation(t, true,
		`"Wie richte ich das Backup ein?"`,
		"„Wie richte ich das Backup ein?“",
		`"Datenbank"`,
		`"vad är status för backupen"`,
		`was heißt "Zuständigkeit"`,
	)
	checkNeedsTranslation(t, false,
		`"what is new in the release"`,
		`"What was the die roll?"`,
		`what does "Zuständigkeit" mean`,
		`"Das Boot" movie review`,
	)
}

// TestNeedsTranslationApostrophesAndOpenQuotes: an apostrophe inside or at the
// end of a word (don't, it's, Jörg's, geht's, users') opens no quote, and a
// quote mark that is never closed skips nothing — the rest of the question is
// read as written.
func TestNeedsTranslationApostrophesAndOpenQuotes(t *testing.T) {
	checkNeedsTranslation(t, false,
		"Don't translate this, it's the user's question",
		"What's the status of Jörg's ticket?",
		"Where is the users' data and the admins' log?",
		`What is the "Größe`,
		"'Twas the night before the release",
		"Jörg's notes and the admins' Übersicht",
	)
	checkNeedsTranslation(t, true,
		"Wie geht's dem Jörg und was macht's Haus?",
		"Hans' Auto ist größer als Klaus' Rad",
		"'s Wetter morgen und wie wird es",
		`"Größe der Datei`,
		"„Größe der Datei",
		"'Größe der Datei",
	)
	for _, q := range []string{
		`Was bedeutet der Fehler "the server is not responding`,
		"Was bedeutet der Fehler „the server is not responding",
		"Was bedeutet der Fehler 'the server is not responding",
	} {
		ev, quoted := scanTranslateEvidence(q)
		if quoted != (translateEvidence{}) || ev.english != 3 {
			t.Errorf("scanTranslateEvidence(%q) = %+v, quoted %+v; want the open quote read as text (english 3) and nothing quoted", q, ev, quoted)
		}
	}
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

// TestNeedsTranslationNordicVerbFirstAndSharedWords: a Nordic yes/no
// question begins with its verb, capitalised, and may carry no other sign; a
// short one may consist of words English and German share. They are
// translated — the reason a few English words with a name such as Ålesund, or
// two shared words such as "var den", are translated too: a rule that left
// those as written would leave these as written as well.
func TestNeedsTranslationNordicVerbFirstAndSharedWords(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Går backupen i natt?",
		"Kører serveren stadig?",
		"Bør vi opgradere databasen?",
		"Står det i loggen?",
		"Får kunden refusjon?",
		"Har du den?",
		"Er den ny?",
		"Er der kaffe?",
		"Er hat recht",
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
		strings.Repeat(`"a `, 1<<18),
		`"` + strings.Repeat("what was the decision ", 40000),
	)
	checkNeedsTranslation(t, true,
		strings.Repeat("a", 100*1024)+"ä",
		strings.Repeat("what was the decision ", 2000)+strings.Repeat("über die Datenbank und das Backup ", 5000),
	)
}

// TestNeedsTranslationCodeAndWordEdges pins the edges of the word reader, one
// input each: a mention or hashtag and a token with a colon between letters
// are code; a decomposed å (a + combining ring above) is one of the letters;
// a two-letter word in capitals is an abbreviation (OG is no Danish "og"),
// and an abbreviation is looked up in no Nordic list; white space beyond
// ASCII (no-break space, ideographic space) separates tokens, so the path
// after it is skipped and the German words before it still count.
func TestNeedsTranslationCodeAndWordEdges(t *testing.T) {
	checkNeedsTranslation(t, false,
		"ping @jörg",
		"#größe",
		"user:jörg",
		"OG image size",
		"AV PA rental",
	)
	checkNeedsTranslation(t, true,
		"pa\u030askefrokost",
		"Prüfung\u00a0von\u00a0/etc/hosts",
		"Größe\u3000/tmp/x",
	)
}

// TestNeedsTranslationOpenBacktick: a backtick that is never closed skips
// nothing, and the rest is read as written.
func TestNeedsTranslationOpenBacktick(t *testing.T) {
	checkNeedsTranslation(t, true,
		"`unclosed Größe",
		"Wie geht das `ohne Ende",
		"Größe der Datei`",
	)
	checkNeedsTranslation(t, false, "`", "a`", "`a", "what is the `status")
}

// neverPanicInputs are inputs at the edges of the scanner: unpaired backticks
// and quote marks, cut UTF-8 sequences, NUL bytes, lone combining marks and
// long runs of one character.
func neverPanicInputs() []string {
	return []string{
		"", "`", "``", "```", "a`", "`a", "`\xc3", "\xc3`",
		`"`, `""`, `"a`, `a"`, `" "`, "„", "„a", "a“", "«", "»a«", "'", "''", "'a", "a'", "‚", "’",
		"\xc3", "Gr\xc3", "\xe2\x80", "„\xe2", "\x22\xc3\x22", "\xff\xfe", "'\xc3'", "\xe2\x80\x9e",
		"\x00", "a\x00b", "\x22\x00\x22", "`\x00`", "\u0308", "\u0308\u0308a", "\u030a",
		strings.Repeat("ä", 1<<16),
		strings.Repeat("`", 1<<16) + "a",
		strings.Repeat("\xc3", 1<<16),
		`"` + strings.Repeat("a ", 1<<16),
		strings.Repeat(`"a `, 1<<15),
		strings.Repeat("„a “", 1<<14),
		strings.Repeat("'s ", 1<<15),
	}
}

// TestNeedsTranslationNeverPanics: the detector runs on every query before
// retrieval, so a panic would fail the request. It returns on every edge
// input and on random mixes of the characters the scanner treats specially.
func TestNeedsTranslationNeverPanics(t *testing.T) {
	inputs := neverPanicInputs()
	alphabet := []string{"`", `"`, "'", "„", "“", "”", "«", "»", "‚", "‘", "’", " ", "\u00a0", "\t", "/", ":", ".", "@", "#",
		"a", "Z", "ä", "Å", "ø", "ß", "ẞ", "\u0308", "\u030a", "\x00", "\xc3", "\xe2\x80", "\xff", "the", "die", "och", "IT", "😀"}
	rng := rand.New(rand.NewPCG(210, 0))
	for range 20000 {
		var b strings.Builder
		for n := rng.IntN(24); n > 0; n-- {
			b.WriteString(alphabet[rng.IntN(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}
	for _, q := range inputs {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("NeedsTranslation(%.80q) panicked: %v", q, r)
				}
			}()
			NeedsTranslation(q)
		}()
	}
}

// FuzzNeedsTranslation: no input makes the detector panic.
func FuzzNeedsTranslation(f *testing.F) {
	for _, q := range neverPanicInputs() {
		if len(q) < 256 {
			f.Add(q)
		}
	}
	f.Fuzz(func(_ *testing.T, q string) {
		NeedsTranslation(q)
	})
}

// TestValidateTranslationDropsAnswersThatKeepCodeTokens: the code-token rule
// leaves a German question as written when its only evidence sits in a path,
// an address, a hashtag or a key:value token. The detector before #210 sent
// such questions to the model, but an answer that keeps the token fails
// validateTranslation and the original question went on to retrieval, so for
// those answers the rule saves the call and changes no result. An answer that
// drops the token passes.
func TestValidateTranslationDropsAnswersThatKeepCodeTokens(t *testing.T) {
	for _, c := range []struct{ original, answer string }{
		{"Größe/Gewicht Paket", "Size/weight package"},
		{"Prüfung/Abnahme Termin", "Inspection/acceptance date"},
		{"Datenbank/Konfiguration", "Database/configuration"},
		{"Hinweis:Datenbank", "Note:Database"},
		{"#Steuererklärung 2025", "#Tax return 2025"},
		{"Mail jörg@firma.de", "Mail jorg@firma.de"},
		{"Wo liegt /home/jörg/Übersicht.md", "Where is /home/jorg/Overview.md"},
	} {
		if validateTranslation(c.answer, c.original) {
			t.Errorf("validateTranslation(%q, %q) = true, want false", c.answer, c.original)
		}
	}
	if !validateTranslation("Size and weight package", "Größe/Gewicht Paket") {
		t.Error("an answer without the slash should pass")
	}
}
