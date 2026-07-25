package rrf

import (
	"context"
	"reflect"
	"sort"
	"testing"

	"github.com/GottZ/ctx/internal/graphcache"
)

// TestExpandArmSeam_DreamOnly is the STRUCTURAL half of the GA5 gate (§3.2
// rationale c, W05.7): the expand cache arm's whole snapshot surface is the
// dreamGraph interface, so a structural or supersedes edge is not merely
// filtered out of the walk — it is unreachable from this arm's type.
//
// The assertion is on the interface's method SET, not on a call site: a future
// author who adds StructNeighbors/InducedEdges/Degree to the seam (the only way
// this arm could ever surface a non-dream edge) trips this test before any
// behavioural gate has to catch the leak downstream.
func TestExpandArmSeam_DreamOnly(t *testing.T) {
	typ := reflect.TypeOf((*dreamGraph)(nil)).Elem()
	got := make([]string, 0, typ.NumMethod())
	for i := 0; i < typ.NumMethod(); i++ {
		got = append(got, typ.Method(i).Name)
	}
	sort.Strings(got)
	want := []string{"DreamNeighbors", "NodeID", "NodeUUID"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("dreamGraph seam = %v, want exactly %v — GA5 isolation is structural: "+
			"a structural/supersedes accessor here would let retrieval inject a non-dream edge", got, want)
	}

	// And the arm really holds the NARROW type, not the snapshot: a widened
	// field would make the interface above decorative.
	f, ok := reflect.TypeOf(expandCacheArm{}).FieldByName("dream")
	if !ok {
		t.Fatal("expandCacheArm has no snapshot field named dream")
	}
	if f.Type != typ {
		t.Errorf("expandCacheArm.dream is %v, want the narrow interface %v", f.Type, typ)
	}
}

// TestGraphExpandCached_NilSnapshotIsSQLPath pins that a zero ExpandCache is the
// permanent SQL fallback: the disabled stage reports Source="sql" and returns
// the input unchanged, byte-identical to GraphExpandWithReport.
func TestGraphExpandCached_NilSnapshotIsSQLPath(t *testing.T) {
	cfg := defaultGraphCfg()
	cfg.Enabled = false
	in := []SearchResult{res("A", 1.0)}

	out, rep, err := GraphExpandCachedWithReport(context.Background(), nil, in,
		[]string{"private"}, nil, []string{"knowledge"}, cfg, ExpandCache{})
	if err != nil {
		t.Fatalf("disabled stage returned an error: %v", err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Errorf("disabled stage changed the results: %+v", out)
	}
	if rep == nil || rep.Source != graphcache.SourceSQL {
		t.Errorf("report source = %+v, want %q", rep, graphcache.SourceSQL)
	}
}
