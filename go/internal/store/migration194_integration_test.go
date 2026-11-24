//go:build integration

package store_test

// Migration 194 (issue #277): context_pending_writes drops its foreign keys on
// context_api_keys and context_principals. The file drops them inline below
// 250 chunks (and a lock table that holds twice the locks the drop takes),
// above it warns and takes no lock on the two tables; a second application
// and an out-of-band drop make it a no-op; lock_timeout bounds its wait for a
// transaction that read one of the tables. Every run beside two connections
// that authenticate through ctx_auth in a loop, a sampler of pg_locks and the
// stages written before it.
//
//	go test -tags=integration ./internal/store/ -run TestMigration194 -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const migration194File = "194_pending_writes_drop_auth_keys.sql"

// m194Keys lists the foreign keys of context_pending_writes.
func m194Keys(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	var keys []string
	if err := pool.QueryRow(context.Background(), `
		SELECT COALESCE(array_agg(conname::text || ' ' || pg_get_constraintdef(oid) ORDER BY conname), '{}')
		  FROM pg_constraint WHERE contype = 'f' AND conrelid = 'context_pending_writes'::regclass`).Scan(&keys); err != nil {
		t.Fatal(err)
	}
	return keys
}

// m194Seed writes keys API keys (store.CreateApiKey) and a stage of each key
// in chunks distinct past hours (one chunk each; no write path stamps a past
// created_at) plus one current stage per key (store.StagePendingWrite). It
// returns the plaintexts of the first two keys and a fingerprint of the
// stages. The fixture holds exactly chunks chunks: it starts with at least
// a minute of the current hour left, so the current stages share one chunk,
// and the past ones take the hours before it.
func m194Seed(t *testing.T, pool *pgxpool.Pool, keys, chunks int) ([]string, string) {
	t.Helper()
	ctx := context.Background()
	if left := time.Until(time.Now().Truncate(time.Hour).Add(time.Hour)); left < time.Minute {
		time.Sleep(left + time.Second)
	}
	var ids, plains []string
	for i := 0; i < keys; i++ {
		k, p, err := store.CreateApiKey(ctx, pool, fmt.Sprintf("m194-%d", i), "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, k.ID)
		plains = append(plains, p)
		if _, err := store.StagePendingWrite(ctx, pool, k.ID, "private", "store", "mcp", []byte(`{"op":"store"}`), "now", 10*time.Minute); err != nil {
			t.Fatal(err)
		}
	}
	if chunks > 1 {
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_pending_writes (api_key_id, scope, op, origin, payload, payload_hash, created_at, principal_id)
			SELECT k.id, 'private', 'store', 'mcp', '{"op":"store"}', 'h' || g, date_trunc('hour', now()) - make_interval(hours => g) + interval '30 minutes', k.principal_id
			  FROM context_api_keys k, generate_series(1, $2::int - 1) g
			 WHERE k.id = ANY($1::uuid[])`, ids, chunks); err != nil {
			t.Fatalf("seed aged stages: %v", err)
		}
	}
	var got int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM timescaledb_information.chunks WHERE hypertable_name = 'context_pending_writes'`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != chunks {
		t.Fatalf("fixture: %d chunks, want %d", got, chunks)
	}
	return plains[:2], m194Fingerprint(t, pool)
}

// m194Boundary reports whether context_guard_state carries part 2's column.
func m194Boundary(t *testing.T, pool *pgxpool.Pool) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM information_schema.columns
		WHERE table_schema = current_schema() AND table_name = 'context_guard_state' AND column_name = 'write_log_evicted_before'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

func m194Fingerprint(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::text || ':' || md5(string_agg(id::text || api_key_id::text || payload_hash, ',' ORDER BY id)) FROM context_pending_writes WHERE payload_hash NOT LIKE 'm194-side-%'`).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

type m194Run struct {
	took     time.Duration
	err      error
	warnings []string
	// authMax is the longest ctx_auth call of the two loops while the file
	// ran; authCalls how many they made.
	authMax   time.Duration
	authCalls int64
	// stageMax is the longest StagePendingWrite of a stager beside it.
	stageMax   atomic.Int64
	stageCalls int64
	// locked names the modes the file's backend held on context_api_keys and
	// context_principals, as a 2 ms sampler of pg_locks saw them.
	locked map[string]bool
}

// m194Apply applies body in one transaction, as the runner does, beside two
// connections that authenticate plains in a loop and a pg_locks sampler.
func m194Apply(t *testing.T, dsn string, body []byte, plains []string) *m194Run {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	run := &m194Run{locked: map[string]bool{}}
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if n.Severity == "WARNING" {
			mu.Lock()
			run.warnings = append(run.warnings, n.Message)
			mu.Unlock()
		}
	}
	mig, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer mig.Close()
	side, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer side.Close()

	tx, err := mig.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int64
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}

	var stop atomic.Bool
	var maxNs atomic.Int64
	var wg sync.WaitGroup
	for _, p := range plains {
		wg.Add(1)
		go func(p string) {
			defer wg.Done()
			for !stop.Load() {
				st := time.Now()
				var v bool
				if err := side.QueryRow(ctx, `SELECT is_valid FROM ctx_auth($1)`, p).Scan(&v); err != nil || !v {
					t.Errorf("ctx_auth beside migration 194: valid=%v err=%v", v, err)
					return
				}
				d := time.Since(st).Nanoseconds()
				atomic.AddInt64(&run.authCalls, 1)
				for m := maxNs.Load(); d > m && !maxNs.CompareAndSwap(m, d); m = maxNs.Load() {
				}
			}
		}(p)
	}
	if len(plains) > 0 {
		// A stager beside the file: StagePendingWrite of a key of the store,
		// a new payload each time.
		var key string
		if err := side.QueryRow(ctx, `SELECT id::text FROM context_api_keys ORDER BY id LIMIT 1`).Scan(&key); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; !stop.Load(); i++ {
				st := time.Now()
				if _, err := store.StagePendingWrite(ctx, side, key, "private", "store", "mcp", []byte(`{"op":"store"}`), fmt.Sprintf("m194-side-%d", i), time.Minute); err != nil {
					t.Errorf("stage beside migration 194: %v", err)
					return
				}
				d := time.Since(st).Nanoseconds()
				atomic.AddInt64(&run.stageCalls, 1)
				for m := run.stageMax.Load(); d > m && !run.stageMax.CompareAndSwap(m, d); m = run.stageMax.Load() {
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for !stop.Load() {
			rows, err := side.Query(ctx, `SELECT l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				WHERE l.pid = $1 AND c.relname IN ('context_api_keys', 'context_principals')`, pid)
			if err == nil {
				for rows.Next() {
					var m string
					if rows.Scan(&m) == nil {
						mu.Lock()
						run.locked[m] = true
						mu.Unlock()
					}
				}
				rows.Close()
			}
			time.Sleep(2 * time.Millisecond)
		}
	}()
	time.Sleep(50 * time.Millisecond)
	start := time.Now()
	_, run.err = tx.Exec(ctx, string(body))
	if run.err == nil {
		run.err = tx.Commit(ctx)
	}
	run.took = time.Since(start)
	time.Sleep(20 * time.Millisecond)
	stop.Store(true)
	wg.Wait()
	run.authMax = time.Duration(maxNs.Load())
	return run
}

func TestMigration194(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(migration194File)
	if err != nil {
		t.Fatalf("read %s: %v", migration194File, err)
	}
	wantKeys := []string{
		"context_pending_writes_api_key_id_fkey FOREIGN KEY (api_key_id) REFERENCES context_api_keys(id) ON DELETE CASCADE",
		"context_pending_writes_principal_id_fkey FOREIGN KEY (principal_id) REFERENCES context_principals(id)",
	}

	// A store of the reference instance's size (11 000 blocks, 46 000
	// write-log rows) with the chunks the 24 h default of
	// writes.confirm_retention leaves, upgraded through the runner.
	t.Run("live_size_through_the_runner", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, 193)
		if got := m194Keys(t, pool); strings.Join(got, "|") != strings.Join(wantKeys, "|") {
			t.Fatalf("keys at 193: %v, want %v", got, wantKeys)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_blocks (category, title, content, scope)
			SELECT 'm194', 'm194 block ' || g, repeat('content ', 40) || g, 'private' FROM generate_series(1, 11000) g`); err != nil {
			t.Fatal(err)
		}
		wlInsert(t, pool, 46000, "3 days", nil)
		_, fp := m194Seed(t, pool, 20, 25)
		start := time.Now()
		if err := store.RunMigrationsUpTo(ctx, pool, 194); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		t.Logf("193 -> 194 through the runner at 25 chunks, 11 000 blocks, 46 000 write-log rows: %s", time.Since(start).Round(time.Millisecond))
		if got := m194Keys(t, pool); len(got) != 0 {
			t.Errorf("keys after 194: %v, want none", got)
		}
		if !m194Boundary(t, pool) {
			t.Errorf("context_guard_state.write_log_evicted_before missing after 194")
		}
		if got := m194Fingerprint(t, pool); got != fp {
			t.Errorf("stages changed: %s -> %s", fp, got)
		}
		if err := store.RunMigrations(ctx, pool); err != nil {
			t.Fatalf("second run: %v", err)
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
		plains, fp := m194Seed(t, pool, 20, 249)
		run := m194Apply(t, dsn, body, plains)
		t.Logf("194 at 249 chunks: %s; authentications %d, longest %s; stages %d, longest %s; locks %v", run.took.Round(time.Millisecond), run.authCalls, run.authMax.Round(time.Millisecond), run.stageCalls, time.Duration(run.stageMax.Load()).Round(time.Millisecond), run.locked)
		if run.err != nil || len(run.warnings) != 0 {
			t.Fatalf("err %v, warnings %v", run.err, run.warnings)
		}
		if got := m194Keys(t, pool); len(got) != 0 {
			t.Errorf("keys after 194: %v, want none", got)
		}
		if !run.locked["AccessExclusiveLock"] {
			// A diagnostic, not a gate: a 2 ms sampler can miss a lock the
			// drop takes and releases between two polls.
			t.Logf("the sampler did not see the drop's ACCESS EXCLUSIVE on the referenced tables (%v): the timings above may not cover the drop", run.locked)
		}
		if got := m194Fingerprint(t, pool); got != fp {
			t.Errorf("stages changed: %s -> %s", fp, got)
		}
		// Again: a no-op without a lock on the referenced tables.
		again := m194Apply(t, dsn, body, plains)
		if again.err != nil || len(again.warnings) != 0 || len(again.locked) != 0 {
			t.Errorf("second application: err %v, warnings %v, locks %v; want a no-op", again.err, again.warnings, again.locked)
		}
	})

	t.Run("warns_at_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
		plains, fp := m194Seed(t, pool, 20, 250)
		run := m194Apply(t, dsn, body, plains)
		t.Logf("194 at 250 chunks: %s; authentications %d, longest %s; locks %v; warnings %v", run.took.Round(time.Millisecond), run.authCalls, run.authMax.Round(time.Millisecond), run.locked, run.warnings)
		if run.err != nil {
			t.Fatal(run.err)
		}
		if len(run.warnings) != 1 || !strings.Contains(run.warnings[0], "at 250 chunks") || !strings.Contains(run.warnings[0], "194_pending_writes_drop_auth_keys.sql") {
			t.Errorf("warnings %v, want one naming the chunks and the runbook", run.warnings)
		}
		if len(run.locked) != 0 {
			t.Errorf("the file held %v on the referenced tables above its threshold", run.locked)
		}
		if got := m194Keys(t, pool); strings.Join(got, "|") != strings.Join(wantKeys, "|") {
			t.Errorf("keys above the threshold: %v, want them kept", got)
		}
		if !m194Boundary(t, pool) {
			t.Errorf("context_guard_state.write_log_evicted_before missing above the chunk threshold (part 2 runs on every path)")
		}
		if got := m194Fingerprint(t, pool); got != fp {
			t.Errorf("stages changed: %s -> %s", fp, got)
		}
		// The runbook's drop out of band, then the file again: a no-op.
		if _, err := pool.Exec(ctx, `ALTER TABLE context_pending_writes
			DROP CONSTRAINT context_pending_writes_api_key_id_fkey,
			DROP CONSTRAINT context_pending_writes_principal_id_fkey`); err != nil {
			t.Fatalf("runbook drop: %v", err)
		}
		after := m194Apply(t, dsn, body, plains)
		if after.err != nil || len(after.warnings) != 0 || len(after.locked) != 0 {
			t.Errorf("after the runbook: err %v, warnings %v, locks %v; want a no-op", after.err, after.warnings, after.locked)
		}
	})

	t.Run("lock_timeout_beside_a_reader", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
		m194Seed(t, pool, 2, 10)
		reader := wlConn(t, dsn)
		rtx, err := reader.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rtx.Rollback(ctx) }()
		if _, err := rtx.Exec(ctx, `SELECT 1 FROM context_api_keys LIMIT 1`); err != nil {
			t.Fatal(err)
		}
		run := m194Apply(t, dsn, body, nil)
		_ = rtx.Rollback(ctx)
		var pgErr *pgconn.PgError
		if !errors.As(run.err, &pgErr) || pgErr.Code != "55P03" {
			t.Fatalf("194 beside an open reader of context_api_keys: %v after %s, want lock_not_available (55P03)", run.err, run.took)
		}
		t.Logf("194 beside an open reader: 55P03 after %s", run.took.Round(time.Millisecond))
		if run.took > 10*time.Second {
			t.Errorf("waited %s, want about the 3 s lock_timeout", run.took)
		}
		if got := m194Keys(t, pool); len(got) != 2 || m194Boundary(t, pool) {
			t.Errorf("keys after the failed run: %v, column of part 2 %v; want both keys kept and no column (rolled back)", got, m194Boundary(t, pool))
		}
		if after := m194Apply(t, dsn, body, nil); after.err != nil || len(m194Keys(t, pool)) != 0 {
			t.Errorf("after the reader ended: err %v, keys %v", after.err, m194Keys(t, pool))
		}
	})
}

// TestMigration194MeasureDrop measures the inline drop and the longest
// authentication beside it per chunk count, three times each, for
// docs/operations.md ("Migration 194"). Not a gate.
//
//	CTX_M194_MEASURE=25,100,250,400,499 go test -tags=integration -run TestMigration194MeasureDrop -v ./internal/store/
func TestMigration194MeasureDrop(t *testing.T) {
	sizes := os.Getenv("CTX_M194_MEASURE")
	if sizes == "" {
		t.Skip("CTX_M194_MEASURE unset")
	}
	raw, err := migrations.FS.ReadFile(migration194File)
	if err != nil {
		t.Fatal(err)
	}
	// The drop itself at every size, also above the file's threshold.
	body := []byte(strings.Replace(string(raw), "inline_chunks    CONSTANT INT := 250;", "inline_chunks    CONSTANT INT := 1000000;", 1))
	if string(body) == string(raw) {
		t.Fatal("the threshold constant of 194 moved: adjust the measurement's replacement")
	}
	for _, s := range strings.Split(sizes, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 3; i++ {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 193)
			plains, _ := m194Seed(t, pool, 20, n)
			run := m194Apply(t, dsn, body, plains)
			if run.err != nil || len(m194Keys(t, pool)) != 0 {
				t.Fatalf("%d chunks: err %v keys %v", n, run.err, m194Keys(t, pool))
			}
			t.Logf("M194|%d chunks|drop %s|authentications %d, longest %s|stages %d, longest %s", n, run.took.Round(time.Millisecond), run.authCalls, run.authMax.Round(time.Millisecond), run.stageCalls, time.Duration(run.stageMax.Load()).Round(time.Millisecond))
			pool.Close()
		}
	}
}
