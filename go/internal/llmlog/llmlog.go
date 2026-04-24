// Package llmlog persists LLM call metadata to context_llm_log for observability
// and post-hoc audits. Writes are async (fire-and-forget goroutine) so logging
// never blocks the LLM request path.
//
// Source: https://github.com/GottZ/ctx
package llmlog

import (
	"context"
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
			 block_ids, dream_version, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		e.Pipeline, e.Model, e.Host, e.Duration.Milliseconds(), errStr,
		nullInt(e.PromptTokens), nullInt(e.CompletionTokens),
		e.RequestSystem, e.RequestUser, e.ResponseContent,
		e.BlockIDs, e.DreamVersion, metadata,
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
