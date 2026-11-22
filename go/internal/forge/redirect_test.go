package forge

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
)

// #459: the forge client carries the project's PAT as a bearer token and, on
// a push, the issue text as the body. net/http alone keeps Authorization on a
// redirect to the same host name on another port (and to a subdomain), and
// re-sends the body on 307/308 to any host. The client follows a redirect
// only within the origin of the request it sent, and a Link rel="next" page
// only on that origin too: GitHub's own redirects (a renamed or transferred
// repository) and its pagination stay on api.github.com.

const forgeIssueMarker = "ISSUE-TEXT-MARKER-459"

// forgeSink records every request that reaches it.
type forgeSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newForgeSink(t *testing.T, h http.HandlerFunc) *forgeSink {
	t.Helper()
	s := &forgeSink{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s authorization=%q body=%s", r.Method, r.URL.RequestURI(), r.Header.Get("Authorization"), b))
		s.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"number":7,"id":7}`)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *forgeSink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

func (s *forgeSink) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

// TestForgeRedirectStaysOnOrigin: a redirect to another origin — here the
// same host name on another port, the case net/http keeps Authorization on —
// is not followed for any status, neither on a GET nor on a write; the
// foreign origin sees nothing and the call ends in an error.
func TestForgeRedirectStaysOnOrigin(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(fmt.Sprintf("write-%d", code), func(t *testing.T) {
			other := newForgeSink(t, nil)
			api := newForgeSink(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.srv.URL+r.URL.Path, code)
			})
			c := newTestClient(api.srv, "pat-459")
			_, _, err := c.CreateIssue(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: api.srv.URL},
				IssueCreate{Title: "t", Body: forgeIssueMarker})
			if n := other.hits(); n != 0 {
				t.Fatalf("a %d redirect carried the request to another origin: %s", code, other.log())
			}
			if err == nil || !strings.Contains(err.Error(), "redirect status") {
				t.Fatalf("a %d redirect to another origin must fail the write as a redirect not followed: %v", code, err)
			}
			if strings.Contains(err.Error(), "pat-459") {
				t.Errorf("the error carries the token: %v", err)
			}
		})
		t.Run(fmt.Sprintf("get-%d", code), func(t *testing.T) {
			other := newForgeSink(t, func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `[{"number":1,"title":"x","state":"open"}]`)
			})
			api := newForgeSink(t, func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, other.srv.URL+r.URL.RequestURI(), code)
			})
			c := newTestClient(api.srv, "pat-459")
			_, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: api.srv.URL}, time.Time{}, "")
			if n := other.hits(); n != 0 {
				t.Fatalf("a %d redirect carried the GET to another origin: %s", code, other.log())
			}
			if err == nil || !strings.Contains(err.Error(), "redirect status") {
				t.Fatalf("a %d redirect to another origin must fail the fetch as a redirect not followed: %v", code, err)
			}
		})
	}
}

// TestForgeRedirectWithinOriginFollowed: GitHub answers a write to a renamed
// repository with a 307 to /repositories/<id>/... on api.github.com — the
// same origin. That redirect is followed with the token and the body.
func TestForgeRedirectWithinOriginFollowed(t *testing.T) {
	var api *forgeSink
	api = newForgeSink(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/repos/") {
			http.Redirect(w, r, api.srv.URL+"/repositories/1/issues", http.StatusTemporaryRedirect)
			return
		}
		_, _ = io.WriteString(w, `{"number":9}`)
	})
	c := newTestClient(api.srv, "pat-459")
	n, _, err := c.CreateIssue(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: api.srv.URL},
		IssueCreate{Title: "t", Body: forgeIssueMarker})
	if err != nil || n != 9 {
		t.Fatalf("CreateIssue through a same-origin 307 = %d, %v; want 9", n, err)
	}
	log := api.log()
	if !strings.Contains(log, "POST /repositories/1/issues authorization=\"Bearer pat-459\"") || strings.Count(log, forgeIssueMarker) != 2 {
		t.Fatalf("the same-origin 307 was not re-sent with token and body: %s", log)
	}
}

// TestForgeNextPageStaysOnOrigin: the Link rel="next" target comes from the
// response; the client sends the token only to it when it is on the origin
// of the request that returned it. A page link to another origin ends the
// fetch with an error and nothing reaches that origin.
func TestForgeNextPageStaysOnOrigin(t *testing.T) {
	other := newForgeSink(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `[]`)
	})
	api := newForgeSink(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<`+other.srv.URL+`/repos/o/r/issues?page=2>; rel="next"`)
		_, _ = io.WriteString(w, `[{"number":1,"title":"p1","state":"open"}]`)
	})
	c := newTestClient(api.srv, "pat-459")
	page, err := c.ListIssuesSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: api.srv.URL}, time.Time{}, "")
	if err == nil && page.NextURL != "" {
		_, _ = c.ListIssuesPage(context.Background(), page.NextURL)
	}
	if n := other.hits(); n != 0 {
		t.Fatalf("the next-page link sent the token to another origin: %s", other.log())
	}
	if err == nil {
		t.Fatal("a next-page link to another origin must fail the fetch")
	}

	// Comments paginate the same way.
	cother := newForgeSink(t, func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `[]`) })
	capi := newForgeSink(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Link", `<`+cother.srv.URL+`/repos/o/r/issues/comments?page=2>; rel="next"`)
		_, _ = io.WriteString(w, `[]`)
	})
	cc := newTestClient(capi.srv, "pat-459")
	cpage, err := cc.ListCommentsSince(context.Background(), RepoRef{Owner: "o", Repo: "r", APIBase: capi.srv.URL}, time.Time{}, "")
	if err == nil && cpage.NextURL != "" {
		_, _ = cc.ListCommentsPage(context.Background(), cpage.NextURL)
	}
	if n := cother.hits(); n != 0 {
		t.Fatalf("the comments next-page link sent the token to another origin: %s", cother.log())
	}
	if err == nil {
		t.Fatal("a comments next-page link to another origin must fail the fetch")
	}
}
