// Package llmlog persists LLM call metadata to context_llm_log for observability
// and post-hoc audits. Writes are async (fire-and-forget goroutine) so logging
// never blocks the LLM request path.
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
const insertTimeout = 5 * time.Second

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

// inflight counts, per pool, the inserts Record has started and that have not
// finished yet (landed, failed or timed out). Record raises the count BEFORE it
// returns, so every Record whose caller has returned is visible to Settle.
//
// Keyed by pool and not one global counter: integration tests give every test
// its own database and pool, and a straggler writing into an earlier test's
// (closed) pool must not hold up the next test's Settle. One entry per pool
// ever created — a single one in ctxd.
var inflight sync.Map // *pgxpool.Pool → *atomic.Int64

func inflightFor(pool *pgxpool.Pool) *atomic.Int64 {
	v, _ := inflight.LoadOrStore(pool, new(atomic.Int64))
	return v.(*atomic.Int64)
}

// Record persists an entry asynchronously. Safe to call from request paths —
// the DB write happens in its own goroutine with a short deadline and failures
// are logged but never bubble up.
func Record(pool *pgxpool.Pool, e Entry) {
	if pool == nil || e.Pipeline == "" {
		return
	}
	n := inflightFor(pool)
	n.Add(1)
	go func() {
		defer n.Add(-1)
		insert(pool, e)
	}()
}

// Settle blocks until every insert Record started on pool has finished, or ctx
// ends. It is the ordering point the fire-and-forget write otherwise lacks: a
// caller that clears or counts context_llm_log right after the LLM call
// returned races the insert goroutine (issue #50 — a row of an earlier run
// landed after the journal reset and was counted against the next one).
// Settle says nothing about rows other processes write.
func Settle(ctx context.Context, pool *pgxpool.Pool) error {
	n := inflightFor(pool)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for n.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("llmlog: settle with %d insert(s) in flight: %w", n.Load(), ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

func insert(pool *pgxpool.Pool, e Entry) {
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
	if err != nil {
		slog.Debug("llmlog: insert failed", "pipeline", e.Pipeline, "error", err)
	}
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
