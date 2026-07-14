//go:build integration

// GB5 gates over the real stack (design/02 §4.2, §7-A7): the link_class
// partition end-to-end — a structural class is a valid selection (the former
// 400), a dream-only selection hides structural edges, and the INVERSE probe
// pins the §4.6 sentinel on the wired path (red against a nilIfEmpty bind:
// `link_class=references` would then show ALL dream edges between the
// structurally reached nodes — a silent filter fail-open).
package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

func gpSetup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := hgSetup(t)
	// The collision pair: hgShared→hgShared2 already carries a dream edge
	// (hgSetup); add a structural fact on the same directed pair.
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_structural_links (source_block_id, target_block_id, link_class, scope, origin)
		 VALUES ($1::uuid,$2::uuid,'references','shared','system')`, hgShared, hgShared2); err != nil {
		t.Fatalf("insert structural link: %v", err)
	}
	return pool
}

type gpEnvelope struct {
	Params struct {
		LinkClass []string `json:"link_class"`
	} `json:"params"`
	Edges           []json.RawMessage `json:"edges"`
	StructuralEdges []json.RawMessage `json:"structural_edges"`
	Stats           struct {
		Edges           int `json:"edges"`
		StructuralEdges int `json:"structural_edges"`
	} `json:"stats"`
}

func gpDo(t *testing.T, pool *pgxpool.Pool, query string) (int, gpEnvelope) {
	t.Helper()
	rec := hgDo(t, pool, hgAuth(hgKeyA, "private"), query)
	var env gpEnvelope
	if rec.Code == 200 {
		if err := json.Unmarshal(rec.Body.Bytes(), &env); err != nil {
			t.Fatalf("invalid envelope: %v", err)
		}
	}
	return rec.Code, env
}

// TestHandleEgo_StructuralClassSelectable: link_class=references is a 200
// (the pre-GB5 400, inverted) with the INVERSE shape — NO dream edges (the
// empty dream partition matches nothing, §4.6 sentinel on the wired path),
// the structural edge ships, and the params echo carries the raw CSV.
func TestHandleEgo_StructuralClassSelectable(t *testing.T) {
	pool := gpSetup(t)

	code, env := gpDo(t, pool, "block="+hgShared+"&link_class=references")
	if code != 200 {
		t.Fatalf("link_class=references → %d, want 200 (pre-GB5 400 pin inverted)", code)
	}
	if len(env.Edges) != 0 || env.Stats.Edges != 0 {
		t.Errorf("INVERSE probe: dream edges = %d (stats %d), want 0 — empty dream partition leaked through the Q2 bind (nilIfEmpty fail-open)", len(env.Edges), env.Stats.Edges)
	}
	if len(env.StructuralEdges) != 1 || env.Stats.StructuralEdges != 1 {
		t.Errorf("structural_edges = %d (stats %d), want 1", len(env.StructuralEdges), env.Stats.StructuralEdges)
	}
	if len(env.Params.LinkClass) != 1 || env.Params.LinkClass[0] != "references" {
		t.Errorf("params.link_class echo = %v, want raw CSV [references]", env.Params.LinkClass)
	}
}

// TestHandleEgo_DreamClassHidesStructural: link_class=topical delivers the
// dream edge and hides every structural edge — "only topical" means only
// topical, on both data classes.
func TestHandleEgo_DreamClassHidesStructural(t *testing.T) {
	pool := gpSetup(t)

	code, env := gpDo(t, pool, "block="+hgShared+"&link_class=topical")
	if code != 200 {
		t.Fatalf("link_class=topical → %d, want 200", code)
	}
	if len(env.Edges) != 1 {
		t.Errorf("dream edges = %d, want 1", len(env.Edges))
	}
	if len(env.StructuralEdges) != 0 || env.Stats.StructuralEdges != 0 {
		t.Errorf("structural_edges = %d, want 0 (dream-only selection)", len(env.StructuralEdges))
	}
}

// TestHandleEgo_DefaultDeliversBoth: no link_class param → both classes ship
// (default visibility without any new parameter — the user directive).
func TestHandleEgo_DefaultDeliversBoth(t *testing.T) {
	pool := gpSetup(t)

	code, env := gpDo(t, pool, "block="+hgShared)
	if code != 200 {
		t.Fatalf("default → %d, want 200", code)
	}
	if len(env.Edges) != 1 || len(env.StructuralEdges) != 1 {
		t.Errorf("default: dream=%d structural=%d, want 1/1", len(env.Edges), len(env.StructuralEdges))
	}
	if env.Params.LinkClass != nil {
		t.Errorf("params.link_class echo = %v, want null (absent param)", env.Params.LinkClass)
	}
}
