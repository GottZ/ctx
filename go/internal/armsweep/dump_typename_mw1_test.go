package armsweep_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/rrf"
)

// M-W1 dump-schema gates. The dump row schema IS rrf.ArmRow (dump.go:67), so
// the ninth SQL column reaches the artefact through encoding/json and nothing
// else. Two properties have to hold and neither is self-evident from the struct
// tag alone.
//
//  1. A dump written after M-W1 carries `type_name` on every row. The field has
//     no omitempty, so it survives even for a block whose type is the empty
//     string — a dump that silently drops the column for some rows would give
//     the type census a hole it cannot see.
//
//  2. A dump written BEFORE M-W1 stays readable. ReadRecords (dump.go:180-197)
//     is a plain json.Unmarshal per line with no schema version and no
//     DisallowUnknownFields, so an absent field decodes to the zero value. That
//     is the Ist behaviour, pinned here rather than assumed: `type_name` comes
//     back as the empty string, and every other field of the old row is
//     untouched. The empty string is therefore the "unknown, old dump" marker —
//     a consumer that counts types must treat it as such instead of as a type
//     named "".
func TestMW1DumpCarriesTypeName(t *testing.T) {
	path := filepath.Join(t.TempDir(), "dump.jsonl")
	rec := armsweep.Record{
		Slice:       "g-mw1",
		Index:       1,
		QuerySHA256: strings.Repeat("a", 64),
		Rows: []rrf.ArmRow{
			{ID: "019fa402-0000-7000-9000-000000050000", MassFactor: 1, TypeFactor: 1, TypeName: "knowledge"},
			{ID: "019fa402-0000-7000-9000-000000050001", MassFactor: 1, TypeFactor: 0.3, TypeName: "audit-trail"},
		},
		FusionOrder: []string{"019fa402-0000-7000-9000-000000050000"},
		Attempts:    1,
	}
	if err := armsweep.WriteRecords(path, []armsweep.Record{rec}); err != nil {
		t.Fatalf("WriteRecords: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if n := strings.Count(string(raw), `"type_name"`); n != 2 {
		t.Errorf("dump carries %d type_name keys, want 2 (one per row):\n%s", n, raw)
	}

	got, err := armsweep.ReadRecords(path)
	if err != nil {
		t.Fatalf("ReadRecords: %v", err)
	}
	if len(got) != 1 || len(got[0].Rows) != 2 {
		t.Fatalf("roundtrip shape: %d records, want 1 with 2 rows", len(got))
	}
	if got[0].Rows[0].TypeName != "knowledge" || got[0].Rows[1].TypeName != "audit-trail" {
		t.Errorf("roundtrip lost the type: %q / %q", got[0].Rows[0].TypeName, got[0].Rows[1].TypeName)
	}
}

// TestMW1ReadsPreMW1Dump pins the forward-compatibility half: a dump line
// written by the pre-142 driver — eight row fields, no type_name — still loads,
// and the missing type reads as the empty string rather than failing the run.
func TestMW1ReadsPreMW1Dump(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.jsonl")
	// Verbatim shape of a pre-M-W1 row: no "type_name" key anywhere.
	const old = `{"slice":"g-old","index":7,"query_sha256":"` + `deadbeef` + `","rows":` +
		`[{"id":"019fa402-0000-7000-9000-000000050000","rank_semantic":3,"rank_fts_de":null,` +
		`"rank_fts_en":null,"rank_trigram":null,"cos_sim":0.5,"mass_factor":1,"type_factor":0.3}],` +
		`"fusion_order":["019fa402-0000-7000-9000-000000050000"],"delivered":null,` +
		`"effective_query":"q","effective_query_spaced":"q","effective_temporal":"",` +
		`"embed_model":"m","embed_cache_hit":false,` +
		`"selector":{"mode":"ann","reason":"","estimate":0,"scan_tuples":null,"exact_cap":null},` +
		`"attempts":1,"latency_ms":12}` + "\n"
	if strings.Contains(old, "type_name") {
		t.Fatal("fixture is not a pre-M-W1 dump — it already carries type_name")
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	got, err := armsweep.ReadRecords(path)
	if err != nil {
		t.Fatalf("ReadRecords on a pre-M-W1 dump: %v", err)
	}
	if len(got) != 1 || len(got[0].Rows) != 1 {
		t.Fatalf("shape: %d records, want 1 with 1 row", len(got))
	}
	r := got[0].Rows[0]
	if r.TypeName != "" {
		t.Errorf("missing type_name decoded to %q, want the empty string", r.TypeName)
	}
	// The rest of the old row must be intact — the new field must not shift
	// anything, and a reader that suddenly zeroed a rank would be far worse
	// than one that cannot name a type.
	if r.ID != "019fa402-0000-7000-9000-000000050000" || r.RankSemantic == nil || *r.RankSemantic != 3 {
		t.Errorf("old row damaged: id=%q rank_semantic=%v", r.ID, r.RankSemantic)
	}
	if r.CosSim == nil || *r.CosSim != 0.5 || r.MassFactor != 1 || r.TypeFactor != 0.3 {
		t.Errorf("old row damaged: cos_sim=%v mass=%v type_factor=%v", r.CosSim, r.MassFactor, r.TypeFactor)
	}
	if got[0].Attempts != 1 || got[0].LatencyMS != 12 {
		t.Errorf("old record damaged: attempts=%d latency=%d", got[0].Attempts, got[0].LatencyMS)
	}
}
