package forge

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// #474 point 1: a forge.api_base on http whose host redirects to https. The
// redirect rule of #459 lets the http→https upgrade on the same host
// through, so the first request followed it — the token had crossed the wire
// in clear text on the first hop already — and the fault surfaced, if at
// all, one page later as "the next-page link leaves the origin". A write
// fared worse: net/http turns a POST or PATCH answered with 301 into a GET of
// the target, so an update "succeeded" against an issue it never changed.
// The forge client follows no upgrade: the call fails on the first hop, and
// the error says which setting to change.

// upgradeHost is the api_base host; the test dialer routes its port 80 to a
// plain listener that redirects and its port 443 to a TLS listener that
// would answer. httptest's certificate names example.com.
const upgradeHost = "example.com"

type upgradeRig struct {
	client *http.Client
	mu     sync.Mutex
	tls    []string // requests that reached the https side
}

func (r *upgradeRig) tlsSeen() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return strings.Join(r.tls, " | ")
}

func newUpgradeRig(t *testing.T, code int) *upgradeRig {
	t.Helper()
	rig := &upgradeRig{}
	secure := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rig.mu.Lock()
		rig.tls = append(rig.tls, r.Method+" "+r.URL.RequestURI()+" body="+string(b))
		rig.mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			if strings.HasSuffix(r.URL.Path, "/issues/7") {
				_, _ = io.WriteString(w, `{"number":7,"title":"t","state":"open"}`)
				return
			}
			_, _ = io.WriteString(w, `[{"number":1,"title":"x","state":"open"}]`)
		default:
			_, _ = io.WriteString(w, `{"number":9,"id":9}`)
		}
	}))
	t.Cleanup(secure.Close)
	plain := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", "https://"+upgradeHost+r.URL.RequestURI())
		w.WriteHeader(code)
	}))
	t.Cleanup(plain.Close)

	tr := secure.Client().Transport.(*http.Transport).Clone()
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		switch addr {
		case upgradeHost + ":80":
			return d.DialContext(ctx, network, plain.Listener.Addr().String())
		case upgradeHost + ":443":
			return d.DialContext(ctx, network, secure.Listener.Addr().String())
		}
		return nil, &net.OpError{Op: "dial", Net: network, Err: io.ErrUnexpectedEOF}
	}
	t.Cleanup(tr.CloseIdleConnections)
	rig.client = &http.Client{Transport: tr, Timeout: 5 * time.Second}
	return rig
}

func TestForgeRefusesSchemeUpgrade(t *testing.T) {
	repo := RepoRef{Owner: "o", Repo: "r", APIBase: "http://" + upgradeHost + "/api/v3"}
	calls := map[string]func(c *githubClient) error{
		"list": func(c *githubClient) error {
			_, err := c.ListIssuesSince(context.Background(), repo, time.Time{}, "")
			return err
		},
		"create": func(c *githubClient) error {
			_, err := c.CreateIssue(context.Background(), repo, IssueCreate{Title: "t", Body: "ISSUE-TEXT-474"})
			return err
		},
		"update": func(c *githubClient) error {
			body := "ISSUE-TEXT-474"
			return c.UpdateIssue(context.Background(), repo, 7, IssuePatch{Body: &body})
		},
	}
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		for name, call := range calls {
			t.Run(name+"-"+http.StatusText(code), func(t *testing.T) {
				rig := newUpgradeRig(t, code)
				err := call(&githubClient{http: rig.client, token: "pat-474"})
				if seen := rig.tlsSeen(); seen != "" {
					t.Fatalf("the client followed the %d upgrade to https: %s", code, seen)
				}
				if err == nil {
					t.Fatalf("a %d upgrade of the api_base must fail the call", code)
				}
				msg := err.Error()
				if !strings.Contains(msg, "forge.api_base") || !strings.Contains(msg, "https") {
					t.Errorf("error %q does not tell the operator to set forge.api_base to https", msg)
				}
				if strings.Contains(msg, "pat-474") || strings.Contains(msg, "/repos/") || strings.Contains(msg, upgradeHost) {
					t.Errorf("error %q carries the token or the URL", msg)
				}
				if got := sanitizeErr(err); got != msg {
					t.Errorf("last_error would read %q, not the advice %q", got, msg)
				}
			})
		}
	}
}

// TestSanitizeErrPassesOperatorErrors: the errors that name a configuration
// the operator has to change reach last_error as they are; they consist of
// fixed text and a status. Every other error stays reduced to its class.
func TestSanitizeErrPassesOperatorErrors(t *testing.T) {
	for _, err := range []error{errRedirectNotFollowed(http.StatusMovedPermanently)} {
		if got := sanitizeErr(err); got != err.Error() {
			t.Errorf("sanitizeErr(%q) = %q; want the error itself", err, got)
		}
	}
}
