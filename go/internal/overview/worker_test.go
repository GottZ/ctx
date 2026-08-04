package overview_test

import (
	"bytes"
	"reflect"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/overview"
)

// TestWorkerOptionsRoundtrip pins the E-A IPC contract: an Options value
// crosses the encode→decode boundary unchanged — including the nil-vs-set
// ScopeFilter distinction (nil = global run; JSON null round-trips to nil,
// so lockKeyForScopes sees the same key on both sides of the process
// boundary).
func TestWorkerOptionsRoundtrip(t *testing.T) {
	cases := []overview.Options{
		{Resolution: 1.0, VisibleTypes: []string{"knowledge"}, OverviewTypes: []string{"knowledge"}},
		{Resolution: 2.5, VisibleTypes: []string{"knowledge", "issue"}, OverviewTypes: []string{"knowledge"}, MaxNodes: 200000, ScopeFilter: []string{"private", "shared"}},
		{}, // zero value survives too (Rebuild rejects it loudly, but the WIRE must not mangle it)
	}
	for i, in := range cases {
		var buf bytes.Buffer
		if err := overview.EncodeWorkerOptions(&buf, in); err != nil {
			t.Fatalf("case %d: encode: %v", i, err)
		}
		out, err := overview.DecodeWorkerOptions(&buf)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("case %d: roundtrip mutated options:\n  in : %+v\n  out: %+v", i, in, out)
		}
	}
}

// TestWorkerStatsRoundtrip pins the child→parent leg, including the Skipped
// carrier (the parent logs skip reasons exactly like the in-process path) and
// — since W-A — the per-scope CandidateCount map the parent needs to stamp a
// skipped attempt (migration 123). The comparison is DeepEqual because Stats
// carries a map since W-A and is no longer comparable with ==.
func TestWorkerStatsRoundtrip(t *testing.T) {
	cases := []overview.Stats{
		{NodeCount: 6, ClusterCount: 2, EdgeRows: 3, Modularity: 0.4375},
		{Skipped: true, SkipReason: "node-cap"},
		// W-A: the candidate tally must survive the process boundary PER
		// SCOPE — a collapsed scalar would write a foreign partition's corpus
		// size into a tenant's own persisted block (BP-1).
		{Skipped: true, SkipReason: "node-cap", CandidateCount: map[string]int{"private": 5, "shared": 500}},
		{NodeCount: 505, ClusterCount: 12, EdgeRows: 40, Modularity: 0.87, CandidateCount: map[string]int{"private": 505}},
	}
	for i, in := range cases {
		var buf bytes.Buffer
		if err := overview.EncodeWorkerStats(&buf, in); err != nil {
			t.Fatalf("case %d: encode: %v", i, err)
		}
		out, err := overview.DecodeWorkerStats(&buf)
		if err != nil {
			t.Fatalf("case %d: decode: %v", i, err)
		}
		if !reflect.DeepEqual(in, out) {
			t.Errorf("case %d: roundtrip mutated stats:\n  in : %+v\n  out: %+v", i, in, out)
		}
	}
}

// TestDecodeWorkerStats_OldFieldSetRejected is the W-A IPC gate (design/02 §7
// gate 10): the field addition IS a protocol change. A parent decoding a
// document from a binary that does not know CandidateCount is fine (missing
// fields are legal JSON), but the reverse — an OLD strict decoder facing the
// NEW field — must fail loudly rather than cluster on silently dropped data.
// Simulated here the only way a single binary can: an unknown-to-the-decoder
// sibling field. This is why W-A and the Achse-04 S2 wave (which extends the
// same struct) share one deploy window (masterplan K12).
func TestDecodeWorkerStats_OldFieldSetRejected(t *testing.T) {
	// The exact shape a NEXT-generation binary would emit against today's
	// decoder: known fields plus one it has never heard of.
	_, err := overview.DecodeWorkerStats(strings.NewReader(
		`{"NodeCount": 3, "CandidateCount": {"private": 3}, "RunJournalID": "abc"}`))
	if err == nil {
		t.Fatal("stats document with a future field decoded without error — mixed-version drift would be silent")
	}
	// …while the CURRENT field set decodes cleanly, so the gate above is
	// attributable to the unknown field, not to the map itself.
	got, err := overview.DecodeWorkerStats(strings.NewReader(
		`{"NodeCount": 3, "CandidateCount": {"private": 3}}`))
	if err != nil {
		t.Fatalf("current stats field set rejected: %v", err)
	}
	if got.CandidateCount["private"] != 3 {
		t.Fatalf("CandidateCount = %v, want private:3", got.CandidateCount)
	}
}

// TestDecodeWorkerOptions_BrokenJSON is the E-A negative gate at the protocol
// layer: garbage on stdin must be an error, never a zero-value Options that
// would reach Rebuild.
func TestDecodeWorkerOptions_BrokenJSON(t *testing.T) {
	if _, err := overview.DecodeWorkerOptions(strings.NewReader(`{"Resolution": nope`)); err == nil {
		t.Fatal("broken options JSON decoded without error")
	}
	if _, err := overview.DecodeWorkerOptions(strings.NewReader(``)); err == nil {
		t.Fatal("empty stdin decoded without error")
	}
}

// TestDecodeWorkerOptions_UnknownFieldRejected pins the STRICT decoder: an
// unknown field means protocol drift between spawner and worker (a
// mixed-version window) and must fail loudly — the daemon then falls back to
// the in-process path instead of clustering with silently dropped options.
func TestDecodeWorkerOptions_UnknownFieldRejected(t *testing.T) {
	_, err := overview.DecodeWorkerOptions(strings.NewReader(`{"Resolution": 1.0, "NiceLevel": 19}`))
	if err == nil {
		t.Fatal("unknown option field decoded without error — decoder is not strict, protocol drift would be silent")
	}
}

// TestDecodeWorkerStats_UnknownFieldRejected — same strictness, child→parent leg.
func TestDecodeWorkerStats_UnknownFieldRejected(t *testing.T) {
	_, err := overview.DecodeWorkerStats(strings.NewReader(`{"NodeCount": 3, "Zombies": 1}`))
	if err == nil {
		t.Fatal("unknown stats field decoded without error — decoder is not strict")
	}
}
