package chat

// MW5 admission tests for the stream path (design/01 §7 W4 gates, N2): the
// lease spans the WHOLE stream (acquire before the first byte, release after
// stream end), the charge comes from the stream-END usage (C1), an aborted
// stream without usage stays uncharged, acquire==wire holds across failover,
// and a rejection is terminal (no wire contact, no failover, generic event).

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
)

// streamRecAdmitter delegates to a real dispatcher and records every Acquire;
// an optional scripted error rejects instead.
type streamRecAdmitter struct {
	d      *dispatch.Dispatcher
	mu     sync.Mutex
	reqs   []dispatch.Request
	reject error
}

func newStreamRecAdmitter(t *testing.T) *streamRecAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &streamRecAdmitter{d: d}
}

func (r *streamRecAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	rej := r.reject
	r.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return r.d.Acquire(ctx, req)
}

func (r *streamRecAdmitter) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.reqs)
}

// eventSink records events and signals the first delta (the "first byte at
// the client" moment for the lease-span probe).
type eventSink struct {
	mu     sync.Mutex
	events []string
	data   []map[string]any
	delta  chan struct{}
	once   sync.Once
}

func newEventSink() *eventSink { return &eventSink{delta: make(chan struct{})} }

func (s *eventSink) Event(typ string, data any) error {
	s.mu.Lock()
	s.events = append(s.events, typ)
	if m, ok := data.(map[string]any); ok {
		s.data = append(s.data, m)
	} else {
		s.data = append(s.data, nil)
	}
	s.mu.Unlock()
	if typ == "delta" {
		s.once.Do(func() { close(s.delta) })
	}
	return nil
}

func (s *eventSink) last(typ string) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.events) - 1; i >= 0; i-- {
		if s.events[i] == typ {
			return s.data[i]
		}
	}
	return nil
}

func streamBackend(name, host string) backends.Backend {
	return backends.Backend{Name: name, Host: host, Model: "m", Trust: backends.TrustFull, Roles: []string{"chat"}, Enabled: true}
}

func streamEngine() *Engine {
	return &Engine{cfg: Config{}.withDefaults(), now: time.Now}
}

func interactiveStreamAdmission(t *testing.T, a dispatch.Admitter) llm.Admission {
	t.Helper()
	// MW4: interactive binds via the principal hook (ctx-derived), not a field.
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: "key-s", TenantID: "t1", HomeScope: "scope-s"}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	return llm.Admission{Admitter: a, Class: dispatch.ClassInteractive}
}

const sseTail = "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":6}}\n\n" +
	"data: [DONE]\n\n"

func sseDelta(text string) string {
	return fmt.Sprintf("data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":%q},\"finish_reason\":null}]}\n\n", text)
}

// TestRunStreamLeaseSpansStreamAndChargesEndUsage: the lease is held from
// before the first byte to stream end; the stream-END usage charges into the
// fairness bucket at Release (C1).
func TestRunStreamLeaseSpansStreamAndChargesEndUsage(t *testing.T) {
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("first"))
		w.(http.Flusher).Flush()
		<-gate // hold the stream open mid-flight
		_, _ = fmt.Fprint(w, sseDelta("second")+sseTail)
	}))
	defer srv.Close()

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	adm := interactiveStreamAdmission(t, d)
	e := streamEngine()
	sink := newEventSink()

	outcome := make(chan streamOutcome, 1)
	go func() {
		outcome <- e.runStream(context.Background(), []backends.Backend{streamBackend("b", srv.URL)},
			[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic, adm, sink)
	}()

	<-sink.delta // first byte reached the client
	inflight := func() int {
		n := 0
		for _, ts := range d.Snapshot().Targets {
			n += ts.Inflight
		}
		return n
	}
	if got := inflight(); got != 1 {
		t.Fatalf("inflight leases mid-stream = %d, want 1 (lease spans the stream)", got)
	}
	close(gate)

	so := <-outcome
	if !so.served || so.err != nil {
		t.Fatalf("outcome = served %v err %v", so.served, so.err)
	}
	if got := inflight(); got != 0 {
		t.Fatalf("inflight leases after stream end = %d, want 0 (released)", got)
	}
	snap := d.Snapshot()
	if snap.UnchargedCalls != 0 {
		t.Fatalf("uncharged calls = %d, want 0", snap.UnchargedCalls)
	}
	var tokens int64
	for _, ts := range snap.Targets {
		for _, b := range ts.Buckets {
			if b.FairKey == "scope-s" {
				tokens += b.Tokens
			}
		}
	}
	if tokens != 17 {
		t.Fatalf("charged tokens = %d, want 17 (stream-end usage 11+6)", tokens)
	}
}

// TestRunStreamAbortMidStreamReleasesUncharged: a stream that dies before its
// usage frame releases the lease and charges NOTHING (C1: visibility via the
// uncharged counter instead of an estimate).
func TestRunStreamAbortMidStreamReleasesUncharged(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("partial"))
		w.(http.Flusher).Flush()
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			panic(err)
		}
		_ = conn.Close() // hard mid-stream death, no finish_reason, no usage
	}))
	defer srv.Close()

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	e := streamEngine()

	so := e.runStream(context.Background(), []backends.Backend{streamBackend("b", srv.URL)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic,
		interactiveStreamAdmission(t, d), newEventSink())
	if so.served || so.err == nil {
		t.Fatalf("outcome = served %v err %v, want unserved error", so.served, so.err)
	}
	snap := d.Snapshot()
	for _, ts := range snap.Targets {
		if ts.Inflight != 0 {
			t.Fatalf("inflight after abort = %d, want 0 (released)", ts.Inflight)
		}
		for _, b := range ts.Buckets {
			if b.Tokens != 0 {
				t.Fatalf("charged tokens after abort = %d, want 0", b.Tokens)
			}
		}
	}
	if snap.UnchargedCalls != 1 {
		t.Fatalf("uncharged calls = %d, want 1 (aborted stream without usage)", snap.UnchargedCalls)
	}
}

// TestRunStreamAcquireEqualsWireCalls: the pre-first-byte failover acquires
// again for the next link — acquire count == wire-call count.
func TestRunStreamAcquireEqualsWireCalls(t *testing.T) {
	var downHits, upHits int
	var mu sync.Mutex
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		downHits++
		mu.Unlock()
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		upHits++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("ok")+sseTail)
	}))
	defer up.Close()

	rec := newStreamRecAdmitter(t)
	e := streamEngine()
	so := e.runStream(context.Background(),
		[]backends.Backend{streamBackend("down", down.URL), streamBackend("up", up.URL)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic,
		interactiveStreamAdmission(t, rec), newEventSink())
	if !so.served || so.backend.Name != "up" {
		t.Fatalf("outcome = served %v backend %s", so.served, so.backend.Name)
	}
	mu.Lock()
	defer mu.Unlock()
	if rec.count() != 2 || downHits != 1 || upHits != 1 {
		t.Fatalf("acquires/wire = %d vs %d+%d, want 2 vs 1+1", rec.count(), downHits, upHits)
	}
}

// TestRunStreamRejectionIsTerminal pins the §4.3 doctrine on the stream path:
// a rejected acquire makes NO wire contact, never spills onto the next chain
// link, and finishUnserved launders it into the generic saturation event
// (the real 429/SSE mapping is MW8's).
func TestRunStreamRejectionIsTerminal(t *testing.T) {
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer srv.Close()

	rec := newStreamRecAdmitter(t)
	rec.reject = dispatch.ErrPrincipalSaturated
	e := streamEngine()
	sink := newEventSink()
	so := e.runStream(context.Background(),
		[]backends.Backend{streamBackend("b1", srv.URL), streamBackend("b2", srv.URL)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic,
		interactiveStreamAdmission(t, rec), sink)
	if so.served || !llm.IsAdmissionError(so.err) || !errors.Is(so.err, dispatch.ErrPrincipalSaturated) {
		t.Fatalf("outcome = served %v err %v, want terminal admission error", so.served, so.err)
	}
	mu.Lock()
	if hits != 0 {
		mu.Unlock()
		t.Fatalf("wire hits after rejection = %d, want 0 (terminal, no failover)", hits)
	}
	mu.Unlock()
	if rec.count() != 1 {
		t.Fatalf("acquires = %d, want 1 (no second-link acquire)", rec.count())
	}

	// finishUnserved must NOT laundering-Classify the sentinel (B6: generic).
	if err := e.finishUnserved(context.Background(), &store.ChatSession{}, so, backends.SensPublic, time.Now(), 1, sink); err != nil {
		t.Fatalf("finishUnserved: %v", err)
	}
	ev := sink.last("error")
	if ev == nil || ev["code"] != "saturated" || ev["retryable"] != true {
		t.Fatalf("error event = %v, want code saturated retryable true", ev)
	}
}

// TestRunStreamWaitFreeTelemetrySplit is the §4.4a stream probe (MW11,
// design/05 A5-W5 gate): a held slot forces a real queue wait W — the
// web-chat row must carry queue_wait_ms == ~W while duration_ms stays the
// wait-free stream span (the clock starts AFTER admission), NOT ≥ W. The
// entry is captured via the recordEntry seam — no DB.
func TestRunStreamWaitFreeTelemetrySplit(t *testing.T) {
	var mu sync.Mutex
	var entries []llmlog.Entry
	orig := recordEntry
	recordEntry = func(_ *pgxpool.Pool, e llmlog.Entry) {
		mu.Lock()
		entries = append(entries, e)
		mu.Unlock()
	}
	t.Cleanup(func() { recordEntry = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("hi")+sseTail)
	}))
	defer srv.Close()

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(srv.URL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})
	adm := interactiveStreamAdmission(t, d)

	// Hold the single slot so the stream acquire has to queue.
	holder, _, err := d.Acquire(context.Background(), dispatch.Request{
		Target: dispatch.Target{Origin: srv.URL}, Class: dispatch.ClassInteractive, Role: "chat",
	})
	if err != nil {
		t.Fatalf("holder acquire: %v", err)
	}

	e := streamEngine()
	outcome := make(chan streamOutcome, 1)
	go func() {
		outcome <- e.runStream(context.Background(), []backends.Backend{streamBackend("b", srv.URL)},
			[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic, adm, newEventSink())
	}()

	waiting := func() bool {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == origin && ts.Interactive.Waiting == 1 {
				return true
			}
		}
		return false
	}
	deadline := time.Now().Add(5 * time.Second)
	for !waiting() {
		if time.Now().After(deadline) {
			t.Fatal("stream acquire never queued")
		}
		time.Sleep(2 * time.Millisecond)
	}
	const holdMs = 60
	time.Sleep(holdMs * time.Millisecond)
	holder.Release()

	so := <-outcome
	if !so.served || so.err != nil {
		t.Fatalf("outcome = served %v err %v", so.served, so.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 1 {
		t.Fatalf("recorded entries = %d, want 1 (one row per physical attempt)", len(entries))
	}
	entry := entries[0]
	if entry.QueueWaitMs == nil || *entry.QueueWaitMs < holdMs-10 {
		t.Errorf("queue_wait_ms = %v, want >= ~%d (the forced wait)", entry.QueueWaitMs, holdMs-10)
	}
	if entry.QueueWaitMs != nil && entry.Duration.Milliseconds() >= *entry.QueueWaitMs {
		t.Errorf("duration = %v >= wait %dms — duration_ms must be wait-free (§4.4a)", entry.Duration, *entry.QueueWaitMs)
	}
	if entry.DispatchClass != dispatch.ClassInteractive.String() {
		t.Errorf("dispatch_class = %q, want interactive", entry.DispatchClass)
	}
	if entry.DispatchAbort != "" {
		t.Errorf("dispatch_abort = %q, want empty (the dispatcher never cancels interactive leases)", entry.DispatchAbort)
	}
}

// TestRunStreamZeroWaitIsARealValue pins B-R4 on the stream row: a
// pass-through admission records queue_wait_ms 0 via pointer — never nil —
// so immediate admissions stay inside every percentile aggregation.
func TestRunStreamZeroWaitIsARealValue(t *testing.T) {
	var mu sync.Mutex
	var entries []llmlog.Entry
	orig := recordEntry
	recordEntry = func(_ *pgxpool.Pool, e llmlog.Entry) {
		mu.Lock()
		entries = append(entries, e)
		mu.Unlock()
	}
	t.Cleanup(func() { recordEntry = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, sseDelta("hi")+sseTail)
	}))
	defer srv.Close()

	d := dispatch.New(nil, dispatch.DefaultSettings()) // pass-through: no policy
	t.Cleanup(d.Close)
	adm := interactiveStreamAdmission(t, d)
	e := streamEngine()
	so := e.runStream(context.Background(), []backends.Backend{streamBackend("b", srv.URL)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key-s", backends.SensPublic, adm, newEventSink())
	if !so.served || so.err != nil {
		t.Fatalf("outcome = served %v err %v", so.served, so.err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(entries) != 1 || entries[0].QueueWaitMs == nil {
		t.Fatalf("entries = %d / wait %v, want 1 row with a NON-nil zero wait (B-R4)", len(entries), entries)
	}
	if *entries[0].QueueWaitMs > 50 {
		t.Fatalf("pass-through wait = %dms, want ~0", *entries[0].QueueWaitMs)
	}
}
