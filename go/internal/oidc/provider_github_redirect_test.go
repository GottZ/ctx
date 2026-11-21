package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// #459: the GitHub userinfo calls carry the user's access token as a bearer
// token. net/http keeps Authorization on a redirect to the same host name on
// another port and to a subdomain. The calls follow a redirect only within
// the origin of the request (api.github.com, or the row's userinfo_url).

type ghSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newGHSink(t *testing.T, h http.HandlerFunc) *ghSink {
	t.Helper()
	s := &ghSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s authorization=%q", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization")))
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *ghSink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

func (s *ghSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// githubAPI answers /user and /user/emails like api.github.com.
func githubAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/user"):
		_ = json.NewEncoder(w).Encode(githubUser{ID: 459, Login: "octo"})
	case strings.HasSuffix(r.URL.Path, "/user/emails"):
		_ = json.NewEncoder(w).Encode([]githubEmail{{Email: "o@example.com", Verified: true, Primary: true}})
	default:
		http.NotFound(w, r)
	}
}

func TestGitHubUserinfoRedirectStaysOnOrigin(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			other := newGHSink(t, githubAPI)
			api := newGHSink(t, func(w http.ResponseWriter, r *http.Request) {
				// http.Redirect repeats the Location in the body of a GET.
				http.Redirect(w, r, other.srv.URL+r.URL.Path+"?"+leakMarker, code)
			})
			p := NewGitHub(GitHubConfig{APIBase: api.srv.URL, Client: &http.Client{}})
			_, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token-459"}, "")
			if n := other.count(); n != 0 {
				t.Fatalf("a %d redirect carried the userinfo call to another origin: %s", code, other.log())
			}
			if err == nil {
				t.Fatalf("a %d redirect to another origin must fail the identity lookup", code)
			}
			// The error reaches the log of a failed login: it names the
			// redirect, not its target.
			if s := err.Error(); !strings.Contains(s, "a redirect") || strings.Contains(s, leakMarker) {
				t.Errorf("error %q: want the redirect named, its body and target left out", err)
			}
		})
	}

	// Within the origin the redirect is followed with the token.
	var api *ghSink
	api = newGHSink(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v2") {
			http.Redirect(w, r, api.srv.URL+"/v2"+r.URL.Path, http.StatusMovedPermanently)
			return
		}
		if r.Header.Get("Authorization") != "Bearer gh-token-459" {
			http.Error(w, "no token", http.StatusUnauthorized)
			return
		}
		githubAPI(w, r)
	})
	p := NewGitHub(GitHubConfig{APIBase: api.srv.URL, Client: &http.Client{}})
	id, err := p.Identity(context.Background(), Tokens{AccessToken: "gh-token-459"}, "")
	if err != nil || id.Subject != "459" {
		t.Fatalf("same-origin redirect: identity %+v, %v; calls %s", id, err, api.log())
	}
}
