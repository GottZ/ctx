//go:build integration

// Issue #452: the dream's temporal review is due for a block whose title or
// content it has not reviewed, whatever the order the writes carry their
// updated_at in. updated_at is the start of the writing transaction (now()),
// not the order the writes commit in, so a comparison of it with the review's
// stamp left two windows (the issue's point 1): a write that began before the
// stamp and commits after it reads as reviewed. These tests observe what the
// next cycle does — how many reviews it sends and what it sends — and the
// temporal rows, not a column: the same tests run against the timestamp form
// and against the version form.
//
// The orders are forced by holds on the client side of the production
// statements (i452BeginHold, testdb.HoldCommitPool), the blocking review stub
// and row locks, not by sleeps; the sleeps of a few milliseconds only keep
// two transaction starts apart.
//
// Run with:
//
//	go test -tags=integration ./internal/dream/ -run 'TestTemporalDue|TestTemporalReview(Rewrites|Failed|ThatAlways)|TestTemporalSameContent|TestTemporalTagsAndMetadata|TestTemporalWritersAgainstReviews' -count=1 -v
package dream_test

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"math/rand"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
)

// i452Apart keeps two transaction starts apart: now() has microsecond
// resolution, and a test that orders updated_at values needs them distinct.
const i452Apart = 5 * time.Millisecond

// i452BeginHold holds the first transaction on its pool that begins: the
// statement after its BEGIN waits on the client side until Release, so the
// transaction's now() — the updated_at its write stores — is the time of the
// BEGIN while the transaction holds no row yet. With commit set, the same
// transaction is held again before its COMMIT until ReleaseCommit, with
// every lock it took. Later transactions pass.
type i452BeginHold struct {
	commit bool

	mu        sync.Mutex
	begun     map[*pgx.Conn]bool
	conn      *pgx.Conn
	used      bool
	committed bool
	pid       uint32

	held, atCommit           chan struct{}
	release, releaseCommit   chan struct{}
	heldOnce, atCommitOnce   sync.Once
	releaseOnce, commitOnce  sync.Once
}

func i452HoldAfterBegin(t *testing.T, dsn string, commit bool) (*pgxpool.Pool, *i452BeginHold) {
	t.Helper()
	h := &i452BeginHold{
		commit: commit, begun: map[*pgx.Conn]bool{},
		held: make(chan struct{}), atCommit: make(chan struct{}),
		release: make(chan struct{}), releaseCommit: make(chan struct{}),
	}
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("begin hold: parse dsn: %v", err)
	}
	cfg.ConnConfig.Tracer = h
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatalf("begin hold: pool: %v", err)
	}
	t.Cleanup(func() {
		h.Release()
		h.ReleaseCommit()
		pool.Close()
	})
	return pool, h
}

// Wait waits until the held transaction has begun and waits before its first
// statement.
func (h *i452BeginHold) Wait(t *testing.T) {
	t.Helper()
	select {
	case <-h.held:
	case <-time.After(i427Wait):
		t.Fatalf("begin hold: no transaction began within %s", i427Wait)
	}
}

// WaitCommit waits until the held transaction waits before its COMMIT and
// returns its backend pid.
func (h *i452BeginHold) WaitCommit(t *testing.T) uint32 {
	t.Helper()
	select {
	case <-h.atCommit:
	case <-time.After(i427Wait):
		t.Fatalf("begin hold: the held transaction did not reach its COMMIT within %s", i427Wait)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.pid
}

func (h *i452BeginHold) Release()       { h.releaseOnce.Do(func() { close(h.release) }) }
func (h *i452BeginHold) ReleaseCommit() { h.commitOnce.Do(func() { close(h.releaseCommit) }) }

func (h *i452BeginHold) statement(conn *pgx.Conn, sql string) {
	lower := strings.ToLower(strings.TrimSpace(sql))
	h.mu.Lock()
	switch {
	case strings.HasPrefix(lower, "begin"):
		if !h.used {
			h.begun[conn] = true
		}
		h.mu.Unlock()
		return
	case lower == "commit" && h.commit && conn == h.conn && !h.committed:
		h.committed = true
		h.pid = conn.PgConn().PID()
		h.mu.Unlock()
		h.atCommitOnce.Do(func() { close(h.atCommit) })
		select {
		case <-h.releaseCommit:
		case <-time.After(i427Wait):
		}
		return
	case h.begun[conn] && !h.used:
		h.used = true
		h.conn = conn
		delete(h.begun, conn)
		h.mu.Unlock()
		h.heldOnce.Do(func() { close(h.held) })
		select {
		case <-h.release:
		case <-time.After(i427Wait):
		}
		return
	}
	delete(h.begun, conn)
	h.mu.Unlock()
}

func (h *i452BeginHold) TraceQueryStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	h.statement(conn, data.SQL)
	return ctx
}

func (h *i452BeginHold) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func (h *i452BeginHold) TraceBatchStart(ctx context.Context, conn *pgx.Conn, data pgx.TraceBatchStartData) context.Context {
	if len(data.Batch.QueuedQueries) > 0 {
		h.statement(conn, data.Batch.QueuedQueries[0].SQL)
	}
	return ctx
}

func (h *i452BeginHold) TraceBatchQuery(context.Context, *pgx.Conn, pgx.TraceBatchQueryData) {}

func (h *i452BeginHold) TraceBatchEnd(context.Context, *pgx.Conn, pgx.TraceBatchEndData) {}

// next runs one more cycle on the block id — embedded again (a title or
// content write clears the vector) and its claim ended — whose reviews answer
// answer, and returns the prompts the cycle sent.
func (rig *i427Rig) next(t *testing.T, id string, answer func(string) string) []string {
	t.Helper()
	rig.embed(t, id)
	rig.endClaim(t, id)
	prompts := rig.review.record(answer)
	i427Await(t, rig.cycle(), "next cycle")
	return *prompts
}

// lateWrite is a content write whose transaction begins now and writes once
// finish runs; finish then runs its temporal afterwork, as the write paths do.
type i452LateWrite struct {
	hold *i452BeginHold
	done chan *store.Block
}

func (rig *i427Rig) lateWrite(t *testing.T, id, content string, holdCommit bool) *i452LateWrite {
	t.Helper()
	pool, hold := i452HoldAfterBegin(t, rig.dsn, holdCommit)
	w := &i452LateWrite{hold: hold, done: make(chan *store.Block, 1)}
	go func() {
		b, _, err := store.UpdateBlock(context.Background(), pool, id, store.UpdateBlockData{Content: &content}, []string{i427Scope})
		if err != nil {
			t.Errorf("late write: %v", err)
		}
		w.done <- b
	}()
	hold.Wait(t)
	time.Sleep(i452Apart)
	return w
}

// finish lets the write go, waits for its commit and runs its afterwork.
func (w *i452LateWrite) finish(t *testing.T, rig *i427Rig) {
	t.Helper()
	w.hold.Release()
	w.hold.ReleaseCommit()
	w.afterwork(t, rig)
}

func (w *i452LateWrite) afterwork(t *testing.T, rig *i427Rig) {
	t.Helper()
	select {
	case b := <-w.done:
		if b == nil {
			t.Fatalf("late write stored nothing")
		}
		if _, err := store.RefreshTemporal(context.Background(), rig.pool, b.ID, b.Content, b.CreatedAt); err != nil {
			t.Fatalf("late write afterwork: %v", err)
		}
	case <-time.After(i427Wait):
		t.Fatalf("late write did not finish within %s", i427Wait)
	}
}

// wantReviewOf fails unless prompts is one review that read content.
func i452WantReviewOf(t *testing.T, prompts []string, content string) {
	t.Helper()
	if len(prompts) != 1 || !strings.Contains(prompts[0], content) {
		t.Errorf("the next cycle sent %d review(s) %q, want one of %q", len(prompts), prompts, content)
	}
}

// TestTemporalDueWriteBegunBeforeTheLastStamp is the first window of point 1
// (CodeRabbit on #449): a content write W begins, a later version is written
// and reviewed (stamp 20), the next version (updated_at 30) is picked, and W
// stores its content with updated_at 10 — during the review, or after it.
// The stored content was never reviewed, so the next pick must review it.
// Before, the stamp stayed 20 (W during the review: the version did not hold)
// or became 30 (W after it), both later than W's updated_at, and the next
// pick skipped W's content.
func TestTemporalDueWriteBegunBeforeTheLastStamp(t *testing.T) {
	for _, tc := range []struct {
		name         string
		duringReview bool
	}{
		{"the write commits during the review", true},
		{"the write commits after the review", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			b := rig.block(t, "Kickoff plan", "Kickoff on 2026-03-15.")

			late := "Kickoff moved to 2026-07-20."
			w := rig.lateWrite(t, b.ID, late, false)

			stamped := "Kickoff on 2026-03-16."
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: &stamped})
			if p := rig.next(t, b.ID, i427Dates()); len(p) != 1 {
				t.Fatalf("the review of the version before the pick: %d review(s), want 1", len(p))
			}
			time.Sleep(i452Apart)
			picked := "Kickoff on 2026-03-17."
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Content: &picked})
			rig.embed(t, b.ID)
			rig.endClaim(t, b.ID)

			entered, release := rig.review.hold(i427Dates())
			done := rig.cycle()
			if p := i427Entered(t, entered, done); !strings.Contains(p, picked) {
				t.Fatalf("the review was sent %q, want the picked content", p)
			}
			if tc.duringReview {
				w.finish(t, rig)
				close(release)
				i427Await(t, done, "cycle")
			} else {
				close(release)
				i427Await(t, done, "cycle")
				w.finish(t, rig)
			}

			i452WantReviewOf(t, rig.next(t, b.ID, i427Dates()), late)
		})
	}
}

// TestTemporalDueWriteBegunBeforeATagsWrite is the second window of point 1
// (the review of #449): during the review a content write A begins, a tags
// write B commits (updated_at t2), and A stores its content with updated_at
// t1 < t2. When A commits after the stamp, the stamp read B's version —
// title and content as reviewed — and set t2; A's content was never reviewed.
// When A holds the row as the stamp comes, the stamp of #449 waits for it and
// leaves the block due; the version form does not wait, and must leave it due
// as well.
func TestTemporalDueWriteBegunBeforeATagsWrite(t *testing.T) {
	for _, tc := range []struct {
		name       string
		holdsAtEnd bool
	}{
		{"the content write commits after the stamp", false},
		{"the content write holds the row when the review ends", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			b := rig.block(t, "Release plan", "Release on 2026-04-02.")

			entered, release := rig.review.hold(i427Dates())
			done := rig.cycle()
			i427Entered(t, entered, done)

			late := "Release moved to 2026-04-30."
			w := rig.lateWrite(t, b.ID, late, tc.holdsAtEnd)
			rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Tags: []string{"release"}})

			if tc.holdsAtEnd {
				w.hold.Release()
				pid := w.hold.WaitCommit(t)
				close(release)
				cycled := make(chan struct{})
				var cycleErr error
				go func() {
					defer close(cycled)
					cycleErr = <-done
				}()
				i452UntilDoneOrBlockedBy(t, rig.pool, pid, cycled)
				w.hold.ReleaseCommit()
				w.afterwork(t, rig)
				select {
				case <-cycled:
				case <-time.After(i427Wait):
					t.Fatalf("the cycle did not finish within %s", i427Wait)
				}
				if cycleErr != nil {
					t.Fatalf("cycle: %v", cycleErr)
				}
			} else {
				close(release)
				i427Await(t, done, "cycle")
				w.finish(t, rig)
			}

			i452WantReviewOf(t, rig.next(t, b.ID, i427Dates()), late)
		})
	}
}

// i452UntilDoneOrBlockedBy waits until the cycle is done or a backend of the
// test database waits on a lock pid holds.
func i452UntilDoneOrBlockedBy(t *testing.T, pool *pgxpool.Pool, pid uint32, done <-chan struct{}) {
	t.Helper()
	deadline := time.Now().Add(i427Wait / 2)
	for {
		select {
		case <-done:
			return
		default:
		}
		var waiters int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid))`, int64(pid)).Scan(&waiters); err != nil {
			t.Fatalf("poll waiters: %v", err)
		}
		if waiters > 0 {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("neither the cycle finished nor a statement waited on pid %d within %s", pid, i427Wait/2)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestTemporalReviewRewritesRowsOfAnOldTitle is point 2: a review found a
// date in the title; a title-only write replaces it (no temporal afterwork
// runs for a title), and the next review finds nothing. The old title's date
// must go. Before, phase 1 kept the rows (there were not fewer than the
// content implies) and no merge ran without a finding.
func TestTemporalReviewRewritesRowsOfAnOldTitle(t *testing.T) {
	rig := newI427Rig(t)
	b := rig.block(t, "Retro on 2026-02-27", "Notes of the retro.")
	if p := rig.next(t, b.ID, i427Dates("2026-02-27")); len(p) != 1 {
		t.Fatalf("first review: %d review(s), want 1", len(p))
	}
	if n := rig.rowsAt(t, b.ID, "2026-02-27"); n != 8 {
		t.Fatalf("the title's date has %d rows after the first review, want 8", n)
	}

	title := "Retro notes"
	rig.update(t, rig.pool, b.ID, store.UpdateBlockData{Title: &title})
	if p := rig.next(t, b.ID, i427Dates()); len(p) != 1 || !strings.Contains(p[0], title) {
		t.Fatalf("the review of the new title: %d review(s) %q, want one", len(p), p)
	}
	if n := rig.rowsAt(t, b.ID, "2026-02-27"); n != 0 {
		t.Errorf("the replaced title's date still has %d rows after the review of the new title", n)
	}
	var created int
	if err := rig.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_temporal WHERE block_id = $1 AND source_time = $2`, b.ID, b.CreatedAt).Scan(&created); err != nil {
		t.Fatalf("count rows: %v", err)
	}
	if created != 8 {
		t.Errorf("the creation time has %d rows, want 8", created)
	}
}

// TestTemporalSameContentUpsertKeepsTheReviewFindings is point 3: an upsert
// that stores the content the block holds runs the temporal afterwork, which
// wrote the extraction's rows and removed the review's findings. Here the
// upsert commits, a cycle runs, and the afterwork comes after it (it runs
// after the answer, outside any lock). The review read this version; its
// findings must stay and the version needs no second review.
func TestTemporalSameContentUpsertKeepsTheReviewFindings(t *testing.T) {
	rig := newI427Rig(t)
	ctx := context.Background()
	title, content := "Budget plan", "Budget review on 2026-05-04."
	b := rig.block(t, title, content)
	if p := rig.next(t, b.ID, i427Dates("2026-05-11")); len(p) != 1 {
		t.Fatalf("first review: %d review(s), want 1", len(p))
	}

	up, err := store.UpsertBlock(ctx, rig.pool, "decisions", title, content, nil, nil, i427Scope, true,
		store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, "")
	if err != nil || up.ID != b.ID {
		t.Fatalf("same-content upsert: %v (id %v)", err, up)
	}
	if p := rig.next(t, b.ID, i427Dates("2026-05-11")); len(p) != 0 {
		t.Errorf("a same-content upsert made the reviewed version due again: %d review(s)", len(p))
	}
	if _, err := store.RefreshTemporal(ctx, rig.pool, up.ID, up.Content, up.CreatedAt); err != nil {
		t.Fatalf("upsert afterwork: %v", err)
	}
	if n := rig.rowsAt(t, b.ID, "2026-05-11"); n != 8 {
		t.Errorf("the review's finding has %d rows after the same-content upsert's afterwork, want 8", n)
	}
	if p := rig.next(t, b.ID, i427Dates()); len(p) != 0 {
		t.Errorf("the unchanged block was reviewed again: %d review(s)", len(p))
	}
	if n := rig.rowsAt(t, b.ID, "2026-05-04"); n != 8 {
		t.Errorf("the content's own date has %d rows, want 8", n)
	}
}

// TestTemporalTagsAndMetadataWritesNeedNoReview is the side effect the issue
// names for the version form: the review reads the title and the content
// (and the creation date, which no write changes), so a write of tags,
// metadata or the category leaves the reviewed version standing. Before,
// each bumped updated_at and cost a review.
func TestTemporalTagsAndMetadataWritesNeedNoReview(t *testing.T) {
	cat := "learnings"
	for _, tc := range []struct {
		name string
		data store.UpdateBlockData
	}{
		{"tags", store.UpdateBlockData{Tags: []string{"plan"}}},
		{"metadata", store.UpdateBlockData{Metadata: map[string]any{"owner": "ops"}}},
		{"category", store.UpdateBlockData{Category: &cat}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			b := rig.block(t, "Plan "+tc.name, "Review on 2026-06-01.")
			if p := rig.next(t, b.ID, i427Dates("2026-06-08")); len(p) != 1 {
				t.Fatalf("first review: %d review(s), want 1", len(p))
			}
			rig.update(t, rig.pool, b.ID, tc.data)
			if p := rig.next(t, b.ID, i427Dates()); len(p) != 0 {
				t.Errorf("a %s write made the reviewed version due: %d review(s)", tc.name, len(p))
			}
			if n := rig.rowsAt(t, b.ID, "2026-06-08"); n != 8 {
				t.Errorf("the review's finding has %d rows after a %s write, want 8", n, tc.name)
			}
		})
	}
}

// i452FailInserts makes every insert of a temporal row of block id fail
// (when is empty) or every insert of a row at the day when, until the test
// drops it: a fault on the database inside the review's own writes.
func i452FailInserts(t *testing.T, pool *pgxpool.Pool, id, when string) func() {
	t.Helper()
	ctx := context.Background()
	cond := fmt.Sprintf("NEW.block_id = '%s'::uuid", id)
	if when != "" {
		cond += fmt.Sprintf(" AND NEW.source_time = '%s'::timestamptz", when)
	}
	stmts := []string{
		`CREATE OR REPLACE FUNCTION i452_fail() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN RAISE EXCEPTION 'i452: injected fault'; END $$`,
		`CREATE TRIGGER i452_fail BEFORE INSERT ON context_temporal FOR EACH ROW WHEN (` + cond + `) EXECUTE FUNCTION i452_fail()`,
	}
	for _, s := range stmts {
		if _, err := pool.Exec(ctx, s); err != nil {
			t.Fatalf("inject fault: %v", err)
		}
	}
	dropped := false
	drop := func() {
		if dropped {
			return
		}
		dropped = true
		if _, err := pool.Exec(ctx, `DROP TRIGGER i452_fail ON context_temporal`); err != nil {
			t.Fatalf("drop fault: %v", err)
		}
	}
	t.Cleanup(drop)
	return drop
}

// TestTemporalReviewFailedOnTheDatabaseStaysDue is point 4: a review whose
// write failed on the database — phase 1's re-extraction, or the write of
// its findings — reviewed nothing that stuck, and the next pick must review
// the block again. Before, the cycle stamped the block after the error.
func TestTemporalReviewFailedOnTheDatabaseStaysDue(t *testing.T) {
	for _, tc := range []struct {
		name    string
		phase1  bool
		finding []string
	}{
		{"phase 1", true, nil},
		{"the findings' write", false, []string{"2026-10-12"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rig := newI427Rig(t)
			ctx := context.Background()
			b := rig.block(t, "Audit plan", "Audit on 2026-10-05.")
			when := ""
			if tc.phase1 {
				// Phase 1 rewrites only a block with fewer rows than its
				// dates imply.
				if _, err := rig.pool.Exec(ctx, `DELETE FROM context_temporal WHERE block_id = $1`, b.ID); err != nil {
					t.Fatalf("clear rows: %v", err)
				}
			} else {
				when = tc.finding[0]
			}
			drop := i452FailInserts(t, rig.pool, b.ID, when)
			rig.next(t, b.ID, i427Dates(tc.finding...))
			drop()

			prompts := rig.next(t, b.ID, i427Dates(tc.finding...))
			i452WantReviewOf(t, prompts, "Audit on 2026-10-05.")
			if !tc.phase1 {
				if n := rig.rowsAt(t, b.ID, tc.finding[0]); n != 8 {
					t.Errorf("the finding has %d rows after the review that could write, want 8", n)
				}
			}
		})
	}
}

// TestTemporalReviewThatAlwaysFailsDoesNotHoldThePick: a block whose review
// fails on the database every time stays due, and must not hold the dream's
// pick (issue #435 saw 55 always-failing blocks stall the temporal backfill).
// The pick does not select by temporal due-ness; it claims the block it hands
// out (PickClaimTTL, the cooldown it writes with the pick), so the next cycle
// picks the next block, and the failing block is retried only once its claim
// — in production the back-off cooldown of the finished cycle — has run out.
func TestTemporalReviewThatAlwaysFailsDoesNotHoldThePick(t *testing.T) {
	rig := newI427Rig(t)
	ctx := context.Background()
	failing := rig.block(t, "Failing plan", "Failing review on 2026-11-02.")
	other := rig.block(t, "Other plan", "Other review on 2026-11-03.")
	// The failing block is picked first (quality_score ASC after
	// dream_checked_at).
	if _, err := rig.pool.Exec(ctx, `UPDATE context_blocks SET quality_score = 0.1 WHERE id = $1`, failing.ID); err != nil {
		t.Fatalf("order: %v", err)
	}
	i452FailInserts(t, rig.pool, failing.ID, "2026-11-09")
	finding := i427Dates("2026-11-09")

	prompts := rig.review.record(finding)
	i427Await(t, rig.cycle(), "first cycle")
	i452WantReviewOf(t, *prompts, "Failing review on 2026-11-02.")

	prompts = rig.review.record(finding)
	i427Await(t, rig.cycle(), "second cycle")
	i452WantReviewOf(t, *prompts, "Other review on 2026-11-03.")

	// Both are claimed now; the failing one's claim runs out first.
	rig.endClaim(t, failing.ID)
	prompts = rig.review.record(finding)
	i427Await(t, rig.cycle(), "third cycle")
	i452WantReviewOf(t, *prompts, "Failing review on 2026-11-02.")
	_ = other
}

// i452Finding is the stub's answer to a review: one date derived from the
// prompt, so each version of a block has a finding of its own that the
// extraction of its content does not yield (the year 2019 appears in no
// content of the load test).
func i452Finding(prompt string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(prompt))
	d := time.Date(2019, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, int(h.Sum32()%360))
	return d.Format("2006-01-02")
}

// TestTemporalWritersAgainstReviews is the load test of the version form:
// eight writers, eighty rounds each, over four blocks — content, title, tags
// and metadata writes, same-content upserts, each with the afterwork its
// handler runs — against two loops of dream cycles whose reviews answer at
// once. No statement may fail (a deadlock is 40P01), and once the writers are
// done and the reviews have caught up, every block holds the finding of the
// review of the version it holds, and is not due: no review was lost.
func TestTemporalWritersAgainstReviews(t *testing.T) {
	if testing.Short() {
		t.Skip("load test")
	}
	rig := newI427Rig(t)
	ctx := context.Background()
	const blocks, writers, rounds = 4, 8, 80

	var mu sync.Mutex
	seen := map[string]string{} // prompt → finding
	answer := func(p string) string {
		f := i452Finding(p)
		mu.Lock()
		seen[p] = f
		mu.Unlock()
		return i427Dates(f)(p)
	}
	rig.review.record(answer)

	ids := make([]string, blocks)
	for i := range ids {
		ids[i] = rig.block(t, fmt.Sprintf("Load plan %d", i), fmt.Sprintf("Load review %d on 2026-01-%02d.", i, i+1)).ID
	}

	var failures sync.Map
	fail := func(what string, err error) {
		if err != nil {
			failures.Store(fmt.Sprintf("%s: %v", what, err), true)
		}
	}
	stop := make(chan struct{})
	var reviewers sync.WaitGroup
	for r := 0; r < 2; r++ {
		reviewers.Add(1)
		go func() {
			defer reviewers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				i452Cycle(ctx, rig, fail)
				for _, id := range ids {
					_, err := rig.pool.Exec(ctx, `UPDATE context_blocks SET dream_cooldown_until = NULL WHERE id = $1`, id)
					fail("end claim", err)
				}
			}
		}()
	}

	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(int64(w + 452)))
			for round := 0; round < rounds; round++ {
				i452Write(ctx, rig, ids[rnd.Intn(blocks)], rnd, w, round, fail)
			}
		}(w)
	}
	wg.Wait()
	close(stop)
	reviewers.Wait()

	failures.Range(func(k, _ any) bool {
		t.Errorf("%s", k)
		return true
	})

	// The reviews catch up: a cycle per block until none is due.
	for _, id := range ids {
		rig.embed(t, id)
	}
	for pass := 0; pass < 3; pass++ {
		for _, id := range ids {
			rig.endClaim(t, id)
		}
		for range ids {
			i452Cycle(ctx, rig, func(what string, err error) {
				if err != nil {
					t.Fatalf("catch-up %s: %v", what, err)
				}
			})
		}
	}
	for _, id := range ids {
		var title, content string
		if err := rig.pool.QueryRow(ctx, `SELECT title, content FROM context_blocks WHERE id = $1`, id).Scan(&title, &content); err != nil {
			t.Fatalf("read block: %v", err)
		}
		want := ""
		mu.Lock()
		for p, f := range seen {
			if strings.Contains(p, "Title: "+title+"\nContent:\n"+content+"\n</block>") {
				want = f
			}
		}
		mu.Unlock()
		if want == "" {
			t.Errorf("block %s: its version (%q, %q) was never reviewed", id, title, content)
			continue
		}
		if n := rig.rowsAt(t, id, want); n != 8 {
			t.Errorf("block %s: the finding %s of the review of its version has %d rows, want 8", id, want, n)
		}
		if p := rig.next(t, id, answer); len(p) != 0 {
			t.Errorf("block %s: still due after the catch-up: %d review(s)", id, len(p))
		}
	}
}

// i452Cycle runs one dream cycle that ends after the temporal review; a
// cycle that picked nothing is not a failure here.
func i452Cycle(ctx context.Context, rig *i427Rig, fail func(string, error)) {
	cctx, cancel := context.WithTimeout(ctx, i427Wait)
	defer cancel()
	_, err := dream.RunDreamCycle(cctx, rig.pool, rig.router, dream.DreamOptions(),
		dream.BackoffConfig{Mode: "exp", Factor: 1.6, MinHours: 12, CapHours: 1080, InertOffset: 7},
		[]string{i427Scope}, i427StopAfterTemporal)
	if err != nil && !errors.Is(err, errI427Stop) {
		fail("cycle", err)
	}
}

// i452Write is one round of a load-test writer on the block id.
func i452Write(ctx context.Context, rig *i427Rig, id string, rnd *rand.Rand, w, round int, fail func(string, error)) {
	var data store.UpdateBlockData
	switch op := rnd.Intn(5); op {
	case 0:
		c := fmt.Sprintf("Load content %d-%d on 2026-%02d-%02d.", w, round, rnd.Intn(12)+1, rnd.Intn(28)+1)
		data.Content = &c
	case 1:
		tt := fmt.Sprintf("Load title %d-%d", w, round)
		data.Title = &tt
	case 2:
		data.Tags = []string{fmt.Sprintf("t%d", rnd.Intn(4))}
	case 3:
		data.Metadata = map[string]any{"round": round}
	case 4:
		var cat, title, content string
		if err := rig.pool.QueryRow(ctx, `SELECT category, title, content FROM context_blocks WHERE id = $1`, id).Scan(&cat, &title, &content); err != nil {
			fail("read for upsert", err)
			return
		}
		b, err := store.UpsertBlock(ctx, rig.pool, cat, title, content, nil, nil, i427Scope, true,
			store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, "")
		if err != nil {
			// A title write between the read and the upsert makes this a
			// new block; that is not this test's subject.
			fail("same-content upsert", err)
			return
		}
		if b.ID != id {
			return
		}
		_, err = store.RefreshTemporal(ctx, rig.pool, b.ID, b.Content, b.CreatedAt)
		fail("upsert afterwork", err)
		return
	}
	b, _, err := store.UpdateBlock(ctx, rig.pool, id, data, []string{i427Scope})
	if err != nil || b == nil {
		fail("update", err)
		return
	}
	if data.Content != nil {
		_, err := store.RefreshTemporal(ctx, rig.pool, b.ID, b.Content, b.CreatedAt)
		fail("update afterwork", err)
	}
	if data.Content != nil || data.Title != nil {
		v := i427Vec()
		fail("embed", store.StoreEmbedding(ctx, rig.pool, id, i427Model, v.Slice()))
	}
}
