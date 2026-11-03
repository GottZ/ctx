// Package llmlog persists LLM call metadata to context_llm_log for observability
// and post-hoc audits. Writes are async (a bounded per-pool queue drained by at
// most maxConcurrentInserts goroutines) so logging never blocks the LLM request
// path; a row the queue cannot take or the database refuses is counted
// (InsertDropped, InsertFailures), never lost silently.
//
// Source: https://github.com/GottZ/ctx
package llmlog

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GottZ/ctx/internal/prompts"
	"github.com/jackc/pgx/v5/pgxpool"
)

// insertTimeout bounds how long an async insert may take before being abandoned.
// Short because the caller has already returned — this is background bookkeeping.
// The clock starts when the insert leaves the queue, not at Record.
const insertTimeout = 5 * time.Second

// Write-path throttle (W06-1, design/06 §6/§7). Hardcoded on purpose, not
// settings: both bound the process's own bookkeeping, not anybody's policy.
const (
	// maxConcurrentInserts caps how many context_llm_log inserts run at once
	// per pool. The llmlog path shares ctxd's 20-connection pool
	// (store.NewPool) with every request path — and, once /v1 lands, with the
	// model-key auth — so a slow database must not let a burst of LLM calls
	// take the whole pool (before W06-1: one goroutine and one connection per
	// row, 20 of 20 under a 200-row burst). The live write rate is ≈ 5 000
	// rows per 24 h, so four slots only bind while the database is slow —
	// which is exactly when the pool is scarce.
	maxConcurrentInserts = 4
	// maxQueuedInserts bounds the rows waiting for an insert slot per pool.
	// Beyond it Record drops the row and counts it (InsertDropped) instead of
	// growing without bound while the database is away. The bound counts
	// rows, not bytes: 4 096 rows are about 20 h of the live average rate
	// (≈ 5 000 rows/24 h) but only around 2 h or less at the target scale of
	// thousands of calls per hour; at the measured 3.6 kB average row (bodies
	// included) a full queue holds roughly 15 MB, a queue of large-body rows
	// more.
	maxQueuedInserts = 4096
	// lossWarnInterval throttles the loss WARN to one line per interval,
	// process-wide: a database outage fails every insert, and one line per
	// row would bury the log the operator needs to read. Each line carries
	// the totals since start, so nothing is hidden by the throttle.
	lossWarnInterval = time.Minute
)

// dispatch_abort vocabulary (Vorhaben E wave MW10, design/05 §3.2). The DB
// column is free text by the backend_trust/required_sensitivity precedent —
// these constants plus their pin test ARE the value contract. Class
// invariant: only background rows ever carry one (I-D1: the dispatcher never
// cancels interactive leases, and interactive acquire rejections write no
// row at all).
const (
	// AbortPreempted — running background attempt canceled for interactive
	// demand (dispatch.ErrPreempted cause).
	AbortPreempted = "preempted"
	// AbortReaped — running background attempt force-released by the lease
	// reaper (dispatch.ErrReaped cause).
	AbortReaped = "reaped"
	// AbortAcquireExpired — K9 rejection line: a never-admitted background
	// acquire whose caller deadline expired while waiting. duration_ms is
	// NULL (no physical call); queue_wait_ms carries the futile wait.
	AbortAcquireExpired = "acquire_expired"
	// AbortQueueFull — K9 rejection line: background acquire bounced off the
	// full background wait queue (dispatch.ErrQueueFull).
	AbortQueueFull = "queue_full"
)

// Entry captures everything we persist for a single LLM call.
// All fields are optional; Pipeline + Model + Host are the minimum worth logging.
type Entry struct {
	Pipeline         string
	Model            string
	Host             string
	Duration         time.Duration
	Err              error
	PromptTokens     int
	CompletionTokens int
	RequestSystem    string
	RequestUser      string
	ResponseContent  string
	BlockIDs         []string       // source + target/candidate UUIDs for graph-aware queries
	DreamVersion     *int16         // set for dream pipelines
	Metadata         map[string]any // free-form; serialised to JSONB

	// F3-P2 backend telemetry (054). BackendName is the backend that
	// ACTUALLY answered (provenance fix: pre-pool code logged host=primary
	// even when the fallback served); Metadata["chain"] carries all
	// attempts. CostUSD stays nil for local backends (OpenRouter fills it
	// in G29). APIKeyID is the caller attribution (E4, no FK by design).
	BackendName         string
	BackendTrust        string
	BackendLocality     string
	RequiredSensitivity string
	Attempt             int
	CostUSD             *float64
	APIKeyID            string

	// Dispatch telemetry (Vorhaben E wave MW10, design/05 §3.2). QueueWaitMs
	// is the row-defining attempt's lease wait — a POINTER because 0 is a
	// real measurement (immediate admission, pass-through state) and must
	// persist as 0, never fall to NULL via the nullInt convention (B-R4: a
	// 0⇒NULL drop would skew every p95 upward). nil = pre-wiring row or
	// lease-free special path. DispatchClass/DispatchAbort follow the
	// empty⇒NULL nullStr pattern.
	QueueWaitMs   *int64
	DispatchClass string
	DispatchAbort string
	// NoWireCall marks a row without any physical model call (the K9
	// rejection line, DispatchAbort acquire_expired/queue_full): duration_ms
	// persists as NULL instead of a fake 0 — Duration measures the wire
	// call, and there was none.
	NoWireCall bool
}

// StampPrompt records WHICH prompt body produced this row: metadata.prompt_id
// and metadata.prompt_version, the two jsonb keys of E04-5 (no column, no
// migration). It is the ONE place in the tree that writes them, so the two
// spellings cannot drift apart across the twelve pipelines that stamp.
//
// The zero Identity is a no-op rather than an empty pair of keys: a row that
// carries no prompt (the embed wire row, the K9 rejection line) must say
// nothing instead of saying "". A pipeline that FORGETS its identity is not
// caught here but at build time — every llm.ChainCall literal is required to
// set Prompt, and every registered body is required to be sent, both by the
// AST gates in internal/prompts.
//
// The bodies themselves are slimmed for credentials-class rows (Slimmed
// below); the identity is not, and must not be: it is telemetry, not content,
// and a sealed row whose prompt generation is unknowable defeats the reason
// the seal keeps the telemetry in the first place.
func (e *Entry) StampPrompt(id prompts.Identity) {
	if id.ID == "" {
		return
	}
	if e.Metadata == nil {
		e.Metadata = map[string]any{}
	}
	e.Metadata["prompt_id"] = id.ID
	e.Metadata["prompt_version"] = id.Version
}

// Slimmed applies the E4/8b body slim: credentials-class rows keep the full
// telemetry + block_ids (the egress trace stays ID-exact) but drop the prompt
// bodies — no plaintext shadow corpus of the hottest tier in context_llm_log.
// Trade-off (user decision E4): loses debug plaintext for local
// credentials-class calls. No-op for every other sensitivity class.
//
// devmode is the CALLER'S RESOLVED tenant devmode (settings key
// tenant.devmode, C6-C): false — the default, and the only value a caller
// without a resolvable tenant may pass — seals exactly as before; true keeps
// the bodies, so a tenant that opted into developer transparency can read its
// OWN credentials-class prompts back on the status page.
//
// It is a PARAMETER on purpose, not a package var and not an Entry field. A
// field would default to the zero value at every construction site and the
// seal would then be one forgotten assignment away from silently correct;
// a parameter makes the compiler ask each caller which tenant it is writing
// for, and "I cannot tell" has exactly one spelling here: false.
func (e Entry) Slimmed(devmode bool) Entry {
	if e.RequiredSensitivity == "credentials" && !devmode {
		e.RequestSystem = ""
		e.RequestUser = ""
		e.ResponseContent = ""
	}
	return e
}

// EvictBodies implements time-based body retention (masterplan E4): after
// retentionDays, the plaintext prompt/response bodies (request_system,
// request_user, response_content) are NULLed while the rest of the row —
// pipeline, model, tokens, cost, block_ids, backend/trust/locality — survives.
// The egress audit trace stays ID-exact and lossless; only the hottest
// plaintext shadow corpus is dropped. This is Body-NULLing, NOT a chunk drop:
// the retention policy must never destroy the audit (E4 user decision).
//
// retentionDays <= 0 disables retention entirely (bodies kept forever — the
// operator opts in to unlimited). The IS NOT NULL guard makes a re-run
// idempotent: already-evicted rows are skipped, so the periodic janitor never
// rewrites long-settled chunks. Returns the number of rows NULLed.
func EvictBodies(ctx context.Context, pool *pgxpool.Pool, retentionDays int) (int64, error) {
	if pool == nil || retentionDays <= 0 {
		return 0, nil
	}
	tag, err := pool.Exec(ctx,
		`UPDATE context_llm_log
		SET request_system = NULL, request_user = NULL, response_content = NULL
		WHERE created_at < now() - make_interval(days => $1)
		  AND (request_system IS NOT NULL
		       OR request_user IS NOT NULL
		       OR response_content IS NOT NULL)`,
		retentionDays,
	)
	if err != nil {
		return 0, fmt.Errorf("llmlog: evict bodies: %w", err)
	}
	return tag.RowsAffected(), nil
}

// writer is the per-pool insert queue: Record appends, at most
// maxConcurrentInserts drain goroutines pop and insert, and a drain goroutine
// exits as soon as the queue is empty — an idle pool holds no goroutine.
//
// pending counts every row Record accepted that has not finished yet (landed,
// failed or timed out) — queued AND running. Record raises it BEFORE it
// returns, so every Record whose caller has returned is visible to Settle,
// including rows still waiting for a slot.
type writer struct {
	pool    *pgxpool.Pool
	insert  func(*pgxpool.Pool, Entry) error
	pending atomic.Int64

	mu      sync.Mutex
	queue   []Entry // FIFO of rows waiting for a slot; len <= maxQueuedInserts
	running int     // drain goroutines alive; <= maxConcurrentInserts
}

func newWriter(pool *pgxpool.Pool, insert func(*pgxpool.Pool, Entry) error) *writer {
	return &writer{pool: pool, insert: insert}
}

// writers holds one writer per pool. Keyed by pool and not one global queue:
// integration tests give every test its own database and pool, and a
// straggler writing into an earlier test's (closed) pool must neither take a
// slot from nor hold up the next test's Settle. One entry per pool ever
// created — a single one in ctxd.
var writers sync.Map // *pgxpool.Pool → *writer

func writerFor(pool *pgxpool.Pool) *writer {
	if v, ok := writers.Load(pool); ok {
		return v.(*writer)
	}
	v, _ := writers.LoadOrStore(pool, newWriter(pool, insert))
	return v.(*writer)
}

// inflightFor returns pool's pending counter — what Settle waits on.
func inflightFor(pool *pgxpool.Pool) *atomic.Int64 {
	return &writerFor(pool).pending
}

// Process-wide loss accounting (W06-1). Not per pool: ctxd has one pool, and
// the status page shows one number per process. Monotone since start.
var (
	insertFailures atomic.Int64 // inserts the database refused or that timed out
	insertDropped  atomic.Int64 // rows Record dropped because the queue was full
	lastLossWarn   atomic.Int64 // unix nanos of the last loss WARN; 0 = none yet
)

// InsertFailures returns how many context_llm_log inserts failed (database
// error or insertTimeout) since the process started. Each one is a lost row.
func InsertFailures() int64 { return insertFailures.Load() }

// InsertDropped returns how many rows Record dropped since the process started
// because the pool's queue already held maxQueuedInserts rows.
func InsertDropped() int64 { return insertDropped.Load() }

// Record persists an entry asynchronously. Safe to call from request paths —
// it never blocks on the database: the row joins the pool's bounded queue and
// one of at most maxConcurrentInserts goroutines writes it with a short
// deadline. A full queue drops the row; a failed insert is counted and logged
// (throttled WARN) but never bubbles up. Calling it after Settle — or on a
// closed pool — is allowed: the row is then a counted failure.
func Record(pool *pgxpool.Pool, e Entry) {
	if pool == nil || e.Pipeline == "" {
		return
	}
	writerFor(pool).enqueue(e)
}

func (w *writer) enqueue(e Entry) {
	w.mu.Lock()
	if len(w.queue) >= maxQueuedInserts {
		w.mu.Unlock()
		insertDropped.Add(1)
		warnLoss("llmlog: insert queue full, row dropped", e.Pipeline, nil)
		return
	}
	w.pending.Add(1)
	w.queue = append(w.queue, e)
	spawn := w.running < maxConcurrentInserts
	if spawn {
		w.running++
	}
	w.mu.Unlock()
	if spawn {
		go w.drain()
	}
}

// drain inserts queued rows until the queue is empty, then exits. The running
// count drops under the same lock that guards the queue, so a row enqueued
// after the last pop always finds either a live drain goroutine or a free slot.
func (w *writer) drain() {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running--
			w.queue = nil // release the backing array of a drained burst
			w.mu.Unlock()
			return
		}
		e := w.queue[0]
		w.queue[0] = Entry{} // do not pin the popped row's bodies
		w.queue = w.queue[1:]
		w.mu.Unlock()

		if err := w.insert(w.pool, e); err != nil {
			insertFailures.Add(1)
			slog.Debug("llmlog: insert failed", "pipeline", e.Pipeline, "error", err)
			warnLoss("llmlog: insert failed, row lost", e.Pipeline, err)
		}
		w.pending.Add(-1)
	}
}

// warnLoss logs a lost row at WARN at most once per lossWarnInterval,
// process-wide, with the loss totals since start; the rows in between are
// still counted (and each failure keeps its Debug line).
func warnLoss(msg, pipeline string, err error) {
	now := time.Now().UnixNano()
	last := lastLossWarn.Load()
	if last != 0 && now-last < int64(lossWarnInterval) {
		return
	}
	if !lastLossWarn.CompareAndSwap(last, now) {
		return // another goroutine took this interval's line
	}
	attrs := []any{
		"pipeline", pipeline,
		"insert_failures_total", insertFailures.Load(),
		"insert_dropped_total", insertDropped.Load(),
	}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.Warn(msg, attrs...)
}

// Settle blocks until every row Record accepted on pool has finished — queued
// rows that have not started yet included — or ctx ends. It is the ordering
// point the async write otherwise lacks: a caller that clears or counts
// context_llm_log right after the LLM call returned races the insert (issue
// #50 — a row of an earlier run landed after the journal reset and was
// counted against the next one), and ctxd's shutdown would close the pool
// under queued rows (W06-1). Settle says nothing about rows other processes
// write, and nothing about rows Recorded after it returned.
func Settle(ctx context.Context, pool *pgxpool.Pool) error {
	n := inflightFor(pool)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for n.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("llmlog: settle with %d insert(s) pending: %w", n.Load(), ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// insert writes one row; the error is the caller's to count (drain).
func insert(pool *pgxpool.Pool, e Entry) error {
	ctx, cancel := context.WithTimeout(context.Background(), insertTimeout)
	defer cancel()

	var errStr *string
	if e.Err != nil {
		s := e.Err.Error()
		errStr = &s
	}

	metadata := e.Metadata
	if metadata == nil {
		metadata = map[string]any{}
	}

	// duration_ms measures the physical model call (wait-free since MW10,
	// design/05 §4.4a). The K9 rejection line has no call — NULL, not 0.
	var durMs *int64
	if !e.NoWireCall {
		v := e.Duration.Milliseconds()
		durMs = &v
	}

	_, err := pool.Exec(ctx,
		`INSERT INTO context_llm_log
			(pipeline, model, host, duration_ms, error,
			 prompt_tokens, completion_tokens,
			 request_system, request_user, response_content,
			 block_ids, dream_version, metadata,
			 backend_name, backend_trust, backend_locality,
			 required_sensitivity, attempt, cost_usd, api_key_id,
			 queue_wait_ms, dispatch_class, dispatch_abort)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		        $14, $15, $16, $17, $18, $19, $20, $21, $22, $23)`,
		e.Pipeline, e.Model, e.Host, durMs, errStr,
		nullInt(e.PromptTokens), nullInt(e.CompletionTokens),
		e.RequestSystem, e.RequestUser, e.ResponseContent,
		e.BlockIDs, e.DreamVersion, metadata,
		nullStr(e.BackendName), nullStr(e.BackendTrust), nullStr(e.BackendLocality),
		nullStr(e.RequiredSensitivity), nullInt16(e.Attempt), e.CostUSD, nullUUID(e.APIKeyID),
		e.QueueWaitMs, nullStr(e.DispatchClass), nullStr(e.DispatchAbort),
	)
	return err
}

// nullInt returns nil for zero values so we can tell "unmeasured" from "zero".
func nullInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func nullInt16(v int) *int16 {
	if v == 0 {
		return nil
	}
	s := int16(v) //nolint:gosec // G115: attempt counts are single digits
	return &s
}

func nullStr(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

// nullUUID passes the id as *string so pgx casts to UUID; empty stays NULL.
func nullUUID(v string) *string {
	return nullStr(v)
}
