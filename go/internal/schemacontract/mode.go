package schemacontract

import (
	"context"
	"encoding/json"
	"os"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Mode values (design/03 §4.4/§4.5 — VERBINDLICH wire values on Report.Mode).
const (
	ModeOff     = "off"
	ModeWarn    = "warn"
	ModeEnforce = "enforce"
)

// Mode source values (design/03 §4.1 Report.ModeSource doc: "env | db |
// default"). SourceDefault deliberately reuses DefaultModeSource (W03-2,
// types.go) rather than declaring a third "default" literal — one constant,
// one meaning.
const (
	SourceEnv = "env"
	SourceDB  = "db"
)

// EnvContractMode is the break-glass env var (design/03 §4.4): when set, it
// dominates ANY DB value, for all three modes — "the switch that turns off
// DB overrides cannot itself be a DB override" (N12, reload.go:60-63's
// CTX_SETTINGS_DISABLE principle, applied to this one key). This is the
// documented, deliberate exception to the config registry's normal
// DB>env>default precedence — see internal/config's ContractConfig.Mode doc.
const EnvContractMode = "CTX_CONTRACT_MODE"

// ValidModeValue reports whether v is one of the three defined modes.
func ValidModeValue(v string) bool {
	return v == ModeOff || v == ModeWarn || v == ModeEnforce
}

// ResolveMode implements the §4.4 special precedence for contract.mode —
// env-dominant, the OPPOSITE of the registry's normal DB>env>default order.
// Pure and DB-free (unit-testable without a container, design/03 §7 W03-3
// Gate 2):
//
//  1. envVal set (non-empty) ⇒ env wins outright, for all three valid
//     values (the break-glass path is unconditionally reachable). An
//     UNRECOGNIZED env value still counts as "env decided" — it does NOT
//     fall through to the DB (a typo'd break-glass attempt must never
//     silently re-enable a DB-controlled mode) — it resolves to the safe
//     default (warn/default); the caller (schemaContractBoot) is
//     responsible for logging the bad value LOUD, since ResolveMode itself
//     has no logger.
//  2. else dbPresent ⇒ DB wins, EXCEPT "off": an "off" row is not honored
//     (the DB writer is the very actor migration_integrity distrusts — it
//     must not be able to silently disable the check that watches it). An
//     "off" row resolves to warn/db with dbOffFinding=true, so the ATTEMPT
//     itself becomes a visible Drift (ClassModeSourceDBOff) in the next
//     report (design/03 §4.4). An unrecognized DB value (corrupt row) is
//     treated the same as "not present" — falls through to the default,
//     symmetric with the env handling above.
//  3. else ⇒ DefaultMode/DefaultModeSource ("warn"/"default").
func ResolveMode(envVal, dbVal string, dbPresent bool) (mode, source string, dbOffFinding bool) {
	if envVal != "" {
		if ValidModeValue(envVal) {
			return envVal, SourceEnv, false
		}
		return DefaultMode, DefaultModeSource, false
	}
	if dbPresent {
		switch dbVal {
		case ModeOff:
			return ModeWarn, SourceDB, true
		case ModeWarn, ModeEnforce:
			return dbVal, SourceDB, false
		}
		// unrecognized dbVal: falls through to the default below.
	}
	return DefaultMode, DefaultModeSource, false
}

// gatherModeResolution reads the two raw precedence inputs (env, direct DB
// peek) and applies ResolveMode. It is best-effort on the DB read: a query
// failure (missing context_settings table on a pre-051 DB, a transient DB
// hiccup) degrades to dbPresent=false rather than propagating an error — a
// failed SETTINGS peek must not abort or fail the whole Check (§4.4's
// fail-closed posture governs Introspect/Diff, not this optional lookup),
// and "DB unreadable" collapsing to "as if absent" is the same safe
// direction ResolveMode already takes for an unrecognized DB value.
func gatherModeResolution(ctx context.Context, pool *pgxpool.Pool) (mode, source string, dbOffFinding bool) {
	envVal := os.Getenv(EnvContractMode)
	dbVal, dbPresent := contractModeDBValue(ctx, pool)
	return ResolveMode(envVal, dbVal, dbPresent)
}

// contractModeDBValue reads the raw contract.mode override value directly
// from context_settings (scope=_global), INDEPENDENT of the config
// registry's normal DB>env>default merge — that merge already picks a
// winner under the wrong precedence for this one key (see
// internal/config's ContractConfig.Mode doc). Any error (no row, missing
// table, connection trouble) collapses to present=false; the value is
// unwrapped from its JSONB scalar shape the same way
// internal/settings.ScalarValue does for strings, duplicated here (a few
// lines) rather than imported — this package stays free of the
// config/settings/store layering entirely (design/03 §3.2's "the vertrag
// muss außerhalb des Prüflings leben" extends to this package's own
// dependency footprint).
func contractModeDBValue(ctx context.Context, pool *pgxpool.Pool) (value string, present bool) {
	var raw []byte
	if err := pool.QueryRow(ctx,
		`SELECT value FROM context_settings WHERE scope = '_global' AND key = 'contract.mode'`,
	).Scan(&raw); err != nil {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		// Malformed scalar (not a JSON string): return it verbatim so
		// ResolveMode's switch fails to match off/warn/enforce and falls
		// through to the default — same safe outcome as "absent", never a
		// hard error out of a best-effort peek.
		return string(raw), true
	}
	return s, true
}
