package armsweep_test

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// The API key of ctx-armsweep goes only to the origin (scheme, host, port) of
// its base URL (#423), by the rule the ctx CLI holds it to (#413). Every
// request helper of armsweep.Client runs against two servers: the configured
// one answers with a redirect to the second, and the second must see neither
// the key nor any request at all — the body of a measurement POST carries the
// gold query text.

const armsweepFixtureKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// armsweepAnswer satisfies every helper below: Measure needs an arm_ranks
// block, Drift a drift block, MigrationsMax and EfSearchEffective a db
// section, Setting a setting value.
const armsweepAnswer = `{"success":true,"sources":[],` +
	`"arm_ranks":{"rows":[],"fusion_order":[],"effective_query":"q","embed_model":"m","selector":{"mode":"ann","reason":"fixture"}},` +
	`"drift":{"at":"2026-11-21T00:00:00Z","retrievable_blocks":1},` +
	`"db":{"migrations_max":7,"hnsw":{"ef_search_effective":"40"}},` +
	`"setting":{"value":true}}`

// armsweepHost records what reaches a server.
type armsweepHost struct {
	srv      *httptest.Server
	requests atomic.Int32 // every request
	keyed    atomic.Int32 // requests that carried the fixture key
}

func newArmsweepHost(t *testing.T) *armsweepHost {
	t.Helper()
	h := &armsweepHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		if r.Header.Get("X-Context-Key") == armsweepFixtureKey {
			h.keyed.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, armsweepAnswer)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// armsweepRedirecting answers every request with a redirect (status code) to
// target followed by the request's own path and query, and counts the
// requests it saw.
func armsweepRedirecting(t *testing.T, target string, code int, seen *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen.Add(1)
		}
		http.Redirect(w, r, target+r.URL.RequestURI(), code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// armsweepCall is one request helper of armsweep.Client.
type armsweepCall struct {
	name string
	body bool // the helper POSTs a body
	call func(c *armsweep.Client) error
}

func armsweepCalls() []armsweepCall {
	ctx := context.Background()
	return []armsweepCall{
		{name: "Measure", body: true, call: func(c *armsweep.Client) error {
			_, err := c.Measure(ctx, armsweep.QueryRequest{Query: "q", ArmRanks: true})
			return err
		}},
		{name: "Drift", body: true, call: func(c *armsweep.Client) error {
			_, err := c.Drift(ctx, []string{"0000"})
			return err
		}},
		{name: "MigrationsMax", call: func(c *armsweep.Client) error {
			_, err := c.MigrationsMax(ctx)
			return err
		}},
		{name: "EfSearchEffective", call: func(c *armsweep.Client) error {
			_, err := c.EfSearchEffective(ctx)
			return err
		}},
		{name: "Setting", call: func(c *armsweep.Client) error {
			_, err := c.Setting(ctx, "graph.enabled")
			return err
		}},
		{name: "PostStageState", call: func(c *armsweep.Client) error {
			_, err := c.PostStageState(ctx)
			return err
		}},
	}
}

var armsweepRedirectCodes = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
	http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
}

// TestArmsweepKeyNeverFollowsARedirectToAnotherOrigin: for every request
// helper and every redirect status, a redirect to another origin — the same
// host on another port, and another host name — is refused before it is sent,
// and the refusal is not retryable: it answers the same way every time.
//
// RED before #423: the client followed the redirect with net/http's default
// rule, which keeps X-Context-Key on a host change — the second host got the
// key once per helper call (four times for PostStageState).
func TestArmsweepKeyNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	for _, ac := range armsweepCalls() {
		for _, code := range armsweepRedirectCodes {
			for _, other := range []string{"other port", "other host"} {
				t.Run(ac.name+"/"+other+"/"+http.StatusText(code), func(t *testing.T) {
					second := newArmsweepHost(t)
					target := second.srv.URL
					if other == "other host" {
						target = strings.Replace(target, "127.0.0.1", "localhost", 1)
					}
					var homeSeen atomic.Int32
					home := armsweepRedirecting(t, target, code, &homeSeen)
					err := ac.call(armsweep.NewClient(home.URL, armsweepFixtureKey, 5*time.Second))

					if n := second.keyed.Load(); n != 0 {
						t.Errorf("the second host got the key %d times", n)
					}
					if n := second.requests.Load(); n != 0 {
						t.Errorf("the second host got %d requests", n)
					}
					if n := homeSeen.Load(); n != 1 {
						t.Errorf("the configured server saw %d requests, want 1", n)
					}
					if err == nil {
						t.Fatal("no error")
					}
					if errors.Is(err, armsweep.ErrRetryable) {
						t.Errorf("a refused redirect is marked retryable: %v", err)
					}
					if want := "refused a redirect (HTTP " + strconv.Itoa(code) + ")"; !strings.Contains(err.Error(), want) {
						t.Errorf("err = %v, want it to say %q", err, want)
					}
				})
			}
		}
	}
}

// TestArmsweepRefusalNamesTargetAndRule: the refusal names where the redirect
// pointed, the origin the key is bound to and the flag that sets it, and
// never echoes the key.
func TestArmsweepRefusalNamesTargetAndRule(t *testing.T) {
	second := newArmsweepHost(t)
	home := armsweepRedirecting(t, second.srv.URL, http.StatusTemporaryRedirect, nil)
	_, err := armsweep.NewClient(home.URL, armsweepFixtureKey, 5*time.Second).Measure(context.Background(), armsweep.QueryRequest{Query: "q"})
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	for _, want := range []string{second.srv.URL, home.URL, "ctx-armsweep", "-base-url", "HTTP 307", "/api/query"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, armsweepFixtureKey) {
		t.Errorf("refusal echoes the key: %q", msg)
	}
}

// TestArmsweepRefusalOfASchemeChangeSaysWhatToSet: an http→https redirect on
// the same host (a proxy upgrading the connection) is not followed — the key
// already crossed in clear text — and the refusal names the -base-url to use.
func TestArmsweepRefusalOfASchemeChangeSaysWhatToSet(t *testing.T) {
	var home *httptest.Server
	home = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(home.URL)
		http.Redirect(w, r, "https://"+u.Host+r.URL.RequestURI(), http.StatusMovedPermanently)
	}))
	t.Cleanup(home.Close)
	u, _ := url.Parse(home.URL)
	_, err := armsweep.NewClient(home.URL, armsweepFixtureKey, 5*time.Second).Setting(context.Background(), "graph.enabled")
	if want := "set -base-url to https://" + u.Host + " if"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
}

// TestArmsweepKeyNeedsAnHTTPBase: a base URL without an http or https host
// binds the key to nothing. The request is not made, and the error is a
// configuration error, not a retryable transport fault.
//
// RED before #423: "localhost:8080" parsed as the scheme "localhost", net/http
// answered "unsupported protocol scheme", and the client marked that
// retryable — a dump spent its retry budget on every case and excluded them.
func TestArmsweepKeyNeedsAnHTTPBase(t *testing.T) {
	for _, base := range []string{"localhost:8080", "ftp://ctx.example", "http:///"} {
		_, err := armsweep.NewClient(base, armsweepFixtureKey, 5*time.Second).Setting(context.Background(), "graph.enabled")
		if err == nil || errors.Is(err, armsweep.ErrRetryable) || !strings.Contains(err.Error(), "-base-url is not a usable URL") {
			t.Errorf("base %q: err = %v, want a non-retryable refusal of the base URL", base, err)
		}
		if err != nil && strings.Contains(err.Error(), armsweepFixtureKey) {
			t.Errorf("base %q: the error echoes the key: %v", base, err)
		}
	}
}

// TestArmsweepRefusedRedirectStopsTheRun: a measurement run whose instance
// redirects off the origin fails with the refusal — each case is tried once,
// none is retried, and the run does not end in a dump of excluded cases that
// reads like a run of transient faults.
func TestArmsweepRefusedRedirectStopsTheRun(t *testing.T) {
	second := newArmsweepHost(t)
	var homeSeen atomic.Int32
	home := armsweepRedirecting(t, second.srv.URL, http.StatusTemporaryRedirect, &homeSeen)
	r := &armsweep.Runner{
		Client:  armsweep.NewClient(home.URL, armsweepFixtureKey, 5*time.Second),
		RunID:   "key-origin-fixture",
		Retries: 2,
	}
	q := "key-origin-frage"
	_, _, _, err := r.Prime(context.Background(), []goldset.Case{{Slice: goldset.SliceGlob, Index: 0, Query: q, QuerySHA256: goldset.SHA256Hex(q)}})
	if err == nil || !strings.Contains(err.Error(), "refused a redirect") {
		t.Errorf("Prime: err = %v, want the run stopped by the refusal", err)
	}
	if n := homeSeen.Load(); n != 1 {
		t.Errorf("the configured server saw %d requests, want 1 (no retry of a refusal)", n)
	}
	if n := second.requests.Load(); n != 0 {
		t.Errorf("the second host got %d requests", n)
	}
}

// TestArmsweepKeyFollowsARedirectWithinTheOrigin: a redirect inside the origin
// is followed as before, the key arrives at the final hop, and a 307/308
// re-sends the body.
func TestArmsweepKeyFollowsARedirectWithinTheOrigin(t *testing.T) {
	for _, ac := range armsweepCalls() {
		for _, code := range armsweepRedirectCodes {
			t.Run(ac.name+"/"+http.StatusText(code), func(t *testing.T) {
				var moved, movedKeyed, movedBody atomic.Int32
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if !strings.HasPrefix(r.URL.Path, "/moved/") {
						http.Redirect(w, r, "/moved"+r.URL.RequestURI(), code)
						return
					}
					moved.Add(1)
					if r.Header.Get("X-Context-Key") == armsweepFixtureKey {
						movedKeyed.Add(1)
					}
					if b, _ := io.ReadAll(r.Body); len(b) > 0 {
						movedBody.Add(1)
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, armsweepAnswer)
				}))
				t.Cleanup(srv.Close)

				if err := ac.call(armsweep.NewClient(srv.URL, armsweepFixtureKey, 5*time.Second)); err != nil {
					t.Fatalf("call: %v", err)
				}
				if moved.Load() == 0 || movedKeyed.Load() != moved.Load() {
					t.Errorf("after the redirect %d requests arrived, %d with the key", moved.Load(), movedKeyed.Load())
				}
				resends := code == http.StatusTemporaryRedirect || code == http.StatusPermanentRedirect
				if ac.body && resends && movedBody.Load() != moved.Load() {
					t.Errorf("the body was re-sent on %d of %d requests after a %d within the origin", movedBody.Load(), moved.Load(), code)
				}
			})
		}
	}
}

// TestArmsweepKeyUsesTheClientsOwnTransport: the request goes out through the
// Client's own http.Client — here the TLS test server's, which trusts its
// certificate — for the first request and for a redirect within the origin.
func TestArmsweepKeyUsesTheClientsOwnTransport(t *testing.T) {
	var keyed atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/moved/") {
			http.Redirect(w, r, "/moved"+r.URL.RequestURI(), http.StatusPermanentRedirect)
			return
		}
		if r.Header.Get("X-Context-Key") == armsweepFixtureKey {
			keyed.Add(1)
		}
		_, _ = io.WriteString(w, armsweepAnswer)
	}))
	t.Cleanup(srv.Close)
	c := &armsweep.Client{BaseURL: srv.URL, Key: armsweepFixtureKey, HTTP: srv.Client()}
	if _, err := c.Measure(context.Background(), armsweep.QueryRequest{Query: "q"}); err != nil {
		t.Fatalf("Measure over the client's own TLS transport: %v", err)
	}
	if n := keyed.Load(); n != 1 {
		t.Errorf("the key arrived %d times after the redirect, want 1", n)
	}
}
