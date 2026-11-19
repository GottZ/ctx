//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// m184Target is one index of a foreign-key lookup migration (184, 185): the
// definition its DO block adopts as PostgreSQL deparses it, the runbook's
// out-of-band DDL from the file header, the plain build the DO block runs
// inline, a writer of its table (RowExclusiveLock, waits for SHARE) and the
// rows the index must hold.
type m184Target struct {
	name, table, def, runbook, writer, entries string
}

// inline is the DDL the DO block runs below the threshold.
func (tg m184Target) inline() string {
	return strings.Replace(tg.runbook, "CREATE INDEX CONCURRENTLY", "CREATE INDEX", 1)
}

var m184Targets = []m184Target{
	{
		name:  "idx_access_tokens_parent",
		table: "context_access_tokens",
		def:   "CREATE INDEX idx_access_tokens_parent ON public.context_access_tokens USING btree (parent_id) WHERE (parent_id IS NOT NULL)",
		runbook: `CREATE INDEX CONCURRENTLY idx_access_tokens_parent
		    ON context_access_tokens (parent_id)
		    WHERE parent_id IS NOT NULL`,
		writer:  `UPDATE context_access_tokens SET revoked_at = revoked_at WHERE id = (SELECT id FROM context_access_tokens ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_access_tokens WHERE parent_id IS NOT NULL`,
	},
	{
		name:  "idx_web_sessions_access_token",
		table: "context_web_sessions",
		def:   "CREATE INDEX idx_web_sessions_access_token ON public.context_web_sessions USING btree (access_token_id)",
		runbook: `CREATE INDEX CONCURRENTLY idx_web_sessions_access_token
		    ON context_web_sessions (access_token_id)`,
		writer:  `UPDATE context_web_sessions SET last_used_at = last_used_at WHERE id = (SELECT id FROM context_web_sessions ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_web_sessions`,
	},
}

// m184ValuesRow is one row of the VALUES block of 184 and 185:
// (ord, name, table, column, predicate, max_rows).
var m184ValuesRow = regexp.MustCompile(`\(\s*(\d+),\s*'(\w+)',\s*'(\w+)',\s*'(\w+)',\s*(?:NULL|'[^']*'),\s*(\d+)\s*\)`)

// m184Thresholds reads the row threshold of every index from the VALUES
// block of the file (the number is the file's to set), and checks the file
// names exactly the targets, each on its table. The order the file builds
// them in is pinned by its behaviour (the lock_timeout subtest).
func m184Thresholds(t *testing.T, body []byte, targets []m184Target) map[string]float64 {
	t.Helper()
	type row struct {
		ord       int
		name, tbl string
		maxRows   float64
	}
	var rows []row
	for _, m := range m184ValuesRow.FindAllStringSubmatch(string(body), -1) {
		ord, _ := strconv.Atoi(m[1])
		n, _ := strconv.ParseFloat(m[5], 64)
		rows = append(rows, row{ord, m[2], m[3], n})
	}
	if len(rows) != len(targets) {
		t.Fatalf("the VALUES block names %d indexes, the test knows %d: %v", len(rows), len(targets), rows)
	}
	out := map[string]float64{}
	for _, r := range rows {
		i := slices.IndexFunc(targets, func(tg m184Target) bool { return tg.name == r.name })
		if i < 0 || targets[i].table != r.tbl {
			t.Fatalf("VALUES row %d is %s on %s, which the test does not know", r.ord, r.name, r.tbl)
		}
		out[r.tbl] = r.maxRows
	}
	return out
}

// m184Run is what one application of a file in a transaction showed.
type m184Run struct {
	took         time.Duration
	locks        map[string][]string // table -> relation locks the open file holds
	readerWaited map[string]bool
	writerWaited map[string]bool
	warnings     []string // WARNINGs naming one of the indexes
}

func (r m184Run) holds(table, mode string) bool { return m182Holds(r.locks[table], mode) }

// m184Apply applies the file in one transaction, as the runner does, over a
// pool of its own whose notice handler collects the WARNINGs of the DO block;
// reltuples fakes the row estimate of tables in the same transaction (the
// threshold reads it). While the file is open it holds, a second connection
// reads and writes every target table, each with a short timeout.
func m184Apply(t *testing.T, dsn string, other *pgxpool.Pool, body []byte, targets []m184Target, reltuples map[string]float64) m184Run {
	t.Helper()
	ctx := context.Background()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	run := m184Run{locks: map[string][]string{}, readerWaited: map[string]bool{}, writerWaited: map[string]bool{}}
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		mu.Lock()
		defer mu.Unlock()
		for _, tg := range targets {
			if n.Severity == "WARNING" && strings.Contains(n.Message, tg.name) {
				run.warnings = append(run.warnings, n.Message)
				return
			}
		}
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var pid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	for tbl, n := range reltuples {
		if _, err := tx.Exec(ctx, `UPDATE pg_class SET reltuples = $1 WHERE oid = $2::regclass`, n, "public."+tbl); err != nil {
			t.Fatalf("set reltuples of %s: %v", tbl, err)
		}
	}
	start := time.Now()
	if _, err := tx.Exec(ctx, string(body)); err != nil {
		t.Fatalf("apply: %v", err)
	}
	run.took = time.Since(start)
	tables := make([]string, len(targets))
	for i, tg := range targets {
		tables[i] = tg.table
	}
	rows, err := other.Query(ctx, `SELECT c.relname::text, l.mode FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
		 WHERE l.pid = $1 AND l.locktype = 'relation' AND l.granted AND c.relname = ANY($2::text[]) ORDER BY 1, 2`, pid, tables)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var rel, mode string
		if err := rows.Scan(&rel, &mode); err != nil {
			t.Fatal(err)
		}
		run.locks[rel] = append(run.locks[rel], mode)
	}
	rows.Close()
	for _, tg := range targets {
		rctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		var n int64
		run.readerWaited[tg.table] = other.QueryRow(rctx, `SELECT count(*) FROM `+tg.table).Scan(&n) != nil
		cancel()
		wctx, cancel := context.WithTimeout(ctx, 400*time.Millisecond)
		_, werr := other.Exec(wctx, tg.writer)
		run.writerWaited[tg.table] = werr != nil
		cancel()
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	return run
}

// m184Fingerprint is every row version of the tables (id and xmin).
func m184Fingerprint(t *testing.T, pool *pgxpool.Pool, targets []m184Target) string {
	t.Helper()
	var out []string
	for _, tg := range targets {
		var s *string
		if err := pool.QueryRow(context.Background(),
			`SELECT md5(string_agg(id::text || ':' || xmin::text, ',' ORDER BY id)) FROM `+tg.table).Scan(&s); err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatalf("fixture: %s is empty", tg.table)
		}
		out = append(out, tg.table+"="+*s)
	}
	return strings.Join(out, " ")
}

// m184Contract is a foreign-key lookup migration under test.
type m184Contract struct {
	file    string
	version int
	targets []m184Target
	seed    func(t *testing.T, pool *pgxpool.Pool) // rows of every target table, at version-1
}

// run pins the ways through the DO block of the file:
//
//	stock         at version-1 with rows written by the store, the runner
//	              builds every index with the definition the file adopts,
//	              holding the rows the index covers, and writes no row; a
//	              second application takes no lock and changes nothing;
//	inline        below the thresholds: SHARE on every table to the commit,
//	              nothing stronger, a reader goes on, a writer of each waits;
//	out_of_band   the runbook's CONCURRENTLY builds are adopted: no lock, no
//	              word;
//	threshold     at the threshold of one table (pg_class.reltuples) exactly
//	              one WARNING, its index not built and no SHARE on its table,
//	              the others built; one row below, built; all tables at their
//	              threshold: one WARNING each, no SHARE anywhere;
//	taken_name    an INVALID index of a name, and every name taken by the
//	              same column with the predicate toggled: a WARNING each,
//	              nothing built or dropped, no SHARE on that table; after the
//	              runbook the file adopts them;
//	lock_timeout  a writer holding each table in turn: the file holds SHARE
//	              on exactly the tables before it (the lock order), fails
//	              after ~3 s with 55P03 and leaves nothing built.
func (c m184Contract) run(t *testing.T) {
	ctx := context.Background()
	body, err := migrations.FS.ReadFile(c.file)
	if err != nil {
		t.Fatalf("read %s: %v", c.file, err)
	}
	limit := m184Thresholds(t, body, c.targets)
	base := c.version - 1
	allBuilt := func(t *testing.T, pool *pgxpool.Pool, except string) {
		t.Helper()
		for _, tg := range c.targets {
			if tg.name == except {
				continue
			}
			if ix := m182Index(t, pool, tg.name); ix.def != tg.def || !ix.valid {
				t.Errorf("%s is %q (valid=%v), want built", tg.name, ix.def, ix.valid)
			}
		}
	}
	noLocks := func(t *testing.T, label string, r m184Run) {
		t.Helper()
		for _, tg := range c.targets {
			if r.holds(tg.table, "ShareLock") || r.readerWaited[tg.table] || r.writerWaited[tg.table] {
				t.Errorf("%s: the file holds %v on %s (reader waited %v, writer waited %v)", label, r.locks[tg.table], tg.table, r.readerWaited[tg.table], r.writerWaited[tg.table])
			}
		}
	}

	t.Run("stock", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
		c.seed(t, pool)
		for _, tg := range c.targets {
			if ix := m182Index(t, pool, tg.name); ix.found {
				t.Fatalf("fixture: %s exists at %d: %s", tg.name, base, ix.def)
			}
		}
		before := m184Fingerprint(t, pool, c.targets)
		if err := store.RunMigrationsUpTo(ctx, pool, c.version); err != nil {
			t.Fatalf("run migrations: %v", err)
		}
		if after := m184Fingerprint(t, pool, c.targets); after != before {
			t.Errorf("migration %d wrote rows:\n  before %s\n  after  %s", c.version, before, after)
		}
		oids := map[string]uint32{}
		for _, tg := range c.targets {
			ix := m182Index(t, pool, tg.name)
			if ix.def != tg.def || !ix.valid {
				t.Errorf("%s deparses as\n  %s (valid=%v)\nwant\n  %s", tg.name, ix.def, ix.valid, tg.def)
			}
			var want int64
			if err := pool.QueryRow(ctx, tg.entries).Scan(&want); err != nil {
				t.Fatal(err)
			}
			if want == 0 || ix.entries != want {
				t.Errorf("%s holds %d entries, want %d (%s), and more than none", tg.name, ix.entries, want, tg.entries)
			}
			oids[tg.name] = ix.oid
		}
		// The second application (the runner skips an applied version; the
		// file itself is idempotent): no lock, no word, the same indexes.
		again := m184Apply(t, dsn, pool, body, c.targets, nil)
		noLocks(t, "second application", again)
		if len(again.warnings) != 0 {
			t.Errorf("second application warns: %v", again.warnings)
		}
		for _, tg := range c.targets {
			if ix := m182Index(t, pool, tg.name); ix.oid != oids[tg.name] || ix.def != tg.def {
				t.Errorf("the second application rebuilt %s: oid %d -> %d", tg.name, oids[tg.name], ix.oid)
			}
		}
	})

	t.Run("inline_below_threshold", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
		c.seed(t, pool)
		r := m184Apply(t, dsn, pool, body, c.targets, nil)
		t.Logf("migration %d inline: %s; locks held to the commit %v; reader waited %v; writer waited %v", c.version, r.took, r.locks, r.readerWaited, r.writerWaited)
		for _, tg := range c.targets {
			for _, m := range r.locks[tg.table] {
				if m != "ShareLock" && m != "AccessShareLock" {
					t.Errorf("the inline build holds %s on %s, want SHARE and nothing stronger", m, tg.table)
				}
			}
			if !r.holds(tg.table, "ShareLock") {
				t.Errorf("the inline build holds %v on %s, want SHARE", r.locks[tg.table], tg.table)
			}
			if r.readerWaited[tg.table] {
				t.Errorf("a reader of %s waited for the open build", tg.table)
			}
			if !r.writerWaited[tg.table] {
				t.Errorf("a writer of %s did not wait for the open build (SHARE)", tg.table)
			}
		}
		if len(r.warnings) != 0 {
			t.Errorf("warnings below the threshold: %v", r.warnings)
		}
		allBuilt(t, pool, "")
	})

	t.Run("built_out_of_band", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
		c.seed(t, pool)
		oids := map[string]uint32{}
		for _, tg := range c.targets {
			m182Exec(t, pool, tg.runbook)
			oids[tg.name] = m182Index(t, pool, tg.name).oid
		}
		r := m184Apply(t, dsn, pool, body, c.targets, nil)
		noLocks(t, "over the out-of-band indexes", r)
		if len(r.warnings) != 0 {
			t.Errorf("an out-of-band build must be adopted without a word, got %v", r.warnings)
		}
		for _, tg := range c.targets {
			if ix := m182Index(t, pool, tg.name); ix.oid != oids[tg.name] || ix.def != tg.def {
				t.Errorf("%s was not adopted as it was: oid %d -> %d, %s", tg.name, oids[tg.name], ix.oid, ix.def)
			}
		}
	})

	for _, tg := range c.targets {
		t.Run("threshold_"+tg.table, func(t *testing.T) {
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
			c.seed(t, pool)
			at := limit[tg.table]
			r := m184Apply(t, dsn, pool, body, c.targets, map[string]float64{tg.table: at})
			want := fmt.Sprintf("%s is missing at %.0f rows of %s", tg.name, at, tg.table)
			if len(r.warnings) != 1 || !strings.Contains(r.warnings[0], want) {
				t.Errorf("warnings %q, want exactly one containing %q", r.warnings, want)
			}
			if ix := m182Index(t, pool, tg.name); ix.found {
				t.Errorf("built %s although %s reports %.0f rows", ix.def, tg.table, at)
			}
			if r.holds(tg.table, "ShareLock") || r.writerWaited[tg.table] {
				t.Errorf("above the threshold the file holds %v on %s (writer waited %v)", r.locks[tg.table], tg.table, r.writerWaited[tg.table])
			}
			for _, o := range c.targets {
				if o.table != tg.table && !r.holds(o.table, "ShareLock") {
					t.Errorf("%s: no SHARE on %s, whose index is built inline", tg.table, o.table)
				}
			}
			allBuilt(t, pool, tg.name)
			// One row below the threshold: built inline, the others adopted.
			below := m184Apply(t, dsn, pool, body, c.targets, map[string]float64{tg.table: at - 1})
			if len(below.warnings) != 0 {
				t.Errorf("warnings at %.0f rows of %s: %v", at-1, tg.table, below.warnings)
			}
			if !below.holds(tg.table, "ShareLock") {
				t.Errorf("at %.0f rows of %s the file holds %v there, want the inline build", at-1, tg.table, below.locks[tg.table])
			}
			allBuilt(t, pool, "")
		})
	}

	t.Run("threshold_all_tables", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
		c.seed(t, pool)
		r := m184Apply(t, dsn, pool, body, c.targets, limit)
		noLocks(t, "every table at its threshold", r)
		if len(r.warnings) != len(c.targets) {
			t.Errorf("%d warnings, want one per table: %q", len(r.warnings), r.warnings)
		}
		for _, tg := range c.targets {
			n := 0
			for _, w := range r.warnings {
				if strings.HasPrefix(w, tg.name+" is missing at ") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("%d warnings name %s, want 1", n, tg.name)
			}
			if ix := m182Index(t, pool, tg.name); ix.found {
				t.Errorf("built %s above the threshold", ix.def)
			}
		}
	})

	// toggled is the same column with the predicate toggled: a definition the
	// file must not adopt.
	toggled := func(tg m184Target) string {
		ddl := tg.inline()
		if i := strings.Index(ddl, "WHERE"); i >= 0 {
			return ddl[:i]
		}
		col := ddl[strings.LastIndex(ddl, "(")+1 : strings.LastIndex(ddl, ")")]
		return ddl + " WHERE " + col + " IS NOT NULL"
	}
	for _, cs := range []struct {
		name  string
		taken []int // indexes of c.targets whose name is taken
		setup func(t *testing.T, pool *pgxpool.Pool)
	}{
		{"invalid", []int{0}, func(t *testing.T, pool *pgxpool.Pool) {
			m182Exec(t, pool, c.targets[0].inline())
			m182Exec(t, pool, `UPDATE pg_index SET indisvalid = false WHERE indexrelid = $1::regclass`, c.targets[0].name)
		}},
		{"foreign_definition", nil, func(t *testing.T, pool *pgxpool.Pool) {
			for _, tg := range c.targets {
				m182Exec(t, pool, toggled(tg))
			}
		}},
	} {
		t.Run("taken_name_"+cs.name, func(t *testing.T) {
			taken := cs.taken
			if taken == nil {
				for i := range c.targets {
					taken = append(taken, i)
				}
			}
			pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
			c.seed(t, pool)
			cs.setup(t, pool)
			before := map[string]m182State{}
			for _, i := range taken {
				before[c.targets[i].name] = m182Index(t, pool, c.targets[i].name)
			}
			r := m184Apply(t, dsn, pool, body, c.targets, nil)
			if len(r.warnings) != len(taken) {
				t.Errorf("%d warnings, want %d: %q", len(r.warnings), len(taken), r.warnings)
			}
			for _, i := range taken {
				tg := c.targets[i]
				n := 0
				for _, w := range r.warnings {
					if strings.HasPrefix(w, tg.name+" exists but is not the foreign-key lookup index") {
						n++
					}
				}
				if n != 1 {
					t.Errorf("%d warnings name the taken %s, want 1", n, tg.name)
				}
				if r.holds(tg.table, "ShareLock") || r.writerWaited[tg.table] {
					t.Errorf("over the taken %s the file holds %v on %s", tg.name, r.locks[tg.table], tg.table)
				}
				if ix, b := m182Index(t, pool, tg.name), before[tg.name]; ix != b {
					t.Errorf("the file changed the taken %s: %+v -> %+v", tg.name, b, ix)
				}
			}
			for i, tg := range c.targets {
				if !slices.Contains(taken, i) {
					if ix := m182Index(t, pool, tg.name); ix.def != tg.def || !ix.valid {
						t.Errorf("%s is %q, want built beside the taken names", tg.name, ix.def)
					}
				}
			}
			// The runbook: drop what holds the name, build it CONCURRENTLY; the
			// file then adopts it without a lock or a word.
			oids := map[string]uint32{}
			for _, i := range taken {
				tg := c.targets[i]
				m182Exec(t, pool, `DROP INDEX CONCURRENTLY IF EXISTS `+tg.name)
				m182Exec(t, pool, tg.runbook)
				oids[tg.name] = m182Index(t, pool, tg.name).oid
			}
			again := m184Apply(t, dsn, pool, body, c.targets, nil)
			noLocks(t, "after the runbook", again)
			if len(again.warnings) != 0 {
				t.Errorf("after the runbook the file warns %v", again.warnings)
			}
			for name, oid := range oids {
				if ix := m182Index(t, pool, name); ix.oid != oid {
					t.Errorf("after the runbook the file rebuilt %s: oid %d -> %d", name, oid, ix.oid)
				}
			}
		})
	}

	// A writer that keeps a transaction open on one table: the file builds
	// the indexes before it, waits lock_timeout (3 s) for that table and
	// fails with 55P03; the transaction rolls back with nothing built. With
	// the writer on each table in turn, the SHARE locks held while waiting
	// show the order the file takes the tables in.
	t.Run("lock_timeout", func(t *testing.T) {
		pool, dsn := testdb.SetupTestDBUpToWithDSN(t, base)
		c.seed(t, pool)
		tables := make([]string, len(c.targets))
		for i, tg := range c.targets {
			tables[i] = tg.table
		}
		for k, tg := range c.targets {
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := holder.Exec(ctx, tg.writer); err != nil {
				t.Fatal(err)
			}
			mpool, err := pgxpool.New(ctx, dsn)
			if err != nil {
				t.Fatal(err)
			}
			mtx, err := mpool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var pid int
			if err := mtx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			start := time.Now()
			go func() {
				_, err := mtx.Exec(ctx, string(body))
				done <- err
			}()
			time.Sleep(1500 * time.Millisecond)
			var held, waiting []string
			rows, err := pool.Query(ctx, `SELECT c.relname::text, l.granted FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
				 WHERE l.pid = $1 AND l.locktype = 'relation' AND l.mode = 'ShareLock' AND c.relname = ANY($2::text[])`, pid, tables)
			if err != nil {
				t.Fatal(err)
			}
			for rows.Next() {
				var rel string
				var granted bool
				if err := rows.Scan(&rel, &granted); err != nil {
					t.Fatal(err)
				}
				if granted {
					held = append(held, rel)
				} else {
					waiting = append(waiting, rel)
				}
			}
			rows.Close()
			var ferr error
			select {
			case ferr = <-done:
			case <-time.After(20 * time.Second):
				t.Fatalf("writer on %s: the file did not give up", tg.table)
			}
			took := time.Since(start)
			_ = mtx.Rollback(ctx)
			mpool.Close()
			_ = holder.Rollback(ctx)
			slices.Sort(held)
			wantHeld := slices.Clone(tables[:k])
			slices.Sort(wantHeld)
			t.Logf("writer on %s: the file failed after %s (%v); SHARE held while waiting %v, waiting for %v", tg.table, took.Round(time.Millisecond), ferr, held, waiting)
			var pgErr *pgconn.PgError
			if !errors.As(ferr, &pgErr) || pgErr.Code != "55P03" {
				t.Errorf("writer on %s: the file ended with %v, want lock_not_available (55P03)", tg.table, ferr)
			}
			if took < 2900*time.Millisecond || took > 10*time.Second {
				t.Errorf("writer on %s: the file gave up after %s, want about the 3 s of its lock_timeout", tg.table, took)
			}
			if !slices.Equal(held, wantHeld) || !slices.Equal(waiting, []string{tg.table}) {
				t.Errorf("writer on %s: the file held SHARE on %v and waited for %v, want held %v (the tables before it) and waiting for %s", tg.table, held, waiting, wantHeld, tg.table)
			}
			for _, o := range c.targets {
				if ix := m182Index(t, pool, o.name); ix.found {
					t.Errorf("writer on %s: %s exists after the failed file: %s", tg.table, o.name, ix.def)
				}
			}
		}
	})
}

// m184SeedTokens writes token rows and web sessions through the store, as
// login, cookie refresh and the OAuth code grant leave them: a login pair
// with its session, rotated once (a pair with parent_id, the session
// repointed), and a lone OAuth access token.
func m184SeedTokens(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	key, _, err := store.CreateApiKey(ctx, pool, "m184", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	var principal string
	if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, key.ID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	login := store.OAuthToken{APIKeyID: key.ID, PrincipalID: principal, Audiences: []string{"ctx-mcp", "ctx-web"}, IssuedVia: "login"}
	for i := 0; i < 2; i++ {
		pair, err := store.MintTokenPair(ctx, pool, login, time.Hour, 24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		sid, err := store.CreateWebSession(ctx, pool, pair.AccessToken, principal, "m184-csrf", "m184-agent", "")
		if err != nil {
			t.Fatal(err)
		}
		rot, outcome, err := store.RotateRefreshToken(ctx, pool, pair.RefreshToken, "", time.Hour, 24*time.Hour, 90*24*time.Hour)
		if err != nil || outcome != store.Rotated {
			t.Fatalf("rotate: %v (outcome %v)", err, outcome)
		}
		if ok, err := store.RepointWebSession(ctx, pool, sid, rot.AccessToken); err != nil || !ok {
			t.Fatalf("repoint: %v %v", ok, err)
		}
	}
	if _, err := store.MintAccessToken(ctx, pool, store.OAuthToken{APIKeyID: key.ID, PrincipalID: principal, Audiences: []string{"ctx-mcp"}, IssuedVia: "oauth"}, time.Hour); err != nil {
		t.Fatal(err)
	}
}

// TestMigration184 applies migration 184 (issue #277) to a database at 183:
// idx_access_tokens_parent and idx_web_sessions_access_token through every
// way of its DO block (m184Contract.run).
func TestMigration184(t *testing.T) {
	m184Contract{file: "184_access_token_fk_lookup_indexes.sql", version: 184, targets: m184Targets, seed: m184SeedTokens}.run(t)
}

// ---------------------------------------------------------------------------
// The cost of the statements the indexes serve, from auto_explain.

// m184Explain collects every plan auto_explain sends as a notice on its pool:
// top-level statements and the nested ones (the foreign-key trigger
// queries), with ANALYZE and BUFFERS.
type m184Explain struct {
	mu    sync.Mutex
	plans []string
}

// m184ExplainPool is a pool of one connection in the given plan_cache_mode
// whose every statement, nested ones included, reports its plan.
func m184ExplainPool(t *testing.T, dsn, planCacheMode string) (*pgxpool.Pool, *m184Explain) {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	rec := &m184Explain{}
	cfg.MaxConns = 1
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) {
		if i := strings.Index(n.Message, "{"); i >= 0 && strings.Contains(n.Message, "plan:") {
			rec.mu.Lock()
			rec.plans = append(rec.plans, n.Message[i:])
			rec.mu.Unlock()
		}
	}
	cfg.AfterConnect = func(ctx context.Context, c *pgx.Conn) error {
		for _, s := range []string{
			`LOAD 'auto_explain'`,
			`SET auto_explain.log_min_duration = 0`,
			`SET auto_explain.log_analyze = on`,
			`SET auto_explain.log_buffers = on`,
			`SET auto_explain.log_timing = off`,
			`SET auto_explain.log_nested_statements = on`,
			`SET auto_explain.log_format = 'json'`,
			`SET auto_explain.log_level = 'notice'`,
			`SET plan_cache_mode = ` + planCacheMode,
		} {
			if _, err := c.Exec(ctx, s); err != nil {
				return fmt.Errorf("%s: %w", s, err)
			}
		}
		return nil
	}
	pool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return pool, rec
}

// m184Lookup is a foreign-key lookup: the trigger query on table.col, and
// the index that must serve it.
type m184Lookup struct{ table, col, index string }

// m184LookupStat is what auto_explain reported for one lookup in one run.
type m184LookupStat struct {
	n, generic, hits int
	maxBuffers       int64
	sumBuffers       int64
	shapes           map[string]int
	bad              []string
}

func m184Walk(pl map[string]any, f func(map[string]any)) {
	f(pl)
	if kids, ok := pl["Plans"].([]any); ok {
		for _, k := range kids {
			if m, ok := k.(map[string]any); ok {
				m184Walk(m, f)
			}
		}
	}
}

func m184Shape(pl map[string]any) string {
	s := fmt.Sprint(pl["Node Type"])
	if ix, ok := pl["Index Name"]; ok {
		s += "(" + fmt.Sprint(ix) + ")"
	}
	if kids, ok := pl["Plans"].([]any); ok {
		var ks []string
		for _, k := range kids {
			ks = append(ks, m184Shape(k.(map[string]any)))
		}
		s += "[" + strings.Join(ks, ",") + "]"
	}
	return s
}

func m184Num(pl map[string]any, k string) int64 {
	v, _ := pl[k].(float64)
	return int64(v)
}

// m184Lookups sorts the plans of a run into the lookups and checks each: an
// index node on its index, no sequential scan, at most maxBuffers shared
// buffers (hit + read, the whole trigger query with its writes).
func m184Lookups(t *testing.T, rec *m184Explain, lookups []m184Lookup, maxBuffers int64) map[string]*m184LookupStat {
	t.Helper()
	rec.mu.Lock()
	plans := append([]string(nil), rec.plans...)
	rec.mu.Unlock()
	out := map[string]*m184LookupStat{}
	for _, l := range lookups {
		out[l.table] = &m184LookupStat{shapes: map[string]int{}}
	}
	for _, raw := range plans {
		var p struct {
			Query string         `json:"Query Text"`
			Plan  map[string]any `json:"Plan"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil {
			t.Fatalf("auto_explain json: %v: %s", err, raw)
		}
		for _, l := range lookups {
			if !strings.Contains(p.Query, `"public"."`+l.table+`"`) || !strings.Contains(p.Query, `$1 OPERATOR(pg_catalog.=) "`+l.col+`"`) {
				continue
			}
			s := out[l.table]
			s.n++
			buf := m184Num(p.Plan, "Shared Hit Blocks") + m184Num(p.Plan, "Shared Read Blocks")
			s.sumBuffers += buf
			if buf > s.maxBuffers {
				s.maxBuffers = buf
			}
			shape := m184Shape(p.Plan)
			s.shapes[shape]++
			served, seq, generic := false, false, false
			m184Walk(p.Plan, func(n map[string]any) {
				if fmt.Sprint(n["Node Type"]) == "Seq Scan" {
					seq = true
				}
				if fmt.Sprint(n["Index Name"]) == l.index {
					served = true
					if strings.Contains(fmt.Sprint(n["Index Cond"]), "$1") {
						generic = true
					}
					if m184Num(n, "Actual Rows") > 0 {
						s.hits++
					}
				}
			})
			if generic {
				s.generic++
			}
			if seq || !served || buf > maxBuffers {
				if len(s.bad) < 3 {
					s.bad = append(s.bad, fmt.Sprintf("%s: %d buffers (served by %s %v, seq scan %v)", shape, buf, l.index, served, seq))
				}
			}
		}
	}
	return out
}

// m184StatsState is a statistics state of the tables under test.
type m184StatsState struct {
	name  string
	apply func(t *testing.T, pool *pgxpool.Pool, tables []string)
}

// m184States: stale (analysed at a small size, then grown — the first state
// of each cost test, set up before it), fresh (VACUUM ANALYZE), never
// analysed (relation and column statistics cleared, as clearStats of the
// guard cost tests).
var m184States = []m184StatsState{
	{"stale", func(*testing.T, *pgxpool.Pool, []string) {}},
	{"fresh", func(t *testing.T, pool *pgxpool.Pool, tables []string) {
		for _, tbl := range tables {
			m182Exec(t, pool, `VACUUM ANALYZE `+tbl)
		}
	}},
	{"never_analysed", func(t *testing.T, pool *pgxpool.Pool, tables []string) {
		ctx := context.Background()
		for _, tbl := range tables {
			rows, err := pool.Query(ctx, `SELECT attname::text FROM pg_attribute WHERE attrelid = $1::regclass AND attnum > 0 AND NOT attisdropped`, tbl)
			if err != nil {
				t.Fatal(err)
			}
			cols, err := pgx.CollectRows(rows, pgx.RowTo[string])
			if err != nil {
				t.Fatal(err)
			}
			m182Exec(t, pool, `SELECT pg_clear_relation_stats('public', $1::text)`, tbl)
			for _, col := range cols {
				m182Exec(t, pool, `SELECT pg_clear_attribute_stats('public', $1::text, $2::text, false)`, tbl, col)
			}
		}
	}},
}

// m184PlanModes: a custom plan per execution, the generic plan, and the
// default (custom for five executions, then generic when it is not dearer).
var m184PlanModes = []string{"force_custom_plan", "force_generic_plan", "auto"}

// m184CostSchema is the schema a cost test runs on: the migration under test,
// or the one before it when env names it (the red proof: without the indexes
// the lookups scan).
func m184CostSchema(t *testing.T, env string, def int) int {
	t.Helper()
	if v := os.Getenv(env); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s=%d: the cost test runs on schema %d", env, n, n)
		return n
	}
	return def
}

const (
	// m184CostFamilies login families of two generations (four tokens, one
	// web session each) beside the due rows of each cell.
	m184CostFamilies = 10000
	// m184DueRotated families whose first generation is due and whose second
	// is alive (the sweep sets their parent_id NULL), m184DueDead families
	// due whole, with their session (the sweep deletes it): 2*50 + 4*25 =
	// 200 due tokens per cell.
	m184DueRotated = 50
	m184DueDead    = 25
	// m184LookupMaxBuffers bounds one foreign-key lookup of the sweep with the
	// index, its writes included. Measured (2026-11-19, 40 000 tokens, 10 000
	// sessions, every plan mode and statistics state): at most 57 buffers on
	// parent_id, where the lookup of a rotated refresh row sets parent_id NULL
	// on its two children (non-HOT updates: a new entry in every other index
	// of the table; a refresh row is taken by one rotation only, so two is the
	// most), 2-4 on access_token_id. Without the indexes (schema 183) every
	// lookup scanned its table: 1 260-1 361 buffers on parent_id, 252-260 on
	// access_token_id, and the sweep took 0.9-1.2 s instead of 41-84 ms.
	m184LookupMaxBuffers = 64
)

// m184SeedFamilies adds alive login families from+1 … from+n as the
// measurement of 183-187 seeds them: access and refresh of generation 0
// (anchors, parent_id NULL) and of generation 1 (parent_id = the first
// refresh), one web session on the current access token.
func m184SeedFamilies(t *testing.T, pool *pgxpool.Pool, keys []string, from, n int) {
	t.Helper()
	m182Exec(t, pool, `
		WITH k AS (SELECT array_agg(id ORDER BY id) AS ids, array_agg(principal_id ORDER BY id) AS ps FROM context_api_keys WHERE id = ANY($3::uuid[])),
		f AS (SELECT g, md5('fam-' || g)::uuid AS fam, k.ids[1 + g % cardinality(k.ids)] AS key_id, k.ps[1 + g % cardinality(k.ids)] AS principal,
		             now() - (g % 6) * interval '1 day' AS t0
		        FROM k, generate_series($1::int + 1, $1::int + $2::int) g)
		INSERT INTO context_access_tokens (id, token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, parent_id, issued_via, expires_at, created_at)
		SELECT md5(x.tag || '-' || f.g)::uuid, encode(sha256(convert_to(x.tag || '-' || f.g, 'UTF8')), 'hex'), x.typ, f.key_id, f.principal,
		       ARRAY['ctx-mcp', 'ctx-web'], f.fam, CASE WHEN x.gen = 1 THEN md5('r0-' || f.g)::uuid END, 'login',
		       f.t0 + interval '30 days', f.t0 + x.gen * interval '1 hour'
		  FROM f, (VALUES ('a0', 'access', 0), ('r0', 'refresh', 0), ('a1', 'access', 1), ('r1', 'refresh', 1)) AS x(tag, typ, gen)
		 ORDER BY f.g, x.gen`, from, n, keys)
	m182Exec(t, pool, `
		INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret, created_at, last_used_at, user_agent)
		SELECT t.principal_id, t.id, t.refresh_family, md5(t.id::text), t.created_at, t.created_at, 'Mozilla/5.0 (X11; Linux x86_64) Chrome/131.0'
		  FROM context_access_tokens t
		 WHERE t.id IN (SELECT md5('a1-' || g)::uuid FROM generate_series($1::int + 1, $1::int + $2::int) g)`, from, n)
}

// m184SeedDue adds the due rows of one cell: m184DueRotated families whose
// generation 0 expired 10 days ago and whose generation 1 (rotated 5 days
// ago) is alive with the session, and m184DueDead families due whole, with
// their session on the due access token.
func m184SeedDue(t *testing.T, pool *pgxpool.Pool, keys []string, cell int) {
	t.Helper()
	m182Exec(t, pool, `
		WITH k AS (SELECT array_agg(id ORDER BY id) AS ids, array_agg(principal_id ORDER BY id) AS ps FROM context_api_keys WHERE id = ANY($4::uuid[])),
		f AS (SELECT g, md5('due-fam-' || $1 || '-' || g)::uuid AS fam, k.ids[1 + g % cardinality(k.ids)] AS key_id,
		             k.ps[1 + g % cardinality(k.ids)] AS principal, g <= $2::int AS rotated
		        FROM k, generate_series(1, $2::int + $3::int) g)
		INSERT INTO context_access_tokens (id, token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, parent_id, issued_via, expires_at, created_at)
		SELECT md5('due-' || x.tag || '-' || $1 || '-' || f.g)::uuid, encode(sha256(convert_to('due-' || x.tag || '-' || $1 || '-' || f.g, 'UTF8')), 'hex'),
		       x.typ, f.key_id, f.principal, ARRAY['ctx-mcp', 'ctx-web'], f.fam,
		       CASE WHEN x.gen = 1 THEN md5('due-r0-' || $1 || '-' || f.g)::uuid END, 'login',
		       CASE WHEN x.gen = 1 AND f.rotated THEN now() + interval '25 days' ELSE now() - interval '10 days' END,
		       CASE WHEN x.gen = 1 AND f.rotated THEN now() - interval '5 days' ELSE now() - interval '40 days' END
		  FROM f, (VALUES ('a0', 'access', 0), ('r0', 'refresh', 0), ('a1', 'access', 1), ('r1', 'refresh', 1)) AS x(tag, typ, gen)
		 ORDER BY f.g, x.gen`, strconv.Itoa(cell), m184DueRotated, m184DueDead, keys)
	m182Exec(t, pool, `
		INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret, created_at, last_used_at)
		SELECT t.principal_id, t.id, t.refresh_family, md5(t.id::text), t.created_at, t.created_at
		  FROM context_access_tokens t
		 WHERE t.id IN (SELECT md5('due-a1-' || $1 || '-' || g)::uuid FROM generate_series(1, $2::int) g)`, strconv.Itoa(cell), m184DueRotated+m184DueDead)
}

// TestMigration184TokenSweepUsesTheIndexes pins the cost of the token sweep
// (store.EvictExpiredOAuthTokens, the production path) beside 40 000 tokens
// and 10 000 web sessions: each of the 200 due tokens per run costs one
// lookup on context_access_tokens.parent_id (ON DELETE SET NULL) and one on
// context_web_sessions.access_token_id (ON DELETE CASCADE), and auto_explain
// reports every one served by its index of 184 — no sequential scan, at most
// m184LookupMaxBuffers buffers — in every plan_cache_mode and with fresh,
// stale and absent statistics. On schema 183 (CTX_M184_COST_SCHEMA=183) every
// lookup is a sequential scan and the test fails.
func TestMigration184TokenSweepUsesTheIndexes(t *testing.T) {
	ctx := context.Background()
	schema := m184CostSchema(t, "CTX_M184_COST_SCHEMA", 184)
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 183)
	tables := []string{"context_access_tokens", "context_web_sessions"}
	for _, tbl := range tables {
		m182Exec(t, pool, `ALTER TABLE `+tbl+` SET (autovacuum_enabled = false)`)
	}
	var keys []string
	for i := 0; i < 20; i++ {
		k, _, err := store.CreateApiKey(ctx, pool, fmt.Sprintf("m184-cost-%d", i), "private", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.ID)
	}
	// Stale statistics: analysed at 100 families, then grown a hundredfold.
	m184SeedFamilies(t, pool, keys, 0, 100)
	for _, tbl := range tables {
		m182Exec(t, pool, `VACUUM ANALYZE `+tbl)
	}
	if err := store.RunMigrationsUpTo(ctx, pool, schema); err != nil {
		t.Fatalf("migrate to %d: %v", schema, err)
	}
	m184SeedFamilies(t, pool, keys, 100, m184CostFamilies-100)
	lookups := []m184Lookup{
		{"context_access_tokens", "parent_id", "idx_access_tokens_parent"},
		{"context_web_sessions", "access_token_id", "idx_web_sessions_access_token"},
	}
	cell := 0
	for _, st := range m184States {
		st.apply(t, pool, tables)
		for _, mode := range m184PlanModes {
			cell++
			m184SeedDue(t, pool, keys, cell)
			var sessionsBefore int64
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_web_sessions`).Scan(&sessionsBefore); err != nil {
				t.Fatal(err)
			}
			epool, rec := m184ExplainPool(t, dsn, mode)
			t0 := time.Now()
			n, err := store.EvictExpiredOAuthTokens(ctx, epool)
			took := time.Since(t0)
			epool.Close()
			if err != nil {
				t.Fatalf("%s/%s: sweep: %v", st.name, mode, err)
			}
			wantDue := int64(2*m184DueRotated + 4*m184DueDead)
			if n != wantDue {
				t.Fatalf("%s/%s: the sweep deleted %d tokens, want the %d due", st.name, mode, n, wantDue)
			}
			var sessionsAfter, orphans int64
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_web_sessions),
				(SELECT count(*) FROM context_access_tokens c WHERE c.parent_id IS NOT NULL AND NOT EXISTS (SELECT 1 FROM context_access_tokens p WHERE p.id = c.parent_id))`).Scan(&sessionsAfter, &orphans); err != nil {
				t.Fatal(err)
			}
			if sessionsBefore-sessionsAfter != m184DueDead || orphans != 0 {
				t.Errorf("%s/%s: the sweep removed %d sessions (want %d) and left %d dangling parent_id", st.name, mode, sessionsBefore-sessionsAfter, m184DueDead, orphans)
			}
			stats := m184Lookups(t, rec, lookups, m184LookupMaxBuffers)
			for _, l := range lookups {
				s := stats[l.table]
				t.Logf("MEASURE|184 sweep|schema=%d|stats=%s|mode=%s|lookup=%s.%s|n=%d|generic=%d|max_buffers=%d|avg_buffers=%.1f|took=%s|shapes=%v",
					schema, st.name, mode, l.table, l.col, s.n, s.generic, s.maxBuffers, float64(s.sumBuffers)/float64(max(s.n, 1)), took.Round(time.Millisecond), s.shapes)
				if int64(s.n) != wantDue {
					t.Errorf("%s/%s: auto_explain reported %d lookups on %s.%s, want one per deleted token (%d)", st.name, mode, s.n, l.table, l.col, wantDue)
				}
				if len(s.bad) > 0 {
					t.Errorf("%s/%s: lookups on %s.%s not served by %s within %d buffers: %v", st.name, mode, l.table, l.col, l.index, m184LookupMaxBuffers, s.bad)
				}
			}
		}
	}
}
