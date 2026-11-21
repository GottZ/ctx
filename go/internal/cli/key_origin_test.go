package cli

import (
	"context"
	"errors"
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

// TestKeyRefusalTextOfTheCLI pins the CLI's refusal byte for byte, as Post
// surfaces it: the wording names the ctx CLI and CTX_BASE_URL, and the advice
// of an upgrade keeps the path of CTX_BASE_URL.
func TestKeyRefusalTextOfTheCLI(t *testing.T) {
	second := newSecondHost(t)
	home := redirectingServer(t, second.srv.URL, http.StatusFound)
	_, err := NewClient(Config{BaseURL: home.URL + "/ctx", Key: keyOriginFixtureKey}).Post("query", map[string]any{"query": "q"})
	want := "request to " + home.URL + "/ctx/api/query: Post \"" + second.srv.URL + "/ctx/api/query\": refused a redirect (HTTP 302) to " + second.srv.URL +
		": the ctx CLI sends its API key only to the origin of CTX_BASE_URL, " + home.URL + ", and follows a redirect only within it"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant  %s", err, want)
	}

	up, _ := upgradingServer(t)
	_, _, err = NewClient(Config{BaseURL: up.URL + "/ctx/", Key: keyOriginFixtureKey}).Do(http.MethodGet, "/api/whoami", nil)
	https := "https://" + strings.TrimPrefix(up.URL, "http://")
	want = "request to " + up.URL + "/ctx/api/whoami: Get \"" + https + "/ctx/api/whoami\": refused a redirect (HTTP 301) to " + https +
		": the ctx CLI sends its API key only to the origin of CTX_BASE_URL, " + up.URL +
		", and follows a redirect only within it; set CTX_BASE_URL to " + https + "/ctx/ if that is the server's address"
	if err == nil || err.Error() != want {
		t.Errorf("err = %v\nwant  %s", err, want)
	}
}

// TestKeyRefusalOfAURLWithoutSchemeOrPort: Client.Get takes a whole URL, and
// one without a scheme (protocol-relative) or without a port the scheme fills
// in is named as written — //host:port, ftp://host — not with an empty scheme
// or a dangling colon.
func TestKeyRefusalOfAURLWithoutSchemeOrPort(t *testing.T) {
	home := newSecondHost(t)
	c := NewClient(Config{BaseURL: home.srv.URL, Key: keyOriginFixtureKey})
	hostPort := strings.TrimPrefix(home.srv.URL, "http://")
	for _, tc := range []struct{ to, named string }{
		{"//" + hostPort + "/health", "//" + hostPort},
		{"//ctx.example/health", "//ctx.example"},
		{"//[::1]/health", "//[::1]"},
		{"ftp://ctx.example/health", "ftp://ctx.example"},
	} {
		_, err := c.Get(tc.to)
		want := "request to " + tc.to + ": refused a request to " + tc.named +
			": the ctx CLI sends its API key only to the origin of CTX_BASE_URL, " + home.srv.URL + ", and follows a redirect only within it"
		if err == nil || err.Error() != want {
			t.Errorf("Get(%q): err = %v\nwant  %s", tc.to, err, want)
		}
	}
	if n := home.requests.Load(); n != 0 {
		t.Errorf("the configured server got %d requests", n)
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

// TestKeyBoundToCTXBaseURLNotToTheRequest: the origin is CTX_BASE_URL's, not
// the first request's — Client.Get takes a whole URL, and one on another
// origin is refused before it is sent.
func TestKeyBoundToCTXBaseURLNotToTheRequest(t *testing.T) {
	second := newSecondHost(t)
	home := newSecondHost(t)
	_, err := NewClient(Config{BaseURL: home.srv.URL, Key: keyOriginFixtureKey}).Get(second.srv.URL + "/health")
	if err == nil || !strings.Contains(err.Error(), "refused a request to") {
		t.Errorf("Get to another origin: err = %v, want a refusal", err)
	}
	if n := second.requests.Load(); n != 0 {
		t.Errorf("the other origin got %d requests", n)
	}
}

// TestKeyRequestsUseTheClientsOwnTransport: send keeps the transport of the
// Client's http.Client (here the TLS test server's, which trusts its
// certificate) for the first request and for a redirect within the origin.
func TestKeyRequestsUseTheClientsOwnTransport(t *testing.T) {
	var keyed atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, "/moved"+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		if r.Header.Get("X-Context-Key") == keyOriginFixtureKey {
			keyed.Add(1)
		}
		_, _ = io.WriteString(w, keyOriginAnswer)
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Key: keyOriginFixtureKey, HTTPClient: srv.Client()}
	if _, err := c.Post("query", map[string]any{"query": "q"}); err != nil {
		t.Fatalf("Post over the client's own TLS transport: %v", err)
	}
	if n := keyed.Load(); n != 1 {
		t.Errorf("the key arrived %d times after the redirect, want 1", n)
	}
}

// TestKeyedTimeoutKeepsNetHTTPText: a keyed request that runs out of its
// budget fails with net/http's own timeout text, every time — send leaves the
// client's *http.Transport in place, so the timeout stays net/http's context
// path (a wrapped transport switches it to a timer that races the context).
func TestKeyedTimeoutKeepsNetHTTPText(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) })
	const budget = 20 * time.Millisecond

	// The reference: a plain http.Client with the same budget.
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/api/query", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Timeout: budget}).Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var plain *url.Error
	if !errors.As(err, &plain) || !strings.Contains(plain.Err.Error(), "Client.Timeout exceeded") {
		t.Fatalf("plain client: err = %v, want net/http's timeout", err)
	}
	want := plain.Err.Error()

	for i := range 20 {
		c := NewClientWithTimeout(Config{BaseURL: srv.URL, Key: keyOriginFixtureKey}, budget, maxResponseSize)
		_, err := c.Post("query", map[string]any{"query": "q"})
		if err == nil || !strings.HasSuffix(err.Error(), want) {
			t.Fatalf("call %d: err = %v, want it to end in net/http's %q", i, err, want)
		}
	}
}

// TestKeyRedirectHopLimit: setting CheckRedirect replaces net/http's default,
// so its ten-hop limit is restated — a chain within the origin ends after as
// many requests, with the same text, as with a plain http.Client.
func TestKeyRedirectHopLimit(t *testing.T) {
	var hops atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hops.Add(1) > 50 {
			_, _ = io.WriteString(w, keyOriginAnswer)
			return
		}
		http.Redirect(w, r, "/api/loop", http.StatusTemporaryRedirect)
	}))
	t.Cleanup(srv.Close)

	// The reference: net/http's default redirect policy.
	plainReq, err := http.NewRequestWithContext(context.Background(), http.MethodGet, srv.URL+"/api/loop", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, plainErr := http.DefaultClient.Do(plainReq)
	if resp != nil {
		_ = resp.Body.Close()
	}
	var plainURLErr *url.Error
	if !errors.As(plainErr, &plainURLErr) {
		t.Fatalf("plain err = %v, want net/http's hop limit as a *url.Error", plainErr)
	}
	plainHops := hops.Swap(0)

	_, _, err = NewClient(Config{BaseURL: srv.URL, Key: keyOriginFixtureKey}).Do(http.MethodGet, "/api/loop", nil)
	if err == nil {
		t.Fatal("keyed client followed 50 redirects")
	}
	if got := hops.Load(); got != plainHops {
		t.Errorf("keyed client made %d requests, a plain http.Client %d", got, plainHops)
	}
	want := plainURLErr.Err.Error()
	if !strings.HasSuffix(err.Error(), want) {
		t.Errorf("keyed error %q does not end in net/http's %q", err, want)
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
