package oai_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/oai"
)

// TestRevokedKeyEndsRerank (W01-8 landing, review 2 M1): a rerank request
// whose key stops authenticating while its block is on the engine — the
// door's registry ends its request context with auth.ErrCredentialRevoked
// (the revoker stands in for it) — answers 401 invalid_api_key with
// WWW-Authenticate before any byte of an answer, on both dialects; the
// engine call is aborted, the lease free, the meter's reservation ended
// exactly once (keep: the block had wire contact, no answer was written)
// and the consumer's one L1 slot free exactly once. Red: the route's
// verdict without the ErrCredentialRevoked branch reads the canceled
// request context as a gone client and writes nothing — an empty 200.
func TestRevokedKeyEndsRerank(t *testing.T) {
	for _, path := range []string{"/v1/rerank", "/v2/rerank"} {
		t.Run(strings.TrimPrefix(path, "/"), func(t *testing.T) {
			e := newRerankEngine(t)
			gate := make(chan struct{})
			var gateOnce sync.Once
			defer gateOnce.Do(func() { close(gate) })
			aborted := make(chan struct{}, 1)
			e.setRespond(func(_ http.ResponseWriter, r *http.Request, _ int64, _ rerankCall) bool {
				select {
				case <-gate:
				case <-r.Context().Done():
					aborted <- struct{}{}
				}
				return false
			})
			m := &endMeter{rigMeter: newRigMeter(keyquota.Limits{RPM: 1000, TPM: 1 << 30, CompletionReserve: 1024, Concurrency: 1}, true)}
			rig := newRerankRig(t, []backends.Backend{e.row("spark-rerank", 10)}, 2, 1, withDeps(func(d *oai.Deps) { d.Limiter = m }))
			rv := newRevoker()
			srv := serveHTTP(t, rv.wrap(rig.router))
			done := make(chan chatAnswer, 1)
			go func() {
				req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, srv.URL+path,
					strings.NewReader(rerankBody("rr", "q", rerankDocs(3))))
				req.Header.Set("Content-Type", "application/json")
				resp, err := srv.Client().Do(req)
				if err != nil {
					done <- chatAnswer{status: -1}
					return
				}
				defer func() { _ = resp.Body.Close() }()
				body, _ := io.ReadAll(resp.Body)
				done <- chatAnswer{status: resp.StatusCode, header: resp.Header, body: body}
			}()
			if !waitFor(5*time.Second, func() bool { return e.hits.Load() >= 1 }) {
				t.Fatal("the rerank block did not reach the engine")
			}
			at := time.Now()
			if n := rv.revoke(); n != 1 {
				t.Fatalf("revoked %d running requests, want 1", n)
			}
			var a chatAnswer
			select {
			case a = <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the rerank request still runs 5 s after the revocation")
			}
			took := time.Since(at)
			t.Logf("POST %s: revocation → %d after %v, %d body bytes: %s", path, a.status, took.Round(time.Millisecond), len(a.body), strings.TrimSpace(string(a.body)))
			if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") == "" || !strings.Contains(string(a.body), `"code":"invalid_api_key"`) {
				t.Fatalf("answer %d %v %q, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.body)
			}
			select {
			case <-aborted:
			case <-time.After(time.Second):
				t.Error("the engine call was not aborted")
			}
			if !waitHeld(rig.disp, e.srv.URL, 0, 2*time.Second) {
				t.Error("the lease is still held")
			}
			if !waitFor(5*time.Second, func() bool { return endCount(m) >= 1 }) {
				t.Fatalf("the reservation never ended (ends %q)", m.endsOf())
			}
			time.Sleep(150 * time.Millisecond) // a second end of the same request would land here
			if ends := m.endsOf(); ends != "keep" {
				t.Errorf("reservation ends %q, want exactly one keep", ends)
			}
			ca := *rig.ca.Load()
			s := keyquota.Subject{ConsumerID: ca.ConsumerID, TenantID: ca.TenantID, Op: keyquota.OpRerank, Lane: keyquota.LaneUrgent}
			r1, d1 := m.m.Admit(s, m.lim, keyquota.Estimate{InputRunes: 1})
			r2, d2 := m.m.Admit(s, m.lim, keyquota.Estimate{InputRunes: 1})
			for _, r := range []*keyquota.Reservation{r1, r2} {
				if r != nil {
					r.Abort()
				}
			}
			t.Logf("L1 slot after the revocation: first allowed=%v, second allowed=%v reason=%s", d1.Allowed, d2.Allowed, d2.Reason)
			if !d1.Allowed || d2.Allowed || d2.Reason != keyquota.ReasonConcurrency {
				t.Errorf("L1 slot: first allowed=%v, second allowed=%v (%s) — want the one slot free exactly once", d1.Allowed, d2.Allowed, d2.Reason)
			}
			if !waitFor(time.Second, func() bool { return rv.running.Load() == 0 }) {
				t.Errorf("%d requests still inside the registry", rv.running.Load())
			}
		})
	}
}
