// Package ctxcheckpoint presents the ctx corpus' own compaction checkpoints as
// a distillsource.Source.
//
// The material is written by the hermes ctx_checkpoint plugin into
// context_blocks: one MANIFEST block per compaction generation, carrying the
// ordered ids of its PART blocks in metadata.source_block_ids, and the parts
// themselves holding the transcript text. Both are type checkpoint; the arm's
// own output type is insight and is written elsewhere.
//
// Four decisions shape this reader, and each one is measured against the live
// corpus rather than assumed:
//
//  1. The readable ATOM is a manifest with all of its parts, never a single
//     part. 93.7 % of the parts carry no message header (5133 of 5477), so a
//     part read on its own has no attribution at all; the role is
//     reconstructible only by walking a manifest's parts in array order and
//     carrying the last header forward.
//  2. The watermark is derived from created_at in MICROSECONDS, not from the
//     uuidv7 prefix. PG18's uuidv7 is only millisecond-monotonic — its rand_a
//     bits do not correlate with the sub-millisecond — so the block order
//     within one millisecond is random. Ordering by the timestamp makes the
//     watermark independent of that.
//  3. A watermark is not unique, so the reader never advances into the middle
//     of a watermark group: the batch either takes a group whole or ends in
//     front of it and reports Complete=false. That is what makes correctness
//     independent of collision-freeness, which holds today (0 collisions over
//     5961 blocks) and will not hold at target scale.
//  4. Material is CHUNKED, never head-capped. A part body is ~36000 characters
//     against a 4000-rune cap; capping would cover 11 % of it and — because the
//     dedup ledger hashes the text that was shown — mark the rest as seen for
//     good.
//
// Layering (F1): this package imports neither internal/config nor
// internal/events. Every tunable is handed to New; the arm owns the keys.
//
// Source: https://github.com/GottZ/ctx
package ctxcheckpoint

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/promptguard"
)

// typeName is the block type of both manifests and parts. It is a property of
// the source, not a setting: an operator who points this reader at another type
// is not reading checkpoints any more, and the manifest/part shape this file
// parses would not be there.
const typeName = "checkpoint"

// sourceUntrusted is this source's answer about its own material, and it is a
// constant because the answer is a property of the type rather than of a row.
// The corpus is a foreign agent transcript: 78.9 % of the parts carry diff
// markers and 34.8 % code fences (measured over all 5477), so the majority of
// the text is what a terminal, a repo or the network produced. The block type
// registry mirrors this with retrieval.untrusted on checkpoint; that mirror is
// the arm's write-side concern, and duplicating the lookup here would let the
// two drift without either side noticing.
const sourceUntrusted = true

// Options are the values the arm hands the reader. None of them is read from
// configuration here — that is the F1 layering rule, and it is the reason this
// package has no config import to keep honest.
type Options struct {
	// Label names the source in the journal's source_key. It must differ from
	// the other source's label, or the two watermark series merge.
	Label string

	// Scope is the READ scope, and the arm holds exactly one value for read and
	// write. A reader scope that differs from the write scope would either open
	// a cross-scope propagation path or read zero rows forever while the
	// journal reports no_new_rows — a silent null operation with no diagnosis.
	Scope string

	// Category selects the checkpoint category (live: compaction-checkpoints).
	Category string

	// MaxSessions caps the candidate list per tick.
	MaxSessions int

	// SessionHorizon caps Sessions to roots with activity inside this window.
	// Zero means no cap.
	//
	// The trade the cap makes is explicit: a root whose newest manifest is
	// older than the horizon becomes invisible to the candidate list. Reading
	// it still works — Read, Head and HasNew take a session id and never
	// consult the horizon.
	//
	// What the cap buys is not a constant, and the quantity it depends on is
	// the SELECTIVITY of the window — how much of the corpus falls inside it —
	// which is set by the ingest RATE, not by the row count. Measured:
	//
	//   - Restricting the aggregation to MANIFESTS is what removes the
	//     sequential scan (the design's full aggregation over every checkpoint
	//     block is a Seq Scan — 5955 rows, 3672 buffers on the live corpus).
	//   - Above roughly 1 % selectivity the horizon adds nothing on the buffer
	//     axis: the planner bitmaps every manifest and applies the timestamp as
	//     a heap filter, so the cost tracks the manifest COUNT.
	//   - Below it the planner switches to idx_context_created and the cost
	//     drops by two orders of magnitude.
	//
	// At the live ingest rate (140.1 blocks/day) a million blocks span ~19.6
	// years, so the 30-day default sits far below the crossover. An earlier
	// measurement concluded the opposite from a fixture that compressed a
	// million rows into 11.6 days — there the default covered the whole corpus
	// and had nothing to cut. The scale test now derives its time span from the
	// live rate and carries the crossover as a measured series.
	//
	// A planner estimate contributes: metadata ? 'source_block_ids' is
	// estimated at 97 rows against 17858 actual (~184x), so the bitmap looks
	// cheap until the window is narrow enough to win outright.
	SessionHorizon time.Duration

	// MaxManifests caps how many manifest heads one Read considers. It bounds
	// the cheap query; the item cap bounds the expensive one.
	MaxManifests int
}

// Source reads checkpoints over an existing ctx pool.
type Source struct {
	pool *pgxpool.Pool
	opt  Options
}

// The contract this package exists to satisfy, checked at compile time.
var _ distillsource.Source = (*Source)(nil)

// New builds a reader over an existing pool.
//
// The pool is BORROWED, not owned: it is the daemon's shared ctx pool, and
// Close must therefore not touch it. That is the one place where this source
// deviates from "a Source owns what it was built over", and it deviates because
// the alternative — closing the daemon's pool when an arm shuts down — is worse
// than the inconsistency.
//
// The caps are validated here for the same reason the strings are: a
// non-positive MaxSessions used to make Sessions return nothing at all, and a
// non-positive MaxManifests used to fall back to a window of one. Both are the
// silent null operation D-02 §4.2.1(b) wants to see RED — the arm would journal
// no_new_rows forever and look healthy doing it — so they are refused at
// construction instead of absorbed at query time.
func New(pool *pgxpool.Pool, opt Options) (*Source, error) {
	switch {
	case pool == nil:
		return nil, fmt.Errorf("%w: nil pool", distillsource.ErrSourceUnavailable)
	case opt.Label == "":
		return nil, fmt.Errorf("%w: empty label", distillsource.ErrSourceUnavailable)
	case opt.Scope == "":
		return nil, fmt.Errorf("%w: empty scope", distillsource.ErrSourceUnavailable)
	case opt.Category == "":
		return nil, fmt.Errorf("%w: empty category", distillsource.ErrSourceUnavailable)
	case opt.MaxSessions <= 0:
		return nil, fmt.Errorf("%w: max sessions must be positive, got %d",
			distillsource.ErrSourceUnavailable, opt.MaxSessions)
	case opt.MaxManifests <= 0:
		return nil, fmt.Errorf("%w: max manifests must be positive, got %d",
			distillsource.ErrSourceUnavailable, opt.MaxManifests)
	}
	return &Source{pool: pool, opt: opt}, nil
}

// Label names the source in logs, journals and the source_key.
func (s *Source) Label() string { return s.opt.Label }

// Close is a no-op: the pool belongs to the daemon (see New).
func (s *Source) Close() error { return nil }

// Sessions lists candidate root sessions, newest manifest first.
//
// The aggregation runs over MANIFESTS, not over every checkpoint block. A root
// whose newest block is a session pointer but whose newest manifest is old has
// no new work in it, and ranking it by the pointer would push a root with
// actual material off the end of the list. The live corpus carries 160 such
// pointer blocks against 318 manifests.
//
// Every Ref carries Watermark 0. The aggregate does name a maximum, but it is
// the maximum of the CANDIDATE query, and handing it out as a head invites a
// caller to advance to a value no batch ever covered. Head exists for that
// question and answers it against the same rows Read walks.
func (s *Source) Sessions(ctx context.Context) ([]distillsource.Ref, error) {
	const q = `
SELECT metadata->>'root_session_id' AS root
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND type_name = $1
   AND scope     = $2
   AND category  = $3
   AND NOT is_archived
   AND metadata ? 'source_block_ids'
   AND ($4::timestamptz IS NULL OR created_at > $4::timestamptz)
 GROUP BY 1
 ORDER BY max(created_at) DESC, 1 ASC
 LIMIT $5`

	// The id tiebreak makes the candidate list deterministic across ticks when
	// two roots share a newest timestamp. Correctness does not depend on it —
	// but A02-6 asks for two runs over the same range to produce identical
	// ledger numbers, and a non-deterministic candidate order would defeat that
	// before the ledger ever sees a row.
	rows, err := s.pool.Query(ctx, q, typeName, s.opt.Scope, s.opt.Category, s.horizonCutoff(), s.opt.MaxSessions)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var out []distillsource.Ref
	for rows.Next() {
		var root string
		if err := rows.Scan(&root); err != nil {
			return nil, mapErr(err)
		}
		out = append(out, distillsource.Ref{Session: root})
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// horizonCutoff turns the horizon into a nullable timestamp bound. A zero
// horizon yields NULL, which the query reads as "no cap" — expressed in SQL so
// there is one statement and one plan shape rather than two.
func (s *Source) horizonCutoff() *time.Time {
	if s.opt.SessionHorizon <= 0 {
		return nil
	}
	t := time.Now().Add(-s.opt.SessionHorizon)
	return &t
}

// HasNew stops at the first manifest above after.
func (s *Source) HasNew(ctx context.Context, sess string, after int64) (bool, error) {
	const q = `
SELECT EXISTS (
  SELECT 1 FROM context_blocks
   WHERE (metadata->>'root_session_id') IS NOT NULL
     AND metadata->>'root_session_id' = $1
     AND type_name = $2 AND scope = $3 AND category = $4
     AND NOT is_archived
     AND metadata ? 'source_block_ids'
     AND created_at > to_timestamp(($5::bigint - 1)::double precision / 1000000)
     AND (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT > $5)`

	var ok bool
	err := s.pool.QueryRow(ctx, q, sess, typeName, s.opt.Scope, s.opt.Category, after).Scan(&ok)
	return ok, mapErr(err)
}

// Head is the highest manifest watermark of the session — the regression check
// alone. A session with no manifest yields 0, which reads as "nothing here",
// not as a regression.
func (s *Source) Head(ctx context.Context, sess string) (int64, error) {
	const q = `
SELECT COALESCE(max((EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT), 0)
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND metadata->>'root_session_id' = $1
   AND type_name = $2 AND scope = $3 AND category = $4
   AND NOT is_archived
   AND metadata ? 'source_block_ids'`

	var head int64
	err := s.pool.QueryRow(ctx, q, sess, typeName, s.opt.Scope, s.opt.Category).Scan(&head)
	return head, mapErr(err)
}

// QuietFor reports how long the session's newest live checkpoint block has been
// standing, clamped at zero.
//
// It measures over ALL live blocks of the root, not only manifests: the
// question is whether a human is still working in that session, and a part
// written seconds ago answers it just as well as the manifest that will list it
// a moment later.
func (s *Source) QuietFor(ctx context.Context, sess string, now time.Time) (time.Duration, error) {
	const q = `
SELECT max(created_at)
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND metadata->>'root_session_id' = $1
   AND type_name = $2 AND scope = $3 AND category = $4
   AND NOT is_archived`

	var newest *time.Time
	if err := s.pool.QueryRow(ctx, q, sess, typeName, s.opt.Scope, s.opt.Category).Scan(&newest); err != nil {
		return 0, mapErr(err)
	}
	if newest == nil {
		return 0, fmt.Errorf("%w: session %q", distillsource.ErrNoActiveRows, sess)
	}
	// A timestamp in the future is a clock regression, not negative idle time.
	if d := now.Sub(*newest); d > 0 {
		return d, nil
	}
	return 0, nil
}

// manifestHead is one manifest as the cheap query returns it: enough to decide
// what to read, without any part content.
type manifestHead struct {
	id      string
	wm      int64
	partIDs []string
}

// Read returns the material of the session above after.
//
// The unit of progress is a MANIFEST, so maxItems is an ATOM SELECTION bound
// here and not a hard item ceiling: the first manifest of a batch is always
// delivered whole, even when its chunks exceed maxItems. A hard ceiling would
// stall permanently on the large manifests this corpus actually contains — 56
// parts at up to 13 chunks each (both measured) is far past a rows_per_read of
// 400, and a batch that can never cover its first atom never advances its
// watermark. Every further manifest is only taken while the item count stays
// below the cap AND the next manifest begins a new watermark group — a group is
// as indivisible as a manifest, for the same reason: the watermark cannot name
// half of one. maxRunes stays HARD: that one is enforced by the chunker per
// item.
//
// The maxItems overshoot is the contract's own atom exception
// (distillsource.go, Read), not a deviation of this implementation.
func (s *Source) Read(ctx context.Context, sess string, after int64, maxItems, maxRunes int) (distillsource.Batch, error) {
	if maxItems <= 0 || maxRunes <= 0 {
		// Nothing was read, so nothing is covered. A source never substitutes a
		// cap of its own for a caller's missing one.
		return distillsource.Batch{Watermark: after}, nil
	}
	heads, groupCut, err := s.manifestHeads(ctx, sess, after)
	if err != nil {
		return distillsource.Batch{Watermark: after}, err
	}
	if len(heads) == 0 {
		// No manifest was named, so nothing is covered — but a read that found
		// nothing is complete unless a group cut is what emptied it.
		return distillsource.Batch{Watermark: after, Complete: !groupCut}, nil
	}

	b := distillsource.Batch{Watermark: after, Complete: true}
	for i, h := range heads {
		// The cap may only stop the batch on a GROUP BOUNDARY. Watermarks are
		// not unique, and b.Watermark already stands on the last delivered
		// manifest — so stopping while the next manifest shares that watermark
		// would report a value covering material this batch never handed out,
		// and the next read (wm > after) could never reach it again. That is
		// the same silent, permanent loss the end-of-window guard in
		// manifestHeads exists to prevent; it just happens one edge further in.
		//
		// A group is therefore always finished before the cap is honoured,
		// which is the rule watermarkGroup already applies at the other edge.
		// Both are the same decision: a watermark group is indivisible, exactly
		// as a manifest is.
		if i > 0 && len(b.Items) >= maxItems && h.wm != b.Watermark {
			// Stopping here leaves material behind, which is what the next tick
			// is for. The batch stays complete: its watermark covers whole
			// atoms and whole groups, and a group cut off at the FAR end of the
			// window is then irrelevant — the next read starts below it and
			// meets it in full.
			//
			// Reporting incomplete here instead would be the expensive kind of
			// wrong: with many manifests per root and a tight cap, a group
			// sitting at the window edge would make EVERY batch incomplete and
			// the arm would never advance at all.
			return b, nil
		}
		items, err := s.readManifest(ctx, h, maxRunes)
		if err != nil {
			return distillsource.Batch{Watermark: after}, err
		}
		b.Items = append(b.Items, items...)
		// The manifest is covered even when it produced no item at all — an
		// unresolvable or empty part list is read material, and re-reading it
		// every tick would be a permanent stall dressed up as progress.
		b.Watermark = h.wm
	}
	// The window was walked to its end, so a group cut at that end is the thing
	// the caller must not advance past.
	b.Complete = !groupCut
	return b, nil
}

// manifestHeads reads the manifest heads above after and reports whether the
// batch had to end in front of a cut watermark group.
//
// The range is expressed TWICE on purpose. `(EXTRACT(EPOCH …) * 1000000)::BIGINT
// > $5` is the exact half-open bound, but it is an expression over created_at
// and therefore not sargable: the planner cannot use it as a range on the
// second column of idx_blocks_checkpoint_root, so every read would filter the
// root's ENTIRE manifest history instead of just the new tail. The added
// `created_at > to_timestamp(…)` is the sargable prefilter that bounds the scan;
// it is deliberately one microsecond WIDER than the exact bound, so float
// rounding can only ever let too much through, never too little. The BIGINT
// comparison then does the fine cut.
//
// The SELECT list is narrower than D-02 §4.2.1, and deliberately so: the design
// reads five columns, this reads three. metadata->>'sha256' and
// metadata->>'parent_manifest_id' are not selected because nothing in this wave
// consumes them — the citation anchor is (BlockID, ChunkIndex) per §0.1, and no
// parent chain is walked. A02-9 will want sha256 for its evidence gate; it is
// one column away, and this note exists so that absence reads as a decision
// rather than an oversight.
//
// It asks for one row more than the cap. If that extra row shares the last
// kept row's watermark, the group is split, and the split is resolved by
// dropping the whole tail group rather than by guessing: advancing into the
// middle of a group loses every manifest of that group that fell outside the
// window, permanently, because the next read starts above the watermark.
//
// A group that fills the entire window is the one case where dropping it would
// stall, so it is kept whole and the cap is exceeded instead.
func (s *Source) manifestHeads(ctx context.Context, sess string, after int64) ([]manifestHead, bool, error) {
	const q = `
SELECT id::text,
       (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT AS wm,
       ARRAY(SELECT jsonb_array_elements_text(metadata->'source_block_ids'))
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND metadata->>'root_session_id' = $1
   AND type_name = $2 AND scope = $3 AND category = $4
   AND NOT is_archived
   AND metadata ? 'source_block_ids'
   AND created_at > to_timestamp(($5::bigint - 1)::double precision / 1000000)
   AND (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT > $5
 ORDER BY created_at ASC, id ASC
 LIMIT $6`

	// No fallback for a non-positive cap: New refuses to build a source with
	// one, so a silent window of 1 can no longer arise here.
	limit := s.opt.MaxManifests
	rows, err := s.pool.Query(ctx, q, sess, typeName, s.opt.Scope, s.opt.Category, after, limit+1)
	if err != nil {
		return nil, false, mapErr(err)
	}
	defer rows.Close()

	var heads []manifestHead
	for rows.Next() {
		var h manifestHead
		if err := rows.Scan(&h.id, &h.wm, &h.partIDs); err != nil {
			return nil, false, mapErr(err)
		}
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapErr(err)
	}
	if len(heads) <= limit {
		return heads, false, nil
	}

	// One row beyond the cap was read: it is never delivered, it only answers
	// whether the last kept watermark is complete.
	overflow := heads[limit]
	heads = heads[:limit]
	if heads[len(heads)-1].wm != overflow.wm {
		return heads, false, nil
	}
	cut := heads[len(heads)-1].wm
	for len(heads) > 0 && heads[len(heads)-1].wm == cut {
		heads = heads[:len(heads)-1]
	}
	if len(heads) == 0 {
		// The group is wider than the window. Dropping it would mean never
		// reading it, so the window yields instead — the alternative is a stall
		// that no configuration can clear.
		return s.watermarkGroup(ctx, sess, after, cut)
	}
	return heads, true, nil
}

// watermarkGroup loads every manifest sharing exactly wm. It exists for the one
// case manifestHeads cannot resolve by shrinking, and it is bounded by the group
// itself rather than by a cap.
func (s *Source) watermarkGroup(ctx context.Context, sess string, after, wm int64) ([]manifestHead, bool, error) {
	const q = `
SELECT id::text,
       (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT AS wm,
       ARRAY(SELECT jsonb_array_elements_text(metadata->'source_block_ids'))
  FROM context_blocks
 WHERE (metadata->>'root_session_id') IS NOT NULL
   AND metadata->>'root_session_id' = $1
   AND type_name = $2 AND scope = $3 AND category = $4
   AND NOT is_archived
   AND metadata ? 'source_block_ids'
   AND created_at > to_timestamp(($5::bigint - 1)::double precision / 1000000)
   AND (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT > $5
   AND (EXTRACT(EPOCH FROM created_at) * 1000000)::BIGINT = $6
 ORDER BY id ASC`

	rows, err := s.pool.Query(ctx, q, sess, typeName, s.opt.Scope, s.opt.Category, after, wm)
	if err != nil {
		return nil, false, mapErr(err)
	}
	defer rows.Close()

	var heads []manifestHead
	for rows.Next() {
		var h manifestHead
		if err := rows.Scan(&h.id, &h.wm, &h.partIDs); err != nil {
			return nil, false, mapErr(err)
		}
		heads = append(heads, h)
	}
	if err := rows.Err(); err != nil {
		return nil, false, mapErr(err)
	}
	return heads, false, nil
}

// readManifest loads a manifest's parts IN ARRAY ORDER and turns them into
// items.
//
// The order is not cosmetic: it is the only source of the role attribution, and
// the carry runs across part boundaries because the producing chunker splits
// inside single messages. A part listed twice is read twice — that is the
// source being faithful about what the manifests say; dropping the repeat is
// the ledger's job, not the reader's.
func (s *Source) readManifest(ctx context.Context, h manifestHead, maxRunes int) ([]distillsource.Item, error) {
	if len(h.partIDs) == 0 {
		return nil, nil
	}
	const q = `
SELECT b.id::text, b.content
  FROM unnest($1::uuid[]) WITH ORDINALITY AS s(pid, ord)
  JOIN context_blocks b ON b.id = s.pid
 WHERE b.type_name = $2 AND b.scope = $3 AND NOT b.is_archived
 ORDER BY s.ord`

	rows, err := s.pool.Query(ctx, q, h.partIDs, typeName, s.opt.Scope)
	if err != nil {
		return nil, mapErr(err)
	}
	defer rows.Close()

	var (
		out []distillsource.Item
		c   carry
	)
	for rows.Next() {
		var id, content string
		if err := rows.Scan(&id, &content); err != nil {
			return nil, mapErr(err)
		}
		body, ok := stripBoilerplate(content)
		if !ok {
			// A part without the transcript marker is not the shape this
			// reader agreed to read. Skipping it keeps the carry intact for
			// the rest of the manifest; guessing an offset would hand the
			// model an unattributable head.
			continue
		}
		var chunks []chunk
		chunks, c = chunkBody(body, maxRunes, c)
		for i, ch := range chunks {
			out = append(out, distillsource.Item{
				Text: ch.Text,
				// Never truncated: this source splits instead of capping, so
				// the whole body reaches the arm across several items.
				Truncated: false,
				// Code-owned attributes only — a block id and a counter.
				// Nothing here is lifted out of foreign content.
				Attrs: []promptguard.Attr{
					{Name: "block", Value: id},
					{Name: "chunk", Value: strconv.Itoa(i + 1)},
				},
				Origin: distillsource.Origin{
					BlockID:    id,
					ChunkIndex: i + 1,
					Ordinal:    ch.Ordinal,
					Role:       ch.Role,
				},
				// The corpus carries no per-block classification this reader
				// could trust, so it reports the highest rank explicitly
				// rather than leaving it to the zero value.
				Sensitivity: backends.SensCredentials,
				Untrusted:   sourceUntrusted,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, mapErr(err)
	}
	return out, nil
}

// sqlStateInvalidTextRepresentation is what Postgres answers when a value in
// metadata.source_block_ids is not a UUID at all. It is a statement about the
// FOREIGN DATA, not about the query — hence its own class below.
const sqlStateInvalidTextRepresentation = "22P02"

// mapErr lifts pgx failures into the abstraction's error classes. The driver's
// own text is kept inside for local diagnosis and never reaches a persisted
// journal field.
func mapErr(err error) error {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return nil
	case errors.Is(err, pgx.ErrNoRows):
		// A missing row is an answer, not a failure: the callers that can see
		// this one read it as "nothing there".
		return nil
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", distillsource.ErrSourceUnavailable, err)
	case errors.As(err, &pgErr) && pgErr.Code == sqlStateInvalidTextRepresentation:
		// A manifest listing something that is not a UUID is the store failing
		// to be the shape this reader agreed to read — not a query that went
		// wrong. Reporting it as schema_untrusted rather than query_failed
		// gives the arm a class it can act on: query_failed says "retry", and
		// retrying this forever is exactly what must not happen.
		//
		// STARVATION LIMIT, stated because it is real and NOT closed here: a
		// single bad id makes Read fail for the WHOLE root session, every tick,
		// and the watermark never advances — so every later, healthy manifest
		// of that root stays unreachable too. This reader deliberately does not
		// paper over it: skipping the id silently would hide corrupt foreign
		// data, and quarantining the root is a journal decision (which run,
		// which cooldown, which counter) that belongs to the arm in A02-5.
		// Live all 5477 listed ids are valid UUIDs (measured), so this is a
		// guard against a future writer, not a current defect.
		return fmt.Errorf("%w: %w", distillsource.ErrSchemaUntrusted, err)
	default:
		return fmt.Errorf("%w: %w", distillsource.ErrQueryFailed, err)
	}
}
