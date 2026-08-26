package blocktype

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/migrations"
)

// V-W7 (design/05 §7, DECISIONS E-12) — container-free half of the checkpoint
// untrusted lockstep. The authority is TestCheckpointUntrusted_Integration
// (registry loaded from the DB after the real chain) plus the registry golden
// drift gate; these probes read the SQL out of migrations.FS so a rewrite that
// still parses goes red in `go test -short` instead of two minutes into the
// container suite. Same construction as TestMigration138SetsUntrustedFlag,
// whose header explains why a landed migration gets a successor instead of an
// edit.

const migration141File = "141_checkpoint_untrusted.sql"

// migration141Statement returns migration 141 with every comment line and every
// blank line removed and the remainder collapsed to a single whitespace-normal
// line. Comments go FIRST: the German header discusses the very tokens asserted
// below, so measuring prose would let every assertion here pass on a file whose
// SQL had been deleted outright.
func migration141Statement(t *testing.T) string {
	t.Helper()
	body, err := migrations.Section(migration141File)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migration141File, err)
	}
	var sql []string
	for _, line := range strings.Split(string(body), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" && !strings.HasPrefix(trimmed, "--") {
			sql = append(sql, trimmed)
		}
	}
	stmt := strings.Join(strings.Fields(strings.Join(sql, " ")), " ")
	if stmt == "" {
		t.Fatalf("%s carries no SQL at all — only comments", migration141File)
	}
	return stmt
}

// TestMigration141SetsCheckpointUntrusted pins the shape of the statement: the
// ONE type it touches, the scope, the jsonb path, the existence guard and the
// lock timeout.
func TestMigration141SetsCheckpointUntrusted(t *testing.T) {
	stmt := migration141Statement(t)

	for _, want := range []struct{ what, substr string }{
		// Exactly one name. A widened list would reframe types whose content
		// nobody has shown to be foreign text — the failure that matters.
		{"the type name", `name = 'checkpoint'`},
		{"the global scope", `scope = '_global'`},
		// The path, not a whole-config rewrite: classify priority and title
		// patterns are operator-tunable and must survive (M107 doctrine, the
		// reason 120 exists as its own migration).
		{"the jsonb path", `jsonb_set(config, '{retrieval,untrusted}', 'true'::jsonb)`},
		// The existence guard is what makes the UPDATE idempotent AND keeps an
		// operator's deliberate false — jsonb_set alone would overwrite it.
		{"the existence guard", `NOT (config->'retrieval' ? 'untrusted')`},
		{"the lock timeout", `SET LOCAL lock_timeout = '2s';`},
	} {
		if !strings.Contains(stmt, want.substr) {
			t.Errorf("%s lost %s — want a statement containing %q, got:\n%s",
				migration141File, want.what, want.substr, stmt)
		}
	}

	// One statement, one effect: a second UPDATE (or a DELETE/INSERT) would put
	// the end state somewhere this probe does not look.
	if n := strings.Count(strings.ToUpper(stmt), "UPDATE "); n != 1 {
		t.Errorf("%s carries %d UPDATE statements, want exactly 1:\n%s", migration141File, n, stmt)
	}
	for _, forbidden := range []string{"DELETE ", "DROP ", "INSERT ", "CREATE "} {
		if strings.Contains(strings.ToUpper(stmt), forbidden) {
			t.Errorf("%s carries a %sstatement — it is registry DATA, one UPDATE:\n%s",
				migration141File, forbidden, stmt)
		}
	}
}

// TestMigration141LeavesPolicyAlone is the container-free half of the
// non-regression contract: V-W7 changes the FRAMING of the type, never its
// retrieval POLICY. checkpoint stays excluded, so the flag is inert and every
// ctx_rrf argument vector derived from the registry is unchanged. A statement
// that also wrote {retrieval,policy} — the "damped by accident" variant the
// wave brief names — would move the type into VisibleTypes/DampedTypesFor and
// change retrieval for the whole 5 900-block checkpoint corpus. The integration
// probe in internal/rrf measures that end state; this one refuses the token.
func TestMigration141LeavesPolicyAlone(t *testing.T) {
	stmt := migration141Statement(t)
	for _, forbidden := range []string{
		`{retrieval,policy}`,
		`'damped'`,
		`"damped"`,
		`{retrieval,damping_factor}`,
	} {
		if strings.Contains(stmt, forbidden) {
			t.Errorf("%s touches %s — V-W7 sets the untrusted flag and nothing else:\n%s",
				migration141File, forbidden, stmt)
		}
	}
}

// TestCheckpointSeedLacksUntrusted answers the question the wave brief asks
// explicitly: does the checkpoint seed need the same "one allowed deviation"
// normalisation TestToolSeedsMatchBuiltin carries for the 136 literals?
//
// It does NOT, because no unit-level seed-vs-builtin comparison exists for
// checkpoint — the M107 seed (folded into 113_baseline.sql) is covered only by
// TestRegistryGolden_Integration, which compares the END STATE of the whole
// chain and is therefore correct as it stands. What this probe adds instead is
// the premise 141 rests on: the seed literal carries no untrusted key at all,
// so 141 is a real write and not a no-op, and the existence guard has something
// to guard. If a later wave ever edits the seed to carry the flag, 141 becomes
// dead and this goes red.
func TestCheckpointSeedLacksUntrusted(t *testing.T) {
	body, err := migrations.Section(migrations.BaselineFile)
	if err != nil {
		t.Fatalf("read %s from migrations.FS: %v", migrations.BaselineFile, err)
	}
	var raw []byte
	for _, m := range seedRowRe.FindAllSubmatch(body, -1) {
		if string(m[1]) == "checkpoint" {
			raw = m[2]
		}
	}
	if raw == nil {
		t.Fatalf("no checkpoint seed row found in %s — the M107 fold moved or the row shape changed",
			migrations.BaselineFile)
	}
	seed, err := DecodePolicy("checkpoint", globalScope, true, false, raw)
	if err != nil {
		t.Fatalf("decode checkpoint seed: %v", err)
	}
	if seed.Retrieval.Untrusted {
		t.Errorf("the checkpoint seed in %s already carries retrieval.untrusted — %s is a no-op "+
			"and the DB half of V-W7 is now unproven", migrations.BaselineFile, migration141File)
	}
	if seed.Retrieval.Kind != RetrievalExcluded {
		t.Errorf("checkpoint seed retrieval policy = %v, want excluded — V-W7 asserts the flag is "+
			"inert, and that claim rests on this", seed.Retrieval.Kind)
	}
}
