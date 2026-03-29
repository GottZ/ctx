package store

import (
	"testing"
)

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
