// fixture_test.go — the fixtures every gate of this package runs against.
//
// NEVER the live file. Every store here is built from testdata/hermes_schema.sql,
// the verbatim SCHEMA_SQL + DEFERRED_INDEX_SQL of NousResearch/hermes-agent at
// commit 1bbb6e5bce56e721ab685af4cd87df21bbff4d35 (provenance and sha256 in the
// file's header), so a schema drift upstream shows up here as a failing test
// rather than as a wrong plan in production.
//
// Source: https://github.com/GottZ/ctx
package hermesstate_test

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

// sessionIDIndexLine is the one line of upstream SCHEMA_SQL the fixture builder
// can remove: the index on (session_id, id). Stores written by an older hermes
// do not have it — that is the shape the rowid-range strategy exists for.
const sessionIDIndexLine = "CREATE INDEX IF NOT EXISTS idx_messages_session_id ON messages(session_id, id);"

// istSessionID is the session the inventory numbers were taken from.
const istSessionID = "20260824_085852_7ec9a4"

// istGapIDs are ids missing from the archived range. The live store holds 910
// archived rows spanning ids 3..930 — 928 slots — so the range has gaps, and a
// fixture without them would let an off-by-one between "row count" and "id
// delta" pass unnoticed. That is precisely the confusion WatermarkFrom exists
// to avoid, so the fixture reproduces it.
var istGapIDs = []int64{7, 19, 43, 88, 131, 197, 260, 314, 377, 429, 488, 541, 603, 666, 715, 780, 841, 899}

// istBaseTS is the fixture epoch: 2026-08-24 08:58:52 UTC, the session's start.
var istBaseTS = time.Date(2026, 8, 24, 8, 58, 52, 0, time.UTC)

// schemaSQL returns the upstream schema, optionally without the (session_id, id)
// index. The removal is line-exact and asserted: if upstream renames or reshapes
// that line, this fails loudly instead of silently building the wrong fixture.
func schemaSQL(t *testing.T, withSessionIDIndex bool) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "hermes_schema.sql"))
	if err != nil {
		t.Fatalf("read schema fixture: %v", err)
	}
	if withSessionIDIndex {
		return string(raw)
	}
	var (
		kept    []string
		removed int
	)
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == sessionIDIndexLine {
			removed++
			continue
		}
		kept = append(kept, line)
	}
	if removed != 1 {
		t.Fatalf("expected exactly one %q line in testdata/hermes_schema.sql, found %d", sessionIDIndexLine, removed)
	}
	return strings.Join(kept, "\n")
}

// writable opens a write-capable handle. Only fixtures use it — the package
// under test has no such path at all.
func writable(t *testing.T, path string, extra ...string) *sql.DB {
	t.Helper()
	dsn := "file:" + path + "?_pragma=journal_mode(wal)&_pragma=synchronous(off)"
	for _, e := range extra {
		dsn += "&" + e
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open writable %s: %v", path, err)
	}
	return db
}

func mustExec(t *testing.T, db *sql.DB, q string, args ...any) {
	t.Helper()
	if _, err := db.Exec(q, args...); err != nil {
		t.Fatalf("exec %.60q: %v", q, err)
	}
}

// newStore creates an empty store with the upstream schema applied.
func newStore(t *testing.T, path string, withSessionIDIndex bool) *sql.DB {
	t.Helper()
	db := writable(t, path)
	mustExec(t, db, schemaSQL(t, withSessionIDIndex))
	return db
}

func insertSession(t *testing.T, db *sql.DB, id, parent string, started time.Time) {
	t.Helper()
	var par any
	if parent != "" {
		par = parent
	}
	mustExec(t, db,
		"INSERT INTO sessions(id, source, parent_session_id, started_at) VALUES(?,?,?,?)",
		id, "cli", par, started.Format(time.RFC3339))
}

// istFixture is the metadata a test needs to check the ist store without
// re-deriving it.
type istFixture struct {
	Path        string
	ToolIDs     map[int64]bool // archived tool rows
	ArchivedIDs []int64        // ascending
	NewestTS    time.Time      // timestamp of the newest ACTIVE row
}

// buildIst reproduces the live shape reported in inventory §1.3/§1.4 for the
// session 20260824_085852_7ec9a4: 910 archived rows over ids 3..930 (418 of
// them role='tool'), 399 live rows over ids 931..1329.
func buildIst(t *testing.T, dir string, withSessionIDIndex bool) istFixture {
	t.Helper()
	path := filepath.Join(dir, "state.db")
	db := newStore(t, path, withSessionIDIndex)
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close ist fixture: %v", err)
		}
	}()

	insertSession(t, db, istSessionID, "", istBaseTS)

	gap := map[int64]bool{}
	for _, g := range istGapIDs {
		gap[g] = true
	}

	fx := istFixture{Path: path, ToolIDs: map[int64]bool{}}

	tx, err := db.Begin()
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	ins, err := tx.Prepare(`INSERT INTO messages
		(id, session_id, role, content, tool_name, timestamp, active, compacted)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}

	// Archived half. Every other row is a tool result until 418 of them exist —
	// the inventory split (418 tool / 492 prose) reproduced exactly.
	tools := 0
	for id := int64(3); id <= 930; id++ {
		if gap[id] {
			continue
		}
		fx.ArchivedIDs = append(fx.ArchivedIDs, id)
		role, tool := "assistant", ""
		if tools < 418 && id%2 == 1 {
			role, tool = "tool", fmt.Sprintf("tool_%d", id%7)
			tools++
			fx.ToolIDs[id] = true
		}
		ts := float64(istBaseTS.Add(time.Duration(id)*time.Second).UnixNano()) / 1e9
		if _, err := ins.Exec(id, istSessionID, role,
			fmt.Sprintf("archived %s payload for id %d", role, id), nullable(tool), ts, 0, 1); err != nil {
			t.Fatalf("insert archived %d: %v", id, err)
		}
	}
	if tools != 418 {
		t.Fatalf("archived tool rows = %d, want 418", tools)
	}

	// Live half. Some of these are tool rows too — they must never show up in a
	// batch read, and compacted=0 is the only thing keeping them out.
	for id := int64(931); id <= 1329; id++ {
		role, tool := "assistant", ""
		if id%3 == 0 {
			role, tool = "tool", "live_tool"
		}
		ts := float64(istBaseTS.Add(time.Duration(id)*time.Second).UnixNano()) / 1e9
		if _, err := ins.Exec(id, istSessionID, role,
			fmt.Sprintf("live %s payload for id %d", role, id), nullable(tool), ts, 1, 0); err != nil {
			t.Fatalf("insert live %d: %v", id, err)
		}
	}
	fx.NewestTS = istBaseTS.Add(1329 * time.Second)

	if err := ins.Close(); err != nil {
		t.Fatalf("close stmt: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
	return fx
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// countRows is a fixture-side assertion helper: it reads with a plain writable
// handle, so a mistake in the package under test cannot mask a mistake in the
// fixture.
func countRows(t *testing.T, path, where string) int {
	t.Helper()
	db := writable(t, path)
	defer func() { _ = db.Close() }()
	var n int
	if err := db.QueryRow("SELECT count(*) FROM messages WHERE " + where).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", where, err)
	}
	return n
}
