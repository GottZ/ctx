package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The API key goes only to the origin (scheme, host, port) of CTX_BASE_URL
// (#413). These tests run every request helper that carries the key against
// two servers: the configured one answers with a redirect to the second, and
// the second must never see the key — nor any request at all, because the
// body of a POST is the user's content too.

const keyOriginFixtureKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// keyOriginAnswer satisfies every helper below: Post/Do/Get return the bytes,
// postIngest decodes success, probeKey reads success and label, the
// statusline reads status and stats.total_blocks.
const keyOriginAnswer = `{"success":true,"label":"fixture","status":"ok","stats":{"total_blocks":3}}`

// secondHost records what reaches the server the configured one redirects to.
type secondHost struct {
	srv      *httptest.Server
	requests atomic.Int32 // every request
	keyed    atomic.Int32 // requests that carried X-Context-Key
	api      atomic.Int32 // requests to an /api/ path (the keyed routes)
}

func newSecondHost(t *testing.T) *secondHost {
	t.Helper()
	h := &secondHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if r.Header.Get("X-Context-Key") != "" {
			h.keyed.Add(1)
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			h.api.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, keyOriginAnswer)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// redirectingServer answers every request with a redirect (status code) to
// target followed by the request's own path and query.
func redirectingServer(t *testing.T, target string, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target+r.URL.RequestURI(), code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// keyedCall is one request helper of the CLI that carries the key. It returns
// the error the helper surfaced (nil where the helper swallows errors by
// design: the init probe and the statusline).
type keyedCall struct {
	name string
	// keyless names a helper that also sends a request WITHOUT the key (the
	// statusline's GET /health): that one request may reach the second host.
	keyless bool
	// body names a helper whose request carries a body.
	body bool
	call func(base string) error
}

func keyedCalls() []keyedCall {
	cfg := func(base string) Config { return Config{BaseURL: base, Key: keyOriginFixtureKey} }
	return []keyedCall{
		{name: "Post", body: true, call: func(base string) error {
			_, err := NewClient(cfg(base)).Post("query", map[string]any{"query": "q"})
			return err
		}},
		{name: "Do GET", call: func(base string) error {
			_, _, err := NewClient(cfg(base)).Do(http.MethodGet, "/api/whoami", nil)
			return err
		}},
		{name: "Do PUT", body: true, call: func(base string) error {
			_, _, err := NewClient(cfg(base)).Do(http.MethodPut, "/api/settings/search.limit", map[string]any{"value": 7})
			return err
		}},
		{name: "Get", call: func(base string) error {
			_, err := NewClient(cfg(base)).Get(strings.TrimSuffix(base, "/") + "/health")
			return err
		}},
		{name: "withTimeout", body: true, call: func(base string) error {
			_, err := NewClient(cfg(base)).withTimeout(5*time.Second).Post("manage", map[string]any{"action": "stats"})
			return err
		}},
		{name: "ingest upload", body: true, call: func(base string) error {
			_, err := postIngest(NewClient(cfg(base)), strings.TrimSuffix(base, "/")+"/api/ingest",
				ingestRequestPayload{Source: "fixture", Chunks: []ingestChunkPayload{{Title: "t", Content: "c"}}})
			return err
		}},
		{name: "init key probe", call: func(base string) error {
			probeKey(base, keyOriginFixtureKey)
			return nil
		}},
		{name: "statusline", keyless: true, body: true, call: func(base string) error {
			fetchBackendLive(cfg(base))
			return nil
		}},
	}
}

var redirectCodes = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
	http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
}

// TestKeyNeverFollowsARedirectToAnotherOrigin: for every keyed helper and
// every redirect status, a redirect to another origin — the same host on
// another port, and another host name — is refused before it is sent.
func TestKeyNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	for _, kc := range keyedCalls() {
		for _, code := range redirectCodes {
			for _, other := range []string{"other port", "other host"} {
				t.Run(kc.name+"/"+other+"/"+http.StatusText(code), func(t *testing.T) {
					second := newSecondHost(t)
					target := second.srv.URL
					if other == "other host" {
						target = strings.Replace(target, "127.0.0.1", "localhost", 1)
					}
					home := redirectingServer(t, target, code)
					err := kc.call(home.URL)

					if n := second.keyed.Load(); n != 0 {
						t.Errorf("the second host got the key %d times", n)
					}
					if n := second.api.Load(); n != 0 {
						t.Errorf("the second host got %d requests to a keyed /api/ route", n)
					}
					if n := second.requests.Load(); n != 0 && !kc.keyless {
						t.Errorf("the second host got %d requests", n)
					}
					if kc.name == "Post" && err == nil {
						t.Error("Post through a redirect to another origin returned no error")
					}
				})
			}
		}
	}
}

// TestKeyRefusalNamesTargetAndRule: the refusal names where the redirect
// pointed, the origin the key is bound to, and CTX_BASE_URL; it never echoes
// the key.
func TestKeyRefusalNamesTargetAndRule(t *testing.T) {
	second := newSecondHost(t)
	home := redirectingServer(t, second.srv.URL, http.StatusTemporaryRedirect)
	_, err := NewClient(Config{BaseURL: home.URL, Key: keyOriginFixtureKey}).Post("query", map[string]any{"query": "q"})
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{second.srv.URL, home.URL, "CTX_BASE_URL", "redirect", "307"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, keyOriginFixtureKey) {
		t.Errorf("refusal echoes the key: %q", msg)
	}
}

// TestKeyRefusalOfASchemeChangeSaysWhatToSet: an http→https redirect on the
// same host is the common benign case (a proxy upgrading the connection). The
// key already crossed the wire in clear text on the first hop, so it is not
// followed; the refusal says what CTX_BASE_URL should be instead.
func TestKeyRefusalOfASchemeChangeSaysWhatToSet(t *testing.T) {
	home, want := upgradingServer(t)
	_, _, err := NewClient(Config{BaseURL: home.URL, Key: keyOriginFixtureKey}).Do(http.MethodGet, "/api/whoami", nil)
	if err == nil {
		t.Fatal("no error")
	}
	if !strings.Contains(err.Error(), want) {
		t.Errorf("refusal %q does not say %q", err, want)
	}
}

// TestInitKeyProbeNamesTheRefusal: ctx init's key probe reports the refusal
// and its advice, not "server unreachable" — the server did answer.
func TestInitKeyProbeNamesTheRefusal(t *testing.T) {
	home, want := upgradingServer(t)
	p := probeKey(home.URL, keyOriginFixtureKey)
	if p.verdict != keyUnverified || !strings.Contains(p.detail, want) {
		t.Errorf("probe = %+v, want unverified with a detail that says %q", p, want)
	}
}

// upgradingServer redirects every request to https on its own host and port,
// as a proxy upgrading the connection does, and returns the advice the
// refusal must give.
func upgradingServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	var home *httptest.Server
	home = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(home.URL)
		http.Redirect(w, r, "https://"+u.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(home.Close)
	u, _ := url.Parse(home.URL)
	return home, "set CTX_BASE_URL to https://" + u.Host
}

// TestKeyRedirectChainLeavingTheOrigin: a redirect within the origin followed
// by one out of it is refused at the second hop.
func TestKeyRedirectChainLeavingTheOrigin(t *testing.T) {
	second := newSecondHost(t)
	var hops atomic.Int32
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hops.Add(1)
		if strings.HasPrefix(r.URL.Path, "/hop/") {
			http.Redirect(w, r, second.srv.URL+r.URL.RequestURI(), http.StatusTemporaryRedirect)
			return
		}
		http.Redirect(w, r, "/hop"+r.URL.RequestURI(), http.StatusTemporaryRedirect)
	}))
	t.Cleanup(home.Close)
	_, err := NewClient(Config{BaseURL: home.URL, Key: keyOriginFixtureKey}).Post("store", map[string]any{"content": "c"})
	if err == nil {
		t.Error("no error")
	}
	if n := hops.Load(); n != 2 {
		t.Errorf("the configured server saw %d requests, want 2 (the first hop and the one within the origin)", n)
	}
	if n := second.requests.Load(); n != 0 {
		t.Errorf("the second host got %d requests", n)
	}
}

// TestKeyFollowsARedirectWithinTheOrigin: a redirect inside the origin is
// followed as before, the key arrives at the final hop, and a 307/308 re-sends
// the body.
func TestKeyFollowsARedirectWithinTheOrigin(t *testing.T) {
	for _, kc := range keyedCalls() {
		for _, code := range redirectCodes {
			t.Run(kc.name+"/"+http.StatusText(code), func(t *testing.T) {
				var moved, movedKeyed, movedBody atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.URL.Path, "/moved/") {
						http.Redirect(w, r, "/moved"+r.URL.RequestURI(), code)
						return
					}
					moved.Add(1)
					if r.Header.Get("X-Context-Key") == keyOriginFixtureKey {
						movedKeyed.Add(1)
					}
					if b, _ := io.ReadAll(r.Body); len(b) > 0 {
						movedBody.Add(1)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, keyOriginAnswer)
				}))
				t.Cleanup(srv.Close)

				if err := kc.call(srv.URL); err != nil {
					t.Fatalf("call: %v", err)
				}
				keyed := moved.Load()
				if kc.keyless {
					keyed-- // the statusline's GET /health carries no key
				}
				if moved.Load() == 0 || movedKeyed.Load() != keyed {
					t.Errorf("after the redirect %d requests arrived, %d with the key, want %d with the key", moved.Load(), movedKeyed.Load(), keyed)
				}
				resends := code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
				if kc.body && resends && movedBody.Load() == 0 {
					t.Error("the body was not re-sent on a 307/308 within the origin")
				}
			})
		}
	}
}
