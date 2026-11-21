package goldset

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The chat call of ctx-goldset carries private block content — the G-Q
// prompt, a paraphrase prompt, a judge excerpt — to a backend that passed
// RequireOnPrem by its base_url. A redirect is followed only within the origin
// (scheme, host, port) of that base_url, by the rule internal/clientkey holds
// the ctx tools to (#446): net/http alone follows a 307/308 to any host with
// the body, past the on-prem check.

const goldsetPrompt = "BODY: the private content of a block"

const goldsetAnswer = `{"choices":[{"message":{"content":"What does the note say?"}}]}`

func goldsetClient(t *testing.T, baseURL string) *ChatClient {
	t.Helper()
	c, err := NewChatClient(Backend{Name: "local-test", BaseURL: baseURL, Locality: "local",
		ModelMap: json.RawMessage(`{"default":{"model":"m"}}`)}, "", 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// TestChatClientNeverFollowsARedirectToAnotherOrigin: for every redirect
// status, a redirect to the same host on another port, or to another host
// name, is refused before it is sent, and the second server sees nothing.
//
// RED before #446: net/http followed it — on 307/308 with the block content.
func TestChatClientNeverFollowsARedirectToAnotherOrigin(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
		http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for _, other := range []string{"other port", "other host"} {
			t.Run(other+"/"+http.StatusText(code), func(t *testing.T) {
				var secondSeen, withBody, homeSeen atomic.Int32
				second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					secondSeen.Add(1)
					if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), goldsetPrompt) {
						withBody.Add(1)
					}
					_, _ = io.WriteString(w, goldsetAnswer)
				}))
				t.Cleanup(second.Close)
				target := second.URL
				if other == "other host" {
					target = strings.Replace(target, "127.0.0.1", "localhost", 1)
				}
				home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					homeSeen.Add(1)
					http.Redirect(w, r, target+r.URL.RequestURI(), code)
				}))
				t.Cleanup(home.Close)

				_, err := goldsetClient(t, home.URL).Ask(context.Background(), "s", goldsetPrompt, 8, 0)

				if n := secondSeen.Load(); n != 0 {
					t.Errorf("the second server got %d requests, %d of them with the block content", n, withBody.Load())
				}
				if n := homeSeen.Load(); n != 1 {
					t.Errorf("the backend saw %d requests, want 1", n)
				}
				if want := "refused a redirect (HTTP " + strconv.Itoa(code) + ")"; err == nil || !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			})
		}
	}
}

// TestChatClientRefusalNamesTargetAndBackend: the refusal names where the
// redirect pointed, the origin it is held to and where that origin comes
// from; for an http→https upgrade on the same host it names the base_url to
// set, without the chat path the client appends.
func TestChatClientRefusalNamesTargetAndBackend(t *testing.T) {
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, goldsetAnswer)
	}))
	t.Cleanup(second.Close)
	home := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, second.URL+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	t.Cleanup(home.Close)
	_, err := goldsetClient(t, home.URL).Ask(context.Background(), "s", goldsetPrompt, 8, 0)
	if err == nil {
		t.Fatal("no error")
	}
	t.Logf("refusal: %s", err)
	for _, want := range []string{second.URL, home.URL, "HTTP 308",
		"ctx-goldset sends its requests only to the origin of the base_url of the -backend row"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("refusal %q does not name %q", err, want)
		}
	}

	var upgrading *httptest.Server
	upgrading = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, _ := url.Parse(upgrading.URL)
		http.Redirect(w, r, "https://"+u.Host+r.URL.RequestURI(), http.StatusPermanentRedirect)
	}))
	t.Cleanup(upgrading.Close)
	u, _ := url.Parse(upgrading.URL)
	_, err = goldsetClient(t, upgrading.URL+"/").Ask(context.Background(), "s", goldsetPrompt, 8, 0)
	if want := "set the base_url of the -backend row to https://" + u.Host + " if"; err == nil || !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to say %q", err, want)
	}
}

// TestChatClientFollowsARedirectWithinItsOrigin: a redirect within the origin
// is followed as before, on 307/308 with the body re-sent.
func TestChatClientFollowsARedirectWithinItsOrigin(t *testing.T) {
	for _, code := range []int{http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var moved, withBody atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasPrefix(r.URL.Path, "/moved/") {
					http.Redirect(w, r, "/moved"+r.URL.RequestURI(), code)
					return
				}
				moved.Add(1)
				if b, _ := io.ReadAll(r.Body); strings.Contains(string(b), goldsetPrompt) {
					withBody.Add(1)
				}
				_, _ = io.WriteString(w, goldsetAnswer)
			}))
			t.Cleanup(srv.Close)
			ans, err := goldsetClient(t, srv.URL).Ask(context.Background(), "s", goldsetPrompt, 8, 0)
			if err != nil || ans != "What does the note say?" {
				t.Fatalf("ans=%q err=%v, want the answer of the moved path", ans, err)
			}
			if moved.Load() != 1 || withBody.Load() != 1 {
				t.Errorf("moved=%d body=%d, want the redirect followed once with the body re-sent", moved.Load(), withBody.Load())
			}
		})
	}
}
