//go:build integration

// Issue #509, the second review of the push's forge stamp: what the push's
// record counts and stamps, and what a flagged mapping's read reports.
//
// The advance counted its conflict inside its transaction, before the
// COMMIT; since a failed leg's counts reach the run's status, a COMMIT that
// failed counted a conflict that was rolled back. It is counted once the
// record committed.
//
// An advance that finds the base moved off both the listed and the sent
// version records the base unknown and keeps the forge stamp the row holds:
// the answer's stamp belongs to the version the PATCH sent, which is not the
// base it records.
//
// A fetch with the stamp a flagged mapping records is the next version of
// one second, not an older one: the read that ends on the conflict reports
// no skip and no WARN for it.
//
//	go test -tags=integration ./internal/forge/ -run 'TestAPushRecordCountsItsConflict|TestAPushAdvanceOffAThirdVersion|TestASameStampFetchOfAFlaggedConflict' -count=1 -v
package forge

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

// i509ThirdVersion arms the next issue PATCH of #5 (A sends the title
// "Crane, A"): while it is on the wire, a local body edit and a remote edit
// bring block and forge to one more version, and another instance's pull
// converges on it — the base moves off both the listed and the sent
// version. after runs once the PATCH landed. It returns the forge stamp of
// the version the pull converged on, filled in by the seam.
func (e i491Env) i509ThirdVersion(t *testing.T, other *pgxpool.Pool, after func()) *time.Time {
	t.Helper()
	const third = "body of a third version, written on both sides during the PATCH"
	var at time.Time
	e.gh.seam(func() {
		e.edit(t, store.IssueUpdate{Content: str(third)})
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Title, it.Body = "Crane, A", third })
		at = e.gh.issue(t, 5).Updated
		r, err := e.applier(other).ApplyIssues(context.Background(), e.proj, e.fetch(t))
		if err != nil || r.Applied != 0 || r.Conflicts != 0 {
			t.Errorf("the other instance's pull of the third version: %+v %v, want a convergence", r, err)
		}
	}, after)
	return &at
}

// i509CommitKill ends the backend of the first COMMIT its pool sends once
// armed, before the COMMIT goes out (pg_terminate_backend over killer), so
// that COMMIT fails and the transaction is rolled back.
type i509CommitKill struct {
	armed, fired atomic.Bool
	killer       *pgxpool.Pool
}

func (k *i509CommitKill) TraceQueryStart(ctx context.Context, c *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	if k.armed.Load() && strings.EqualFold(strings.TrimSpace(data.SQL), "commit") && k.fired.CompareAndSwap(false, true) {
		_, _ = k.killer.Exec(context.Background(), `SELECT pg_terminate_backend($1, 5000)`, c.PgConn().PID())
	}
	return ctx
}

func (k *i509CommitKill) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestAPushRecordCountsItsConflictOnceItCommitted(t *testing.T) {
	e := i491Setup(t, "i509reccount", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	kill := &i509CommitKill{killer: e.otherInstance(t)}
	cfg, err := pgxpool.ParseConfig(e.dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Tracer = kill
	pushPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pushPool.Close)
	e.i509ThirdVersion(t, e.otherInstance(t), func() { kill.armed.Store(true) })

	res, err := NewPusher(pushPool, e.reg.SnapshotForTenant).PushProject(context.Background(), e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
	if err == nil || !kill.fired.Load() {
		t.Fatalf("precondition: the push %+v ended with %v and the record's COMMIT was killed %v — want a failed COMMIT", res, err, kill.fired.Load())
	}
	if res.Conflicts != 0 {
		t.Errorf("the push counted %d conflict(s) of a record whose COMMIT failed, want 0", res.Conflicts)
	}
	if base, flagged := e.mapping(t); flagged || base == store.UnknownBaseHash {
		t.Errorf("after the failed COMMIT: flagged %v, base unknown %v — want the record rolled back", flagged, base == store.UnknownBaseHash)
	}
}

func TestAPushAdvanceOffAThirdVersionKeepsTheRowsStamp(t *testing.T) {
	e := i491Setup(t, "i509thirdstamp", "", fakeIssue{Title: "Crane", Body: "forge body"})
	e.edit(t, store.IssueUpdate{Title: str("Crane, A")})
	converged := e.i509ThirdVersion(t, e.otherInstance(t), nil)
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || r.Conflicts != 1 {
		t.Fatalf("push: %+v, want the PATCH sent and the moved base counted as a conflict", r)
	}
	if base, flagged := e.mapping(t); !flagged || base != store.UnknownBaseHash {
		t.Fatalf("precondition: flagged %v, base unknown %v — want the base recorded unknown and flagged", flagged, base == store.UnknownBaseHash)
	}
	answered := e.gh.issue(t, 5).Updated
	if got := e.recordedStamp(t, e.id(t)); !i509SameStamp(got, *converged) || !answered.After(*converged) {
		t.Errorf("the mapping records the stamp %v, want %v, the version the other pull converged on (the PATCH answered %v)", got, *converged, answered)
	}
}

func TestASameStampFetchOfAFlaggedConflictIsNoSkip(t *testing.T) {
	logs := i491CaptureLog(t)
	e, _ := i509FlaggedLevel(t, "i509flagsame")
	recorded := e.recordedStamp(t, e.id(t))
	e.edit(t, store.IssueUpdate{Title: str("Crane, edited locally")})
	e.gh.editInSecond(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge, third" })
	if at := e.gh.issue(t, 5).Updated; !i509SameStamp(recorded, at) {
		t.Fatalf("precondition: the mapping records %v, the forge lists %v — want the same stamp", recorded, at)
	}
	before := logs.count(i491StaleWarn)
	if r := e.pullOn(t, e.pool); r.Skipped != 0 || r.Applied != 0 || r.Conflicts != 0 {
		t.Errorf("the pull of a fetch with the recorded stamp on the flagged mapping: %+v, want no skip and nothing written or counted", r)
	}
	if n := logs.count(i491StaleWarn) - before; n != 0 {
		t.Errorf("%d WARN line(s) for a fetch that is not older than the recorded stamp, want none", n)
	}
	if _, flagged := e.mapping(t); !flagged {
		t.Errorf("the flag is gone")
	}
}
