// open_test.go — the Open contract: no write path, hardened pragmas, schema
// type check, strategy choice, and the WAL states of design §0.2.
//
// Source: https://github.com/GottZ/ctx
package hermesstate_test

import (
	"database/sql"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/hermesstate"
	sqlite "modernc.org/sqlite"
)

// TestOpenMissingPathCreatesNothing is the modernc URI probe. A driver that
// does not parse URI DSNs would treat "state.db?mode=ro" as a literal file name
// and silently create an empty database — every later gate would then pass
// against a store nobody wrote.
func TestOpenMissingPathCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent.db")

	// ROT first, in the same test, so the gate is not vacuous. Without the
	// "file:" scheme the DSN is not a URI: the driver drops the query string
	// from the file name, SQLite never sees mode=ro, and the open falls back to
	// READWRITE|CREATE. The control below creates a store where the package
	// must create nothing — that is the silent failure this gate guards.
	control := filepath.Join(dir, "control.db")
	db, err := sql.Open("sqlite", control+"?mode=ro")
	if err != nil {
		t.Fatalf("open control: %v", err)
	}
	if _, err := db.Exec("CREATE TABLE probe(x INTEGER)"); err != nil {
		t.Fatalf("the non-URI control was read-only after all — the red state of this gate is gone: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close control: %v", err)
	}
	if _, err := os.Stat(control); err != nil {
		t.Fatalf("the non-URI control created no file: %v", err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(control + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove control: %v", err)
		}
	}

	src, err := hermesstate.Open(path, "absent", hermesstate.Options{})
	if err == nil {
		_ = src.Close()
		t.Fatal("Open against a missing path succeeded")
	}
	if !errors.Is(err, hermesstate.ErrSourceUnavailable) {
		t.Errorf("error = %v, want ErrSourceUnavailable", err)
	}

	entries, rdErr := os.ReadDir(dir)
	if rdErr != nil {
		t.Fatalf("read dir: %v", rdErr)
	}
	if len(entries) != 0 {
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("Open created %v; the directory must stay untouched", names)
	}
}

// TestReadOnlyDSNRefusesWrites is the other half of the write-protection gate.
// The package itself has no statement-running surface at all — the compile-time
// probe for that is `grep -c 'Exec\|Prepare' *.go` over the non-test files — so
// what remains to prove is that the DSN the package builds really is read-only.
// A raw handle on that same DSN, opened here in the test, must be refused.
func TestReadOnlyDSNRefusesWrites(t *testing.T) {
	fx := buildIst(t, t.TempDir(), false)
	db, err := sql.Open("sqlite", "file:"+fx.Path+"?mode=ro&_query_only=1")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = db.Close() }()

	_, err = db.Exec("UPDATE messages SET compacted = 0 WHERE id = 3")
	if err == nil {
		t.Fatal("write through a mode=ro handle succeeded")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "readonly") {
		t.Errorf("error = %v, want a readonly refusal", err)
	}
	if got := countRows(t, fx.Path, "compacted = 1"); got != 910 {
		t.Errorf("archived rows after the refused write = %d, want 910", got)
	}
}

// TestHardeningPragmasApplied reads the pragmas back through a raw handle on
// the package's own DSN shape. Open already asserts them, so this is the probe
// that the assertion is not vacuous.
func TestHardeningPragmasApplied(t *testing.T) {
	fx := buildIst(t, t.TempDir(), false)
	src, err := hermesstate.Open(fx.Path, "pragma", hermesstate.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = src.Close() }()

	db, err := sql.Open("sqlite", "file:"+fx.Path+"?mode=ro")
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	defer func() { _ = db.Close() }()
	for _, p := range []struct {
		name string
		want int
	}{{"trusted_schema", 1}, {"cell_size_check", 0}, {"query_only", 0}} {
		var got int
		if err := db.QueryRow("SELECT * FROM pragma_" + p.name).Scan(&got); err != nil {
			t.Fatalf("read pragma %s: %v", p.name, err)
		}
		if got != p.want {
			t.Errorf("unhardened default of %s = %d, want %d — if this changed, the hardening may be redundant or misdirected", p.name, got, p.want)
		}
	}
}

// TestSchemaTypeCheckRejectsView is the B7 gate. A prepared file can replace a
// table with a VIEW over arbitrary expressions; a size cap would not notice.
func TestSchemaTypeCheckRejectsView(t *testing.T) {
	for _, victim := range []string{"messages", "sessions"} {
		t.Run(victim, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "state.db")
			db := writable(t, path)
			mustExec(t, db, `CREATE TABLE messages_real(
				id INTEGER PRIMARY KEY AUTOINCREMENT, session_id TEXT NOT NULL, role TEXT NOT NULL,
				content TEXT, tool_name TEXT, timestamp REAL NOT NULL,
				active INTEGER NOT NULL DEFAULT 1, compacted INTEGER NOT NULL DEFAULT 0)`)
			mustExec(t, db, `CREATE TABLE sessions_real(id TEXT PRIMARY KEY, parent_session_id TEXT)`)
			mustExec(t, db, `INSERT INTO messages_real(id, session_id, role, content, tool_name, timestamp, active, compacted)
				VALUES(1,'s','tool','honest','probe',1.0,0,1)`)
			mustExec(t, db, "INSERT INTO sessions_real(id, parent_session_id) VALUES('s', NULL)")
			for _, name := range []string{"messages", "sessions"} {
				if name == victim {
					// Not just a rename: the VIEW rewrites what a reader sees.
					// That is the point of B7 — the shape looks right and the
					// values do not come from the table.
					mustExec(t, db, "CREATE VIEW "+name+
						" AS SELECT *, 'INJECTED' AS injected FROM "+name+"_real")
					continue
				}
				mustExec(t, db, "CREATE TABLE "+name+" AS SELECT * FROM "+name+"_real")
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}

			// ROT: a reader without the type check reads the VIEW happily, and
			// gets the view's values rather than the table's.
			var injected string
			naive := writable(t, path)
			if err := naive.QueryRow("SELECT injected FROM " + victim + " LIMIT 1").Scan(&injected); err != nil {
				t.Fatalf("the VIEW control is not readable — the red state of this gate is gone: %v", err)
			}
			if err := naive.Close(); err != nil {
				t.Fatalf("close naive: %v", err)
			}
			if injected != "INJECTED" {
				t.Fatalf("VIEW control returned %q", injected)
			}

			src, err := hermesstate.Open(path, "hostile", hermesstate.Options{})
			if err == nil {
				_ = src.Close()
				t.Fatalf("Open accepted a store whose %s is a VIEW", victim)
			}
			if !errors.Is(err, hermesstate.ErrSchemaUntrusted) {
				t.Errorf("error = %v, want ErrSchemaUntrusted", err)
			}
		})
	}
}

func TestSchemaTypeCheckRejectsMissingTable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	db := writable(t, path)
	mustExec(t, db, "CREATE TABLE unrelated(x INTEGER)")
	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	src, err := hermesstate.Open(path, "empty", hermesstate.Options{})
	if err == nil {
		_ = src.Close()
		t.Fatal("Open accepted a store without a messages table")
	}
	if !errors.Is(err, hermesstate.ErrSchemaUntrusted) {
		t.Errorf("error = %v, want ErrSchemaUntrusted", err)
	}
}

// TestStrategyChoice is the §0.3 gate: the same fixture, once with and once
// without the (session_id, id) index, must land on different strategies — and
// both plans must survive the assertion.
func TestStrategyChoice(t *testing.T) {
	for _, tc := range []struct {
		name     string
		withIdx  bool
		want     hermesstate.Strategy
		wantPlan string
	}{
		{"without the (session_id, id) index", false, hermesstate.StrategyRowidRange, "USING INTEGER PRIMARY KEY"},
		{"with the (session_id, id) index", true, hermesstate.StrategyIndexSeek, "idx_messages_session_id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fx := buildIst(t, t.TempDir(), tc.withIdx)
			src, err := hermesstate.Open(fx.Path, "strategy", hermesstate.Options{})
			if err != nil {
				t.Fatalf("open: %v", err)
			}
			defer func() { _ = src.Close() }()

			if src.Strategy() != tc.want {
				t.Errorf("strategy = %q, want %q", src.Strategy(), tc.want)
			}
			plan := src.Plan()
			if !strings.Contains(plan, tc.wantPlan) {
				t.Errorf("plan = %q, want it to name %q", plan, tc.wantPlan)
			}
			if strings.Contains(strings.ToUpper(plan), "USE TEMP B-TREE") {
				t.Errorf("plan materialises a sort: %q", plan)
			}
			if strings.Contains(plan, "idx_messages_session ") {
				t.Errorf("plan uses the timestamp-ordered index: %q", plan)
			}
			t.Logf("strategy=%s plan=%s", src.Strategy(), plan)
		})
	}
}

// TestOpenWALStates walks the three states of design §0.2.
//
// Deviation from the design's probe, stated rather than papered over: the
// design measured SQLITE_READONLY (8) as an unprivileged NON-OWNER, where
// SQLite sees an unwritable file and takes its read-only path. The Go test
// process here runs as root in this environment, and root bypasses both file
// mode and directory mode — a 0555 directory is measurably NOT enough (probed:
// the read succeeds and creates -wal/-shm). The state is therefore forced with
// an immutable directory (chattr +i), which root cannot bypass either; the
// primary result code is then SQLITE_CANTOPEN (14) rather than
// SQLITE_READONLY (8), because the failure arrives as EPERM on the create
// instead of as an unwritable file. Both map to ErrSourceUnavailable, and that
// mapping — not the number — is what the arm depends on. The test asserts the
// class and records the code.
func TestOpenWALStates(t *testing.T) {
	dir := t.TempDir()
	fx := buildIst(t, dir, false)

	// (a) a live store: -wal and -shm exist because a writer holds the file.
	holder := writable(t, fx.Path)
	var n int
	if err := holder.QueryRow("SELECT count(*) FROM messages").Scan(&n); err != nil {
		t.Fatalf("holder read: %v", err)
	}
	assertSidecars(t, fx.Path, true)

	src, err := hermesstate.Open(fx.Path, "live", hermesstate.Options{})
	if err != nil {
		t.Fatalf("(a) Open against a live WAL store: %v", err)
	}
	if _, err := src.GlobalMaxID(t.Context()); err != nil {
		t.Fatalf("(a) read against a live WAL store: %v", err)
	}
	_ = src.Close()
	if err := holder.Close(); err != nil {
		t.Fatalf("close holder: %v", err)
	}
	assertSidecars(t, fx.Path, false)

	// The B6 finding, recorded as behaviour rather than as prose: a mode=ro
	// handle is NOT write-free when the -shm is missing and the opener may
	// write. It reads — and leaves -wal/-shm behind. In production ctxd holds
	// no write right on the hermes path, which is what makes case (b) the real
	// one; here it is what the immutable directory below has to simulate.
	if s2, err := hermesstate.Open(fx.Path, "recreate", hermesstate.Options{}); err == nil {
		_ = s2.Close()
		created := sidecarsPresent(t, fx.Path)
		t.Logf("mode=ro as a writable-directory opener: read succeeded, sidecars created = %v", created)
		removeSidecars(t, fx.Path)
	} else {
		t.Logf("mode=ro on a cleanly closed store already refused here: %v", err)
	}

	if !setImmutable(t, dir, true) {
		t.Skip("cannot make the directory immutable in this environment; (b)/(c) not probed")
	}
	defer func() { setImmutable(t, dir, false) }()

	// (b) cleanly closed WAL store, nothing may be created next to it.
	src, err = hermesstate.Open(fx.Path, "closed", hermesstate.Options{})
	if err == nil {
		_ = src.Close()
		t.Fatal("(b) Open succeeded against a cleanly closed WAL store in an unwritable directory")
	}
	if !errors.Is(err, hermesstate.ErrSourceUnavailable) {
		t.Errorf("(b) error = %v, want ErrSourceUnavailable", err)
	}
	var se *sqlite.Error
	if !errors.As(err, &se) {
		t.Fatalf("(b) error %v does not carry a SQLite result code", err)
	}
	code := se.Code() & 0xff
	if code != 8 && code != 14 {
		t.Errorf("(b) primary result code = %d, want SQLITE_READONLY (8) or SQLITE_CANTOPEN (14)", code)
	}
	t.Logf("(b) cleanly closed WAL store, unwritable directory: code=%d err=%v", se.Code(), err)

	// (c) the snapshot path: immutable=1 is the only way to read that store.
	snap, err := hermesstate.Open(fx.Path, "snapshot", hermesstate.Options{Snapshot: true})
	if err != nil {
		t.Fatalf("(c) Open with Snapshot: %v", err)
	}
	defer func() { _ = snap.Close() }()
	got, err := snap.MaxCompactedID(t.Context(), istSessionID)
	if err != nil {
		t.Fatalf("(c) read: %v", err)
	}
	if got != 930 {
		t.Errorf("(c) MaxCompactedID = %d, want 930", got)
	}
	assertSidecars(t, fx.Path, false)
}

func sidecarsPresent(t *testing.T, path string) []string {
	t.Helper()
	var present []string
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			present = append(present, suffix)
		}
	}
	return present
}

func assertSidecars(t *testing.T, path string, want bool) {
	t.Helper()
	got := sidecarsPresent(t, path)
	if want && len(got) != 2 {
		t.Fatalf("sidecars present = %v, want both -wal and -shm", got)
	}
	if !want && len(got) != 0 {
		t.Fatalf("sidecars present = %v, want none", got)
	}
}

func removeSidecars(t *testing.T, path string) {
	t.Helper()
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			t.Fatalf("remove %s: %v", path+suffix, err)
		}
	}
}

// setImmutable toggles the immutable attribute on dir. It reports whether the
// change took effect: not every filesystem or container has the capability, and
// a gate that cannot be enforced must skip loudly rather than pass quietly.
func setImmutable(t *testing.T, dir string, on bool) bool {
	t.Helper()
	flag := "-i"
	if on {
		flag = "+i"
	}
	out, err := exec.Command("chattr", flag, dir).CombinedOutput()
	if err != nil {
		t.Logf("chattr %s %s: %v (%s)", flag, dir, err, strings.TrimSpace(string(out)))
		return false
	}
	return true
}
