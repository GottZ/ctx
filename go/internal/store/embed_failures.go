package store

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/pgxdb"
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
	// names an oversize marker (oversizeBodyMarkers). Gets next_attempt_at = infinity: a longer
	// backoff would just be "retry forever in slow motion" for a block that
	// structurally cannot succeed without a content change (design/04 §4.4
	// "infinity-Semantik gilt auch für Pfad A/B").
	EmbedFailureOversize EmbedFailureClass = "oversize"
	// EmbedFailureSensitivityIneligible marks a block whose floor-adjusted
	// sensitivity leaves the migration worker's embed chain EMPTY — no
	// backend is trusted enough for the block's content (design/04 §4.4/§5
	// Bruchpfad 3: skip with memo, NEVER escalate across the trust border).
	// Like oversize it gets next_attempt_at = infinity: the condition is
	// structural (content sensitivity × backend trust), not transient — a
	// trust-config change plus operator retry is the only way out, not a
	// timer. Migration-scoped only in W04-4 (the regular backfill keeps its
	// memo-free "stays unembedded, retried every cycle" semantics for this
	// case — a pool outage there must not permanently park blocks; its pick
	// skips such blocks per cycle instead of stalling on the oldest one,
	// events.embeddableConjunct, PR #46 review finding M2).
	EmbedFailureSensitivityIneligible EmbedFailureClass = "sensitivity_ineligible"
	// EmbedFailureCallerTimeout marks a block whose embed did not fit the
	// query path's sync budget (embed_backfill.sync_budget) or died with
	// the request context — the backend is not known to be down, it was
	// slow relative to THIS caller's deadline (the tracking issue, force-push.me/damienmoon/ctx/issues/1: a
	// 10k-token block on a CPU backend). The memo therefore says nothing
	// about the backend and must not park the block on the wire curve: while
	// the background arm runs (embed_backfill.interval > 0) the query path
	// (EmbedFailureQueryPathExcludedPredicate) never picks a caller_timeout
	// block again, the arm picks it at once (next_attempt_at = now()). With
	// the arm off the query path retries it itself: after a backoff counted
	// from the cut, and in the place of that cut in its oldest-first order
	// (handler.backfillPicker.oldEnd). Only written while a sync budget is
	// active, and with the arm off only for a block that had the whole of
	// it (the first embed of its request) — with
	// sync_budget=0 a request cancel stays wire (review finding M1). A later
	// wire failure in the background arm reclassifies it as wire with the
	// ordinary backoff.
	EmbedFailureCallerTimeout EmbedFailureClass = "caller_timeout"
)

// releasableClasses are the classes of the regular backfill's memos
// (migration_id IS NULL) that ReleaseBackfillMemos takes: wire (a backoff
// after a failed wire call) and oversize (the park of a block that did not
// fit). The third class the regular backfill writes, caller_timeout, is no
// park: while the background arm runs the arm takes such a block at once
// (its plain predicate admits the memo), and with the arm off the query path
// retries it after the hold its attempts give (CallerTimeoutHoldEnd).
// Deleting it would only hand the block back to the requests' sync budget
// and restart its attempts, which #240 part 4 keeps an edit from doing.
// sensitivity_ineligible is a migration's class only, and store is reserved.
var releasableClasses = []EmbedFailureClass{EmbedFailureWire, EmbedFailureOversize}

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

// oversizeBodyMarkers are the substrings a rejected oversize embed carries in
// its response body — the one list of the oversize detector:
//
//   - "exceed_context_size": llama.cpp's error type. Live-vermessener
//     Vertrag (Lead-Messung 2026-07-24, W04-4): HTTP 400 with JSON body
//     {"error":{"code":400,"message":"request (N tokens) exceeds the
//     available context size (M tokens), …","type":"exceed_context_size_error",…}}
//     — the type string carries the _error SUFFIX. Substring match on the
//     design-doc stem catches both spellings and older server versions.
//   - "context_length_exceeded": the OpenAI error code — what a ctx
//     gateway's POST /v1/embeddings answers (400, its own text) when its
//     backend refused an input as too long (FOLGE-EMBLIMIT), and OpenAI's
//     own code for the same refusal. A ctx instance embedding through
//     another one's gateway would otherwise retry such a block on the wire
//     curve forever.
//
// The exact JSON envelope is not load-bearing, only the presence of a token.
var oversizeBodyMarkers = []string{"exceed_context_size", "context_length_exceeded"}

// ClassifyEmbedError inspects an embed-wire error and returns the memo class
// plus a normalized (ready-to-store) last_error. An *httpx.StatusError with
// HTTP 400 whose body names one of oversizeBodyMarkers classifies as
// EmbedFailureOversize (the Netz-Klassifikation behind the pre-wire token
// estimate, design/04 §4.4); every other wire error — including other
// 4xx/5xx — classifies as EmbedFailureWire. The 400-AND-substring rule is
// the measured llama.cpp contract (see oversizeBodyMarkers): a 5xx that
// merely ECHOES a marker in its body is a transient server condition and
// must keep the retryable wire class, never the permanent infinity park.
func ClassifyEmbedError(err error) (EmbedFailureClass, string) {
	if se, ok := errors.AsType[*httpx.StatusError](err); ok {
		if se.Code == http.StatusBadRequest && slices.ContainsFunc(oversizeBodyMarkers, func(m string) bool { return strings.Contains(se.Body, m) }) {
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
// the migration-scoped counterpart is W04-3+). It accepts any pgxdb.Execer so
// callers can run it standalone on the pool (query.go) or inside the SAME
// tx that holds the FOR UPDATE SKIP LOCKED pick (scheduler.go) — the memo
// write must be atomic with the pick the same way StoreEmbedding's write is
// (Welle-49 tx-wrap doctrine): otherwise a concurrent picker could grab the
// row between the failed attempt and the memo commit.
//
// The backoff itself is computed SERVER-SIDE (base * 2^(attempts-1), capped)
// so a read-then-write race across concurrent pickers can never desync the
// exponent from the persisted attempts count. An isInfinityClass match
// short-circuits to next_attempt_at='infinity' regardless of attempts
// (design/04 §4.4: a capped exponential backoff would still be "retry
// forever in slow motion" for a block that cannot structurally succeed).
//
// Such a park also ends a stale window (issue #198, part B): a title or
// content change keeps the previous vector until the backfill writes the new
// one, and a parked block's new vector is never written, so the previous one
// is dropped here, and the block is unembedded as it was before part B. The
// block row is written before the memo row, the order every content writer
// takes them in (InvalidateEmbeddingTx).
func RecordEmbedFailure(ctx context.Context, q pgxdb.Execer, blockID string, class EmbedFailureClass, normalizedErr string, backoffBase, backoffCap time.Duration) error {
	if isInfinityClass(class) {
		if _, err := q.Exec(ctx,
			`UPDATE context_blocks SET embedding = NULL, embed_model = NULL, embedding_stale = false
			 WHERE id = $1 AND embedding_stale`,
			blockID,
		); err != nil {
			return fmt.Errorf("store: record embed failure (%s): drop the stale vector: %w", class, err)
		}
	}
	_, err := q.Exec(ctx, regularMemoSQL(class, false), regularMemoArgs(blockID, class, normalizedErr, backoffBase, backoffCap)...)
	if err != nil {
		return fmt.Errorf("store: record embed failure (%s): %w", class, err)
	}
	return nil
}

// RecordEmbedFailureIfUnchanged is RecordEmbedFailure for a caller that read
// the block's title and content without holding its row (the query path's
// inline backfill, the sibling of StoreEmbeddingIfUnchanged): the memo lands
// only while the block still holds that title and content and is pending, and
// written reports whether a memo row was inserted or updated (issue #372,
// part 1). A title or content change releases the block's memo in its own
// transaction (InvalidateEmbeddingTx); a memo of the attempt at the earlier
// text that committed after that parked the changed block under the earlier
// attempt's verdict, and for an oversize memo that was 'infinity', so the
// changed text was never embedded.
//
// The upsert locks the block's row (attemptGuard) on the condition that the
// row still holds the text, and writes the memo, and for an infinity class
// drops the stale vector, only for a row it locked. A change that committed
// before is seen by the condition (READ COMMITTED re-checks a row it locks on
// its latest version); a change that comes later waits for the statement and
// releases its memo. A row another transaction holds is skipped instead of
// waited for: a content writer's, whose change releases the memo anyway and
// must not find a memo of the earlier text after it, or a background
// embedder's (the arm, Dream or a migration worker holds the row for its
// whole embed call, and waiting for it held the request up to the memo
// context's 5 s). Without the row nothing is inserted, and a memo of the same
// class already in place is updated in place (regularMemoUpdateSQL): the memo
// row, which a content writer locks as well, orders the two. The block row
// comes before the memo row, the order every content writer takes them in.
// written is false as well for a caller_timeout cut inside the hold of the
// memo in place (regularMemoParts), which leaves that memo as it is.
func RecordEmbedFailureIfUnchanged(ctx context.Context, q pgxdb.Execer, blockID, title, content string, class EmbedFailureClass, normalizedErr string, backoffBase, backoffCap time.Duration) (written bool, err error) {
	args := append(regularMemoArgs(blockID, class, normalizedErr, backoffBase, backoffCap), title, content)
	tag, err := q.Exec(ctx, regularMemoSQL(class, true), args...)
	if err != nil {
		return false, fmt.Errorf("store: record embed failure (%s, bound to the text): %w", class, err)
	}
	if tag.RowsAffected() == 1 || isInfinityClass(class) {
		return tag.RowsAffected() == 1, nil
	}
	tag, err = q.Exec(ctx, regularMemoUpdateSQL(class), args...)
	if err != nil {
		return false, fmt.Errorf("store: record embed failure (%s, in place): %w", class, err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordEmbedRequestGoneIfUnchanged writes the memo of a block whose inline
// embed ended with its request (the client went away, or the request's own
// deadline passed) and not with the budget: the caller_timeout memo of a cut,
// next_attempt_at = now() and one attempt for a block without a memo, except
// that an existing memo keeps its attempts. Such a cut says nothing about the
// block and is no attempt at it. Bound to the text the attempt read like
// RecordEmbedFailureIfUnchanged, with the same lock order and the same
// in-place update of a caller_timeout memo when the block row is held.
func RecordEmbedRequestGoneIfUnchanged(ctx context.Context, q pgxdb.Execer, blockID, title, content, normalizedErr string) (written bool, err error) {
	args := []any{blockID, normalizedErr, string(EmbedFailureCallerTimeout), title, content}
	tag, err := q.Exec(ctx,
		attemptGuard(4, 5, "id")+
			`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
			 SELECT b.id, NULL, 1, $2, $3, now() FROM b
			 ON CONFLICT (block_id) WHERE migration_id IS NULL
			 DO UPDATE SET last_error      = EXCLUDED.last_error,
			               last_class      = EXCLUDED.last_class,
			               next_attempt_at = now()`, args...)
	if err != nil {
		return false, fmt.Errorf("store: record caller_timeout memo of an ended request: %w", err)
	}
	if tag.RowsAffected() == 1 {
		return true, nil
	}
	tag, err = q.Exec(ctx,
		`UPDATE context_embed_failures SET last_error = $2, last_class = $3, next_attempt_at = now()
		  WHERE block_id = $1 AND migration_id IS NULL AND last_class = $3
		    AND `+attemptText(4, 5), args...)
	if err != nil {
		return false, fmt.Errorf("store: record caller_timeout memo of an ended request (in place): %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// attemptGuard is the head of a memo write bound to the text an embed attempt
// read: the CTE b holds the block's row ($1), locked FOR NO KEY UPDATE, while
// it still holds that title ($titleArg) and content ($contentArg) and is
// still pending (EmbedPendingPredicate), and no row otherwise, also when
// another transaction holds the row (SKIP LOCKED,
// RecordEmbedFailureIfUnchanged). A block that another writer embedded
// meanwhile, at the same text, needs no memo; one left on it would outlive
// the vector. cols are the columns of b.
func attemptGuard(titleArg, contentArg int, cols string) string {
	return fmt.Sprintf(`WITH b AS (
		SELECT %s FROM context_blocks
		 WHERE id = $1 AND title = $%d AND content = $%d AND %s
		 FOR NO KEY UPDATE SKIP LOCKED)
		`, cols, titleArg, contentArg, EmbedPendingPredicate)
}

// attemptText is the condition of an in-place memo update bound to the text
// an embed attempt read: the block ($1) holds that title and content and is
// pending, read without a lock (the memo row the update locks orders it
// against a content writer, which releases that row in its transaction).
func attemptText(titleArg, contentArg int) string {
	return fmt.Sprintf(`EXISTS (SELECT 1 FROM context_blocks
		                WHERE id = $1 AND title = $%d AND content = $%d AND %s)`,
		titleArg, contentArg, EmbedPendingPredicate)
}

// CallerTimeoutHoldEnd is the SQL expression of the time a caller_timeout
// memo's hold ends: the cut (next_attempt_at) plus backoff_base *
// 2^(attempts-1), at most backoff_cap. The exponent is capped so a block that
// has been cut for years cannot overflow the float. memo is the memo row's
// alias, base and cap the placeholders of the base and the cap in seconds.
// With the arm off the query path retries such a block once its hold has
// ended (handler.backfillPicker.pickOverran), and a cut counts as an attempt
// only after it (regularMemoParts), so one definition serves both.
func CallerTimeoutHoldEnd(memo, base, capArg string) string {
	return memo + `.next_attempt_at + make_interval(secs =>
	        least(` + base + `::float8 * power(2, least(` + memo + `.attempts, 31) - 1), ` + capArg + `::float8))`
}

// regularMemoParts are what the upsert of a block's regular-backfill memo
// (regularMemoSQL) and its in-place update (regularMemoUpdateSQL) share for
// class: the number of placeholders before the text, the next_attempt_at of a
// new memo, the SET of an existing one, and the condition under which an
// existing one is updated at all (empty: always). Placeholders: $1 the block,
// $2 the normalized error, $3 the class; for a class with a backoff $4 and
// $5 the base and the cap in seconds.
//
// A caller_timeout cut counts an attempt only once the hold of the memo in
// place has passed (issue #240, part 2): the hold a caller_timeout memo gives
// (CallerTimeoutHoldEnd) or, for a memo of another class, its
// next_attempt_at. A cut inside it leaves the memo as it is. N requests that
// pick the same block before the first cut is written and are all cut count
// one attempt, not N, whichever ctxd instance they run on, since the write
// decides under the memo row's lock; counted N times, with the arm off the
// block's next hold was backoff_base * 2^(N-1).
func regularMemoParts(class EmbedFailureClass) (nArgs int, next, set, hold string) {
	switch {
	case isInfinityClass(class):
		return 3, `'infinity'`, `attempts        = context_embed_failures.attempts + 1,
		               last_error      = $2,
		               last_class      = $3,
		               next_attempt_at = 'infinity'`, ""
	case class == EmbedFailureCallerTimeout:
		// Eligible for the background arm at once: the backoff curve is
		// for a backend that failed, this backend only took longer than
		// the caller could wait. Pfad A's own exclusion of this class is
		// in the predicate, not in the timestamp. The timestamp is the
		// time of the cut, and with the arm off Pfad A counts the block's
		// backoff, and its place in the order, from it.
		return 5, `now()`, `attempts        = context_embed_failures.attempts + 1,
		               last_error      = $2,
		               last_class      = $3,
		               next_attempt_at = now()`,
			`CASE WHEN context_embed_failures.last_class = '` + string(EmbedFailureCallerTimeout) + `'
			            THEN ` + CallerTimeoutHoldEnd("context_embed_failures", "$4", "$5") + `
			            ELSE context_embed_failures.next_attempt_at
			       END <= now()`
	default:
		return 5, `now() + make_interval(secs => least($4::float8 * power(2, 0), $5::float8))`,
			`attempts        = context_embed_failures.attempts + 1,
		               last_error      = $2,
		               last_class      = $3,
		               next_attempt_at = now() + make_interval(secs =>
		                   least($4::float8 * power(2, context_embed_failures.attempts), $5::float8))`, ""
	}
}

// regularMemoSQL is the upsert of a block's regular-backfill memo
// (migration_id IS NULL) for class (regularMemoParts); bound to the
// attempt's text (attemptGuard) the two placeholders after the class's are
// the title and the content.
func regularMemoSQL(class EmbedFailureClass, bound bool) string {
	nArgs, next, set, hold := regularMemoParts(class)
	where := ""
	if hold != "" {
		where = `
		 WHERE ` + hold
	}
	insert := `INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		 VALUES ($1, NULL, 1, $2, $3, ` + next + `)`
	head := ""
	if bound {
		cols := "id"
		if isInfinityClass(class) {
			// Such a park also drops the stale vector (RecordEmbedFailure),
			// here of the row b locked, in the same statement.
			cols = "id, embedding_stale"
			head = attemptGuard(nArgs+1, nArgs+2, cols) + `, dropped AS (
		UPDATE context_blocks SET embedding = NULL, embed_model = NULL, embedding_stale = false
		  FROM b WHERE context_blocks.id = b.id AND b.embedding_stale)
		`
		} else {
			head = attemptGuard(nArgs+1, nArgs+2, cols)
		}
		insert = `INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		 SELECT b.id, NULL, 1, $2, $3, ` + next + ` FROM b`
	}
	return head + insert + `
		 ON CONFLICT (block_id) WHERE migration_id IS NULL
		 DO UPDATE SET ` + set + where
}

// regularMemoUpdateSQL is the in-place update of a block's regular memo of
// class when the bound upsert did not get the block's row
// (RecordEmbedFailureIfUnchanged): the memo of the same class only, under
// the class's condition, while the block holds the attempt's text
// (attemptText). Not for an infinity class, whose park also drops the stale
// vector and so needs the block row.
func regularMemoUpdateSQL(class EmbedFailureClass) string {
	nArgs, _, set, hold := regularMemoParts(class)
	cond := ""
	if hold != "" {
		cond = `
		    AND ` + hold
	}
	return `UPDATE context_embed_failures SET ` + set + `
		  WHERE block_id = $1 AND migration_id IS NULL AND last_class = $3` + cond + `
		    AND ` + attemptText(nArgs+1, nArgs+2)
}

// regularMemoArgs are the arguments of regularMemoSQL before the text.
func regularMemoArgs(blockID string, class EmbedFailureClass, normalizedErr string, backoffBase, backoffCap time.Duration) []any {
	if isInfinityClass(class) {
		return []any{blockID, normalizedErr, string(class)}
	}
	return []any{blockID, normalizedErr, string(class), backoffBase.Seconds(), backoffCap.Seconds()}
}

// isInfinityClass reports the classes whose memo is a permanent park
// (next_attempt_at = 'infinity') instead of an exponential backoff: oversize
// (block > slot window — cannot succeed without a content change, design/04
// §4.4) and sensitivity_ineligible (block sensitivity × backend trust —
// cannot succeed without a config change, §5 Bruchpfad 3).
func isInfinityClass(class EmbedFailureClass) bool {
	return class == EmbedFailureOversize || class == EmbedFailureSensitivityIneligible
}

// RecordEmbedFailureForMigration is the MIGRATION-scoped sibling of
// RecordEmbedFailure (design/04 §4.4, W04-4): it upserts the block's memo
// row under (block_id, migration_id) — the partial-unique index
// idx_embed_failures_migration from migration 113 — so one block can carry
// an independent memo per migration AND per regular backfill (migration_id
// NULL) without the two bookkeeping streams overwriting each other.
// Same pgxdb.Execer doctrine as RecordEmbedFailure: the migration worker
// passes the SAME tx that holds its FOR UPDATE SKIP LOCKED pick, so the
// memo commit is atomic with the pick. Same server-side backoff exponent
// (base * 2^(attempts-1), capped); oversize AND sensitivity_ineligible
// short-circuit to next_attempt_at='infinity' (permanent park, §4.4).
func RecordEmbedFailureForMigration(ctx context.Context, q pgxdb.Execer, blockID, migrationID string, class EmbedFailureClass, normalizedErr string, backoffBase, backoffCap time.Duration) error {
	if migrationID == "" {
		return fmt.Errorf("store: record embed failure (migration): migration id is required (block %s)", blockID)
	}
	if isInfinityClass(class) {
		_, err := q.Exec(ctx,
			`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
			 VALUES ($1, $2::uuid, 1, $3, $4, 'infinity')
			 ON CONFLICT (block_id, migration_id) WHERE migration_id IS NOT NULL
			 DO UPDATE SET attempts        = context_embed_failures.attempts + 1,
			               last_error      = EXCLUDED.last_error,
			               last_class      = EXCLUDED.last_class,
			               next_attempt_at = 'infinity'`,
			blockID, migrationID, normalizedErr, string(class),
		)
		if err != nil {
			return fmt.Errorf("store: record embed failure (migration, %s): %w", class, err)
		}
		return nil
	}

	baseSecs := backoffBase.Seconds()
	capSecs := backoffCap.Seconds()
	_, err := q.Exec(ctx,
		`INSERT INTO context_embed_failures (block_id, migration_id, attempts, last_error, last_class, next_attempt_at)
		 VALUES ($1, $2::uuid, 1, $3, $4, now() + make_interval(secs => least($5::float8 * power(2, 0), $6::float8)))
		 ON CONFLICT (block_id, migration_id) WHERE migration_id IS NOT NULL
		 DO UPDATE SET attempts        = context_embed_failures.attempts + 1,
		               last_error      = EXCLUDED.last_error,
		               last_class      = EXCLUDED.last_class,
		               next_attempt_at = now() + make_interval(secs =>
		                   least($5::float8 * power(2, context_embed_failures.attempts), $6::float8))`,
		blockID, migrationID, normalizedErr, string(class), baseSecs, capSecs,
	)
	if err != nil {
		return fmt.Errorf("store: record embed failure (migration): %w", err)
	}
	return nil
}

// EmbedMigrationFailureExcludedPredicate is the migration-scoped twin of
// EmbedFailureExcludedPredicate (design/04 §3.3, Migrations-Backfill row):
// the migration worker's peek/pick AND it in so a block with an outstanding
// migration-scoped backoff — or a permanently parked infinity memo — is
// skipped instead of re-picked every cycle. POSITIONAL CONTRACT: the
// migration id must be the query's $1 (both worker queries put it there);
// like its sibling it references context_blocks.id, so it composes only
// into queries whose outer FROM is the bare context_blocks table.
const EmbedMigrationFailureExcludedPredicate = `
	AND NOT EXISTS (
		SELECT 1 FROM context_embed_failures f
		WHERE f.block_id = context_blocks.id
		  AND f.migration_id = $1::uuid
		  AND f.next_attempt_at > now()
	)`

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

// EmbedFailureQueryPathExcludedPredicate is Pfad A's (query.go
// backfillPending) variant of EmbedFailureExcludedPredicate: the same
// backoff exclusion PLUS every block memoized as caller_timeout. A block
// that once overran the sync budget is the background arm's job from then
// on; re-picking it inline would burn the budget of every later query for a
// block the arm is about to embed anyway (and, while the arm holds its FOR
// UPDATE row, embed it a second time in parallel). The background arm keeps
// the plain predicate, so for it a caller_timeout memo (next_attempt_at =
// now()) is immediately eligible. backfillPending uses this variant for
// its picks with the arm on or off. With the arm off
// (embed_backfill.interval = 0) nothing else would embed a caller_timeout
// block, so the old end of its pick reads those blocks with a statement of
// its own (handler.backfillPicker.pickOverran): the ones whose backoff,
// counted from the cut, has passed, by the time of the cut. The hand-off so
// never leaves a block to nobody (review finding M1), a block that cannot
// fit the budget costs one request per backoff period, and it does not hold
// the others, which were pending before its latest cut and so stand ahead
// of it. Same positional contract as its sibling: bare context_blocks as
// the outer FROM.
const EmbedFailureQueryPathExcludedPredicate = `
	AND NOT EXISTS (
		SELECT 1 FROM context_embed_failures f
		WHERE f.block_id = context_blocks.id
		  AND f.migration_id IS NULL
		  AND (f.next_attempt_at > now() OR f.last_class = 'caller_timeout')
	)`

// BackfillReleaseMaxLimit is the most regular memos one
// embed-backfill-release call deletes (one statement, one page). A fixed
// bound, not a policy: it keeps the statement's hold on the memo rows it
// takes short and its WAL bounded; a larger set is released page by page.
const BackfillReleaseMaxLimit = 1000

// ErrBackfillReleaseSelector is a release that does not name exactly one of
// a block or a releasable class, names a page size out of range, or
// confirms without the as_of of its dry run.
var ErrBackfillReleaseSelector = errors.New("store: backfill release: exactly one of a block id or a class (wire, oversize), a limit of 1 to 1000, and a confirm together with the dry run's as_of")

// ErrBackfillReleaseConfirmMismatch is a confirmed release whose fresh count
// differs from the number it confirms: nothing was deleted.
var ErrBackfillReleaseConfirmMismatch = errors.New("store: backfill release: the memos in range are not the confirmed number")

// BackfillRelease is one release of regular backfill memos (issue #236):
// the memo of one block (BlockID) or the memos of one failure class (Class),
// in the reader's range. The reader is part of it and its zero value reaches
// nothing, as in PurgeQuery: a server-admin sets AllTenants (every memo);
// every other caller sets TenantID and reaches only the memos of blocks in
// the scopes that tenant owns (context_tenant_scopes), so a block of another
// tenant and a block that does not exist count alike: zero. Confirm nil is a
// dry run, which answers its count and its AsOf; a confirmed release passes
// both back, and takes only the memos the dry run could count (first_seen
// not after AsOf), and only when their fresh count equals Confirm. Limit is
// the page size (0 = BackfillReleaseMaxLimit).
type BackfillRelease struct {
	BlockID    string
	Class      EmbedFailureClass
	AllTenants bool
	TenantID   string
	Limit      int
	Confirm    *int64
	AsOf       *time.Time
}

// BackfillReleaseResult is what a release counted and did. AsOf is the time
// of the dry run's count, which bounds every page of the release; Counted the
// number of memos in range not written after AsOf, when the call counted;
// Released the number it deleted; Remaining what is left of them after it
// (Counted - Released: memos it skipped because another transaction held
// them, and memos past the page).
type BackfillReleaseResult struct {
	DryRun    bool
	AsOf      time.Time
	Counted   int64
	Released  int64
	Remaining int64
}

// ReleaseBackfillMemos deletes regular backfill memos (migration_id IS NULL)
// so the backfill takes their blocks again: the next pick of the block's
// class reads it, embeds it when it fits now (a raised
// embed_backfill.max_tokens, an embed backend with a larger context, a
// backend that is up again), and parks it again after one attempt when it
// does not. Without it a parked block came back only through a change of its
// title or content (InvalidateEmbeddingTx); a write that re-sends the same
// text does not release it (#216). A migration's memos (migration_id set)
// are that migration's bookkeeping and are never taken, and neither is a
// caller_timeout memo (releasableClasses).
//
// The pages of one release are bound to its dry run: a memo whose
// first_seen is after AsOf (the start of the transaction that wrote it) is
// neither counted nor deleted. A released block that does not fit is parked
// again with a new memo, so without the bound every page would find the
// blocks the pages before it released, and a release paged to remaining 0
// would not end while blocks keep not fitting; a memo written between a
// page's count and its delete stays out of that page as well.
//
// Locks: memo rows only, and none it would have to wait for — the page is
// taken FOR UPDATE SKIP LOCKED, so a memo another transaction holds (the
// backfill writing its verdict on a block it holds, a content writer
// releasing the memo of a block it changes) is skipped and stays in
// Remaining. The release never waits on a row lock, so it cannot be part of
// a wait cycle: not with the backfill and the content writers, which take a
// block's row before its memo row, nor with a batch writer that holds the
// memo of one block while it waits for the next block. It reads the block
// rows of the reader rule without a lock.
func ReleaseBackfillMemos(ctx context.Context, pool *pgxpool.Pool, r BackfillRelease) (BackfillReleaseResult, error) {
	where, args, err := r.where()
	if err != nil {
		return BackfillReleaseResult{}, err
	}
	res := BackfillReleaseResult{DryRun: r.Confirm == nil}
	if !r.AllTenants && r.TenantID == "" {
		return res, nil // the zero reader reaches nothing
	}
	if res.DryRun {
		if err := pool.QueryRow(ctx, `SELECT count(*), now() FROM context_embed_failures f WHERE `+where, args...).Scan(&res.Counted, &res.AsOf); err != nil {
			return res, fmt.Errorf("store: backfill release: count: %w", err)
		}
		res.Remaining = res.Counted
		return res, nil
	}
	res.AsOf = *r.AsOf
	where += fmt.Sprintf(` AND f.first_seen <= $%d`, len(args)+1)
	args = append(args, res.AsOf)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_failures f WHERE `+where, args...).Scan(&res.Counted); err != nil {
		return res, fmt.Errorf("store: backfill release: count: %w", err)
	}
	if res.Counted != *r.Confirm {
		res.Remaining = res.Counted
		return res, ErrBackfillReleaseConfirmMismatch
	}
	limit := r.Limit
	if limit == 0 {
		limit = BackfillReleaseMaxLimit
	}
	tag, err := pool.Exec(ctx, fmt.Sprintf(`WITH page AS (
		SELECT f.block_id FROM context_embed_failures f
		 WHERE %s
		 LIMIT $%d
		 FOR UPDATE SKIP LOCKED)
		DELETE FROM context_embed_failures d USING page
		 WHERE d.block_id = page.block_id AND d.migration_id IS NULL`, where, len(args)+1),
		append(args, limit)...)
	if err != nil {
		return res, fmt.Errorf("store: backfill release: %w", err)
	}
	res.Released = tag.RowsAffected()
	res.Remaining = max(res.Counted-res.Released, 0)
	return res, nil
}

// where is the condition of r's memos on context_embed_failures f, and its
// arguments: the regular memos of the block or of the class, and for a
// tenant reader only those of blocks in the tenant's scopes.
func (r BackfillRelease) where() (string, []any, error) {
	if (r.BlockID == "") == (r.Class == "") || r.Limit < 0 || r.Limit > BackfillReleaseMaxLimit ||
		(r.Confirm != nil) != (r.AsOf != nil) {
		return "", nil, ErrBackfillReleaseSelector
	}
	where := `f.migration_id IS NULL AND `
	var args []any
	if r.BlockID != "" {
		where += `f.block_id = $1::uuid AND f.last_class = ANY($2::text[])`
		args = append(args, r.BlockID, releasableClassNames())
	} else {
		if !slices.Contains(releasableClasses, r.Class) {
			return "", nil, ErrBackfillReleaseSelector
		}
		where += `f.last_class = $1`
		args = append(args, string(r.Class))
	}
	if !r.AllTenants {
		args = append(args, r.TenantID)
		where += fmt.Sprintf(`
		   AND EXISTS (SELECT 1 FROM context_blocks b JOIN context_tenant_scopes ts ON ts.scope = b.scope
		                WHERE b.id = f.block_id AND ts.tenant_id = $%d::uuid)`, len(args))
	}
	return where, args, nil
}

// releasableClassNames is releasableClasses as the text[] of a statement.
func releasableClassNames() []string {
	out := make([]string, len(releasableClasses))
	for i, c := range releasableClasses {
		out[i] = string(c)
	}
	return out
}
