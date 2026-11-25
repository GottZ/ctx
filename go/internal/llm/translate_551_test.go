package llm

import (
	"fmt"
	"net/http"
	"net/http/httptest"
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

