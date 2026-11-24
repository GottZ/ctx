package dream

import (
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// TestDreamEligibleIndexPredicateIsThePickPredicate (issue #271): the
// predicate of idx_blocks_dream_eligible (migration 194) lists the lifecycle
// states dreamEligibleWhere lists. A state added to the code and not to the
// index makes the index unusable for QueueDepth (PostgreSQL proves a partial
// index's predicate from the statement's conditions): the count stays right
// and loses the index, the cost before 194.
func TestDreamEligibleIndexPredicateIsThePickPredicate(t *testing.T) {
	body, err := migrations.FS.ReadFile("194_pending_writes_drop_auth_keys.sql")
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`(?s)CREATE INDEX idx_blocks_dream_eligible\s+ON context_blocks \(scope, type_name\)\s+WHERE NOT is_archived AND lifecycle_state IN \(([^)]*)\);`)
	m := re.FindSubmatch(body)
	if m == nil {
		t.Fatal("migration 194 builds no idx_blocks_dream_eligible of the expected form")
	}
	code := regexp.MustCompile(`lifecycle_state IN \(([^)]*)\)`).FindStringSubmatch(dreamEligibleWhere("$2"))
	if code == nil {
		t.Fatal("dreamEligibleWhere names no lifecycle list")
	}
	norm := func(s string) string { return strings.Join(strings.Fields(s), " ") }
	if norm(string(m[1])) != norm(code[1]) {
		t.Errorf("the index lists the lifecycle states %s, dreamEligibleWhere lists %s", m[1], code[1])
	}
	if !strings.Contains(dreamEligibleWhere("$2"), "NOT is_archived") {
		t.Errorf("dreamEligibleWhere no longer requires NOT is_archived, the index's other conjunct")
	}
}
