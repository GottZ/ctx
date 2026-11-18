// block_reads.go — the read record of context_access_log: one row per block a
// caller was handed by a read surface (issue #345).
package store

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// The block-read actions of context_access_log. The action names the surface
// AND the tool, so "who read what through which door" is one indexed column
// (idx_access_log_action, idx_access_log_ratelimit) and needs no metadata
// lookup.
//
// They are deliberately NOT "query": the read rate limit of the REST surfaces
// counts action 'query' per key and minute (handler/ratelimit.go,
// query.rate_limit_read), and a read through these tools must not start to
// spend a budget that does not apply to it. No reader of the table counts
// rows without an action filter (the M007/M008 gravity functions that did
// were dropped by the baseline fold, 113_baseline.sql), so a new action
// changes the meaning of no existing aggregate.
const (
	// ActionManageGet is REST POST /api/manage {action:get}; the name it has
	// carried since the table existed.
	ActionManageGet = "manage-get"
	// ActionMCPGet is the MCP `get` tool: the one block it answered.
	ActionMCPGet = "mcp-get"
	// ActionMCPSearch is the MCP `search` tool: each row it answered.
	ActionMCPSearch = "mcp-search"
	// ActionMCPRecent is the MCP `recent` tool: each row it answered.
	ActionMCPRecent = "mcp-recent"
)

// Write-path bounds of the read record. Hardcoded on purpose, not settings:
// they bound the process's own bookkeeping, not anybody's policy — the
// precedent of the llmlog writer (llmlog.go maxConcurrentInserts).
const (
	// blockReadMaxConcurrent caps how many read-record inserts run at once.
	// The record shares ctxd's 20-connection pool (pool.go tunePool) with
	// every request path: a slow or locked context_access_log must cost the
	// read path at most these two connections, never the pool. Two slots are
	// enough because a drain coalesces everything queued into one statement
	// (blockReadBatchRows), so the slots bind only while the table is slow.
	blockReadMaxConcurrent = 2
	// blockReadBatchRows bounds the rows one insert statement carries. A drain
	// takes queued reads up to this many rows at once — one round trip for a
	// burst instead of one per request.
	blockReadBatchRows = 1000
	// blockReadMaxQueuedRows bounds the rows waiting for an insert slot.
	// Beyond it a read's rows are dropped and counted instead of growing
	// without bound while the database is away: 65 536 ids are about 5 MB.
	blockReadMaxQueuedRows = 65536
	// blockReadInsertTimeout bounds one insert statement. The caller has long
	// returned; this is background bookkeeping. The clock starts when the
	// statement leaves the queue.
	blockReadInsertTimeout = 5 * time.Second
	// blockReadLossWarnInterval throttles the loss WARN to one line per
	// interval, process-wide; every line carries the totals since start.
	blockReadLossWarnInterval = time.Minute
)

// blockRead is one read surface answer: the key, the action, the block ids.
type blockRead struct {
	apiKeyID string
	action   string
	ids      []string
}

// blockReadWriter is the per-pool queue of the read record: RecordBlockReads
// appends, at most blockReadMaxConcurrent drain goroutines take everything
// queued (up to blockReadBatchRows) and insert it in one statement, and a
// drain goroutine exits when the queue is empty — an idle pool holds none.
type blockReadWriter struct {
	pool    *pgxpool.Pool
	pending atomic.Int64 // reads accepted and not finished (queued + running)

	mu      sync.Mutex
	queue   []blockRead
	rows    int // ids in queue
	running int
}

// blockReadWriters holds one writer per pool, keyed by pool for the reason the
// llmlog writers are: every integration test has its own pool, and a
// straggler of one test must not hold up the next one's settle.
var blockReadWriters sync.Map // *pgxpool.Pool → *blockReadWriter

func blockReadWriterFor(pool *pgxpool.Pool) *blockReadWriter {
	if v, ok := blockReadWriters.Load(pool); ok {
		return v.(*blockReadWriter)
	}
	v, _ := blockReadWriters.LoadOrStore(pool, &blockReadWriter{pool: pool})
	return v.(*blockReadWriter)
}

// Process-wide loss accounting of the read record, in rows. Monotone since
// start.
var (
	blockReadFailed  atomic.Int64 // rows of inserts that failed or timed out
	blockReadDropped atomic.Int64 // rows dropped because the queue was full
	blockReadSkipped atomic.Int64 // rows the insert skipped (see insertBlockReads)
	blockReadWarned  atomic.Int64 // unix nanos of the last loss WARN; 0 = none
)

// BlockReadsLost returns how many read-record rows were lost since the process
// started: failed (the insert failed or timed out), dropped (the queue was
// full) and skipped (the block or the key was gone, or another transaction
// held its row FOR UPDATE when the insert ran — see insertBlockReads).
func BlockReadsLost() (failed, dropped, skipped int64) {
	return blockReadFailed.Load(), blockReadDropped.Load(), blockReadSkipped.Load()
}

// RecordBlockReads books one context_access_log row per block id a read
// surface handed to the caller: api_key_id and principal_id of the reader, the
// block id, the action (surface and tool), and in metadata the block's type
// and its created_at at the moment of the read —
//
//	{"type": "<type_name>", "block_created_at": "<timestamptz>"}
//
// — so the age of a block when it was read stays computable per type after the
// block is gone: block_id is ON DELETE SET NULL, a purge or delete clears it,
// and the table keeps its rows by decision (tenant.go, prune-audit-tables).
// No content, no title, no query text.
//
// It never blocks the read and never fails it. The rows join the pool's
// bounded queue and are written asynchronously (see the bounds above); a
// full queue drops them, a failed insert loses them, and both are counted and
// logged with a throttled WARN; a row the insert has to skip is counted (see
// insertBlockReads). Callers pass only blocks they ANSWERED — a
// row is a delivered read, never an attempted one (the G9 oracle rule of
// manage-get: a refused or foreign id must leave no trace).
//
// An empty apiKeyID (sentinel or test wiring without a key) or an empty id
// list books nothing.
func RecordBlockReads(pool *pgxpool.Pool, apiKeyID, action string, blockIDs []string) {
	if pool == nil || apiKeyID == "" || len(blockIDs) == 0 {
		return
	}
	blockReadWriterFor(pool).enqueue(blockRead{apiKeyID: apiKeyID, action: action, ids: blockIDs})
}

func (w *blockReadWriter) enqueue(r blockRead) {
	w.mu.Lock()
	if w.rows+len(r.ids) > blockReadMaxQueuedRows {
		w.mu.Unlock()
		blockReadDropped.Add(int64(len(r.ids)))
		warnBlockReadLoss("access log: read record queue full, rows dropped", r.action, nil)
		return
	}
	w.pending.Add(1)
	w.queue = append(w.queue, r)
	w.rows += len(r.ids)
	spawn := w.running < blockReadMaxConcurrent
	if spawn {
		w.running++
	}
	w.mu.Unlock()
	if spawn {
		go w.drain()
	}
}

// drain takes queued reads up to blockReadBatchRows rows (at least one read,
// whatever its size) and inserts them in one statement, until the queue is
// empty. The running count drops under the queue's lock, so a read enqueued
// after the last take always finds a live drain or a free slot.
func (w *blockReadWriter) drain() {
	for {
		w.mu.Lock()
		if len(w.queue) == 0 {
			w.running--
			w.queue = nil // release the backing array of a drained burst
			w.mu.Unlock()
			return
		}
		n, rows := 0, 0
		for n < len(w.queue) && (n == 0 || rows+len(w.queue[n].ids) <= blockReadBatchRows) {
			rows += len(w.queue[n].ids)
			n++
		}
		batch := make([]blockRead, n)
		copy(batch, w.queue[:n])
		clear(w.queue[:n])
		w.queue = w.queue[n:]
		w.rows -= rows
		w.mu.Unlock()

		ctx, cancel := context.WithTimeout(context.Background(), blockReadInsertTimeout)
		inserted, err := insertBlockReads(ctx, w.pool, batch)
		cancel()
		switch {
		case err != nil:
			blockReadFailed.Add(int64(rows))
			slog.Debug("access log: read record insert failed", "rows", rows, "error", err)
			warnBlockReadLoss("access log: read record insert failed, rows lost", batch[0].action, err)
		case inserted < int64(rows):
			// Skips are part of normal operation (the embedding backfill
			// holds its blocks FOR UPDATE over the embed call), so they are
			// counted and named in every loss line, but raise no WARN of
			// their own.
			blockReadSkipped.Add(int64(rows) - inserted)
			slog.Debug("access log: read record rows skipped", "rows", rows, "inserted", inserted)
		}
		w.pending.Add(-int64(n))
	}
}

// warnBlockReadLoss logs a loss at WARN at most once per
// blockReadLossWarnInterval, process-wide, with the totals since start.
func warnBlockReadLoss(msg, action string, err error) {
	now := time.Now().UnixNano()
	last := blockReadWarned.Load()
	if last != 0 && now-last < int64(blockReadLossWarnInterval) {
		return
	}
	if !blockReadWarned.CompareAndSwap(last, now) {
		return
	}
	failed, dropped, skipped := BlockReadsLost()
	attrs := []any{"action", action, "rows_failed_total", failed, "rows_dropped_total", dropped, "rows_skipped_total", skipped}
	if err != nil {
		attrs = append(attrs, "error", err)
	}
	slog.Warn(msg, attrs...)
}

// SettleBlockReads blocks until every read RecordBlockReads accepted on pool
// has been written or lost — queued ones included — or ctx ends. ctxd calls it
// at shutdown after the HTTP drain, so the reads of the last requests land
// before the pool closes.
func SettleBlockReads(ctx context.Context, pool *pgxpool.Pool) error {
	n := &blockReadWriterFor(pool).pending
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for n.Load() > 0 {
		select {
		case <-ctx.Done():
			return fmt.Errorf("access log: settle with %d read record(s) pending: %w", n.Load(), ctx.Err())
		case <-tick.C:
		}
	}
	return nil
}

// insertBlockReads writes the rows of a batch in ONE statement and returns how
// many it wrote.
//
// The block rows are taken FOR KEY SHARE … SKIP LOCKED, joined, not trusted
// from the caller:
//
//   - type and created_at come from the row itself, so the record carries
//     what the block was when it was read;
//   - a block deleted since it was read is skipped instead of failing the
//     whole batch: block_id is a foreign key, and one id whose row is gone
//     would otherwise refuse the statement with 23503 and lose the other reads
//     with it;
//   - KEY SHARE is the lock the foreign-key check takes anyway. It conflicts
//     with a delete and with an explicit FOR UPDATE, never with an update or
//     an archive (FOR NO KEY UPDATE). SKIP LOCKED therefore skips the rows a
//     delete in flight holds — the insert never waits behind a purge — and
//     also the rows a picker holds FOR UPDATE at that moment: the embedding
//     backfill and migration (over the embed call) and the dream pick. The
//     guard batch holds its blocks FOR NO KEY UPDATE (issue #359), which does
//     not conflict, so a block under its duplicate check is recorded. Waiting
//     instead would hold one of the two insert slots for as long as those
//     run. A skipped row is counted (BlockReadsLost); the types the record
//     exists for (checkpoint, insight) are on none of those pickers' paths;
//   - the key row is joined the same way: a key deleted since it
//     authenticated skips its rows (api_key_id is a foreign key too), and
//     principal_id is read from it — the person anchor of LogAccess (096).
func insertBlockReads(ctx context.Context, pool *pgxpool.Pool, batch []blockRead) (int64, error) {
	var keys, actions, ids []string
	for _, r := range batch {
		for _, id := range r.ids {
			keys = append(keys, r.apiKeyID)
			actions = append(actions, r.action)
			ids = append(ids, id)
		}
	}
	tag, err := pool.Exec(ctx,
		`INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id)
		 SELECT k.id, b.id, r.action,
		        jsonb_build_object('type', b.type_name, 'block_created_at', b.created_at),
		        k.principal_id
		   FROM unnest($1::text[], $2::text[], $3::text[]) AS r(key_id, action, block_id)
		   JOIN context_blocks b ON b.id = r.block_id::uuid
		   JOIN context_api_keys k ON k.id = r.key_id::uuid
		    FOR KEY SHARE OF b, k SKIP LOCKED`,
		keys, actions, ids,
	)
	if err != nil {
		return 0, fmt.Errorf("store: insert block reads: %w", err)
	}
	return tag.RowsAffected(), nil
}
