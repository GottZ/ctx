package oai_test

// The admission gates of the embeddings route (W02-09): v1census rule 7 at
// the real route, with failover (Z1); continuations on the request context
// (Z2); usage of every block (Z3, the route half); a failed continuation is
// terminal (Z5); Finding 14 — never a refusal after block ≥ 2 (G4); the
// k > 1 envelope pins; the N-1 pin (the request deadline during a
// continuation); fairness at the lease boundary and the request deadline in
// the middle of the blocks (O-09-1); the reserve before any acquire
// (O-09-3, G8). The federation hop (G11) runs in internal/chat
// (dispatch_invariant_embed_test.go): it calls embed.Embed, a wire site the
// X13 census refuses in this package outside a lease.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/handler/v1census"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// TestEmbeddingsAdmissionCensus (Z1): v1census over the real route with the
// real planner on the Recorder — 100 inputs in blocks [4, 4, …] (E = 4):
// every continuation after the First, none while the request holds a
// lease (rule 7), ExternalAdmissible and ExternalWeightCap read with b.Host.
// The failover case: block one fails on row A (502), the attempt moves to
// row B through Next and walks every block there — still 0 violations.
func TestEmbeddingsAdmissionCensus(t *testing.T) {
	body := func(string, string) string { return embedBody("emb4k", embedInputs(100)) }
	t.Run("one row", func(t *testing.T) {
		e := newEmbedEngine(t, 4096)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 5, 1)
		v1census.Assert(t, rig.router, rig.recorder, v1census.Options{
			Allowed: rig.planner.AllowedClasses(), Hosts: []string{e.srv.URL},
			Admitting: []string{"POST /v1/embeddings"}, Body: body,
		})
		if h := e.hits.Load(); h != 25 {
			t.Errorf("wire hits %d, want 25 (100 inputs in blocks of E = 4)", h)
		}
	})
	t.Run("failover", func(t *testing.T) {
		a := newEmbedEngine(t, 4096)
		b := newEmbedEngine(t, 4096)
		a.setRespond(func(w http.ResponseWriter, _ *http.Request, _ int64, _ []string) bool {
			w.WriteHeader(http.StatusBadGateway)
			return true
		})
		rig := newEmbedRig(t, []backends.Backend{a.row("row-a", backends.ProtocolOpenAI, 20), b.row("row-b", backends.ProtocolOpenAI, 10)}, 5, 1)
		v1census.Assert(t, rig.router, rig.recorder, v1census.Options{
			Allowed: rig.planner.AllowedClasses(), Hosts: []string{a.srv.URL, b.srv.URL},
			Admitting: []string{"POST /v1/embeddings"}, Body: body,
		})
		if a.hits.Load() != 1 || b.hits.Load() != 25 {
			t.Errorf("wire hits A %d B %d, want 1 and 25", a.hits.Load(), b.hits.Load())
		}
	})
}

// TestEmbeddingsContinueOnTheRequestContext (Z2): a request of several
// blocks is served — the continuations ask on the request context. On the
// released block lease's run context they would end at once with
// context.Canceled (A03-W8 §Übergaben): a 503, and never an empty 200.
func TestEmbeddingsContinueOnTheRequestContext(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	a := rig.postEmbed(t, embedBody("emb", embedInputs(6)))
	if a.status == http.StatusOK && len(a.body) == 0 {
		t.Fatal("empty 200")
	}
	if r := decodeEmbed(t, a); len(r.Data) != 6 || e.hits.Load() != 6 {
		t.Fatalf("data %d, wire hits %d, want 6/6", len(r.Data), e.hits.Load())
	}
}

// usageLimiter records the usage every reservation settles with.
type usageLimiter struct {
	*fakeLimiter
	mu      sync.Mutex
	settled []dispatch.Usage
}

type usageReservation struct {
	oai.LimitReservation
	l *usageLimiter
}

func (r usageReservation) Settle(u dispatch.Usage) {
	r.l.mu.Lock()
	r.l.settled = append(r.l.settled, u)
	r.l.mu.Unlock()
	r.LimitReservation.Settle(u)
}

func (l *usageLimiter) Admit(ctx context.Context, ca *auth.ConsumerAuth, a modelalias.Alias, e oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	res, refusal := l.fakeLimiter.Admit(ctx, ca, a, e)
	if res == nil {
		return nil, refusal
	}
	return usageReservation{LimitReservation: res, l: l}, refusal
}

// TestEmbeddingsUsageOfEveryBlock (Z3, the route half): the prompt tokens
// of all blocks — not block one's — reach the limiter's Settle, the llmlog
// row and the access record (the per-lease half: subblock_internal_test).
func TestEmbeddingsUsageOfEveryBlock(t *testing.T) {
	e := newEmbedEngine(t, 8)
	var ul *usageLimiter
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 3, 1, func(r *chatRig) {
		ul = &usageLimiter{fakeLimiter: r.limiter}
		r.deps.Limiter = ul
	})
	r := decodeEmbed(t, rig.postEmbed(t, embedBody("emb", embedInputs(7))))
	if r.Usage.PromptTokens != 14 {
		t.Errorf("answer usage %d, want 14 (7 inputs × 2 over 4 blocks)", r.Usage.PromptTokens)
	}
	if len(ul.settled) != 1 || ul.settled[0].PromptTokens != 14 {
		t.Errorf("settled %+v, want one Settle with 14 prompt tokens", ul.settled)
	}
	rows := rig.rows.all()
	if len(rows) != 1 || rows[0].Telemetry.PromptTokens != 14 || rows[0].Gateway.Usage != "backend" {
		t.Fatalf("row tokens/usage %+v, want 14 backend", rows)
	}
	if rec := <-rig.recs; rec.PromptTokens != 14 {
		t.Errorf("access record tokens %d, want 14", rec.PromptTokens)
	}
}

// contPlanner wraps the real planner: its acquirer's Continue answers err
// (after `after` granted continuations) instead of the dispatcher.
type contPlanner struct {
	oai.AdmissionPlanner
	err   error
	after int
}

func (p contPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = &contAcquirer{RequestAcquirer: plan.Acquirer, err: p.err, after: p.after}
	}
	return plan, err
}

type contAcquirer struct {
	oai.RequestAcquirer
	err   error
	after int
	n     atomic.Int64
}

func (a *contAcquirer) Continue(ctx context.Context, b *backends.Backend, role string, t time.Duration) (*dispatch.Lease, context.Context, error) {
	if int(a.n.Add(1)) > a.after {
		return nil, nil, a.err
	}
	return a.RequestAcquirer.Continue(ctx, b, role, t)
}

// TestEmbeddingsContinuationFailureIsTerminal (Z5): a refused continuation
// (block 3 of 4) ends the walk as an admission outcome — the planner's
// writer answers (429 server_busy for a rejection), metadata.chain names
// err_class admission, the row is not reported to the pool's health
// (consecutive_fails 0), no failover onto row B, the wire work done is
// settled and logged (two blocks).
func TestEmbeddingsContinuationFailureIsTerminal(t *testing.T) {
	a := newEmbedEngine(t, 8)
	b := newEmbedEngine(t, 8)
	rejection := &dispatch.AdmissionError{Err: &dispatch.RejectionError{Err: dispatch.ErrWaitExpired, RetryAfter: 2 * time.Second}}
	rig := newEmbedRig(t, []backends.Backend{a.row("row-a", backends.ProtocolOpenAI, 20), b.row("row-b", backends.ProtocolOpenAI, 10)}, 3, 1,
		func(r *chatRig) { r.deps.Planner = contPlanner{AdmissionPlanner: r.planner, err: rejection, after: 1} })
	ans := rig.postEmbed(t, embedBody("emb", embedInputs(8)))
	code, _, _ := ans.envelope(t)
	if ans.status != http.StatusTooManyRequests || code != oai.CodeServerBusy {
		t.Fatalf("%d %s, want 429 server_busy from the planner's writer", ans.status, ans.body)
	}
	if n := consecutiveFails(rig.pool, "row-a"); n != 0 {
		t.Errorf("consecutive_fails of row A = %d, want 0 (an admission outcome is no fault of the row)", n)
	}
	if b.hits.Load() != 0 || a.hits.Load() != 2 {
		t.Errorf("wire hits A %d B %d, want 2 and 0", a.hits.Load(), b.hits.Load())
	}
	rows := rig.rows.all()
	if len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Attempts[0].Class != "admission" {
		t.Fatalf("rows %+v, want one row whose attempt class is admission", rows)
	}
	if rows[0].Telemetry.Metadata["sub_batches_done"] != 2 || rows[0].Err == nil || rows[0].Err.Class != "admission" {
		t.Errorf("row metadata %v err %+v, want 2 blocks done, err class admission", rows[0].Telemetry.Metadata, rows[0].Err)
	}
	if l := rig.limiter; l.settles.Load() != 1 || l.rejects.Load() != 0 || l.aborts.Load() != 0 {
		t.Errorf("limiter settles %d rejects %d aborts %d, want 1/0/0", l.settles.Load(), l.rejects.Load(), l.aborts.Load())
	}
}

// logCapture is a slog handler collecting ERROR lines.
type logCapture struct {
	mu   sync.Mutex
	errs []string
}

func (c *logCapture) Enabled(context.Context, slog.Level) bool { return true }
func (c *logCapture) Handle(_ context.Context, r slog.Record) error {
	if r.Level >= slog.LevelError {
		c.mu.Lock()
		c.errs = append(c.errs, r.Message)
		c.mu.Unlock()
	}
	return nil
}
func (c *logCapture) WithAttrs([]slog.Attr) slog.Handler { return c }
func (c *logCapture) WithGroup(string) slog.Handler      { return c }

func (c *logCapture) all() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]string(nil), c.errs...)
}

// TestEmbeddingsEnvelopePins (K5/K29): an acquirer sentinel of First ⇒ 503
// internal_error, x-should-retry false; ErrNoContinuation from Continue
// although the plan says Continuation ⇒ 503 internal_error, x-should-retry
// false, and one ERROR line (a wiring defect, never a 400).
func TestEmbeddingsEnvelopePins(t *testing.T) {
	t.Run("first sentinel", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 3, 1,
			func(r *chatRig) {
				r.deps.Planner = chFaultPlanner{AdmissionPlanner: r.planner, err: dispatch.ErrClassUnauthorized}
			})
		a := rig.postEmbed(t, embedBody("emb", embedInputs(4)))
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
			t.Fatalf("%d %s retry=%q, want 503 internal_error x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
		}
		if e.hits.Load() != 0 || len(rig.rows.all()) != 0 {
			t.Errorf("wire hits %d rows %d, want 0/0", e.hits.Load(), len(rig.rows.all()))
		}
	})
	t.Run("ErrNoContinuation despite Continuation", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		logs := &logCapture{}
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 3, 1, func(r *chatRig) {
			p := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: r.recorder, Settings: func() oai.Settings { return *r.settings.Load() },
				Logger: slog.New(logs)})
			r.deps.Planner = contPlanner{AdmissionPlanner: p, err: llm.ErrNoContinuation}
		})
		a := rig.postEmbed(t, embedBody("emb", embedInputs(4)))
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
			t.Fatalf("%d %s retry=%q, want 503 internal_error x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
		}
		if errs := logs.all(); len(errs) != 1 {
			t.Errorf("ERROR lines %q, want exactly one", errs)
		}
		if e.hits.Load() != 1 {
			t.Errorf("wire hits %d, want 1 (block one)", e.hits.Load())
		}
	})
}

// waitWaiting waits until n requests queue for host's external partition
// in class XU (bulk: XB).
func waitWaiting(t *testing.T, d *dispatch.Dispatcher, host string, n int, bulk bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, ts := range d.Snapshot().Targets {
			w := ts.External.Waiting
			if bulk {
				w = ts.ExternalBulk.Waiting
			}
			if ts.Origin == host && w >= n {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("not %d external waiter(s) on %s within 5 s", n, host)
}

// TestEmbeddingsRequestDeadlineInAContinuation (N-1, the Ist pin until the
// user decides): the request deadline ends a continuation that waits at the
// class head — the planner's writer answers 503 internal_error with
// x-should-retry false (A03-W8 FR3-1; not 504). The pin moves with the
// decision on N-1, in one place (V1Admission.WriteAdmissionError).
func TestEmbeddingsRequestDeadlineInAContinuation(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = 400 * time.Millisecond })
	// During block one of A a foreign consumer queues for the one unit (a
	// direct acquire: no request deadline of its own); it gets the unit at
	// A's release, before A's continuation asks, and keeps it past A's
	// deadline.
	var foreign atomic.Pointer[dispatch.Lease]
	defer func() {
		if l := foreign.Load(); l != nil {
			l.Release()
		}
	}()
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		if e.hits.Load() == 1 {
			go func() {
				ctx := context.WithValue(context.Background(), consumerKey{}, foreignConsumer(urgentApp(), "n1"))
				l, _, err := rig.disp.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: e.srv.URL}, Class: dispatch.ClassExternal, Role: "embed"})
				if err == nil {
					foreign.Store(l)
				}
			}()
			waitWaiting(t, rig.disp, e.srv.URL, 1, false)
		}
		return false
	})
	start := time.Now()
	a := rig.postEmbed(t, embedBody("emb", embedInputs(2)))
	code, _, _ := a.envelope(t)
	t.Logf("answer after %v: %d %s", time.Since(start).Round(time.Millisecond), a.status, a.body)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || a.header.Get("x-should-retry") != "false" {
		t.Fatalf("%d %s retry=%q, want the Ist: 503 internal_error, x-should-retry false", a.status, a.body, a.header.Get("x-should-retry"))
	}
	rows := rig.rows.all()
	if len(rows) != 1 || rows[0].Telemetry.Metadata["sub_batches_done"] != 1 || rows[0].Telemetry.PromptTokens != 2 {
		t.Errorf("rows %+v, want one row: one block done, 2 tokens", rows)
	}
}

// TestEmbeddingsFairAtTheLeaseBoundary (O-09-1 (b)): 32 inputs on E = 1 are
// 32 leases; a foreign tenant that queues during block one is served at the
// first lease boundary — before block two — not after the 32nd block.
func TestEmbeddingsFairAtTheLeaseBoundary(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
	foreign := make(chan chatAnswer, 1)
	e.setRespond(func(_ http.ResponseWriter, _ *http.Request, n int64, _ []string) bool {
		if n == 1 {
			go func() {
				foreign <- rig.postEmbed(t, embedBody("emb", []string{"foreign"}), foreignTenantHeader, "fair")
			}()
			waitWaiting(t, rig.disp, e.srv.URL, 1, false)
		}
		return false
	})
	r := decodeEmbed(t, rig.postEmbed(t, embedBody("emb", embedInputs(32))))
	if len(r.Data) != 32 {
		t.Fatalf("data %d, want 32", len(r.Data))
	}
	decodeEmbed(t, <-foreign)
	calls := e.allCalls()
	if len(calls) != 33 {
		t.Fatalf("engine calls %d, want 33", len(calls))
	}
	at := -1
	for i, c := range calls {
		if c[0] == "foreign" {
			at = i
		}
	}
	if at != 1 {
		t.Errorf("the foreign request was call %d, want call 1 (the first lease boundary)", at)
	}
}

// TestEmbeddingsDeadlineInTheMiddleOfTheBlocks (O-09-1 (b)): the request
// deadline runs out in the middle of the block sequence ⇒ a defined answer
// (503, never 200), no lease held afterwards, and the row carries the
// blocks that got a result and their tokens.
func TestEmbeddingsDeadlineInTheMiddleOfTheBlocks(t *testing.T) {
	e := newEmbedEngine(t, 8)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
	rig.setSettings(func(s *oai.Settings) { s.RequestDeadline = 450 * time.Millisecond })
	e.setRespond(func(http.ResponseWriter, *http.Request, int64, []string) bool {
		time.Sleep(200 * time.Millisecond)
		return false
	})
	start := time.Now()
	a := rig.postEmbed(t, embedBody("emb", embedInputs(5)))
	code, _, _ := a.envelope(t)
	t.Logf("answer after %v: %d %s", time.Since(start).Round(time.Millisecond), a.status, a.body)
	if a.status != http.StatusServiceUnavailable || (code != oai.CodeUpstreamUnavailable && code != oai.CodeInternalError) {
		t.Fatalf("%d %s, want 503 (upstream_unavailable: the wire call of a block ≥ 2 hit the deadline; internal_error: the next acquire)", a.status, a.body)
	}
	if held, _ := heldOn(rig.disp, e.srv.URL); held != 0 {
		t.Errorf("held leases %d after the answer, want 0", held)
	}
	rows := rig.rows.all()
	if len(rows) != 1 {
		t.Fatalf("rows %d, want 1", len(rows))
	}
	done, _ := rows[0].Telemetry.Metadata["sub_batches_done"].(int)
	if done < 1 || done >= 5 || rows[0].Telemetry.PromptTokens != 2*done {
		t.Errorf("row: %d blocks done, %d tokens — want 1…4 blocks and 2 tokens each", done, rows[0].Telemetry.PromptTokens)
	}
	t.Logf("blocks done %d of 5, code %s", done, code)
	// The deadline is the request's, not the row's: no health report
	// against a healthy row (Fremd-Review 1 F-3).
	if n := consecutiveFails(rig.pool, "spark-embed"); n != 0 {
		t.Errorf("consecutive_fails %d, want 0 — the request deadline counted as the row's timeout", n)
	}
	if rec := <-rig.recs; rec.PromptTokens != 2*done {
		t.Errorf("access record tokens %d, want %d (what the row books)", rec.PromptTokens, 2*done)
	}
}

// TestEmbeddingsUnknownDimensionReservesTheCeiling (O-09-3 (a)): without a
// native dimension the reserve is gateway.embedding_max_floats × 4 B + the
// encoder buffer — before any acquire; a budget that does not hold it is
// 503 server_overloaded at once, no acquire, no wire contact. The ceiling
// 2²⁴ floats (64 MiB of answer) lies above the decode share's clamp (3 × the
// 16-MiB transport cap of a block's answer): the decode share fits, only the
// answer's does not (Delta-Review 6 F6-2: at a ceiling of 2²⁰ the decode
// share refused on its own, and a route that reserved nothing for an unknown
// dimension passed the gate). The budget is the sum of the request's shares,
// computed here from the named factors: one byte short ⇒ 503 at the entry;
// the sum itself serves the request — the refusal is the ceiling's reserve,
// exactly.
func TestEmbeddingsUnknownDimensionReservesTheCeiling(t *testing.T) {
	// The factors as the gate's own constants: a mutated factor in the
	// route must not move the expectation with it.
	const (
		ceiling      = 1 << 24  // gateway.embedding_max_floats for the test
		floatBytes   = 4        // one float32 component held for the answer
		encodeBuffer = 64 << 10 // the answer's encoder buffer
		decodeFactor = 3        // the decoder buffer's doubling
		transportCap = 16 << 20 // httpx.MaxResponseBytes: a block's read cap at this ceiling
	)
	body := embedBody("emb", []string{"x"})
	// The request part: the stage's share, or the ledger's bookings past it
	// (FOLGE-E: the read, the decoded text, the upstream body of the one
	// block).
	answer := int64(ceiling*floatBytes+encodeBuffer) + // the answer at the ceiling
		decodeFactor*transportCap // the decode share of the one-input block
	entry := embedLedgerRequest(body, 2, []string{"x"}, false, -1, "") + answer
	shares := embedLedgerRequest(body, 2, []string{"x"}, false, 1, "EMBED-MODEL") + answer
	// FOLGE-E: the last booking is the frame of the block's upstream body,
	// at the attempt — one byte short of all shares is refused after the
	// acquire, before any wire call; one byte short of what the entry books
	// is refused at the entry.
	for _, tc := range []struct {
		name     string
		budget   int64
		served   bool
		acquired int64
	}{
		{"one byte short of the entry shares", entry - 1, false, 0},
		{"one byte short of the shares", shares - 1, false, 1},
		{"exactly the shares", shares, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
			rig.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = ceiling; s.InflightBudget = tc.budget })
			_ = rig.recorder.Take()
			a := rig.postEmbed(t, body)
			n := acquires(rig.recorder.Take())
			t.Logf("budget %d (shares %d): answer %d %.120s; acquires %d, wire hits %d", tc.budget, shares, a.status, a.body, n, e.hits.Load())
			if tc.served {
				decodeEmbed(t, a)
				if n != 1 || e.hits.Load() != 1 {
					t.Errorf("acquires %d wire hits %d, want 1/1", n, e.hits.Load())
				}
				return
			}
			code, _, _ := a.envelope(t)
			if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
				t.Fatalf("%d %s, want 503 server_overloaded — the answer at the ceiling does not fit", a.status, a.body)
			}
			if int64(n) != tc.acquired || e.hits.Load() != 0 {
				t.Errorf("acquires %d wire hits %d, want %d/0 (the refusal stands before any wire call; at the entry for the entry's shares)", n, e.hits.Load(), tc.acquired)
			}
		})
	}
	// A ceiling below the decode clamp and a small budget: an unknown
	// dimension is refused at the entry within 50 ms (by the answer's share
	// or the decode share's, whichever comes first), and a known native
	// dimension is served at the same budget — it pays n × native × 4 B and
	// the decode share of a one-input block at its dimension, never the
	// ceiling.
	t.Run("a known dimension does not pay the ceiling", func(t *testing.T) {
		e := newEmbedEngine(t, 8)
		rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
		rig.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = 1 << 20; s.InflightBudget = 4<<20 + 32<<10 })
		_ = rig.recorder.Take()
		start := time.Now()
		a := rig.postEmbed(t, embedBody("emb", []string{"x"}))
		took := time.Since(start)
		code, _, _ := a.envelope(t)
		if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || took > 50*time.Millisecond {
			t.Fatalf("%d %s after %v, want 503 server_overloaded within 50 ms", a.status, a.body, took)
		}
		if n := acquires(rig.recorder.Take()); n != 0 || e.hits.Load() != 0 {
			t.Errorf("acquires %d wire hits %d, want 0/0", n, e.hits.Load())
		}
		decodeEmbed(t, rig.postEmbed(t, embedBody("embsmall", []string{"x"})))
	})
}

// discardWriter is a ResponseWriter that counts the answer and keeps
// nothing of it (the heap gate measures the route, not a recorder's copy).
type discardWriter struct {
	h      http.Header
	status int
	n      int
}

func (d *discardWriter) Header() http.Header         { return d.h }
func (d *discardWriter) WriteHeader(code int)        { d.status = code }
func (d *discardWriter) Write(p []byte) (int, error) { d.n += len(p); return len(p), nil }

// TestEmbeddingResponseReserved (G8, B9b): the answer share is reserved
// before the acquire. A request of 256 × 65 536 floats (the ceiling raised
// for the test) reserves 64 MiB + 64 KiB for its answer, the decode share of
// a one-input block before the acquire (hardening 2, C-2: the block size of a
// known native dimension is set by the row's capacity at the acquire) and
// grows it to its largest block, 3 × the 16 MiB read cap of a 32-input block,
// before the first wire call (H-3). While it holds in the engine the budget
// holds EXACTLY that sum, computed here from the named factors, not read off
// the booking (Fremd-Review 3 F3-9: with a budget chosen loose, a 30 % too
// small answer reserve passed); the budget is that sum plus what a second
// request books before its acquire, minus one byte — the second is 503
// server_overloaded at once, without an acquire, exactly when every share is
// right. The first one then serves its 64 MiB of vectors: the live heap peak
// over the baseline stays below the answer reserve plus a constant (the
// decode transient of one block).
func TestEmbeddingResponseReserved(t *testing.T) {
	const (
		dims  = 65536
		count = 256
		block = 32 // min(count, sub_batch 256, E 32)
		slack = 32 << 20
	)
	// The factors of the reserve (design/02 §4.2.1, hardening H-3), as the
	// gate's own constants: a mutated factor in the route must not move the
	// expectation with it.
	const (
		floatBytes    = 4        // one float32 component held for the answer
		encodeBuffer  = 64 << 10 // the answer's encoder buffer
		wireComponent = 25       // the longest float64 literal and its separator
		wireItem      = 256      // the frame of one data entry
		wireFrame     = 4 << 10  // the frame of the answer
		decodeFactor  = 3        // the decoder buffer's doubling
		transportCap  = 16 << 20 // httpx.MaxResponseBytes
	)
	readCap := func(inputs int) int64 {
		return min(int64(inputs)*dims*wireComponent+int64(inputs)*wireItem+wireFrame, transportCap)
	}
	body := embedBody("embbig", embedInputs(count))
	// The request part (FOLGE-E, the ledger): before the acquire the read
	// and the decoded texts; in the engine also the upstream bodies of the
	// attempt's count/block blocks.
	bodyShare := embedLedgerRequest(body, 2, embedInputs(count), false, count/block, "EMBED-MODEL")
	answerShare := int64(count*dims*floatBytes + encodeBuffer)
	holds := bodyShare + answerShare + decodeFactor*readCap(block)                                              // the first request in the engine
	books := embedLedgerRequest(body, 2, embedInputs(count), false, -1, "") + answerShare + decodeFactor*readCap(1) // a request before its acquire
	budget := holds + books - 1                                    // the second does not fit by one byte
	e := newEmbedEngine(t, dims)
	row := []byte(strings.TrimSuffix(strings.Repeat("1.5,", dims), ","))
	release := make(chan struct{})
	var releaseOnce sync.Once
	openEngine := func() { releaseOnce.Do(func() { close(release) }) }
	defer openEngine() // a failing assert below must not leave the first request in the engine
	e.setRespond(func(w http.ResponseWriter, _ *http.Request, n int64, inputs []string) bool {
		if n == 1 {
			<-release // the first request holds its reserve in the engine
		}
		_, _ = io.WriteString(w, `{"data":[`)
		for i := range inputs {
			if i > 0 {
				_, _ = io.WriteString(w, ",")
			}
			fmt.Fprintf(w, `{"index":%d,"embedding":[`, i)
			_, _ = w.Write(row)
			_, _ = io.WriteString(w, "]}")
		}
		_, _ = io.WriteString(w, `],"usage":{"prompt_tokens":1}}`)
		return true
	})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = budget; s.EmbeddingMaxFloats = 256 * dims })
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base := ms.HeapAlloc
	// Two peaks: HeapAlloc as sampled (it carries the garbage the GC has
	// not reclaimed yet — up to GOGC over the live heap), and the live heap,
	// sampled right after a forced GC every 10 ms. The gate is on the live
	// peak: what the reservation bounds is what the request holds.
	var peak, live atomic.Uint64
	stop := make(chan struct{})
	sampled := make(chan struct{})
	go func() {
		defer close(sampled)
		var ms runtime.MemStats
		for i := 0; ; i++ {
			if i%5 == 0 {
				runtime.GC()
				runtime.ReadMemStats(&ms)
				if ms.HeapAlloc > live.Load() {
					live.Store(ms.HeapAlloc)
				}
			}
			runtime.ReadMemStats(&ms)
			if ms.HeapAlloc > peak.Load() {
				peak.Store(ms.HeapAlloc)
			}
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
		}
	}()
	first := &discardWriter{h: http.Header{}}
	done := make(chan struct{})
	go func() {
		defer close(done)
		req := httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body))
		rig.router.ServeHTTP(first, req)
	}()
	for e.hits.Load() == 0 {
		select {
		case <-done: // a reserve larger than the sum: the first does not fit the budget
			t.Fatalf("first: %d before the engine — the budget %d B (the sum of the shares + one request's booking − 1) did not hold it", first.status, budget)
		case <-time.After(time.Millisecond):
		}
	}
	if got := rig.budget.InUse(); got != holds {
		t.Errorf("the budget holds %d B while the first request is in the engine, want exactly %d B = request %d (the ledger's) + answer %d + decode %d (3 × the read cap of a %d-input block)",
			got, holds, bodyShare, answerShare, holds-bodyShare-answerShare, block)
	}
	_ = rig.recorder.Take()
	start := time.Now()
	second := rig.postEmbed(t, body)
	took := time.Since(start)
	code, _, _ := second.envelope(t)
	if second.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded || took > 50*time.Millisecond {
		t.Fatalf("second: %d %s after %v, want 503 server_overloaded within 50 ms", second.status, second.body, took)
	}
	if n := acquires(rig.recorder.Take()); n != 0 {
		t.Errorf("second request acquired %d times, want 0", n)
	}
	openEngine()
	<-done
	close(stop)
	<-sampled
	if first.status != http.StatusOK || first.n < 256*dims*2 {
		t.Fatalf("first: status %d, %d bytes", first.status, first.n)
	}
	over := live.Load() - base
	reserved := uint64(256*dims*oai.EmbedFloatBytes + oai.EmbedEncodeBuffer)
	t.Logf("live heap peak %d MiB over a baseline of %d MiB (reserve %d MiB + %d MiB slack, budget %d MiB); HeapAlloc peak incl. garbage %d MiB; answer %d MiB streamed",
		over>>20, base>>20, reserved>>20, slack>>20, budget>>20, (peak.Load()-base)>>20, first.n>>20)
	// Bounded by the request's own reserve plus the decode transient of one
	// block — twice the vectors would not fit (Fremd-Review 1 F-5).
	if over > reserved+slack {
		t.Errorf("live heap peak %d MiB over the baseline, want < reserve + slack = %d MiB", over>>20, (reserved+slack)>>20)
	}
}

// TestEmbeddingsFinding14 (G4): 100 inputs, sub_batch 32, E = 32, the
// external queue capped at 1 and a competing waiter in it when block two
// asks: block two is a continuation — at the class head, exempt from the
// cap — so the answer is 200 (or a 429 BEFORE block one), never a refusal
// after block ≥ 2.
func TestEmbeddingsFinding14(t *testing.T) {
	e := newEmbedEngine(t, 4096)
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 33, 1)
	s := dispatch.DefaultSettings()
	s.ExternalQueueMax = 1
	rig.disp.UpdateSettings(s)
	host := e.srv.URL
	var once sync.Once
	var compete chan chatAnswer
	var occupied atomic.Pointer[dispatch.Lease]
	gate := &g4Dispatch{V1Dispatch: rig.serveBy, before: func(ctx context.Context) { // not the census Recorder: two requests run at once
		once.Do(func() {
			// A foreign lease takes the units block one just gave back, and a
			// foreign request queues behind it — the queue is full when
			// block two asks.
			other := foreignConsumer(urgentApp(), "occupy")
			l, _, err := rig.disp.Acquire(context.WithValue(context.Background(), consumerKey{}, other),
				dispatch.Request{Target: dispatch.Target{Origin: host}, Class: dispatch.ClassExternal, Role: "embed", Weight: 32})
			if err != nil {
				t.Errorf("occupy: %v", err)
				return
			}
			occupied.Store(l)
			compete = make(chan chatAnswer, 1)
			go func() {
				compete <- rig.postEmbed(t, embedBody("emb", []string{"compete"}), foreignTenantHeader, "compete")
			}()
			waitWaiting(t, rig.disp, host, 1, false)
			time.AfterFunc(50*time.Millisecond, l.Release)
		})
	}}
	p := handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: gate, Settings: func() oai.Settings { return *rig.settings.Load() },
		Logger: slog.New(slog.DiscardHandler)})
	rig.deps.Planner = p
	rig.router = rig.mountEmbed(oai.Embeddings(rig.deps))
	a := rig.postEmbed(t, embedBody("emb4k", embedInputs(100)))
	hitsAtAnswer := e.hits.Load()
	switch {
	case a.status == http.StatusOK:
		if r := decodeEmbed(t, a); len(r.Data) != 100 {
			t.Fatalf("data %d, want 100", len(r.Data))
		}
	case a.status == http.StatusTooManyRequests && hitsAtAnswer == 0:
	default:
		t.Fatalf("%d %s after %d wire hits — a refusal after block ≥ 2 (Finding 14)", a.status, a.body, hitsAtAnswer)
	}
	if compete != nil {
		if c := <-compete; c.status != http.StatusOK {
			t.Errorf("competing request: %d %s", c.status, c.body)
		}
	} else {
		t.Error("the gate never intercepted block two")
	}
}

// g4Dispatch runs before() ahead of every blocking acquire after the first
// one of the request under test (its block two).
type g4Dispatch struct {
	handler.V1Dispatch
	before func(ctx context.Context)
	n      atomic.Int64
}

func (g *g4Dispatch) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	if g.n.Add(1) == 2 {
		g.before(ctx)
	}
	return g.V1Dispatch.Acquire(ctx, req)
}
