package handler

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// #459: backend-test's own GETs against an operator (_global) row carry the
// row's key — x-api-key plus extra_headers on an Anthropic row, a bearer
// token otherwise — and went through http.DefaultClient, which copies
// x-api-key and every extra header to any redirect target and keeps
// Authorization on the same host name with another port. They follow a
// redirect only within the origin of the row's base_url now, the rule of the
// row's own chat wire; tenant rows already did (guarded pool client).

type probeSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newProbeSink(t *testing.T, h http.HandlerFunc) *probeSink {
	t.Helper()
	s := &probeSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s authorization=%q x-api-key=%q x-row-secret=%q",
			r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Api-Key"), r.Header.Get("X-Row-Secret")))
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *probeSink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

func (s *probeSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func TestBackendTestProbeRedirectStaysOnOrigin(t *testing.T) {
	ok := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"data":{}}`)) }
	rows := map[string]func(host string) *backends.Backend{
		"generic": func(host string) *backends.Backend {
			return &backends.Backend{Name: "g", Host: host, Scope: backends.GlobalScope, Protocol: backends.ProtocolOpenAI,
				APIKey: "row-key-459", APIKeyRef: "row-key"}
		},
		"anthropic": func(host string) *backends.Backend {
			return &backends.Backend{Name: "a", Host: host, Scope: backends.GlobalScope, Protocol: backends.ProtocolAnthropic,
				APIKey: "row-key-459", APIKeyRef: "row-key", ExtraHeaders: map[string]string{"X-Row-Secret": "extra-459"}}
		},
		"openrouter": func(host string) *backends.Backend {
			return &backends.Backend{Name: "o", Host: host, Scope: backends.GlobalScope, Protocol: backends.ProtocolOpenAI,
				ProviderClass: backends.ProviderOpenRouter, APIKey: "row-key-459", APIKeyRef: "row-key"}
		},
	}
	for name, row := range rows {
		for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
			t.Run(fmt.Sprintf("%s-%d", name, code), func(t *testing.T) {
				other := newProbeSink(t, ok)
				api := newProbeSink(t, func(w http.ResponseWriter, r *http.Request) {
					http.Redirect(w, r, other.srv.URL+r.URL.Path, code)
				})
				b := row(api.srv.URL)
				checks := map[string]string{}
				reachable := probeBackendURL(context.Background(), b, checks)
				if n := other.count(); n != 0 {
					t.Fatalf("the probe followed a %d to another origin: %s", code, other.log())
				}
				// The host answered: reachable, as a tenant row's probe
				// always read a refused redirect.
				if !reachable || checks["base_url"] != "ok" {
					t.Errorf("a refused redirect: reachable %v, checks %v; want the host's answer read as reachable", reachable, checks)
				}
				// The key never reached a host that judged it: auth is not
				// ok, and the redirect is named — a call on this path meets
				// the same redirect.
				if !strings.HasPrefix(checks["auth"], "unverified") {
					t.Errorf("a refused redirect reported auth %q; want unverified (checks %v)", checks["auth"], checks)
				}
				if !strings.Contains(checks["redirect"], fmt.Sprint(code)) {
					t.Errorf("a refused redirect: redirect check %q, want its status named (checks %v)", checks["redirect"], checks)
				}
				if b.ProviderClass == backends.ProviderOpenRouter {
					_ = openRouterDetails(context.Background(), b)
					if n := other.count(); n != 0 {
						t.Fatalf("the OpenRouter detail GET followed a %d to another origin: %s", code, other.log())
					}
				}
			})
		}
	}

	// Within the origin the probe follows, as the chat wire does.
	var api *probeSink
	api = newProbeSink(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api") {
			http.Redirect(w, r, api.srv.URL+"/api"+r.URL.Path, http.StatusFound)
			return
		}
		ok(w, r)
	})
	b := rows["anthropic"](api.srv.URL)
	checks := map[string]string{}
	if !probeBackendURL(context.Background(), b, checks) || checks["base_url"] != "ok" || checks["auth"] != "ok" {
		t.Fatalf("same-origin redirect: checks %v; calls %s", checks, api.log())
	}
	if _, ok := checks["redirect"]; ok {
		t.Errorf("a followed redirect is reported as not followed: %v", checks)
	}
}
