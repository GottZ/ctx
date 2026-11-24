// All-or-none block batches (issue #211): several store writes and block
// updates applied in ONE transaction, so a client whose logical record is
// several blocks — a record and the index block that points at it, a
// correction and the block it supersedes — never leaves the store holding a
// prefix of them.
//
// The batch is built from the in-transaction halves the single writes already
// run (prepareUpsert/exec, execDeclaring, prepareUpdate/execHeld,
// LinkBlockParent), not from a copy of them: every rule UpsertBlock,
// UpsertBlockSupersedes and UpdateBlock apply inside their transaction — the
// identity row lock, the I7/S3 provenance gate, the stale-vector clear, the
// supersedes gates and declaration, the guard-key protection, the edits on the
// held row, the scope move's sweep — applies to each item here unchanged.
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
// UpsertBlockSupersedes' write, and the parent the written block hangs under.
// With Update set the item is an update of an existing block by id instead,
// and every other field is unused (issue #211).
type BatchUpsert struct {
	Category, Title, Content string
	Tags                     []string
	Metadata                 map[string]any
	Scope                    string
	ScopeExplicit            bool
	Sens                     SensitivityWrite
	TypeName                 string
	Supersedes               string
	// ParentID hangs the written block under an existing block, the
	// /api/store parent_id: a full uuid of a live block in the item's scope,
	// not the item's own identity (lockBatchParent), not closing a parent
	// cycle (LinkBlockParent). "" = none.
	ParentID string
	// ParentItem hangs the written block under the block an EARLIER item of
	// the same batch writes, by that item's index in the list — the one parent
	// a client cannot name by id before the batch ran. nil = none. A reference
	// to the item itself or a later one is refused (ErrBatchParentItem): every
	// link points backwards in the list, so the batch's own links cannot form
	// a cycle among its items.
	ParentItem *int
	// Update makes the item an update of an existing block by id, the update
	// form (UpdateBlock's arguments); nil for an upsert.
	Update *BatchUpdate
}

// BatchUpdate is an update item of UpsertBlocks: UpdateBlock's arguments.
type BatchUpdate struct {
	ID          string
	Data        UpdateBlockData
	WriteScopes []string
}

// ErrBatchParentItem refuses a ParentItem that does not name an earlier item
// of the batch, and a parent on an update item (UpdateBlock carries none).
var ErrBatchParentItem = errors.New("store: block batch: parent_item must name an earlier item of the batch")

// BatchSameBlockError refuses the later of two items of one batch that would
// write one block: the same row (two update items of one id, a store item
// whose identity is an update item's row) or, with Identity, the same
// (category, title, scope) after the batch (an update item renamed or moved
// onto the identity a store item or another update item writes). First is the
// earlier item's index. Which of the two the block would keep would depend on
// the order the batch writes in, so neither is written.
type BatchSameBlockError struct {
	First    int
	Identity bool
}

func (e *BatchSameBlockError) Error() string {
	if e.Identity {
		return fmt.Sprintf("store: block batch: same category, title and scope as items[%d]", e.First)
	}
	return fmt.Sprintf("store: block batch: same block as items[%d]", e.First)
}

// BatchItemError is the refusal or failure of one item of UpsertBlocks. Index
// is the item's position in the caller's list, Err what UpsertBlock,
// UpsertBlockSupersedes or UpdateBlock would have returned for that item alone
// (ErrProvenanceProtected, ErrRetypeBound, ErrSupersedesTarget,
// *SupersedesConflictError, *BlockIdentityTakenError, *ContentEditError,
// ErrBlockChanged, ErrBlockNotFound for an update item whose block is not a
// live block of its write scopes, ErrLinkScopeViolation for a parent, or a
// fault), reachable through errors.Is/As. The whole batch is rolled back: no
// item of it is written.
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
	// write rewrote. Zero for an item that declares a supersedes or names a
	// parent — such a write is never a no-op, as on /api/store. For an update
	// item it is Unchanged() exactly when the update would change no column
	// but updated_at (Block nil, Match.ID the block).
	Match IdenticalContent
}

// UpsertBlocks writes every item or none, in one transaction, and returns
// each item's outcome in the order of items.
//
// An item whose write would change nothing — the identical-content no-op of
// /api/store (IdenticalContentMatch: content, tags, metadata, sensitivity,
// type), or an update item whose every assignment already holds — is not
// written. That is decided HERE, on the item's row while this transaction
// holds its lock, not before the transaction: a decision taken on a row nobody
// holds lets another writer change it in between, and the batch would then
// write the rest of its items around a value it no longer matches — a store
// holding part of this batch beside part of another, which no order of the two
// batches produces (review of #389). The row of a no-op item stays locked to
// the commit, so the value the batch reports unchanged is the value it commits
// beside its writes.
//
// The caller has run the write gates of each item (the handler chain) and
// passes store items whose (category, title, scope) identities are pairwise
// distinct; this layer trusts its caller for that as UpsertBlock does for
// sensitivity levels and type names. Two items that meet on one block anyway —
// through a row an update item names by id, or an identity an update item
// moves onto — are refused here (*BatchSameBlockError), because only the
// transaction knows the rows. apiKeyID and types are what a supersedes
// declaration records and checks (SupersedesDeclaration).
//
// An update item runs UpdateBlock's statement through UpdateBlock's
// transaction half (execHeld) — the provenance exclusion, the guard-key
// protection of its metadata, the identity collision, the re-type bound, the
// edits on the held row, the ExpectUpdatedAt pin of a confirmed card, the
// scope move's link sweep and supersedes reconcile, the vector invalidation —
// after this batch's lock pass took its locks. A parent (ParentID,
// ParentItem) is linked after every item is written, in list order, through
// the one parent check of /api/store (LinkBlockParent).
//
// budget is the key's write budget, spent inside this transaction after the
// writes (WriteBudget.spend): the items that wrote are counted (a batch whose
// writes do not fit is refused with *WriteBudgetError and its writes roll
// back; a batch that writes nothing is neither counted nor refused) and
// booked, one 'write' row per written block, with the commit — so a batch is
// never written without its booking, and never booked without being written.
// nil books nothing: the confirm of a staged batch, which paid
// at stage time (ReserveWriteIntents), and a single /api/store with a parent,
// which books as every single store does.
//
// Lock order, the same for every batch, so two batches never wait for each
// other in opposite orders:
//
//  1. the supersedes lock of every scope the batch names — written or not,
//     an update item's current scope (read without a lock first,
//     readBatchUpdateTargets) and the scope it moves to — EXCLUSIVE, in sorted
//     scope order, before any row lock (LockSupersedesScope: "the lock is the
//     FIRST its transaction takes").
//     A single upsert holds one block row and waits for nothing else while it
//     holds it; a batch holds several and waits for the next one, so every
//     other transaction that holds block rows and then waits for more would
//     close a lock cycle with it. The exclusive lock keeps every holder of the
//     scope's lock out for the batch's duration — dream link batches (they
//     hold the key-share lock of every link endpoint they inserted and then
//     wait for the next), archives and scope moves (lockSupersedesSource: a
//     block and its targets in id order), declarations, guard-resolve batches
//     and the other batches of the scope. A scope without any supersedes item
//     takes it as well, for exactly that reason;
//  2. the items in (scope, category, title) order — an update item by its
//     current identity — every item: the identity row FOR NO KEY UPDATE (with
//     the no-op comparison, lockBatchMatch), for a supersedes item the target
//     FOR NO KEY UPDATE after it (lockSupersedesPair), for a parent named by
//     id the parent FOR SHARE after it (lockBatchParent), for an update item
//     its row by id within the scope read in step 1 (lockBatchUpdate) and, for
//     a scope move, the targets of its supersedes links after it, in id order.
//     Only then are the writes made, in the same order; they wait for no row
//     lock this pass did not take, except an identity another transaction is
//     inserting right then (the unique index);
//  3. when an item wrote, the key's budget lock (WriteBudget, an advisory
//     lock), the count and the booking — the last lock, held to the commit, as
//     in a single update (UpdateBlockBooked): two writers of one key count and
//     book one after the other, on every ctxd instance of the database, while
//     a writer that waits for a row holds no budget lock and keeps no other
//     write of its key waiting (review of PR #553), and the holder waits for
//     nothing but its own count and booking.
//
// An update item whose block left the scope read in step 1 before the lock
// pass reached it runs the whole transaction again (writeSupersedesSource):
// the next read names the new scope, and its lock is taken in order.
//
// Paths that hold several block rows without the scope lock remain: the guard
// run (its picked blocks FOR NO KEY UPDATE, then the key-share lock of a
// matched neighbour at its audit insert; its archive verdict takes the scope
// lock and its targets only when nobody holds them) and the resolve of a
// dream link that is not a supersedes (the link row, then nothing). The
// identity rows are locked FOR NO KEY UPDATE, not FOR UPDATE, for the first
// of them (issue #284): FOR UPDATE refused the run's key-share lock on a
// matched neighbour the batch held while the batch waited for a block the run
// had picked — 97 to 100 of 200 rounds of a batch beside a guard run ended
// in 40P01. A batch that meets a cycle anyway answers the error with nothing
// written; all-or-none holds either way.
func UpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set, budget *WriteBudget) ([]BatchResult, error) {
	prep, err := prepareBatch(items)
	if err != nil {
		return nil, err
	}
	var results []BatchResult
	err = writeSupersedesSource(ctx, pool, pgxdb.At("store: upsert block batch"), func(tx pgx.Tx) error {
		var err error
		if results, err = upsertBlocksTx(ctx, tx, items, prep, apiKeyID, types); err != nil {
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
		return budget.spend(ctx, tx, ids)
	})
	if err != nil {
		batchIdentityHolder(ctx, pool, items, err)
		return nil, err
	}
	for i, u := range prep.updates {
		if u != nil && results[i].Block != nil {
			u.handOver(results[i].Block)
		}
	}
	return results, nil
}

// CheckUpsertBlocks runs UpsertBlocks' transaction without its budget and
// rolls it back: every refusal the batch would meet on the rows as they are
// now — the supersedes declarations against each other included (two items
// naming one target, two blocks declaring each other), which a check of one
// item at a time cannot see. The staged MCP batch runs it, so a card never
// promises a batch its confirm would refuse on today's rows; the confirm
// decides again.
func CheckUpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set) error {
	prep, err := prepareBatch(items)
	if err != nil {
		return err
	}
	err = writeSupersedesSource(ctx, pool, pgxdb.At("store: check block batch"), func(tx pgx.Tx) error {
		if _, err := upsertBlocksTx(ctx, tx, items, prep, apiKeyID, types); err != nil {
			return err
		}
		return pgxdb.ErrRollback
	})
	if errors.Is(err, pgxdb.ErrRollback) {
		return nil
	}
	batchIdentityHolder(ctx, pool, items, err)
	return err
}

// batchPrep is what a batch computes before its transaction: each store
// item's content_times (prepareUpsert says why), each update item's prepared
// statement (prepareUpdate).
type batchPrep struct {
	times   [][]time.Time
	updates []*blockUpdate
}

// prepareBatch prepares every item, refusing a ParentItem that names no
// earlier item and a parent on an update item (ErrBatchParentItem), and an
// update the store would refuse unsent (CheckUpdatePatch, no field).
func prepareBatch(items []BatchUpsert) (batchPrep, error) {
	p := batchPrep{times: make([][]time.Time, len(items)), updates: make([]*blockUpdate, len(items))}
	for i, it := range items {
		if it.Update != nil {
			if it.ParentID != "" || it.ParentItem != nil {
				return batchPrep{}, &BatchItemError{Index: i, Err: ErrBatchParentItem}
			}
			u, err := prepareUpdate(it.Update.ID, it.Update.Data, it.Update.WriteScopes)
			if err != nil {
				return batchPrep{}, &BatchItemError{Index: i, Err: err}
			}
			p.updates[i] = &u
			continue
		}
		if it.ParentItem != nil && (*it.ParentItem < 0 || *it.ParentItem >= i) {
			return batchPrep{}, &BatchItemError{Index: i, Err: ErrBatchParentItem}
		}
		p.times[i] = temporalExtraction(it.Content)
	}
	return p, nil
}

// batchHold is what the lock pass of a batch learned about one item: the row
// it holds (rowID, "" for a store item whose identity has no row yet) and
// whether that row existed (the pre-read's answer), its no-op comparison, the
// locked target of a supersedes item, the identity the item's block has after
// the batch, and for an update item the identity its block has on the locked
// row (from) and the supersedes targets a scope move locked.
type batchHold struct {
	rowID    string
	hadRow   bool
	match    IdenticalContent
	target   supersedesTarget
	identity batchIdentity
	from     batchIdentity
	targets  []string
}

// batchIdentity is a block's (scope, category, title).
type batchIdentity struct{ scope, category, title string }

// upsertBlocksTx is the batch inside its transaction (see UpsertBlocks): the
// update items' scopes read, the scope locks, the lock pass over every item
// with its no-op decision, the writes, then the parent links. The budget is
// the caller's, spent after this (WriteBudget.spend).
func upsertBlocksTx(ctx context.Context, tx pgx.Tx, items []BatchUpsert, prep batchPrep, apiKeyID string, types *blocktype.Set) ([]BatchResult, error) {
	current, err := readBatchUpdateTargets(ctx, tx, items)
	if err != nil {
		return nil, err
	}
	for _, scope := range batchScopes(items, current) {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return nil, err
		}
	}
	order := batchLockOrder(items, current)
	holds := make([]batchHold, len(items))
	rows := make(map[string]int, len(items))
	for _, i := range order {
		h, err := lockBatchItem(ctx, tx, items[i], prep.updates[i], current[i])
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
		if first, dup := rows[h.rowID]; dup && h.rowID != "" {
			return nil, &BatchItemError{Index: max(i, first), Err: &BatchSameBlockError{First: min(i, first)}}
		}
		rows[h.rowID] = i
		holds[i] = h
	}
	if err := batchIdentityClash(items, holds); err != nil {
		return nil, err
	}
	results := make([]BatchResult, len(items))
	for _, i := range order {
		results[i].Match = holds[i].match
		if holds[i].match.Unchanged() {
			continue
		}
		var b *Block
		var err error
		if u := prep.updates[i]; u != nil {
			b, err = writeBatchUpdate(ctx, tx, u, holds[i])
		} else {
			b, err = writeBatchItem(ctx, tx, items[i], prep.times[i], holds[i], apiKeyID, types)
		}
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
		results[i].Block = b
	}
	if err := linkBatchParents(ctx, tx, items, results); err != nil {
		return nil, err
	}
	return results, nil
}

// lockBatchItem is the lock pass of one item: an update item's row
// (lockBatchUpdate); a supersedes item's lock pair (identity row, then target
// — lockSupersedesPair, the locks UpsertBlockSupersedes takes); every other
// item's identity row with its no-op comparison (lockBatchMatch). A parent
// named by id is locked after the item's row (lockBatchParent), and an item
// with a parent is never a no-op: the link is part of its write.
func lockBatchItem(ctx context.Context, tx pgx.Tx, it BatchUpsert, u *blockUpdate, cur batchIdentity) (batchHold, error) {
	if u != nil {
		return lockBatchUpdate(ctx, tx, u, cur)
	}
	var h batchHold
	if it.Supersedes != "" {
		tgt, sourceID, err := lockSupersedesPair(ctx, tx, it.Category, it.Title, it.Scope, it.Supersedes)
		if err != nil {
			return batchHold{}, err
		}
		h = batchHold{rowID: sourceID, hadRow: sourceID != "", target: tgt}
	} else {
		var err error
		if h, err = lockBatchMatch(ctx, tx, it); err != nil {
			return batchHold{}, err
		}
	}
	h.identity = batchIdentity{it.Scope, it.Category, it.Title}
	if it.ParentID == "" && it.ParentItem == nil {
		return h, nil
	}
	h.match = IdenticalContent{}
	if it.ParentID != "" {
		if err := lockBatchParent(ctx, tx, it.ParentID, it.Scope, it.Category, it.Title); err != nil {
			return batchHold{}, err
		}
	}
	return h, nil
}

// lockBatchMatch locks the item's identity row FOR NO KEY UPDATE — the
// pre-read of UpsertBlock's exec, same predicate and lock — and compares it
// with the item in the same statement (identicalContentCompare,
// IdenticalContentMatch's comparison). Under READ COMMITTED a lock that waited for another
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
		  FOR NO KEY UPDATE`,
		args...,
	).Scan(&id, &sameContent, &m.TagsEqual, &m.MetadataEqual, &m.SensitivityEqual, &m.TypeEqual)
	if errors.Is(err, pgx.ErrNoRows) {
		return batchHold{}, nil
	}
	if err != nil {
		return batchHold{}, fmt.Errorf("store: upsert block batch: lock: %w", err)
	}
	if !sameContent {
		return batchHold{rowID: id, hadRow: true}, nil
	}
	m.ID = id
	return batchHold{rowID: id, hadRow: true, match: m}, nil
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
// overlaps a batch's count-to-commit stretch (the count after the batch's
// writes, right before its booking) is not in the batch's count, and the
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

// batchLockOrder is the order UpsertBlocks locks and writes the items in: by
// (scope, category, title) — an update item by the identity its block had
// when the batch read it (current) — the position in the list breaking a tie.
func batchLockOrder(items []BatchUpsert, current []batchIdentity) []int {
	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	key := func(i int) batchIdentity {
		if items[i].Update != nil {
			return current[i]
		}
		return batchIdentity{items[i].Scope, items[i].Category, items[i].Title}
	}
	sort.SliceStable(order, func(a, b int) bool {
		x, y := key(order[a]), key(order[b])
		if x.scope != y.scope {
			return x.scope < y.scope
		}
		if x.category != y.category {
			return x.category < y.category
		}
		return x.title < y.title
	})
	return order
}

// batchScopes is the sorted set of scopes the items write: a store item's
// scope, an update item's current scope (current) and the scope it moves to.
func batchScopes(items []BatchUpsert, current []batchIdentity) []string {
	scopes := make([]string, 0, 1)
	add := func(s string) {
		if !slices.Contains(scopes, s) {
			scopes = append(scopes, s)
		}
	}
	for i, it := range items {
		if it.Update == nil {
			add(it.Scope)
			continue
		}
		add(current[i].scope)
		if it.Update.Data.Scope != nil {
			add(*it.Update.Data.Scope)
		}
	}
	sort.Strings(scopes)
	return scopes
}
