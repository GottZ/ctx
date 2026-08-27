package blocktype

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// TestMigration146SetsAuditTrailDamping is the SQL half of the damping
// lockstep, and — like TestMigration138SetsUntrustedFlag, whose shape it
// follows — a deliberately CONTAINER-FREE OUTPOST rather than the authority.
// The authority is TestRegistryGolden_Integration: it applies the real chain
// (113 seeds 0.3, 146 lifts it) and diffs the END STATE of the rows against
// builtinPolicies(). This probe reads migration 146 out of migrations.FS —
// never a test-local copy of the statement (design/01 §4.1 R1) — so a rewrite
// that still parses goes red in `go test -short` instead of two minutes into
// the container suite.
//
// BOTH numbers below are written out as frozen literals and are deliberately
// NOT derived from auditTrailDamping. Migration 146 is landed history the
// moment it is committed: its text never changes again, while the constant is
// meant to move again (design/01 §7 W01-8; w01-m1.md reports the G-KI optimum
// at 0.85, not at 0.6). An expectation that followed the living constant would
// fail twice over — the next damping wave would turn this probe red with the
// misleading message "migration 146 lost the jsonb path" although 146 is
// untouched and correct, and a coordinated rewrite of BOTH the file and the
// constant would slip past it, which is precisely the class it claims to
// catch. The living-constant side of the lockstep therefore stays where it
// belongs: TestRegistryGolden_Integration compares builtinPolicies() against
// the end state of the real chain, not against one migration file.
//
// 113/136/143 are not touched by this wave: they are committed, and a landed
// migration is not edited (see the 138 header for the full argument).
func TestMigration146SetsAuditTrailDamping(t *testing.T) {
	body, err := migrations.Section("146_audit_trail_damping_060.sql")
	if err != nil {
		t.Fatalf("read 146 from migrations.FS: %v", err)
	}
	// Comments are dropped FIRST, then whitespace is collapsed: the statement
	// may be reformatted, never rewritten. The German header discusses the very
	// tokens asserted below (it names the type, both factors and the guard in
	// prose), so measuring prose would let every assertion here pass on a file
	// whose SQL had been deleted outright.
	var sql []string
	for _, line := range strings.Split(string(body), "\n") {
		if s := strings.TrimSpace(line); s != "" && !strings.HasPrefix(s, "--") {
			sql = append(sql, s)
		}
	}
	stmt := strings.Join(strings.Fields(strings.Join(sql, " ")), " ")
	if stmt == "" {
		t.Fatal("migration 146 carries no SQL at all — only comments")
	}

	for _, want := range []struct{ what, substr string }{
		// Exactly ONE type. A widened predicate would move tool-evidence,
		// tool-overview or a tenant override along with it — the measurement
		// behind this wave (W01-M1) covers audit-trail and nothing else.
		{"the single type name", `name = 'audit-trail'`},
		{"the global scope", `scope = '_global'`},
		{"the builtin predicate", `builtin = true`},
		// The path, not a whole-config rewrite: intent patterns, classify rules
		// and structural link classes are operator-tunable and must survive
		// (M107 doctrine, the same reason 138 used jsonb_set).
		{"the jsonb path", `jsonb_set(config, '{retrieval,damping_factor}', '0.6'::jsonb)`},
		// The value guard is what makes the UPDATE idempotent (a second run
		// finds 0.6 and updates nothing) and what bounds its reach: every row
		// still carrying 0.3 is lifted, every other value stays. It compares a
		// VALUE, not a provenance — a row an operator deliberately set to 0.3
		// is indistinguishable from the seed and is lifted with it.
		{"the old-value guard", `(config->'retrieval'->>'damping_factor')::numeric = 0.3`},
		// A damped policy is the precondition of the field having any meaning;
		// without it the UPDATE could resurrect a factor on a type an operator
		// had switched to excluded or full-pass.
		{"the policy guard", `config->'retrieval'->>'policy' = 'damped'`},
		{"the lock timeout", `SET LOCAL lock_timeout = '2s';`},
	} {
		if !strings.Contains(stmt, want.substr) {
			t.Errorf("migration 146 lost %s — want a statement containing %q, got:\n%s",
				want.what, want.substr, stmt)
		}
	}

	// A second UPDATE (or a DELETE/INSERT) would put the end state somewhere
	// the assertions above cannot see, and the golden gate would then be the
	// only thing left standing between a rewrite and production.
	if n := strings.Count(stmt, "UPDATE"); n != 1 {
		t.Errorf("migration 146 carries %d UPDATE statements, want exactly 1:\n%s", n, stmt)
	}
	for _, forbidden := range []string{"DELETE", "INSERT", "DROP", "ALTER TABLE"} {
		if strings.Contains(stmt, forbidden) {
			t.Errorf("migration 146 carries %s — this wave is a single data UPDATE:\n%s", forbidden, stmt)
		}
	}
}
