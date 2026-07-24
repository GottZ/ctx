package schemacontract

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// snapshotException is the fixed code constant carrying the T07-inherited
// hand-snapshot-table exception (design/03 §4.4). It is intentionally NOT a
// settings key: a configurable exception pattern would be exactly the
// bypass hebel a threat actor needs. Scope is drawn as narrowly as possible
// — see snapshotExcluded.
const snapshotException = "_snapshot_"

// snapshotExcluded reports whether name matches the *_snapshot_* naming
// exception (design/03 §4.4, T07's inherited convention, test.sh:315
// `table_name NOT LIKE '%_snapshot_%'`). Callers MUST apply this only to
// ordinary tables (relkind='r') in the unknown_object (live-only) direction
// — never to triggers, functions, indexes, rules or policies. diffDetailed
// enforces that scope structurally: this helper is only ever called from
// the live-only-table branch.
func snapshotExcluded(name string) bool {
	return strings.Contains(name, snapshotException)
}

// Diff is the pure, DB-free bidirectional comparison of a Manifest against
// a LiveSnapshot (design/03 §4.1 — VERBINDLICH signature). It classifies
// every finding into a DriftClass + Severity (design/03 §4.4) but never
// touches the network — Introspect is the only DB-facing half of the pair.
func Diff(m Manifest, live LiveSnapshot) []Drift {
	drifts, _ := diffDetailed(m, live)
	return drifts
}

// diffDetailed is Diff's implementation, additionally returning the count
// of tables excluded by the snapshot-name exception (design/03 §4.1 Report
// field ExcludedSnapshotTables) — Check needs that count, Diff's pinned
// signature has no room for a second return value, so Check calls this
// unexported twin directly instead of duplicating the classification logic.
func diffDetailed(m Manifest, live LiveSnapshot) ([]Drift, int) {
	var drifts []Drift
	excluded := 0

	drifts = append(drifts, diffExtensions(m, live)...)
	tableDrifts, exc := diffTables(m, live)
	drifts = append(drifts, tableDrifts...)
	excluded += exc
	drifts = append(drifts, diffIndexes(m, live)...)
	drifts = append(drifts, diffFunctions(m, live)...)
	drifts = append(drifts, diffTriggers(m, live)...)
	drifts = append(drifts, diffRules(m, live)...)
	drifts = append(drifts, diffPolicies(live)...)
	drifts = append(drifts, diffHypertables(m, live)...)

	sort.Slice(drifts, func(i, j int) bool {
		if drifts[i].Class != drifts[j].Class {
			return drifts[i].Class < drifts[j].Class
		}
		return drifts[i].Object < drifts[j].Object
	})

	return drifts, excluded
}

func diffExtensions(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	for name, spec := range m.Extensions {
		liveExt, ok := live.Extensions[name]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "extension:" + name, Detail: "extension not installed",
			})
			continue
		}
		if compareVersions(liveExt.Version, spec.MinVersion) < 0 {
			drifts = append(drifts, Drift{
				Class: ClassExtensionVersion, Severity: SeverityBreaking,
				Object: "extension:" + name,
				Detail: fmt.Sprintf("min_version=%s live=%s", spec.MinVersion, liveExt.Version),
			})
		}
	}
	return drifts
}

// compareVersions compares dotted numeric version strings (e.g. "0.8.2" vs
// "0.8.10"), NOT lexicographically — a naive string compare would rank
// "0.8.10" below "0.8.2". Non-numeric components compare as 0.
func compareVersions(a, b string) int {
	as := strings.Split(a, ".")
	bs := strings.Split(b, ".")
	n := len(as)
	if len(bs) > n {
		n = len(bs)
	}
	for i := 0; i < n; i++ {
		var av, bv int
		if i < len(as) {
			av, _ = strconv.Atoi(as[i])
		}
		if i < len(bs) {
			bv, _ = strconv.Atoi(bs[i])
		}
		if av != bv {
			if av < bv {
				return -1
			}
			return 1
		}
	}
	return 0
}

func diffTables(m Manifest, live LiveSnapshot) ([]Drift, int) {
	var drifts []Drift

	for name, spec := range m.Tables {
		liveT, ok := live.Tables[name]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "table:" + name, Detail: "table missing",
			})
			continue
		}

		if spec.RowSecurity != liveT.RowSecurity {
			// RLS flip: breaking exception within definition_drift, design/03 §4.3/§4.4.
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityBreaking,
				Object: "table:" + name,
				Detail: fmt.Sprintf("row_security: manifest=%v live=%v", spec.RowSecurity, liveT.RowSecurity),
			})
		}

		mCols := map[string]ColumnSpec{}
		for _, c := range spec.Columns {
			mCols[c.Name] = c
		}
		lCols := map[string]ColumnSpec{}
		for _, c := range liveT.Columns {
			lCols[c.Name] = c
		}

		for colName, mc := range mCols {
			lc, ok := lCols[colName]
			if !ok {
				drifts = append(drifts, Drift{
					Class: ClassMissingObject, Severity: SeverityBreaking,
					Object: "table:" + name + "." + colName, Detail: "column missing",
				})
				continue
			}
			if diff := describeColumnDiff(mc, lc); diff != "" {
				drifts = append(drifts, Drift{
					Class: ClassDefinitionDrift, Severity: SeverityParam,
					Object: "table:" + name + "." + colName, Detail: diff,
				})
			}
		}
		for colName := range lCols {
			if _, ok := mCols[colName]; !ok {
				drifts = append(drifts, Drift{
					Class: ClassUnknownObject, Severity: SeverityParam,
					Object: "table:" + name + "." + colName, Detail: "extra column not in manifest",
				})
			}
		}
	}

	excluded := 0
	for name, liveT := range live.Tables {
		if _, ok := m.Tables[name]; ok {
			continue
		}
		if snapshotExcluded(name) {
			excluded++
			continue
		}
		_ = liveT
		drifts = append(drifts, Drift{
			Class: ClassUnknownObject, Severity: SeverityParam,
			Object: "table:" + name, Detail: "unknown table not in manifest",
		})
	}

	return drifts, excluded
}

func describeColumnDiff(mc, lc ColumnSpec) string {
	var parts []string
	if mc.Type != lc.Type {
		parts = append(parts, fmt.Sprintf("type: manifest=%s live=%s", mc.Type, lc.Type))
	}
	if mc.NotNull != lc.NotNull {
		parts = append(parts, fmt.Sprintf("not_null: manifest=%v live=%v", mc.NotNull, lc.NotNull))
	}
	if mc.GeneratedExprHash != lc.GeneratedExprHash {
		parts = append(parts, "generated_expr_hash differs")
	}
	if mc.Storage != lc.Storage {
		parts = append(parts, fmt.Sprintf("storage: manifest=%s live=%s", mc.Storage, lc.Storage))
	}
	return strings.Join(parts, "; ")
}

func diffIndexes(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	for name, spec := range m.Indexes {
		liveI, ok := live.Indexes[name]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "index:" + name, Detail: "index missing",
			})
			continue
		}
		if diff := describeIndexDiff(spec, liveI); diff != "" {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "index:" + name, Detail: diff,
			})
		}
	}
	for name := range live.Indexes {
		if _, ok := m.Indexes[name]; !ok {
			// No snapshot exception for indexes — design/03 §4.4 draws the
			// exception exclusively around relkind='r' tables.
			drifts = append(drifts, Drift{
				Class: ClassUnknownObject, Severity: SeverityParam,
				Object: "index:" + name, Detail: "unknown index not in manifest",
			})
		}
	}
	return drifts
}

func describeIndexDiff(spec, liveI IndexSpec) string {
	var parts []string
	// Reloptions compared field-by-field FIRST so a pure parameter change
	// (ef_construction 128 vs 64) reads as "ef_construction: manifest=128
	// live=64" instead of an opaque "hash mismatch" (design/03 §4.3).
	keys := map[string]bool{}
	for k := range spec.RelOptions {
		keys[k] = true
	}
	for k := range liveI.RelOptions {
		keys[k] = true
	}
	sortedKeys := make([]string, 0, len(keys))
	for k := range keys {
		sortedKeys = append(sortedKeys, k)
	}
	sort.Strings(sortedKeys)
	for _, k := range sortedKeys {
		mv, mok := spec.RelOptions[k]
		lv, lok := liveI.RelOptions[k]
		if mv != lv || mok != lok {
			parts = append(parts, fmt.Sprintf("reloptions %s: manifest=%s live=%s", k, mv, lv))
		}
	}
	if spec.DefHash != liveI.DefHash && len(parts) == 0 {
		parts = append(parts, "indexdef hash differs")
	}
	return strings.Join(parts, "; ")
}

func diffFunctions(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	for key, spec := range m.Functions {
		liveF, ok := live.Functions[key]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "function:" + key, Detail: "function missing",
			})
			continue
		}
		if spec.SrcHash != liveF.SrcHash {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "function:" + key, Detail: "function body hash differs",
			})
		}
	}
	for key := range live.Functions {
		if _, ok := m.Functions[key]; !ok {
			// Active code, no snapshot exception (design/03 §4.4).
			drifts = append(drifts, Drift{
				Class: ClassUnknownActiveObject, Severity: SeverityBreaking,
				Object: "function:" + key, Detail: "unknown function not in manifest",
			})
		}
	}
	return drifts
}

func diffTriggers(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	for key, spec := range m.Triggers {
		liveT, ok := live.Triggers[key]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "trigger:" + key, Detail: "trigger missing",
			})
			continue
		}
		if spec.DefHash != liveT.DefHash {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "trigger:" + key, Detail: "trigger definition hash differs",
			})
		}
	}
	for key := range live.Triggers {
		if _, ok := m.Triggers[key]; !ok {
			drifts = append(drifts, Drift{
				Class: ClassUnknownActiveObject, Severity: SeverityBreaking,
				Object: "trigger:" + key, Detail: "unknown trigger not in manifest",
			})
		}
	}
	return drifts
}

func diffRules(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	for key, spec := range m.Rules {
		liveR, ok := live.Rules[key]
		if !ok {
			drifts = append(drifts, Drift{
				Class: ClassMissingObject, Severity: SeverityBreaking,
				Object: "rule:" + key, Detail: "rule missing",
			})
			continue
		}
		if spec.DefHash != liveR.DefHash {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "rule:" + key, Detail: "rule definition hash differs",
			})
		}
	}
	for key := range live.Rules {
		if _, ok := m.Rules[key]; !ok {
			drifts = append(drifts, Drift{
				Class: ClassUnknownActiveObject, Severity: SeverityBreaking,
				Object: "rule:" + key, Detail: "unknown rule not in manifest",
			})
		}
	}
	return drifts
}

// diffPolicies: the Manifest never declares policies (design/03 §4.3 —
// expected empty unconditionally), so every live policy is unconditionally
// unknown_active_object; there is no manifest-side missing_object branch.
func diffPolicies(live LiveSnapshot) []Drift {
	var drifts []Drift
	for _, name := range live.Policies {
		drifts = append(drifts, Drift{
			Class: ClassUnknownActiveObject, Severity: SeverityBreaking,
			Object: "policy:" + name, Detail: "policy present, contract expects none",
		})
	}
	return drifts
}

// diffHypertables compares hypertable-conversion status for tables that
// otherwise exist as declared tables on both sides. It never overlaps with
// diffTables' missing/unknown-table findings, which cover column-level
// existence; hypertable-ness is an orthogonal property of an existing
// table (like row_security), so a mismatch is definition_drift, not
// missing/unknown_object.
func diffHypertables(m Manifest, live LiveSnapshot) []Drift {
	var drifts []Drift
	mSet := map[string]bool{}
	for _, n := range m.Hypertables {
		mSet[n] = true
	}
	lSet := map[string]bool{}
	for _, n := range live.Hypertables {
		lSet[n] = true
	}
	for n := range mSet {
		if !lSet[n] {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "table:" + n, Detail: "hypertable: manifest=expected live=not-converted",
			})
		}
	}
	for n := range lSet {
		if !mSet[n] {
			drifts = append(drifts, Drift{
				Class: ClassDefinitionDrift, Severity: SeverityParam,
				Object: "table:" + n, Detail: "hypertable: manifest=not-expected live=converted",
			})
		}
	}
	return drifts
}
