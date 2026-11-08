//go:build integration

package handler

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
)

// openPeer starts one request on key with the hop headers of instance id
// behind door → PeerIngress (peer stage wired) → stream, and returns once it
// runs behind both.
func (e *ifEnv) openPeer(t *testing.T, key modelkey.Key, from fedhop.Identity) {
	t.Helper()
	refused := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(e.reqs, http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = ifClient + ":40000"
		r.TLS = &tls.ConnectionState{}
		r.Header.Set("Authorization", "Bearer "+key.Plaintext())
		r.Header.Set(fedhop.HeaderHop, from.MintToken())
		r.Header.Set(fedhop.HeaderPath, from.ID)
		e.h.ServeHTTP(rec, r)
		refused <- rec.Code
	}()
	select {
	case <-e.stream.started:
	case code := <-refused:
		t.Fatalf("door or PeerIngress answered %d instead of reaching the stream", code)
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the stream")
	}
}

// TestInflightRevokeEndsBoundPeer is F-W12's W01-8 pin (RC-D S1): a
// ctx-peer's first request binds it at the PeerIngress (TOFU) and runs on;
// revoking its key through model-key-revoke ends that running request with
// auth.ErrCredentialRevoked within the R6 bound — the binding is no second
// credential and no exemption from the in-flight registry. Red against the
// S1 stub only through its precondition: no binding is written.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestInflightRevokeEndsBoundPeer' ./internal/handler/
func TestInflightRevokeEndsBoundPeer(t *testing.T) {
	e := ifSetup(t, ifOpts{sweep: ifLongSweep})
	admin := ifServerAdmin(t, e)
	mh := mcHandler(t, e.seed, true, false)
	mh.SetInflight(e.door.Inflight())
	e.h = e.door.Handler(federation.PeerIngress(federation.PeerIngressConfig{
		Consumer: ConsumerAuthFromContext, Binder: auth.PeerBindings{Pool: e.seed},
	})(e.stream))
	c, k, key := e.consumer(t, store.ModelConsumerSpec{Kind: "ctx-peer"})
	b := fedhop.NewIdentity("00000000-0000-4000-8000-0000000000b2", bytes.Repeat([]byte{0xb2}, 32))

	e.openPeer(t, key, b)
	e.still(t, 300*time.Millisecond, "the bound peer before the revocation")
	var bound *string
	if err := e.seed.QueryRow(context.Background(), `SELECT peer_instance_id::text FROM context_model_consumers WHERE id = $1::uuid`, c.ID).Scan(&bound); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if bound == nil || *bound != b.ID {
		t.Fatalf("F-W12 precondition: binding %v after the first peer request, want %s (TOFU at the PeerIngress)", bound, b.ID)
	}
	at := time.Now()
	status, body, resp := mcCall(t, mh, admin, "model-key-revoke", map[string]any{"key_id": k.ID})
	e.revokedWithin(t, at, ifBound, "model-key-revoke of a bound peer")
	if v, ok := resp["cancelled_inflight"].(float64); status != http.StatusOK || !ok || v != 1 {
		t.Errorf("model-key-revoke: %d %s, want 200 with cancelled_inflight 1", status, body)
	}
	e.emptyAfter(t)
}
