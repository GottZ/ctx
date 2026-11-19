//go:build integration

package store_test

// The measurements behind the thresholds and runbooks of migrations 183 to
// 187 (not gates; each test skips unless its variable is set):
//
//	CTX_M183_MEASURE=1  context_blocks: 183's DROP, 186's inline validation,
//	                    187's inline build, each in one transaction beside a
//	                    writer and a reader on second connections, at each
//	                    size of CTX_M183_ROWS (default 250000,500000,1000000);
//	                    at the last size the out-of-band forms (DROP INDEX
//	                    CONCURRENTLY, VALIDATE in its own transaction, CREATE
//	                    INDEX CONCURRENTLY).
//	CTX_M184_MEASURE=1  the token and session tables (184) and the four
//	                    tables of 185, the same way.
//
// Every line that carries a number starts with MEASURE|.

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// mwSizes reads a comma-separated list of row counts from env, else def.
func mwSizes(t *testing.T, env string, def []int) []int {
	t.Helper()
	v := os.Getenv(env)
	if v == "" {
		return def
	}
	var out []int
	for _, f := range strings.Split(v, ",") {
		var n int
		if _, err := fmt.Sscan(f, &n); err != nil {
			t.Fatal(err)
		}
		out = append(out, n)
	}
	return out
}

func mwExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

func mwLoad() string {
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		return strings.Join(strings.Fields(string(b))[:3], " ")
	}
	return "?"
}

// mwLoop runs stmt every 2 ms on pool until stop is closed and returns the
// longest single run and the count; a failing statement fails the test.
type mwLoop struct {
	stop    chan struct{}
	done    chan struct{}
	mu      sync.Mutex
	longest time.Duration
	n       int
}

func mwStart(t *testing.T, pool *pgxpool.Pool, stmt func(i int) (string, []any)) *mwLoop {
	t.Helper()
	l := &mwLoop{stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		for i := 0; ; i++ {
			select {
			case <-l.stop:
				return
			default:
			}
			sql, args := stmt(i)
			w0 := time.Now()
			if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
				t.Errorf("loop %q: %v", sql, err)
				return
			}
			d := time.Since(w0)
			l.mu.Lock()
			if d > l.longest {
				l.longest = d
			}
			l.n++
			l.mu.Unlock()
			time.Sleep(2 * time.Millisecond)
		}
	}()
	return l
}

func (l *mwLoop) end() (time.Duration, int) {
	close(l.stop)
	<-l.done
	return l.longest, l.n
}

// mwTimedTx runs the statements in one transaction (lock_timeout 3 s, as the
// runner's files set it) beside the loops and returns the time from the
// first statement to the commit and each loop's longest wait.
func mwTimedTx(t *testing.T, pool *pgxpool.Pool, stmts []string, loops ...func(i int) (string, []any)) (time.Duration, []time.Duration) {
	t.Helper()
	ctx := context.Background()
	var ls []*mwLoop
	for _, f := range loops {
		ls = append(ls, mwStart(t, pool, f))
	}
	time.Sleep(200 * time.Millisecond)
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t0 := time.Now()
	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		t.Fatal(err)
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", s, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	took := time.Since(t0)
	time.Sleep(200 * time.Millisecond)
	var waits []time.Duration
	for _, l := range ls {
		w, _ := l.end()
		waits = append(waits, w.Round(time.Millisecond))
	}
	return took.Round(time.Millisecond), waits
}

// mwTimed runs one statement outside a transaction (CONCURRENTLY) beside the
// loops.
func mwTimed(t *testing.T, pool *pgxpool.Pool, stmt string, loops ...func(i int) (string, []any)) (time.Duration, []time.Duration) {
	t.Helper()
	var ls []*mwLoop
	for _, f := range loops {
		ls = append(ls, mwStart(t, pool, f))
	}
	time.Sleep(200 * time.Millisecond)
	t0 := time.Now()
	mwExec(t, pool, stmt)
	took := time.Since(t0)
	time.Sleep(200 * time.Millisecond)
	var waits []time.Duration
	for _, l := range ls {
		w, _ := l.end()
		waits = append(waits, w.Round(time.Millisecond))
	}
	return took.Round(time.Millisecond), waits
}

const (
	mw183Old = `CREATE INDEX idx_guard_pending ON context_blocks (created_at)
		WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL`
	mw187Build = `CREATE INDEX idx_context_derived_identity ON context_blocks (category, title COLLATE "C", scope)
		WHERE metadata ? 'provenance'`
)

// mw186Inline is the inline path of 186 on a store without a row lacking a
// value: both constraints NOT VALID, the index-driven repair, one VALIDATE.
var mw186Inline = []string{
	`ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_created_at_not_null NOT NULL created_at NOT VALID`,
	`UPDATE context_blocks SET created_at = COALESCE(uuid_extract_timestamp(id), updated_at, now()) WHERE created_at IS NULL`,
	`ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_updated_at_not_null NOT NULL updated_at NOT VALID`,
	`ALTER TABLE context_blocks VALIDATE CONSTRAINT context_blocks_created_at_not_null, VALIDATE CONSTRAINT context_blocks_updated_at_not_null`,
}

// mw186CreatedOnly is the same for created_at alone (the comparison: one
// scan for both or one per constraint).
var mw186CreatedOnly = []string{
	`ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_created_at_not_null NOT NULL created_at NOT VALID`,
	`UPDATE context_blocks SET created_at = COALESCE(uuid_extract_timestamp(id), updated_at, now()) WHERE created_at IS NULL`,
	`ALTER TABLE context_blocks VALIDATE CONSTRAINT context_blocks_created_at_not_null`,
}

const mw186Undo = `ALTER TABLE context_blocks ALTER COLUMN created_at DROP NOT NULL, ALTER COLUMN updated_at DROP NOT NULL`

func TestMigrationWave183MeasureBlocks(t *testing.T) {
	if os.Getenv("CTX_M183_MEASURE") == "" {
		t.Skip("measurement; set CTX_M183_MEASURE=1")
	}
	ctx := context.Background()
	sizes := mwSizes(t, "CTX_M183_ROWS", []int{250000, 500000, 1000000})
	pool := testdb.SetupTestDBUpTo(t, 182)
	// The HNSW index is read by none of the statements measured; a
	// dimensionless one-element vector keeps the seed in minutes (the guard
	// cost tests of 182 do the same).
	mwExec(t, pool, `DROP INDEX IF EXISTS idx_embedding_hnsw`)
	mwExec(t, pool, `ALTER TABLE context_blocks ALTER COLUMN embedding TYPE vector`)
	writer := func(i int) (string, []any) {
		return `INSERT INTO context_blocks (category, title, content, scope) VALUES ('mw', $1, 'writer', 'mw-w')`, []any{fmt.Sprintf("mw writer %d %d", time.Now().UnixNano(), i)}
	}
	var probeID string
	reader := func(int) (string, []any) {
		return `SELECT title FROM context_blocks WHERE id = $1`, []any{probeID}
	}
	have := 0
	for si, rows := range sizes {
		t0 := time.Now()
		mwExec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
		mwExec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER trg_block_fulltext`)
		// Rows of the live width (about 1.5 KB of heap); 5 % with a vector
		// and no verdict (entries of idx_guard_pending), 5 % derivatives.
		mwExec(t, pool, `INSERT INTO context_blocks (category, title, content, scope, type_name, embedding, embed_model, metadata, created_at, updated_at)
			SELECT 'mw', 'mw ' || g, repeat('lorem ipsum dolor sit amet ', 40) || g, 'mw-' || (g % 100),
			       CASE WHEN g % 3 = 0 THEN 'reference' ELSE 'knowledge' END,
			       CASE WHEN g % 20 = 0 THEN '[0.01]'::vector END, CASE WHEN g % 20 = 0 THEN 'q183' END,
			       CASE WHEN g % 20 = 1 THEN jsonb_build_object('pad', repeat('m', 260), 'provenance', jsonb_build_object('v', 1, 'arm', 'mw', 'stratum', 1))
			            ELSE jsonb_build_object('pad', repeat('m', 260)) END,
			       now() - g * interval '1 second', now() - g * interval '1 second'
			  FROM generate_series($1::int + 1, $2::int) g`, have, rows)
		have = rows
		mwExec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
		mwExec(t, pool, `VACUUM ANALYZE context_blocks`)
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = 'mw 1'`).Scan(&probeID); err != nil {
			t.Fatal(err)
		}
		var heap, oldIdx int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size('context_blocks'), pg_relation_size('idx_guard_pending')`).Scan(&heap, &oldIdx); err != nil {
			t.Fatal(err)
		}
		t.Logf("MEASURE|seed|rows=%d|heap_mb=%d|idx_guard_pending_kb=%d|took=%s|load=%s", rows, heap>>20, oldIdx>>10, time.Since(t0).Round(time.Second), mwLoad())
		for round := 1; round <= 3; round++ {
			// 183: the DROP of idx_guard_pending, ACCESS EXCLUSIVE to the commit.
			took, waits := mwTimedTx(t, pool, []string{`DROP INDEX idx_guard_pending`}, writer, reader)
			t.Logf("MEASURE|183 inline drop|rows=%d|round=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, round, took, waits[0], waits[1], mwLoad())
			mwExec(t, pool, mw183Old)
			// 186: the inline path, both columns, and created_at alone.
			took, waits = mwTimedTx(t, pool, mw186Inline, writer, reader)
			t.Logf("MEASURE|186 inline both|rows=%d|round=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, round, took, waits[0], waits[1], mwLoad())
			mwExec(t, pool, mw186Undo)
			took, waits = mwTimedTx(t, pool, mw186CreatedOnly, writer, reader)
			t.Logf("MEASURE|186 inline created_at only|rows=%d|round=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, round, took, waits[0], waits[1], mwLoad())
			mwExec(t, pool, mw186Undo)
			// 187: the inline build, SHARE to the commit.
			took, waits = mwTimedTx(t, pool, []string{mw187Build}, writer, reader)
			t.Logf("MEASURE|187 inline build|rows=%d|round=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, round, took, waits[0], waits[1], mwLoad())
			mwExec(t, pool, `DROP INDEX idx_context_derived_identity`)
		}
		if si != len(sizes)-1 {
			continue
		}
		// The out-of-band forms at the largest size.
		took, waits := mwTimed(t, pool, `DROP INDEX CONCURRENTLY idx_guard_pending`, writer, reader)
		t.Logf("MEASURE|183 runbook drop concurrently|rows=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, took, waits[0], waits[1], mwLoad())
		mwExec(t, pool, mw183Old)
		mwExec(t, pool, `ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_created_at_not_null NOT NULL created_at NOT VALID`)
		mwExec(t, pool, `ALTER TABLE context_blocks ADD CONSTRAINT context_blocks_updated_at_not_null NOT NULL updated_at NOT VALID`)
		took, waits = mwTimed(t, pool, `ALTER TABLE context_blocks VALIDATE CONSTRAINT context_blocks_created_at_not_null, VALIDATE CONSTRAINT context_blocks_updated_at_not_null`, writer, reader)
		t.Logf("MEASURE|186 runbook validate both|rows=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, took, waits[0], waits[1], mwLoad())
		mwExec(t, pool, mw186Undo)
		took, waits = mwTimed(t, pool, `UPDATE context_blocks SET updated_at = COALESCE(created_at, uuid_extract_timestamp(id), now()) WHERE updated_at IS NULL`, writer, reader)
		t.Logf("MEASURE|186 runbook updated_at repair scan|rows=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, took, waits[0], waits[1], mwLoad())
		took, waits = mwTimed(t, pool, strings.Replace(mw187Build, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1), writer, reader)
		t.Logf("MEASURE|187 runbook build concurrently|rows=%d|took=%s|writer_wait=%s|reader_wait=%s|load=%s", rows, took, waits[0], waits[1], mwLoad())
		var entries float64
		if err := pool.QueryRow(ctx, `SELECT reltuples FROM pg_class WHERE relname = 'idx_context_derived_identity'`).Scan(&entries); err != nil {
			t.Fatal(err)
		}
		t.Logf("MEASURE|187 index entries|rows=%d|entries=%.0f", rows, entries)
	}
}

func TestMigrationWave183MeasureKeys(t *testing.T) {
	if os.Getenv("CTX_M184_MEASURE") == "" {
		t.Skip("measurement; set CTX_M184_MEASURE=1")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDBUpTo(t, 183)
	var keys []string
	for i := 0; i < 20; i++ {
		k, _, err := store.CreateApiKey(ctx, pool, fmt.Sprintf("mw-key-%d", i), "private", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		keys = append(keys, k.ID)
	}
	var keyID, principal string
	if err := pool.QueryRow(ctx, `SELECT id, principal_id FROM context_api_keys WHERE id = $1::uuid`, keys[0]).Scan(&keyID, &principal); err != nil {
		t.Fatal(err)
	}

	// 184: login families of two generations (as store.RotateRefreshToken
	// leaves them), one web session per family on the current access token.
	seedTokens := func(from, families int) {
		mwExec(t, pool, `
			WITH k AS (SELECT array_agg(id ORDER BY id) AS ids, array_agg(principal_id ORDER BY id) AS ps FROM context_api_keys WHERE id = ANY($3::uuid[])),
			f AS (SELECT g, md5('fam-' || g)::uuid AS fam, k.ids[1 + g % cardinality(k.ids)] AS key_id, k.ps[1 + g % cardinality(k.ids)] AS principal,
			             now() - (g % 6) * interval '1 day' AS t0
			        FROM k, generate_series($1::int + 1, $1::int + $2::int) g)
			INSERT INTO context_access_tokens (id, token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, parent_id, issued_via, expires_at, created_at)
			SELECT md5(x.tag || '-' || f.g)::uuid, encode(sha256(convert_to(x.tag || '-' || f.g, 'UTF8')), 'hex'), x.typ, f.key_id, f.principal,
			       ARRAY['ctx-mcp', 'ctx-web'], f.fam, CASE WHEN x.gen = 1 THEN md5('r0-' || f.g)::uuid END, 'login',
			       f.t0 + interval '30 days', f.t0 + x.gen * interval '1 hour'
			  FROM f, (VALUES ('a0', 'access', 0), ('r0', 'refresh', 0), ('a1', 'access', 1), ('r1', 'refresh', 1)) AS x(tag, typ, gen)
			 ORDER BY f.g, x.gen`, from, families, keys)
		mwExec(t, pool, `
			INSERT INTO context_web_sessions (principal_id, access_token_id, refresh_family, csrf_secret, created_at, last_used_at, user_agent)
			SELECT t.principal_id, t.id, t.refresh_family, md5(t.id::text), t.created_at, t.created_at, 'Mozilla/5.0 (X11; Linux x86_64) Chrome/131.0'
			  FROM context_access_tokens t
			 WHERE t.token_type = 'access' AND t.parent_id IS NOT NULL
			   AND t.id IN (SELECT md5('a1-' || g)::uuid FROM generate_series($1::int + 1, $1::int + $2::int) g)`, from, families)
		mwExec(t, pool, `VACUUM ANALYZE context_access_tokens`)
		mwExec(t, pool, `VACUUM ANALYZE context_web_sessions`)
	}
	var sessionID string
	tokenWriter := func(i int) (string, []any) {
		tag := fmt.Sprintf("mw-w-%d-%d", time.Now().UnixNano(), i)
		return `INSERT INTO context_access_tokens (token_hash, token_type, api_key_id, principal_id, audiences, refresh_family, issued_via, expires_at)
			VALUES (encode(sha256(convert_to($1, 'UTF8')), 'hex'), 'access', $2::uuid, $3::uuid, ARRAY['ctx-mcp'], gen_random_uuid(), 'oauth', now() + interval '1 hour')`,
			[]any{tag, keyID, principal}
	}
	sessionWriter := func(int) (string, []any) {
		return `UPDATE context_web_sessions SET last_used_at = now() WHERE id = $1::uuid`, []any{sessionID}
	}
	tokIdx := []string{
		`CREATE INDEX idx_access_tokens_parent ON context_access_tokens (parent_id) WHERE parent_id IS NOT NULL`,
		`CREATE INDEX idx_web_sessions_access_token ON context_web_sessions (access_token_id)`,
	}
	have := 0
	for _, families := range mwSizes(t, "CTX_M184_FAMILIES", []int{125000, 250000}) {
		seedTokens(have, families-have)
		have = families
		if err := pool.QueryRow(ctx, `SELECT id FROM context_web_sessions LIMIT 1`).Scan(&sessionID); err != nil {
			t.Fatal(err)
		}
		var tokens, sessions int64
		if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_access_tokens), (SELECT count(*) FROM context_web_sessions)`).Scan(&tokens, &sessions); err != nil {
			t.Fatal(err)
		}
		for round := 1; round <= 3; round++ {
			took, waits := mwTimedTx(t, pool, tokIdx, tokenWriter, sessionWriter)
			t.Logf("MEASURE|184 inline both|tokens=%d|sessions=%d|round=%d|took=%s|token_writer_wait=%s|session_writer_wait=%s|load=%s", tokens, sessions, round, took, waits[0], waits[1], mwLoad())
			mwExec(t, pool, `DROP INDEX idx_access_tokens_parent`)
			mwExec(t, pool, `DROP INDEX idx_web_sessions_access_token`)
		}
		for _, s := range tokIdx {
			took, waits := mwTimed(t, pool, strings.Replace(s, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1), tokenWriter, sessionWriter)
			t.Logf("MEASURE|184 runbook concurrently|tokens=%d|sessions=%d|%s|took=%s|token_writer_wait=%s|session_writer_wait=%s|load=%s", tokens, sessions, s[:45], took, waits[0], waits[1], mwLoad())
		}
		mwExec(t, pool, `DROP INDEX idx_access_tokens_parent`)
		mwExec(t, pool, `DROP INDEX idx_web_sessions_access_token`)
	}
	mwExec(t, pool, `TRUNCATE context_web_sessions, context_access_tokens`)

	// 185: the write log at each size of CTX_M185_WRITE_LOG (rows of the
	// guard's and dream's width, every api_key_id NULL as on live), the other
	// three tables at CTX_M185_OTHER rows.
	mwExec(t, pool, `INSERT INTO context_blocks (category, title, content, scope) SELECT 'mw', 'mw ' || g, 'x', 'private' FROM generate_series(1, 2000) g`)
	logWriter := func(int) (string, []any) {
		return `INSERT INTO context_write_log (decision, scope, block_title, block_category, metadata) VALUES ('clean', 'private', 'mw writer', 'mw', '{"threshold_duplicate": 0.92}')`, nil
	}
	have = 0
	for _, n := range mwSizes(t, "CTX_M185_WRITE_LOG", []int{1000000, 2000000}) {
		mwExec(t, pool, `
			WITH b AS (SELECT array_agg(id ORDER BY id) AS a FROM context_blocks)
			INSERT INTO context_write_log (block_id, matched_block_id, decision, similarity, scope, block_title, block_category, metadata, created_at)
			SELECT b.a[1 + g % cardinality(b.a)], CASE WHEN g % 30 = 0 THEN b.a[1 + (g * 7) % cardinality(b.a)] END,
			       CASE WHEN g % 10 = 0 THEN 'clean' ELSE 'dream_link' END, 0.8, 'private',
			       'mw write-log title of moderate length number ' || g, 'learnings',
			       jsonb_build_object('source', 'dream_v1', 'links_created', 4, 'links', jsonb_build_array(
			           jsonb_build_object('target_id', b.a[1 + (g * 3) % cardinality(b.a)]::text, 'confidence', 0.9500197690538208, 'relationship', 'topical'),
			           jsonb_build_object('target_id', b.a[1 + (g * 5) % cardinality(b.a)]::text, 'confidence', 0.8812345678901234, 'relationship', 'supports'))),
			       now() - g * interval '1 second'
			  FROM b, generate_series($1::int + 1, $2::int) g`, have, n)
		have = n
		mwExec(t, pool, `VACUUM ANALYZE context_write_log`)
		var heap int64
		if err := pool.QueryRow(ctx, `SELECT pg_relation_size('context_write_log')`).Scan(&heap); err != nil {
			t.Fatal(err)
		}
		const build = `CREATE INDEX idx_write_log_api_key ON context_write_log (api_key_id) WHERE api_key_id IS NOT NULL`
		for round := 1; round <= 3; round++ {
			took, waits := mwTimedTx(t, pool, []string{build}, logWriter)
			t.Logf("MEASURE|185 inline write log|rows=%d|heap_mb=%d|round=%d|took=%s|writer_wait=%s|load=%s", n, heap>>20, round, took, waits[0], mwLoad())
			mwExec(t, pool, `DROP INDEX idx_write_log_api_key`)
		}
		took, waits := mwTimed(t, pool, strings.Replace(build, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1), logWriter)
		t.Logf("MEASURE|185 runbook write log concurrently|rows=%d|heap_mb=%d|took=%s|writer_wait=%s|load=%s", n, heap>>20, took, waits[0], mwLoad())
		mwExec(t, pool, `DROP INDEX idx_write_log_api_key`)
	}
	other := mwSizes(t, "CTX_M185_OTHER", []int{500000})[0]
	var tenant string
	if err := pool.QueryRow(ctx, `SELECT id FROM context_tenants LIMIT 1`).Scan(&tenant); err != nil {
		t.Fatal(err)
	}
	mwExec(t, pool, `INSERT INTO context_chat_sessions (scope, read_scopes, created_by, title, metadata)
		SELECT 'private', ARRAY['private'], CASE WHEN g % 2 = 0 THEN $2::uuid END, 'chat ' || g, '{}' FROM generate_series(1, $1::int) g`, other, keyID)
	mwExec(t, pool, `INSERT INTO context_oauth_codes (code_hash, api_key_id, principal_id, redirect_uri, code_challenge, expires_at)
		SELECT md5('code-' || g) || md5('c2-' || g), $2::uuid, $3::uuid, 'https://example.invalid/cb', md5('ch-' || g), now() + interval '1 minute'
		  FROM generate_series(1, $1::int) g`, other, keyID, principal)
	mwExec(t, pool, `ALTER TABLE context_blocks DISABLE TRIGGER USER`)
	mwExec(t, pool, `INSERT INTO context_blocks (category, title, content, scope) SELECT 'mwg', 'mwg ' || g, 'x', 'private' FROM generate_series(1, $1::int) g`, other)
	mwExec(t, pool, `ALTER TABLE context_blocks ENABLE TRIGGER USER`)
	mwExec(t, pool, `INSERT INTO context_block_grants (block_id, grantee_tenant, granted_by)
		SELECT id, $1::uuid, $2::uuid FROM context_blocks WHERE category = 'mwg'`, tenant, keyID)
	for _, tbl := range []string{"context_chat_sessions", "context_oauth_codes", "context_block_grants"} {
		mwExec(t, pool, `VACUUM ANALYZE `+tbl)
	}
	var chatID string
	if err := pool.QueryRow(ctx, `SELECT id FROM context_chat_sessions LIMIT 1`).Scan(&chatID); err != nil {
		t.Fatal(err)
	}
	chatWriter := func(int) (string, []any) {
		return `UPDATE context_chat_sessions SET updated_at = now() WHERE id = $1::uuid`, []any{chatID}
	}
	// In the order of 185.
	all := []string{
		`CREATE INDEX idx_block_grants_granted_by ON context_block_grants (granted_by) WHERE granted_by IS NOT NULL`,
		`CREATE INDEX idx_oauth_codes_api_key ON context_oauth_codes (api_key_id)`,
		`CREATE INDEX idx_write_log_api_key ON context_write_log (api_key_id) WHERE api_key_id IS NOT NULL`,
		`CREATE INDEX idx_chat_sessions_created_by ON context_chat_sessions (created_by) WHERE created_by IS NOT NULL`,
	}
	for round := 1; round <= 3; round++ {
		took, waits := mwTimedTx(t, pool, all, logWriter, chatWriter)
		t.Logf("MEASURE|185 inline all four|write_log=%d|others=%d|round=%d|took=%s|write_log_writer_wait=%s|chat_writer_wait=%s|load=%s", have, other, round, took, waits[0], waits[1], mwLoad())
		for _, n := range []string{"idx_write_log_api_key", "idx_block_grants_granted_by", "idx_chat_sessions_created_by", "idx_oauth_codes_api_key"} {
			mwExec(t, pool, `DROP INDEX `+n)
		}
	}
	// Each of the three small ones alone.
	for _, s := range []string{all[0], all[1], all[3]} {
		took, _ := mwTimedTx(t, pool, []string{s})
		t.Logf("MEASURE|185 inline one|others=%d|%s|took=%s|load=%s", other, s[13:45], took, mwLoad())
	}
}
