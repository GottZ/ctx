package oai_test

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// TestRevokedKeyEndsEmbeddings (W01-8): an embeddings request whose key
// stops authenticating while the engine works — the door's registry ends
// its request context with auth.ErrCredentialRevoked (the revoker stands in
// for it) — answers 401 invalid_api_key with WWW-Authenticate at once, the
// engine call aborted, the lease free. Red: the route's verdict without the
// ErrCredentialRevoked branch reads the canceled request context as a gone
// client and answers nothing (an empty 200).
func TestRevokedKeyEndsEmbeddings(t *testing.T) {
	e := newEmbedEngine(t, 8)
	gate := make(chan struct{})
	var gateOnce sync.Once
	defer gateOnce.Do(func() { close(gate) })
	e.setRespond(func(_ http.ResponseWriter, r *http.Request, _ int64, _ []string) bool {
		select {
		case <-gate:
		case <-r.Context().Done():
		}
		return false
	})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
	rv := newRevoker()
	srv := serveHTTP(t, rv.wrap(rig.router))
	done := make(chan chatAnswer, 1)
	go func() {
		req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+"/v1/embeddings", strings.NewReader(embedBody("emb", embedInputs(2))))
		req.Header.Set("Content-Type", "application/json")
		resp, err := srv.Client().Do(req)
		if err != nil {
			done <- chatAnswer{status: -1}
			return
		}
		defer func() { _ = resp.Body.Close() }()
		var b strings.Builder
		buf := make([]byte, 4096)
		for {
			n, rerr := resp.Body.Read(buf)
			b.WriteString(string(buf[:n]))
			if rerr != nil {
				break
			}
		}
		done <- chatAnswer{status: resp.StatusCode, header: resp.Header, body: []byte(b.String())}
	}()
	if !waitFor(5*time.Second, func() bool { return e.hits.Load() >= 1 }) {
		t.Fatal("the embeddings request did not reach the engine")
	}
	at := time.Now()
	if n := rv.revoke(); n != 1 {
		t.Fatalf("revoked %d running requests, want 1", n)
	}
	var a chatAnswer
	select {
	case a = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the embeddings request still runs after the revocation")
	}
	t.Logf("revocation → %d after %v: %s", a.status, time.Since(at).Round(time.Millisecond), strings.TrimSpace(string(a.body)))
	if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") == "" || !strings.Contains(string(a.body), `"code":"invalid_api_key"`) {
		t.Fatalf("answer %d %v %s, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.body)
	}
	if !waitHeld(rig.disp, e.srv.URL, 0, 2*time.Second) {
		t.Error("the lease is still held")
	}
}
