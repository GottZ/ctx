package store

import (
	"io/fs"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// TestMigrationVersionsUnique guards the runner's silent-skip trap (MW10
// gate): RunMigrations keys applied-tracking on the numeric prefix ONLY — a
// second file with a duplicate number would be skipped SILENTLY (the
// _migrations EXISTS check matches the first file's record), shipping a
// migration that never runs anywhere. This test makes the collision loud at
// unit-test time, using the exact parse the runner uses.
func TestMigrationVersionsUnique(t *testing.T) {
	entries, err := fs.ReadDir(migrations.FS, ".")
	if err != nil {
		t.Fatalf("read embedded migrations: %v", err)
	}
	seen := map[int]string{}
	count := 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".sql") {
			continue
		}
		parts := strings.SplitN(name, "_", 2)
		if len(parts) < 2 {
			continue
		}
		v, err := strconv.Atoi(parts[0])
		if err != nil {
			continue // the runner skips non-numeric prefixes with a WARN
		}
		if prev, dup := seen[v]; dup {
			t.Errorf("duplicate migration version %d: %q vs %q — the runner would skip the second SILENTLY", v, prev, name)
		}
		seen[v] = name
		count++
	}
	if count == 0 {
		t.Fatal("no migrations parsed — embed or parse broken")
	}
	// Pin the MW10 migration is present under its final number. Since the
	// fold it lives inside the baseline rather than as its own file, so the
	// identity comes from the folded chain — the same assertion, one layer
	// further in.
	got := ""
	for _, f := range migrations.Folded() {
		if f.Version == 91 {
			got = f.Filename
		}
	}
	if got != "091_dispatch_telemetry.sql" {
		t.Errorf("migration 091 = %q, want 091_dispatch_telemetry.sql (dispatch telemetry, design/05 §3.1)", got)
	}

	// The fold line itself: versions at or below it ship inside the baseline,
	// versions above it ship as files. A file that reappears below the line
	// would collide with a section the baseline already applies and would be
	// skipped silently by the very bookkeeping this test guards.
	for v, name := range seen {
		if v <= migrations.BaselineVersion && name != migrations.BaselineFile {
			t.Errorf("%s sits at version %d, at or below the fold line %d — its effect is already inside %s",
				name, v, migrations.BaselineVersion, migrations.BaselineFile)
		}
	}
	if seen[migrations.BaselineVersion] != migrations.BaselineFile {
		t.Errorf("version %d = %q, want the baseline %q", migrations.BaselineVersion, seen[migrations.BaselineVersion], migrations.BaselineFile)
	}
}

// migrations.go has no pure functions — RunMigrations depends on pgxpool and
// the embedded filesystem. The parsing/sorting logic is inline within RunMigrations.
//
// The migration version extraction logic:
//   parts := strings.SplitN(name, "_", 2)
//   v, err := strconv.Atoi(parts[0])
// is embedded in the function and not exposed for testing.
//
// If refactored to expose parseMigrationVersion(filename) -> (int, error),
// these edge cases should be tested:
//   - "001_initial.sql" -> 1
//   - "010_tenth.sql" -> 10
//   - "noseparator.sql" -> skip
//   - "abc_invalid.sql" -> skip (non-numeric prefix)
//   - "_leading.sql" -> skip (empty prefix -> Atoi fails)
//   - "0_zero.sql" -> 0
//   - "-1_negative.sql" -> -1 (valid parse, questionable semantics)

func TestRunMigrations_RequiresDB(t *testing.T) {
	t.Skip("requires database connection — RunMigrations needs pgxpool.Pool")
	// Integration test plan:
	// 1. Create a test database with _migrations table.
	// 2. Run migrations — verify all .sql files are applied in version order.
	// 3. Run again — verify no migrations are re-applied (idempotent).
	// 4. Add a new migration — verify only the new one is applied.
}
