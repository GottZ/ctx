// Package schemacontract is the Clean-Room schema-contract checker
// (Evokoa-Clean-Room design/03-contract-observability.md, W03-2/W03-3). It
// carries a generated, embedded expectation of the live Postgres schema
// (Manifest), introspects the real catalog into the same canonical shape
// (LiveSnapshot), and produces a bidirectional, severity-classed diff
// (Drift/Report).
//
// Since W03-3 this package owns its own boot-callable surface:
// RunCheckSingleFlight (Check + the §4.4 env-dominant contract.mode
// resolution + the process-wide Report holder, CAS single-flight guarded)
// is what cmd/ctxd's schemaContractBoot and periodic re-check ticker call —
// this package deliberately stays free of internal/config/internal/settings
// even so (mode.go reads os.Getenv + context_settings directly), so the
// enforce/os.Exit DECISION and the boot-time LOUD logging stay in cmd/ctxd,
// the only place entitled to end the process. No API/CLI surface yet
// (W03-4).
package schemacontract

import "time"

// Severity is the fail-closed classification of a Drift (design/03 §4.4):
// breaking stops an enforce-mode boot, param never does.
type Severity string

const (
	SeverityBreaking Severity = "breaking"
	SeverityParam    Severity = "param"
)

// DriftClass names the kind of bidirectional diff finding (design/03 §4.4).
type DriftClass string

const (
	// ClassMissingObject: declared in the Manifest, absent from the live
	// catalog (table, column, index, function, trigger, rule, hypertable,
	// extension). Always breaking.
	ClassMissingObject DriftClass = "missing_object"
	// ClassUnknownObject: present live, not declared — passive/structural
	// object (table, column, index). Always param. The *_snapshot_*
	// exception (Diff, tables only) removes matching hand-added tables from
	// this class entirely (counted separately, never silently).
	ClassUnknownObject DriftClass = "unknown_object"
	// ClassUnknownActiveObject: present live, not declared — active code in
	// the write path (trigger, function, rule, policy). Always breaking.
	// The *_snapshot_* exception never applies here (design/03 §4.4).
	ClassUnknownActiveObject DriftClass = "unknown_active_object"
	// ClassDefinitionDrift: present in both, definitions differ (indexdef,
	// reloptions, function src hash, GENERATED expr, trigger def). Param,
	// EXCEPT a row-security flip on a declared table, which is breaking
	// (design/03 §4.3/§4.4).
	ClassDefinitionDrift DriftClass = "definition_drift"
	// ClassMigrationIntegrity: checksum mismatch or version gap between the
	// embedded migration chain and _migrations. Always breaking.
	ClassMigrationIntegrity DriftClass = "migration_integrity"
	// ClassGucProbeFailed: the library-load + set_config + pg_settings
	// three-step probe (design/03 §4.3) could not confirm a GUC the
	// contract functions depend on. Always breaking.
	ClassGucProbeFailed DriftClass = "guc_probe_failed"
	// ClassExtensionVersion: an installed extension is below the Manifest's
	// declared minimum. Always breaking.
	ClassExtensionVersion DriftClass = "extension_version"
	// ClassModeSourceDBOff: reserved for W03-3 (contract.mode=off written
	// from the DB is not honored). Not emitted by this package's Check —
	// no settings wiring exists yet in W03-2.
	ClassModeSourceDBOff DriftClass = "mode_source_db_off"
)

// Drift is a single finding of the bidirectional diff.
type Drift struct {
	Class    DriftClass `json:"class"`
	Severity Severity   `json:"severity"`
	Object   string     `json:"object"` // e.g. "index:idx_embedding_hnsw", "table:context_blocks.embed_status"
	Detail   string     `json:"detail"` // e.g. "reloptions ef_construction: manifest=128 live=64"
}

// Report status values (design/03 §4.1/§4.6). "off" is a W03-3
// health-aggregation concept built on top of Report, not a Status this
// package's Check ever assigns.
const (
	StatusOK        = "ok"
	StatusDrift     = "drift"
	StatusUnchecked = "unchecked"
)

// DefaultMode/DefaultModeSource are the static placeholders Check writes
// into Report.Mode/ModeSource until W03-3 wires contract.mode through
// settings (design/03 §4.4 default is "warn"; source "default" mirrors the
// resolution-order fallback documented there).
const (
	DefaultMode       = "warn"
	DefaultModeSource = "default"
)

// Report is the outcome of one Check call (design/03 §4.1 — VERBINDLICH
// struct shape). Mode/ModeSource are wire-shape placeholders in W03-2 (see
// package doc); real resolution lands in W03-3.
type Report struct {
	Status                 string    `json:"status"` // ok | drift | unchecked
	CheckedAt              time.Time `json:"checked_at"`
	ManifestMax            int       `json:"manifest_max"`
	LiveMax                int       `json:"live_max"`
	Mode                   string    `json:"mode"`
	ModeSource             string    `json:"mode_source"`
	ExcludedSnapshotTables int       `json:"excluded_snapshot_tables"`
	Drifts                 []Drift   `json:"drifts"`
}

// GeneratedAgainst records the provenance of a generated Manifest
// (design/03 §4.9): which migration chain built it, against which Postgres
// major version and image. Check compares PGMajor against the live server
// and reports unchecked (never a schema drift) on a major mismatch, because
// pg_get_functiondef/pg_get_expr deparse can differ across majors.
type GeneratedAgainst struct {
	MigrationMax   int    `json:"migration_max"`
	MigrationCount int    `json:"migration_count"`
	PGMajor        int    `json:"pg_major"`
	Image          string `json:"image"`
}

// ExtSpec is the Manifest's expectation for one extension: a minimum
// version (design/03 E-03-3 — minimum-pin, not exact-pin, so a routine
// image bump never bricks an enforce boot).
type ExtSpec struct {
	MinVersion string `json:"min_version"`
}

// ColumnSpec is one column of a TableSpec.
type ColumnSpec struct {
	Name    string `json:"name"`
	Type    string `json:"type"`
	NotNull bool   `json:"not_null"`
	// GeneratedExprHash is sha256(hex) of the deparsed GENERATED expression
	// (pg_get_expr(adbin, adrelid)), empty for ordinary columns.
	GeneratedExprHash string `json:"generated_expr_hash,omitempty"`
	// Storage is the raw pg_attribute.attstorage code (p/e/m/x).
	Storage string `json:"storage"`
}

// TableSpec is one table's expected columns and row-security posture.
// RowSecurity is expected false for every table in v1 (design/03 §4.3 — no
// contract table declares RLS today; a flip is definition_drift/breaking).
type TableSpec struct {
	Columns     []ColumnSpec `json:"columns"`
	RowSecurity bool         `json:"row_security"`
}

// IndexSpec is one index's normalized definition hash and reloptions.
// RelOptions is kept as its own map (not folded into DefHash) so a drift
// report can say "ef_construction: manifest=128 live=64" instead of just
// "hash mismatch" (design/03 §4.3).
type IndexSpec struct {
	DefHash    string            `json:"def_hash"`
	RelOptions map[string]string `json:"rel_options,omitempty"`
}

// FuncSpec is one function's pg_get_functiondef hash. Keyed by
// "proname(identity_arguments)" so overloaded/generational signatures
// (ctx_rrf Gen14 vs Gen15) are distinct manifest entries.
type FuncSpec struct {
	SrcHash string `json:"src_hash"`
}

// TriggerSpec is one trigger's pg_get_triggerdef hash. Keyed by
// "table.trigger".
type TriggerSpec struct {
	DefHash string `json:"def_hash"`
}

// RuleSpec is one rewrite rule's definition hash. Keyed by "table.rulename".
// The Manifest is expected to declare none in v1 (design/03 §4.3).
type RuleSpec struct {
	DefHash string `json:"def_hash"`
}

// GucProbe is one entry of the library-load + set_config + pg_settings
// three-step probe (design/03 §4.3) — the exact GUC name and value the
// contract functions depend on at runtime (e.g. hnsw.iterative_scan /
// relaxed_order, 073_rrf_policy_params.sql:100).
type GucProbe struct {
	Name       string `json:"name"`
	ProbeValue string `json:"probe_value"`
}

// Manifest is the eingebettete Erwartungs-Stand, generated against a fresh
// migration chain (design/03 §4.1 — VERBINDLICH struct shape). All maps are
// keyed by object name (see each Spec type's doc for the exact key shape).
type Manifest struct {
	ManifestVersion  int                    `json:"manifest_version"`
	GeneratedAgainst GeneratedAgainst       `json:"generated_against"`
	Extensions       map[string]ExtSpec     `json:"extensions"`
	Tables           map[string]TableSpec   `json:"tables"`
	Indexes          map[string]IndexSpec   `json:"indexes"`
	Functions        map[string]FuncSpec    `json:"functions"`
	Triggers         map[string]TriggerSpec `json:"triggers"`
	Rules            map[string]RuleSpec    `json:"rules"`
	Hypertables      []string               `json:"hypertables"`
	GucProbes        []GucProbe             `json:"guc_probes"`
}

// LiveExtension is one introspected extension's installed version.
type LiveExtension struct {
	Version string
}

// LiveSnapshot is the live catalog read into the same canonical shape as
// Manifest (design/03 §4.1 doc: "Introspect ... liest den Live-Katalog in
// dieselbe kanonische Form wie das Manifest"). Its shape is this package's
// own (not pinned by design/03 §4.1, which only pins Manifest/Drift/Report)
// — Policies has no Manifest counterpart because the contract expects zero
// policies unconditionally (design/03 §4.3: "Manifest erwartet ... leer");
// any live policy is unconditionally unknown_active_object.
type LiveSnapshot struct {
	Extensions  map[string]LiveExtension
	Tables      map[string]TableSpec
	Indexes     map[string]IndexSpec
	Functions   map[string]FuncSpec
	Triggers    map[string]TriggerSpec
	Rules       map[string]RuleSpec
	Hypertables []string
	// Policies are fully qualified "table.polname" identifiers of every
	// pg_policy row on a public table. Always unknown_active_object.
	Policies []string
	// PGMajor is the live server's major version (server_version_num / 10000).
	PGMajor int
}
