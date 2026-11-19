//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// m185Targets are the four indexes of 185 in the order the file builds them
// (its lock order).
var m185Targets = []m184Target{
	{
		name:  "idx_block_grants_granted_by",
		table: "context_block_grants",
		def:   "CREATE INDEX idx_block_grants_granted_by ON public.context_block_grants USING btree (granted_by) WHERE (granted_by IS NOT NULL)",
		runbook: `CREATE INDEX CONCURRENTLY idx_block_grants_granted_by
		    ON context_block_grants (granted_by)
		    WHERE granted_by IS NOT NULL`,
		writer:  `UPDATE context_block_grants SET metadata = metadata WHERE id = (SELECT id FROM context_block_grants ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_block_grants WHERE granted_by IS NOT NULL`,
	},
	{
		name:  "idx_oauth_codes_api_key",
		table: "context_oauth_codes",
		def:   "CREATE INDEX idx_oauth_codes_api_key ON public.context_oauth_codes USING btree (api_key_id)",
		runbook: `CREATE INDEX CONCURRENTLY idx_oauth_codes_api_key
		    ON context_oauth_codes (api_key_id)`,
		writer:  `UPDATE context_oauth_codes SET expires_at = expires_at WHERE id = (SELECT id FROM context_oauth_codes ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_oauth_codes`,
	},
	{
		name:  "idx_write_log_api_key",
		table: "context_write_log",
		def:   "CREATE INDEX idx_write_log_api_key ON public.context_write_log USING btree (api_key_id) WHERE (api_key_id IS NOT NULL)",
		runbook: `CREATE INDEX CONCURRENTLY idx_write_log_api_key
		    ON context_write_log (api_key_id)
		    WHERE api_key_id IS NOT NULL`,
		writer:  `UPDATE context_write_log SET decision = decision WHERE id = (SELECT id FROM context_write_log ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_write_log WHERE api_key_id IS NOT NULL`,
	},
	{
		name:  "idx_chat_sessions_created_by",
		table: "context_chat_sessions",
		def:   "CREATE INDEX idx_chat_sessions_created_by ON public.context_chat_sessions USING btree (created_by) WHERE (created_by IS NOT NULL)",
		runbook: `CREATE INDEX CONCURRENTLY idx_chat_sessions_created_by
		    ON context_chat_sessions (created_by)
		    WHERE created_by IS NOT NULL`,
		writer:  `UPDATE context_chat_sessions SET updated_at = updated_at WHERE id = (SELECT id FROM context_chat_sessions ORDER BY id LIMIT 1)`,
		entries: `SELECT count(*) FROM context_chat_sessions WHERE created_by IS NOT NULL`,
	},
}

// m185Key creates an API key in the default tenant and returns it with its
// principal.
func m185Key(t *testing.T, pool *pgxpool.Pool, label string) (key, principal string) {
	t.Helper()
	ctx := context.Background()
	k, _, err := store.CreateApiKey(ctx, pool, label, "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, k.ID).Scan(&principal); err != nil {
		t.Fatal(err)
	}
	return k.ID, principal
}

// m185SeedKeyRows writes rows of all four tables that name the key, through
// the store where it has a writer: a grant (store.CreateBlockGrant) on a
// block written by SQL, two chats (store.CreateSession; one without a key),
// an authorization code (store.PutOAuthCode); the write log by SQL, since no
// writer of this version sets context_write_log.api_key_id (one row with the
// key, one without).
func m185SeedKeyRows(t *testing.T, pool *pgxpool.Pool, key, principal string) {
	t.Helper()
	ctx := context.Background()
	var block string
	if err := pool.QueryRow(ctx, `INSERT INTO context_blocks (category, title, content, scope) VALUES ('m185', $1, 'm185 grant', 'private') RETURNING id::text`,
		"m185 "+key).Scan(&block); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateBlockGrant(ctx, pool, block, store.DefaultTenantID, key); err != nil {
		t.Fatal(err)
	}
	for _, by := range []string{key, ""} {
		if _, err := store.CreateSession(ctx, pool, "private", []string{"private"}, by, "m185 chat"); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PutOAuthCode(ctx, pool, store.TokenHash("m185-code-"+key), store.OAuthCode{
		APIKeyID: key, PrincipalID: principal, RedirectURI: "https://example.invalid/cb", CodeChallenge: "m185-challenge", ExpiresAt: time.Now().Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	m182Exec(t, pool, `INSERT INTO context_write_log (block_id, decision, scope, block_title, block_category, api_key_id)
		VALUES ($1::uuid, 'clean', 'private', 'm185', 'm185', $2::uuid), ($1::uuid, 'clean', 'private', 'm185', 'm185', NULL)`, block, key)
}

func m185Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	key, principal := m185Key(t, pool, "m185")
	m185SeedKeyRows(t, pool, key, principal)
}

// TestMigration185 applies migration 185 (issue #277) to a database at 184:
// the four indexes through every way of its DO block (m184Contract.run).
func TestMigration185(t *testing.T) {
	m184Contract{file: "185_api_key_fk_lookup_indexes.sql", version: 185, targets: m185Targets, seed: m185Seed}.run(t)
}

const (
	// m185CostWriteLog write-log rows of the guard's and dream's width beside
	// the deleted keys, api_key_id NULL as on live; m185CostOther rows of each
	// of the other three tables, spread over m185RestKeys other keys (grants
	// and codes are written by many keys at the target scale; one key holding
	// every row is the case the generic plan reads as "the whole table", see
	// TestMigration185KeyDeleteUsesTheIndexes).
	m185CostWriteLog = 300000
	m185CostOther    = 20000
	m185RestKeys     = 50
	// m185CellKeys keys per pruned tenant: more than the five custom plans of
	// plan_cache_mode auto, so the generic plan is reached.
	m185CellKeys = 8
	// m185LookupMaxBuffers bounds one foreign-key lookup of a key delete with
	// the index, its writes included (two rows per key and table; SET NULL is
	// a non-HOT update with a new entry in every other index of the table).
	// Measured (2026-11-19, 300 000 write-log rows, 20 000 of each other
	// table, every plan mode and statistics state): at most 36 buffers on the
	// write log, 23 on the grants, 19 on the chats, 4 on the codes. Without
	// the indexes (schema 184) every lookup scanned its table: 18 768-18 784
	// buffers on the write log, 271-293 on the grants, 573-575 on the codes,
	// 369-386 on the chats; the prune of 8 keys took 375-566 ms instead of
	// 42-80 ms.
	m185LookupMaxBuffers = 64
)

// m185SeedRest adds rows that do not name the deleted keys: write-log rows
// wlFrom+1 … wlFrom+wl (api_key_id NULL, as the measurement of 183-187 seeds
// them), and blocks, grants, chats and codes from+1 … from+other on the rest
// keys.
func m185SeedRest(t *testing.T, pool *pgxpool.Pool, restKeys []string, wlFrom, wl, from, other int) {
	t.Helper()
	m182Exec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
	m182Exec(t, pool, `INSERT INTO context_blocks (category, title, content, scope)
		SELECT 'm185r', 'm185r ' || g, 'x', 'private' FROM generate_series($1::int + 1, $1::int + $2::int) g`, from, other)
	m182Exec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
	m182Exec(t, pool, `
		WITH b AS (SELECT array_agg(id ORDER BY id) AS a FROM (SELECT id FROM context_blocks WHERE category = 'm185r' ORDER BY id LIMIT 2000) x)
		INSERT INTO context_write_log (block_id, matched_block_id, decision, similarity, scope, block_title, block_category, metadata, created_at)
		SELECT b.a[1 + g % cardinality(b.a)], CASE WHEN g % 30 = 0 THEN b.a[1 + (g * 7) % cardinality(b.a)] END,
		       CASE WHEN g % 10 = 0 THEN 'clean' ELSE 'dream_link' END, 0.8, 'private',
		       'm185 write-log title of moderate length number ' || g, 'learnings',
		       jsonb_build_object('source', 'dream_v1', 'links_created', 4, 'links', jsonb_build_array(
		           jsonb_build_object('target_id', b.a[1 + (g * 3) % cardinality(b.a)]::text, 'confidence', 0.9500197690538208, 'relationship', 'topical'),
		           jsonb_build_object('target_id', b.a[1 + (g * 5) % cardinality(b.a)]::text, 'confidence', 0.8812345678901234, 'relationship', 'supports'))),
		       now() - g * interval '1 second'
		  FROM b, generate_series($1::int + 1, $1::int + $2::int) g`, wlFrom, wl)
	const keys = `k AS (SELECT array_agg(id ORDER BY id) AS ids, array_agg(principal_id ORDER BY id) AS ps FROM context_api_keys WHERE id = ANY($2::uuid[]))`
	m182Exec(t, pool, `WITH `+keys+`
		INSERT INTO context_chat_sessions (scope, read_scopes, created_by, title, metadata)
		SELECT 'private', ARRAY['private'], CASE WHEN g % 2 = 0 THEN k.ids[1 + (g / 2) % cardinality(k.ids)] END, 'm185 chat ' || g, '{}'
		  FROM k, generate_series(1, $1::int) g`, other, restKeys)
	m182Exec(t, pool, `WITH `+keys+`
		INSERT INTO context_oauth_codes (code_hash, api_key_id, principal_id, redirect_uri, code_challenge, expires_at)
		SELECT md5('m185-code-' || $3::int || '-' || g) || md5('m185-c2-' || $3::int || '-' || g), k.ids[1 + g % cardinality(k.ids)], k.ps[1 + g % cardinality(k.ids)],
		       'https://example.invalid/cb', md5('ch-' || g), now() + interval '1 minute'
		  FROM k, generate_series(1, $1::int) g`, other, restKeys, from)
	m182Exec(t, pool, `WITH `+keys+`
		INSERT INTO context_block_grants (block_id, grantee_tenant, granted_by)
		SELECT b.id, $1::uuid, k.ids[1 + abs(hashtext(b.id::text)) % cardinality(k.ids)]
		  FROM k, context_blocks b WHERE b.category = 'm185r' AND b.title = ANY(
		    SELECT 'm185r ' || g FROM generate_series($3::int + 1, $3::int + $4::int) g)`, store.DefaultTenantID, restKeys, from, other)
}

// TestMigration185KeyDeleteUsesTheIndexes pins the cost of deleting API keys
// through the tenant prune (store.PruneTenant, the production path that
// deletes keys) beside 300 000 write-log rows and 20 000 rows of each other
// table: each deleted key costs one lookup per foreign key on
// context_write_log.api_key_id, context_block_grants.granted_by,
// context_chat_sessions.created_by (ON DELETE SET NULL) and
// context_oauth_codes.api_key_id (ON DELETE CASCADE), and auto_explain
// reports every one served by its index of 185 — no sequential scan, at most
// m185LookupMaxBuffers buffers — in every plan_cache_mode and with fresh,
// stale and absent statistics. On schema 184 (CTX_M185_COST_SCHEMA=184)
// every lookup is a sequential scan and the test fails.
func TestMigration185KeyDeleteUsesTheIndexes(t *testing.T) {
	ctx := context.Background()
	schema := m184CostSchema(t, "CTX_M185_COST_SCHEMA", 185)
	pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 184)
	tables := []string{"context_block_grants", "context_oauth_codes", "context_write_log", "context_chat_sessions"}
	for _, tbl := range tables {
		m182Exec(t, pool, `ALTER TABLE `+tbl+` SET (autovacuum_enabled = false)`)
	}
	var restKeys []string
	for i := 0; i < m185RestKeys; i++ {
		k, _ := m185Key(t, pool, fmt.Sprintf("m185-rest-%d", i))
		restKeys = append(restKeys, k)
	}
	// Stale statistics: analysed at a small size, then grown.
	m185SeedRest(t, pool, restKeys, 0, 2000, 0, 200)
	for _, tbl := range tables {
		m182Exec(t, pool, `VACUUM ANALYZE `+tbl)
	}
	if err := store.RunMigrationsUpTo(ctx, pool, schema); err != nil {
		t.Fatalf("migrate to %d: %v", schema, err)
	}
	m185SeedRest(t, pool, restKeys, 2000, m185CostWriteLog-2000, 200, m185CostOther-200)
	lookups := []m184Lookup{
		{"context_block_grants", "granted_by", "idx_block_grants_granted_by"},
		{"context_oauth_codes", "api_key_id", "idx_oauth_codes_api_key"},
		{"context_write_log", "api_key_id", "idx_write_log_api_key"},
		{"context_chat_sessions", "created_by", "idx_chat_sessions_created_by"},
	}
	cell := 0
	for _, st := range m184States {
		st.apply(t, pool, tables)
		for _, mode := range m184PlanModes {
			cell++
			tn, err := store.CreateTenant(ctx, pool, fmt.Sprintf("m185-cost-%d", cell), "m185 cost")
			if err != nil {
				t.Fatal(err)
			}
			scope := fmt.Sprintf("m185-cost-%d", cell)
			m182Exec(t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, tn.ID)
			var keys []string
			for i := 0; i < m185CellKeys; i++ {
				k, _, err := store.CreateApiKey(ctx, pool, fmt.Sprintf("m185-cost-%d-%d", cell, i), scope, nil, tn.ID)
				if err != nil {
					t.Fatal(err)
				}
				var principal string
				if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, k.ID).Scan(&principal); err != nil {
					t.Fatal(err)
				}
				m185SeedKeyRows(t, pool, k.ID, principal)
				keys = append(keys, k.ID)
			}
			epool, rec := m184ExplainPool(t, dsn, mode)
			t0 := time.Now()
			err = store.PruneTenant(ctx, epool, tn.ID, store.AuditStamp{})
			took := time.Since(t0)
			epool.Close()
			if err != nil {
				t.Fatalf("%s/%s: prune: %v", st.name, mode, err)
			}
			var left int64
			if err := pool.QueryRow(ctx, `SELECT
				(SELECT count(*) FROM context_api_keys WHERE id = ANY($1::uuid[])) +
				(SELECT count(*) FROM context_write_log WHERE api_key_id = ANY($1::uuid[])) +
				(SELECT count(*) FROM context_block_grants WHERE granted_by = ANY($1::uuid[])) +
				(SELECT count(*) FROM context_chat_sessions WHERE created_by = ANY($1::uuid[])) +
				(SELECT count(*) FROM context_oauth_codes WHERE api_key_id = ANY($1::uuid[]))`, keys).Scan(&left); err != nil {
				t.Fatal(err)
			}
			if left != 0 {
				t.Errorf("%s/%s: %d rows still name the pruned keys", st.name, mode, left)
			}
			stats := m184Lookups(t, rec, lookups, m185LookupMaxBuffers)
			for _, l := range lookups {
				s := stats[l.table]
				t.Logf("MEASURE|185 key delete|schema=%d|stats=%s|mode=%s|lookup=%s.%s|n=%d|generic=%d|hits=%d|max_buffers=%d|avg_buffers=%.1f|prune=%s|shapes=%v",
					schema, st.name, mode, l.table, l.col, s.n, s.generic, s.hits, s.maxBuffers, float64(s.sumBuffers)/float64(max(s.n, 1)), took.Round(time.Millisecond), s.shapes)
				if s.n != m185CellKeys {
					t.Errorf("%s/%s: auto_explain reported %d lookups on %s.%s, want one per deleted key (%d)", st.name, mode, s.n, l.table, l.col, m185CellKeys)
				}
				if len(s.bad) > 0 {
					t.Errorf("%s/%s: lookups on %s.%s not served by %s within %d buffers: %v", st.name, mode, l.table, l.col, l.index, m185LookupMaxBuffers, s.bad)
				}
			}
		}
	}
}

// m185DeleteOrder is the order in which a delete on parent reaches the four
// tables of 185: its foreign-key triggers fire in the order of their names
// (byte order), and each one locks its table (RowExclusiveLock) until the
// transaction ends.
func m185DeleteOrder(t *testing.T, pool *pgxpool.Pool, parent string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `
		SELECT c.conrelid::regclass::text FROM pg_trigger tg JOIN pg_constraint c ON c.oid = tg.tgconstraint
		 WHERE tg.tgrelid = $1::regclass AND tg.tgisinternal AND c.contype = 'f' AND c.confrelid = $1::regclass
		   AND tg.tgtype & 8 = 8
		   AND c.conrelid::regclass::text IN ('context_write_log', 'context_block_grants', 'context_chat_sessions', 'context_oauth_codes')
		 ORDER BY tg.tgname COLLATE "C"`, parent)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for rows.Next() {
		var rel string
		if err := rows.Scan(&rel); err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(order, rel) {
			order = append(order, rel)
		}
	}
	rows.Close()
	return order
}

// m185Recreate drops and re-adds the delete-action foreign keys of the given
// (table, column) pairs that point at parent, in that order: the triggers get
// new oids, and the delete reaches the tables in that order (the order of a
// store grown through the releases).
func m185Recreate(t *testing.T, pool *pgxpool.Pool, parent string, cols [][2]string) {
	t.Helper()
	ctx := context.Background()
	for _, tc := range cols {
		var name, def string
		if err := pool.QueryRow(ctx, `SELECT c.conname::text, pg_get_constraintdef(c.oid) FROM pg_constraint c
			 WHERE c.conrelid = $1::regclass AND c.confrelid = $2::regclass AND c.contype = 'f'
			   AND c.conkey = ARRAY[(SELECT attnum FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $3)]::int2[]`,
			tc[0], parent, tc[1]).Scan(&name, &def); err != nil {
			t.Fatalf("%s.%s: %v", tc[0], tc[1], err)
		}
		m182Exec(t, pool, fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT %s, ADD CONSTRAINT %s %s`, tc[0], name, name, def))
	}
}

// m185CycleRun is what the file and a delete did when run against each other.
type m185CycleRun struct {
	fileErr, deleteErr error
	fileTook, delTook  time.Duration
}

const (
	// m185CycleWithin bounds each wait of m185Cycle for a lock state; the
	// states show within milliseconds, and the file's lock_timeout (3 s) ends
	// its wait for context_oauth_codes. m185CycleEnd bounds the wait for both
	// sides to end once the holder committed.
	m185CycleWithin = 10 * time.Second
	m185CycleEnd    = 30 * time.Second
	// m185DeleteApp names the delete's connections in pg_stat_activity.
	m185DeleteApp = "m185-cycle-delete"
)

// m185Lock is a lock of the file or the delete in m185Cycle: a relation lock
// on one of the four tables, or any request that waits.
type m185Lock struct {
	who, rel, mode string
	granted        bool
}

func (l m185Lock) String() string {
	if l.granted {
		return l.who + " holds " + l.mode + " on " + l.rel
	}
	return l.who + " waits for " + l.mode + " on " + l.rel
}

func m185Has(locks []m185Lock, who, rel, mode string, granted bool) bool {
	return slices.Contains(locks, m185Lock{who, rel, mode, granted})
}

// m185Cycle runs the file and a delete against each other: a third
// connection holds context_oauth_codes; once pg_locks shows the file holding
// context_block_grants (SHARE) and waiting for context_oauth_codes, the
// delete runs and locks the tables in its trigger order; once pg_locks shows
// it waiting for a table the file holds or waits for (a request for
// context_oauth_codes queues behind the file's), the holder commits. A state not
// reached within m185CycleWithin, or a side that ends before it, fails the
// test with the locks pg_locks showed. On every path both sides are
// cancelled (their contexts) and the holder rolled back before the test
// waits for them and closes their pools.
func m185Cycle(t *testing.T, pool *pgxpool.Pool, dsn string, body []byte, del func(ctx context.Context, conn *pgxpool.Pool) error) m185CycleRun {
	t.Helper()
	ctx := context.Background()
	holder, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(ctx) }()
	if _, err := holder.Exec(ctx, m185Targets[1].writer); err != nil {
		t.Fatal(err)
	}
	mpool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer mpool.Close()
	dcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	dcfg.ConnConfig.RuntimeParams["application_name"] = m185DeleteApp
	dpool, err := pgxpool.NewWithConfig(ctx, dcfg)
	if err != nil {
		t.Fatal(err)
	}
	defer dpool.Close()
	tx, err := mpool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var filePid int
	if err := tx.QueryRow(ctx, `SELECT pg_backend_pid()`).Scan(&filePid); err != nil {
		t.Fatal(err)
	}
	wctx, cancel := context.WithCancel(ctx)
	var started []chan struct{}
	// Deferred last, so it runs first on every path: both sides cancelled,
	// the holder rolled back, both waited for; then the file's transaction
	// and the pools.
	defer func() {
		cancel()
		_ = holder.Rollback(ctx)
		for _, ch := range started {
			<-ch
		}
	}()
	tables := make([]string, len(m185Targets))
	for i, tg := range m185Targets {
		tables[i] = tg.table
	}
	locks := func() []m185Lock {
		rows, err := pool.Query(ctx, `
			SELECT CASE WHEN l.pid = $1 THEN 'file' ELSE 'delete' END, COALESCE(l.relation::regclass::text, l.locktype), l.mode, l.granted
			  FROM pg_locks l LEFT JOIN pg_stat_activity a ON a.pid = l.pid
			 WHERE (l.pid = $1 OR (a.application_name = $2 AND a.datname = current_database()))
			   AND (NOT l.granted OR (l.locktype = 'relation' AND l.relation::regclass::text = ANY($3::text[])))
			 ORDER BY 1, 2, 3`, filePid, m185DeleteApp, tables)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []m185Lock
		for rows.Next() {
			var l m185Lock
			if err := rows.Scan(&l.who, &l.rel, &l.mode, &l.granted); err != nil {
				t.Fatal(err)
			}
			out = append(out, l)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	closed := func(ch chan struct{}) bool {
		select {
		case <-ch:
			return true
		default:
			return false
		}
	}
	var run m185CycleRun
	var delDone chan struct{}
	await := func(state string, reached func([]m185Lock) bool) {
		deadline := time.Now().Add(m185CycleWithin)
		for {
			ls := locks()
			switch {
			case reached(ls):
				return
			case closed(started[0]):
				t.Fatalf("the file ended with %v before %s; locks %v", run.fileErr, state, ls)
			case delDone != nil && closed(delDone):
				t.Fatalf("the delete ended with %v before %s; locks %v", run.deleteErr, state, ls)
			case time.Now().After(deadline):
				t.Fatalf("%s not seen within %s; locks %v", state, m185CycleWithin, ls)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
	started = append(started, make(chan struct{}))
	go func(done chan struct{}) {
		defer close(done)
		t0 := time.Now()
		_, err := tx.Exec(wctx, string(body))
		if err == nil {
			err = tx.Commit(wctx)
		} else {
			_ = tx.Rollback(ctx)
		}
		run.fileErr, run.fileTook = err, time.Since(t0)
	}(started[0])
	grants, codes := m185Targets[0].table, m185Targets[1].table
	await("the file holding SHARE on "+grants+" and waiting for "+codes, func(ls []m185Lock) bool {
		return m185Has(ls, "file", grants, "ShareLock", true) && m185Has(ls, "file", codes, "ShareLock", false)
	})
	delDone = make(chan struct{})
	started = append(started, delDone)
	go func() {
		defer close(delDone)
		t0 := time.Now()
		run.deleteErr = del(wctx, dpool)
		run.delTook = time.Since(t0)
	}()
	await("the delete waiting for a table the file holds or waits for", func(ls []m185Lock) bool {
		for _, l := range ls {
			if l.who == "delete" && !l.granted && (m185Has(ls, "file", l.rel, "ShareLock", true) || m185Has(ls, "file", l.rel, "ShareLock", false)) {
				return true
			}
		}
		return false
	})
	if err := holder.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	for _, ch := range started {
		select {
		case <-ch:
		case <-time.After(m185CycleEnd):
			t.Fatalf("the file or the delete did not end within %s; locks %v", m185CycleEnd, locks())
		}
	}
	return run
}

func m185Code(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	if err != nil {
		return "other"
	}
	return ""
}

// TestMigration185LockOrderAgainstDeletes runs the file against the deletes
// that lock its tables through their foreign-key triggers, a key delete
// (store.PruneTenant) and a block delete, with the interleaving that closes a
// cycle when a delete reaches a later table of the file before an earlier
// one: the file holds context_block_grants and waits (a writer holds
// context_oauth_codes), the delete locks the tables in its trigger order,
// the writer commits. In the trigger order of a store grown through the
// releases (grants before codes, write log and chats; re-created here) both
// commit. In the order of a fresh chain, where the write log is reached
// first, PostgreSQL detects the cycle and aborts one side (40P01); the other
// commits, and the aborted one changed nothing. A delete that reaches
// context_oauth_codes first queues behind the file's request for it and holds
// nothing the file takes later: both commit (codes_first, re-created here; a
// fresh chain gives that order too when the oids of its trigger names sort so).
func TestMigration185LockOrderAgainstDeletes(t *testing.T) {
	body, err := migrations.FS.ReadFile("185_api_key_fk_lookup_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	fileOrder := []string{"context_block_grants", "context_oauth_codes", "context_write_log", "context_chat_sessions"}
	// closes reports whether the delete closes a cycle with the file. The
	// delete stops at the first table the file holds (context_block_grants)
	// or waits for (context_oauth_codes); the cycle closes when it holds a
	// table the file takes after those by then.
	closes := func(order []string) bool {
		held := false
		for _, tbl := range order {
			switch i := slices.Index(fileOrder, tbl); {
			case i == 0 || i == 1:
				return held
			case i > 1:
				held = true
			}
		}
		return false
	}
	type deleteCase struct {
		name, parent string
		reference    [][2]string // the foreign keys re-created in the order of the grown store
		codesFirst   [][2]string // re-created so the delete reaches context_oauth_codes first (nil: it has no key there)
		setup        func(t *testing.T, pool *pgxpool.Pool) (del func(ctx context.Context, conn *pgxpool.Pool) error, gone func() bool)
	}
	keyDelete := func(t *testing.T, pool *pgxpool.Pool) (func(context.Context, *pgxpool.Pool) error, func() bool) {
		ctx := context.Background()
		tn, err := store.CreateTenant(ctx, pool, "m185-cycle", "m185 cycle")
		if err != nil {
			t.Fatal(err)
		}
		m182Exec(t, pool, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ('m185-cycle', $1::uuid)`, tn.ID)
		k, _, err := store.CreateApiKey(ctx, pool, "m185-cycle", "m185-cycle", nil, tn.ID)
		if err != nil {
			t.Fatal(err)
		}
		var principal string
		if err := pool.QueryRow(ctx, `SELECT principal_id::text FROM context_api_keys WHERE id = $1::uuid`, k.ID).Scan(&principal); err != nil {
			t.Fatal(err)
		}
		m185SeedKeyRows(t, pool, k.ID, principal)
		return func(ctx context.Context, conn *pgxpool.Pool) error {
				return store.PruneTenant(ctx, conn, tn.ID, store.AuditStamp{})
			}, func() bool {
				var n int
				_ = pool.QueryRow(ctx, `SELECT count(*) FROM context_api_keys WHERE id = $1::uuid`, k.ID).Scan(&n)
				return n == 0
			}
	}
	blockDelete := func(t *testing.T, pool *pgxpool.Pool) (func(context.Context, *pgxpool.Pool) error, func() bool) {
		ctx := context.Background()
		var block string
		if err := pool.QueryRow(ctx, `SELECT block_id::text FROM context_block_grants ORDER BY id LIMIT 1`).Scan(&block); err != nil {
			t.Fatal(err)
		}
		m182Exec(t, pool, `INSERT INTO context_write_log (block_id, decision, scope, block_title, block_category) VALUES ($1::uuid, 'clean', 'private', 'm185', 'm185')`, block)
		return func(ctx context.Context, conn *pgxpool.Pool) error {
				_, err := conn.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, block)
				return err
			}, func() bool {
				var n int
				_ = pool.QueryRow(ctx, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, block).Scan(&n)
				return n == 0
			}
	}
	for _, dc := range []deleteCase{
		{"key_delete", "context_api_keys", [][2]string{
			{"context_block_grants", "granted_by"}, {"context_oauth_codes", "api_key_id"},
			{"context_write_log", "api_key_id"}, {"context_chat_sessions", "created_by"},
		}, [][2]string{
			{"context_oauth_codes", "api_key_id"}, {"context_write_log", "api_key_id"},
			{"context_chat_sessions", "created_by"}, {"context_block_grants", "granted_by"},
		}, keyDelete},
		{"block_delete", "context_blocks", [][2]string{
			{"context_block_grants", "block_id"}, {"context_write_log", "block_id"}, {"context_write_log", "matched_block_id"},
		}, nil, blockDelete},
	} {
		orders := []string{"fresh_chain", "grown_store"}
		if dc.codesFirst != nil {
			orders = append(orders, "codes_first")
		}
		for _, order := range orders {
			t.Run(dc.name+"_"+order, func(t *testing.T) {
				pool, dsn := testdb.SetupTestDBUpToWithDSN(t, 184)
				m185Seed(t, pool)
				switch order {
				case "grown_store":
					m185Recreate(t, pool, dc.parent, dc.reference)
				case "codes_first":
					m185Recreate(t, pool, dc.parent, dc.codesFirst)
				}
				got := m185DeleteOrder(t, pool, dc.parent)
				cycle := closes(got)
				t.Logf("a delete on %s reaches %v (cycle with the file's order %v: %v)", dc.parent, got, fileOrder, cycle)
				if order == "grown_store" && cycle {
					t.Fatalf("fixture: the re-created keys did not give the order of the grown store: %v", got)
				}
				if order == "codes_first" && (len(got) == 0 || got[0] != "context_oauth_codes") {
					t.Fatalf("fixture: the re-created keys did not reach context_oauth_codes first: %v", got)
				}
				del, gone := dc.setup(t, pool)
				run := m185Cycle(t, pool, dsn, body, del)
				fc, dcode := m185Code(run.fileErr), m185Code(run.deleteErr)
				built := 0
				for _, tg := range m185Targets {
					if m182Index(t, pool, tg.name).found {
						built++
					}
				}
				t.Logf("file: %q after %s; delete: %q after %s; indexes built %d; deleted %v", fc, run.fileTook.Round(time.Millisecond), dcode, run.delTook.Round(time.Millisecond), built, gone())
				if !cycle {
					if fc != "" || dcode != "" || built != len(m185Targets) || !gone() {
						t.Errorf("without a cycle both must commit: file %v, delete %v, built %d, deleted %v", run.fileErr, run.deleteErr, built, gone())
					}
					return
				}
				switch {
				case fc == "40P01" && dcode == "":
					if built != 0 || !gone() {
						t.Errorf("the file was aborted: want nothing built (got %d) and the delete committed (deleted %v)", built, gone())
					}
				case dcode == "40P01" && fc == "":
					if built != len(m185Targets) || gone() {
						t.Errorf("the delete was aborted: want the file committed (built %d) and nothing deleted (deleted %v)", built, gone())
					}
				default:
					t.Errorf("a cycle must end with one side aborted (40P01) and the other committed: file %v, delete %v", run.fileErr, run.deleteErr)
				}
			})
		}
	}
}
