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
// carrier (the parent logs skip reasons exactly like the in-process path).
func TestWorkerStatsRoundtrip(t *testing.T) {
	cases := []overview.Stats{
		{NodeCount: 6, ClusterCount: 2, EdgeRows: 3, Modularity: 0.4375},
		{Skipped: true, SkipReason: "node-cap"},
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
		if in != out {
			t.Errorf("case %d: roundtrip mutated stats:\n  in : %+v\n  out: %+v", i, in, out)
		}
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
