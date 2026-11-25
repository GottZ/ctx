package llm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
)

// The follow-ups of #210 collected in #551.

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

