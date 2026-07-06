//go:build integration

// MW19 consumer-semantics pins, DB-visible half (design/02 §7 wave P2, §4.6):
// the wire-facing half lives in preempt_semantics_test.go; this file asserts
// what a preempt leaves BEHIND in Postgres. All three scenarios run the REAL
// MW18 preempt path (a dispatcher with {slots:1, preempt_background:true} on
// the dream origin, an interactive acquire as the trigger) against the
// exported chatJSON seam, mirroring the verified transport of a canceled wire
// call — the returned error wraps context.Cause(runCtx) (Go ≥1.23 places the
// cancel CAUSE into the http error chain, pinned wire-level in
// dispatch/k1_wire_test.go, gate P1(h)).
//
// Pinned here:
//   - preempted dream-eval ⇒ no context_dream_links row, one dream-eval
//     context_llm_log row with error non-null AND empty response_content
//     (§4.6 dream-eval row, PB4);
//   - the transient-cooldown semantics a preempted eval takes (P-N6):
//     SetDreamCooldownMinutes leaves dream_eval_count UNCHANGED and parks the
//     block ~5 min out — counter-probed by SetDreamCooldown (the regular
//     step-7 path of a SUCCESSFUL cycle) advancing the count, proving the test
//     could see an advance (§7 P2 back-off counter-probe);
//   - the dream-recurrence exception (§4.6 dream-recurrence row): a preempted
//     confirmRecurrence pair yields NO link, the candidate loop CONTINUES, the
//     next pair re-acquires (P-N9-shaped count), and the non-preempted pair
//     with verdict=recurrent DOES produce a persisted link — the W5
//     counter-probe that the pin could see a link loss.
package dream_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	ipPrimaryOrigin   = "http://gpu-mw19-it:8089"
	ipSecondaryOrigin = "http://cpu-mw19-it:8090"

	ipEvalSrcID  = "019d0000-0000-7000-9000-000000001901"
	ipEvalCandID = "019d0000-0000-7000-9000-000000001902"

	ipCooldownID = "019d0000-0000-7000-9000-000000001903"

	ipRecSrcID     = "019d0000-0000-7000-9000-000000001904"
	ipRecPreemptID = "019d0000-0000-7000-9000-000000001905" // pair that gets preempted → no link
	ipRecRecurID   = "019d0000-0000-7000-9000-000000001906" // pair with verdict=recurrent → link
)

// Shared preempt scaffolding: package dream_test twins of the unit helpers
// (the unit ones live in package dream and are not reachable here).

// newPreemptDispatcherIT builds a real dispatcher whose primary origin is
// preempt-enabled with ONE slot — the live herbert-chat shape (design/02
// §4.4). The interactive-principal hook is installed process-wide by the
// package-dream TestMain (preempt_export_test.go), so an acquire under
// dream.WithInteractivePrincipal earns ClassInteractive here too.
func newPreemptDispatcherIT(t *testing.T) *dispatch.Dispatcher {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{
		ipPrimaryOrigin: {Slots: 1, PreemptBackground: true},
	}})
	t.Cleanup(d.Close)
	return d
}

// countingAdmitterIT counts Acquire calls while delegating to the real
// dispatcher — the re-acquire pins need the count, not a fake.
type countingAdmitterIT struct {
	inner dispatch.Admitter
	n     *atomic.Int32
}

func (c countingAdmitterIT) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	c.n.Add(1)
	return c.inner.Acquire(ctx, req)
}

// fireInteractiveIT opens the preempt trigger: an interactive acquire on the
// primary origin from an authenticated ctx. It releases its lease as soon as
// it is admitted (which per §4.2.4 happens at the VICTIM's release).
func fireInteractiveIT(d *dispatch.Dispatcher) <-chan error {
	done := make(chan error, 1)
	go func() {
		l, _, err := d.Acquire(dream.WithInteractivePrincipal(context.Background()), dispatch.Request{
			Target: dispatch.Target{Origin: ipPrimaryOrigin},
			Class:  dispatch.ClassInteractive,
			Role:   "mw19-it-trigger",
		})
		if err == nil {
			l.Release()
		}
		done <- err
	}()
	return done
}

// dreamBackend is one seeded backend row shaped like a GPU dream target on the
// given origin.
func dreamBackend(id, origin string, priority int) backends.Backend {
	return backends.Backend{
		ID: id, Name: id, Host: origin,
		Trust: backends.TrustFull, Locality: "lan",
		Roles:    []string{backends.RoleDream},
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m-" + id}},
		Priority: priority, Enabled: true,
	}
}

// swapChatJSON installs fn as the package-level dream chatJSON seam and
// restores the previous implementation at test end.
func swapChatJSON(t *testing.T, fn dream.ChatJSONFunc) {
	t.Helper()
	prev := dream.SetChatJSONForTest(fn)
	t.Cleanup(func() { dream.SetChatJSONForTest(prev) })
}

// recurResp is the recurrent-verdict body the non-preempted pair returns.
func recurResp() *llm.ChatResponse {
	return &llm.ChatResponse{
		Message:      llm.Message{Content: `{"verdict":"recurrent","pattern":"parallel","confidence":0.95}`},
		EvalCount:    12,
		PromptTokens: 200,
	}
}

// insertTemporal seeds one context_temporal row so the recurrence Phase-1 SQL
// can JOIN source and candidate on a shared dimension+value.
func insertTemporal(t *testing.T, pool *pgxpool.Pool, blockID, dimension, value string, date time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_temporal (block_id, dimension, value, source_time)
		 VALUES ($1::uuid, $2, $3, $4)`,
		blockID, dimension, value, date,
	); err != nil {
		t.Fatalf("insert temporal %s/%s=%s: %v", blockID, dimension, value, err)
	}
}

// waitLlmlogRows polls until exactly `want` context_llm_log rows exist for the
// pipeline (llmlog.Record inserts asynchronously via `go insert`), then
// returns them ordered by error-NULL-ness so callers can address the failed
// and the succeeded attempt deterministically.
type llmlogRow struct {
	errText     *string
	respContent string
}

func waitLlmlogRows(t *testing.T, pool *pgxpool.Pool, pipeline string, want int) []llmlogRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		var n int
		err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, pipeline).Scan(&n)
		cancel()
		if err != nil {
			t.Fatalf("count llmlog rows for %s: %v", pipeline, err)
		}
		if n >= want {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("llmlog: pipeline %s reached %d rows, want %d (async insert never landed)", pipeline, n, want)
		}
		time.Sleep(20 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	rows, err := pool.Query(ctx,
		`SELECT error, COALESCE(response_content, '') FROM context_llm_log
		 WHERE pipeline = $1 ORDER BY (error IS NULL), id`, pipeline)
	if err != nil {
		t.Fatalf("read llmlog rows for %s: %v", pipeline, err)
	}
	defer rows.Close()
	var out []llmlogRow
	for rows.Next() {
		var r llmlogRow
		if err := rows.Scan(&r.errText, &r.respContent); err != nil {
			t.Fatalf("scan llmlog row: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate llmlog rows: %v", err)
	}
	return out
}

// Test 1: preempted dream-eval leaves no link + a failed llmlog row.

// TestDreamEvalPreempted_NoLinkRowFailedLlmlog_DB pins the DB-visible residue
// of a preempted eval (§4.6 dream-eval row, PB4): no context_dream_links row
// is written (the cycle returns before WriteLinks), and the one dream-eval
// context_llm_log attempt row carries a non-null error with an EMPTY
// response_content — the payload invariant (01 §9): a canceled call persists
// its attempt metadata but never a body.
func TestDreamEvalPreempted_NoLinkRowFailedLlmlog_DB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	tLate := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, ipEvalSrcID, "private", "decisions", "Eval source block", tEarly, tEarly)
	insertBlock(t, pool, ipEvalCandID, "private", "projects", "Eval candidate block", tLate, tLate)

	d := newPreemptDispatcherIT(t)
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{dreamBackend("mw19-it-gpu", ipPrimaryOrigin, 100)})
	r := &dream.Router{Pool: p, Admit: llm.Admission{Admitter: d, Class: dispatch.ClassBackground}}

	inflight := make(chan struct{})
	var once atomic.Bool
	swapChatJSON(t, func(ctx context.Context, host, _, _ string, _ *bool, _, _ string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if once.CompareAndSwap(false, true) {
			close(inflight)
		}
		<-ctx.Done()
		return nil, fmt.Errorf("post chat: %w", context.Cause(ctx))
	})

	type evalResult struct {
		links []dream.Link
		err   error
	}
	res := make(chan evalResult, 1)
	src := dream.BlockInfo{ID: ipEvalSrcID, Title: "Eval source block", Content: "src", Scope: "private", Sensitivity: backends.SensInternal, CreatedAt: tEarly, UpdatedAt: tEarly}
	cand := dream.BlockInfo{ID: ipEvalCandID, Title: "Eval candidate block", Content: "cand", Scope: "private", Sensitivity: backends.SensInternal, CreatedAt: tLate, UpdatedAt: tLate}
	go func() {
		links, err := dream.EvaluateRelationships(context.Background(), pool, r, llm.Options{}, src, []dream.BlockInfo{cand})
		res <- evalResult{links, err}
	}()

	<-inflight
	trigger := fireInteractiveIT(d)

	got := <-res
	if got.err == nil {
		t.Fatal("preempted eval must return an error")
	}
	if !errors.Is(got.err, dispatch.ErrPreempted) {
		t.Fatalf("errors.Is(err, ErrPreempted) = false, err = %v", got.err)
	}
	if len(got.links) != 0 {
		t.Fatalf("preempted eval must yield no links, got %d", len(got.links))
	}
	if err := <-trigger; err != nil {
		t.Fatalf("interactive trigger acquire: %v", err)
	}

	// No link row: EvaluateRelationships never writes links, and a preempted
	// cycle returns before WriteLinks — the table stays empty for the source.
	if n := countLinks(t, pool, ipEvalSrcID); n != 0 {
		t.Fatalf("preempted eval wrote %d dream link(s), want 0 (PB4)", n)
	}

	// One dream-eval attempt row: error non-null, response_content empty.
	rows := waitLlmlogRows(t, pool, "dream-eval", 1)
	if len(rows) != 1 {
		t.Fatalf("dream-eval llmlog rows = %d, want 1", len(rows))
	}
	if rows[0].errText == nil || *rows[0].errText == "" {
		t.Fatalf("preempted attempt must log a non-null error, got %v", rows[0].errText)
	}
	if !strings.Contains(*rows[0].errText, "preempted") {
		t.Fatalf("logged error must name the preempt, got %q", *rows[0].errText)
	}
	if rows[0].respContent != "" {
		t.Fatalf("preempted attempt must persist an empty response_content, got %q (payload invariant 01 §9)", rows[0].respContent)
	}
}

// Test 2: transient cooldown preserves the back-off, the regular path
// advances it (§7 P2 back-off counter-probe, P-N6).

// TestDreamCooldown_TransientPreservesRegularAdvances_DB pins the exact
// semantic the RunDreamCycle step-4 preempt branch relies on: a preempted eval
// takes SetDreamCooldownMinutes, which parks the block ~5 min out WITHOUT
// touching dream_eval_count — a preempt is a scheduling decision, not
// completed work (P-N6). The counter-probe runs SetDreamCooldown (the step-7
// path of a SUCCESSFUL cycle) on the same block and shows the count advance —
// proving the constancy assertion above could actually fail.
func TestDreamCooldown_TransientPreservesRegularAdvances_DB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tEarly := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insertBlock(t, pool, ipCooldownID, "private", "decisions", "Cooldown block", tEarly, tEarly)
	// Seed a matured back-off level so an accidental advance is unmistakable.
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET dream_eval_count = 7, dream_checked_at = now() - interval '1 day' WHERE id = $1::uuid`,
		ipCooldownID); err != nil {
		t.Fatalf("seed eval count: %v", err)
	}

	// Transient path (what a preempted eval takes).
	if err := dream.SetDreamCooldownMinutes(ctx, pool, ipCooldownID, dream.CooldownTransientMinutes); err != nil {
		t.Fatalf("SetDreamCooldownMinutes: %v", err)
	}
	var count int
	var cooldownSecs float64
	if err := pool.QueryRow(ctx,
		`SELECT dream_eval_count, EXTRACT(EPOCH FROM (dream_cooldown_until - now())) FROM context_blocks WHERE id = $1::uuid`,
		ipCooldownID).Scan(&count, &cooldownSecs); err != nil {
		t.Fatalf("read block state after transient cooldown: %v", err)
	}
	if count != 7 {
		t.Fatalf("transient cooldown advanced dream_eval_count to %d, want 7 (P-N6: a preempt is no completed work)", count)
	}
	// CooldownTransientMinutes = 5 → parked ~5 min out (allow scheduling slack).
	if cooldownSecs < 4*60 || cooldownSecs > 6*60 {
		t.Fatalf("transient cooldown parked block %.0fs out, want ~300s (5 min)", cooldownSecs)
	}

	// Counter-probe: the regular step-7 path DOES advance the count — the
	// assertion above is falsifiable.
	bc := dream.BackoffConfig{Mode: "exp", Factor: 1.6, Grace: 0, MinHours: 1, CapHours: 1080, InertOffset: 2}
	if err := dream.SetDreamCooldown(ctx, pool, ipCooldownID, false, bc); err != nil {
		t.Fatalf("SetDreamCooldown: %v", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT dream_eval_count FROM context_blocks WHERE id = $1::uuid`, ipCooldownID).Scan(&count); err != nil {
		t.Fatalf("read eval count after regular cooldown: %v", err)
	}
	if count != 8 {
		t.Fatalf("regular cooldown left dream_eval_count at %d, want 8 (counter-probe: an advance IS visible)", count)
	}
}

// Test 3: preempted recurrence pair loses its verdict, the loop continues,
// the recurrent pair still produces a persisted link (§4.6 dream-recurrence).

// TestDreamRecurrencePreempted_PairLostLoopContinuesLinkWritten_DB pins the
// dream-recurrence exception: a preempted confirmRecurrence call is a per-pair
// non-fatal skip — the candidate loop CONTINUES, the next pair re-acquires
// (background waits in the queue, §4.2.1), and the non-preempted pair with
// verdict=recurrent still yields a link that WriteLinks persists. The seam
// selects the victim by target-id in the prompt, so the OUTCOME is
// order-independent — but the loop-continues half of the pin is only visible
// when the preempted pair runs FIRST (a break after the LAST pair looks like a
// continue). pickRecurrenceCandidates orders by title_sim DESC, so the
// preempted candidate carries the shorter title suffix (higher pg_trgm sim)
// to run first, deterministically — the W5 counter-probe that both a link
// loss AND a loop abort would be visible (integrator red-probe 2026-07-06:
// with suffixes the other way round, a break in the preempt branch stayed
// green).
func TestDreamRecurrencePreempted_PairLostLoopContinuesLinkWritten_DB(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	tRef := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	// Near-identical titles so both candidates clear the pg_trgm sim>0.5 gate;
	// the preempted pair's suffix is the shortest so it sorts first (see doc
	// comment above).
	insertBlock(t, pool, ipRecSrcID, "private", "projects", "Recurring Weekly Sync Session", tRef, tRef)
	insertBlock(t, pool, ipRecPreemptID, "private", "projects", "Recurring Weekly Sync Session A", tRef, tRef)
	insertBlock(t, pool, ipRecRecurID, "private", "projects", "Recurring Weekly Sync Session Gamma", tRef, tRef)
	// Shared temporal dimension+value so Phase-1 JOIN pairs them with the source,
	// and a non-credentials sensitivity so the required class stays 'internal'
	// (credentials would trip llmlog.Slimmed and null the response body).
	for _, id := range []string{ipRecSrcID, ipRecPreemptID, ipRecRecurID} {
		insertTemporal(t, pool, id, "year", "2026", tRef)
		if _, err := pool.Exec(context.Background(),
			`UPDATE context_blocks SET sensitivity = 'internal' WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("set sensitivity: %v", err)
		}
	}

	d := newPreemptDispatcherIT(t)
	var acquires atomic.Int32
	p := backends.NewPool(nil, nil)
	p.SeedSnapshotForTest([]backends.Backend{dreamBackend("mw19-it-gpu", ipPrimaryOrigin, 100)})
	r := &dream.Router{
		Pool:  p,
		Admit: llm.Admission{Admitter: countingAdmitterIT{inner: d, n: &acquires}, Class: dispatch.ClassBackground},
	}

	// Victim selection by target-id in the prompt (buildRecurrencePrompt emits
	// id="<TargetID>"): the pair carrying ipRecPreemptID blocks until preempted
	// and returns the cause-wrapped transport error; the pair carrying
	// ipRecRecurID answers recurrent. Order-independent.
	inflight := make(chan struct{})
	var once atomic.Bool
	swapChatJSON(t, func(ctx context.Context, host, _, _ string, _ *bool, _, userPrompt string, _ llm.Options, _ time.Duration) (*llm.ChatResponse, error) {
		if strings.Contains(userPrompt, ipRecPreemptID) {
			if once.CompareAndSwap(false, true) {
				close(inflight)
			}
			<-ctx.Done()
			return nil, fmt.Errorf("post chat: %w", context.Cause(ctx))
		}
		return recurResp(), nil
	})

	type recResult struct {
		links []dream.Link
		err   error
	}
	res := make(chan recResult, 1)
	src := dream.BlockInfo{ID: ipRecSrcID, Title: "Recurring Weekly Sync Session", Content: "src", Scope: "private", Sensitivity: backends.SensInternal, CreatedAt: tRef, UpdatedAt: tRef}
	go func() {
		links, err := dream.DetectRecurrence(context.Background(), pool, r, llm.Options{}, src)
		res <- recResult{links, err}
	}()

	<-inflight
	trigger := fireInteractiveIT(d)

	got := <-res
	if err := <-trigger; err != nil {
		t.Fatalf("interactive trigger acquire: %v", err)
	}
	if got.err != nil {
		t.Fatalf("DetectRecurrence must be non-fatal on a preempted pair, got: %v", got.err)
	}
	// Both pairs were attempted (loop continued past the preempt), each through
	// its own Acquire (re-acquire, not a reused lease — P-N9 shape).
	if n := acquires.Load(); n != 2 {
		t.Fatalf("Acquire count = %d, want 2 — both candidate pairs must re-acquire (did both clear the sim>0.5 Phase-1 gate?)", n)
	}
	// Exactly the recurrent pair survives as a link; the preempted pair does not.
	if len(got.links) != 1 {
		t.Fatalf("returned links = %d, want 1 (the recurrent pair only)", len(got.links))
	}
	if got.links[0].TargetID != ipRecRecurID {
		t.Fatalf("surviving link targets %s, want the non-preempted pair %s", got.links[0].TargetID, ipRecRecurID)
	}

	// Persist and assert the DB residue: one row, for the recurrent pair only.
	written, err := dream.WriteLinks(context.Background(), pool, bootedSet(t, pool), ipRecSrcID, "private", 1.0, got.links)
	if err != nil {
		t.Fatalf("WriteLinks recurrent: %v", err)
	}
	if written != 1 || countLinks(t, pool, ipRecSrcID) != 1 {
		t.Fatalf("recurrent pair wrote %d link(s) (countLinks=%d), want exactly 1", written, countLinks(t, pool, ipRecSrcID))
	}
	var linkTarget, rel string
	if err := pool.QueryRow(context.Background(),
		`SELECT target_block_id::text, relationship FROM context_dream_links WHERE source_block_id = $1::uuid`,
		ipRecSrcID).Scan(&linkTarget, &rel); err != nil {
		t.Fatalf("read persisted link: %v", err)
	}
	if linkTarget != ipRecRecurID || rel != "recurrent" {
		t.Fatalf("persisted link = (%s, %s), want (%s, recurrent) — preempted pair must have NO link", linkTarget, rel, ipRecRecurID)
	}

	// Both pairs logged an attempt: one failed (preempted, empty body), one
	// succeeded (verdict body present).
	rows := waitLlmlogRows(t, pool, "dream-recurrence", 2)
	if len(rows) != 2 {
		t.Fatalf("dream-recurrence llmlog rows = %d, want 2", len(rows))
	}
	// ORDER BY (error IS NULL): the failed attempt sorts first.
	if rows[0].errText == nil || *rows[0].errText == "" || rows[0].respContent != "" {
		t.Fatalf("preempted recurrence attempt: want non-null error + empty body, got err=%v body=%q", rows[0].errText, rows[0].respContent)
	}
	if rows[1].errText != nil || rows[1].respContent == "" {
		t.Fatalf("recurrent attempt: want null error + non-empty body, got err=%v body=%q", rows[1].errText, rows[1].respContent)
	}
}
