package llm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// The follow-ups of #210 collected in #551.

// TestNeedsTranslation551GermanFrames (point 1): a German question whose
// frame carries fewer than two of the stop words listed before #551 and no
// umlaut — a question word such as wann, wieso, woher, weshalb or wozu, a
// contraction such as beim, an adverb such as heute, a frequent verb form,
// or a noun ending in -ung, -heit, -keit or -schaft — is translated. On
// 89d9e70c each of these was sent as written.
func TestNeedsTranslation551GermanFrames(t *testing.T) {
	checkNeedsTranslation(t, true,
		// the issue's example
		`Woher kommt "the quick brown fox"?`,
		// a German question word is enough on its own
		"Wann kommt der Techniker",
		"Wieso Timeout",
		"weshalb so langsam",
		"Wozu dient eigentlich der Cronjob?",
		"Seit wann gibt es den Export?",
		`Wann erscheint "connection refused"?`,
		// stop words that were not on the list
		"Was passiert beim Neustart",
		"heute Abend noch deployen",
		"Hab ich schon gemacht",
		"Kommt drauf an, sagt der Kollege",
		"geht nicht mehr",
		`Fehler "permission denied" beim Login seit heute`,
		// a noun with a German ending
		"Heizung Wartung",
		"Rechnung Oktober",
		"Ummeldung Formular",
		"Erreichbarkeit Hotline",
		"Mitgliedschaft kuendigen",
		"Gesundheitsamt Bescheinigungen",
	)
}

// TestNeedsTranslation551EnglishControls (point 1): every word added to the
// German lists is checked against English. English questions with the words
// German and English share (in, was, war, die, fast, gift, not, bald, hat,
// arm, kind, rock, stern, bin, will, halt, oft, am, um, im) and English words
// with a German ending (Samsung, Fahrenheit, hamstrung) stay English.
func TestNeedsTranslation551EnglishControls(t *testing.T) {
	checkNeedsTranslation(t, false,
		"War Thunder update notes",
		"fast boot not working",
		"gift wrap bald spot",
		"rock band stern review",
		"kind words arm sling",
		"bin day halt",
		"oft quoted line",
		"am I late",
		"um ok sure",
		"im done here",
		"Samsung TV firmware update",
		"Fahrenheit to Celsius",
		"hamstrung budget plans",
		"Kaohsiung night market",
		"young people meeting",
		"was it fast",
		"Die cast car models",
	)
}

// germanListHomographs are words the German lists must not carry: each is a
// German function word that is also an English word form (en_US hunspell,
// all affix forms) or an English abbreviation or chat form. They were
// checked out of the list added in #551; the ambiguous German stop words
// (was die den hat des dem) count half and stay where they are.
var germanListHomographs = []string{
	"du", "es", "mich", "mir", "dir", "seine", "seiner", "allen", "am", "um", "im",
	"ins", "ans", "bis", "hinter", "ob", "falls", "halt", "eben", "oft", "nun",
	"nix", "darin", "bin", "war", "hast", "will", "mag", "weiss", "tun", "tut",
	"mal", "sei", "in", "fast", "gift", "not", "bald", "arm", "kind", "rock",
	"stern", "also", "so", "man", "fern", "hell", "rat", "rot",
}

// TestGermanListsCarryNoEnglishWord: none of the homographs is a strong
// German word — a German stop word, question word or domain term.
func TestGermanListsCarryNoEnglishWord(t *testing.T) {
	for _, w := range germanListHomographs {
		for name, set := range map[string]map[string]struct{}{
			"germanStopwords": germanStopwords, "germanQuestionWords": germanQuestionWords, "germanTechTerms": germanTechTerms,
		} {
			if _, ok := set[w]; ok {
				t.Errorf("%s carries %q, an English word", name, w)
			}
		}
		if germanNounEnding([]byte(w), utf8.RuneCountInString(w)) {
			t.Errorf("%q reads as a German noun ending", w)
		}
	}
}

// TestValidateTranslation551Letters (point 2): a translation that keeps a
// letter beyond ASCII — a place or person name, an accent, a decomposed
// umlaut — passes. On 89d9e70c each was dropped and the original question
// went on to retrieval.
func TestValidateTranslation551Letters(t *testing.T) {
	const original = "eine Frage, die lang genug für jede Antwort hier ist"
	for _, answer := range []string{
		"Trip planning in Ålesund",
		"Call with Jürgen Müller about the contract",
		"Søren Kierkegaard quotes",
		"Ærø ferry times",
		"café opening hours",
		"Mu\u0308ller notes",
		"Straße names",
		"Δ of the release",
	} {
		if !validateTranslation(answer, original) {
			t.Errorf("validateTranslation(%q) = false, want true", answer)
		}
	}
}

// TestValidateTranslation551StillRejected (point 2): letters are let through,
// nothing else that was refused before — one case per character class.
func TestValidateTranslation551StillRejected(t *testing.T) {
	const original = "eine Frage, die lang genug für jede Antwort hier ist"
	for _, c := range []struct{ class, answer string }{
		{"Cc NUL", "backup\x00status"},
		{"Cc ESC", "backup\x1b[31mstatus"},
		{"Cc DEL", "backup\x7fstatus"},
		{"Cc C1 NEL", "backup\u0085status"},
		{"newline", "backup\nstatus"},
		{"Cf zero-width space", "back\u200bup"},
		{"Cf right-to-left override", "backup \u202estatus"},
		{"Cf isolate", "backup \u2066status\u2069"},
		{"Cf byte order mark", "\ufeffbackup status"},
		{"Cf soft hyphen", "back\u00adup"},
		{"Zl line separator", "backup\u2028status"},
		{"Zp paragraph separator", "backup\u2029status"},
		{"Zs no-break space", "backup\u00a0status"},
		{"Zs ideographic space", "backup\u3000status"},
		{"Co private use", "backup \ue000"},
		{"invalid UTF-8", "backup \xff status"},
		{"Sm plus", "backup + status"},
		{"Sm less-than", "backup < status"},
		{"Sm plus-minus", "backup ± status"},
		{"Sc dollar", "backup $status"},
		{"Sc euro", "backup costs 5 €"},
		{"Sk circumflex", "backup^status"},
		{"Sk backtick", "backup `status`"},
		{"So emoji", "backup status 😀"},
		{"So copyright", "backup © status"},
		{"Pc underscore", "backup_status"},
		{"Pd en dash", "backup – status"},
		{"Ps/Pe brackets", "backup [status]"},
		{"Ps/Pe braces", "backup {status}"},
		{"Pi/Pf guillemets", "backup «status»"},
		{"Pi/Pf curly quotes", "backup “status”"},
		{"Po colon", "Translation: backup status"},
		{"Po semicolon", "backup; status"},
		{"Po slash", "backup/status"},
		{"Po at", "backup@status"},
		{"Po hash", "#backup status"},
		{"Po asterisk", "**backup** status"},
		{"Po ampersand", "backup & status"},
		{"Po percent", "backup 100%"},
		{"Po apostrophe", "the backup's status"},
		{"Po double quote", `the "backup" status`},
		{"Po backslash", `backup\status`},
		{"Po pipe", "backup | status"},
		{"Nd beyond ASCII", "backup ٣"},
		{"Nl roman numeral", "backup Ⅻ"},
		{"No superscript", "backup²"},
	} {
		if validateTranslation(c.answer, original) {
			t.Errorf("%s: validateTranslation(%q) = true, want false", c.class, c.answer)
		}
	}
}

// translateAnswer runs TranslateQuery against a stub backend that answers
// with content, the production path: strip, then validate.
func translateAnswer(t *testing.T, query, content string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprintf(w, `{"message":{"role":"assistant","content":%q},"eval_count":1,"prompt_eval_count":1}`, content)
	}))
	t.Cleanup(srv.Close)
	b := backends.Backend{Host: srv.URL, Protocol: backends.ProtocolOllama, Model: "m"}
	got, err := TranslateQuery(testPrincipalCtx(), nil, translatePool(b), nil, "t", backends.SensPersonal, query, "", testAdmission(t, dispatch.ClassInteractive))
	if err != nil {
		t.Fatalf("TranslateQuery(%q): %v", query, err)
	}
	return got
}

// TestTranslateQuery551KeepsNames (point 2): through TranslateQuery, the
// answer that keeps a Nordic or German name replaces the question.
func TestTranslateQuery551KeepsNames(t *testing.T) {
	for _, c := range []struct{ query, answer string }{
		{"Reiseplanung nach Ålesund", "Trip planning to Ålesund"},
		{"Hvornår kommer Søren?", "When is Søren coming?"},
		{"Wann ist der Termin mit Herrn Müller?", "When is the appointment with Mr. Müller?"},
	} {
		if got := translateAnswer(t, c.query, c.answer); got != c.answer {
			t.Errorf("TranslateQuery(%q) with answer %q = %q, want the answer", c.query, c.answer, got)
		}
	}
}

// TestNeedsTranslation551SingleQuotePossessive (point 4): inside a single
// quote, an apostrophe after s, x, z or ß followed by more text before the
// next closing mark is a possessive, not the end of the quote. On 89d9e70c
// the span closed at users', the rest counted as English outside the quote,
// and the German question was not translated.
func TestNeedsTranslation551SingleQuotePossessive(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Was bedeutet der Fehler 'the users' data is not in the table'?",
		"Was bedeutet der Fehler ‘the users’ data is not in the table’?",
		"Was heißt 'the class' fields are not in the index' hier?",
	)
	checkNeedsTranslation(t, false,
		"What does 'Wer rastet, der rostet' mean?",
		"Who wrote 'Im Westen nichts Neues' and the users' guide?",
	)
	for _, q := range []string{
		"Was bedeutet der Fehler 'the users' data is not in the table'?",
		"Wie behebe ich 'the users' data is not in the table'",
	} {
		if ev, _ := scanTranslateEvidence(q); ev.english != 0 {
			t.Errorf("scanTranslateEvidence(%q): english %d outside the quote, want 0", q, ev.english)
		}
	}
	// A quote ending in s closes where the next quote opens.
	if ev, _ := scanTranslateEvidence("Was heißt 'process' und 'thread' in the docs?"); ev.english != 1 {
		t.Errorf("two quotes: evidence %+v outside the quotes, want english 1 (the)", ev)
	}
}

// TestNeedsTranslation551QuoteInsideCodeToken (point 4): a quote mark inside
// a token that already reads as code opens no quote, so the token stays code
// as a whole. On 89d9e70c the quote split config["größe"] before the
// code-token rule saw it, and größe counted as quoted evidence.
func TestNeedsTranslation551QuoteInsideCodeToken(t *testing.T) {
	checkNeedsTranslation(t, false,
		`config["größe"]`,
		`map[string]int{"größe": 1}`,
		`row['übersicht']`,
		`key="Größe"`,
		`=SUMME(„Größe“)`,
	)
	checkNeedsTranslation(t, true,
		`Was bedeutet "Größe"?`,
		`("Größe der Datei")`,
		`Wie lese ich config["größe"] aus?`,
	)
}

// TestNeedsTranslation551QuoteReaderLinear (point 4): the possessive
// look-ahead and the code read in front of a quote mark keep the walk linear
// — about 1 MB each of possessive-shaped marks, quote marks inside a code
// token, backtick spans between quotes, and quote marks after a dot.
func TestNeedsTranslation551QuoteReaderLinear(t *testing.T) {
	checkNeedsTranslation(t, false,
		"'"+strings.Repeat("users' ", 1<<17),
		strings.Repeat("'a s' ", 1<<17),
		"x["+strings.Repeat(`"a"`, 1<<18),
		"x="+strings.Repeat(`"`, 1<<20),
		strings.Repeat("`a`\"b\"", 1<<17),
		strings.Repeat("a.\"", 1<<18),
		strings.Repeat("x['", 1<<18),
	)
}

// TestNeedsTranslation551MoreQuoteMarks (point 4): an acute accent (´…´), a
// fullwidth quotation mark (＂…＂) and a double prime (″…″) are quote marks.
// On 89d9e70c each was read as text.
func TestNeedsTranslation551MoreQuoteMarks(t *testing.T) {
	checkNeedsTranslation(t, false,
		"What does ´Das ist nicht mein Bier´ mean?",
		"What does ＂Das ist nicht mein Bier＂ mean?",
		"What does ″Das ist nicht mein Bier″ mean?",
		// an inch or second mark after a digit opens nothing
		"Is the 27″ screen in the box with the 24″ one?",
	)
	checkNeedsTranslation(t, true,
		"Was bedeutet der Fehler ´the server is not responding´?",
		"Was bedeutet der Fehler ＂the server is not responding＂?",
		"Was bedeutet der Fehler ″the server is not responding″?",
	)
}

// TestNeedsTranslation551WordSlash (point 6): a single slash between two
// capitalised words is how German writes two nouns as alternatives
// (Größe/Gewicht) — words, not a path. On 89d9e70c the token was code and
// the question was sent as written. Paths stay code: lower-case segments, a
// leading slash, more than one slash, a dot or another code character.
func TestNeedsTranslation551WordSlash(t *testing.T) {
	checkNeedsTranslation(t, true,
		"Größe/Gewicht Paket",
		"Prüfung/Abnahme Termin",
		"Datenbank/Konfiguration",
	)
	checkNeedsTranslation(t, false,
		"docs/übersicht",
		"/Größe/Gewicht",
		"Größe/Gewicht/Höhe",
		"Größe/gewicht",
		"Übersicht/Datei.md",
		"Größe/Gewicht_neu",
		"Ä/",
		"/Ä",
		strings.Repeat("Größe/", 1<<17)+"Gewicht",
	)
	checkNeedsTranslation(t, true, strings.Repeat("Größe/Gewicht ", 1<<16))
}

// TestTranslateQuery551WordSlashAnswers (point 6): what the step returns for
// a word-slash question, by the answer's form. An answer that keeps the
// slash fails validateTranslation and the question goes on as written (as
// before #210); an answer that rewords it replaces the question.
func TestTranslateQuery551WordSlashAnswers(t *testing.T) {
	const q = "Größe/Gewicht Paket"
	for _, c := range []struct {
		answer, want string
	}{
		{"Size/weight package", q},
		{"Size / weight package", q},
		{"Size/Weight Package", q},
		{"Size and weight package", "Size and weight package"},
		{"Size or weight package", "Size or weight package"},
		{"package size and weight", "package size and weight"},
	} {
		if got := translateAnswer(t, q, c.answer); got != c.want {
			t.Errorf("answer %q: TranslateQuery = %q, want %q", c.answer, got, c.want)
		}
	}
}
