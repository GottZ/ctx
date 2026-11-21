package goldbench_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/goldbench"
)

// The API key of ctx-goldbench (-api-key, else GOLDBENCH_API_KEY) goes only to
// the origin (scheme, host, port) of -endpoint (#432), by the rule the ctx CLI
// and ctx-armsweep hold their keys to (internal/clientkey). Each test below
// runs the chat call against two servers: the configured one answers with a
// redirect to the second, and the second must see neither the key nor any
// request at all — the body of a chat call carries the gold case's prompt.

const goldbenchFixtureKey = "0f1e2d3c4b5a69788796a5b4c3d2e1f00f1e2d3c4b5a69788796a5b4c3d2e1f0"

// goldbenchAnswer is a chat completion ChatWithUsage accepts.
const goldbenchAnswer = `{"choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],` +
	`"usage":{"prompt_tokens":3,"completion_tokens":1}}`

// goldbenchHost records what reaches a server and answers every request with
// goldbenchAnswer.
type goldbenchHost struct {
	srv      *httptest.Server
	requests atomic.Int32 // every request
	keyed    atomic.Int32 // requests that carried the fixture key in any header
}

func newGoldbenchHost(t *testing.T) *goldbenchHost {
	t.Helper()
	h := &goldbenchHost{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.requests.Add(1)
		for _, vs := range r.Header {
			for _, v := range vs {
				if strings.Contains(v, goldbenchFixtureKey) {
					h.keyed.Add(1)
				}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, goldbenchAnswer)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

// goldbenchRedirecting answers every request with a redirect (status code) to
// target followed by the request's own path and query, and counts the
// requests it saw.
func goldbenchRedirecting(t *testing.T, target string, code int, seen *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen.Add(1)
		http.Redirect(w, r, target+r.URL.RequestURI(), code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func goldbenchChat(endpoint, key string) (goldbench.ChatResult, error) {
	c := goldbench.NewClient(endpoint, "m", key, 1, 5*time.Second)
	return c.ChatWithUsage(context.Background(), goldbench.ChatRequest{System: "s", User: "the gold case's prompt"})
}

var goldbenchRedirectCodes = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
	http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
}

// TestGoldbenchKeyNeverFollowsARedirectToAnotherOrigin: for every redirect
// status, a redirect to another origin — the same host on another port, and
// another host name — is refused before it is sent.
//
// RED before #432: the client followed the redirect with net/http's default
// rule, which keeps Authorization whenever the host name stays the same (the
// port is not compared) — the second server got the key — and sends the
// request to another host name without the key, the prompt with it on 307/308.
func TestGoldbenchKeyNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	for _, code := range goldbenchRedirectCodes {
		for _, other := range []string{"other port", "other host"} {
			t.Run(other+"/"+http.StatusText(code), func(t *testing.T) {
				second := newGoldbenchHost(t)
				target := second.srv.URL
				if other == "other host" {
					target = strings.Replace(target, "127.0.0.1", "localhost", 1)
				}
				var homeSeen atomic.Int32
				home := goldbenchRedirecting(t, target, code, &homeSeen)
				_, err := goldbenchChat(home.URL, goldbenchFixtureKey)

				if n := second.keyed.Load(); n != 0 {
					t.Errorf("the second server got the key %d times", n)
				}
				if n := second.requests.Load(); n != 0 {
					t.Errorf("the second server got %d requests", n)
				}
				if n := homeSeen.Load(); n != 1 {
					t.Errorf("the configured server saw %d requests, want 1", n)
				}
				if want := "refused a redirect (HTTP " + strconv.Itoa(code) + ")"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			})
		}
	}
}

// TestGoldbenchRefusalNamesTargetAndRule: the refusal names where the redirect
// pointed, the origin the key is bound to and the flag that sets it, and never
// echoes the key.
func TestGoldbenchRefusalNamesTargetAndRule(t *testing.T) {
	second := newGoldbenchHost(t)
	var seen atomic.Int32
	home := goldbenchRedirecting(t, second.srv.URL, http.StatusTemporaryRedirect, &seen)
	_, err := goldbenchChat(home.URL, goldbenchFixtureKey)
	if err == nil {
		t.Fatal("no error")
	}
	msg := err.Error()
	t.Logf("refusal: %s", msg)
	for _, want := range []string{second.srv.URL, home.URL, "ctx-goldbench", "-endpoint", "HTTP 307", "/v1/chat/completions"} {
		if !strings.Contains(msg, want) {
			t.Errorf("refusal %q does not name %q", msg, want)
		}
	}
	if strings.Contains(msg, goldbenchFixtureKey) {
		t.Errorf("refusal echoes the key: %q", msg)
	}
}

// TestGoldbenchRefusalOfASchemeChangeSaysWhatToSet: an http→https redirect on
// the same host (a proxy upgrading the connection) is not followed — the key
// already crossed in clear text — and the refusal names the -endpoint to use.
func TestGoldbenchRefusalOfASchemeChangeSaysWhatToSet(t *testing.T) {
	var home *httptest.Server
	home = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(home.URL)
		http.Redirect(w, r, "https://"+u.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	t.Cleanup(home.Close)
	u, _ := url.Parse(home.URL)
	_, err := goldbenchChat(home.URL+"/v1", goldbenchFixtureKey)
	if want := "set -endpoint to https://" + u.Host + "/v1/chat/completions if"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
}

// TestGoldbenchKeyFollowsARedirectWithinItsOrigin: a redirect within the
// origin is followed as before, with the key as a bearer token (the form an
// OpenAI-compatible endpoint takes it in, ctx's /v1 among them) and, on
// 307/308, with the body re-sent.
func TestGoldbenchKeyFollowsARedirectWithinItsOrigin(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var moved, bearer, contextKey, withBody atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") == "Bearer "+goldbenchFixtureKey {
					bearer.Add(1)
				}
				if r.Header.Get("X-Context-Key") != "" {
					contextKey.Add(1)
				}
				if !strings.HasPrefix(r.URL.Path, "/moved/") {
					http.Redirect(w, r, "/moved"+r.URL.RequestURI(), code)
					return
				}
				moved.Add(1)
				if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), "the gold case's prompt") {
					withBody.Add(1)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, goldbenchAnswer)
			}))
			t.Cleanup(srv.Close)
			res, err := goldbenchChat(srv.URL, goldbenchFixtureKey)
			if err != nil || res.Content != "ok" {
				t.Fatalf("res=%+v err=%v, want the answer of the moved path", res, err)
			}
			if moved.Load() != 1 || bearer.Load() != 2 || withBody.Load() != 1 {
				t.Errorf("moved=%d bearer=%d body=%d, want the redirect followed once, both hops with the bearer key and the body re-sent",
					moved.Load(), bearer.Load(), withBody.Load())
			}
			if n := contextKey.Load(); n != 0 {
				t.Errorf("X-Context-Key sent %d times — the key goes as a bearer token only", n)
			}
		})
	}
}

// TestGoldbenchWithoutKeySendsNoCredential: without a key nothing is bound —
// no Authorization header, and a redirect is followed as net/http follows it,
// as before #432.
func TestGoldbenchWithoutKeySendsNoCredential(t *testing.T) {
	var auth atomic.Int32
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, ok := r.Header["Authorization"]; ok {
			auth.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, goldbenchAnswer)
	}))
	t.Cleanup(second.Close)
	var seen atomic.Int32
	home := goldbenchRedirecting(t, second.URL, http.StatusTemporaryRedirect, &seen)
	res, err := goldbenchChat(home.URL, "")
	if err != nil || res.Content != "ok" {
		t.Fatalf("res=%+v err=%v, want the keyless call to follow the redirect as before", res, err)
	}
	if n := auth.Load(); n != 0 {
		t.Errorf("a keyless call sent Authorization %d times", n)
	}
}

// TestGoldbenchKeyNeedsAnHTTPEndpoint: with a key, an -endpoint without an
// http or https host is refused before any request, naming the flag.
func TestGoldbenchKeyNeedsAnHTTPEndpoint(t *testing.T) {
	_, err := goldbenchChat("ftp://ctx.example", goldbenchFixtureKey)
	if want := "-endpoint is not a usable URL"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
}
