package backends

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// #459: endpoint discovery of an operator row sends the row's key as a
// bearer token and its extra_headers — the request a chat call of the row
// carries. net/http copies the extra headers to every redirect target and
// keeps Authorization on the same host name with another port. The operator
// client follows a redirect only within the origin of the row's base_url, the
// rule the row's chat wire follows (httpx.SameOriginRedirect); tenant rows
// already went through the guarded pool client with that rule.

type discoverySink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newDiscoverySink(t *testing.T, h http.HandlerFunc) *discoverySink {
	t.Helper()
	s := &discoverySink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s authorization=%q x-row-secret=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"), r.Header.Get("X-Row-Secret")))
		s.mu.Unlock()
		h(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *discoverySink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

func (s *discoverySink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func TestEndpointDiscoveryRedirectStaysOnOrigin(t *testing.T) {
	serve := func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(endpointsBody)) }
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			other := newDiscoverySink(t, serve)
			api := newDiscoverySink(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.srv.URL+r.URL.Path, code)
			})
			b := discoveryBackend(api.srv.URL)
			b.Scope = GlobalScope
			b.APIKey = "row-key-459"
			b.ExtraHeaders = map[string]string{"X-Row-Secret": "extra-459"}
			_, err := NewEndpointCache().fetch(context.Background(), b, "qwen/qwen3.6-27b")
			if n := other.count(); n != 0 {
				t.Fatalf("a %d redirect carried the discovery GET to another origin: %s", code, other.log())
			}
			if err == nil {
				t.Fatalf("a %d redirect to another origin must end the discovery with an error", code)
			}
		})
	}

	// Within the origin the redirect is followed.
	var api *discoverySink
	api = newDiscoverySink(t, func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api") {
			http.Redirect(w, r, api.srv.URL+"/api"+r.URL.Path, http.StatusTemporaryRedirect)
			return
		}
		serve(w, r)
	})
	b := discoveryBackend(api.srv.URL)
	b.APIKey = "row-key-459"
	eps, err := NewEndpointCache().fetch(context.Background(), b, "qwen/qwen3.6-27b")
	if err != nil || len(eps) == 0 {
		t.Fatalf("same-origin redirect: %d endpoints, %v; calls %s", len(eps), err, api.log())
	}
}
