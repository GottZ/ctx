//go:build integration

// Issue #491 point 2: a pull between the two advances of a push race.
//
// Instance A lists a local version and PATCHes it; while A's PATCH is on the
// wire, a local edit makes a newer version, instance B lists, PATCHes and
// advances it, and A's PATCH lands last — the forge holds A's older version.
// A pull of a third instance that runs before A advances finds the block at
// B's base and the forge ahead of it, and writes A's older version over the
// newer local one; A's advance then sees its base moved and flags the
// mapping, and the next pull converges on A's version. The newer local
// version is gone, on both sides, without a conflict.
//
// Points 1 and 3 do not close this: the pull decides on the locked rows,
// and they say exactly "forge-ahead". What makes it a loss is a second push
// writing the same entity while the first one's PATCH is unrecorded. The
// push now writes an entity, and records the write, inside a transaction
// that holds the entity's advisory lock; another instance that finds the
// lock taken leaves the entity to its next run instead of waiting.
//
// The other instances run inside the fake forge's PATCH of A (fakegithub
// seam): B's push while A's PATCH has not landed, the third instance's pull
// once it landed and before A's advance.
//
//	go test -tags=integration ./internal/forge/ -run TestPushRace -count=1 -v
package forge

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// i491StatementHold holds the first statement its pool sends for which match
// answers true, before it goes out, until release is closed.
type i491StatementHold struct {
	match   func(sql string) bool
	mu      sync.Mutex
	used    bool
	entered chan struct{}
	release chan struct{}
}

func (h *i491StatementHold) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if !h.match(data.SQL) {
		return ctx
	}
	h.mu.Lock()
	first := !h.used
	h.used = true
	h.mu.Unlock()
	if first {
		close(h.entered)
		select {
		case <-h.release:
		case <-time.After(i452Wait):
		}
	}
	return ctx
}

func (h *i491StatementHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func i491HoldStatementPool(t *testing.T, dsn string, match func(sql string) bool) (*pgxpool.Pool, *i491StatementHold) {
	t.Helper()
	h := &i491StatementHold{match: match, entered: make(chan struct{}), release: make(chan struct{})}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = h
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		select {
		case <-h.release:
		default:
			close(h.release)
		}
		pool.Close()
	})
	return pool, h
}

// i491TitleSent is the title an issue PATCH carried ("" when none).
func i491TitleSent(p fakePatch) string {
	var s string
	if raw, ok := p.Fields["title"]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func TestPushRaceKeepsTheNewerLocalVersion(t *testing.T) {
	e := i491Setup(t, "i491race", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})

	var bRes PushResult
	var pullRes ApplyResult
	e.gh.seam(func() {
		// A's PATCH is on the wire: a newer local version, and instance B's
		// push of it.
		e.edit(t, store.IssueUpdate{Title: str("Crane, B")})
		bRes = e.pushOn(t, e.pool)
	}, func() {
		// A's PATCH landed: a third instance's pull before A advances.
		pullRes = e.pullOn(t, e.pool)
	})
	aRes := e.pushOn(t, e.pool)
	t.Logf("A: %+v, B: %+v, pull between: %+v", aRes, bRes, pullRes)

	if b := getBlock(t, e.pool, e.proj.Scope, e.id(t)); b.Title != "#5 Crane, B" {
		t.Fatalf("the block holds %q after the race, want the newer local version %q kept", b.Title, "#5 Crane, B")
	}
	// Whatever the runs decide afterwards, the newer local version is never
	// replaced by the older one: it stays in the block (a flagged conflict
	// leaves it to the user) or reaches the forge.
	e.cycles(t, 3)
	b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
	it := e.gh.issue(t, 5)
	_, conflict := e.mapping(t)
	if b.Title != "#5 Crane, B" {
		t.Errorf("after three runs the block holds %q, want %q", b.Title, "#5 Crane, B")
	}
	if it.Title != "Crane, B" && !conflict {
		t.Errorf("after three runs the forge holds %q and the mapping is not flagged: the newer local version is neither on the forge nor left to the user", it.Title)
	}
}

// TestPushSectionLeavesAnEntityInFlightToTheNextRun: while A's PATCH of an
// entity is on the wire, instance B's push of the same entity writes
// nothing — no PATCH, no advance, no conflict — and A records its write as
// the base, unflagged. The newer local version goes out with the next run.
func TestPushSectionLeavesAnEntityInFlightToTheNextRun(t *testing.T) {
	t.Run("issue", func(t *testing.T) {
		e := i491Setup(t, "i491inflight", "", fakeIssue{Title: "Crane", Body: "forge body"})
		e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
		var bRes PushResult
		e.gh.seam(func() {
			e.edit(t, store.IssueUpdate{Title: str("Crane, B")})
			bRes = e.pushOn(t, e.pool)
		}, nil)
		aRes := e.pushOn(t, e.pool)
		if bRes.Pushed != 0 || bRes.Conflicts != 0 {
			t.Errorf("instance B during A's PATCH: %+v, want nothing pushed and no conflict", bRes)
		}
		if aRes.Pushed != 1 || aRes.Conflicts != 0 {
			t.Errorf("instance A: %+v, want one push and no conflict", aRes)
		}
		if got := e.gh.landed(); len(got) != 1 || i491TitleSent(got[0]) != "Crane, A" {
			t.Fatalf("PATCHes landed: %+v, want A's alone", got)
		}
		if _, conflict := e.mapping(t); conflict {
			t.Fatalf("the mapping is flagged after A's advance")
		}
		if n := e.cycles(t, 1); n != 1 || e.gh.issue(t, 5).Title != "Crane, B" {
			t.Fatalf("the next run landed %d PATCH(es), forge title %q — want the newer local version pushed", n, e.gh.issue(t, 5).Title)
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three more runs landed %d PATCH(es), want 0", n)
		}
		if base, conflict := e.mapping(t); conflict || base != e.forgeH(t) {
			t.Errorf("mapping flagged %v, base is the forge's %v", conflict, base == e.forgeH(t))
		}
	})
	t.Run("comment", func(t *testing.T) {
		pool := testdb.SetupTestDB(t)
		ctx := context.Background()
		proj := enablePush(seedIFProject(t, pool, "i491inflightc"))
		_, commentID := i452Pulled(t, pool, proj, true)
		i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment, A")})
		var bRes PushResult
		bSpy := newPushSpy()
		aSpy := &hookSpy{pushSpyForge: newPushSpy()}
		aSpy.setHook(func() {
			i452Edit(t, ctx, pool, proj.Scope, commentID, store.IssueUpdate{Content: str("comment, B")})
			var err error
			if bRes, err = pusherWithRegistry(pool).PushProject(ctx, proj, bSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil {
				t.Errorf("instance B: %v", err)
			}
		})
		aRes, err := pusherWithRegistry(pool).PushProject(ctx, proj, aSpy, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow)
		if err != nil || aRes.Pushed != 1 || aRes.Conflicts != 0 {
			t.Errorf("instance A: %+v %v, want one push and no conflict", aRes, err)
		}
		if bRes.Pushed != 0 || bRes.Conflicts != 0 || bSpy.wireCalls() != 0 {
			t.Errorf("instance B during A's PATCH: %+v, %d wire writes — want none", bRes, bSpy.wireCalls())
		}
		if _, _, conflict := mapByBlock(t, pool, commentID); conflict {
			t.Fatalf("the mapping is flagged after A's advance")
		}
		next := newPushSpy()
		if res, err := pusherWithRegistry(pool).PushProject(ctx, proj, next, RepoRef{Owner: "o", Repo: "r"}, alwaysAllow); err != nil || res.Pushed != 1 ||
			len(next.commentPatches) != 1 || next.commentPatches[0].Body != "comment, B" {
			t.Fatalf("next run: %+v %v, comment patches %+v — want the newer local version pushed", res, err, next.commentPatches)
		}
		i452NoCandidates(t, pool, proj)
	})
}

// TestPushSectionSkipsAListingAnotherPushOvertook: instance B lists the
// entity while A's PATCH is on the wire and opens its section once A
// recorded its write. B's listing holds a version and a base that A's
// advance replaced: B writes nothing (a PATCH of its stale field-diff and an
// advance off the moved base would flag a conflict the user had to resolve),
// and the next run pushes the newer local version against A's base.
func TestPushSectionSkipsAListingAnotherPushOvertook(t *testing.T) {
	e := i491Setup(t, "i491overtook", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	held, hold := i452HoldBeginPool(t, e.dsn)
	type out struct {
		res PushResult
		err error
	}
	bOut := make(chan out, 1)
	e.gh.seam(func() {
		e.edit(t, store.IssueUpdate{Content: str("body edited locally during A's PATCH")})
		go func() {
			res, err := NewPusher(held, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
			bOut <- out{res, err}
		}()
		select {
		case <-hold.entered: // B listed and waits at its section's BEGIN
		case o := <-bOut:
			t.Errorf("instance B finished without opening a section: %+v %v", o.res, o.err)
		case <-time.After(i452Wait):
			t.Errorf("instance B did not reach its section within %s", i452Wait)
		}
	}, nil)
	if aRes := e.pushOn(t, e.pool); aRes.Pushed != 1 || aRes.Conflicts != 0 {
		t.Errorf("instance A: %+v, want one push and no conflict", aRes)
	}
	close(hold.release)
	select {
	case o := <-bOut:
		if o.err != nil || o.res.Pushed != 0 || o.res.Conflicts != 0 {
			t.Errorf("instance B after A's advance: %+v %v, want nothing pushed and no conflict", o.res, o.err)
		}
	case <-time.After(i452Wait):
		t.Fatalf("instance B did not finish within %s", i452Wait)
	}
	if got := e.gh.landed(); len(got) != 1 {
		t.Fatalf("PATCHes landed: %+v, want A's alone", got)
	}
	if _, conflict := e.mapping(t); conflict {
		t.Fatalf("the mapping is flagged")
	}
	landed := len(e.gh.landed())
	if n := e.cycles(t, 1); n != 1 {
		t.Fatalf("the next run landed %d PATCH(es), want 1", n)
	}
	p := e.gh.landed()[landed]
	if _, ok := p.Fields["title"]; ok || !strings.Contains(string(p.Fields["body"]), "edited locally") {
		t.Errorf("the next run's PATCH carries %v, want the body alone (the title is A's base)", p.Fields)
	}
	if n := e.cycles(t, 3); n != 0 {
		t.Errorf("three more runs landed %d PATCH(es), want 0", n)
	}
}
