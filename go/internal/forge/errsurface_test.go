package forge

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testSince puts a since window into the first-page URL.
var testSince = time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)

// #474 point 4: net/http wraps a transport failure in a *url.Error that
// names the request URL — the api_base path with owner and repository, the
// since window, and on a next page the query the forge chose (GitHub's issue
// listing carries a cursor in it). The forge client keeps the URL out of its
// errors (design/02 §5.4), as the comments at the call sites say: the
// failure stays, the URL does not.
func TestForgeErrorsCarryNoURL(t *testing.T) {
	gone := httptest.NewServer(http.NotFoundHandler())
	base := gone.URL
	gone.Close() // every dial is refused from here on

	const marker = "CURSOR-MARKER-474"
	repo := RepoRef{Owner: "owner474", Repo: "repo474", APIBase: base}
	c := &githubClient{http: &http.Client{}, token: "pat-474"}
	calls := map[string]func() error{
		"first-page": func() error {
			_, err := c.ListIssuesSince(context.Background(), repo, testSince, "")
			return err
		},
		"next-page": func() error {
			_, err := c.ListIssuesPage(context.Background(), base+"/repositories/1/issues?after="+marker)
			return err
		},
		"comments": func() error {
			_, err := c.ListCommentsPage(context.Background(), base+"/repositories/1/issues/comments?after="+marker)
			return err
		},
		"write": func() error {
			_, err := c.CreateIssue(context.Background(), repo, IssueCreate{Title: "t", Body: "b"})
			return err
		},
		"unparseable-url": func() error {
			_, err := c.ListIssuesPage(context.Background(), "http://bad host/x?after="+marker)
			return err
		},
	}
	for name, call := range calls {
		t.Run(name, func(t *testing.T) {
			err := call()
			if err == nil {
				t.Fatal("want the transport failure as an error")
			}
			msg := err.Error()
			for _, leak := range []string{marker, "owner474", "repo474", "/repositories/", "since=", "pat-474"} {
				if strings.Contains(msg, leak) {
					t.Errorf("error %q carries %q from the request URL", msg, leak)
				}
			}
			if !strings.HasPrefix(msg, "forge: ") {
				t.Errorf("error %q lost its forge prefix", msg)
			}
		})
	}
}
