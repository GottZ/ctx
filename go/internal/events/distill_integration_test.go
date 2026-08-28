//go:build integration

// Gate A02-5 (design/02 §7.2), journal half. The design's gates name
// `docker compose logs ctx`; E2-5 forbids a deploy, so every probe is a
// testcontainer instead and the live-log half stays the deploy-bound remainder.
//
// Every subtest is "erst rot": the arm-wide red is recorded in the wave report
// (a running scheduler with material and open gates left distill_run empty and
// the tree carried no distiller symbol at all), and each probe below carries its
// own red in-test where the contrast is a state rather than a missing symbol —
// the startup sweep asserts the derivation is 0 BEFORE it runs, the state-change
// rule asserts ten rows on the always-write path against one on the rule path.
//
// Run with:
//
//	go test -tags=integration ./internal/events/ -run TestDistillArm -count=1 -v
package events

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	dfScope    = "private"
	dfCategory = "compaction-checkpoints"
	dfLabel    = "ctx-checkpoint"
	dfRoot     = "20260712_205012_837f2c"
)

// Below: the steerable source.

// fakeDistillSource answers the three calls this wave makes. It exists because
// three gates ask for source behaviour a real database cannot be told to
// produce: a query that fails, a head that fell below the stored watermark, and
// an error whose text carries a credential.
type fakeDistillSource struct {
	sessions []distillsource.Ref
	head     map[string]int64
	hasNew   map[string]bool
	err      error  // returned by the call named in errOn
	errOn    string // "sessions" | "head" | "hasnew"
	closed   bool
	reads    int // Sessions/Head/HasNew calls, for the "null read queries" probe

	// readFn steers Read (A02-6). nil means "no material": an empty, complete
	// batch at the caller's own watermark, which is the answer that ends a
	// batch loop without pretending to have covered anything.
	readFn    func(after int64) (distillsource.Batch, error)
	lastAfter int64 // the watermark the last Read was asked from
}

func (f *fakeDistillSource) Label() string { return dfLabel }

func (f *fakeDistillSource) Sessions(context.Context) ([]distillsource.Ref, error) {
	f.reads++
	if f.errOn == "sessions" {
		return nil, f.err
	}
	return f.sessions, nil
}

func (f *fakeDistillSource) HasNew(_ context.Context, sess string, _ int64) (bool, error) {
	f.reads++
	if f.errOn == "hasnew" {
		return false, f.err
	}
	v, ok := f.hasNew[sess]
	return ok && v, nil
}

func (f *fakeDistillSource) Head(_ context.Context, sess string) (int64, error) {
	f.reads++
	if f.errOn == "head" {
		return 0, f.err
	}
	return f.head[sess], nil
}

func (f *fakeDistillSource) Read(_ context.Context, _ string, after int64, _, _ int) (distillsource.Batch, error) {
	f.lastAfter = after
	if f.readFn == nil {
		return distillsource.Batch{Watermark: after, Complete: true}, nil
	}
	return f.readFn(after)
}

func (f *fakeDistillSource) QuietFor(context.Context, string, time.Time) (time.Duration, error) {
	return 0, distillsource.ErrNoActiveRows
}

func (f *fakeDistillSource) Close() error { f.closed = true; return nil }

// Below: the harness.

func dfConfig() *config.Config {
	c := &config.Config{}
	c.Distill.Enabled = true
	c.Distill.CtxEnabled = true
	c.Distill.CtxSourceLabel = dfLabel
	c.Distill.CheckpointCategory = dfCategory
	c.Distill.MaxSessionsPerRun = 4
	c.Distill.RowsPerRead = 400
	// The two sizing keys of the selection stage, at their registry defaults.
	// They are set EXPLICITLY because the arm takes them as configured: the
	// clamp that used to substitute a default for a non-positive value is gone
	// (review #4 — config.validateDistillCounters is the one authority, and it
	// refuses a non-positive value with SeverityError), so a test config that
	// leaves them at the Go zero value would be a config the daemon refuses to
	// start with.
	c.Distill.MaxRowRunes = 4000
	c.Distill.MinRowRunes = 200
	c.Distill.CtxSessionHorizon = 30 * 24 * time.Hour
	c.Distill.Interval = time.Second
	// The write side (A02-9), at its registry defaults. EXPLICIT for the reason
	// the two sizing keys above are: the arm refuses an incomplete write
	// identity rather than writing an untyped block, and a test config that left
	// them at the Go zero value would be a config the daemon refuses to start
	// with (config.validateDistill*, 422 on an empty category or block type).
	c.Distill.Category = "session-insights"
	c.Distill.BlockType = "insight"
	c.Distill.BlockSensitivity = backends.SensCredentials
	c.Distill.MaxBlockRunes = 6000
	c.Scheduler.HomeScope = dfScope
	return c
}

func dfScheduler(pool *pgxpool.Pool, cfg *config.Config, src distillsource.Source) *Scheduler {
	s := NewScheduler(pool, config.NewStore(cfg), backends.NewPool(nil, nil), StartupConfig{})
	s.SetBlocktypeRegistry(blocktype.NewRegistry())
	if src != nil {
		s.distillSource = func(*config.Config, string) (distillsource.Source, error) { return src, nil }
	}
	return s
}

func dfNoDemand() int { return 0 }

func dfTruncate(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `TRUNCATE distill_run`); err != nil {
		t.Fatalf("truncate distill_run: %v", err)
	}
}

type dfRow struct {
	sourceKey  string
	root       string
	outcome    string
	skipReason string
	errClass   string
	from, to   int64
	finished   bool
}

func dfRows(t *testing.T, pool *pgxpool.Pool) []dfRow {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT source_key, COALESCE(root_session_id, ''), outcome,
		       COALESCE(skip_reason, ''), COALESCE(error, ''),
		       watermark_from, watermark_to, finished_at IS NOT NULL
		  FROM distill_run ORDER BY started_at, run_id`)
	if err != nil {
		t.Fatalf("select distill_run: %v", err)
	}
	defer rows.Close()
	var out []dfRow
	for rows.Next() {
		var r dfRow
		if err := rows.Scan(&r.sourceKey, &r.root, &r.outcome, &r.skipReason,
			&r.errClass, &r.from, &r.to, &r.finished); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

func dfSeedJournal(t *testing.T, pool *pgxpool.Pool, key string, to int64) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), `
		INSERT INTO distill_run (source_key, outcome, watermark_from, watermark_to, finished_at)
		VALUES ($1, 'ok', $2, $2, now())`, key, to); err != nil {
		t.Fatalf("seed journal row: %v", err)
	}
}

// dfSeedCheckpoint writes one manifest with one part and returns the manifest's
// microsecond watermark plus the two block ids.
func dfSeedCheckpoint(t *testing.T, pool *pgxpool.Pool, root string, at time.Time) (int64, string, string) {
	t.Helper()
	ctx := context.Background()
	body := "# Compaction checkpoint " + root + " part 1\n\n" +
		"## Compaction source evidence\n\n" +
		"- Transcript SHA-256: 6f1c2d3e4a5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9\n" +
		"- Source blocks: 1\n\n" +
		"## Direct transcript\n\n" +
		"### Message 1 — user\nthe material this arm will one day distil"
	var partID string
	if err := pool.QueryRow(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, $3, $4, 'checkpoint',
        jsonb_build_object('root_session_id', $5::text, 'part', '1'), $6)
RETURNING id::text`, dfCategory, fmt.Sprintf("%s-part-1-%d", root, at.UnixMicro()), body, dfScope, root, at).Scan(&partID); err != nil {
		t.Fatalf("insert part: %v", err)
	}
	var manifestID string
	if err := pool.QueryRow(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, 'manifest', $3, 'checkpoint',
        jsonb_build_object('root_session_id', $4::text,
                           'source_block_ids', to_jsonb(ARRAY[$5]::text[])), $6)
RETURNING id::text`, dfCategory, fmt.Sprintf("%s-manifest-%d", root, at.UnixMicro()), dfScope, root, partID, at).Scan(&manifestID); err != nil {
		t.Fatalf("insert manifest: %v", err)
	}
	return at.UnixMicro(), manifestID, partID
}

// Below: the gate.

func TestDistillArm(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	sessionKey := distillSourceKey(dfLabel, dfScope, dfRoot)

	// GREEN, production path: the REAL reader over seeded checkpoint material.
	// Everything below steers a fake; this subtest is the one that proves the
	// wiring the fake stands in for.
	//
	// A02-6 MOVED THIS ASSERTION, and the move is the wave, not a weakening:
	// while the arm processed nothing, the truthful row was 'partial' at 0..0
	// ("covered material, did not finish it"). With the selection stage in
	// place the run walks its range to the end, so the row is 'ok' and the
	// watermark stands on the manifest it covered. Everything else this probe
	// held — one row, the three-part identity, finished_at, no skip and no
	// error class — is unchanged.
	t.Run("RealReaderCoversItsRange", func(t *testing.T) {
		dfTruncate(t, pool)
		wm, _, _ := dfSeedCheckpoint(t, pool, dfRoot, time.Now().Add(-2*time.Hour))

		s := dfScheduler(pool, dfConfig(), nil) // nil ⇒ ctxcheckpoint.New over the pool
		if !s.distillOnce(ctx, dfNoDemand) {
			t.Fatal("distillOnce reported no per-session work although material exists")
		}
		rows := dfRows(t, pool)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
		}
		r := rows[0]
		if r.sourceKey != sessionKey || r.root != dfRoot {
			t.Fatalf("identity = %q / %q, want %q / %q", r.sourceKey, r.root, sessionKey, dfRoot)
		}
		if r.outcome != "ok" || r.skipReason != "" || r.errClass != "" {
			t.Fatalf("outcome/skip/error = %q/%q/%q, want ok//", r.outcome, r.skipReason, r.errClass)
		}
		if !r.finished {
			t.Fatal("finished_at is NULL on a closed row — the two-phase update did not run")
		}
		if r.from != 0 || r.to != wm {
			t.Fatalf("watermark %d..%d, want 0..%d — the manifest was covered", r.from, r.to, wm)
		}
		if got := dfDerive(t, pool, sessionKey); got != wm {
			t.Fatalf("derived watermark = %d, want %d", got, wm)
		}
	})

	t.Run("DisabledWritesNoRow", func(t *testing.T) {
		for _, tc := range []struct {
			name                string
			enabled, ctxEnabled bool
			homeScope           string
		}{
			{"ctx_enabled=false", true, false, dfScope},
			{"enabled=false", false, true, dfScope},
			{"both off", false, false, dfScope},
			// THE ORDER of gate 0 against gate 5, not just gate 0 itself. Gate 5
			// writes every tick (always=true), so a disabled install whose scope
			// gate would refuse must still produce nothing: §4.5.3 says "Tor 0
			// schreibt NIE — eine ctx-Installation ohne hermes darf kein Journal
			// akkumulieren". With gate 0 behind gate 5 that install accumulates
			// a scope_forbidden row per tick, 96 a day, forever.
			{"disabled AND a refused scope", false, false, "shared"},
			{"ctx_enabled=false AND a refused scope", true, false, "shared"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dfTruncate(t, pool)
				cfg := dfConfig()
				cfg.Distill.Enabled = tc.enabled
				cfg.Distill.CtxEnabled = tc.ctxEnabled
				cfg.Scheduler.HomeScope = tc.homeScope
				src := &fakeDistillSource{}
				s := dfScheduler(pool, cfg, src)
				if s.distillOnce(ctx, dfNoDemand) {
					t.Fatal("a disabled arm reported work")
				}
				if rows := dfRows(t, pool); len(rows) != 0 {
					t.Fatalf("rows = %d, want 0 — gate 0 writes no journal row at all: %+v", len(rows), rows)
				}
				if src.reads != 0 {
					t.Fatalf("source was read %d times behind a closed master gate", src.reads)
				}
				// The stamp is the other half of "nothing happened": a disabled
				// arm has not reached its source, so it must not look like one
				// that did (review #4).
				if got := s.LastDistillRun(); !got.IsZero() {
					t.Fatalf("LastDistillRun = %v on a disabled arm, want the zero time", got)
				}
			})
		}
	})

	t.Run("NoNewRows", func(t *testing.T) {
		t.Run("head equals watermark", func(t *testing.T) {
			dfTruncate(t, pool)
			dfSeedJournal(t, pool, sessionKey, 5000)
			src := &fakeDistillSource{
				sessions: []distillsource.Ref{{Session: dfRoot}},
				head:     map[string]int64{dfRoot: 5000},
				hasNew:   map[string]bool{dfRoot: true}, // would pass, but Head answers first
			}
			dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
			dfWantSkip(t, pool, sessionKey, "no_new_rows", 5000, 1)
		})
		t.Run("head above watermark but nothing addressable", func(t *testing.T) {
			dfTruncate(t, pool)
			dfSeedJournal(t, pool, sessionKey, 5000)
			src := &fakeDistillSource{
				sessions: []distillsource.Ref{{Session: dfRoot}},
				head:     map[string]int64{dfRoot: 9000},
				hasNew:   map[string]bool{dfRoot: false},
			}
			dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
			dfWantSkip(t, pool, sessionKey, "no_new_rows", 5000, 1)
		})
	})

	t.Run("Demand", func(t *testing.T) {
		dfTruncate(t, pool)
		src := &fakeDistillSource{sessions: []distillsource.Ref{{Session: dfRoot}}}
		s := dfScheduler(pool, dfConfig(), src)
		if s.distillOnce(ctx, func() int { return 3 }) {
			t.Fatal("the arm ran under interactive demand")
		}
		tickKey := distillSourceKey(dfLabel, dfScope, "")
		dfWantSkip(t, pool, tickKey, "demand", 0, 1)
		if src.reads != 0 {
			t.Fatalf("source was read %d times although the demand gate closed", src.reads)
		}
	})

	// BA9's missing half: the INHERITED path resolving to shared. V22 refuses
	// only what is explicitly configured, and says so in its own comment.
	t.Run("ScopeForbidden", func(t *testing.T) {
		for _, tc := range []struct{ name, home, explicit, wantScope string }{
			{"inherited shared", "shared", "", "shared"},
			{"scope the operator does not own", dfScope, "fremd", "fremd"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dfTruncate(t, pool)
				cfg := dfConfig()
				cfg.Scheduler.HomeScope = tc.home
				cfg.Distill.Scope = tc.explicit
				s := dfScheduler(pool, cfg, nil)
				// Gate 5 must refuse BEFORE the reader exists: a builder that
				// fails the test is the strongest form of "null read queries".
				s.distillSource = func(*config.Config, string) (distillsource.Source, error) {
					t.Error("the source was constructed although gate 5 refused the scope")
					return &fakeDistillSource{}, nil
				}
				if s.distillOnce(ctx, dfNoDemand) {
					t.Fatal("the arm ran on a refused scope")
				}
				tickKey := distillSourceKey(dfLabel, tc.wantScope, "")
				dfWantSkip(t, pool, tickKey, "scope_forbidden", 0, 1)
				if rows := dfRows(t, pool); rows[0].root != "" {
					t.Fatalf("root_session_id = %q on a tick-level row, want NULL", rows[0].root)
				}
			})
		}
	})

	// §4.5.3: gates 1–4 write only on a CHANGED reason, gates 5–7 write every
	// tick. Both halves through distillOnce, so the contrast is the rule and not
	// a flag a test set.
	t.Run("StateChangeRule", func(t *testing.T) {
		t.Run("RED — an always-write gate produces ten rows", func(t *testing.T) {
			dfTruncate(t, pool)
			cfg := dfConfig()
			cfg.Scheduler.HomeScope = "shared" // gate 5: always
			s := dfScheduler(pool, cfg, nil)
			for i := 0; i < 10; i++ {
				s.distillOnce(ctx, dfNoDemand)
			}
			if rows := dfRows(t, pool); len(rows) != 10 {
				t.Fatalf("rows = %d, want 10 — gates 5–7 write every tick", len(rows))
			}
		})
		t.Run("GREEN — a state-change gate produces one", func(t *testing.T) {
			dfTruncate(t, pool)
			dfSeedJournal(t, pool, sessionKey, 5000)
			src := &fakeDistillSource{
				sessions: []distillsource.Ref{{Session: dfRoot}},
				head:     map[string]int64{dfRoot: 5000},
			}
			s := dfScheduler(pool, dfConfig(), src)
			for i := 0; i < 10; i++ {
				s.distillOnce(ctx, dfNoDemand)
			}
			// One seeded 'ok' row plus exactly ONE no_new_rows row.
			rows := dfRows(t, pool)
			skips := 0
			for _, r := range rows {
				if r.outcome == "skipped" && r.skipReason == "no_new_rows" {
					skips++
				}
			}
			if skips != 1 {
				t.Fatalf("no_new_rows rows = %d over ten ticks, want 1: %+v", skips, rows)
			}
		})
		t.Run("a CHANGED reason writes again", func(t *testing.T) {
			dfTruncate(t, pool)
			dfSeedJournal(t, pool, sessionKey, 5000)
			src := &fakeDistillSource{
				sessions: []distillsource.Ref{{Session: dfRoot}},
				head:     map[string]int64{dfRoot: 5000},
			}
			s := dfScheduler(pool, dfConfig(), src)
			s.distillOnce(ctx, dfNoDemand) // no_new_rows
			src.errOn, src.err = "head", fmt.Errorf("%w: unreachable", distillsource.ErrSourceUnavailable)
			s.distillOnce(ctx, dfNoDemand) // source_unreachable — a new reason
			s.distillOnce(ctx, dfNoDemand) // same reason again — suppressed
			var reasons []string
			for _, r := range dfRows(t, pool) {
				if r.outcome == "skipped" {
					reasons = append(reasons, r.skipReason)
				}
			}
			if strings.Join(reasons, ",") != "no_new_rows,source_unreachable" {
				t.Fatalf("skip series = %v, want [no_new_rows source_unreachable]", reasons)
			}
		})
	})

	t.Run("SkipInvariance", func(t *testing.T) {
		dfTruncate(t, pool)
		dfSeedJournal(t, pool, sessionKey, 7777)
		before := dfDerive(t, pool, sessionKey)
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: dfRoot}},
			head:     map[string]int64{dfRoot: 7777},
		}
		dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
		after := dfDerive(t, pool, sessionKey)
		if before != after || after != 7777 {
			t.Fatalf("derivation moved across a skip tick: %d -> %d (want 7777 both)", before, after)
		}
		// The derivation alone is a ONE-SIDED probe: max() cannot see a skip row
		// written as 0..0, so a mutation to zero leaves this subtest green while
		// breaking 135:43-46 ("nie 0, nie NULL"). It matters after the A02-12
		// retention, where a surviving 0..0 row would be the last of its series
		// and the derivation would fall to 0 — the whole range re-processed. So
		// the ROW is asserted here too, not only the aggregate.
		var seen int
		for _, r := range dfRows(t, pool) {
			if r.sourceKey != sessionKey || r.outcome != "skipped" {
				continue
			}
			seen++
			if r.from != 7777 || r.to != 7777 {
				t.Fatalf("skip row is %d..%d, want 7777..7777 — a skip carries the DERIVED mark, never 0", r.from, r.to)
			}
		}
		if seen != 1 {
			t.Fatalf("skip rows = %d, want 1", seen)
		}
	})

	// §4.5.5. The red is in the test: the derivation is asked BEFORE the sweep
	// and must answer 0, because the orphan's watermark_to is invisible to
	// "outcome <> running".
	t.Run("StartupSweep", func(t *testing.T) {
		dfTruncate(t, pool)
		if _, err := pool.Exec(ctx, `
			INSERT INTO distill_run (source_key, root_session_id, outcome, watermark_from, watermark_to)
			VALUES ($1, $2, 'running', 0, 500)`, sessionKey, dfRoot); err != nil {
			t.Fatalf("insert orphan: %v", err)
		}
		if got := dfDerive(t, pool, sessionKey); got != 0 {
			t.Fatalf("RED precondition broken: derivation = %d before the sweep, want 0", got)
		}

		dfScheduler(pool, dfConfig(), nil).distillStartupSweep(ctx)

		if got := dfDerive(t, pool, sessionKey); got != 500 {
			t.Fatalf("derivation = %d after the sweep, want 500", got)
		}
		rows := dfRows(t, pool)
		if len(rows) != 1 || rows[0].outcome != "killed" || rows[0].errClass != "daemon_restart" || !rows[0].finished {
			t.Fatalf("swept row = %+v, want one killed/daemon_restart row with finished_at", rows)
		}
	})

	// §4.5.3 last row. Archiving is the real path: Head filters NOT is_archived,
	// so an archived manifest drops its range out of the head.
	t.Run("WatermarkRegression", func(t *testing.T) {
		t.Run("real reader, newest manifest archived", func(t *testing.T) {
			dfTruncate(t, pool)
			regRoot := "20260801_120000_regres"
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(),
					`DELETE FROM context_blocks WHERE metadata->>'root_session_id' = $1`, regRoot)
			})
			// TWO manifests. Archiving only the newer one is what the gate's
			// precondition needs: the root stays a candidate through its
			// surviving manifest, and its head falls BELOW the stored mark.
			oldWM, _, _ := dfSeedCheckpoint(t, pool, regRoot, time.Now().Add(-5*time.Hour))
			newWM, newManifest, _ := dfSeedCheckpoint(t, pool, regRoot, time.Now().Add(-3*time.Hour))
			regKey := distillSourceKey(dfLabel, dfScope, regRoot)
			dfSeedJournal(t, pool, regKey, newWM)

			if _, err := pool.Exec(ctx,
				`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, newManifest); err != nil {
				t.Fatalf("archive manifest: %v", err)
			}

			dfScheduler(pool, dfConfig(), nil).distillOnce(ctx, dfNoDemand)

			var got dfRow
			var seen int
			for _, r := range dfRows(t, pool) {
				if r.sourceKey != regKey {
					continue // other roots of the shared corpus are not this probe
				}
				if r.outcome == "running" || r.outcome == "partial" {
					t.Fatalf("the arm processed a regressed source: %+v", r)
				}
				if r.outcome == "skipped" {
					got, seen = r, seen+1
				}
			}
			if seen != 1 || got.skipReason != "watermark_regression" {
				t.Fatalf("skip rows = %d (%q), want 1 watermark_regression (head fell to %d, mark is %d)",
					seen, got.skipReason, oldWM, newWM)
			}
			if got.from != newWM || got.to != newWM {
				t.Fatalf("skip row watermark %d..%d, want %d..%d — a skip is invariant", got.from, got.to, newWM, newWM)
			}
			if after := dfDerive(t, pool, regKey); after != newWM {
				t.Fatalf("derivation moved to %d, want %d unchanged", after, newWM)
			}
		})
		// The BOUNDARY of that detection, held as an assertion rather than left
		// as a surprise: Sessions filters NOT is_archived AND
		// metadata ? 'source_block_ids', so a root whose manifests are archived
		// WITHOUT EXCEPTION leaves the candidate list — the arm never asks Head
		// and journals nothing. That is not a fail-open (nothing is read,
		// nothing is written, the watermark stands), but it is the one
		// regression shape the arm cannot report. Wave note NB-1.
		t.Run("every manifest archived leaves the candidate list unseen", func(t *testing.T) {
			dfTruncate(t, pool)
			goneRoot := "20260801_140000_allgone"
			t.Cleanup(func() {
				_, _ = pool.Exec(context.Background(),
					`DELETE FROM context_blocks WHERE metadata->>'root_session_id' = $1`, goneRoot)
			})
			wm, manifestID, _ := dfSeedCheckpoint(t, pool, goneRoot, time.Now().Add(-4*time.Hour))
			goneKey := distillSourceKey(dfLabel, dfScope, goneRoot)
			dfSeedJournal(t, pool, goneKey, wm)
			if _, err := pool.Exec(ctx,
				`UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, manifestID); err != nil {
				t.Fatalf("archive manifest: %v", err)
			}

			dfScheduler(pool, dfConfig(), nil).distillOnce(ctx, dfNoDemand)

			for _, r := range dfRows(t, pool) {
				if r.sourceKey == goneKey && r.outcome != "ok" {
					t.Fatalf("unexpected row for a root outside the candidate list: %+v", r)
				}
			}
			if after := dfDerive(t, pool, goneKey); after != wm {
				t.Fatalf("derivation moved to %d, want %d — nothing was read or written", after, wm)
			}
		})
		t.Run("steered head below the watermark", func(t *testing.T) {
			dfTruncate(t, pool)
			dfSeedJournal(t, pool, sessionKey, 1000)
			src := &fakeDistillSource{
				sessions: []distillsource.Ref{{Session: dfRoot}},
				head:     map[string]int64{dfRoot: 500},
				hasNew:   map[string]bool{dfRoot: true},
			}
			dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
			dfWantSkip(t, pool, sessionKey, "watermark_regression", 1000, 1)
		})
	})

	// The AKIA probe (§7.2). distill_run.error carries a class string and
	// nothing else — the journal is readable over /api and lives 90 days.
	t.Run("ErrorTaxonomy", func(t *testing.T) {
		const secret = "AKIAIOSFODNN7EXAMPLE"
		for _, tc := range []struct {
			name        string
			err         error
			wantOutcome string
			wantErr     string
			wantSkip    string
		}{
			{
				name:        "query_failed carries only the class",
				err:         fmt.Errorf("%w: pgx: row 42 contains %s", distillsource.ErrQueryFailed, secret),
				wantOutcome: "failed", wantErr: "query_failed",
			},
			{
				name:        "schema_untrusted carries only the class",
				err:         fmt.Errorf("%w: root id %s is not a uuid", distillsource.ErrSchemaUntrusted, secret),
				wantOutcome: "failed", wantErr: "schema_untrusted",
			},
			{
				// The contract's sentinel that is NOT an error class: the
				// journal's CHECK has no source_unavailable, so a pass-through
				// would fail the INSERT outright.
				name:        "source_unavailable becomes a skip, never an error class",
				err:         fmt.Errorf("%w: pool gone, dsn %s", distillsource.ErrSourceUnavailable, secret),
				wantOutcome: "skipped", wantSkip: "source_unreachable",
			},
			{
				name:        "an unclassified error still lands as query_failed",
				err:         fmt.Errorf("something nobody named, %s", secret),
				wantOutcome: "failed", wantErr: "query_failed",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dfTruncate(t, pool)
				src := &fakeDistillSource{
					sessions: []distillsource.Ref{{Session: dfRoot}},
					errOn:    "head",
					err:      tc.err,
				}
				dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
				rows := dfRows(t, pool)
				if len(rows) != 1 {
					t.Fatalf("rows = %d, want 1: %+v", len(rows), rows)
				}
				r := rows[0]
				if r.outcome != tc.wantOutcome || r.errClass != tc.wantErr || r.skipReason != tc.wantSkip {
					t.Fatalf("row = %s/%s/%s, want %s/%s/%s",
						r.outcome, r.errClass, r.skipReason, tc.wantOutcome, tc.wantErr, tc.wantSkip)
				}
				// Nothing of the raw text reached ANY column of the row.
				var dump string
				if err := pool.QueryRow(ctx, `SELECT to_jsonb(d)::text FROM distill_run d`).Scan(&dump); err != nil {
					t.Fatalf("dump row: %v", err)
				}
				if strings.Contains(dump, secret) || strings.Contains(dump, "AKIA") {
					t.Fatalf("foreign text reached the journal: %s", dump)
				}
			})
		}
	})

	// A02-3 review #6: an unreadable root must not stall the tick. The arm skips
	// that root and keeps going.
	t.Run("BadRootDoesNotStopTheTick", func(t *testing.T) {
		dfTruncate(t, pool)
		good := "20260801_130000_healthy"
		goodKey := distillSourceKey(dfLabel, dfScope, good)
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: dfRoot}, {Session: good}},
			head:     map[string]int64{dfRoot: 0, good: 900},
			hasNew:   map[string]bool{good: true},
		}
		// Only the first root fails: errOn is global, so instead the first root
		// answers head 0 == watermark 0 (no_new_rows) and the SECOND must still
		// reach the two-phase path — and, since A02-6, walk it to the end.
		src.readFn = func(after int64) (distillsource.Batch, error) {
			if after >= 900 {
				return distillsource.Batch{Watermark: after, Complete: true}, nil
			}
			return distillsource.Batch{
				Items: []distillsource.Item{{
					Text:   strings.Repeat("material ", 40),
					Origin: distillsource.Origin{BlockID: "good-part", ChunkIndex: 1},
				}},
				Watermark: 900,
				Complete:  true,
			}, nil
		}
		dfScheduler(pool, dfConfig(), src).distillOnce(ctx, dfNoDemand)
		var done int
		for _, r := range dfRows(t, pool) {
			if r.sourceKey == goodKey && r.outcome == "ok" {
				done++
				if r.to != 900 {
					t.Fatalf("the healthy root closed at watermark %d, want 900", r.to)
				}
			}
		}
		if done != 1 {
			t.Fatalf("second root produced %d ok rows, want 1: %+v", done, dfRows(t, pool))
		}
	})

	// No candidate root at all: nothing to key a row on, so nothing is written.
	// The counter-state to "empty range ⇒ no_new_rows" above.
	t.Run("NoCandidateSessionsWritesNoRow", func(t *testing.T) {
		dfTruncate(t, pool)
		src := &fakeDistillSource{}
		s := dfScheduler(pool, dfConfig(), src)
		if s.distillOnce(ctx, dfNoDemand) {
			t.Fatal("the arm claimed per-session work with no candidates")
		}
		if rows := dfRows(t, pool); len(rows) != 0 {
			t.Fatalf("rows = %d, want 0: %+v", len(rows), rows)
		}
		// The source is built per tick, so every exit path has to release it.
		if !src.closed {
			t.Fatal("the source was not closed — a per-tick reader that leaks its handle leaks one per tick")
		}
		// THE OBSERVABLE SURFACE of this state (review #4). The arm is enabled,
		// the scope resolved, the source reached — and it writes no row on
		// purpose. Without the stamp that is byte-identical, from outside the
		// process, to an arm that never ran.
		if s.LastDistillRun().IsZero() {
			t.Fatal("LastDistillRun is the zero time although the tick reached its source")
		}
	})

	// Review #1, the S13 state: a manifest whose metadata.root_session_id is the
	// EMPTY STRING. Sessions only filters IS NOT NULL, and the value is writable
	// over the public store path, so the corpus can hand the arm a root it cannot
	// name. Keyed naively that root's per-session row lands on "ctx-checkpoint:
	// private:" — the TICK key, whose series the gates before the candidate list
	// share. Harmless while every row is 0..0; from A02-6 it is two sources on
	// one watermark series, plus a shared distill_seen PK.
	t.Run("EmptyRootSessionIsRefused", func(t *testing.T) {
		dfTruncate(t, pool)
		emptyRootAt := time.Now().Add(-90 * time.Minute)
		t.Cleanup(func() {
			_, _ = pool.Exec(context.Background(),
				`DELETE FROM context_blocks WHERE metadata->>'root_session_id' = ''`)
		})
		dfSeedCheckpoint(t, pool, "", emptyRootAt)

		tickKey := distillSourceKey(dfLabel, dfScope, "")
		dfScheduler(pool, dfConfig(), nil).distillOnce(ctx, dfNoDemand) // REAL reader

		var failed int
		for _, r := range dfRows(t, pool) {
			if r.sourceKey != tickKey {
				continue
			}
			// The collision the review measured: a per-session row under the
			// tick key. Any processing outcome here is the bug.
			if r.outcome == "partial" || r.outcome == "running" {
				t.Fatalf("an unnameable root produced a per-session row under the tick key: %+v", r)
			}
			if r.outcome == "failed" {
				failed++
				if r.errClass != "schema_untrusted" {
					t.Fatalf("error class = %q, want schema_untrusted", r.errClass)
				}
				if r.root != "" {
					t.Fatalf("root_session_id = %q on a tick-level row, want NULL", r.root)
				}
				if r.from != 0 || r.to != 0 {
					t.Fatalf("tick-key row is %d..%d, want 0..0 — the tick series never advances", r.from, r.to)
				}
			}
		}
		if failed != 1 {
			t.Fatalf("schema_untrusted rows = %d, want exactly 1: %+v", failed, dfRows(t, pool))
		}
		// And the tick key's derivation stays 0, so no gate answer inherits a
		// watermark from a root that has no identity.
		if got := dfDerive(t, pool, tickKey); got != 0 {
			t.Fatalf("tick-key derivation = %d, want 0", got)
		}
	})

	// Review #5. owned == nil is "the entitlement set is unknown", not "no
	// restriction": ListTenants failed, or no active default tenant leaves the
	// register without a _global entry. The config-side V30 that would catch an
	// explicitly set foreign scope is still unbuilt, so a passthrough here would
	// let one through BOTH halves of the guard.
	t.Run("UnresolvedEntitlementsFailClosed", func(t *testing.T) {
		for _, tc := range []struct {
			name    string
			tenants []backgroundTenant
		}{
			{"register unreachable (owned nil on _global)", []backgroundTenant{{scope: store.GlobalScope, owned: nil}}},
			{"no active default tenant (no _global entry)", []backgroundTenant{{scope: "andere", owned: []string{"andere"}}}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				dfTruncate(t, pool)
				s := dfScheduler(pool, dfConfig(), nil)
				s.backgroundTenantsFn = func(context.Context) []backgroundTenant { return tc.tenants }
				s.distillSource = func(*config.Config, string) (distillsource.Source, error) {
					t.Error("the source was constructed although the entitlement set was unresolved")
					return &fakeDistillSource{}, nil
				}
				if s.distillOnce(ctx, dfNoDemand) {
					t.Fatal("the arm ran without a verified entitlement set")
				}
				dfWantSkip(t, pool, distillSourceKey(dfLabel, dfScope, ""), "scope_forbidden", 0, 1)
				if got := s.LastDistillRun(); !got.IsZero() {
					t.Fatalf("LastDistillRun = %v although gate 5 stopped the tick", got)
				}
			})
		}
	})

	// Review #7, decided by extension rather than by prose: a failure obeys the
	// same state-change rule as a skip. Without it the SAME reader error journals
	// at two rates depending only on which sentinel it wrapped.
	t.Run("FailedRowsObeyTheStateChangeRule", func(t *testing.T) {
		dfTruncate(t, pool)
		src := &fakeDistillSource{
			sessions: []distillsource.Ref{{Session: dfRoot}},
			errOn:    "head",
			err:      fmt.Errorf("%w: the same broken query, every tick", distillsource.ErrQueryFailed),
		}
		s := dfScheduler(pool, dfConfig(), src)
		for i := 0; i < 10; i++ {
			s.distillOnce(ctx, dfNoDemand)
		}
		var failed int
		for _, r := range dfRows(t, pool) {
			if r.outcome == "failed" && r.errClass == "query_failed" {
				failed++
			}
		}
		if failed != 1 {
			t.Fatalf("query_failed rows = %d over ten identical failures, want 1", failed)
		}
		// A CHANGED class writes again — the rule throttles repetition, not
		// diagnosis.
		src.err = fmt.Errorf("%w: a different shape", distillsource.ErrSchemaUntrusted)
		s.distillOnce(ctx, dfNoDemand)
		var classes []string
		for _, r := range dfRows(t, pool) {
			if r.outcome == "failed" {
				classes = append(classes, r.errClass)
			}
		}
		if strings.Join(classes, ",") != "query_failed,schema_untrusted" {
			t.Fatalf("failure series = %v, want [query_failed schema_untrusted]", classes)
		}
	})
}

// dfDerive is THE watermark derivation, spelled out in the test rather than
// called through the arm: a gate that used the production helper could not see
// the helper drift.
func dfDerive(t *testing.T, pool *pgxpool.Pool, key string) int64 {
	t.Helper()
	var wm int64
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(max(watermark_to), 0)
		  FROM distill_run
		 WHERE source_key = $1 AND outcome <> 'running'`, key).Scan(&wm); err != nil {
		t.Fatalf("derive watermark: %v", err)
	}
	return wm
}

func dfWantSkip(t *testing.T, pool *pgxpool.Pool, key, reason string, wm int64, want int) {
	t.Helper()
	var seen int
	for _, r := range dfRows(t, pool) {
		if r.sourceKey != key || r.outcome != "skipped" {
			continue
		}
		seen++
		if r.skipReason != reason {
			t.Fatalf("skip_reason = %q, want %q", r.skipReason, reason)
		}
		if r.from != wm || r.to != wm {
			t.Fatalf("skip watermark %d..%d, want %d..%d (a skip is invariant)", r.from, r.to, wm, wm)
		}
		if !r.finished {
			t.Fatal("a skip row without finished_at would violate dr_finished_iff_done")
		}
	}
	if seen != want {
		t.Fatalf("skip rows for %q = %d, want %d: %+v", key, seen, want, dfRows(t, pool))
	}
}
