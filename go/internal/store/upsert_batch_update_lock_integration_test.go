//go:build integration

// Issue #211, the lock order of a block batch made of UPDATE items against the
// other writers of the same blocks. An update item's batch reads the current
// scope of each block without a lock, takes the supersedes lock of every scope
// it names (the current one and a move's target) exclusively and in sorted
// order, and only then locks the rows, in (scope, category, title) order,
// FOR NO KEY UPDATE (upsert_batch.go, "Lock order"). It holds several rows and
// waits for the next one, so every other writer that holds a row and then
// waits for another could close a cycle with it.
//
// The deterministic tests hold one party on a lock a gate transaction holds
// (or at its COMMIT, testdb.HoldCommitPool), let the other party queue behind
// it, then release: the interleaving in which a wrong order deadlocks. Each
// runs in both list orders of the batch and, where the other writer can be
// held with a lock in hand, in both orders of the two parties. The count runs
// every pair 200 rounds with both parties released together, for the batch
// and for the same updates sent as single UpdateBlock calls.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestUpsertBlocksUpdateItems(TwoBatches|Against|ScopeMove|DeadlockCount)' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ublParty is one writer of a test: its writes on the pool it is given.
type ublParty func(ctx context.Context, pool *pgxpool.Pool) error

// ublGo starts party on pool and returns its result channel.
func ublGo(pool *pgxpool.Pool, party ublParty) <-chan error {
	done := make(chan error, 1)
	go func() { done <- party(context.Background(), pool) }()
	return done
}

// ublContent is an update item that replaces the content of block id.
func ublContent(id, content string, scopes ...string) store.BatchUpsert {
	return ubuUpdate(id, store.UpdateBlockData{Content: ubuPtr(content)}, scopes...)
}

// ublList is the two items in list order x, y, or y, x when reverse is set.
func ublList(reverse bool, x, y store.BatchUpsert) []store.BatchUpsert {
	if reverse {
		return []store.BatchUpsert{y, x}
	}
	return []store.BatchUpsert{x, y}
}

// ublOrders are the two list orders every deterministic test runs the batch in.
var ublOrders = []struct {
	name    string
	reverse bool
}{{"list a, b", false}, {"list b, a", true}}

func ublBatch(items []store.BatchUpsert) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := store.UpsertBlocks(ctx, pool, items, "ubl", nil, nil)
		return err
	}
}

func ublUpdate(id string, data store.UpdateBlockData, scopes []string) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		_, _, err := store.UpdateBlock(ctx, pool, id, data, scopes)
		return err
	}
}

func ublDelete(id string, scopes []string) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := store.DeleteBlock(ctx, pool, id, scopes)
		return err
	}
}

// ublResolve is a guard resolve batch over ids; resolved, if set, receives
// the number of blocks it resolved.
func ublResolve(ids []string, resolution string, scopes []string, resolved *int) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		blocks, _, err := store.GuardResolveBatch(ctx, pool, ids, resolution, scopes)
		if resolved != nil {
			*resolved = len(blocks)
		}
		return err
	}
}

// ublLinks is a dream link batch by its locks (dream.WriteLinks, modelled as
// in TestUpsertBlocksAgainstADreamBatch): the scope's supersedes lock shared,
// then the key-share lock of first (a link already inserted), then of second
// (the next link).
func ublLinks(scope, first, second string) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		return pgxWriteSteps(ctx, pool,
			fmt.Sprintf(`SELECT pg_advisory_xact_lock_shared(hashtext('ctx:supersedes:' || '%s'))`, scope),
			fmt.Sprintf(`SELECT 1 FROM context_blocks WHERE id = '%s' FOR KEY SHARE`, first),
			fmt.Sprintf(`SELECT 1 FROM context_blocks WHERE id = '%s' FOR KEY SHARE`, second))
	}
}

// ublGateRow opens a transaction that holds block id FOR UPDATE, a lock every
// row lock and update of the block waits for. The caller rolls it back.
func ublGateRow(t *testing.T, pool *pgxpool.Pool, id string) pgx.Tx {
	t.Helper()
	ctx := context.Background()
	gate, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := gate.Exec(ctx, `SELECT 1 FROM context_blocks WHERE id = $1 FOR UPDATE`, id); err != nil {
		_ = gate.Rollback(ctx)
		t.Fatal(err)
	}
	return gate
}

// ublOpen rolls the gate back: the party waiting for it goes on.
func ublOpen(t *testing.T, gate pgx.Tx) {
	t.Helper()
	if err := gate.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// ublWaitOn returns once the backend named name waits for a lock of the kind
// want — "advisory", a supersedes scope lock, or "row", any other lock — and
// fails the test when it waits for the other kind: each test claims what a
// party waits for (a scope lock, holding no row), not only that it waits.
func ublWaitOn(t *testing.T, pool *pgxpool.Pool, name, want string) {
	t.Helper()
	for deadline := time.Now().Add(20 * time.Second); ; {
		var event string
		err := pool.QueryRow(context.Background(),
			`SELECT COALESCE(wait_event, '') FROM pg_stat_activity
			  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock'`, name).Scan(&event)
		switch {
		case errors.Is(err, pgx.ErrNoRows) || (err == nil && event == ""):
		case err != nil:
			t.Fatalf("pg_stat_activity: %v", err)
		case (event == "advisory") == (want == "advisory"):
			return
		default:
			t.Fatalf("%s waits on a lock of kind %q, want %s", name, event, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never waited for a lock", name)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// ublThrough fails the test unless the party answered without an error, and
// names a 40P01.
func ublThrough(t *testing.T, name string, done <-chan error) {
	t.Helper()
	if err := ubAwait(t, name, done); err != nil {
		t.Errorf("%s: %v (deadlock: %v)", name, err, ubIsDeadlock(err))
	}
}

// ublRefused fails the test unless the party answered ErrBlockNotFound — and
// not 40P01.
func ublRefused(t *testing.T, name string, done <-chan error) {
	t.Helper()
	if err := ubAwait(t, name, done); !errors.Is(err, store.ErrBlockNotFound) || ubIsDeadlock(err) {
		t.Errorf("%s: %v (deadlock: %v), want the not-found refusal of a block archived before it", name, err, ubIsDeadlock(err))
	}
}

// ublBlock is what these tests read back of a block; guardStatus "" in a
// want is not compared.
type ublBlock struct {
	content, scope string
	archived       bool
	guardStatus    string
}

func ublWant(t *testing.T, pool *pgxpool.Pool, id string, want ublBlock) {
	t.Helper()
	var got ublBlock
	if err := pool.QueryRow(context.Background(),
		`SELECT content, scope, is_archived, COALESCE(guard_status, '') FROM context_blocks WHERE id = $1`, id).
		Scan(&got.content, &got.scope, &got.archived, &got.guardStatus); err != nil {
		t.Fatalf("read %s: %v", id, err)
	}
	if want.guardStatus == "" {
		got.guardStatus = ""
	}
	if got != want {
		t.Errorf("block %s = %+v, want %+v", id, got, want)
	}
}

// ublFlag marks blocks needs_review, the input of a guard resolve. The column
// is the guard run's verdict; no client write path sets it (the resolve tests
// of guard_resolve_batch_integration_test.go seed it the same way).
func ublFlag(t *testing.T, pool *pgxpool.Pool, ids ...string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET guard_status = 'needs_review' WHERE id = ANY($1::uuid[])`, ids); err != nil {
		t.Fatalf("flag %v: %v", ids, err)
	}
}

// TestUpsertBlocksUpdateItemsTwoBatches: two batches of update items over the
// same two blocks in opposite list orders, across two scopes and in one. A
// gate holds b, the block the first batch locks last: the first batch holds
// both scope locks and a and waits for b; the second queues behind it. Opened,
// both write and neither ends in 40P01 — the second waits for the first scope
// lock holding no row — and the second, which wrote last, holds both blocks.
func TestUpsertBlocksUpdateItemsTwoBatches(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	for si, layout := range []struct {
		name      string
		twoScopes bool
	}{{"two scopes", true}, {"one scope", false}} {
		for oi, order := range ublOrders {
			t.Run(layout.name+", first "+order.name, func(t *testing.T) {
				sa := fmt.Sprintf("ubl-bb%d%d-s1", si, oi)
				sb := sa
				if layout.twoScopes {
					sb = fmt.Sprintf("ubl-bb%d%d-s2", si, oi)
				}
				ws := []string{sa, sb}
				a, b := ubSeed(t, pool, sa, "a"), ubSeed(t, pool, sb, "b")
				gate := ublGateRow(t, pool, b)
				defer func() { _ = gate.Rollback(context.Background()) }()

				first := ublGo(ubActor(t, dsn, "ubl-first", nil), ublBatch(ublList(order.reverse,
					ublContent(a, "a first", ws...), ublContent(b, "b first", ws...))))
				ublWaitOn(t, pool, "ubl-first", "row")
				second := ublGo(ubActor(t, dsn, "ubl-second", nil), ublBatch(ublList(!order.reverse,
					ublContent(a, "a second", ws...), ublContent(b, "b second", ws...))))
				ublWaitOn(t, pool, "ubl-second", "advisory")
				ublOpen(t, gate)

				ublThrough(t, "first batch", first)
				ublThrough(t, "second batch", second)
				ublWant(t, pool, a, ublBlock{content: "a second", scope: sa})
				ublWant(t, pool, b, ublBlock{content: "b second", scope: sb})
			})
		}
	}
}

// TestUpsertBlocksUpdateItemsAgainstASingleUpdate: a batch of update items over
// a and b of one scope beside a single UpdateBlock of one of them, in place
// and with a scope move.
//
//   - batch first: a gate holds b; the batch holds the scope lock and a and
//     waits for b. The single update of a waits for the batch — for a's row in
//     place, for the scope lock with a move (lockSupersedesSource takes it
//     first). Opened, the batch writes, then the update.
//   - update first: the single update of b stands before its COMMIT holding b
//     (and, with a move, the scope lock). The batch waits — holding a, for b in
//     place; for the scope lock, holding no row, with a move. Released, the
//     update commits, then the batch writes; after a move its lock pass misses
//     b in the scope it read and the batch runs again in b's new scope.
//
// Neither ends in 40P01; the party that wrote last holds the block.
func TestUpsertBlocksUpdateItemsAgainstASingleUpdate(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	n := 0
	for _, move := range []bool{false, true} {
		for _, updateFirst := range []bool{false, true} {
			for _, order := range ublOrders {
				n++
				name := fmt.Sprintf("in place, batch first, %s", order.name)
				if move {
					name = strings.Replace(name, "in place", "scope move", 1)
				}
				if updateFirst {
					name = strings.Replace(name, "batch first", "update first", 1)
				}
				t.Run(name, func(t *testing.T) {
					s1, s2 := fmt.Sprintf("ubl-su%d-s1", n), fmt.Sprintf("ubl-su%d-s2", n)
					ws := []string{s1, s2}
					a, b := ubSeed(t, pool, s1, "a"), ubSeed(t, pool, s1, "b")
					batch := ublBatch(ublList(order.reverse, ublContent(a, "a batch", ws...), ublContent(b, "b batch", ws...)))
					single := func(id, content string) ublParty {
						data := store.UpdateBlockData{Content: ubuPtr(content)}
						if move {
							data.Scope = ubuPtr(s2)
						}
						return ublUpdate(id, data, ws)
					}
					// In place the two meet at a row; a move's scope lock comes
					// first on both sides, so the later of them waits for it.
					moved, singleWaits := s1, "row"
					if move {
						moved, singleWaits = s2, "advisory"
					}
					if !updateFirst {
						gate := ublGateRow(t, pool, b)
						defer func() { _ = gate.Rollback(context.Background()) }()
						batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
						ublWaitOn(t, pool, "ubl-batch", "row")
						updateDone := ublGo(ubActor(t, dsn, "ubl-single", nil), single(a, "a single"))
						ublWaitOn(t, pool, "ubl-single", singleWaits)
						ublOpen(t, gate)
						ublThrough(t, "batch", batchDone)
						ublThrough(t, "single update", updateDone)
						ublWant(t, pool, a, ublBlock{content: "a single", scope: moved})
						ublWant(t, pool, b, ublBlock{content: "b batch", scope: s1})
						return
					}
					held, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool { return strings.Contains(sql, "UPDATE context_blocks SET") })
					defer hold.Release() // a failing test ends without waiting out the hold
					updateDone := ublGo(held, single(b, "b single"))
					hold.Wait(t)
					batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
					ublWaitOn(t, pool, "ubl-batch", singleWaits)
					hold.Release()
					ublThrough(t, "single update", updateDone)
					ublThrough(t, "batch", batchDone)
					ublWant(t, pool, a, ublBlock{content: "a batch", scope: s1})
					ublWant(t, pool, b, ublBlock{content: "b batch", scope: moved})
				})
			}
		}
	}
}

// TestUpsertBlocksUpdateItemsAgainstAnArchive: a batch of update items over a
// and b beside DeleteBlock of one of them.
//
//   - batch first: a gate holds b; the batch holds the scope lock and a and
//     waits for b. The archive of a waits for the scope lock
//     (lockSupersedesSource), holding no row. Opened, the batch writes, then
//     the archive archives a as the batch left it.
//   - archive first: the archive of b stands before its COMMIT holding the
//     scope lock and b. The batch read b live, without a lock, and waits for
//     the scope lock. Released, the archive commits and the batch's lock pass
//     finds b archived: the batch is refused with ErrBlockNotFound and writes
//     nothing, a included.
//
// Neither ends in 40P01.
func TestUpsertBlocksUpdateItemsAgainstAnArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	n := 0
	for _, archiveFirst := range []bool{false, true} {
		for _, order := range ublOrders {
			n++
			name := "batch first, " + order.name
			if archiveFirst {
				name = "archive first, " + order.name
			}
			t.Run(name, func(t *testing.T) {
				s := fmt.Sprintf("ubl-ar%d", n)
				ws := []string{s}
				a, b := ubSeed(t, pool, s, "a"), ubSeed(t, pool, s, "b")
				batch := ublBatch(ublList(order.reverse, ublContent(a, "a batch", ws...), ublContent(b, "b batch", ws...)))
				if !archiveFirst {
					gate := ublGateRow(t, pool, b)
					defer func() { _ = gate.Rollback(context.Background()) }()
					batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
					ublWaitOn(t, pool, "ubl-batch", "row")
					archiveDone := ublGo(ubActor(t, dsn, "ubl-archive", nil), ublDelete(a, ws))
					ublWaitOn(t, pool, "ubl-archive", "advisory")
					ublOpen(t, gate)
					ublThrough(t, "batch", batchDone)
					ublThrough(t, "archive", archiveDone)
					ublWant(t, pool, a, ublBlock{content: "a batch", scope: s, archived: true})
					ublWant(t, pool, b, ublBlock{content: "b batch", scope: s})
					return
				}
				held, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool { return strings.Contains(sql, "SET is_archived = true") })
				defer hold.Release() // a failing test ends without waiting out the hold
				archiveDone := ublGo(held, ublDelete(b, ws))
				hold.Wait(t)
				batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
				ublWaitOn(t, pool, "ubl-batch", "advisory")
				hold.Release()
				ublThrough(t, "archive", archiveDone)
				ublRefused(t, "batch", batchDone)
				ublWant(t, pool, a, ublBlock{content: "a body", scope: s})
				ublWant(t, pool, b, ublBlock{content: "b body", scope: s, archived: true})
			})
		}
	}
}

// TestUpsertBlocksUpdateItemsAgainstAGuardResolveBatch: a batch of update items
// over a and b, both flagged needs_review, beside GuardResolveBatch of the same
// two blocks, keep and archive.
//
//   - batch first: a gate holds b; the batch holds the scope lock and a and
//     waits for b. The resolve waits for the scope lock (lockGuardBatchScopes:
//     shared for keep, exclusive for archive), holding no row. Opened, the
//     batch writes, then the resolve resolves both blocks.
//   - resolve first: the resolve stands before its COMMIT holding the scope
//     lock and both rows; the batch waits for the scope lock. Released, the
//     resolve commits; after a keep the batch writes, after an archive it is
//     refused with ErrBlockNotFound and writes nothing.
//
// Neither ends in 40P01.
func TestUpsertBlocksUpdateItemsAgainstAGuardResolveBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	n := 0
	for _, resolution := range []string{"keep", "archive"} {
		for _, resolveFirst := range []bool{false, true} {
			for _, order := range ublOrders {
				n++
				name := fmt.Sprintf("%s, batch first, %s", resolution, order.name)
				if resolveFirst {
					name = strings.Replace(name, "batch first", "resolve first", 1)
				}
				t.Run(name, func(t *testing.T) {
					s := fmt.Sprintf("ubl-gr%d", n)
					ws := []string{s}
					a, b := ubSeed(t, pool, s, "a"), ubSeed(t, pool, s, "b")
					ublFlag(t, pool, a, b)
					batch := ublBatch(ublList(order.reverse, ublContent(a, "a batch", ws...), ublContent(b, "b batch", ws...)))
					archived := resolution == "archive"
					status := "active"
					if archived {
						status = "archived_dup"
					}
					resolved := -1
					if !resolveFirst {
						gate := ublGateRow(t, pool, b)
						defer func() { _ = gate.Rollback(context.Background()) }()
						batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
						ublWaitOn(t, pool, "ubl-batch", "row")
						resolveDone := ublGo(ubActor(t, dsn, "ubl-resolve", nil), ublResolve([]string{b, a}, resolution, ws, &resolved))
						ublWaitOn(t, pool, "ubl-resolve", "advisory")
						ublOpen(t, gate)
						ublThrough(t, "batch", batchDone)
						ublThrough(t, "guard resolve", resolveDone)
						if resolved != 2 {
							t.Errorf("the resolve resolved %d blocks, want both", resolved)
						}
						ublWant(t, pool, a, ublBlock{content: "a batch", scope: s, archived: archived, guardStatus: status})
						ublWant(t, pool, b, ublBlock{content: "b batch", scope: s, archived: archived, guardStatus: status})
						return
					}
					held, hold := testdb.HoldCommitPool(t, dsn, func(sql string) bool { return strings.Contains(sql, "'guard_resolution'") })
					defer hold.Release() // a failing test ends without waiting out the hold
					resolveDone := ublGo(held, ublResolve([]string{b, a}, resolution, ws, &resolved))
					hold.Wait(t)
					batchDone := ublGo(ubActor(t, dsn, "ubl-batch", nil), batch)
					ublWaitOn(t, pool, "ubl-batch", "advisory")
					hold.Release()
					ublThrough(t, "guard resolve", resolveDone)
					if resolved != 2 {
						t.Errorf("the resolve resolved %d blocks, want both", resolved)
					}
					content := "%s batch"
					if archived {
						ublRefused(t, "batch", batchDone)
						content = "%s body"
					} else {
						ublThrough(t, "batch", batchDone)
					}
					ublWant(t, pool, a, ublBlock{content: fmt.Sprintf(content, "a"), scope: s, archived: archived, guardStatus: status})
					ublWant(t, pool, b, ublBlock{content: fmt.Sprintf(content, "b"), scope: s, archived: archived, guardStatus: status})
				})
			}
		}
	}
}

// TestUpsertBlocksUpdateItemsAgainstADreamBatch: a batch of update items over a
// and b beside a dream link batch of the scope, modelled by its locks (the
// scope's supersedes lock shared, the key-share lock of b, then of a).
//
//   - batch first: a gate holds b; the batch holds the scope lock and a and
//     waits for b. The link batch waits for the scope lock, holding no row.
//   - link batch first: a gate holds a; the link batch holds the scope lock
//     shared and b key-shared and waits for a. The batch waits for the scope
//     lock, holding no row.
//
// The link batch's key-share locks do not conflict with the batch's row locks
// (FOR NO KEY UPDATE, the lock of the UPDATE itself), so the two meet only at
// the scope lock. Opened, both go through and neither ends in 40P01.
func TestUpsertBlocksUpdateItemsAgainstADreamBatch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	n := 0
	for _, linksFirst := range []bool{false, true} {
		for _, order := range ublOrders {
			n++
			name := "batch first, " + order.name
			if linksFirst {
				name = "link batch first, " + order.name
			}
			t.Run(name, func(t *testing.T) {
				s := fmt.Sprintf("ubl-dr%d", n)
				ws := []string{s}
				a, b := ubSeed(t, pool, s, "a"), ubSeed(t, pool, s, "b")
				batch := ublBatch(ublList(order.reverse, ublContent(a, "a batch", ws...), ublContent(b, "b batch", ws...)))
				parties := []struct {
					name  string
					party ublParty
				}{{"ubl-batch", batch}, {"ubl-links", ublLinks(s, b, a)}}
				gated := b
				if linksFirst {
					parties[0], parties[1] = parties[1], parties[0]
					gated = a
				}
				gate := ublGateRow(t, pool, gated)
				defer func() { _ = gate.Rollback(context.Background()) }()
				done := make([]<-chan error, len(parties))
				// The first party waits for the gate's row, the second for
				// the scope lock the first holds.
				for i, p := range parties {
					done[i] = ublGo(ubActor(t, dsn, p.name, nil), p.party)
					ublWaitOn(t, pool, p.name, []string{"row", "advisory"}[i])
				}
				ublOpen(t, gate)
				for i, p := range parties {
					ublThrough(t, p.name, done[i])
				}
				ublWant(t, pool, a, ublBlock{content: "a batch", scope: s})
				ublWant(t, pool, b, ublBlock{content: "b batch", scope: s})
			})
		}
	}
}

// TestUpsertBlocksUpdateItemsScopeMoveAgainstABatchOfBothScopes: a batch that
// moves a from s1 to s2 and rewrites b in s2, beside a batch that rewrites b
// and a, listed s2 first. A gate holds b, the block each batch locks last.
//
//   - move first: the move batch holds both scope locks and a and waits for
//     b; the other batch waits for the s1 lock. Opened, the move commits; the
//     other batch's lock pass misses a in s1, the scope it read, and it runs
//     again with a in s2.
//   - other first: the other batch holds both scope locks and a and waits for
//     b; the move batch waits for the s1 lock. Opened, it writes, then the
//     move.
//
// Neither ends in 40P01; a ends in s2, both blocks carry the later batch's
// content.
func TestUpsertBlocksUpdateItemsScopeMoveAgainstABatchOfBothScopes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	n := 0
	for _, moveFirst := range []bool{true, false} {
		for _, order := range ublOrders {
			n++
			name := "move first, move " + order.name
			if !moveFirst {
				name = "other first, move " + order.name
			}
			t.Run(name, func(t *testing.T) {
				s1, s2 := fmt.Sprintf("ubl-mv%d-s1", n), fmt.Sprintf("ubl-mv%d-s2", n)
				ws := []string{s1, s2}
				a, b := ubSeed(t, pool, s1, "a"), ubSeed(t, pool, s2, "b")
				moveBatch := ublBatch(ublList(order.reverse,
					ubuUpdate(a, store.UpdateBlockData{Content: ubuPtr("a moved"), Scope: ubuPtr(s2)}, ws...),
					ublContent(b, "b by the move batch", ws...)))
				otherBatch := ublBatch([]store.BatchUpsert{
					ublContent(b, "b by the other batch", ws...),
					ublContent(a, "a by the other batch", ws...),
				})
				parties := []struct {
					name  string
					party ublParty
				}{{"ubl-move", moveBatch}, {"ubl-other", otherBatch}}
				if !moveFirst {
					parties[0], parties[1] = parties[1], parties[0]
				}
				gate := ublGateRow(t, pool, b)
				defer func() { _ = gate.Rollback(context.Background()) }()
				done := make([]<-chan error, len(parties))
				// The first batch waits for the gate's row, the second for the
				// s1 lock the first holds.
				for i, p := range parties {
					done[i] = ublGo(ubActor(t, dsn, p.name, nil), p.party)
					ublWaitOn(t, pool, p.name, []string{"row", "advisory"}[i])
				}
				ublOpen(t, gate)
				for i, p := range parties {
					ublThrough(t, p.name, done[i])
				}
				if moveFirst {
					ublWant(t, pool, a, ublBlock{content: "a by the other batch", scope: s2})
					ublWant(t, pool, b, ublBlock{content: "b by the other batch", scope: s2})
				} else {
					ublWant(t, pool, a, ublBlock{content: "a moved", scope: s2})
					ublWant(t, pool, b, ublBlock{content: "b by the move batch", scope: s2})
				}
			})
		}
	}
}

// ublCountRounds is how many rounds the count runs each pair in each mode.
const ublCountRounds = 200

// ublRoundBound bounds one round of the count: a round still running then
// is a hang, and its parties' context ends it (counted as another error).
const ublRoundBound = 30 * time.Second

// ublTracer is the tracer of one party's connection in the count: it counts
// the statements that ended in 40P01, and right before each COMMIT it reads
// from a third connection (monitor) whether the other party's backend (other,
// by application_name) waits for a lock, and on what — a supersedes scope
// lock (advisory) or a row: the moment this party holds every lock it took,
// so a wait seen then is a wait for this party's locks or for a lock the two
// wait for together. The read delays the COMMIT by one statement.
type ublTracer struct {
	monitor            *pgxpool.Pool
	other              string
	deadlocks          atomic.Int64
	waitScope, waitRow atomic.Int64
}

func (tr *ublTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, d pgx.TraceQueryStartData) context.Context {
	if !strings.EqualFold(strings.TrimSpace(d.SQL), "commit") {
		return ctx
	}
	var event string
	if err := tr.monitor.QueryRow(context.Background(),
		`SELECT COALESCE(max(wait_event), '') FROM pg_stat_activity
		  WHERE application_name = $1 AND datname = current_database() AND wait_event_type = 'Lock'`, tr.other).Scan(&event); err != nil {
		return ctx
	}
	switch event {
	case "":
	case "advisory":
		tr.waitScope.Add(1)
	default:
		tr.waitRow.Add(1)
	}
	return ctx
}

func (tr *ublTracer) TraceQueryEnd(_ context.Context, _ *pgx.Conn, e pgx.TraceQueryEndData) {
	if ubIsDeadlock(e.Err) {
		tr.deadlocks.Add(1)
	}
}

// ublRound is one round of the count: two fresh blocks, a in s1 and b in s1
// (in s2 for a pair across two scopes), and the scopes every party writes.
type ublRound struct {
	r      int
	a, b   string
	s1, s2 string
	ws     []string
}

// ublCountPair is a pair of the count: party A — the two content updates of a
// and b, as one batch of update items (head) or as two UpdateBlock calls one
// after the other (basis, what a client without update items sends) — and
// its opponent, party B.
type ublCountPair struct {
	name      string
	twoScopes bool
	// flagged marks both blocks needs_review, the input of a guard resolve.
	flagged bool
	// refuses: party A may be refused with ErrBlockNotFound, because party B
	// archived a block before A locked it.
	refuses  bool
	opponent func(rd ublRound, head bool) ublParty
	// contended, if set, names the block both parties write a content to:
	// after a round in which both went through it holds the content of one of
	// them (never the seed's), and which one tells how the round ran.
	contended func(rd ublRound) string
}

// ublCounted is what one pair and mode of the count measured besides the
// parties' outcomes: the time, and in how many rounds the contended block
// ended with party B's content (-1 for a pair without one).
type ublCounted struct {
	took  time.Duration
	bLast int
}

// ublTwoUpdates is the content update of each of ids by who, as one batch of
// update items (head) or as one UpdateBlock call after the other (basis); the
// basis stops at the first refusal, as a client would.
func ublTwoUpdates(rd ublRound, who string, head bool, ids ...string) ublParty {
	content := func(id string) string { return fmt.Sprintf("%s by %s in round %d", id, who, rd.r) }
	if head {
		items := make([]store.BatchUpsert, len(ids))
		for i, id := range ids {
			items[i] = ublContent(id, content(id), rd.ws...)
		}
		return ublBatch(items)
	}
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, id := range ids {
			if _, _, err := store.UpdateBlock(ctx, pool, id, store.UpdateBlockData{Content: ubuPtr(content(id))}, rd.ws); err != nil {
				return err
			}
		}
		return nil
	}
}

// ublAlternate is x, y on even rounds of the pair (r/step even) and y, x on odd ones.
func ublAlternate(r, step int, x, y string) []string {
	if (r/step)%2 == 1 {
		return []string{y, x}
	}
	return []string{x, y}
}

// ublSide is one party's outcomes over the rounds of a pair and mode, and in
// how many rounds it was seen waiting, at the other party's COMMIT, for a
// scope lock (waitScope) or a row (waitRow).
type ublSide struct {
	deadlocks, refused, other int
	waitScope, waitRow        int
	deadRounds                []int
	firstOther                string
}

// waited counts the round's waits the other party's tracer saw (scope, row:
// its counters moved in the round).
func (s *ublSide) waited(scope, row bool) {
	if scope {
		s.waitScope++
	}
	if row {
		s.waitRow++
	}
}

// add counts one round: 40P01 in the answer or at any statement (the
// tracer's count for the round), a refusal party A may meet, or another error.
func (s *ublSide) add(r int, err error, deadlocked bool, refuses bool) {
	switch {
	case deadlocked || ubIsDeadlock(err):
		s.deadlocks++
		s.deadRounds = append(s.deadRounds, r)
	case err == nil:
	case refuses && errors.Is(err, store.ErrBlockNotFound):
		s.refused++
	default:
		s.other++
		if s.firstOther == "" {
			s.firstOther = fmt.Sprintf("round %d: %v", r, err)
		}
	}
}

// ublCountOffsets are the delays of party B behind party A's start, one per
// round in turn. Started at the same instant, one party of a pair can win the
// race in nearly every round (a single update or an archive needs fewer
// statements than the batch's lock pass before it holds its lock); the
// offsets move B's start across A's lock pass, so the count meets the
// interleavings in which A holds rows while B waits as well.
var ublCountOffsets = []time.Duration{0, 100 * time.Microsecond, 300 * time.Microsecond, 600 * time.Microsecond, time.Millisecond}

// ublRace runs both parties of a round, released by one barrier, party B
// after delayB, each on its own connection, and returns their answers.
func ublRace(poolA, poolB *pgxpool.Pool, partyA, partyB ublParty, delayB time.Duration) (errA, errB error) {
	ctx, cancel := context.WithTimeout(context.Background(), ublRoundBound)
	defer cancel()
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		errA = partyA(ctx, poolA)
	}()
	go func() {
		defer wg.Done()
		<-start
		if delayB > 0 {
			time.Sleep(delayB)
		}
		errB = partyB(ctx, poolB)
	}()
	close(start)
	wg.Wait()
	return errA, errB
}

// ublCount runs one pair in one mode over ublCountRounds rounds, each on two
// fresh blocks, and returns both parties' outcomes and what it measured.
func ublCount(t *testing.T, pool *pgxpool.Pool, dsn string, pi int, pair ublCountPair, head bool) (a, b ublSide, c ublCounted) {
	t.Helper()
	mode := "basis"
	if head {
		mode = "head"
	}
	trA := &ublTracer{monitor: pool, other: "ubl-count-b"}
	trB := &ublTracer{monitor: pool, other: "ubl-count-a"}
	poolA := ubActor(t, dsn, "ubl-count-a", trA)
	poolB := ubActor(t, dsn, "ubl-count-b", trB)
	s1, s2 := fmt.Sprintf("ubl-n%d-%s-s1", pi, mode), fmt.Sprintf("ubl-n%d-%s-s2", pi, mode)
	if pair.contended == nil {
		c.bLast = -1
	}
	start := time.Now()
	for r := 0; r < ublCountRounds; r++ {
		rd := ublRound{r: r, s1: s1, s2: s2, ws: []string{s1, s2}}
		bScope := s1
		if pair.twoScopes {
			bScope = s2
		}
		// Ids are uuidv7, in the order of their writes: a, b written in that
		// order or the other by turns, so the id order — the order a guard
		// resolve and an archive lock rows in — runs with the batch's
		// identity order (a before b) and against it.
		if (r/4)%2 == 0 {
			rd.a = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-a", r))
			rd.b = ubSeed(t, pool, bScope, fmt.Sprintf("r%03d-b", r))
		} else {
			rd.b = ubSeed(t, pool, bScope, fmt.Sprintf("r%03d-b", r))
			rd.a = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-a", r))
		}
		if pair.flagged {
			ublFlag(t, pool, rd.a, rd.b)
		}
		deadA, scopeA, rowA := trA.deadlocks.Load(), trA.waitScope.Load(), trA.waitRow.Load()
		deadB, scopeB, rowB := trB.deadlocks.Load(), trB.waitScope.Load(), trB.waitRow.Load()
		errA, errB := ublRace(poolA, poolB, ublTwoUpdates(rd, "A", head, ublAlternate(r, 1, rd.a, rd.b)...), pair.opponent(rd, head),
			ublCountOffsets[r%len(ublCountOffsets)])
		a.add(r, errA, trA.deadlocks.Load() > deadA, pair.refuses)
		b.add(r, errB, trB.deadlocks.Load() > deadB, false)
		b.waited(trA.waitScope.Load() > scopeA, trA.waitRow.Load() > rowA)
		a.waited(trB.waitScope.Load() > scopeB, trB.waitRow.Load() > rowB)
		if pair.contended == nil || errA != nil || errB != nil {
			continue
		}
		var content string
		id := pair.contended(rd)
		if err := pool.QueryRow(context.Background(), `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&content); err != nil {
			t.Fatalf("round %d: read %s: %v", r, id, err)
		}
		switch {
		case strings.Contains(content, "by B in round"):
			c.bLast++
		case !strings.Contains(content, "by A in round"):
			t.Errorf("%s, round %d: both parties went through, yet %s holds %q, neither party's content", mode, r, id, content)
		}
	}
	c.took = time.Since(start)
	return a, b, c
}

// TestUpsertBlocksUpdateItemsDeadlockCount counts 40P01 over 200 rounds of each
// pair on fresh blocks (written a, b or b, a by turns, so their id order runs
// with and against the batch's identity order), both parties released by one
// barrier, party B after an offset of 0 to 1 ms in turn (ublCountOffsets).
//
// Party A is the two content updates of a and b: in the head mode one batch
// of update items, in the basis mode two UpdateBlock calls one after the
// other (list order a, b and b, a by turns). Party B is the pair's opponent:
// a second batch of the same blocks in the opposite order (the basis: two
// single updates in that order), a single update of a or b in place and with
// a scope move, an archive of a or b, a guard resolve batch (keep, archive)
// and a dream link batch by its locks.
//
// A 40P01 is counted at the statement (a tracer on each party's connection)
// and in the answer. Where both parties write a content to one block, the
// block must hold one of the two after a round both went through, and the
// count of rounds that party B wrote last shows that the parties met in both
// orders (for an archive, the refusals of party A show it); right before each
// COMMIT a third connection reads whether the other party waits for a lock, a
// scope lock or a row (the "waits s/r" columns: rounds in which the party was
// seen waiting so). Asserted: no 40P01 and no unexpected error in the head
// mode, for either party of any pair, and no lost content in either mode. The
// basis is the comparison value and only reported.
func TestUpsertBlocksUpdateItemsDeadlockCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	pick := func(rd ublRound) string { return ublAlternate(rd.r, 2, rd.a, rd.b)[0] }
	pairs := []ublCountPair{
		{name: "a second batch in the opposite order", twoScopes: true, opponent: func(rd ublRound, head bool) ublParty {
			return ublTwoUpdates(rd, "B", head, ublAlternate(rd.r, 1, rd.b, rd.a)...)
		}, contended: func(rd ublRound) string { return rd.a }},
		{name: "a single update", opponent: func(rd ublRound, _ bool) ublParty {
			return ublUpdate(pick(rd), store.UpdateBlockData{Content: ubuPtr(fmt.Sprintf("by B in round %d", rd.r))}, rd.ws)
		}, contended: pick},
		{name: "a single update with a scope move", opponent: func(rd ublRound, _ bool) ublParty {
			return ublUpdate(pick(rd), store.UpdateBlockData{Content: ubuPtr(fmt.Sprintf("by B in round %d", rd.r)), Scope: ubuPtr(rd.s2)}, rd.ws)
		}, contended: pick},
		{name: "an archive", refuses: true, opponent: func(rd ublRound, _ bool) ublParty {
			return ublDelete(pick(rd), rd.ws)
		}},
		{name: "a guard resolve batch, keep", flagged: true, opponent: func(rd ublRound, _ bool) ublParty {
			return ublResolve(ublAlternate(rd.r, 2, rd.a, rd.b), "keep", rd.ws, nil)
		}},
		{name: "a guard resolve batch, archive", flagged: true, refuses: true, opponent: func(rd ublRound, _ bool) ublParty {
			return ublResolve(ublAlternate(rd.r, 2, rd.a, rd.b), "archive", rd.ws, nil)
		}},
		{name: "a dream link batch", opponent: func(rd ublRound, _ bool) ublParty {
			return ublLinks(rd.s1, rd.b, rd.a)
		}},
	}
	table := []string{fmt.Sprintf("%-38s %-5s %6s %9s %9s %10s %12s %7s %13s %13s %9s",
		"pair", "mode", "rounds", "40P01 A", "40P01 B", "refused A", "other errors", "B last",
		"A waits s/r", "B waits s/r", "time")}
	for pi, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			for _, head := range []bool{false, true} {
				mode := "basis"
				if head {
					mode = "head"
				}
				a, b, c := ublCount(t, pool, dsn, pi, pair, head)
				bLast := "-"
				if c.bLast >= 0 {
					bLast = fmt.Sprint(c.bLast)
				}
				row := fmt.Sprintf("%-38s %-5s %6d %9d %9d %10d %12d %7s %13s %13s %9s", pair.name, mode, ublCountRounds,
					a.deadlocks, b.deadlocks, a.refused, a.other+b.other, bLast,
					fmt.Sprintf("%d/%d", a.waitScope, a.waitRow), fmt.Sprintf("%d/%d", b.waitScope, b.waitRow),
					c.took.Round(time.Millisecond))
				table = append(table, row)
				t.Log(row)
				for _, s := range []struct {
					who  string
					side ublSide
				}{{"A", a}, {"B", b}} {
					if s.side.firstOther != "" {
						t.Logf("%s, party %s, first other error: %s", mode, s.who, s.side.firstOther)
					}
					if !head {
						continue
					}
					if s.side.deadlocks > 0 {
						t.Errorf("head: party %s ended in 40P01 in %d of %d rounds (rounds %v)", s.who, s.side.deadlocks, ublCountRounds, s.side.deadRounds)
					}
					if s.side.other > 0 {
						t.Errorf("head: party %s met %d other errors (%s)", s.who, s.side.other, s.side.firstOther)
					}
				}
			}
		})
	}
	t.Logf("40P01 count, %d rounds per pair and mode:\n%s", ublCountRounds, strings.Join(table, "\n"))
}
