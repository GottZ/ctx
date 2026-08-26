//go:build integration

// V-W6 result-set gate (design/05 §7): the MCP `search` tool's `types` argument
// must CUT the answer down to the requested visible types — and the cut has to
// come from the visible allowlist, not from a bare registry-existence check.
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
		var in searchInput
		if err := json.Unmarshal([]byte(`{"category":"`+vw6Category+`"}`), &in); err != nil {
			t.Fatalf("decode: %v", err)
		}
		res, _, err := mcpSearchHandler(cfg)(vw6Ctx(), nil, in)
		if err != nil || res.IsError {
			t.Fatalf("search: err=%v result=%s", err, mcpTextOf(res))
		}
		ctx := vw6Ctx()
		want, err := store.SearchBlocks(ctx, pool, "", []string{"private"}, vw6Category, nil, 10, true,
			nil, resolveGrants(ctx, pool, AuthResultFromContext(ctx)), nil, nil, nil)
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

// TestMCPTypeFilterSearchRejectsInvisibleAndUnknown covers negative probes (a)
// and (b) at the RESULT level: neither answers rows, and neither answers an
// empty list — both are refusals.
func TestMCPTypeFilterSearchRejectsInvisibleAndUnknown_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	seedVW6Fixture(t, pool)
	cfg := vw6Cfg(t, pool)

	for name, raw := range map[string]string{
		"unknown_name":           `{"category":"` + vw6Category + `","types":["nicht-existent"]}`,
		"existing_but_invisible": `{"category":"` + vw6Category + `","types":["checkpoint"]}`,
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

// existenceOnlyCut is the WRONG implementation the gate has to be able to tell
// apart: it checks only that every requested name is REGISTERED and passes the
// list straight through — the shape recentInput's raw `type_name = ANY($n)`
// predicate has today.
func existenceOnlyCut(set *blocktype.Set, requested []string) ([]string, bool) {
	for _, n := range requested {
		if _, ok := set.Resolve(n); !ok {
			return nil, false
		}
	}
	return requested, true
}

// TestMCPTypeFilterSearchGateSelfProbe is negative probe (c): the same fixture,
// the same store call, the two candidate implementations side by side. The
// existence-only variant accepts `checkpoint` and RETURNS checkpoint rows —
// i.e. it would widen retrieval visibility for every key. The gate above is red
// against it, which is what makes the gate a real gate rather than a
// restatement of the implementation.
func TestMCPTypeFilterSearchGateSelfProbe_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	seedVW6Fixture(t, pool)
	cfg := vw6Cfg(t, pool)
	ctx := vw6Ctx()
	set := cfg.Blocktypes.SnapshotForRequest(ctx)

	// Variant 1 — existence only.
	cut, ok := existenceOnlyCut(set, []string{"checkpoint"})
	if !ok {
		t.Fatalf("existence-only variant rejected checkpoint — the fixture premise is wrong")
	}
	rows, err := store.SearchBlocks(ctx, pool, "", []string{"private"}, vw6Category, nil, 10, true,
		nil, nil, cut, nil, nil)
	if err != nil {
		t.Fatalf("existence-only search: %v", err)
	}
	naive := 0
	for _, r := range rows {
		if r.TypeName == "checkpoint" {
			naive++
		}
	}
	if naive == 0 {
		t.Fatalf("existence-only variant returned no checkpoint row — it would not have been distinguishable")
	}

	// Variant 2 — the shipped handler, same fixture, same argument.
	var in searchInput
	if err := json.Unmarshal([]byte(`{"category":"`+vw6Category+`","types":["checkpoint"]}`), &in); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res, _, err := mcpSearchHandler(cfg)(ctx, nil, in)
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if !res.IsError {
		t.Fatalf("shipped handler behaves like the existence-only variant: %s", mcpTextOf(res))
	}
	t.Logf("existence-only variant returned %d checkpoint row(s); shipped handler refuses: %s",
		naive, mcpTextOf(res))
}
