package store

import (
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// W01-4 (design/01 §3.5 + §7 W01-4, design/05 §7 V-W9) — the container-free
// half of the `sensitivity_source='derived'` gate. The authority for the
// VALUE is TestMigration144DerivedSensitivitySource_Integration (real chain,
// real CHECK constraint); these probes read the SQL out of migrations.FS so a
// rewrite that still parses goes red in `go test -short` instead of two
// minutes into the container suite. Same construction as
// TestMigration141SetsCheckpointUntrusted.
//
// The load-bearing probe here is an ABSENCE: migration 144 must carry no
// `VALIDATE CONSTRAINT`. The migration runner executes the WHOLE FILE in ONE
// transaction (store/migrations.go:132-156), so a VALIDATE would run its full
// heap scan while the transaction still holds the ACCESS EXCLUSIVE lock the
// two catalog updates took — at target scale (1M+ blocks, 10M+ organic) that
// is a minutes-long total outage on context_blocks for readers AND writers.
// The scan is also provably pointless: the new value set is a strict superset
// of the old one, so every existing row satisfies it by construction.
const migration144File = "144_sensitivity_source_derived.sql"

// migration144Statement returns migration 144 with every comment line and
// every blank line removed and the remainder collapsed to a single
// whitespace-normal line. Comments go FIRST and that is not cosmetic: the
// German header of 144 DISCUSSES the token `VALIDATE` at length, so measuring
// prose would make the absence probe below pass on a file that carries the
// statement.
func migration144Statement(t *testing.T) string {
	t.Helper()
	body, err := migrations.Section(migration144File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration144File, err)
	}
	return normalizeMigration144SQL(string(body))
}

// normalizeMigration144SQL is the comment stripper as a pure function so the
// negative probes below can run it over scratch variants that never touch the
// filesystem.
func normalizeMigration144SQL(body string) string {
	var sql []string
	for _, line := range strings.Split(body, "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			sql = append(sql, trimmed)
		}
	}
	return strings.Join(strings.Fields(strings.Join(sql, " ")), " ")
}

// migration144Violations returns one message per pinned property the given
// normalized statement breaks. Empty slice = the shape holds. Returning the
// findings instead of calling t.Errorf is what lets the SAME rules run
// against the real file (want: none) and against deliberately broken scratch
// variants (want: at least one) — a probe that cannot fail proves nothing.
func migration144Violations(stmt string) []string {
	var out []string
	if stmt == "" {
		return []string{"the file carries no SQL at all — only comments"}
	}
	upper := strings.ToUpper(stmt)

	for _, want := range []struct{ what, substr string }{
		// The lock timeout guards the ACQUISITION of the ACCESS EXCLUSIVE lock
		// (never its hold time — that is what the VALIDATE absence is for).
		{"the lock timeout", `SET LOCAL lock_timeout = '2s';`},
		// Postgres cannot widen a CHECK in place, so the constraint is dropped
		// and re-added under the SAME name. IF EXISTS keeps a re-run and a
		// hand-repaired database working.
		{"the drop", `DROP CONSTRAINT IF EXISTS context_blocks_sensitivity_source_check`},
		{"the re-add under the same name", `ADD CONSTRAINT context_blocks_sensitivity_source_check`},
		// The value set, verbatim: the four established sources plus the new
		// one. Order is part of the pin — it is what the manifest-free reader
		// compares against 113_baseline.sql:5477.
		{"the value set", `CHECK (sensitivity_source IN ('default','llm-audit','pattern','manual','derived'))`},
		{"the NOT VALID clause", `NOT VALID`},
	} {
		if !strings.Contains(stmt, want.substr) {
			out = append(out, "lost "+want.what+" — want a statement containing "+want.substr)
		}
	}

	// THE probe of this wave (design/01 §3.5). `NOT VALID` does not contain the
	// token `VALIDATE`, so this is a clean discriminator between the clause we
	// require and the statement we forbid.
	if strings.Contains(upper, "VALIDATE") {
		out = append(out, "carries a VALIDATE statement — the runner would hold ACCESS EXCLUSIVE across a full heap scan (§3.5)")
	}

	// Two catalog updates, no more: a third ALTER would put schema state
	// somewhere none of the gates look.
	if n := strings.Count(upper, "ALTER TABLE"); n != 2 {
		out = append(out, "carries "+strconv.Itoa(n)+" ALTER TABLE statements, want exactly 2 (drop + re-add)")
	}
	// No data, no index, no function: this migration is a constraint widening.
	for _, forbidden := range []string{"INSERT ", "UPDATE ", "DELETE ", "CREATE ", "REINDEX", "VACUUM"} {
		if strings.Contains(upper, forbidden) {
			out = append(out, "carries a "+strings.TrimSpace(forbidden)+" statement — 144 widens a constraint, nothing else")
		}
	}
	// SET LOCAL + two ALTERs = three statements. A fourth semicolon means a
	// fourth statement nobody probed.
	if n := strings.Count(stmt, ";"); n != 3 {
		out = append(out, "carries "+strconv.Itoa(n)+" statements, want exactly 3 (SET LOCAL + DROP + ADD)")
	}
	return out
}

// TestMigration144WidensSensitivitySource pins the shape of migration 144 —
// including, above all, the absence of VALIDATE.
func TestMigration144WidensSensitivitySource(t *testing.T) {
	stmt := migration144Statement(t)
	for _, v := range migration144Violations(stmt) {
		t.Errorf("%s %s\n%s", migration144File, v, stmt)
	}
}

// TestMigration144ShapeProbesAreRed is the negative half: each scratch variant
// below breaks exactly one pinned property, and the rule set must SEE it.
// Without this, TestMigration144WidensSensitivitySource could be green because
// its rules never fire, not because the file is right — and the VALIDATE
// variant is the one the brief names explicitly ("eine Scratch-Variante MIT
// VALIDATE macht den Pin rot").
func TestMigration144ShapeProbesAreRed(t *testing.T) {
	const good = `SET LOCAL lock_timeout = '2s';
ALTER TABLE context_blocks DROP CONSTRAINT IF EXISTS context_blocks_sensitivity_source_check;
ALTER TABLE context_blocks
  ADD CONSTRAINT context_blocks_sensitivity_source_check
  CHECK (sensitivity_source IN ('default','llm-audit','pattern','manual','derived'))
  NOT VALID;`

	// Sanity: the reference variant itself must pass, otherwise every probe
	// below is red for the wrong reason.
	if v := migration144Violations(normalizeMigration144SQL(good)); len(v) != 0 {
		t.Fatalf("the reference variant violates the rules: %v", v)
	}

	for _, tc := range []struct {
		name string
		sql  string
	}{
		{
			// The variant §3.5 exists to forbid. Note the comment line: it also
			// proves the stripper runs BEFORE the absence check, since the
			// prose here mentions the very token.
			name: "with VALIDATE",
			sql: good + `
-- validating the constraint afterwards, in the same transaction
ALTER TABLE context_blocks VALIDATE CONSTRAINT context_blocks_sensitivity_source_check;`,
		},
		{
			name: "derived missing from the value set",
			sql:  strings.Replace(good, `,'derived'`, "", 1),
		},
		{
			name: "an established value dropped",
			sql:  strings.Replace(good, `'llm-audit',`, "", 1),
		},
		{
			name: "NOT VALID dropped",
			sql:  strings.Replace(good, "\n  NOT VALID;", ";", 1),
		},
		{
			name: "no lock timeout",
			sql:  strings.Replace(good, "SET LOCAL lock_timeout = '2s';\n", "", 1),
		},
		{
			name: "a data statement smuggled in",
			sql:  good + "\nUPDATE context_blocks SET sensitivity_source = 'derived' WHERE false;",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := migration144Violations(normalizeMigration144SQL(tc.sql)); len(v) == 0 {
				t.Errorf("scratch variant %q passed every rule — the pin cannot fail and proves nothing", tc.name)
			}
		})
	}
}
