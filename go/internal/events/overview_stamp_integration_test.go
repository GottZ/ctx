//go:build integration

// W-A scheduler gates (design/02 §3.1 point 2/5, §7 gates 1/2/4/7): every one
// of the FIVE exits of rebuildOverviewOnce leaves a database row, none of them
// claims freshness, and the boot probe survives the new row shape.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestOverviewStamp -count=1 -v
package events

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
)

type stampRow struct {
	computedAt  *time.Time
	lastAttempt *time.Time
	skipReason  *string
	candidateN  *int
}

func readStamp(t *testing.T, pool *pgxpool.Pool, scope string) stampRow {
	t.Helper()
	var r stampRow
	if err := pool.QueryRow(context.Background(), `
		SELECT computed_at, last_attempt_at, skip_reason, candidate_n
		  FROM graph_overview_meta WHERE scope = $1`, scope).Scan(
		&r.computedAt, &r.lastAttempt, &r.skipReason, &r.candidateN); err != nil {
		t.Fatalf("meta row %q: %v", scope, err)
	}
	return r
}

// stampScheduler builds a Scheduler wired for the rebuild path: real pool,
// hot-reloadable config, a block-type registry (so the registry gate passes
// unless a case deliberately clears it) and a single-tenant owned filter.
func stampScheduler(t *testing.T, pool *pgxpool.Pool, cfg *config.Config) *Scheduler {
	t.Helper()
	s := NewScheduler(pool, config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)
	s.SetBlocktypeRegistry(reg)
	return s
}

func stampBlock(t *testing.T, pool *pgxpool.Pool, id, scope, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO context_blocks (id, scope, category, title, content)
		VALUES ($1::uuid, $2, 'learnings', $3, 'w-a fixture')`, id, scope, title); err != nil {
		t.Fatalf("stampBlock %s: %v", title, err)
	}
}

// TestOverviewStamp_AllFiveExits is §7 gates 1, 2 and 9 in one fixture: each
// exit of rebuildOverviewOnce writes its own reason, and NONE of them moves
// computed_at.
//
// RED against HEAD: four of the five exits (`scheduler.go:920`, `:928`,
// `:967-972`, `:973-976`) only log and return — the row does not exist at all,
// and after the success case it carries no skip_reason column to clear.
func TestOverviewStamp_AllFiveExits(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bt := backgroundTenant{scope: "private", owned: []string{"private"}}

	const (
		P1 = "019d0000-0000-7000-9000-00000000c001"
		P2 = "019d0000-0000-7000-9000-00000000c002"
		P3 = "019d0000-0000-7000-9000-00000000c003"
	)
	stampBlock(t, pool, P1, "private", "S-P1")
	stampBlock(t, pool, P2, "private", "S-P2")
	stampBlock(t, pool, P3, "private", "S-P3")

	enabled := func() *config.Config {
		c := &config.Config{}
		c.GraphOverview.Enabled = true
		c.GraphOverview.Resolution = 1.0
		c.GraphOverview.RebuildTimeout = time.Minute
		return c
	}

	// (1) Success — the reference. persist writes the row, skip_reason NULL,
	// last_attempt_at = computed_at (gate 9).
	s := stampScheduler(t, pool, enabled())
	s.rebuildOverviewOnce(ctx, bt)
	success := readStamp(t, pool, "private")
	if success.computedAt == nil || success.skipReason != nil {
		t.Fatalf("success: computed_at=%v skip_reason=%v, want fresh + NULL", success.computedAt, success.skipReason)
	}
	if success.lastAttempt == nil || !success.lastAttempt.Equal(*success.computedAt) {
		t.Fatalf("success: last_attempt_at=%v != computed_at=%v", success.lastAttempt, success.computedAt)
	}
	if success.candidateN == nil || *success.candidateN != 3 {
		t.Fatalf("success: candidate_n=%v, want 3", success.candidateN)
	}
	builtAt := *success.computedAt

	// (2) node-cap: MaxNodes below the candidate count. The partition freezes,
	// candidate_n records how far over the cap it was, computed_at stands still.
	capCfg := enabled()
	capCfg.GraphOverview.MaxNodes = 1
	s = stampScheduler(t, pool, capCfg)
	s.rebuildOverviewOnce(ctx, bt)
	capped := readStamp(t, pool, "private")
	if capped.skipReason == nil || *capped.skipReason != "node-cap" {
		t.Fatalf("node-cap: skip_reason=%v, want node-cap", capped.skipReason)
	}
	if capped.candidateN == nil || *capped.candidateN != 3 {
		t.Fatalf("node-cap: candidate_n=%v, want 3 (> MaxNodes 1)", capped.candidateN)
	}
	if capped.computedAt == nil || !capped.computedAt.Equal(builtAt) {
		t.Fatalf("node-cap moved computed_at: %v → %v (a skip makes a partition OLD, not fresh)", builtAt, capped.computedAt)
	}
	if capped.lastAttempt == nil || !capped.lastAttempt.After(builtAt) {
		t.Fatalf("node-cap: last_attempt_at=%v not advanced past %v", capped.lastAttempt, builtAt)
	}

	// (3) timeout: a 1 ns rebuild budget. The deadline discriminates 'timeout'
	// from 'error' — the outcome that DOMINATES at 1M+ and that a CHECK
	// covering only the two observable skips would have locked out (23514).
	toCfg := enabled()
	toCfg.GraphOverview.RebuildTimeout = time.Nanosecond
	s = stampScheduler(t, pool, toCfg)
	s.rebuildOverviewOnce(ctx, bt)
	timedOut := readStamp(t, pool, "private")
	if timedOut.skipReason == nil || *timedOut.skipReason != "timeout" {
		t.Fatalf("timeout: skip_reason=%v, want timeout", timedOut.skipReason)
	}
	if timedOut.computedAt == nil || !timedOut.computedAt.Equal(builtAt) {
		t.Fatalf("timeout moved computed_at: %v → %v", builtAt, timedOut.computedAt)
	}

	// (4) registry-unwired: the wiring gap that today only reaches slog.Error.
	s = stampScheduler(t, pool, enabled())
	s.blocktypes = nil
	s.rebuildOverviewOnce(ctx, bt)
	if r := readStamp(t, pool, "private"); r.skipReason == nil || *r.skipReason != "registry-unwired" {
		t.Fatalf("registry-unwired: skip_reason=%v", r.skipReason)
	}

	// (5) disabled: operator misconfiguration, previously invisible entirely.
	offCfg := enabled()
	offCfg.GraphOverview.Enabled = false
	s = stampScheduler(t, pool, offCfg)
	s.rebuildOverviewOnce(ctx, bt)
	off := readStamp(t, pool, "private")
	if off.skipReason == nil || *off.skipReason != "disabled" {
		t.Fatalf("disabled: skip_reason=%v", off.skipReason)
	}
	if off.candidateN == nil || *off.candidateN != 3 {
		t.Fatalf("disabled: candidate_n=%v, want the preserved 3 — a candidate-less exit must not zero the denominator", off.candidateN)
	}

	// …and a fresh success clears the whole cap record again.
	s = stampScheduler(t, pool, enabled())
	s.rebuildOverviewOnce(ctx, bt)
	healed := readStamp(t, pool, "private")
	if healed.skipReason != nil {
		t.Fatalf("post-recovery skip_reason=%q, want NULL — a good run must clear the cap", *healed.skipReason)
	}
	if healed.computedAt == nil || !healed.computedAt.After(builtAt) {
		t.Fatalf("post-recovery computed_at=%v not advanced past %v", healed.computedAt, builtAt)
	}
}

// TestOverviewStamp_BootProbeSurvivesStamp is §7 gate 4 — the one that makes
// W-A a LIVE REGRESSION if it is missed. A partition that carries nothing but
// an attempt stamp (computed_at NULL) must still read as "never built", or
// the boot-time first build (scheduler.go:822-837) is suppressed forever and
// the first partition only appears after a full rebuild_interval.
//
// RED against the pre-W-A `SELECT count(*) FROM graph_overview_meta`: one
// stamp row makes it non-zero and overviewNeverBuilt returns false.
func TestOverviewStamp_BootProbeSurvivesStamp(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	s := stampScheduler(t, pool, &config.Config{})

	if !s.overviewNeverBuilt(ctx) {
		t.Fatal("precondition: empty meta table must read as never built")
	}

	// A stamp-only row — exactly the "fresh deploy above the cap" case.
	if err := overview.StampAttempt(ctx, pool, []string{"private"}, "node-cap",
		map[string]int{"private": 1204331}, time.Now()); err != nil {
		t.Fatalf("stamp: %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_overview_meta`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("stamp row count = %d (err=%v), want 1 — the red probe needs the row to exist", n, err)
	}
	if !s.overviewNeverBuilt(ctx) {
		t.Fatal("a stamp-only row suppressed the boot build — count(*) instead of computed_at IS NOT NULL (live regression)")
	}

	// A row with a real computed_at flips the verdict, so the probe still
	// discriminates and did not simply become a constant true.
	if _, err := pool.Exec(ctx,
		`UPDATE graph_overview_meta SET computed_at = now(), skip_reason = NULL WHERE scope = 'private'`); err != nil {
		t.Fatal(err)
	}
	if s.overviewNeverBuilt(ctx) {
		t.Fatal("a built partition still reads as never built — probe degenerated to constant true")
	}
}

// TestOverviewStamp_ContentionRaceKeepsSuccess is §7 gate 7. advisory-lock
// means another instance is building this partition SUCCESSFULLY right now
// (cluster.go:553-555 "keeps serializing"). An unconditional stamp by the
// losing instance would mark a seconds-old partition as frozen and the root
// map would print a cap that does not exist — the map lying in the OTHER
// direction. The ON CONFLICT WHERE clause discards the loser's stamp.
//
// RED against a stamp without the `computed_at < attemptStart` condition.
func TestOverviewStamp_ContentionRaceKeepsSuccess(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Instance B starts its attempt…
	attemptStart := time.Now()

	// …instance A wins the lock and commits a fresh success in the meantime.
	time.Sleep(10 * time.Millisecond)
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, skip_reason, candidate_n, node_n, cluster_n)
		VALUES ('private', now(), now(), NULL, 1190, 1190, 59)`); err != nil {
		t.Fatal(err)
	}
	winner := readStamp(t, pool, "private")

	// …and only THEN does B's advisory-lock skip come back to stamp.
	if err := overview.StampAttempt(ctx, pool, []string{"private"}, "advisory-lock", nil, attemptStart); err != nil {
		t.Fatalf("loser stamp: %v", err)
	}

	after := readStamp(t, pool, "private")
	if after.skipReason != nil {
		t.Fatalf("loser's stamp marked a fresh partition as %q — the map would print a cap that does not exist", *after.skipReason)
	}
	if after.computedAt == nil || !after.computedAt.Equal(*winner.computedAt) {
		t.Fatalf("winner's computed_at changed: %v → %v", winner.computedAt, after.computedAt)
	}
	if after.candidateN == nil || *after.candidateN != 1190 {
		t.Fatalf("candidate_n = %v, want the winner's 1190", after.candidateN)
	}

	// Red-probe symmetry: a stamp whose attempt started AFTER the success is
	// a genuinely newer attempt and MUST land.
	if err := overview.StampAttempt(ctx, pool, []string{"private"}, "node-cap",
		map[string]int{"private": 2000000}, time.Now()); err != nil {
		t.Fatalf("later stamp: %v", err)
	}
	if r := readStamp(t, pool, "private"); r.skipReason == nil || *r.skipReason != "node-cap" {
		t.Fatalf("a genuinely later attempt was discarded too (skip_reason=%v) — the condition is not a race guard but a mute button", r.skipReason)
	}
}

// TestOverviewStamp_GlobalRunTouchesOnlyExistingRows pins the nil-ScopeFilter
// branch (design/02 §3.1 point 5): a global run has no known scope list, so
// its attempt stamps the rows that exist and creates none — never a row for a
// scope nobody has ever built.
func TestOverviewStamp_GlobalRunTouchesOnlyExistingRows(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_overview_meta (scope, computed_at, last_attempt_at, candidate_n, node_n)
		VALUES ('private', now() - interval '2 hours', now() - interval '2 hours', 100, 100)`); err != nil {
		t.Fatal(err)
	}

	if err := overview.StampAttempt(ctx, pool, nil, "timeout", nil, time.Now()); err != nil {
		t.Fatalf("global stamp: %v", err)
	}

	var rows int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM graph_overview_meta`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 1 {
		t.Fatalf("global stamp created rows: count = %d, want the pre-existing 1", rows)
	}
	r := readStamp(t, pool, "private")
	if r.skipReason == nil || *r.skipReason != "timeout" {
		t.Fatalf("global stamp: skip_reason = %v, want timeout", r.skipReason)
	}
	if r.candidateN == nil || *r.candidateN != 100 {
		t.Fatalf("global stamp zeroed candidate_n: %v, want the preserved 100", r.candidateN)
	}
	if r.computedAt == nil {
		t.Fatal("global stamp cleared computed_at")
	}
}
