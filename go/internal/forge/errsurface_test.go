package forge

import (
	"context"
	"errors"
	"net"
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
	// A redirect whose Location does not parse: net/http's error quotes the
	// whole Location (#474 R4).
	badLocation := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", "http://[::1/repositories/1/issues?after="+marker)
		w.WriteHeader(http.StatusFound)
	}))
	t.Cleanup(badLocation.Close)
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
		"malformed-location": func() error {
			_, err := c.ListIssuesPage(context.Background(), badLocation.URL+"/repositories/1/issues")
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

// TestForgeErrorsKeepTheirClass: the text of a transport failure is fixed,
// its cause stays reachable — a canceled or timed-out request and a network
// error are still told apart by errors.Is and errors.As.
func TestForgeErrorsKeepTheirClass(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) { <-r.Context().Done() }))
	t.Cleanup(slow.Close)
	gone := httptest.NewServer(http.NotFoundHandler())
	goneURL := gone.URL
	gone.Close()

	c := &githubClient{http: &http.Client{}}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, errCanceled := c.ListIssuesPage(canceled, slow.URL+"/x")
	expired, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	_, errDeadline := c.ListIssuesPage(expired, slow.URL+"/x")
	_, errRefused := c.ListIssuesPage(context.Background(), goneURL+"/x")

	if !errors.Is(errCanceled, context.Canceled) {
		t.Errorf("canceled: %v is not context.Canceled", errCanceled)
	}
	if !errors.Is(errDeadline, context.DeadlineExceeded) {
		t.Errorf("deadline: %v is not context.DeadlineExceeded", errDeadline)
	}
	var ne net.Error
	if !errors.As(errRefused, &ne) {
		t.Errorf("refused: %v carries no net.Error", errRefused)
	}
	for _, err := range []error{errCanceled, errDeadline, errRefused} {
		if err == nil || !strings.HasPrefix(err.Error(), "forge: request failed: ") {
			t.Errorf("error %v lost its forge prefix", err)
		}
	}
}
