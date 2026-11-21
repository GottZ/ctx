package goldset

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// #459 point 4: Ask binds the origin rule per call so that a ChatClient
// assembled as a literal holds it too; a literal without HTTP took
// clientkey.Do a nil client and panicked. It sends through http.DefaultClient's
// settings instead, under the same rule.
func TestChatClientLiteralWithoutHTTP(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, goldsetAnswer)
	}))
	t.Cleanup(srv.Close)
	c := &ChatClient{URL: srv.URL + chatPath, Model: "m"}
	var ans string
	var err error
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Ask on a literal without HTTP panicked: %v", r)
			}
		}()
		ans, err = c.Ask(context.Background(), "s", goldsetPrompt, 8, 0)
	}()
	if err != nil || ans == "" {
		t.Fatalf("Ask = %q, %v; want the answer", ans, err)
	}

	// The rule holds for it: a redirect to another origin is refused.
	var otherSeen int
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		otherSeen++
		_, _ = io.WriteString(w, goldsetAnswer)
	}))
	t.Cleanup(other.Close)
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(home.Close)
	lit := &ChatClient{URL: home.URL + chatPath, Model: "m"}
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("Ask on a literal without HTTP panicked: %v", r)
			}
		}()
		_, err = lit.Ask(context.Background(), "s", goldsetPrompt, 8, 0)
	}()
	if err == nil || otherSeen != 0 {
		t.Fatalf("literal without HTTP: err %v, other origin saw %d requests; want a refusal and none", err, otherSeen)
	}
}
