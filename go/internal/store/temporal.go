// temporal.go — Temporal EAV Dimension Functions
// Part of ctx by GottZ — The memory your LLM pretends to have.
//
// Manages the context_temporal table (block_id, dimension, value, source_time)
// for O(log n) temporal queries via partial B-Tree indexes.
//
// Source: https://github.com/GottZ/ctx
package store

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TemporalDimension represents a single dimension extracted from a timestamp.
type TemporalDimension struct {
	Dimension  string    // "year", "month", "week", "weekday", "quarter", "monthday", "seasonal", "daily"
	Value      string    // e.g. "2026", "3", "13", "1", "1", "29", "88", "14"
	SourceTime time.Time // the original timestamp this dimension was extracted from
}

// ExpandDimensions returns the 8 temporal dimensions for a given timestamp.
// Pure function, no DB access.
//
// Dimensions (all cyclic except year):
//   - year:     linear, monotonic (NOT cyclic)
//   - month:    month-of-year (1-12, 12-cycle)
//   - week:     ISO week-of-year (1-53, 52-cycle)
//   - weekday:  day-of-week (ISO 1-7, 7-cycle)
//   - quarter:  quarter-of-year (1-4, 4-cycle)
//   - monthday: day-of-month (1-31, ~30-cycle)
//   - seasonal: day-of-year (1-366, 365-cycle)
//   - daily:    hour-of-day (0-23, 24-cycle)
func ExpandDimensions(t time.Time) []TemporalDimension {
	_, isoWeek := t.ISOWeek()

	// Go Weekday: 0=Sunday .. 6=Saturday
	// ISO weekday: 1=Monday .. 7=Sunday
	goWd := t.Weekday()
	isoWd := int(goWd)
	if isoWd == 0 {
		isoWd = 7 // Sunday: Go=0 → ISO=7
	}

	quarter := (int(t.Month()) - 1) / 3 + 1

	return []TemporalDimension{
		{Dimension: "year", Value: strconv.Itoa(t.Year()), SourceTime: t},
		{Dimension: "month", Value: strconv.Itoa(int(t.Month())), SourceTime: t},
		{Dimension: "week", Value: strconv.Itoa(isoWeek), SourceTime: t},
		{Dimension: "weekday", Value: strconv.Itoa(isoWd), SourceTime: t},
		{Dimension: "quarter", Value: strconv.Itoa(quarter), SourceTime: t},
		{Dimension: "monthday", Value: strconv.Itoa(t.Day()), SourceTime: t},
		{Dimension: "seasonal", Value: strconv.Itoa(t.YearDay()), SourceTime: t},
		{Dimension: "daily", Value: strconv.Itoa(t.Hour()), SourceTime: t},
	}
}

// BuildTemporalBatch creates the pgx.Batch for PopulateTemporal.
// Returns the batch and the expected query count.
// Exported for testing — no DB access.
func BuildTemporalBatch(blockID string, times []time.Time, links ...[]string) (*pgx.Batch, int) {
	batch := &pgx.Batch{}

	// Delete existing dimensions for this block
	batch.Queue(`DELETE FROM context_temporal WHERE block_id = $1`, blockID)

	// Collect link targets (variadic for backward compatibility)
	var linkTargets []string
	if len(links) > 0 {
		linkTargets = links[0]
	}

	// Sentinel for blocks without times AND without links
	sentinel := time.Unix(0, 0).UTC() // 1970-01-01 00:00:00 UTC
	if len(times) == 0 && len(linkTargets) == 0 {
		batch.Queue(
			`INSERT INTO context_temporal (block_id, dimension, value, source_time)
			 VALUES ($1, '_none', '', $2)
			 ON CONFLICT DO NOTHING`,
			blockID, sentinel,
		)
		return batch, 2
	}

	queryCount := 1 // DELETE

	// Insert temporal dimensions for each timestamp
	for _, t := range times {
		dims := ExpandDimensions(t)
		for _, dim := range dims {
			batch.Queue(
				`INSERT INTO context_temporal (block_id, dimension, value, source_time)
				 VALUES ($1, $2, $3, $4)
				 ON CONFLICT DO NOTHING`,
				blockID, dim.Dimension, dim.Value, t,
			)
			queryCount++
		}
	}

	// Insert link dimensions (graph associations)
	for _, target := range linkTargets {
		if target == "" {
			continue
		}
		batch.Queue(
			`INSERT INTO context_temporal (block_id, dimension, value, source_time)
			 VALUES ($1, 'link', $2, $3)
			 ON CONFLICT DO NOTHING`,
			blockID, target, sentinel,
		)
		queryCount++
	}

	// Links are already inserted above; no sentinel needed when only links, no dates.

	return batch, queryCount
}

// PopulateTemporal replaces all temporal dimensions for a block.
// Deletes existing rows, then batch-inserts new dimensions.
//
// A block can have multiple temporal anchors simultaneously:
//   - contentTimes: timestamps mentioned/extracted from block content (semantic)
//   - createdAt: the block's creation timestamp (meta anchor)
//
// Both sources contribute INDEPENDENT dimensions. A block created on Friday
// whose content says "Meeting am Dienstag" gets weekday=2 (content) AND
// weekday=5 (meta) — enables retrieval for both signals. SQL-level dedup
// via ON CONFLICT handles timestamp overlap between sources.
//
// If createdAt is zero, only contentTimes is used (defensive fallback).
//
// content is the content contentTimes were read from. The rows are written
// only while the block still holds it (issue #414): a caller that read the
// block, spent time on it and writes afterwards — the dream's temporal review
// waits on an LLM in between — writes nothing once another write replaced the
// content, and reports applied=false. Guard and rows commit together
// (writeTemporalGuarded, temporalContentGuardSQL).
func PopulateTemporal(ctx context.Context, pool *pgxpool.Pool, blockID, content string, contentTimes []time.Time, createdAt time.Time) (applied bool, err error) {
	return writeTemporalGuarded(ctx, pool, blockID, contentTimes, createdAt, temporalReviewClear(blockID), temporalContentGuardSQL, blockID, content)
}

// PopulateTemporalReview writes the result of the dream's temporal review of
// a block: the rows of times (the dates of the content and those the LLM
// found; issue #427) and, in the same transaction, the version the rows stand
// for — the title and the content the review read — in
// context_temporal_reviews (issue #452). The review sends the block's title
// to the LLM as well as its content, and a date read off one title is no
// finding for another, so both are written only while the block holds the
// title and the content the review read (temporalReviewGuardSQL). An update
// of tags or metadata leaves them valid. The deterministic re-extraction
// before the review reads the content alone and writes with PopulateTemporal.
//
// The rows are replaced whether or not the LLM found a date: rows a review of
// an earlier title left (a date read off it) belong to no version the block
// holds (issue #452 point 2). applied=false (the block changed, or is gone)
// writes neither, and the block stays due (TemporalReviewDueCol).
//
// With the version it records the extraction the rows hold beside the LLM's
// dates: the creation time and temporalExtraction of content, as RefreshTemporal
// would write them (temporalExtractionDigest, issue #466 point 8). A
// same-content write is covered by the review only while the extraction in
// force yields the same.
func PopulateTemporalReview(ctx context.Context, pool *pgxpool.Pool, blockID, title, content string, times []time.Time, createdAt time.Time) (applied bool, err error) {
	extraction := temporalExtractionDigest(temporalAnchors(temporalExtraction(content), createdAt))
	return writeTemporalGuarded(ctx, pool, blockID, times, createdAt,
		temporalMark{sql: temporalReviewMarkSQL, args: []any{blockID, content, title, extraction}},
		temporalReviewGuardSQL, blockID, content, title)
}

// TemporalReviewDueCol answers, in a statement on context_blocks, whether the
// dream's temporal review is due for the row: no row of
// context_temporal_reviews carries the version of the row's title and
// content (issue #452). The answer depends on what the block holds and what
// the review wrote, not on when either happened: a write that began before a
// review and committed after it leaves the block due, whatever updated_at it
// carries, and a write of tags or metadata leaves a reviewed block reviewed.
// It reads one row of context_temporal_reviews by its primary key; the dream's
// pick reads it in its RETURNING, for the one block it hands out.
const TemporalReviewDueCol = `NOT EXISTS (SELECT 1 FROM context_temporal_reviews r
		 WHERE r.block_id = context_blocks.id
		   AND r.version = ` + temporalReviewVersionOfRow + `)`

// temporalReviewVersionOfRow is the version of the block row's title and
// content: sha256 over content_hash and title. content_hash is the fixed-length
// hex digest of the content (64 characters), so the concatenation is
// unambiguous; the digest reads no TOASTed content.
const temporalReviewVersionOfRow = `digest(context_blocks.content_hash || context_blocks.title, 'sha256')`

// temporalReviewMarkSQL writes the version of the review of block $1 with
// content $2 and title $3 — the expression of temporalReviewVersionOfRow over
// the review's input, whose content_hash part is the generated column's
// expression (contentVersionCol) — and its extraction: the digest $4 of the
// extraction's times (temporalExtractionDigest), bound to that version
// (temporalReviewMarkExtraction, migration 190).
const temporalReviewMarkSQL = `INSERT INTO context_temporal_reviews (block_id, version, extraction)
	SELECT $1, v.version, ` + temporalReviewMarkExtraction + `
	  FROM (SELECT digest(encode(digest($2::text, 'sha256'), 'hex') || $3::text, 'sha256') AS version) v
	ON CONFLICT (block_id) DO UPDATE SET version = EXCLUDED.version, extraction = EXCLUDED.extraction`

// temporalReviewMarkExtraction and temporalReviewRowExtraction are the
// extraction column for a version — the mark's new one, a row's own — and the
// bytea parameter $4 (temporalExtractionDigest): sha256 over both, so the
// extraction a row carries holds for the version it was written with only.
// The review of a binary before migration 190 rewrites an existing row's
// version and leaves its extraction; where the version changed, that
// extraction is bound to the one before and covers no write of the new one
// (where it did not, the extraction stays in force).
const (
	temporalReviewMarkExtraction = `digest(v.version || $4::bytea, 'sha256')`
	temporalReviewRowExtraction  = `digest(r.version || $4::bytea, 'sha256')`
)

// temporalExtractionDigest is the digest of the times the deterministic
// extraction yields for a block (temporalAnchors: the creation time and the
// content's dates): sha256 over their distinct values in microseconds since
// the epoch (the resolution of timestamptz), ascending, eight bytes each.
// The same set of times, in any order or repetition, has the same digest.
func temporalExtractionDigest(anchors []time.Time) []byte {
	us := make([]int64, 0, len(anchors))
	for _, t := range anchors {
		us = append(us, t.UnixMicro())
	}
	slices.Sort(us)
	us = slices.Compact(us)
	buf := make([]byte, 0, 8*len(us))
	for _, u := range us {
		buf = binary.BigEndian.AppendUint64(buf, uint64(u))
	}
	sum := sha256.Sum256(buf)
	return sum[:]
}

// temporalReviewClearSQL removes the review version of block $1: every
// temporal writer but the review's own runs it with its rows, because the
// rows it writes are not the review's (issue #452).
const temporalReviewClearSQL = `DELETE FROM context_temporal_reviews WHERE block_id = $1`

// temporalMark is the statement a temporal writer runs on the block's row of
// context_temporal_reviews, in the transaction of its rows and before its
// guard (writeTemporalGuarded). skipSQL, when set, is a check that runs
// right after the writer's advisory lock: when it answers true, the writer
// writes nothing and reports applied (RefreshTemporal's
// temporalReviewCoversSQL).
type temporalMark struct {
	sql      string
	args     []any
	skipSQL  string
	skipArgs []any
}

// errTemporalCovered ends a temporal writer's transaction whose skip check
// answered true: nothing is written, and the writer reports applied.
var errTemporalCovered = errors.New("store: temporal: rows already hold what the write would write")

// temporalReviewClear is the temporalMark of a writer that is not the review.
func temporalReviewClear(blockID string) temporalMark {
	return temporalMark{sql: temporalReviewClearSQL, args: []any{blockID}}
}

// temporalReviewCoversSQL answers whether the rows of block $1 already hold
// what RefreshTemporal would write for content $2: the block holds $2, its
// rows are the review's of the title and content it holds (the version in
// context_temporal_reviews, which every other temporal writer deletes), the
// review recorded the extraction $4 (temporalExtractionDigest of $3) for that
// version (issue #466 point 8), and the rows hold each time of $3 (the
// creation time and the content's dates). The review's rows are then the
// extraction's plus the LLM's dates, of this very version and this very
// extraction. An extraction that changed since the review answers false — a
// date it no longer yields as well as one it yields now — and so does a row
// without an extraction (NULL: taken over by migration 189, or written by a
// binary before migration 190).
const temporalReviewCoversSQL = `SELECT EXISTS (
	SELECT 1 FROM context_blocks
	  JOIN context_temporal_reviews r ON r.block_id = context_blocks.id
	 WHERE context_blocks.id = $1
	   AND ` + contentVersionCol + `
	   AND r.version = ` + temporalReviewVersionOfRow + `
	   AND r.extraction = ` + temporalReviewRowExtraction + `
	   AND NOT EXISTS (
		SELECT 1 FROM unnest($3::timestamptz[]) AS t(at)
		 WHERE NOT EXISTS (SELECT 1 FROM context_temporal ct
		                    WHERE ct.block_id = context_blocks.id AND ct.source_time = t.at)))`

// RefreshTemporal is the temporal afterwork of a write that stored content on
// a block: it writes the dimension rows PopulateTemporal writes, from the
// dates of content, only while the block still holds content. The block's
// content_times is not its part: the write set it in its own statement, from
// the content it stored (issue #421; UpsertBlock, UpdateBlock, InsertChunk).
//
// It writes nothing when the rows already hold what it would write: the
// block holds content, the dream's temporal review wrote the rows for the
// title and content the block holds and recorded the extraction this call
// writes from, and they hold the creation time and every date of content
// (temporalReviewCoversSQL); applied is then true. An
// upsert that stores the content the block holds runs this afterwork, and
// would otherwise replace the review's findings with the extraction's rows
// while the review still covered the block (issue #452 point 3). The check
// runs in the writer's turn, right after the advisory lock
// (writeTemporalGuarded's skip): a review of this content that held the turn
// has committed by then and is seen, where a check before the turn would
// have missed it and the write would have replaced its rows.
//
// The afterwork runs after the write committed, outside its lock, from the
// block the write returned (issue #414). Two writes A, B of one block that
// commit in that order can finish their afterwork in the order B, A; A's then
// finds B's content on the block and writes nothing (applied=false), so the
// block keeps the temporal data of the content it holds. The version is the
// content itself (content_hash), not updated_at: an update of tags or
// metadata between a content write and its afterwork changes updated_at but
// not the content the dates belong to, and two writes can carry the same
// updated_at (now() is the transaction start).
func RefreshTemporal(ctx context.Context, pool *pgxpool.Pool, blockID, content string, createdAt time.Time) (applied bool, err error) {
	times := temporalExtraction(content)
	anchors := temporalAnchors(times, createdAt)
	mark := temporalReviewClear(blockID)
	mark.skipSQL, mark.skipArgs = temporalReviewCoversSQL, []any{blockID, content, anchors, temporalExtractionDigest(anchors)}
	return writeTemporalGuarded(ctx, pool, blockID, times, createdAt, mark, temporalContentGuardSQL, blockID, content)
}

// temporalExtraction is the extraction the temporal writers write their rows
// from and the review records (ExtractDates); a variable only so a test can
// stand in a different extraction for the same content, as a later release's
// would be (issue #466 point 8).
var temporalExtraction = ExtractDates

// backfillTemporalBlock is RefreshTemporal for the backfill, which writes the
// block's content_times as well: a block without rows was written by a path
// that sets no content_times (the issue tracker's and the forge's inserts),
// or by a write whose afterwork failed. The column is written by the guard
// itself, an UPDATE of the block row that answers contentVersionCol: it waits
// for a content write in flight and answers for the row version it locked,
// so the column is never written over a newer content — a plain read would
// not do here, the column is the block row's own. It holds the block row from
// the guard to the commit, as RefreshTemporal did before issue #421; the
// backfill runs once, at startup, over blocks without rows.
func backfillTemporalBlock(ctx context.Context, pool *pgxpool.Pool, blockID, content string, createdAt time.Time) (applied bool, err error) {
	times := temporalExtraction(content)
	return writeTemporalGuarded(ctx, pool, blockID, times, createdAt, temporalReviewClear(blockID),
		`UPDATE context_blocks SET content_times = $3::timestamptz[] WHERE id = $1 RETURNING `+contentVersionCol,
		blockID, content, times)
}

// temporalContentGuardSQL is the guard of RefreshTemporal and
// PopulateTemporal: contentVersionCol for the block ($1), read without a row
// lock (writeTemporalGuarded says why none is needed).
const temporalContentGuardSQL = `SELECT ` + contentVersionCol + ` FROM context_blocks WHERE id = $1`

// contentVersionCol answers whether the block row holds the content $2: the
// stored content_hash (a generated column; content is NOT NULL, so it is never
// NULL) against the same expression of $2, the comparison identicalContentWhere
// makes. The guards return it as a column instead of filtering on it, so their
// WHERE names the primary key alone: content_hash is not unique and has an
// index of its own, and a planner that chose that index for a hash many blocks
// share (identical contents are common) would read every one of them. The
// comparison reads the row version the guard sees; the hash is in that row,
// so no TOASTed content is read.
const contentVersionCol = `content_hash = encode(digest($2::text, 'sha256'), 'hex')`

// temporalReviewVersionCol answers whether the block row holds the input of
// the dream's temporal review: the content $2 (contentVersionCol) and the
// title $3, the two fields the review sends to the LLM. title is NOT NULL, so
// the answer is never NULL. Like contentVersionCol it is a column, not a
// filter: a `title = $n` in a WHERE beside the primary key is a condition the
// trigram indexes on title support (store/classify.go, classifyInputsSQL).
const temporalReviewVersionCol = contentVersionCol + ` AND title = $3`

// temporalReviewGuardSQL is the guard of PopulateTemporalReview:
// temporalReviewVersionCol for the block ($1), read without a row lock like
// temporalContentGuardSQL.
const temporalReviewGuardSQL = `SELECT ` + temporalReviewVersionCol + ` FROM context_blocks WHERE id = $1`

// writeTemporalGuarded writes the dimension rows of a block, runs mark on the
// block's row of context_temporal_reviews (the review writes its version,
// every other writer deletes it; issue #452), and then runs guardSQL, a
// statement that reads the block row by its primary key and answers
// contentVersionCol (temporalReviewVersionCol for the dream's review) for
// it; it commits when the answer is true and rolls back when it is false or
// the block is gone — the version row with the rows. After the BEGIN, the
// lock, the block row's lock, the rows, the version row and the guard travel
// in one round trip; the commit or rollback is the last.
//
// The block row before the rows (issue #465): right after its turn, the
// writer takes the block row FOR KEY SHARE (temporalBlockLockSQL), the lock
// the foreign key of its first row would take, and only then deletes the
// block's rows. A hard delete of the block (a purge; its DELETE cascades
// into context_temporal and context_temporal_reviews) takes the block row
// first and the rows in its cascade: both now take the block row before the
// rows, and neither waits for the other while holding what the other waits
// for. Before, the writer's DELETE held the block's rows while its first
// insert waited for a block row the delete held, and the delete's cascade
// waited for those rows: a deadlock (40P01), on either side. A writer that
// waits for the block row holds only its turn, which no deleter takes. The
// dimension rows' and the version row's foreign keys find the row locked
// already.
//
// A block deleted before the writer locks it has no row to lock, and its
// first row is refused on temporalBlockFK before the guard runs; that refusal
// is the same answer as a guard that finds no row: applied=false, no error.
// So is a deadlock the writer loses while the block goes — a cycle with a
// transaction that took a row of the block before deleting it, which the
// order above does not rule out: either refusal counts as the block going
// only once the block is gone (temporalBlockGoing), read again after the
// rollback and waiting for a delete in flight; a block still there leaves
// the refusal the writer's error.
//
// The guard of RefreshTemporal and PopulateTemporal (temporalContentGuardSQL)
// takes no row lock (issue #421), nor does PopulateTemporalReview's
// (temporalReviewGuardSQL). The transaction holds the block row only in
// FOR KEY SHARE, from right after its turn to the commit — before issue #465
// from its first row, one statement later. FOR NO KEY UPDATE does not conflict with it — UpdateBlock
// and the query path's failure memo (FOR NO KEY UPDATE SKIP LOCKED,
// RecordEmbedFailureIfUnchanged) neither wait for this transaction nor skip
// the block because of it. FOR UPDATE does, as it did before: the pre-read of
// UpsertBlock and the lock pass of UpsertBlocks wait for the commit, and the
// FOR UPDATE SKIP LOCKED picks of the embed arm, the dream and the embed
// migration skip the block meanwhile; the guard batch picks FOR NO KEY UPDATE
// SKIP LOCKED (issue #359) and does not. Before, the guard also took the row
// lock FOR NO KEY UPDATE (the UPDATE of content_times) and held it to the
// commit.
//
// What the lock did is done by the turn the temporal writers of a block take
// (temporalWriterLockSQL, below). The guard stays the last statement: a
// content write that committed before it is seen (READ COMMITTED takes the
// guard's snapshot after the rows), and the rows roll back. A content write
// that commits after it, before this COMMIT, leaves this transaction to commit
// the rows of the content it saw — and that write's own afterwork is a
// temporal writer of the block too: it takes its turn after this one, and its
// DELETE sees and replaces these rows. A content write that is followed by no
// afterwork leaves rows of the content before it standing, lock or not, as it
// did before.
//
// The rows are written before the guard knows whether they stay: a stale
// caller writes and rolls back a batch it never needed — the rare case. The
// backfill's guard (backfillTemporalBlock) is an UPDATE of the block row, and
// last for the reason measured in issue #414: a lock taken first would be
// held across the dimension writes, and a concurrent update of the block
// would wait for them.
//
// The temporal writers of one block take turns (issue #428): the first
// statement takes temporalWriterLockSQL, a transaction-scoped advisory lock
// of the block, so the DELETE runs only once every temporal write of the
// block before it has committed and sees its rows. Without it, two writers of
// one content with different dates (the write path's extraction and the
// dream's review, or two reviews) both passed the guard, and under READ
// COMMITTED the second DELETE skipped the rows the first had deleted and did
// not see the ones it had inserted: both date sets stayed.
func writeTemporalGuarded(ctx context.Context, pool *pgxpool.Pool, blockID string, contentTimes []time.Time, createdAt time.Time, mark temporalMark, guardSQL string, guardArgs ...any) (bool, error) {
	rows, _ := BuildTemporalBatch(blockID, temporalAnchors(contentTimes, createdAt))
	batch := &pgx.Batch{}
	if mark.skipSQL == "" {
		batch.Queue(temporalWriterLockSQL, blockID)
	}
	batch.Queue(temporalBlockLockSQL, blockID)
	batch.QueuedQueries = append(batch.QueuedQueries, rows.QueuedQueries...)
	batch.Queue(mark.sql, mark.args...)
	queryCount := len(batch.QueuedQueries)
	batch.Queue(guardSQL, guardArgs...)

	err := pgxdb.Write(ctx, pool, pgxdb.At("store: temporal"), func(tx pgx.Tx) error {
		if mark.skipSQL != "" {
			// The lock and the check first, in a round trip of their own:
			// the check reads what every temporal writer before this one
			// committed.
			var covered bool
			sb := &pgx.Batch{}
			sb.Queue(temporalWriterLockSQL, blockID)
			sb.Queue(mark.skipSQL, mark.skipArgs...)
			sr := tx.SendBatch(ctx, sb)
			_, err := sr.Exec()
			if err == nil {
				err = sr.QueryRow().Scan(&covered)
			}
			if cerr := sr.Close(); err == nil {
				err = cerr
			}
			if err != nil {
				return fmt.Errorf("store: temporal: lock and skip check: %w", err)
			}
			if covered {
				return errTemporalCovered
			}
		}
		br := tx.SendBatch(ctx, batch)
		for i := 0; i < queryCount; i++ {
			if _, err := br.Exec(); err != nil {
				_ = br.Close()
				return fmt.Errorf("store: temporal: batch query %d: %w", i, err)
			}
		}
		var holds bool
		err := br.QueryRow().Scan(&holds)
		if errors.Is(err, pgx.ErrNoRows) {
			err = nil // the block is gone: holds stays false
		}
		if cerr := br.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fmt.Errorf("store: temporal: content version guard: %w", err)
		}
		if !holds {
			// The block no longer holds the content (or is gone): the
			// bracket's rollback drops the rows.
			return pgxdb.ErrRollback
		}
		return nil
	})
	if errors.Is(err, errTemporalCovered) {
		return true, nil
	}
	if errors.Is(err, pgxdb.ErrRollback) {
		return false, nil
	}
	if temporalBlockGoing(err) {
		gone, gerr := temporalBlockIsGone(ctx, pool, blockID)
		switch {
		case gerr != nil:
			return false, errors.Join(err, gerr)
		case gone:
			return false, nil
		}
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// temporalBlockLockSQL locks the block row ($1) FOR KEY SHARE, the lock the
// foreign keys of the temporal rows take: the second statement of a temporal
// writer, before it touches a row of the block (writeTemporalGuarded, issue
// #465), and the read of temporalBlockIsGone.
const temporalBlockLockSQL = `SELECT 1 FROM context_blocks WHERE id = $1 FOR KEY SHARE`

// temporalBlockGoing reports a refusal a temporal writer meets when its block
// is deleted beside it: the foreign-key refusal of its first row
// (temporalBlockGone), or a deadlock (40P01) it lost. Whether the block went
// is the read of temporalBlockIsGone.
func temporalBlockGoing(err error) bool {
	var pgErr *pgconn.PgError
	return temporalBlockGone(err) || (errors.As(err, &pgErr) && pgErr.Code == "40P01")
}

// temporalBlockIsGone answers, after a writer's rollback, whether the block is
// gone. The read locks the row FOR KEY SHARE (temporalBlockLockSQL), so it
// waits for a delete in flight and answers for its outcome: a delete that
// rolls back leaves the block, and the writer's refusal stays an error. A
// block once gone does not come back (its id is not reused), so gone now is
// gone at the refusal.
func temporalBlockIsGone(ctx context.Context, pool *pgxpool.Pool, blockID string) (bool, error) {
	var one int
	err := pool.QueryRow(ctx, temporalBlockLockSQL, blockID).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: temporal: read the block after a refusal: %w", err)
	}
	return false, nil
}

// temporalAnchors are the times a block's rows are written from: the
// creation time (when known) and the content's dates.
func temporalAnchors(contentTimes []time.Time, createdAt time.Time) []time.Time {
	if createdAt.IsZero() {
		return contentTimes
	}
	all := make([]time.Time, 0, len(contentTimes)+1)
	all = append(all, createdAt)
	return append(all, contentTimes...)
}

// temporalWriterLockSQL is the lock the temporal writers of a block ($1) take
// in turn (writeTemporalGuarded): transaction-scoped, so it ends with the
// commit or the rollback. The key is the pair (hashtext('ctx:temporal'),
// hashtext(block id)), the two-key form the per-block lock of the guard's
// archive uses (guard.go) under a namespace of its own; the two-key form is a
// key space apart from the one-key locks. hashtext is 32 bits wide, so two
// blocks can share a key: their temporal writes then wait for each other as
// well. That costs a wait, never a wrong row — every writer still writes and
// checks only its own block. Only writeTemporalGuarded takes this lock, as the
// first statement of a transaction that holds nothing yet, so a writer
// waiting for it holds nothing another writer could wait for.
const temporalWriterLockSQL = `SELECT pg_advisory_xact_lock(hashtext('ctx:temporal'), hashtext($1::uuid::text))`

// temporalBlockFK is the foreign key of context_temporal.block_id, the name
// PostgreSQL gave the column's REFERENCES context_blocks(id) ON DELETE CASCADE
// (migration 113).
const temporalBlockFK = "context_temporal_block_id_fkey"

// temporalBlockGone reports the refusal a dimension row gets when its block
// does not exist: 23503 (foreign_key_violation) on temporalBlockFK. Any other
// constraint's refusal, and any other code, is an error of its own and stays
// one.
func temporalBlockGone(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23503" && pgErr.ConstraintName == temporalBlockFK
}

// FetchContentTimes retrieves content_times for multiple blocks.
// Returns a map from block ID to parsed timestamps.
func FetchContentTimes(ctx context.Context, pool *pgxpool.Pool, blockIDs []string) (map[string][]time.Time, error) {
	if len(blockIDs) == 0 {
		return make(map[string][]time.Time), nil
	}

	rows, err := pool.Query(ctx,
		`SELECT id, content_times FROM context_blocks WHERE id = ANY($1)`,
		blockIDs,
	)
	if err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_times: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]time.Time, len(blockIDs))
	for rows.Next() {
		var id string
		var times []time.Time
		if err := rows.Scan(&id, &times); err != nil {
			return nil, fmt.Errorf("store: temporal: fetch content_times scan: %w", err)
		}
		if times != nil {
			result[id] = times
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: temporal: fetch content_times rows: %w", err)
	}
	return result, nil
}

// BlockDimension is the (dimension, value) pair returned by FetchBlockDimensions.
// Keeps the type minimal (no source_time) to match rrf.TemporalDim semantics.
type BlockDimension struct {
	Dimension string
	Value     string
}

// FetchBlockDimensions retrieves EAV temporal dimensions for multiple blocks.
// Filters by the requested dimension names (e.g. ["weekday", "month"]).
// Sentinel "_none" rows are excluded automatically.
//
// Returns map[blockID][]BlockDimension for use in cyclic gravity computation.
// Blocks with no matching dimensions are simply absent from the map.
func FetchBlockDimensions(ctx context.Context, pool *pgxpool.Pool, blockIDs []string, dimensions []string) (map[string][]BlockDimension, error) {
	if len(blockIDs) == 0 || len(dimensions) == 0 {
		return make(map[string][]BlockDimension), nil
	}

	rows, err := pool.Query(ctx,
		`SELECT block_id, dimension, value
		 FROM context_temporal
		 WHERE block_id = ANY($1)
		   AND dimension = ANY($2)
		   AND dimension != '_none'`,
		blockIDs, dimensions,
	)
	if err != nil {
		return nil, fmt.Errorf("store: temporal: fetch block dimensions: %w", err)
	}
	defer rows.Close()

	result := make(map[string][]BlockDimension, len(blockIDs))
	for rows.Next() {
		var blockID, dim, val string
		if err := rows.Scan(&blockID, &dim, &val); err != nil {
			return nil, fmt.Errorf("store: temporal: fetch block dimensions scan: %w", err)
		}
		result[blockID] = append(result[blockID], BlockDimension{Dimension: dim, Value: val})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: temporal: fetch block dimensions rows: %w", err)
	}
	return result, nil
}

// backfillAttempts bounds the writes BackfillTemporal makes for one block. A
// block whose content changed between the backfill's read and its write
// (issue #428) is read again and written with the content it holds then, up
// to this many writes in all; a block whose content changed on every one of
// them is left without rows and reported. It is a fixed bound, not a policy:
// each further try costs a write and a read, and a content that changes
// faster than the backfill writes is one whose own writes run the afterwork.
const backfillAttempts = 3

// backfillLeftShown bounds the block ids the backfill's report names.
const backfillLeftShown = 10

// backfillRereadSQL reads a block the backfill could not write ($1) again:
// its content and creation time while it is live and still has no rows. A
// block that has rows by now was written by another temporal writer (the
// afterwork of the write that changed it); one that is gone or archived needs
// none.
const backfillRereadSQL = `SELECT cb.content, cb.created_at
	 FROM context_blocks cb
	 WHERE cb.id = $1 AND NOT cb.is_archived
	   AND NOT EXISTS (SELECT 1 FROM context_temporal ct WHERE ct.block_id = cb.id)`

// BackfillTemporal populates context_temporal for all blocks that don't have
// temporal dimensions yet. Processes in batches of 50.
// Returns the number of blocks processed, and an error naming the blocks it
// left without rows: those whose content changed on every try
// (backfillAttempts) and those whose write failed (each failure is logged as
// well); the other blocks are written all the same.
func BackfillTemporal(ctx context.Context, pool *pgxpool.Pool) (int, error) {
	const batchSize = 50
	processed := 0
	// left are the blocks given up on because their content kept changing,
	// failed those whose write returned an error. The reads of later batches
	// skip both (skip): a full batch of them would otherwise be read again
	// and again, and every block behind it never — the backfill runs before
	// ctxd's scheduler starts.
	left, failed, skip := []string{}, []string{}, []string{}

	for {
		rows, err := pool.Query(ctx,
			`SELECT cb.id, cb.content, cb.created_at
			 FROM context_blocks cb
			 WHERE NOT cb.is_archived
			   AND NOT EXISTS (SELECT 1 FROM context_temporal ct WHERE ct.block_id = cb.id)
			   AND cb.id <> ALL($2::uuid[])
			 LIMIT $1`,
			batchSize, skip,
		)
		if err != nil {
			return processed, fmt.Errorf("store: temporal: backfill query: %w", err)
		}

		var blocks []backfillBlock
		for rows.Next() {
			var b backfillBlock
			if err := rows.Scan(&b.ID, &b.Content, &b.CreatedAt); err != nil {
				rows.Close()
				return processed, fmt.Errorf("store: temporal: backfill scan: %w", err)
			}
			blocks = append(blocks, b)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return processed, fmt.Errorf("store: temporal: backfill rows: %w", err)
		}

		if len(blocks) == 0 {
			break // No more blocks to process
		}

		for _, b := range blocks {
			// Always populate temporal — createdAt is passed as meta-anchor.
			// Blocks without content times still get dimensions from their
			// creation time.
			written, gaveUp, err := b.write(ctx, pool)
			switch {
			case err != nil:
				slog.Warn("backfill: temporal refresh failed",
					"block_id", b.ID, "error", err)
				failed = append(failed, b.ID)
				skip = append(skip, b.ID)
			case gaveUp:
				left = append(left, b.ID)
				skip = append(skip, b.ID)
			case written:
				processed++
			}
		}

		slog.Info("backfill: batch complete",
			"batch_size", len(blocks), "total_processed", processed)

		if len(blocks) < batchSize {
			break // Last batch was partial, we're done
		}
	}

	var report []string
	if len(left) > 0 {
		report = append(report, fmt.Sprintf("%d block(s) left without rows, their content changed on each of %d tries: %s",
			len(left), backfillAttempts, strings.Join(left[:min(len(left), backfillLeftShown)], ", ")))
	}
	if len(failed) > 0 {
		report = append(report, fmt.Sprintf("%d block(s) left after an error (each logged): %s",
			len(failed), strings.Join(failed[:min(len(failed), backfillLeftShown)], ", ")))
	}
	if len(report) > 0 {
		return processed, fmt.Errorf("store: temporal: backfill: %s", strings.Join(report, "; "))
	}
	return processed, nil
}

// backfillBlock is one block the backfill read: its id, and the content and
// creation time it read.
type backfillBlock struct {
	ID        string
	Content   string
	CreatedAt time.Time
}

// write writes the block's temporal data with the content read
// (backfillTemporalBlock). When that content is no longer the block's, the
// block is read again (backfillRereadSQL) and written with its current
// content, up to backfillAttempts writes; gaveUp reports a block still live
// and without rows after the last of them.
func (b backfillBlock) write(ctx context.Context, pool *pgxpool.Pool) (written, gaveUp bool, err error) {
	for attempt := 1; ; attempt++ {
		applied, err := backfillTemporalBlock(ctx, pool, b.ID, b.Content, b.CreatedAt)
		if err != nil || applied {
			return applied, false, err
		}
		err = pool.QueryRow(ctx, backfillRereadSQL, b.ID).Scan(&b.Content, &b.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return false, false, nil // written by another writer, gone or archived
		}
		if err != nil {
			return false, false, fmt.Errorf("store: temporal: backfill re-read: %w", err)
		}
		if attempt == backfillAttempts {
			return false, true, nil
		}
	}
}
