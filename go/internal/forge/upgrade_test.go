package forge

import (
	"context"
	"errors"
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
			_, _, err := c.CreateIssue(context.Background(), repo, IssueCreate{Title: "t", Body: "ISSUE-TEXT-474"})
			return err
		},
		"update": func(c *githubClient) error {
			body := "ISSUE-TEXT-474"
			_, err := c.UpdateIssue(context.Background(), repo, 7, IssuePatch{Body: &body})
			return err
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
	for _, err := range []error{errRedirectNotFollowed(http.StatusMovedPermanently), errAPIBaseUpgrade, errNextOffOrigin, errLinkUnparseable} {
		if got := sanitizeErr(err); got != err.Error() {
			t.Errorf("sanitizeErr(%q) = %q; want the error itself", err, got)
		}
	}
	if got := sanitizeErr(withoutURL("forge: request failed", errors.New("dial tcp: connection refused"))); got != "sync wire error" {
		t.Errorf("a transport failure reads %q in last_error; want the class", got)
	}
}

// #474 R3: net/http repeats a POST or PATCH answered with 301, 302 or 303 as
// a GET of the Location, without the body. Within the origin the redirect
// rule let that hop through, and the GET of the moved issue answered 200:
// UpdateIssue reported success for a write that never happened, and the
// push leg advanced its base. A hop that changes the method is refused;
// 307 and 308 keep method and body and stay followed.
func TestForgeWriteRedirectThatTurnsIntoAGetRefused(t *testing.T) {
	for _, code := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther, http.StatusTemporaryRedirect, http.StatusPermanentRedirect} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				seen = append(seen, r.Method+" "+r.URL.Path+" body="+string(b))
				mu.Unlock()
				if strings.HasPrefix(r.URL.Path, "/repos/") {
					w.Header().Set("Location", "/moved"+strings.TrimPrefix(r.URL.Path, "/repos/o/r"))
					w.WriteHeader(code)
					return
				}
				_, _ = io.WriteString(w, `{"number":7,"id":7,"title":"t","state":"open"}`)
			}))
			t.Cleanup(srv.Close)
			c := &githubClient{http: srv.Client(), token: "pat-474"}
			repo := RepoRef{Owner: "o", Repo: "r", APIBase: srv.URL}
			body := "ISSUE-TEXT-474"
			errs := map[string]error{}
			_, errs["update"] = c.UpdateIssue(context.Background(), repo, 7, IssuePatch{Body: &body})
			_, errs["comment"] = c.UpdateComment(context.Background(), repo, 7, body)
			_, _, errs["create"] = c.CreateIssue(context.Background(), repo, IssueCreate{Title: "t", Body: body})
			mu.Lock()
			log := strings.Join(seen, " | ")
			mu.Unlock()
			for call, err := range errs {
				switch code {
				case http.StatusTemporaryRedirect, http.StatusPermanentRedirect:
					if err != nil {
						t.Errorf("%s: a %d keeps method and body and must be followed: %v", call, code, err)
					}
				default:
					if err == nil || !strings.Contains(err.Error(), "GET") {
						t.Errorf("%s: a %d that would repeat the write as a GET: err %v; want it refused", call, code, err)
					}
					if got := sanitizeErr(err); got == "sync wire error" {
						t.Errorf("%s: last_error would read %q", call, got)
					}
				}
			}
			if code != http.StatusTemporaryRedirect && code != http.StatusPermanentRedirect && strings.Contains(log, "GET /moved") {
				t.Fatalf("the %d write was repeated as a GET: %s", code, log)
			}
		})
	}
}
