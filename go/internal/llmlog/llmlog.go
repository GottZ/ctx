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
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// insertTimeout bounds how long an async insert may take before being abandoned.
// Short because the caller has already returned — this is background bookkeeping.
const insertTimeout = 5 * time.Second

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
}

// Slimmed applies the E4/8b body slim: credentials-class rows keep the full
// telemetry + block_ids (the egress trace stays ID-exact) but drop the prompt
// bodies — no plaintext shadow corpus of the hottest tier in context_llm_log.
// Trade-off (user decision E4): loses debug plaintext for local
// credentials-class calls. No-op for every other sensitivity class.
func (e Entry) Slimmed() Entry {
	if e.RequiredSensitivity == "credentials" {
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

// Record persists an entry asynchronously. Safe to call from request paths —
// the DB write happens in its own goroutine with a short deadline and failures
// are logged but never bubble up.
func Record(pool *pgxpool.Pool, e Entry) {
	if pool == nil || e.Pipeline == "" {
		return
	}
	go insert(pool, e)
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

	_, err := pool.Exec(ctx,
		`INSERT INTO context_llm_log
			(pipeline, model, host, duration_ms, error,
			 prompt_tokens, completion_tokens,
			 request_system, request_user, response_content,
			 block_ids, dream_version, metadata,
			 backend_name, backend_trust, backend_locality,
			 required_sensitivity, attempt, cost_usd, api_key_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13,
		        $14, $15, $16, $17, $18, $19, $20)`,
		e.Pipeline, e.Model, e.Host, e.Duration.Milliseconds(), errStr,
		nullInt(e.PromptTokens), nullInt(e.CompletionTokens),
		e.RequestSystem, e.RequestUser, e.ResponseContent,
		e.BlockIDs, e.DreamVersion, metadata,
		nullStr(e.BackendName), nullStr(e.BackendTrust), nullStr(e.BackendLocality),
		nullStr(e.RequiredSensitivity), nullInt16(e.Attempt), e.CostUSD, nullUUID(e.APIKeyID),
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
