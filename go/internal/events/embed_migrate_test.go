package events

import (
	"strings"
	"testing"
)

// The peek/pick SQL builders carry the §4.3 cursor contract: the cursor
// clause is emitted conditionally (never an `$2 IS NULL OR …` OR-form that
// defeats the index range condition), the memo exclusion is always present
// (migration id at $1), and only the pick locks.
func TestMigratePeekSQL_CursorVariants(t *testing.T) {
	with := migratePeekSQL(true)
	without := migratePeekSQL(false)

	if !strings.Contains(with, "created_at > $2") {
		t.Errorf("with-cursor peek lacks the range condition:\n%s", with)
	}
	if strings.Contains(without, "$2") {
		t.Errorf("cursor-less peek must not reference $2:\n%s", without)
	}
	for name, sql := range map[string]string{"with": with, "without": without} {
		if !strings.Contains(sql, "f.migration_id = $1::uuid") {
			t.Errorf("%s-cursor peek lacks the migration-scoped memo exclusion:\n%s", name, sql)
		}
		if !strings.Contains(sql, "embedding IS NOT NULL AND embedding_next IS NULL AND NOT is_archived") {
			t.Errorf("%s-cursor peek lacks the migration pending predicate:\n%s", name, sql)
		}
		if strings.Contains(sql, "FOR UPDATE") {
			t.Errorf("%s-cursor peek must be lock-free:\n%s", name, sql)
		}
		if !strings.Contains(sql, "ORDER BY created_at ASC") {
			t.Errorf("%s-cursor peek lacks oldest-first ordering:\n%s", name, sql)
		}
	}
}

func TestMigratePickSQL_LocksAndMatchesPeek(t *testing.T) {
	pick := migratePickSQL(true)
	if !strings.Contains(pick, "FOR UPDATE SKIP LOCKED") {
		t.Errorf("pick lacks FOR UPDATE SKIP LOCKED:\n%s", pick)
	}
	if !strings.Contains(pick, "created_at > $2") ||
		!strings.Contains(pick, "f.migration_id = $1::uuid") {
		t.Errorf("pick predicate diverges from the peek contract:\n%s", pick)
	}
	if strings.Contains(migratePickSQL(false), "$2") {
		t.Errorf("cursor-less pick must not reference $2")
	}
}

// The runtime index DDL must be CONCURRENTLY (a plain CREATE INDEX holds
// SHARE over a full-table scan — §3.3 lock-class rule) and its predicate
// must match the peek's pending predicate, or the planner cannot serve one
// from the other.
func TestMigrationPendingIndexDDL_Contract(t *testing.T) {
	if !strings.Contains(migrationPendingIndexDDL, "CREATE INDEX CONCURRENTLY IF NOT EXISTS") {
		t.Errorf("index DDL is not CONCURRENTLY IF NOT EXISTS:\n%s", migrationPendingIndexDDL)
	}
	if !strings.Contains(migrationPendingIndexDDL, migratePendingWhere) {
		t.Errorf("index predicate diverges from the peek pending predicate:\nDDL: %s\nWHERE: %s",
			migrationPendingIndexDDL, migratePendingWhere)
	}
}
