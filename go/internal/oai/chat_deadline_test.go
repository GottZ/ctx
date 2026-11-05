package oai_test

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// dlWriter records every write deadline the route sets (the method
// http.ResponseController looks for).
type dlWriter struct {
	http.ResponseWriter
	mu  sync.Mutex
	dls []time.Time
}

func (w *dlWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.dls = append(w.dls, t)
	w.mu.Unlock()
	return nil
}

// TestWriteDeadlinesCarryTheMargin (Fremd-Review 3 R3-4 / review 2 D11, D5;
// design/02 §4.12): the deadline at entry is entry + MaxWait +
// request_deadline + 30 s, the one at the admission now + the rest of the
// request frame + 30 s — the margin an answer ready at the end of the frame
// has to reach the socket. A margin of a few milliseconds, no margin or a
// missing MaxWait term is red.
func TestWriteDeadlinesCarryTheMargin(t *testing.T) {
	const (
		maxWait  = 20 * time.Second // the rig's MaxWait
		deadline = 2 * time.Second
		margin   = 30 * time.Second
		slack    = time.Second
	)
	u := newUpstream(t, answerSGLang)
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = deadline })
	var rec *dlWriter
	h := oai.ChatCompletions(rig.deps)
	rig.router = rig.mount(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec = &dlWriter{ResponseWriter: w}
		h.ServeHTTP(rec, r)
	}))
	start := time.Now()
	if a := rig.post(t, chatBody("chat")); a.status != http.StatusOK {
		t.Fatalf("answer %d %s, want 200", a.status, a.body)
	}
	end := time.Now()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.dls) != 2 {
		t.Fatalf("write deadlines set: %d (%v), want 2 (entry, admission)", len(rec.dls), rec.dls)
	}
	if lo, hi := start.Add(maxWait+deadline+margin), end.Add(maxWait+deadline+margin); rec.dls[0].Before(lo) || rec.dls[0].After(hi) {
		t.Errorf("entry deadline = start + %v, want entry + MaxWait + request_deadline + 30 s = %v", rec.dls[0].Sub(start), maxWait+deadline+margin)
	}
	if lo, hi := start.Add(margin), end.Add(deadline+margin+slack); rec.dls[1].Before(lo) || rec.dls[1].After(hi) {
		t.Errorf("admission deadline = start + %v, want now + rest of the frame (≤ %v) + 30 s", rec.dls[1].Sub(start), deadline)
	}
}
