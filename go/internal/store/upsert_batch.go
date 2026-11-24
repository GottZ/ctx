// All-or-none block batches (issue #211): several store writes applied in ONE
// transaction, so a client whose logical record is several blocks — a record
// and the index block that points at it, a correction and the block it
// supersedes — never leaves the store holding a prefix of them.
//
// The batch is built from the in-transaction halves the single writes already
// run (prepareUpsert/exec, execDeclaring), not from a copy of them: every rule
// UpsertBlock and UpsertBlockSupersedes apply inside their transaction — the
// identity row lock, the I7/S3 provenance gate, the stale-vector clear, the
// supersedes gates and declaration — applies to each item here unchanged.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/pgxdb"
)

// BatchUpsert is one item of UpsertBlocks: the arguments of UpsertBlock, plus
// the block the write declares to supersede ("" = none), which makes the item
// UpsertBlockSupersedes' write.
type BatchUpsert struct {
	Category, Title, Content string
	Tags                     []string
	Metadata                 map[string]any
	Scope                    string
	ScopeExplicit            bool
	Sens                     SensitivityWrite
	TypeName                 string
	Supersedes               string
}

// BatchItemError is the refusal or failure of one item of UpsertBlocks. Index
// is the item's position in the caller's list, Err what UpsertBlock or
// UpsertBlockSupersedes would have returned for that item alone
// (ErrProvenanceProtected, ErrRetypeBound, ErrSupersedesTarget,
// *SupersedesConflictError, or a fault), reachable through errors.Is/As. The
// whole batch is rolled back: no item of it is written.
type BatchItemError struct {
	Index int
	Err   error
}

func (e *BatchItemError) Error() string {
	return fmt.Sprintf("store: block batch item %d: %v", e.Index, e.Err)
}

func (e *BatchItemError) Unwrap() error { return e.Err }

// BatchResult is one item's outcome of UpsertBlocks.
type BatchResult struct {
	// Block is the written block; nil for an item the batch left as it was.
	Block *Block
	// Match is the item's identical-content comparison (IdenticalContentMatch's
	// answer), made on its row while the transaction holds that row's lock:
	// Match.Unchanged() is an item the batch did not write (Block nil, Match.ID
	// the block it matches), Match.Changed() names what an identical-content
	// write rewrote. Zero for an item that declares a supersedes — such a write
	// is never a no-op, as on /api/store.
	Match IdenticalContent
}

// UpsertBlocks writes every item or none, in one transaction, and returns
// each item's outcome in the order of items.
//
// An item whose write would change nothing — the identical-content no-op of
// /api/store (IdenticalContentMatch: content, tags, metadata, sensitivity,
// type) — is not written. That is decided HERE, on the item's row while this
// transaction holds its lock, not before the transaction: a decision taken on
// a row nobody holds lets another writer change it in between, and the batch
// would then write the rest of its items around a value it no longer matches
// — a store holding part of this batch beside part of another, which no order
// of the two batches produces (review of #389). The row of a no-op item stays
// locked to the commit, so the value the batch reports unchanged is the value
// it commits beside its writes.
//
// The caller has run the write gates of each item (the handler chain) and
// passes items whose (category, title, scope) identities are pairwise distinct;
// this layer trusts its caller for that as UpsertBlock does for sensitivity
// levels and type names. apiKeyID and types are what a supersedes declaration
// records and checks (SupersedesDeclaration).
//
// budget is the key's write budget, spent inside this transaction: checked
// after the no-op decisions and before the first write, for the items that
// write (a batch whose writes do not fit is refused with *WriteBudgetError and
// writes nothing; a batch that writes nothing is neither counted nor
// refused), and booked, one 'write' row per written block, with the commit —
// so a batch is never written without its booking, and never booked without
// being written. nil books nothing: the confirm of a staged batch, which paid
// at stage time (ReserveWriteIntents).
//
// Lock order, the same for every batch, so two batches never wait for each
// other in opposite orders:
//
//  1. the key's budget lock (WriteBudget, an advisory lock no row-lock holder
//     takes), so two batches of one key count and book one after the other —
//     on every ctxd instance of the database;
//  2. the supersedes lock of every scope the batch names — written or not —
//     EXCLUSIVE, in sorted scope order, before any row lock
//     (LockSupersedesScope: "the lock is the FIRST its transaction takes" —
//     the budget lock before it holds no row). A single upsert holds one block
//     row and waits for nothing else while it holds it; a batch holds several
//     and waits for the next one, so every other transaction that holds block
//     rows and then waits for more would close a lock cycle with it. The
//     exclusive lock keeps every holder of the scope's lock out for the
//     batch's duration — dream link batches (they hold the key-share lock of
//     every link endpoint they inserted and then wait for the next), archives
//     and scope moves (lockSupersedesSource: a block and its targets in id
//     order), declarations and the other batches of the scope. A scope without
//     any supersedes item takes it as well, for exactly that reason;
//  3. the items in (scope, category, title) order, every item: the identity
//     row FOR UPDATE (with the no-op comparison, lockBatchMatch), and for a
//     supersedes item the target FOR NO KEY UPDATE after it
//     (lockSupersedesPair). Only then are the writes made, in the same order;
//     they wait for no row lock this pass did not take, except an identity
//     another transaction is inserting right then (the unique index).
//
// Paths that hold several block rows without the scope lock remain: the guard
// run (its picked blocks FOR NO KEY UPDATE, then the key-share lock of a
// matched neighbour at its audit insert), an operator's guard-resolve batch
// (ids FOR UPDATE in scan order), a scope purge (blocks FOR UPDATE in scan
// order) and the resolve of a dream link (the link row, then its target). A
// batch that holds a block such a path wants next — the row of a no-op item
// as well as a written one — while it waits for one that path holds meets it
// in a cycle that PostgreSQL ends with 40P01 after deadlock_timeout — a cycle
// one single
// UpsertBlockSupersedes can meet already (identity row, then target). The
// batch then answers the error with nothing written; all-or-none holds either
// way.
func UpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set, budget *WriteBudget) ([]BatchResult, error) {
	var results []BatchResult
	times := batchContentTimes(items)
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: upsert block batch"), func(tx pgx.Tx) error {
		if budget != nil {
			if err := budget.lock(ctx, tx); err != nil {
				return err
			}
		}
		var err error
		if results, err = upsertBlocksTx(ctx, tx, items, times, apiKeyID, types, budget); err != nil {
			return err
		}
		if budget == nil {
			return nil
		}
		ids := make([]string, 0, len(results))
		for _, r := range results {
			if r.Block != nil {
				ids = append(ids, r.Block.ID)
			}
		}
		if len(ids) == 0 {
			return nil
		}
		return budget.book(ctx, tx, ids, 0)
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

// CheckUpsertBlocks runs UpsertBlocks' transaction without its budget and
// rolls it back: every refusal the batch would meet on the rows as they are
// now — the supersedes declarations against each other included (two items
// naming one target, two items declaring each other), which a check of one
// item at a time cannot see. The staged MCP batch runs it, so a card never
// promises a batch its confirm would refuse on today's rows; the confirm
// decides again.
func CheckUpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set) error {
	times := batchContentTimes(items)
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: check block batch"), func(tx pgx.Tx) error {
		if _, err := upsertBlocksTx(ctx, tx, items, times, apiKeyID, types, nil); err != nil {
			return err
		}
		return pgxdb.ErrRollback
	})
	if errors.Is(err, pgxdb.ErrRollback) {
		return nil
	}
	return err
}

// batchHold is what the lock pass of a batch learned about one item: whether
// its identity row exists (the pre-read's answer), its no-op comparison, and
// the locked target of a supersedes item.
type batchHold struct {
	hadRow bool
	match  IdenticalContent
	target supersedesTarget
}

// batchContentTimes is temporalExtraction of every item's content, computed
// before the batch's transaction (prepareUpsert says why).
func batchContentTimes(items []BatchUpsert) [][]time.Time {
	times := make([][]time.Time, len(items))
	for i, it := range items {
		times[i] = temporalExtraction(it.Content)
	}
	return times
}

// upsertBlocksTx is the batch inside its transaction (see UpsertBlocks): the
// scope locks, the lock pass over every item with its no-op decision, the
// budget check for the items that write, then the writes. budget nil checks
// nothing; its lock is the caller's, taken before this. times holds each
// item's content_times (batchContentTimes).
func upsertBlocksTx(ctx context.Context, tx pgx.Tx, items []BatchUpsert, times [][]time.Time, apiKeyID string, types *blocktype.Set, budget *WriteBudget) ([]BatchResult, error) {
	for _, scope := range batchScopes(items) {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return nil, err
		}
	}
	order := batchLockOrder(items)
	holds := make([]batchHold, len(items))
	writes := 0
	for _, i := range order {
		h, err := lockBatchItem(ctx, tx, items[i])
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
		holds[i] = h
		if !h.match.Unchanged() {
			writes++
		}
	}
	if budget != nil {
		if err := budget.check(ctx, tx, writes); err != nil {
			return nil, err
		}
	}
	results := make([]BatchResult, len(items))
	for _, i := range order {
		results[i].Match = holds[i].match
		if holds[i].match.Unchanged() {
			continue
		}
		b, err := writeBatchItem(ctx, tx, items[i], times[i], holds[i], apiKeyID, types)
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
		results[i].Block = b
	}
	return results, nil
}

// lockBatchItem is the lock pass of one item: a supersedes item's lock pair
// (identity row, then target — lockSupersedesPair, the locks
// UpsertBlockSupersedes takes), every other item's identity row with its
// no-op comparison (lockBatchMatch).
func lockBatchItem(ctx context.Context, tx pgx.Tx, it BatchUpsert) (batchHold, error) {
	if it.Supersedes != "" {
		tgt, sourceID, err := lockSupersedesPair(ctx, tx, it.Category, it.Title, it.Scope, it.Supersedes)
		if err != nil {
			return batchHold{}, err
		}
		return batchHold{hadRow: sourceID != "", target: tgt}, nil
	}
	return lockBatchMatch(ctx, tx, it)
}

// lockBatchMatch locks the item's identity row FOR UPDATE — the pre-read of
// UpsertBlock's exec, same predicate — and compares it with the item in the
// same statement (identicalContentCompare, IdenticalContentMatch's
// comparison). Under READ COMMITTED a FOR UPDATE that waited for another
// writer of the row returns the row as that writer committed it, the select
// list evaluated on that version: the comparison is made on the row as it is
// once this transaction holds it.
func lockBatchMatch(ctx context.Context, tx pgx.Tx, it BatchUpsert) (batchHold, error) {
	cols, args := identicalContentCompare(it.Content, it.Scope, it.Category, it.Title, it.Tags, it.Metadata, it.Sens, it.TypeName)
	var id string
	var sameContent bool
	var m IdenticalContent
	err := tx.QueryRow(ctx,
		`SELECT id::text, content_hash = encode(digest($1, 'sha256'), 'hex'), `+cols+`
		   FROM context_blocks
		  WHERE category = $3 AND title = $4 AND scope = $2 AND NOT is_archived
		  FOR UPDATE`,
		args...,
	).Scan(&id, &sameContent, &m.TagsEqual, &m.MetadataEqual, &m.SensitivityEqual, &m.TypeEqual)
	if errors.Is(err, pgx.ErrNoRows) {
		return batchHold{}, nil
	}
	if err != nil {
		return batchHold{}, fmt.Errorf("store: upsert block batch: lock: %w", err)
	}
	if !sameContent {
		return batchHold{hadRow: true}, nil
	}
	m.ID = id
	return batchHold{hadRow: true, match: m}, nil
}

// WriteBudget is a key's write budget (query.rate_limit_write: writes per api
// key and 60-second window, counted over context_access_log action 'write')
// as a block batch spends it inside its own transaction. Limit 0 is no limit:
// nothing is refused, every write is still booked.
//
// A single store checks the count and books after its commit, so concurrent
// single stores of one key can overshoot the limit by one each; a batch would
// overshoot by its size. The budget lock serializes the batches (and the
// staged batches) of one key between the count and the booking instead.
// Single stores do not take it: a single store whose check-to-booking stretch
// overlaps a batch's count-to-commit stretch (the count after the batch's lock
// pass, right before its first write) is not in the batch's count, and the
// batch's writes are not in its count, so beside the overshoot single stores
// have among themselves the two exceed the limit together by at most the
// smaller of the batch's writes and the number of such single stores
// (docs/api.md, POST /api/store/batch).
type WriteBudget struct {
	APIKeyID string
	Limit    int
}

// WriteBudgetError is the refusal of a batch whose writes do not fit into what
// is left of the window.
type WriteBudgetError struct {
	Limit, Writes, Used int
}

func (e *WriteBudgetError) Error() string {
	return fmt.Sprintf("store: write budget: %d writes do not fit, %d of %d used", e.Writes, e.Used, e.Limit)
}

// writeBudgetLockSQL is the key's budget lock: a transaction advisory lock in a
// two-key space of its own ('ctx:write-budget'), which shares no key with the
// one-key supersedes scope lock or the other two-key classes.
const writeBudgetLockSQL = `SELECT pg_advisory_xact_lock(hashtext('ctx:write-budget'), hashtext($1::text))`

// writeBookingSQL books one 'write' row per block id ($2) or, with $2 empty,
// $3 rows without a block (the write INTENTS of a stage) — LogAccess's row,
// principal included. created_at is the time of the booking
// (clock_timestamp), not the column default now(): now() is the start of the
// transaction, and a batch that waited for the budget lock or a scope lock
// would book its rows that far in the past — past the 60 s window after a
// long wait, where no later count of the key sees them.
const writeBookingSQL = `INSERT INTO context_access_log (api_key_id, block_id, action, metadata, principal_id, created_at)
	SELECT $1::uuid, b.id, 'write', '{}'::jsonb, (SELECT k.principal_id FROM context_api_keys k WHERE k.id = $1::uuid), clock_timestamp()
	  FROM (SELECT unnest($2::uuid[]) AS id
	        UNION ALL
	        SELECT NULL::uuid FROM generate_series(1, $3::int)) b`

// reserve takes the budget lock and refuses n writes that do not fit.
func (b WriteBudget) reserve(ctx context.Context, tx pgx.Tx, n int) error {
	if err := b.lock(ctx, tx); err != nil {
		return err
	}
	return b.check(ctx, tx, n)
}

// lock takes the key's budget lock (nothing for Limit 0).
func (b WriteBudget) lock(ctx context.Context, tx pgx.Tx) error {
	if b.Limit <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, writeBudgetLockSQL, b.APIKeyID); err != nil {
		return fmt.Errorf("store: write budget lock: %w", err)
	}
	return nil
}

// check refuses n writes that do not fit; the caller holds the budget lock
// (lock). The count runs after the lock, in a statement of its own, so it
// sees every booking a batch of this key committed while this one waited. n 0
// counts nothing and refuses nothing.
func (b WriteBudget) check(ctx context.Context, tx pgx.Tx, n int) error {
	if b.Limit <= 0 || n == 0 {
		return nil
	}
	used, err := CheckRateLimitByAction(ctx, tx, b.APIKeyID, "write")
	if err != nil {
		return err
	}
	if used+n > b.Limit {
		return &WriteBudgetError{Limit: b.Limit, Writes: n, Used: used}
	}
	return nil
}

// book writes the booking rows in the transaction: one per block id, plus
// intents rows without a block.
func (b WriteBudget) book(ctx context.Context, tx pgx.Tx, blockIDs []string, intents int) error {
	if blockIDs == nil {
		blockIDs = []string{}
	}
	if _, err := tx.Exec(ctx, writeBookingSQL, b.APIKeyID, blockIDs, intents); err != nil {
		return fmt.Errorf("store: write booking: %w", err)
	}
	return nil
}

// ReserveWriteIntents spends n writes of the budget as write INTENTS (block_id
// NULL) in the caller's transaction: the stage of an MCP batch for a
// confirm_writes key, which stages its card in the same transaction, so the
// card and its charge commit together and two stages of one key count one
// after the other. The confirm books nothing.
func ReserveWriteIntents(ctx context.Context, tx pgx.Tx, b WriteBudget, n int) error {
	if err := b.reserve(ctx, tx, n); err != nil {
		return err
	}
	return b.book(ctx, tx, nil, n)
}

// writeBatchItem runs one item's write after the lock pass took its locks:
// UpsertBlock's transaction half, or UpsertBlockSupersedes' for an item that
// declares a supersedes. A declaring item checks its target as the items
// written before it left that target (rereadSupersedesTarget): an earlier item
// of the batch may have upserted the target's identity with another type, and
// the lock pass read it before that write (review of #389).
func writeBatchItem(ctx context.Context, tx pgx.Tx, it BatchUpsert, contentTimes []time.Time, h batchHold, apiKeyID string, types *blocktype.Set) (*Block, error) {
	u := prepareUpsert(it.Category, it.Title, it.Content, it.Tags, it.Metadata, it.Scope, it.ScopeExplicit, it.Sens, it.TypeName, contentTimes)
	if it.Supersedes == "" {
		return u.execHeld(ctx, tx, h.hadRow)
	}
	tgt, err := rereadSupersedesTarget(ctx, tx, h.target, it.Scope)
	if err != nil {
		return nil, err
	}
	return u.execDeclared(ctx, tx, SupersedesDeclaration{TargetID: it.Supersedes, APIKeyID: apiKeyID, Types: types}, tgt)
}

// batchLockOrder is the order UpsertBlocks writes the items in: by
// (scope, category, title), the position in the list breaking a tie.
func batchLockOrder(items []BatchUpsert) []int {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := items[order[a]], items[order[b]]
		if x.Scope != y.Scope {
			return x.Scope < y.Scope
		}
		if x.Category != y.Category {
			return x.Category < y.Category
		}
		return x.Title < y.Title
	})
	return order
}

// batchScopes is the sorted set of scopes the items write.
func batchScopes(items []BatchUpsert) []string {
	scopes := make([]string, 0, 1)
	for _, it := range items {
		if !slices.Contains(scopes, it.Scope) {
			scopes = append(scopes, it.Scope)
		}
	}
	sort.Strings(scopes)
	return scopes
}
