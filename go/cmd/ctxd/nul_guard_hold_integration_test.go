//go:build integration

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// stalledBody is a request body that delivers head and then stalls until
// release, as a connection that declared a long body and stopped sending. At
// the stalled read it notes what the reader holds for the request: the bytes
// it received before plus the capacity of the buffer it offers for the next
// ones (cap of p — the guard's own buffer, which the body cap's reader passes
// through shortened in length only).
type stalledBody struct {
	head    []byte
	sent    int
	held    int
	stalled chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *stalledBody) Read(p []byte) (int, error) {
	if b.sent < len(b.head) {
		n := copy(p, b.head[b.sent:])
		b.sent += n
		return n, nil
	}
	b.once.Do(func() {
		b.held = b.sent + cap(p)
		close(b.stalled)
	})
	<-b.release
	return 0, io.ErrUnexpectedEOF // the connection's read deadline ends it
}

// TestNULGuardHoldsWhatArrived (issue #519, review of PR #553): the NUL guard
// reads a body before its route's handler runs. It reserved a buffer of the
// DECLARED length (Content-Length) before a byte had arrived, so a connection
// that declared a large body, sent a few bytes and stalled held the whole
// declared buffer until the server's read timeout — on the webhook route
// before any authentication. What the guard holds must grow with the bytes
// that arrived: at most twice them plus a first chunk.
//
// The measure is exact: the capacity the guard offers the body at the stall
// (stalledBody.held), per request, through the production router.
func TestNULGuardHoldsWhatArrived(t *testing.T) {
	f := nulSweepRouter(t, nil)
	const (
		declared   = 1 << 20  // the webhook group's cap (DefaultMaxBodySize)
		conns      = 200      // stalled requests at once
		firstChunk = 64 << 10 // bound for what the guard may hold before data
	)
	for _, tc := range []struct {
		name     string
		received int
	}{
		{"a few bytes", 25},
		{"a third of the body", 300 << 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			head := []byte(`{"action":"opened","x":"` + strings.Repeat("a", max(0, tc.received-24)))[:tc.received]
			release := make(chan struct{})
			bodies := make([]*stalledBody, conns)
			var done sync.WaitGroup
			statuses := make([]int, conns)
			for i := range bodies {
				b := &stalledBody{head: head, stalled: make(chan struct{}), release: release}
				bodies[i] = b
				req := httptest.NewRequest(http.MethodPost, "/webhooks/github/00000000-0000-0000-0000-000000000001", b)
				req.ContentLength = declared
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-GitHub-Event", "issues")
				req.RemoteAddr = "192.0.2.11:4000"
				done.Add(1)
				go func(i int) {
					defer done.Done()
					rec := httptest.NewRecorder()
					f.router.ServeHTTP(rec, req)
					statuses[i] = rec.Code
				}(i)
			}
			total := 0
			worst := 0
			for _, b := range bodies {
				<-b.stalled
				total += b.held
				worst = max(worst, b.held)
			}
			close(release)
			done.Wait()
			bound := 2*tc.received + firstChunk
			t.Logf("%d stalled requests, %d bytes received each of %d declared: the guard holds %d bytes in all (at most %d for one; bound %d)",
				conns, tc.received, declared, total, worst, bound)
			if worst > bound {
				t.Errorf("a request that sent %d of %d declared bytes holds %d bytes in the guard, want at most %d (twice the received bytes plus a first chunk)",
					tc.received, declared, worst, bound)
			}
			for i, s := range statuses {
				if s == http.StatusOK || s >= 500 {
					t.Errorf("request %d: status %d for a body that ended in a read error", i, s)
					break
				}
			}
		})
	}
}
