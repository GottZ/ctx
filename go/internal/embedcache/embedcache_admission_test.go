package embedcache

// MW5 admission tests for the embed chain (design/01 §7 W4 gates, N3):
// acquire==wire coupling per attempt, the lease-free cache hit (I-D1),
// the terminal rejection doctrine (§4.3), the prompt-token charge (MW22/C1)
// and the E-U5(a) structural overtake — per-attempt acquire with a fresh
// queue position lets a younger interactive query embed pass the waiting
// backfill rest between two blocks.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/jackc/pgx/v5/pgxpool"
)

// recordingAdmitter delegates to a real dispatcher and records every Acquire
// (coupling + classification probes); an optional scripted error rejects
// instead of delegating.
type recordingAdmitter struct {
	d      *dispatch.Dispatcher
	mu     sync.Mutex
	reqs   []dispatch.Request
	reject error
}

func newRecordingAdmitter(t *testing.T) *recordingAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &recordingAdmitter{d: d}
}

func (r *recordingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	r.mu.Lock()
	r.reqs = append(r.reqs, req)
	rej := r.reject
	r.mu.Unlock()
	if rej != nil {
		return nil, nil, rej
	}
	return r.d.Acquire(ctx, req)
}

func (r *recordingAdmitter) recorded() []dispatch.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]dispatch.Request, len(r.reqs))
	copy(out, r.reqs)
	return out
}

// interactiveAdmission binds a pass-through interactive admission and
// installs the MW4 principal hook with a non-empty principal (the B8
// downgrade must not fire in these probes). The hook is global: every
// acquire of the test resolves the SAME principal, which is exactly the
// production shape for one request's embed+backfill sequence.
func interactiveAdmission(t *testing.T, a dispatch.Admitter, key, scope string) Admission {
	t.Helper()
	dispatch.SetPrincipalHook(func(context.Context) dispatch.Principal {
		return dispatch.Principal{ApiKeyID: key, TenantID: "t1", HomeScope: scope}
	})
	t.Cleanup(func() { dispatch.SetPrincipalHook(nil) })
	return Admission{Admitter: a, Class: dispatch.ClassInteractive}
}

// quality-gate-passing raw vector for the fake wire (alternating 0/2 has
// norm > 0 and variance above the floor after L2 normalization).
func fakeRawVector() []float64 {
	vec := make([]float64, embed.TargetDims)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	return vec
}

// embedServer serves the ollama /api/embed wire shape. Every request records
// the embedded input text (order probe) and may block on gate(input) before
// answering. promptTokens lands as prompt_eval_count (0 = field carries 0).
type embedServer struct {
	t            *testing.T
	srv          *httptest.Server
	promptTokens int
	status       int
	gate         func(input string) // optional per-request block
	mu           sync.Mutex
	inputs       []string
}

func newEmbedServer(t *testing.T, promptTokens, status int) *embedServer {
	t.Helper()
	es := &embedServer{t: t, promptTokens: promptTokens, status: status}
	es.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		es.mu.Lock()
		es.inputs = append(es.inputs, req.Input)
		es.mu.Unlock()
		if es.gate != nil {
			es.gate(req.Input)
		}
		if es.status != http.StatusOK {
			w.WriteHeader(es.status)
			_, _ = w.Write([]byte("boom"))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"embeddings":        [][]float64{fakeRawVector()},
			"prompt_eval_count": es.promptTokens,
		})
	}))
	t.Cleanup(es.srv.Close)
	return es
}

func (es *embedServer) hits() int {
	es.mu.Lock()
	defer es.mu.Unlock()
	return len(es.inputs)
}

func (es *embedServer) recordedInputs() []string {
	es.mu.Lock()
	defer es.mu.Unlock()
	out := make([]string, len(es.inputs))
	copy(out, es.inputs)
	return out
}

func embedBackend(name, host string) backends.Backend {
	return backends.Backend{
		ID:   name + "-id",
		Name: name,
		Host: host,
		ModelMap: map[string]backends.ModelSpec{
			"default": {Model: "test-embed"},
		},
	}
}

// waitFor polls cond until true or the deadline fails the test.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}

// TestEmbedChainAcquireEqualsWireCalls pins the W4 coupling gate on the embed
// path: one Acquire per wire ATTEMPT on the attempt's origin — the failover
// walk acquires again for the next link.
func TestEmbedChainAcquireEqualsWireCalls(t *testing.T) {
	down := newEmbedServer(t, 0, http.StatusBadGateway) // 502 = Next()-class, fails over
	up := newEmbedServer(t, 7, http.StatusOK)
	rec := newRecordingAdmitter(t)
	adm := interactiveAdmission(t, rec, "k1", "scope-a")

	var reports []backends.ErrClass
	report := func(_ string, class backends.ErrClass, _ time.Duration) { reports = append(reports, class) }

	chain := []backends.Backend{embedBackend("down", down.srv.URL), embedBackend("up", up.srv.URL)}
	vec, served, attempts, wired, err := EmbedChain(context.Background(), nil, chain,
		"embed", "coupling probe", embed.PrefixQuery, report, adm)
	if err != nil {
		t.Fatalf("EmbedChain: %v", err)
	}
	if len(vec) != embed.TargetDims || served == nil || served.Name != "up" || attempts != 2 || !wired {
		t.Fatalf("outcome = served %v attempts %d wired %v", served, attempts, wired)
	}
	reqs := rec.recorded()
	if len(reqs) != 2 {
		t.Fatalf("acquires = %d, want 2 (one per wire attempt)", len(reqs))
	}
	if down.hits() != 1 || up.hits() != 1 {
		t.Fatalf("wire hits = %d/%d, want 1/1 (acquire==wire)", down.hits(), up.hits())
	}
	for i, srvURL := range []string{down.srv.URL, up.srv.URL} {
		want, _ := dispatch.NormalizeOrigin(srvURL)
		got, _ := dispatch.NormalizeOrigin(reqs[i].Target.Origin)
		if got != want {
			t.Errorf("acquire %d origin = %q, want %q", i, got, want)
		}
		if reqs[i].Class != dispatch.ClassInteractive || reqs[i].Role != "embed" {
			t.Errorf("acquire %d = class %s role %s", i, reqs[i].Class, reqs[i].Role)
		}
	}
	if len(reports) != 2 || reports[1] != backends.ClassOK {
		t.Errorf("health reports = %v, want [<failure> ok]", reports)
	}
}

// TestEmbedChainCacheHitAcquiresNoLease is the I-D1 cache-hit negative probe:
// a hit contacts no backend AND acquires no lease (wired=false, zero wire
// hits, zero Acquire calls).
func TestEmbedChainCacheHitAcquiresNoLease(t *testing.T) {
	srv := newEmbedServer(t, 0, http.StatusOK)
	rec := newRecordingAdmitter(t)
	adm := interactiveAdmission(t, rec, "k1", "scope-a")

	cached := make([]float32, embed.TargetDims)
	restore := SetCacheProbeForTest(func(context.Context, *pgxpool.Pool, []byte, string) ([]float32, bool) {
		return cached, true
	})
	defer restore()

	vec, served, attempts, wired, err := EmbedChain(context.Background(), nil,
		[]backends.Backend{embedBackend("b", srv.srv.URL)},
		"embed", "cached text", embed.PrefixQuery, nil, adm)
	if err != nil {
		t.Fatalf("EmbedChain: %v", err)
	}
	if wired || served != nil || attempts != 0 || len(vec) != embed.TargetDims {
		t.Fatalf("cache hit outcome = wired %v served %v attempts %d", wired, served, attempts)
	}
	if got := len(rec.recorded()); got != 0 {
		t.Fatalf("acquires on cache hit = %d, want 0 (I-D1 cache clause)", got)
	}
	if srv.hits() != 0 {
		t.Fatalf("wire hits on cache hit = %d, want 0", srv.hits())
	}
}

// TestEmbedChainRejectionIsTerminal pins the §4.3 acquire-error doctrine on
// the embed path: a rejected acquire is NOT an attempt — no wire contact, no
// health report, wired=false (the caller's llmlog gate writes no row), and
// the sentinel stays errors.Is-reachable for the MW8 handler mapping.
func TestEmbedChainRejectionIsTerminal(t *testing.T) {
	srv := newEmbedServer(t, 0, http.StatusOK)
	rec := newRecordingAdmitter(t)
	rec.reject = dispatch.ErrQueueFull
	adm := interactiveAdmission(t, rec, "k1", "scope-a")

	reportCalls := 0
	report := func(string, backends.ErrClass, time.Duration) { reportCalls++ }

	_, served, attempts, wired, err := EmbedChain(context.Background(), nil,
		[]backends.Backend{embedBackend("b", srv.srv.URL), embedBackend("b2", srv.srv.URL)},
		"embed", "rejected text", embed.PrefixQuery, report, adm)
	if err == nil || !errors.Is(err, dispatch.ErrQueueFull) || !dispatch.IsRejection(err) {
		t.Fatalf("err = %v, want wrapped dispatch.ErrQueueFull rejection", err)
	}
	if wired || served != nil || attempts != 0 {
		t.Fatalf("rejection outcome = wired %v served %v attempts %d, want no attempt", wired, served, attempts)
	}
	if srv.hits() != 0 {
		t.Fatalf("wire hits after rejection = %d, want 0 (terminal, no failover)", srv.hits())
	}
	if reportCalls != 0 {
		t.Fatalf("health reports after rejection = %d, want 0", reportCalls)
	}
}

// TestEmbedChainChargesPromptTokens pins the MW22/C1 charge: a successful
// embed books its backend-reported prompt tokens into the caller's fairness
// bucket at Release (completion side zero by nature of the call).
func TestEmbedChainChargesPromptTokens(t *testing.T) {
	srv := newEmbedServer(t, 42, http.StatusOK)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	adm := interactiveAdmission(t, d, "k1", "scope-charge")

	_, _, _, wired, err := EmbedChain(context.Background(), nil,
		[]backends.Backend{embedBackend("b", srv.srv.URL)},
		"embed", "charge probe", embed.PrefixQuery, nil, adm)
	if err != nil || !wired {
		t.Fatalf("EmbedChain: wired %v err %v", wired, err)
	}

	snap := d.Snapshot()
	if snap.UnchargedCalls != 0 {
		t.Fatalf("uncharged calls = %d, want 0", snap.UnchargedCalls)
	}
	var tokens int64
	for _, ts := range snap.Targets {
		for _, b := range ts.Buckets {
			if b.FairKey == "scope-charge" {
				tokens += b.Tokens
			}
		}
	}
	if tokens != 42 {
		t.Fatalf("charged tokens = %d, want 42 (prompt_eval_count)", tokens)
	}
}

// TestEmbedChainBackfillOvertake is the E-U5(a) structural probe: on a 1-slot
// target a sequential backfill-style consumer (one EmbedChain per block,
// fresh acquire each) holds NO standing reservation — a younger interactive
// query embed that enqueues during block 1 is served BEFORE the backfill's
// block 2 reaches the wire.
func TestEmbedChainBackfillOvertake(t *testing.T) {
	srv := newEmbedServer(t, 0, http.StatusOK)
	block1Gate := make(chan struct{})
	srv.gate = func(input string) {
		if strings.Contains(input, "block1") {
			<-block1Gate
		}
	}

	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	origin, err := dispatch.NormalizeOrigin(srv.srv.URL)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{origin: {Slots: 1}}})

	chain := []backends.Backend{embedBackend("b", srv.srv.URL)}
	admBackfill := interactiveAdmission(t, d, "key-backfill", "scope-bf")
	admQuery := interactiveAdmission(t, d, "key-query", "scope-q")

	embedOne := func(adm Admission, text string) error {
		_, _, _, _, err := EmbedChain(context.Background(), nil, chain, "embed", text, embed.PrefixDocument, nil, adm)
		return err
	}

	backfillDone := make(chan error, 1)
	go func() {
		// The backfill rest: block2 follows block1 on the SAME goroutine —
		// exactly the query.go/scheduler loop shape.
		if err := embedOne(admBackfill, "block1"); err != nil {
			backfillDone <- fmt.Errorf("block1: %w", err)
			return
		}
		backfillDone <- embedOne(admBackfill, "block2")
	}()

	// block1 is admitted and on the wire (holding the single slot).
	waitFor(t, "block1 on the wire", func() bool { return srv.hits() == 1 })

	queryDone := make(chan error, 1)
	go func() { queryDone <- embedOne(admQuery, "query-embed") }()

	// The query embed is enqueued behind the held slot.
	waitFor(t, "query embed waiting", func() bool {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == origin && ts.Interactive.Waiting == 1 {
				return true
			}
		}
		return false
	})

	close(block1Gate) // block1 finishes, slot hands off
	if err := <-queryDone; err != nil {
		t.Fatalf("query embed: %v", err)
	}
	if err := <-backfillDone; err != nil {
		t.Fatalf("backfill: %v", err)
	}

	inputs := srv.recordedInputs()
	if len(inputs) != 3 {
		t.Fatalf("wire calls = %d, want 3", len(inputs))
	}
	order := make([]string, len(inputs))
	for i, in := range inputs {
		switch {
		case strings.Contains(in, "block1"):
			order[i] = "block1"
		case strings.Contains(in, "query-embed"):
			order[i] = "query"
		case strings.Contains(in, "block2"):
			order[i] = "block2"
		}
	}
	want := []string{"block1", "query", "block2"}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("wire order = %v, want %v (younger query overtakes the backfill rest)", order, want)
		}
	}
}
