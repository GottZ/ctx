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
		"Absturz beim Neustart der Dienste",
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
		"Mitgliedschaft Verein",
		"Rechnung bezahlen bitte",
		"Rechnung an Vermieter",
		"Kraftfahrzeughaftpflichtversicherung Vergleich",
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
		// a domain term with a noun ending counts once
		"the Einstellung tab",
		// a capitalised word with a noun ending beside unlisted lower-case
		// words and no other sign: a name or a loanword
		"Achtung Baby album review",
		"Tanjung Priok port congestion",
		"Nibelungen saga summary",
		"Gleichschaltung history",
		"Keelung port schedule",
		"sprung floor tiles",
		`invoice "Rechnung"`,
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
		if germanNounEnding([]byte(w), w, utf8.RuneCountInString(w)) {
			t.Errorf("%q reads as a German noun ending", w)
		}
	}
}

// TestValidateTranslation551Letters (point 2): a translation that keeps a
// letter beyond ASCII the question wrote — a place or person name, an accent
// — passes. On 89d9e70c each was dropped and the original question went on
// to retrieval.
func TestValidateTranslation551Letters(t *testing.T) {
	for _, c := range []struct{ original, answer string }{
		{"Reiseplanung für Ålesund im Sommer", "Trip planning for Ålesund in summer"},
		{"Gespräch mit Jürgen Müller über den Vertrag", "Call with Jürgen Müller about the contract"},
		{"Zitate von Søren Kierkegaard gesucht", "Søren Kierkegaard quotes"},
		{"Wann fährt die Fähre nach Ærø?", "When does the ferry to Ærø leave?"},
		{"Öffnungszeiten vom Café am Markt", "café opening hours at the market"},
		{"Notizen zum Treffen mit MÜLLER gestern", "Notes on the meeting with Müller yesterday"},
		{"Wie heißt die Straße beim Bahnhof?", "What is the Straße at the station called?"},
		{"Was ist das Δ zum letzten Release?", "What is the Δ to the last release?"},
	} {
		if !validateTranslation(c.answer, c.original) {
			t.Errorf("validateTranslation(%q, %q) = false, want true", c.answer, c.original)
		}
	}
}

// TestValidateTranslation551OwnLetters (review of PR #556): a letter or mark
// beyond ASCII that the question did not write is not a name the answer
// kept. An answer in another script, a look-alike letter inside an ASCII
// word, a pile of combining marks or a decomposed spelling of the question's
// own letter leaves the question as it was — what the ASCII-only pattern did
// for each of them before #551.
func TestValidateTranslation551OwnLetters(t *testing.T) {
	const original = "Wie setze ich das Passwort für Müller zurück?"
	for _, c := range []struct{ class, answer string }{
		{"a sentence in Cyrillic", "Как сбросить пароль"},
		{"a sentence in Chinese", "如何重置密码"},
		{"a look-alike letter in an ASCII word", "How do I reset the p\u0430ssword"},
		{"an accent the question did not write", "How do I reset the pässword for Mueller"},
		{"a combining mark the question did not write", "How do I reset the passwo\u0301rd"},
		{"the question's letter in a decomposed spelling", "Password reset for Mu\u0308ller"},
		{"a name the question did not write", "Password reset for Ålesund"},
	} {
		if validateTranslation(c.answer, original) {
			t.Errorf("%s: validateTranslation(%q) = true, want false", c.class, c.answer)
		}
	}
	if !validateTranslation("How do I reset the password for Müller?", original) {
		t.Errorf("the question's own letter in the answer is refused")
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
		{"variation selector", "café\ufe0f menu"},
		{"keycap sequence", "1\ufe0f\u20e3 backup"},
		{"Mongolian free variation selector", "back\u180bup"},
		{"combining grapheme joiner", "back\u034fup"},
		{"Hangul filler", "backup\u3164status"},
		{"Khmer inherent vowel", "back\u17b4up"},
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
		"Was bedeutet der Fehler 'the box' lid is not on the table'?",
		"Was heißt 'the users' rights are not in the groups'?",
	)
	checkNeedsTranslation(t, false,
		"What does 'Wer rastet, der rostet' mean?",
		"Who wrote 'Im Westen nichts Neues' and the users' guide?",
		// a double quote mark is never a possessive
		`Was bedeutet "the users" data is not in the table"?`,
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
	if ev, _ := scanTranslateEvidence("Was heißt 'process' und 'thread' in the docs?"); ev.english != 1 || ev.strong != 2 {
		t.Errorf("two quotes: evidence %+v outside the quotes, want english 1 (the) and strong 2 (heißt, und)", ev)
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
		`obj.attr("Größe")`,
		`src/"größe"`,
	)
	checkNeedsTranslation(t, true,
		`Was bedeutet "Größe"?`,
		`("Größe der Datei")`,
		`Wie lese ich config["größe"] aus?`,
		// the next token is read on its own
		`Wieso config["x"] "the file is not there"`,
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

// TestNeedsTranslation551SlashStaysCode (point 6): a slash between two
// capitalised words stays code. A German keyword query such as
// Größe/Gewicht Paket is sent as written; reading such a token as words
// would also translate a capitalised path and would read an English pair a
// German question asks about as English (measured by the review of #551).
func TestNeedsTranslation551SlashStaysCode(t *testing.T) {
	checkNeedsTranslation(t, false,
		"Größe/Gewicht Paket",
		"cd Dokumente/Übersicht",
		"git add Dokumente/Prüfung",
		"docs/übersicht",
		strings.Repeat("Größe/Gewicht ", 1<<16),
	)
	checkNeedsTranslation(t, true,
		"Wie benutze ich Has/Have richtig?",
		"Wie schreibe ich When/Where korrekt",
	)
}

// TestTranslateQuery551SlashAnswers (point 6): what the step would return
// for a slash question, by the answer's form — the measurement behind the
// decision above. An answer that keeps the slash fails validateTranslation
// and the question goes on as written, so the call buys something only when
// the model rewords the slash.
func TestTranslateQuery551SlashAnswers(t *testing.T) {
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

// TestNeedsTranslation556Review pins what the review of PR #556 found
// unpinned or wrong.
func TestNeedsTranslation556Review(t *testing.T) {
	for _, c := range []struct {
		query string
		want  bool
	}{
		// An unlisted lower-case word before an English quote: the quote is
		// read with its English function words, as on 89d9e70c.
		{`translate "what is the Zugriff policy?"`, false},
		{`explain "where is the Müller office"`, false},
		{`quote "the best café in Malmö"`, false},
		{`check "is the Müller account active"`, false},
		{`translate "jeg er sulten"`, true},
		{`invoice "Rechnung"`, false},
		// English words and names on -ung the ending rule read as German.
		{"unsprung mass reduction", false},
		{"underslung trailer axle kit", false},
		{"highstrung horse training", false},
		{"Keelung harbour tour", false},
		// The veto of a capitalised noun ending needs a query without any
		// stop word: one weak stop word beside it keeps the ending's signal.
		{"Rechnung den kunden schicken", true},
		// ß ends a word a possessive mark may follow, inside single quotes.
		{"Was bedeutet der Satz 'the Strauß' waltz is not in the list of the works'?", true},
	} {
		if got := NeedsTranslation(c.query); got != c.want {
			t.Errorf("NeedsTranslation(%q) = %v, want %v", c.query, got, c.want)
		}
	}
}
