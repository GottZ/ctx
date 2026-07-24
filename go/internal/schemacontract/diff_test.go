package schemacontract

import (
	"testing"
)

// snapshotOf converts a Manifest into the LiveSnapshot shape it would
// produce if a live catalog matched it exactly — the "own snapshot" half of
// Gate 1's nullbeweis (design/03 §7 W03-2: "Diff(m, snapshot(m)) == leer").
// Reused directly (not copied) because Manifest's Tables/Indexes/Functions/
// Triggers/Rules already share LiveSnapshot's Spec types field-for-field —
// the only translation needed is Extensions (ExtSpec.MinVersion becomes the
// "installed" LiveExtension.Version).
func snapshotOf(m Manifest) LiveSnapshot {
	live := LiveSnapshot{
		Extensions:  map[string]LiveExtension{},
		Tables:      m.Tables,
		Indexes:     m.Indexes,
		Functions:   m.Functions,
		Triggers:    m.Triggers,
		Rules:       m.Rules,
		Hypertables: append([]string(nil), m.Hypertables...),
		PGMajor:     m.GeneratedAgainst.PGMajor,
	}
	for name, spec := range m.Extensions {
		live.Extensions[name] = LiveExtension{Version: spec.MinVersion}
	}
	return live
}

// TestDiff_NullbeweisAgainstOwnSnapshot is Gate 1 (design/03 §7 W03-2):
// Diff(m, snapshot(m)) must be empty for ANY manifest, not just a trivial
// one — proven against both a small synthetic fixture and the real
// checked-in Embedded() manifest generated from the full 001-112 chain
// (191 indexes, 47 tables, 16 functions). A non-empty result here means the
// classification logic itself is asymmetric, independent of any DB.
func TestDiff_NullbeweisAgainstOwnSnapshot(t *testing.T) {
	t.Run("synthetic", func(t *testing.T) {
		m := Manifest{
			Extensions: map[string]ExtSpec{"vector": {MinVersion: "0.8.2"}},
			Tables: map[string]TableSpec{
				"context_blocks": {
					Columns: []ColumnSpec{
						{Name: "id", Type: "uuid", NotNull: true, Storage: "p"},
						{Name: "title", Type: "text", NotNull: false, Storage: "x"},
					},
					RowSecurity: false,
				},
			},
			Indexes: map[string]IndexSpec{
				"idx_embedding_hnsw": {DefHash: "abc", RelOptions: map[string]string{"m": "16", "ef_construction": "128"}},
			},
			Functions:   map[string]FuncSpec{"ctx_rrf(halfvec)": {SrcHash: "def"}},
			Triggers:    map[string]TriggerSpec{"context_blocks.notify_ins": {DefHash: "ghi"}},
			Rules:       map[string]RuleSpec{},
			Hypertables: []string{"context_llm_log"},
		}
		if drifts := Diff(m, snapshotOf(m)); len(drifts) != 0 {
			t.Fatalf("Diff(m, snapshot(m)) not empty: %+v", drifts)
		}
	})

	t.Run("embedded manifest", func(t *testing.T) {
		m := Embedded()
		if len(m.Tables) == 0 {
			t.Fatal("Embedded() manifest is empty — generator has not been run (go test -tags=genmanifest)")
		}
		drifts := Diff(m, snapshotOf(m))
		if len(drifts) != 0 {
			t.Fatalf("Diff(Embedded(), snapshot(Embedded())) not empty (%d drifts): %+v", len(drifts), drifts)
		}
	})
}

func TestDiff_MissingTable_Breaking(t *testing.T) {
	m := Manifest{Tables: map[string]TableSpec{"context_blocks": {}}}
	live := LiveSnapshot{Tables: map[string]TableSpec{}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassMissingObject || drifts[0].Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want missing_object/breaking", drifts[0].Class, drifts[0].Severity)
	}
}

func TestDiff_MissingColumn_Breaking(t *testing.T) {
	m := Manifest{Tables: map[string]TableSpec{
		"context_blocks": {Columns: []ColumnSpec{{Name: "id", Type: "uuid"}, {Name: "checksum", Type: "text"}}},
	}}
	live := LiveSnapshot{Tables: map[string]TableSpec{
		"context_blocks": {Columns: []ColumnSpec{{Name: "id", Type: "uuid"}}},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassMissingObject || drifts[0].Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want missing_object/breaking", drifts[0].Class, drifts[0].Severity)
	}
	if drifts[0].Object != "table:context_blocks.checksum" {
		t.Errorf("Object = %q, want table:context_blocks.checksum", drifts[0].Object)
	}
}

// TestDiff_ExtraColumn_UnknownObject_Param: a column present live but not
// declared is a passive, structural addition (like a hand-added table) —
// unknown_object/param, not breaking. Matches the Live-Gate's real-world
// case (embed_status: dropped by migration 109 in the fresh chain, still
// present on a pre-109 live DB).
func TestDiff_ExtraColumn_UnknownObject_Param(t *testing.T) {
	m := Manifest{Tables: map[string]TableSpec{
		"context_blocks": {Columns: []ColumnSpec{{Name: "id", Type: "uuid"}}},
	}}
	live := LiveSnapshot{Tables: map[string]TableSpec{
		"context_blocks": {Columns: []ColumnSpec{{Name: "id", Type: "uuid"}, {Name: "embed_status", Type: "text"}}},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassUnknownObject || drifts[0].Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want unknown_object/param", drifts[0].Class, drifts[0].Severity)
	}
	if drifts[0].Object != "table:context_blocks.embed_status" {
		t.Errorf("Object = %q, want table:context_blocks.embed_status", drifts[0].Object)
	}
}

// TestDiff_UnknownTable_SnapshotExceptionScope: only relkind='r' tables
// matching *_snapshot_* are excluded from unknown_object — proven both
// ways (excluded table produces zero drift + increments the count;
// non-matching table still reports).
func TestDiff_UnknownTable_SnapshotExceptionScope(t *testing.T) {
	m := Manifest{Tables: map[string]TableSpec{}}
	live := LiveSnapshot{Tables: map[string]TableSpec{
		"context_dream_links_snapshot_20260423_prev5": {},
		"some_hand_added_table":                       {},
	}}
	drifts, excluded := diffDetailed(m, live)
	if excluded != 1 {
		t.Errorf("excluded = %d, want 1", excluded)
	}
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift (the non-snapshot table only), got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Object != "table:some_hand_added_table" {
		t.Errorf("Object = %q, want table:some_hand_added_table — the snapshot-named table must NOT appear here", drifts[0].Object)
	}
	if drifts[0].Class != ClassUnknownObject || drifts[0].Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want unknown_object/param", drifts[0].Class, drifts[0].Severity)
	}
}

// TestDiff_UnknownTrigger_SnapshotNameStillReported_Breaking is the rot-
// probe named in the W03-2 brief: a hand-added trigger whose name matches
// the *_snapshot_* pattern MUST still be reported as unknown_active_object/
// breaking — the exception is scoped to relkind='r' tables only (design/03
// §4.4). A pauschale (blanket) classifier that routed every "*_snapshot_*"
// name through the table exception, or that classified all unknown live
// objects as param regardless of class, would make this trigger vanish
// from the report or downgrade it to param — either way this assertion
// (Class==unknown_active_object AND Severity==breaking, on an object whose
// name contains "_snapshot_") tears.
func TestDiff_UnknownTrigger_SnapshotNameStillReported_Breaking(t *testing.T) {
	m := Manifest{Triggers: map[string]TriggerSpec{}}
	live := LiveSnapshot{Triggers: map[string]TriggerSpec{
		"context_blocks.audit_snapshot_trigger": {DefHash: "x"},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	d := drifts[0]
	if d.Class != ClassUnknownActiveObject {
		t.Errorf("Class = %s, want unknown_active_object (a pauschale param-classification would put this at unknown_object — ROT)", d.Class)
	}
	if d.Severity != SeverityBreaking {
		t.Errorf("Severity = %s, want breaking (a pauschale param-classification would put this at param — ROT)", d.Severity)
	}
}

// TestDiff_UnknownFunction_SnapshotNameStillReported_Breaking mirrors the
// trigger case for functions — the exception never applies to active
// objects, regardless of naming (design/03 §4.4).
func TestDiff_UnknownFunction_SnapshotNameStillReported_Breaking(t *testing.T) {
	m := Manifest{Functions: map[string]FuncSpec{}}
	live := LiveSnapshot{Functions: map[string]FuncSpec{
		"restore_snapshot_helper()": {SrcHash: "x"},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassUnknownActiveObject || drifts[0].Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want unknown_active_object/breaking", drifts[0].Class, drifts[0].Severity)
	}
}

func TestDiff_UnknownIndex_NoSnapshotException_Param(t *testing.T) {
	m := Manifest{Indexes: map[string]IndexSpec{}}
	live := LiveSnapshot{Indexes: map[string]IndexSpec{
		"idx_context_blocks_snapshot_backup": {DefHash: "x"},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	// Indexes get no snapshot exception at all (design/03 §4.4 scopes the
	// exception to tables only) but stay param severity regardless of name,
	// since an index is a passive object either way.
	if drifts[0].Class != ClassUnknownObject || drifts[0].Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want unknown_object/param", drifts[0].Class, drifts[0].Severity)
	}
}

func TestDiff_UnknownFunction_Breaking(t *testing.T) {
	m := Manifest{Functions: map[string]FuncSpec{}}
	live := LiveSnapshot{Functions: map[string]FuncSpec{"evil_hook()": {SrcHash: "x"}}}
	drifts := Diff(m, live)
	if len(drifts) != 1 || drifts[0].Class != ClassUnknownActiveObject || drifts[0].Severity != SeverityBreaking {
		t.Fatalf("got %+v, want single unknown_active_object/breaking", drifts)
	}
}

func TestDiff_FunctionBodyChange_Param(t *testing.T) {
	m := Manifest{Functions: map[string]FuncSpec{"ctx_rrf(halfvec)": {SrcHash: "aaa"}}}
	live := LiveSnapshot{Functions: map[string]FuncSpec{"ctx_rrf(halfvec)": {SrcHash: "bbb"}}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassDefinitionDrift || drifts[0].Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want definition_drift/param — a MODIFIED declared function is param, an UNDECLARED one is breaking", drifts[0].Class, drifts[0].Severity)
	}
}

func TestDiff_UnknownRule_Breaking(t *testing.T) {
	m := Manifest{Rules: map[string]RuleSpec{}}
	live := LiveSnapshot{Rules: map[string]RuleSpec{"context_blocks.evil_redirect": {DefHash: "x"}}}
	drifts := Diff(m, live)
	if len(drifts) != 1 || drifts[0].Class != ClassUnknownActiveObject || drifts[0].Severity != SeverityBreaking {
		t.Fatalf("got %+v, want single unknown_active_object/breaking", drifts)
	}
}

func TestDiff_Policy_AlwaysUnknownActiveObject(t *testing.T) {
	live := LiveSnapshot{Policies: []string{"context_api_keys.leak_policy"}}
	drifts := Diff(Manifest{}, live)
	if len(drifts) != 1 || drifts[0].Class != ClassUnknownActiveObject || drifts[0].Severity != SeverityBreaking {
		t.Fatalf("got %+v, want single unknown_active_object/breaking", drifts)
	}
	if drifts[0].Object != "policy:context_api_keys.leak_policy" {
		t.Errorf("Object = %q", drifts[0].Object)
	}
}

// TestDiff_RowSecurityFlip_BreakingException proves the definition_drift
// severity exception: every other definition_drift is param, but an RLS
// flip on a declared table is breaking (design/03 §4.3/§4.4).
func TestDiff_RowSecurityFlip_BreakingException(t *testing.T) {
	m := Manifest{Tables: map[string]TableSpec{"context_api_keys": {RowSecurity: false}}}
	live := LiveSnapshot{Tables: map[string]TableSpec{"context_api_keys": {RowSecurity: true}}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassDefinitionDrift || drifts[0].Severity != SeverityBreaking {
		t.Errorf("got class=%s severity=%s, want definition_drift/breaking (RLS-flip exception)", drifts[0].Class, drifts[0].Severity)
	}
}

func TestDiff_IndexRelOptionsDrift_ReportsFieldNotJustHash(t *testing.T) {
	m := Manifest{Indexes: map[string]IndexSpec{
		"idx_embedding_hnsw": {DefHash: "same", RelOptions: map[string]string{"m": "16", "ef_construction": "128"}},
	}}
	live := LiveSnapshot{Indexes: map[string]IndexSpec{
		"idx_embedding_hnsw": {DefHash: "same", RelOptions: map[string]string{"m": "16", "ef_construction": "64"}},
	}}
	drifts := Diff(m, live)
	if len(drifts) != 1 {
		t.Fatalf("want 1 drift, got %d: %+v", len(drifts), drifts)
	}
	if drifts[0].Class != ClassDefinitionDrift || drifts[0].Severity != SeverityParam {
		t.Errorf("got class=%s severity=%s, want definition_drift/param", drifts[0].Class, drifts[0].Severity)
	}
	want := "reloptions ef_construction: manifest=128 live=64"
	if drifts[0].Detail != want {
		t.Errorf("Detail = %q, want %q", drifts[0].Detail, want)
	}
}

func TestDiff_ExtensionMissing_Breaking(t *testing.T) {
	m := Manifest{Extensions: map[string]ExtSpec{"vector": {MinVersion: "0.8.0"}}}
	live := LiveSnapshot{Extensions: map[string]LiveExtension{}}
	drifts := Diff(m, live)
	if len(drifts) != 1 || drifts[0].Class != ClassMissingObject || drifts[0].Severity != SeverityBreaking {
		t.Fatalf("got %+v, want single missing_object/breaking", drifts)
	}
}

func TestDiff_ExtensionBelowMinimum_Breaking(t *testing.T) {
	m := Manifest{Extensions: map[string]ExtSpec{"vector": {MinVersion: "0.8.10"}}}
	live := LiveSnapshot{Extensions: map[string]LiveExtension{"vector": {Version: "0.8.2"}}}
	drifts := Diff(m, live)
	if len(drifts) != 1 || drifts[0].Class != ClassExtensionVersion || drifts[0].Severity != SeverityBreaking {
		t.Fatalf("got %+v, want single extension_version/breaking", drifts)
	}
}

func TestDiff_ExtensionAboveMinimum_NoDrift(t *testing.T) {
	// A routine image bump must NEVER become a drift (E-03-3: minimum-pin,
	// not exact-pin) — an enforce boot must survive a pgvector 0.8.2 -> 0.8.4
	// upgrade.
	m := Manifest{Extensions: map[string]ExtSpec{"vector": {MinVersion: "0.8.2"}}}
	live := LiveSnapshot{Extensions: map[string]LiveExtension{"vector": {Version: "0.8.4"}}}
	if drifts := Diff(m, live); len(drifts) != 0 {
		t.Fatalf("want 0 drifts on an above-minimum upgrade, got %+v", drifts)
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.8.2", "0.8.2", 0},
		{"0.8.2", "0.8.10", -1}, // NOT a lexicographic compare
		{"0.8.10", "0.8.2", 1},
		{"2.26.0", "2.9.0", 1},
		{"1", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := compareVersions(c.a, c.b); got != c.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestDiff_HypertableMismatch_BothDirections(t *testing.T) {
	m := Manifest{Hypertables: []string{"context_llm_log", "context_pending_writes"}}
	live := LiveSnapshot{Hypertables: []string{"context_pending_writes", "context_access_log"}}
	drifts := Diff(m, live)
	if len(drifts) != 2 {
		t.Fatalf("want 2 drifts, got %d: %+v", len(drifts), drifts)
	}
	byObject := map[string]Drift{}
	for _, d := range drifts {
		byObject[d.Object] = d
	}
	missing, ok := byObject["table:context_llm_log"]
	if !ok || missing.Class != ClassDefinitionDrift {
		t.Errorf("expected definition_drift for table:context_llm_log, got %+v", byObject)
	}
	extra, ok := byObject["table:context_access_log"]
	if !ok || extra.Class != ClassDefinitionDrift {
		t.Errorf("expected definition_drift for table:context_access_log, got %+v", byObject)
	}
}
