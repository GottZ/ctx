package handler

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// #459: the token request carries the authorization code, the PKCE verifier
// and — per token_auth — the client secret in the form body or as Basic
// auth. net/http re-sends that body on 307/308 to any host and keeps the
// Basic header on the same host name with another port. The token endpoint
// is the URL the discovery document or the provider row names; a redirect
// from it is not followed at all, on its own origin or elsewhere.

// tokenSink records every request that reaches it.
type tokenSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newTokenSink(t *testing.T, h http.HandlerFunc) *tokenSink {
	t.Helper()
	s := &tokenSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s authorization=%q body=%s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), b))
		s.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"access_token":"at","token_type":"Bearer"}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *tokenSink) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func (s *tokenSink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

func TestExchangeCodeRefusesRedirect(t *testing.T) {
	statuses := []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect}
	for _, auth := range []string{"client_secret_post", "client_secret_basic", "none"} {
		for _, code := range statuses {
			for _, where := range []string{"other-origin", "same-origin"} {
				t.Run(fmt.Sprintf("%s-%d-%s", auth, code, where), func(t *testing.T) {
					other := newTokenSink(t, nil)
					var idp *tokenSink
					idp = newTokenSink(t, func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path != "/token" {
							_, _ = io.WriteString(w, `{"access_token":"at","token_type":"Bearer"}`)
							return
						}
						target := other.srv.URL + "/token?leak-459"
						if where == "same-origin" {
							target = idp.srv.URL + "/moved-token?leak-459"
						}
						// The Location repeated in the body, as servers
						// commonly answer (http.Redirect writes none for POST).
						w.Header().Set("Location", target)
						w.WriteHeader(code)
						_, _ = fmt.Fprintf(w, `<a href="%s">moved</a>`, target)
					})
					h := &SSOHandler{client: &http.Client{Timeout: 5 * time.Second}}
					p := &store.OAuthProvider{Slug: "idp", Type: "oidc", ClientID: "client-459", TokenAuth: auth}
					_, err := h.exchangeCode(context.Background(), idp.srv.URL+"/token", "code-459", "https://ctx.example/auth/callback/idp", p, "secret-459", "verifier-459")
					if n := other.hits(); n != 0 {
						t.Fatalf("the token request reached another origin: %s", other.log())
					}
					if err == nil {
						t.Fatalf("a %d from the token endpoint must fail the exchange: %s", code, idp.log())
					}
					if n := idp.hits(); n != 1 {
						t.Fatalf("the token request was sent %d times (want once, no redirect followed): %s", n, idp.log())
					}
					if s := err.Error(); strings.Contains(s, "secret-459") || strings.Contains(s, "verifier-459") {
						t.Errorf("the error carries a credential: %v", err)
					}
					// The error reaches the server log: it names the
					// redirect, not its target.
					if s := err.Error(); !strings.Contains(s, "a redirect") || strings.Contains(s, "leak-459") {
						t.Errorf("error %q: want the redirect named, its body and target left out", err)
					}
				})
			}
		}
	}
}
