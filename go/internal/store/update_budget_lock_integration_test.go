//go:build integration

// Issue #519, review of PR #553: a single update took its key's budget lock
// first and then waited for its row, so an update waiting for a row another
// transaction held kept every other write of the key waiting. The budget lock
// is now the LAST lock of every writer that takes it — a single update after
// its row and no-op test, a batch after its scope and row locks — and its
// holder writes only rows it holds. The count below runs the budgeted writers
// of ONE key against each other and against an archive, 200 rounds per pair
// on fresh blocks (the harness of TestUpsertBlocksUpdateItemsDeadlockCount):
// no round may end in 40P01, and no write may be lost.
//
//	go test -tags=integration -p 1 ./internal/store/ -run 'TestBudgetedWritersDeadlockCount' -count=1 -v
package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// budgetedUpdates is the content update of each of ids by who, as one booked
// single update after the other.
func budgetedUpdates(rd ublRound, who string, budget *store.WriteBudget, ids ...string) ublParty {
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		for _, id := range ids {
			if _, _, err := store.UpdateBlockBooked(ctx, pool, id, store.UpdateBlockData{Content: ubuPtr(fmt.Sprintf("%s by %s in round %d", id, who, rd.r))}, rd.ws, budget); err != nil {
				return err
			}
		}
		return nil
	}
}

// budgetedBatch is the content update of each of ids by who, as one batch of
// update items on the key's budget.
func budgetedBatch(rd ublRound, who string, budget *store.WriteBudget, ids ...string) ublParty {
	items := make([]store.BatchUpsert, len(ids))
	for i, id := range ids {
		items[i] = ublContent(id, fmt.Sprintf("%s by %s in round %d", id, who, rd.r), rd.ws...)
	}
	return func(ctx context.Context, pool *pgxpool.Pool) error {
		_, err := store.UpsertBlocks(ctx, pool, items, "ubl", nil, budget)
		return err
	}
}

// race3 runs the parties released by one barrier, party i after delays[i], each
// on its own connection, and returns their answers.
func race3(pools []*pgxpool.Pool, parties []ublParty, delays []time.Duration) []error {
	ctx, cancel := context.WithTimeout(context.Background(), ublRoundBound)
	defer cancel()
	start := make(chan struct{})
	errs := make([]error, len(parties))
	var wg sync.WaitGroup
	for i := range parties {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			if delays[i] > 0 {
				time.Sleep(delays[i])
			}
			errs[i] = parties[i](ctx, pools[i])
		}(i)
	}
	close(start)
	wg.Wait()
	return errs
}

func TestBudgetedWritersDeadlockCount(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	key, _, err := store.CreateApiKey(context.Background(), pool, "budget-lock-count", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	// A limit no round reaches: the lock is taken and every write is
	// counted and booked, none refused.
	budget := &store.WriteBudget{APIKeyID: key.ID, Limit: 1 << 30}
	pick := func(rd ublRound) string { return ublAlternate(rd.r, 2, rd.a, rd.b)[0] }
	type party func(rd ublRound) ublParty
	pairs := []struct {
		name    string
		parties []party
	}{
		{"update × update", []party{
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
		}},
		{"update × batch", []party{
			func(rd ublRound) ublParty { return budgetedBatch(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
		}},
		{"batch × batch", []party{
			func(rd ublRound) ublParty { return budgetedBatch(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedBatch(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
		}},
		{"update × update × archive", []party{
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
			func(rd ublRound) ublParty { return ublDelete(pick(rd), rd.ws) },
		}},
		{"update × batch × archive", []party{
			func(rd ublRound) ublParty { return budgetedBatch(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedUpdates(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
			func(rd ublRound) ublParty { return ublDelete(pick(rd), rd.ws) },
		}},
		{"batch × batch × archive", []party{
			func(rd ublRound) ublParty { return budgetedBatch(rd, "A", budget, ublAlternate(rd.r, 1, rd.a, rd.b)...) },
			func(rd ublRound) ublParty { return budgetedBatch(rd, "B", budget, ublAlternate(rd.r, 1, rd.b, rd.a)...) },
			func(rd ublRound) ublParty { return ublDelete(pick(rd), rd.ws) },
		}},
	}
	var table []string
	for pi, pair := range pairs {
		t.Run(pair.name, func(t *testing.T) {
			pools := make([]*pgxpool.Pool, len(pair.parties))
			tracers := make([]*ublTracer, len(pair.parties))
			for i := range pools {
				tracers[i] = &ublTracer{monitor: pool, other: fmt.Sprintf("budget-count-%d", (i+1)%len(pools))}
				pools[i] = ubActor(t, dsn, fmt.Sprintf("budget-count-%d", i), tracers[i])
			}
			s1 := fmt.Sprintf("budget-n%d-s1", pi)
			deadlocks, refused, other := 0, 0, 0
			var first string
			start := time.Now()
			for r := 0; r < ublCountRounds; r++ {
				rd := ublRound{r: r, s1: s1, ws: []string{s1}}
				if (r/4)%2 == 0 {
					rd.a = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-a", r))
					rd.b = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-b", r))
				} else {
					rd.b = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-b", r))
					rd.a = ubSeed(t, pool, s1, fmt.Sprintf("r%03d-a", r))
				}
				before := make([]int64, len(tracers))
				for i, tr := range tracers {
					before[i] = tr.deadlocks.Load()
				}
				parties := make([]ublParty, len(pair.parties))
				delays := make([]time.Duration, len(pair.parties))
				for i, p := range pair.parties {
					parties[i] = p(rd)
					if i > 0 { // party A starts at once, the others after an offset in turn
						delays[i] = ublCountOffsets[(r+2*i)%len(ublCountOffsets)]
					}
				}
				errs := race3(pools, parties, delays)
				for i, err := range errs {
					switch {
					case tracers[i].deadlocks.Load() > before[i] || ubIsDeadlock(err):
						deadlocks++
					case err == nil:
					case errors.Is(err, store.ErrBlockNotFound):
						refused++ // the archive took the block first
					default:
						other++
						if first == "" {
							first = fmt.Sprintf("round %d, party %d: %v", r, i, err)
						}
					}
				}
			}
			row := fmt.Sprintf("%-28s rounds %d  40P01 %d  refused %d  other %d  %v", pair.name, ublCountRounds, deadlocks, refused, other, time.Since(start).Round(time.Millisecond))
			table = append(table, row)
			t.Log(row)
			if deadlocks > 0 || other > 0 {
				t.Errorf("%s: %d × 40P01, %d other errors (%s)", pair.name, deadlocks, other, first)
			}
		})
	}
	t.Logf("budgeted writers of one key, %d rounds per pair:\n%s", ublCountRounds, strings.Join(table, "\n"))
}

// TestBudgetedWritesBookWithTheirCommit (issue #519, review of PR #553): the
// booking of a single update and of a batch is a row of the write's own
// transaction, so a write whose COMMIT fails leaves no booking — a booking
// on the pool beside the transaction would stay. A deferred constraint
// trigger refuses, at the commit, a row that carries the marker content.
func TestBudgetedWritesBookWithTheirCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "budget-commit", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE FUNCTION zz_refuse_at_commit() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN IF NEW.content = 'refused at commit' THEN RAISE EXCEPTION 'refused at commit'; END IF; RETURN NULL; END $$`,
		`CREATE CONSTRAINT TRIGGER zz_refuse_at_commit AFTER UPDATE ON context_blocks
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION zz_refuse_at_commit()`,
	} {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	budget := &store.WriteBudget{APIKeyID: key.ID, Limit: 100}
	scopes := []string{"budget-commit"}
	a, b := ubSeed(t, pool, "budget-commit", "a"), ubSeed(t, pool, "budget-commit", "b")
	bookings := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*)::int FROM context_access_log WHERE api_key_id = $1 AND action = 'write'`, key.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	refused := store.UpdateBlockData{Content: ubuPtr("refused at commit")}
	if _, _, err := store.UpdateBlockBooked(ctx, pool, a, refused, scopes, budget); err == nil || !strings.Contains(err.Error(), "refused at commit") {
		t.Fatalf("single update: %v, want the commit's refusal", err)
	}
	if _, err := store.UpsertBlocks(ctx, pool, []store.BatchUpsert{ubuUpdate(b, refused, scopes...)}, "ubl", nil, budget); err == nil || !strings.Contains(err.Error(), "refused at commit") {
		t.Fatalf("batch: %v, want the commit's refusal", err)
	}
	if n := bookings(); n != 0 {
		t.Errorf("%d bookings after two writes whose commit failed, want 0", n)
	}
	if _, _, err := store.UpdateBlockBooked(ctx, pool, a, store.UpdateBlockData{Content: ubuPtr("written")}, scopes, budget); err != nil {
		t.Fatal(err)
	}
	if _, err := store.UpsertBlocks(ctx, pool, []store.BatchUpsert{ubuUpdate(b, store.UpdateBlockData{Content: ubuPtr("written")}, scopes...)}, "ubl", nil, budget); err != nil {
		t.Fatal(err)
	}
	if n := bookings(); n != 2 {
		t.Errorf("%d bookings after two committed writes, want 2", n)
	}
}
