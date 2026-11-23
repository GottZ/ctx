//go:build integration

// Issue #491 (review of the first round, F4): the push section holds no
// transaction across its wire write.
//
// The section serialises the push legs of several ctxd instances on one
// entity with an advisory lock held across the PATCH. In its first form the
// lock was transaction-level, so the transaction stayed open while the forge
// answered: "idle in transaction" with a snapshot (backend_xmin, the vacuum
// horizon of the whole database) and an AccessShareLock on
// context_project_sync_map, for up to the forge client's timeout — an ALTER
// TABLE on the table queued behind it, and every later access of the table
// behind the ALTER. The lock is now session-level on a connection the section
// holds out of the pool with no transaction open; the advance runs in a short
// transaction on that connection after the wire write.
//
// A session-level lock outlives a way out that forgets it, until the session
// ends — and a pooled connection does not end. So every way out of the section
// gives the lock back, and a connection whose release is not proven is closed.
//
//	go test -tags=integration ./internal/forge/ -run 'TestPushSectionHoldsNoTransaction|TestPushSectionGivesItsLockBack' -count=1 -v
package forge

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// i491Observer is a connection of its own for looking at the push's session.
func (e i491Env) i491Observer(t *testing.T) *pgx.Conn {
	t.Helper()
	ctx := context.Background()
	obs, err := pgx.Connect(ctx, e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close(context.Background()) })
	return obs
}

// i491PushLocks counts the advisory locks granted in the database.
func i491PushLocks(t *testing.T, obs *pgx.Conn) int {
	t.Helper()
	var n int
	if err := obs.QueryRow(context.Background(),
		`SELECT count(*) FROM pg_locks WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestPushSectionHoldsNoTransactionAcrossTheWireWrite(t *testing.T) {
	e := i491Setup(t, "i491notx", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	obs := e.i491Observer(t)
	ctx := context.Background()
	var state string
	var xmin *string
	var relLocks int
	var ddlErr error
	e.gh.seam(func() {
		// While the PATCH is on the wire: the session that holds the push lock.
		if err := obs.QueryRow(ctx, `
			SELECT a.state, a.backend_xmin::text,
			       (SELECT count(*) FROM pg_locks l WHERE l.pid = a.pid AND l.locktype = 'relation' AND l.granted)
			  FROM pg_stat_activity a
			  JOIN pg_locks k ON k.pid = a.pid AND k.locktype = 'advisory' AND k.granted
			 WHERE a.datname = current_database()`).Scan(&state, &xmin, &relLocks); err != nil {
			t.Errorf("the session of the push lock: %v", err)
			return
		}
		if _, err := obs.Exec(ctx, `SET lock_timeout = '5s'`); err != nil {
			t.Error(err)
			return
		}
		_, ddlErr = obs.Exec(ctx, `ALTER TABLE context_project_sync_map ALTER COLUMN conflict SET DEFAULT false`)
	}, nil)
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 0 {
		t.Errorf("push: %+v, want one push", r)
	}
	if state != "idle" || xmin != nil || relLocks != 0 {
		t.Errorf("during the PATCH the push lock's session is %q, backend_xmin set %v, %d relation lock(s) — want idle, no snapshot, no relation lock", state, xmin != nil, relLocks)
	}
	if ddlErr != nil {
		t.Errorf("an ALTER TABLE on context_project_sync_map during the PATCH: %v, want it through", ddlErr)
	}
	if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
		t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
	}
	if n := i491PushLocks(t, obs); n != 0 {
		t.Errorf("%d advisory lock(s) still held after the push", n)
	}
}

// failingPatch is the production client whose issue PATCH fails before it
// reaches the forge.
type failingPatch struct{ Forge }

var errI491Wire = errors.New("the forge answered 502")

func (failingPatch) UpdateIssue(context.Context, RepoRef, int64, IssuePatch) (*time.Time, error) {
	return nil, errI491Wire
}

func TestPushSectionGivesItsLockBackOnEveryWayOut(t *testing.T) {
	var checkPushed func(t *testing.T, e i491Env, obs *pgx.Conn, want int)
	// otherInstance pushes from a pool of its own: a connection of the first
	// pool that kept the lock would let only that pool's sessions in (a
	// session-level advisory lock is re-entrant for its own session).
	otherInstance := func(t *testing.T, e i491Env) PushResult {
		t.Helper()
		pool, err := pgxpool.New(context.Background(), e.dsn)
		if err != nil {
			t.Fatal(err)
		}
		defer pool.Close()
		return e.pushOn(t, pool)
	}
	check := func(t *testing.T, e i491Env, obs *pgx.Conn) {
		t.Helper()
		checkPushed(t, e, obs, 1)
	}
	checkPushed = func(t *testing.T, e i491Env, obs *pgx.Conn, want int) {
		t.Helper()
		if n := i491PushLocks(t, obs); n != 0 {
			t.Errorf("%d advisory lock(s) held after the section ended", n)
		}
		if r := otherInstance(t, e); r.Pushed != want || r.Conflicts != 0 {
			t.Errorf("another instance's push after the section ended: %+v, want %d pushed", r, want)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
		}
	}

	t.Run("the PATCH fails", func(t *testing.T) {
		e := i491Setup(t, "i491lockwire", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		obs := e.i491Observer(t)
		_, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, failingPatch{e.gh.client()}, e.gh.repo(), alwaysAllow)
		if !errors.Is(err, errI491Wire) {
			t.Fatalf("push: %v, want the wire error", err)
		}
		check(t, e, obs)
	})

	// A cancellation of the run while the PATCH is on the wire does not cut
	// the PATCH (issue #474: the run's deadline reaches a section up to its
	// lock): the PATCH lands and is recorded, the lock goes back, and the
	// pass ends at its next step with the cancellation — another instance
	// finds nothing left to push.
	t.Run("the run is canceled during the PATCH", func(t *testing.T) {
		e := i491Setup(t, "i491lockcancel", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		obs := e.i491Observer(t)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		e.gh.seam(cancel, nil)
		if _, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow); !errors.Is(err, context.Canceled) {
			t.Fatalf("push: %v, want the cancellation", err)
		}
		checkPushed(t, e, obs, 0)
	})

	t.Run("the bucket runs dry inside the section", func(t *testing.T) {
		e := i491Setup(t, "i491lockdry", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		obs := e.i491Observer(t)
		res, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), func() bool { return false })
		if err != nil || !res.Throttled || res.Pushed != 0 {
			t.Fatalf("push with a dry bucket: %+v %v, want throttled, nothing pushed", res, err)
		}
		check(t, e, obs)
	})

	// The database ends the push's session while the PATCH is on the wire:
	// the record fails, the unlock cannot answer, and the connection is
	// closed instead of going back into the pool.
	t.Run("the session ends during the PATCH", func(t *testing.T) {
		e := i491Setup(t, "i491lockterm", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		obs := e.i491Observer(t)
		e.gh.seam(func() {
			if _, err := obs.Exec(context.Background(), `
				SELECT pg_terminate_backend(pid) FROM pg_locks
				 WHERE locktype = 'advisory' AND granted AND database = (SELECT oid FROM pg_database WHERE datname = current_database())`); err != nil {
				t.Error(err)
			}
		}, nil)
		if _, err := e.pushOnErr(e.pool); err == nil {
			t.Fatalf("push: no error after its session ended")
		}
		check(t, e, obs)
	})
}

// pushOnErr is pushOn that hands the error back.
func (e i491Env) pushOnErr(pool *pgxpool.Pool) (PushResult, error) {
	return NewPusher(pool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
}
