package handler

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestQueryRetrievalWiring structurally pins the T5 policy sourcing
// (design/01 §7-T5 DB-sourcing probe, static side): the retrieval callers
// MUST feed ctx_rrf from a registry snapshot (SnapshotForRequest /
// SnapshotForTenant via Router.TypeSet) and MUST NOT construct a fresh
// registry (= compiled-in builtin set) at the call site. Builtin set and
// M072 seeds are golden-identical, so every behavioural test would stay
// green under that drift — only a live registry edit (integration probe
// TestT5_DampingPolicyFromDB) or this structural pin catches it.
func TestQueryRetrievalWiring(t *testing.T) {
	querySrc, err := os.ReadFile("query.go")
	if err != nil {
		t.Fatalf("read query.go: %v", err)
	}
	qs := string(querySrc)

	if !strings.Contains(qs, "h.blocktypes.SnapshotForRequest(ctx)") {
		t.Error("query.go: retrieval no longer sources the type policy from h.blocktypes.SnapshotForRequest(ctx)")
	}
	for _, arg := range []string{"visibleTypes", "dampedTypes", "dampedFactors"} {
		if !strings.Contains(qs, arg) {
			t.Errorf("query.go: rrf.Search wiring lost the snapshot-derived %s argument", arg)
		}
	}

	dreamSrc, err := os.ReadFile(filepath.Join("..", "dream", "dream.go"))
	if err != nil {
		t.Fatalf("read dream.go: %v", err)
	}
	if !strings.Contains(string(dreamSrc), "r.TypeSet(ctx)") {
		t.Error("dream.go: candidate search no longer resolves the tenant type set via Router.TypeSet")
	}

	// No retrieval caller may build its own registry — that would serve the
	// compiled-in builtin set instead of the DB-backed process singleton.
	for _, dir := range []string{".", filepath.Join("..", "dream")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir %s: %v", dir, err)
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			src, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s: %v", name, err)
			}
			if strings.Contains(string(src), "blocktype.NewRegistry(") {
				t.Errorf("%s/%s constructs its own blocktype.Registry — retrieval must use the injected process singleton", dir, name)
			}
		}
	}
}
