//go:build integration

// V-W6 result-set gate (design/05 §7): the MCP `search` tool's `types` argument
// must CUT the answer down to the requested types. Since #342 the names are
// checked against the registry snapshot, not the visible allowlist: a
// retrieval-excluded type is a filter like any other
// (mcp_search_types_neverranked_integration_test.go), an unknown name a refusal.
//
// Like the short probes, every argument object is decoded from JSON rather than
// written as a Go struct literal: before this wave the key was unknown and the
// decoder dropped it, and that silent drop IS the red state.
//
//	go test -tags=integration ./internal/handler/ -run TestMCPTypeFilterSearch -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const vw6Category = "vw6-typefilter"

// seedVW6Fixture writes one block per type axis the gate needs: two VISIBLE
// types (knowledge full-pass, audit-trail damped) and one retrieval-EXCLUDED
// type (checkpoint), all in one category so the probes can address them
// without an FTS match.
func seedVW6Fixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, fx := range []struct{ title, typeName string }{
		{"VW6 Knowledge", "knowledge"},
		{"VW6 Audit", "audit-trail"},
		{"VW6 Checkpoint", "checkpoint"},
	} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ($1, $2, 'vw6 fixture', 'private', $3)`,
			vw6Category, fx.title, fx.typeName); err != nil {
			t.Fatalf("seed %s: %v", fx.title, err)
		}
	}
}

func vw6Cfg(t *testing.T, pool *pgxpool.Pool) MCPConfig {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return MCPConfig{Pool: pool, Blocktypes: reg}
}

func vw6Ctx() context.Context {
	return context.WithValue(context.Background(), authResultKey, &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "private", ReadScopes: []string{"private"},
	})
}

// vw6Search runs the MCP search tool over a raw argument object and returns the
// per-type row counts of the answer.
func vw6Search(t *testing.T, cfg MCPConfig, raw string) map[string]int {
	t.Helper()
	var in searchInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode tool arguments %s: %v", raw, err)
	}
	res, _, err := mcpSearchHandler(cfg)(vw6Ctx(), nil, in)
	if err != nil {
		t.Fatalf("search %s: transport error %v", raw, err)
	}
	if res.IsError {
		t.Fatalf("search %s: IsError=true (%s)", raw, mcpTextOf(res))
	}
	return vw6CountTypes(t, []byte(mcpTextOf(res)))
}

func vw6CountTypes(t *testing.T, raw []byte) map[string]int {
	t.Helper()
	var rows []struct {
		Category string `json:"category"`
		TypeName string `json:"type"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		t.Fatalf("decode search rows %s: %v", raw, err)
	}
	out := map[string]int{}
	for _, r := range rows {
		if r.Category == vw6Category {
			out[r.TypeName]++
		}
	}
	return out
}

// TestMCPTypeFilterSearchCutsToRequestedVisibleTypes is the red→green core.
//
// RED before this wave: `types` is an unknown key, the decoder drops it, and
// the answer still carries the audit-trail row.
func TestMCPTypeFilterSearchCutsToRequestedVisibleTypes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	seedVW6Fixture(t, pool)
	cfg := vw6Cfg(t, pool)

	t.Run("baseline_without_filter", func(t *testing.T) {
		got := vw6Search(t, cfg, `{"category":"`+vw6Category+`"}`)
		if got["knowledge"] != 1 || got["audit-trail"] != 1 || got["checkpoint"] != 1 {
			t.Fatalf("unfiltered fixture answer = %v, want one row per seeded type", got)
		}
	})

	t.Run("types_cuts_to_knowledge", func(t *testing.T) {
		got := vw6Search(t, cfg, `{"category":"`+vw6Category+`","types":["knowledge"]}`)
		if got["knowledge"] != 1 {
			t.Errorf("types=[knowledge] lost the knowledge row: %v", got)
		}
		if got["audit-trail"] != 0 {
			t.Errorf("types=[knowledge] still returns audit-trail rows: %v", got)
		}
		if len(got) != 1 {
			t.Errorf("types=[knowledge] answer carries other types: %v", got)
		}
	})

	t.Run("types_exclude_removes_exactly_those_rows", func(t *testing.T) {
		got := vw6Search(t, cfg, `{"category":"`+vw6Category+`","types_exclude":["audit-trail"]}`)
		if got["audit-trail"] != 0 {
			t.Errorf("types_exclude=[audit-trail] still returns audit-trail rows: %v", got)
		}
		if got["knowledge"] != 1 || got["checkpoint"] != 1 {
			t.Errorf("types_exclude=[audit-trail] removed more than the named type: %v", got)
		}
	})

	t.Run("no_fields_is_unchanged", func(t *testing.T) {
		// Non-regression at the seam: with neither field the tool must answer
		// exactly what the store call it wraps answers with nil/nil filters —
		// the arguments the handler passed before the fields existed.
		//
		// V-11 note: the reference call now takes the tool's OWN registry
		// snapshot, because that is what the wrapped call takes since V-11. The
		// TYPE FILTER arguments stay nil/nil — the invariant under test — and
		// the snapshot is the untrusted framing input, not a filter. Passing
		// nil here instead would compare the tool against a DIFFERENT store
		// call than the one it makes.
		var in searchInput
		if err := json.Unmarshal([]byte(`{"category":"`+vw6Category+`"}`), &in); err != nil {
			t.Fatalf("decode: %v", err)
		}
		res, _, err := mcpSearchHandler(cfg)(vw6Ctx(), nil, in)
		if err != nil || res.IsError {
			t.Fatalf("search: err=%v result=%s", err, mcpTextOf(res))
		}
		ctx := vw6Ctx()
		refGrants, err := resolveGrants(ctx, pool, AuthResultFromContext(ctx))
		if err != nil { // T04-20: the reference call must resolve like the tool does
			t.Fatalf("reference grants: %v", err)
		}
		want, err := store.SearchBlocks(ctx, pool, cfg.mcpTypeSnapshot(ctx), "", []string{"private"}, vw6Category, nil, 10, true,
			nil, refGrants, nil, nil, nil)
		if err != nil {
			t.Fatalf("reference search: %v", err)
		}
		wantJSON, _ := json.MarshalIndent(want, "", "  ")
		if mcpTextOf(res) != string(wantJSON) {
			t.Fatalf("answer without type fields differs from the pre-wave store call:\n got %s\nwant %s",
				mcpTextOf(res), wantJSON)
		}
	})
}

// TestMCPTypeFilterSearchRejectsUnknown covers negative probe (a) at the
// RESULT level: an unknown name answers neither rows nor an empty list, it is a
// refusal. Probe (b), a retrieval-excluded name, is admitted since #342.
func TestMCPTypeFilterSearchRejectsUnknown_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	seedVW6Fixture(t, pool)
	cfg := vw6Cfg(t, pool)

	for name, raw := range map[string]string{
		"unknown_name": `{"category":"` + vw6Category + `","types":["nicht-existent"]}`,
	} {
		t.Run(name, func(t *testing.T) {
			var in searchInput
			if err := json.Unmarshal([]byte(raw), &in); err != nil {
				t.Fatalf("decode: %v", err)
			}
			res, _, err := mcpSearchHandler(cfg)(vw6Ctx(), nil, in)
			if err != nil {
				t.Fatalf("transport error: %v", err)
			}
			if !res.IsError {
				t.Fatalf("%s: IsError=false — argument ignored or silently emptied (%s)", raw, mcpTextOf(res))
			}
		})
	}
}
