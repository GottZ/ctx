package rerank

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/ssrfguard"
)

// F-W1 Gate (a), rerank wire (design/04 I17, S5): a 307 to another origin
// is returned as StatusError{307} ⇒ ClassBadRequest on both pool clients;
// the foreign host sees neither the query nor the documents.

func TestRerankWireRefusesCrossOriginRedirect(t *testing.T) {
	for _, scope := range []string{backends.GlobalScope, "acme"} {
		client := "open"
		if scope != backends.GlobalScope {
			client = "guarded"
		}
		t.Run(client, func(t *testing.T) {
			var mu sync.Mutex
			var seen []string
			foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				mu.Lock()
				seen = append(seen, fmt.Sprintf("%s %s authorization=%q body=%s", r.Method, r.URL.Path, r.Header.Get("Authorization"), b))
				mu.Unlock()
				_, _ = io.WriteString(w, `{"results":[{"index":0,"relevance_score":1.5}]}`)
			}))
			t.Cleanup(foreign.Close)
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Location", foreign.URL+r.URL.Path)
				w.WriteHeader(http.StatusTemporaryRedirect)
			}))
			t.Cleanup(origin.Close)
			if scope != backends.GlobalScope {
				admitGuardedLoopback(t, origin, foreign)
			}

			b := backends.Backend{Host: origin.URL, Scope: scope, APIKey: "row-key"}
			_, _, err := Score(context.Background(), b, "m", "QUERY-MARKER-F-W1", []string{"DOC-MARKER-F-W1"}, "")

			mu.Lock()
			n, log := len(seen), strings.Join(seen, " | ")
			mu.Unlock()
			if n != 0 {
				t.Errorf("the foreign host saw %d request(s): %s", n, log)
			}
			var se *httpx.StatusError
			if !errors.As(err, &se) || se.Code != http.StatusTemporaryRedirect {
				t.Fatalf("err = %v, want StatusError{307}", err)
			}
			if c := backends.Classify(err, ""); c != backends.ClassBadRequest {
				t.Errorf("Classify = %v, want bad_request", c)
			}
		})
	}
}

// admitGuardedLoopback: see the llm package's twin — the guarded client may
// dial exactly these loopback listeners (stand-ins for public hosts); every
// other address goes through the real ssrfguard.DialControl, and the
// client's CheckRedirect stays the production one.
func admitGuardedLoopback(t *testing.T, srvs ...*httptest.Server) {
	t.Helper()
	g := httpClients.For(true)
	orig := g.Transport
	allow := map[string]bool{}
	for _, s := range srvs {
		allow[s.Listener.Addr().String()] = true
	}
	tr := orig.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 5 * time.Second, Control: func(network, address string, c syscall.RawConn) error {
		if allow[address] {
			return nil
		}
		return ssrfguard.DialControl(network, address, c)
	}}
	tr.DialContext = d.DialContext
	g.Transport = tr
	t.Cleanup(func() {
		tr.CloseIdleConnections()
		g.Transport = orig
	})
}
