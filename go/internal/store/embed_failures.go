package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/httpx"
)

// EmbedFailureClass is the last_class column of context_embed_failures
// (migration 113, Achse 04 §3.2a/§4.4). "wire" and "oversize" are the two
// classes W04-2 produces; "sensitivity_ineligible" and "store" are reserved
// for the migration worker (W04-3+) and named here only so the column's
// documented value set has one authoritative Go anchor.
type EmbedFailureClass string

const (
	// EmbedFailureWire is any embed attempt that reached the wire and came
	// back an error OTHER than a recognized oversize rejection (backend
	// down, timeout, malformed response, non-400 status, …). Gets the
	// exponential backoff curve (design/04 §4.4).
	EmbedFailureWire EmbedFailureClass = "wire"
	// EmbedFailureOversize marks a block that cannot be embedded at the
	// current slot window — either skipped BEFORE the wire call by the
	// token-estimate gate, or classified AFTER an HTTP 400 whose body
	// names exceed_context_size. Gets next_attempt_at = infinity: a longer
	// backoff would just be "retry forever in slow motion" for a block that
	// structurally cannot succeed without a content change (design/04 §4.4
	// "infinity-Semantik gilt auch für Pfad A/B").
	EmbedFailureOversize EmbedFailureClass = "oversize"
)

// maxLastErrorLen mirrors migration 113's documented last_error contract:
// normalized, ≤500 chars, never a raw wire error (Embed-TEXT content must
// not reach this column structurally — a backend response body can echo
// input fragments).
const maxLastErrorLen = 500

// NormalizeEmbedError builds the last_error value: "<class>: <sanitized
// excerpt>", control characters collapsed to spaces, hard-capped at 500
// runes. It is the ONLY path that may write into last_error — callers pass
// the raw error/response text, never anything block-content-derived beyond
// what the wire response itself already carried.
func NormalizeEmbedError(class EmbedFailureClass, raw string) string {
	clean := strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20:
			return -1 // drop other control bytes
		default:
			return r
		}
	}, raw)
	clean = strings.Join(strings.Fields(clean), " ") // collapse whitespace runs

	msg := string(class) + ": " + clean
	runes := []rune(msg)
	if len(runes) > maxLastErrorLen {
		runes = runes[:maxLastErrorLen]
	}
	return string(runes)
}

// oversizeBodyMarker is the llama.cpp error-type substring a rejected
// oversize embed carries in its response body (docs/operations.md "Backfill
// head-of-line caveat" / ctx incident 2026-07-20, rerank sibling). Substring
// match, not exact-parse: the exact JSON error envelope is not itself
// load-bearing here, only the presence of this token is.
const oversizeBodyMarker = "exceed_context_size"

// ClassifyEmbedError inspects an embed-wire error and returns the memo class
// plus a normalized (ready-to-store) last_error. An *httpx.StatusError whose
// body names exceed_context_size classifies as EmbedFailureOversize (the
// Netz-Klassifikation behind the pre-wire token estimate, design/04 §4.4);
// every other wire error — including other 4xx/5xx — classifies as
// EmbedFailureWire.
func ClassifyEmbedError(err error) (EmbedFailureClass, string) {
	var se *httpx.StatusError
	if errors.As(err, &se) {
		if strings.Contains(se.Body, oversizeBodyMarker) {
			msg := fmt.Sprintf("HTTP %d: %s", se.Code, se.Body)
			return EmbedFailureOversize, NormalizeEmbedError(EmbedFailureOversize, msg)
		}
		msg := fmt.Sprintf("HTTP %d: %s", se.Code, se.Body)
		return EmbedFailureWire, NormalizeEmbedError(EmbedFailureWire, msg)
	}
	if err == nil {
		return EmbedFailureWire, NormalizeEmbedError(EmbedFailureWire, "unknown error")
	}
	return EmbedFailureWire, NormalizeEmbedError(EmbedFailureWire, err.Error())
}

// OversizeEstimateMessage builds the last_error text for a block skipped by
// the PRE-wire token-estimate gate (never reached the backend) — kept
// distinct from ClassifyEmbedError's post-wire wording so a memo row's text
// alone tells you which of the two oversize paths fired.
func OversizeEstimateMessage(estimatedTokens, maxTokens int) string {
	return fmt.Sprintf("pre-wire estimate %d tokens > max_tokens %d", estimatedTokens, maxTokens)
}

// RecordEmbedFailure upserts the block's context_embed_failures row for the
// REGULAR backfill paths (migration_id IS NULL — Pfad A/B, Achse 04 W04-2;
// the migration-scoped counterpart is W04-3+). It accepts any execQuerier so
// callers can run it standalone on the pool (query.go) or inside the SAME
// tx that holds the FOR UPDATE SKIP LOCKED pick (scheduler.go) — the memo
// write must be atomic with the pick the same way StoreEmbedding's write is
// (Welle-49 tx-wrap doctrine): otherwise a concurrent picker could grab the
// row between the failed attempt and the memo commit.
//
// The backoff itself is computed SERVER-SIDE (base * 2^(attempts-1), capped)
// so a read-then-write race across concurrent pickers can never desync the
// exponent from the persisted attempts count. class==EmbedFailureOversize
// short-circuits to next_attempt_at='infinity' regardless of attempts
// (design/04 §4.4: a capped exponential backoff would still be "retry
// forever in slow motion" for a block that cannot structurally succeed).
func RecordEmbedFailure(ctx context.Context, q execQuerier, blockID string, class EmbedFailureClass, normalizedErr string, backoffBase, backoffCap time.Duration) error {
	if class == EmbedFailureOversize {
		_, err := q.Exec(ctx,
			`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
			 VALUES ($1, NULL, 1, $2, $3, 'infinity')
			 ON CONFLICT (block_id) WHERE migration_id IS NULL
			 DO UPDATE SET attempts        = context_embed_failures.attempts + 1,
			               last_error      = EXCLUDED.last_error,
			               last_class      = EXCLUDED.last_class,
			               next_attempt_at = 'infinity'`,
			blockID, normalizedErr, string(class),
		)
		if err != nil {
			return fmt.Errorf("store: record embed failure (oversize): %w", err)
		}
		return nil
	}

	baseSecs := backoffBase.Seconds()
	capSecs := backoffCap.Seconds()
	_, err := q.Exec(ctx,
		`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		 VALUES ($1, NULL, 1, $2, $3, now() + make_interval(secs => least($4::float8 * power(2, 0), $5::float8)))
		 ON CONFLICT (block_id) WHERE migration_id IS NULL
		 DO UPDATE SET attempts        = context_embed_failures.attempts + 1,
		               last_error      = EXCLUDED.last_error,
		               last_class      = EXCLUDED.last_class,
		               next_attempt_at = now() + make_interval(secs =>
		                   least($4::float8 * power(2, context_embed_failures.attempts), $5::float8))`,
		blockID, normalizedErr, string(class), baseSecs, capSecs,
	)
	if err != nil {
		return fmt.Errorf("store: record embed failure: %w", err)
	}
	return nil
}

// EmbedFailureExcludedPredicate is the shared SQL fragment both pending-pick
// queries (scheduler.go Pfad B, query.go Pfad A) AND-in: a block with an
// outstanding backoff (or a permanently parked oversize memo, next_attempt_at
// = infinity) is excluded from the pick until the backoff lapses — this is
// the structural fix for the Vorfall-2026-07-10 head-of-line class (the next
// PEEK reaches the next-oldest block instead of re-picking the same one).
// Defined ONCE (design/04 §3.3 "je einmal definiert in Go-Konstanten") and
// interpolated into both call sites' query strings — it references
// context_blocks.id positionally, so it composes into any query whose outer
// FROM is the bare (unaliased) context_blocks table.
const EmbedFailureExcludedPredicate = `
	AND NOT EXISTS (
		SELECT 1 FROM context_embed_failures f
		WHERE f.block_id = context_blocks.id
		  AND f.migration_id IS NULL
		  AND f.next_attempt_at > now()
	)`
