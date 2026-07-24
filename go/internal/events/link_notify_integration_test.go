//go:build integration

// Integration gates for Migration 116 (link-write NOTIFY, design/05 §3.1 +
// §7 wave W05.3) against a real migrated Postgres (testcontainers, PG18).
// Everything here drives the ACTUAL trigger SQL over a live LISTEN connection
// on ctx_link_write, so the dedupe/0-row/column-filter claims are measured on
// the deployed catalog rather than argued.
//
//	ManagePutNotifies      — the production manage-link-put write path
//	                         (store.PutStructuralLink, the DB layer behind
//	                         handler/context_manage_issues.go's issue-link-create)
//	                         produces a ctx_link_write NOTIFY. RED against Ist:
//	                         without 116 that path is completely signal-free.
//	BatchDeleteOneNotify   — (a) one DELETE statement hitting N rows in one
//	                         transaction yields EXACTLY ONE notification
//	                         (NOTIFY dedupe on a constant payload, §3.1 Nr. 1).
//	ZeroRowStatementSilent — (b) a deleteStaleLinks-shaped statement with 0 hits
//	                         yields NO notification (row-level trigger, §3.1 Nr. 1).
//	BlockAttrFlip          — (c) is_archived / scope flips on context_blocks
//	                         notify; a dream_checked_at stamp does NOT
//	                         (column-filtered + WHEN-guarded trigger, E-05-7).
//	TruncateNotifies       — TRUNCATE on either link table notifies via the
//	                         statement-level trigger (§3.1 Nr. 6).
//	DrivesRebuildCondition — (d) REAL notifications fed through the production
//	                         LinkWriteHandler drive the Dirty clock, and under
//	                         writes denser than DebounceWindow the §4.3 rebuild
//	                         condition still fires at MaxPendingAge.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/events/ -run TestW053 -count=1 -v
//
// Source: https://github.com/GottZ/ctx
package events

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// ── helpers ──────────────────────────────────────────────────────────────────.

// w053Listener is a dedicated LISTEN connection on ctx_link_write. Observing the
// channel directly (raw conn) instead of through the full scheduler keeps every
// gate deterministic: what is asserted is exactly what Postgres delivered.
type w053Listener struct {
	t    *testing.T
	conn *pgxpool.Conn
}

func w053Listen(t *testing.T, pool *pgxpool.Pool) *w053Listener {
	t.Helper()
	c, err := pool.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	t.Cleanup(c.Release)
	if _, err := c.Exec(context.Background(), "LISTEN "+channelLinkWrite); err != nil {
		t.Fatalf("LISTEN %s: %v", channelLinkWrite, err)
	}
	return &w053Listener{t: t, conn: c}
}

// drain collects payloads until the channel stays idle for the given window.
func (l *w053Listener) drain(idle time.Duration) []string {
	l.t.Helper()
	var out []string
	for {
		ctx, cancel := context.WithTimeout(context.Background(), idle)
		n, err := l.conn.Conn().WaitForNotification(ctx)
		cancel()
		if err != nil {
			return out
		}
		out = append(out, n.Payload)
	}
}

// w053SeedBlock inserts one block in the given scope and returns its id.
func w053SeedBlock(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name, is_archived)
		 VALUES ('test', $1, 'content', $2, 'knowledge', false) RETURNING id::text`,
		title, scope).Scan(&id); err != nil {
		t.Fatalf("seed block %q: %v", title, err)
	}
	return id
}

// ── Integrations-Gate: the signal-free manage-link-put path ──────────────────.

func TestW053ManagePutNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := w053SeedBlock(t, pool, "shared", "w053-put-src")
	dst := w053SeedBlock(t, pool, "shared", "w053-put-dst")

	l := w053Listen(t, pool)

	// Exactly the DB layer handler/context_manage_issues.go's issue-link-create
	// calls (store.PutStructuralLink inside the request transaction).
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.PutStructuralLink(ctx, tx, store.StructuralLink{
		SourceID: src, TargetID: dst, LinkClass: "references", Origin: "manual",
	}, []string{"shared"}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("PutStructuralLink: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	got := l.drain(2 * time.Second)
	if len(got) != 1 || got[0] != "context_structural_links" {
		t.Fatalf("manage-link-put produced %v on %s, want exactly [context_structural_links] "+
			"(RED against Ist: without migration 116 this path is signal-free)", got, channelLinkWrite)
	}
}

// ── (a) batch DELETE ⇒ exactly one notification ──────────────────────────────.

func TestW053BatchDeleteOneNotify(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := w053SeedBlock(t, pool, "shared", "w053-batch-src")
	const n = 25
	for i := 0; i < n; i++ {
		dst := w053SeedBlock(t, pool, "shared", "w053-batch-dst-"+uuid.NewString()[:8])
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared')`, src, dst); err != nil {
			t.Fatalf("seed dream link %d: %v", i, err)
		}
	}

	// LISTEN only AFTER the seed writes so their own notifies are not counted.
	l := w053Listen(t, pool)

	tag, err := pool.Exec(ctx, `DELETE FROM context_dream_links WHERE source_block_id = $1::uuid`, src)
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if tag.RowsAffected() != n {
		t.Fatalf("batch delete removed %d rows, want %d (setup broke)", tag.RowsAffected(), n)
	}

	got := l.drain(1500 * time.Millisecond)
	if len(got) != 1 {
		t.Fatalf("batch DELETE of %d rows produced %d notifications (%v), want 1 "+
			"(NOTIFY dedupe on the constant payload, §3.1 Nr. 1)", n, len(got), got)
	}
}

// ── (b) 0-row statement ⇒ no notification ────────────────────────────────────.

func TestW053ZeroRowStatementSilent(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := w053SeedBlock(t, pool, "shared", "w053-zero-src")
	dst := w053SeedBlock(t, pool, "shared", "w053-zero-dst")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared')`, src, dst); err != nil {
		t.Fatalf("seed dream link: %v", err)
	}

	l := w053Listen(t, pool)

	// deleteStaleLinks shape (dream/writelinks.go:48-53) where EVERY existing
	// target is kept ⇒ 0 rows deleted. A statement-level trigger would fire here.
	tag, err := pool.Exec(ctx,
		`DELETE FROM context_dream_links
		   WHERE source_block_id = $1::uuid
		     AND target_block_id != ALL($2::uuid[])`, src, []string{dst})
	if err != nil {
		t.Fatalf("zero-row delete: %v", err)
	}
	if tag.RowsAffected() != 0 {
		t.Fatalf("zero-row delete removed %d rows, want 0 (setup broke)", tag.RowsAffected())
	}

	// Positive terminator: a real link write must be the FIRST thing on the wire.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid, $2::uuid, 'references', 'shared', 'system')`, src, dst); err != nil {
		t.Fatalf("terminator struct link: %v", err)
	}

	got := l.drain(1500 * time.Millisecond)
	if len(got) != 1 || got[0] != "context_structural_links" {
		t.Fatalf("0-row statement + terminator produced %v, want exactly [context_structural_links] "+
			"(a 0-hit statement must stay silent, §3.1 Nr. 1)", got)
	}
}

// ── (c) block attribute flips vs. dream stamps ───────────────────────────────.

func TestW053BlockAttrFlip(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	id := w053SeedBlock(t, pool, "shared", "w053-flip")
	l := w053Listen(t, pool)

	t.Run("ArchiveFlipNotifies", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("archive flip: %v", err)
		}
		got := l.drain(1500 * time.Millisecond)
		if len(got) != 1 || got[0] != "context_blocks" {
			t.Fatalf("is_archived flip produced %v, want exactly [context_blocks]", got)
		}
	})

	t.Run("ScopeFlipNotifies", func(t *testing.T) {
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET scope = 'private' WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("scope flip: %v", err)
		}
		got := l.drain(1500 * time.Millisecond)
		if len(got) != 1 || got[0] != "context_blocks" {
			t.Fatalf("scope flip produced %v, want exactly [context_blocks]", got)
		}
	})

	t.Run("NoOpFlipSilent", func(t *testing.T) {
		// Same value written again: the WHEN (IS DISTINCT FROM) guard must swallow it.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("no-op flip: %v", err)
		}
		got := l.drain(1500 * time.Millisecond)
		if len(got) != 0 {
			t.Fatalf("no-op is_archived write produced %v, want nothing (WHEN-guard)", got)
		}
	})

	t.Run("DreamStampSilent", func(t *testing.T) {
		// The high-frequency dream stamp must NOT reach the channel (§3.1 Nr. 5).
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET dream_checked_at = now() WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("dream stamp: %v", err)
		}
		// Positive terminator so "nothing arrived" is not just a slow wire.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = false WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("terminator flip: %v", err)
		}
		got := l.drain(1500 * time.Millisecond)
		if len(got) != 1 {
			t.Fatalf("dream stamp + terminator flip produced %d notifications (%v), want 1 "+
				"(a dream_checked_at stamp must stay off the channel)", len(got), got)
		}
	})
}

// ── TRUNCATE (§3.1 Nr. 6) ────────────────────────────────────────────────────.

func TestW053TruncateNotifies(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := w053SeedBlock(t, pool, "shared", "w053-trunc-src")
	dst := w053SeedBlock(t, pool, "shared", "w053-trunc-dst")
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared')`, src, dst); err != nil {
		t.Fatalf("seed dream link: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid, $2::uuid, 'references', 'shared', 'system')`, src, dst); err != nil {
		t.Fatalf("seed struct link: %v", err)
	}

	l := w053Listen(t, pool)

	for _, table := range []string{"context_dream_links", "context_structural_links"} {
		if _, err := pool.Exec(ctx, "TRUNCATE "+table); err != nil {
			t.Fatalf("truncate %s: %v", table, err)
		}
		got := l.drain(1500 * time.Millisecond)
		if len(got) != 1 || got[0] != table {
			t.Fatalf("TRUNCATE %s produced %v, want exactly [%s] "+
				"(statement-level TRUNCATE trigger, §3.1 Nr. 6)", table, got, table)
		}
	}
}

// ── (d) real notifications drive the §4.3 rebuild condition ──────────────────.

// TestW053DrivesRebuildCondition closes the DB-trigger-level half of gate (d):
// the pure condition logic (graphCacheDue) is covered by the table-driven unit
// test TestGraphCacheDueRebuildCondition (graph_cache_condition_test.go); what
// can only be shown against a live catalog is that REAL Migration-116 NOTIFYs,
// routed through the production LinkWriteHandler, are what drives the clock —
// and that under writes DENSER than DebounceWindow the condition still fires at
// MaxPendingAge instead of starving.
func TestW053DrivesRebuildCondition(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	src := w053SeedBlock(t, pool, "shared", "w053-clock-src")
	dst := w053SeedBlock(t, pool, "shared", "w053-clock-dst")

	// DebounceWindow far out of reach (quiet can never satisfy it inside the
	// test), MaxPendingAge short: only the starvation bound can fire.
	cfg := config.GraphCacheConfig{
		Enabled:            true,
		RebuildInterval:    time.Hour,
		DebounceWindow:     time.Hour,
		MinRebuildInterval: 0,
		MaxPendingAge:      2 * time.Second,
		MaxStaleness:       15 * time.Minute,
		FailedThreshold:    3,
	}
	s := NewScheduler(pool, config.NewStore(&config.Config{GraphCache: cfg}), backends.NewPool(nil, nil), StartupConfig{})

	// One real build so LastBuildStart is anchored — otherwise the zero time
	// makes the HARD interval trivially due and would mask the signal path.
	s.graphCacheBuildOnce(ctx, time.Now(), cfg, "test-anchor")
	if pending, _, _ := s.graphCache.Dirty(time.Now()); pending {
		t.Fatalf("dirty after the anchoring build — the pre-build signals were not consumed")
	}

	// Production routing: every notification on the wire goes through the same
	// handler the pgxlisten registration uses.
	l := w053Listen(t, pool)
	h := &LinkWriteHandler{scheduler: s}
	pump := func() int {
		n := 0
		for {
			wctx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
			nfy, err := l.conn.Conn().WaitForNotification(wctx)
			cancel()
			if err != nil {
				return n
			}
			if err := h.HandleNotification(ctx, nfy, nil); err != nil {
				t.Fatalf("HandleNotification: %v", err)
			}
			n++
		}
	}

	// First write: a real NOTIFY must open the dirty episode.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'shared')`, src, dst); err != nil {
		t.Fatalf("first link write: %v", err)
	}
	if got := pump(); got == 0 {
		t.Fatalf("no ctx_link_write notification for a real link write — the dirty clock is not driven")
	}
	if pending, _, _ := s.graphCache.Dirty(time.Now()); !pending {
		t.Fatal("pending = false after a real link-write notification, want true")
	}
	firstWrite := time.Now()

	// Not yet due: quiet << DebounceWindow and pendingAge << MaxPendingAge.
	if s.graphCacheDue(time.Now(), cfg) {
		t.Fatal("rebuild due immediately after the first signal — neither debounce nor pending age is satisfied")
	}

	// Keep writing DENSER than DebounceWindow (which is an hour): the quiet
	// window is never reached, only MaxPendingAge can break the starvation.
	for i := 0; time.Since(firstWrite) < cfg.MaxPendingAge+500*time.Millisecond; i++ {
		if _, err := pool.Exec(ctx,
			`UPDATE context_dream_links SET confidence = $2 WHERE source_block_id = $1::uuid`,
			src, 0.5+float64(i%10)/100); err != nil {
			t.Fatalf("dense write %d: %v", i, err)
		}
		pump()
		time.Sleep(200 * time.Millisecond)
	}
	pump()

	now := time.Now()
	pending, quiet, pendingAge := s.graphCache.Dirty(now)
	if !pending {
		t.Fatal("pending = false under sustained writes, want true")
	}
	if quiet >= cfg.DebounceWindow {
		t.Fatalf("quiet = %v reached DebounceWindow %v — the writes were not dense enough (setup broke)", quiet, cfg.DebounceWindow)
	}
	if pendingAge < cfg.MaxPendingAge {
		t.Fatalf("pendingAge = %v < MaxPendingAge %v (setup broke)", pendingAge, cfg.MaxPendingAge)
	}
	if !s.graphCacheDue(now, cfg) {
		t.Fatalf("rebuild NOT due at pendingAge %v ≥ MaxPendingAge %v under quiet %v — starvation bound did not fire",
			pendingAge, cfg.MaxPendingAge, quiet)
	}
}
