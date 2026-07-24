package schemacontract

import "testing"

// TestResolveMode_EnvAlwaysWins is the design/03 §7 W03-3 Gate 2 "3×3"
// table: for every combination of env ∈ {off,warn,enforce} × db ∈
// {off,warn,enforce} (db always PRESENT), env must win outright — including
// the case DB=enforce + env=off, which is RED under the registry's normal
// DB>env>default precedence (a DB "enforce" row would there beat env "off",
// exactly the W21 break the design document calls out): under the §4.4
// special precedence this key uses, env off must win regardless.
func TestResolveMode_EnvAlwaysWins(t *testing.T) {
	values := []string{ModeOff, ModeWarn, ModeEnforce}
	for _, env := range values {
		for _, db := range values {
			mode, source, dbOff := ResolveMode(env, db, true)
			if mode != env {
				t.Errorf("env=%s db=%s: mode=%s, want %s (env must win — rot unter der alten DB>env-Annahme, wenn db=%s hier gewinnt)",
					env, db, mode, env, db)
			}
			if source != SourceEnv {
				t.Errorf("env=%s db=%s: source=%s, want %s", env, db, source, SourceEnv)
			}
			if dbOff {
				t.Errorf("env=%s db=%s: dbOffFinding=true, want false (env decided, the db=off attempt is not even consulted)", env, db)
			}
		}
	}
}

// TestResolveMode_DBOffNotHonored: contract.mode=off written to the DB
// (env unset) must NOT disable the check — it resolves to warn/db and
// raises the dbOffFinding signal (design/03 §4.4: "off aus DB wird NICHT
// honoriert").
func TestResolveMode_DBOffNotHonored(t *testing.T) {
	mode, source, dbOff := ResolveMode("", ModeOff, true)
	if mode != ModeWarn {
		t.Errorf("mode=%s, want %s", mode, ModeWarn)
	}
	if source != SourceDB {
		t.Errorf("source=%s, want %s", source, SourceDB)
	}
	if !dbOff {
		t.Error("dbOffFinding=false, want true — a DB off-attempt must become a visible finding")
	}
}

// TestResolveMode_DBWarnEnforceHonored: db=warn / db=enforce (env unset)
// are honored as-is — only "off" gets the special treatment.
func TestResolveMode_DBWarnEnforceHonored(t *testing.T) {
	for _, db := range []string{ModeWarn, ModeEnforce} {
		mode, source, dbOff := ResolveMode("", db, true)
		if mode != db {
			t.Errorf("db=%s: mode=%s, want %s", db, mode, db)
		}
		if source != SourceDB {
			t.Errorf("db=%s: source=%s, want %s", db, source, SourceDB)
		}
		if dbOff {
			t.Errorf("db=%s: dbOffFinding=true, want false", db)
		}
	}
}

// TestResolveMode_NothingSet: no env, no DB row ⇒ the plain default.
func TestResolveMode_NothingSet(t *testing.T) {
	mode, source, dbOff := ResolveMode("", "", false)
	if mode != DefaultMode || source != DefaultModeSource || dbOff {
		t.Errorf("got mode=%s source=%s dbOff=%v, want %s/%s/false", mode, source, dbOff, DefaultMode, DefaultModeSource)
	}
}

// TestResolveMode_InvalidEnv: a typo'd env value must resolve to the safe
// default (warn/default) — NOT fall through to the DB (a broken break-glass
// attempt must never silently hand control back to a DB-controlled mode,
// even an "enforce" one).
func TestResolveMode_InvalidEnv(t *testing.T) {
	mode, source, dbOff := ResolveMode("bogus", ModeEnforce, true)
	if mode != DefaultMode {
		t.Errorf("mode=%s, want %s (invalid env must not fall through to db=enforce)", mode, DefaultMode)
	}
	if source != DefaultModeSource {
		t.Errorf("source=%s, want %s", source, DefaultModeSource)
	}
	if dbOff {
		t.Error("dbOffFinding=true, want false — the db value was never consulted")
	}
}

// TestResolveMode_InvalidDBValue: a corrupt/unrecognized DB row (env unset)
// is treated the same as "not present" — falls through to the default,
// never mistaken for a valid mode and never flagged as the off-attempt.
func TestResolveMode_InvalidDBValue(t *testing.T) {
	mode, source, dbOff := ResolveMode("", "sideways", true)
	if mode != DefaultMode || source != DefaultModeSource || dbOff {
		t.Errorf("got mode=%s source=%s dbOff=%v, want %s/%s/false", mode, source, dbOff, DefaultMode, DefaultModeSource)
	}
}

// TestValidModeValue pins the exact three-value vocabulary.
func TestValidModeValue(t *testing.T) {
	for _, v := range []string{ModeOff, ModeWarn, ModeEnforce} {
		if !ValidModeValue(v) {
			t.Errorf("%q must be valid", v)
		}
	}
	for _, v := range []string{"", "OFF", "Warn", "enforced", "bogus"} {
		if ValidModeValue(v) {
			t.Errorf("%q must be invalid", v)
		}
	}
}
