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

// UpsertBlocks writes every item or none, in one transaction, and returns the
// written blocks in the order of items.
//
// The caller has run the write gates of each item (the handler chain) and
// passes items whose (category, title, scope) identities are pairwise distinct;
// this layer trusts its caller for that as UpsertBlock does for sensitivity
// levels and type names. apiKeyID and types are what a supersedes declaration
// records and checks (SupersedesDeclaration).
//
// budget is the key's write budget, spent inside this transaction: checked
// before the first write (a batch that does not fit is refused with
// *WriteBudgetError and writes nothing) and booked, one 'write' row per block,
// with the commit — so a batch is never written without its booking, and never
// booked without being written. nil books nothing: the confirm of a staged
// batch, which paid at stage time (ReserveWriteIntents).
//
// Lock order, the same for every batch, so two batches never wait for each
// other in opposite orders:
//
//  1. the key's budget lock (WriteBudget, an advisory lock no row-lock holder
//     takes), so two batches of one key count and book one after the other —
//     on every ctxd instance of the database;
//  2. the supersedes lock of every scope the batch writes, EXCLUSIVE, in
//     sorted scope order, before any row lock (LockSupersedesScope: "the lock
//     is the FIRST its transaction takes" — the budget lock before it holds no
//     row). A single upsert holds one block row and waits for nothing else
//     while it holds it; a batch holds several and waits for the next one, so
//     every other transaction that holds block rows and then waits for more
//     would close a lock cycle with it. The exclusive lock keeps every holder
//     of the scope's lock out for the batch's duration — dream link batches
//     (they hold the key-share lock of every link endpoint they inserted and
//     then wait for the next), archives and scope moves (lockSupersedesSource:
//     a block and its targets in id order), declarations and the other batches
//     of the scope. A scope without any supersedes item takes it as well, for
//     exactly that reason;
//  3. the items in (scope, category, title) order: each item's own
//     transaction half — the identity row FOR UPDATE, and for a supersedes
//     item the target FOR NO KEY UPDATE after it (lockSupersedesPair).
//
// Paths that hold several block rows without the scope lock remain: the guard
// run (its picked blocks FOR NO KEY UPDATE, then the key-share lock of a
// matched neighbour at its audit insert), an operator's guard-resolve batch
// (ids FOR UPDATE in scan order), a scope purge (blocks FOR UPDATE in scan
// order) and the resolve of a dream link (the link row, then its target). A
// batch that holds a block such a path wants next while it waits for one that
// path holds meets it in a cycle that PostgreSQL ends with 40P01 after
// deadlock_timeout — a cycle one single UpsertBlockSupersedes can meet already
// (identity row, then target). The batch then answers the error with nothing
// written; all-or-none holds either way.
func UpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set, budget *WriteBudget) ([]*Block, error) {
	var blocks []*Block
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: upsert block batch"), func(tx pgx.Tx) error {
		if budget != nil {
			if err := budget.reserve(ctx, tx, len(items)); err != nil {
				return err
			}
		}
		var err error
		if blocks, err = upsertBlocksTx(ctx, tx, items, apiKeyID, types); err != nil {
			return err
		}
		if budget == nil {
			return nil
		}
		ids := make([]string, len(blocks))
		for i, b := range blocks {
			ids[i] = b.ID
		}
		return budget.book(ctx, tx, ids, 0)
	})
	if err != nil {
		return nil, err
	}
	return blocks, nil
}

// CheckUpsertBlocks runs UpsertBlocks' transaction without its budget and
// rolls it back: every refusal the batch would meet on the rows as they are
// now — the supersedes declarations against each other included (two items
// naming one target, two items declaring each other), which a check of one
// item at a time cannot see. The staged MCP batch runs it, so a card never
// promises a batch its confirm would refuse on today's rows; the confirm
// decides again.
func CheckUpsertBlocks(ctx context.Context, pool *pgxpool.Pool, items []BatchUpsert, apiKeyID string, types *blocktype.Set) error {
	err := pgxdb.Write(ctx, pool, pgxdb.At("store: check block batch"), func(tx pgx.Tx) error {
		if _, err := upsertBlocksTx(ctx, tx, items, apiKeyID, types); err != nil {
			return err
		}
		return pgxdb.ErrRollback
	})
	if errors.Is(err, pgxdb.ErrRollback) {
		return nil
	}
	return err
}

// upsertBlocksTx is the batch inside its transaction: the scope locks, then
// the items in lock order (see UpsertBlocks).
func upsertBlocksTx(ctx context.Context, tx pgx.Tx, items []BatchUpsert, apiKeyID string, types *blocktype.Set) ([]*Block, error) {
	for _, scope := range batchScopes(items) {
		if err := LockSupersedesScope(ctx, tx, scope); err != nil {
			return nil, err
		}
	}
	blocks := make([]*Block, len(items))
	for _, i := range batchLockOrder(items) {
		b, err := upsertBatchItem(ctx, tx, items[i], apiKeyID, types)
		if err != nil {
			return nil, &BatchItemError{Index: i, Err: err}
		}
		blocks[i] = b
	}
	return blocks, nil
}

// WriteBudget is a key's write budget (query.rate_limit_write: writes per api
// key and 60-second window, counted over context_access_log action 'write')
// as a block batch spends it inside its own transaction. Limit 0 is no limit:
// nothing is refused, every write is still booked.
//
// A single store checks the count and books after its commit, so concurrent
// single stores of one key can overshoot the limit by one each; a batch would
// overshoot by its size. The budget lock serializes the batches (and the
// staged batches) of one key between the count and the booking instead;
// single stores do not take it, so a single store running beside a batch can
// still overshoot by one.
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

// reserve takes the budget lock and refuses n writes that do not fit. The
// count runs after the lock, in a statement of its own, so it sees every
// booking a batch of this key committed while this one waited.
func (b WriteBudget) reserve(ctx context.Context, tx pgx.Tx, n int) error {
	if b.Limit <= 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, writeBudgetLockSQL, b.APIKeyID); err != nil {
		return fmt.Errorf("store: write budget lock: %w", err)
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

// upsertBatchItem runs one item's transaction half: UpsertBlock's, or
// UpsertBlockSupersedes' for an item that declares a supersedes.
func upsertBatchItem(ctx context.Context, tx pgx.Tx, it BatchUpsert, apiKeyID string, types *blocktype.Set) (*Block, error) {
	u := prepareUpsert(it.Category, it.Title, it.Content, it.Tags, it.Metadata, it.Scope, it.ScopeExplicit, it.Sens, it.TypeName)
	if it.Supersedes == "" {
		return u.exec(ctx, tx)
	}
	return u.execDeclaring(ctx, tx, SupersedesDeclaration{TargetID: it.Supersedes, APIKeyID: apiKeyID, Types: types})
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
