//go:build integration

// Wave W-G (plan-cluster-topicmap design/02 §3.3/§6.1, §7 "W-G"; user decision
// E8-02 A): context_digest_state, mark_digest_dirty() and trg_digest_dirty are
// gone.
//
// The wave removes a per-write UPDATE on a SINGLETON row — every writer took the
// same row lock, which at the target scale is a pure serialisation point with no
// consumer on the other side. Three things therefore have to be proved, not
// assumed: that nothing reads the state (or the migration pulls the floor out
// from under someone), that the write path is healthy without the trigger, and
// that the digest itself produces the SAME BYTES with and without it.
package digest_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// digestStateDDL is the 001_initial.sql fragment this migration removes,
// verbatim enough to be re-installable. The golden gate below re-creates it in
// order to compare the digest output WITH the trigger against the output
// WITHOUT it — the only form in which "byte-identical" is a measurement rather
// than a hope, because a fresh container has already run migration 129.
const digestStateDDL = `
CREATE TABLE IF NOT EXISTS context_digest_state (
    id              BOOLEAN PRIMARY KEY DEFAULT true CHECK (id),
    dirty_since     TIMESTAMPTZ,
    last_digest_at  TIMESTAMPTZ
);
INSERT INTO context_digest_state (id) VALUES (true) ON CONFLICT DO NOTHING;

CREATE OR REPLACE FUNCTION mark_digest_dirty()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        IF OLD.category = 'index' THEN RETURN OLD; END IF;
    ELSE
        IF NEW.category = 'index' THEN RETURN NEW; END IF;
    END IF;
    UPDATE context_digest_state SET dirty_since = COALESCE(dirty_since, now());
    IF TG_OP = 'DELETE' THEN RETURN OLD; ELSE RETURN NEW; END IF;
END;
$$;

DROP TRIGGER IF EXISTS trg_digest_dirty ON context_blocks;
CREATE TRIGGER trg_digest_dirty
    AFTER INSERT OR UPDATE OR DELETE ON context_blocks
    FOR EACH ROW
    EXECUTE FUNCTION mark_digest_dirty();
`

func objectExists(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&ok); err != nil {
		t.Fatalf("probe %q: %v", sql, err)
	}
	return ok
}

func stateObjectsPresent(t *testing.T, pool *pgxpool.Pool) (table, fn, trg bool) {
	t.Helper()
	table = objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM information_schema.tables
	                                 WHERE table_schema = 'public' AND table_name = 'context_digest_state')`)
	fn = objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_proc WHERE proname = 'mark_digest_dirty')`)
	trg = objectExists(t, pool, `SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgname = 'trg_digest_dirty')`)
	return table, fn, trg
}

// W-G-1 — all three objects are gone after the migration chain, and a block
// write still works. The write half is not decoration: the trigger function
// resolves its table by NAME at execution time, so dropping the table without
// the trigger would fail no migration and every INSERT afterwards.
func TestDropDigestState_ObjectsGoneWritePathHealthy(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	if table, fn, trg := stateObjectsPresent(t, pool); table || fn || trg {
		t.Errorf("after migration 129: table=%v function=%v trigger=%v — all three must be gone", table, fn, trg)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', 'W-G write probe', 'x', 'private')`); err != nil {
		t.Fatalf("insert after the drop: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET content = 'y' WHERE title = 'W-G write probe'`); err != nil {
		t.Fatalf("update after the drop: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`DELETE FROM context_blocks WHERE title = 'W-G write probe'`); err != nil {
		t.Fatalf("delete after the drop: %v", err)
	}
}

// W-G-2 — THE BEHAVIOUR GOLDEN the wave hangs on: the full-mode digest produces
// the same bytes with and without the state machinery. Measured in three
// phases against ONE seeded corpus — without trigger, with it re-installed,
// without it again — so a difference cannot be blamed on the fixture.
func TestDropDigestState_DigestGoldenUnchanged(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i, title := range []string{"Alpha", "Beta", "Gamma", "Delta"} {
		cat := "learnings"
		if i%2 == 1 {
			cat = "decisions"
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope) VALUES ($1, $2, 'body', 'private')`,
			cat, title); err != nil {
			t.Fatalf("seed %s: %v", title, err)
		}
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	run := func(phase string) string {
		t.Helper()
		if err := digest.RunDigest(ctx, pool, reg, "full", "", "_global", "private", []string{"private"}); err != nil {
			t.Fatalf("%s: RunDigest: %v", phase, err)
		}
		var content string
		if err := pool.QueryRow(ctx,
			`SELECT content FROM context_blocks
			  WHERE category = 'index' AND title = 'topic-map-private' AND NOT is_archived`).Scan(&content); err != nil {
			t.Fatalf("%s: read map: %v", phase, err)
		}
		return content
	}

	// Two warm-up runs first. The map is itself a digest-included block
	// (system-meta), so the FIRST run adds a fifth block to the corpus the
	// SECOND one counts; only from there is the output a fixed point. Comparing
	// across that step would report the digest's own reflexivity as a trigger
	// effect.
	run("warm-up 1")
	run("warm-up 2")

	without := run("without trigger")
	if without == "" {
		t.Fatal("the digest wrote an empty map — the golden would be vacuous")
	}

	if _, err := pool.Exec(ctx, digestStateDDL); err != nil {
		t.Fatalf("re-install the 001 state machinery: %v", err)
	}
	if table, fn, trg := stateObjectsPresent(t, pool); !table || !fn || !trg {
		t.Fatalf("re-install incomplete (table=%v function=%v trigger=%v) — the comparison below would compare nothing", table, fn, trg)
	}
	with := run("with trigger")

	if _, err := pool.Exec(ctx, `
		DROP TRIGGER IF EXISTS trg_digest_dirty ON context_blocks;
		DROP FUNCTION IF EXISTS mark_digest_dirty();
		DROP TABLE IF EXISTS context_digest_state;`); err != nil {
		t.Fatalf("drop again: %v", err)
	}
	after := run("without trigger, again")

	if with != without {
		t.Errorf("the map differs with and without the state trigger:\n--- without ---\n%s\n--- with ---\n%s", without, with)
	}
	if after != without {
		t.Errorf("the map is not stable across the drop:\n--- first ---\n%s\n--- after ---\n%s", without, after)
	}
}

// W-G-3 — nobody reads the state. The pin exists so the migration cannot pull
// the floor out from under a consumer that appears later; it deliberately
// tolerates COMMENTS (the bench header names the trigger as the serialisation
// point this wave removes) and rejects code.
func TestDropDigestState_NoGoConsumer(t *testing.T) {
	root := filepath.Join("..", "..")
	needles := []string{"context_digest_state", "mark_digest_dirty", "trg_digest_dirty"}

	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
			return err
		}
		body, readErr := os.ReadFile(path)
		if readErr != nil {
			return readErr
		}
		for i, line := range strings.Split(string(body), "\n") {
			code := strings.TrimSpace(line)
			if strings.HasPrefix(code, "//") {
				continue
			}
			// The DDL fixture in THIS file is the one legitimate code
			// occurrence: it re-installs what the migration removed in order to
			// measure the difference.
			if strings.HasSuffix(path, "dropstate_integration_test.go") {
				continue
			}
			for _, n := range needles {
				if strings.Contains(code, n) {
					t.Errorf("%s:%d references %q in code — migration 129 removes it", path, i+1, n)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
}

// W-G-4 — idempotency and the lock deckel. DROP TRIGGER takes ACCESS EXCLUSIVE
// on context_blocks; without lock_timeout the migration waits behind the longest
// open statement and stacks every reader and writer behind it (083 header
// vocabulary). The order inside the file is load-bearing too and asserted here,
// because a file that dropped the table first would leave a trigger whose
// function fails on the next write.
func TestDropDigestState_MigrationShape(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	body, err := migrations.FS.ReadFile("129_drop_digest_state.sql")
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	text := string(body)

	if !strings.Contains(text, "SET LOCAL lock_timeout") {
		t.Error("no lock_timeout — DROP TRIGGER on context_blocks can stall the whole database")
	}
	// Only the STATEMENTS decide the order — the header explains the ordering
	// rule in prose and names all three drops long before any of them runs.
	var stmts []string
	for _, line := range strings.Split(text, "\n") {
		if s := strings.TrimSpace(line); !strings.HasPrefix(s, "--") {
			stmts = append(stmts, s)
		}
	}
	sql := strings.Join(stmts, "\n")
	iTrg := strings.Index(sql, "DROP TRIGGER")
	iFn := strings.Index(sql, "DROP FUNCTION")
	iTbl := strings.Index(sql, "DROP TABLE")
	if iTrg < 0 || iFn < 0 || iTbl < 0 || !(iTrg < iFn && iFn < iTbl) {
		t.Errorf("drop order is trigger=%d function=%d table=%d — must be trigger before function before table", iTrg, iFn, iTbl)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, text); err != nil {
		t.Fatalf("second apply of 129 failed — not idempotent: %v", err)
	}
}
