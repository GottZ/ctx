//go:build integration

// Issue #342, step (a): the MCP `search` tool's `types` names every type of the
// caller's registry snapshot, the retrieval-excluded ones (checkpoint,
// system-meta) included. The tool answers rows of those types without a type
// filter, so naming one only narrows an answer the key already gets; before
// this step it was refused ("not retrieval-visible"), and the clients that look
// a checkpoint up over MCP could not narrow their search to it.
//
// What stays: an unknown name is a refusal; a type another tenant registered
// for itself is answered exactly like a name nobody registered; the `query`
// tool keeps its retrieval-visible cut (TestMCPTypeFilterRejections).
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestMCPSearchTypes' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	i342Category = "i342-types"
	// i342OtherCategory holds one more checkpoint with the term, so a search that
	// names i342Category shows when it ignores the category.
	i342OtherCategory = "i342-other"
	// i342Term is in every fixture block, so the full-text path matches all of them.
	i342Term = "quarzkopf"
)

type i342Row struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Type     string `json:"type"`
}

// i342SearchCall runs the MCP search tool for ctx over a raw argument object.
// It returns every row of the answer, or the refusal text.
func i342SearchCall(t *testing.T, cfg MCPConfig, ctx context.Context, raw string) (rows []i342Row, refusal string) {
	t.Helper()
	var in searchInput
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("decode tool arguments %s: %v", raw, err)
	}
	res, _, err := mcpSearchHandler(cfg)(ctx, nil, in)
	if err != nil {
		t.Fatalf("search %s: transport error %v", raw, err)
	}
	if res.IsError {
		return nil, mcpTextOf(res)
	}
	if err := json.Unmarshal([]byte(mcpTextOf(res)), &rows); err != nil {
		t.Fatalf("decode search rows of %s: %v (%s)", raw, err, mcpTextOf(res))
	}
	return rows, ""
}

// i342InCategory fails the test when a row of an answer to a search that named
// category lies outside it.
func i342InCategory(t *testing.T, raw string, rows []i342Row, category string) {
	t.Helper()
	for _, r := range rows {
		if r.Category != category {
			t.Errorf("%s: row %s of category %q in the answer, want only %q", raw, r.ID, r.Category, category)
		}
	}
}

func i342TypesOf(rows []i342Row) map[string]int {
	out := map[string]int{}
	for _, r := range rows {
		out[r.Type]++
	}
	return out
}

// i342Seed writes one block per type into scope, all in the fixture category and
// all holding the full-text term. A plain INSERT like the V-W6 fixture
// (seedVW6Fixture): the type triggers run, and system-meta has no client write
// path.
func i342Seed(t *testing.T, pool *pgxpool.Pool, scope string, typeNames ...string) {
	t.Helper()
	i342SeedIn(t, pool, i342Category, scope, typeNames...)
}

func i342SeedIn(t *testing.T, pool *pgxpool.Pool, category, scope string, typeNames ...string) {
	t.Helper()
	for _, name := range typeNames {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_blocks (category, title, content, scope, type_name)
			 VALUES ($1, $2, $3, $4, $5)`,
			category, "i342 "+name, "i342 fixture "+i342Term+" of type "+name, scope, name); err != nil {
			t.Fatalf("seed %s in %s: %v", name, category, err)
		}
	}
}

func TestMCPSearchTypesNameNeverRankedTypes_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	i342Seed(t, pool, "private", "knowledge", "audit-trail", "checkpoint", "system-meta")
	i342SeedIn(t, pool, i342OtherCategory, "private", "checkpoint")
	cfg := vw6Cfg(t, pool)
	ctx := vw6Ctx()
	set := cfg.Blocktypes.SnapshotForRequest(ctx)

	// Premise: both are registered, and retrieval never ranks them.
	for _, name := range []string{"checkpoint", "system-meta"} {
		if _, ok := set.Resolve(name); !ok {
			t.Fatalf("fixture premise: %s is not registered", name)
		}
		if slices.Contains(set.VisibleTypes(), name) {
			t.Fatalf("fixture premise: %s is retrieval-visible", name)
		}
	}

	unfilteredRaw := `{"category":"` + i342Category + `"}`
	unfiltered, refusal := i342SearchCall(t, cfg, ctx, unfilteredRaw)
	if refusal != "" || len(unfiltered) != 4 {
		t.Fatalf("search without types: %d rows, refusal %q — want the four fixture rows", len(unfiltered), refusal)
	}
	i342InCategory(t, unfilteredRaw, unfiltered, i342Category)

	for _, tc := range []struct {
		name, raw, category string
		want                map[string]int
	}{
		{"checkpoint listing", `{"category":"` + i342Category + `","types":["checkpoint"]}`, i342Category, map[string]int{"checkpoint": 1}},
		{"system-meta listing", `{"category":"` + i342Category + `","types":["system-meta"]}`, i342Category, map[string]int{"system-meta": 1}},
		{"checkpoint full text", `{"query":"` + i342Term + `","types":["checkpoint"]}`, "", map[string]int{"checkpoint": 2}},
		{"checkpoint full text in category", `{"query":"` + i342Term + `","category":"` + i342Category + `","types":["checkpoint"]}`, i342Category, map[string]int{"checkpoint": 1}},
		{"never-ranked and ranked together", `{"category":"` + i342Category + `","types":["checkpoint","knowledge"]}`, i342Category, map[string]int{"checkpoint": 1, "knowledge": 1}},
		{"a name twice", `{"category":"` + i342Category + `","types":["checkpoint","checkpoint"]}`, i342Category, map[string]int{"checkpoint": 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, refusal := i342SearchCall(t, cfg, ctx, tc.raw)
			if refusal != "" {
				t.Fatalf("%s: refused (%s), want the rows of the named types", tc.raw, refusal)
			}
			if tc.category != "" {
				i342InCategory(t, tc.raw, rows, tc.category)
			}
			got := i342TypesOf(rows)
			if len(got) != len(tc.want) {
				t.Fatalf("%s: rows by type %v, want %v", tc.raw, got, tc.want)
			}
			for typ, n := range tc.want {
				if got[typ] != n {
					t.Errorf("%s: rows by type %v, want %v", tc.raw, got, tc.want)
				}
			}
		})
	}

	// No name of the registry widens the answer: the rows of types=[name] are
	// rows the same search without types answers, and only rows of that type.
	t.Run("no registered name widens the answer", func(t *testing.T) {
		ids := make([]string, 0, len(unfiltered))
		for _, r := range unfiltered {
			ids = append(ids, r.ID)
		}
		for _, name := range set.Names() {
			raw := `{"category":"` + i342Category + `","types":["` + name + `"]}`
			rows, refusal := i342SearchCall(t, cfg, ctx, raw)
			if refusal != "" {
				t.Errorf("types=[%s]: refused (%s), want a filtered answer", name, refusal)
				continue
			}
			i342InCategory(t, raw, rows, i342Category)
			for _, r := range rows {
				if r.Type != name || !slices.Contains(ids, r.ID) {
					t.Errorf("types=[%s] answered row %s of type %s, which the search without types does not answer as such", name, r.ID, r.Type)
				}
			}
		}
	})

	t.Run("an unknown name is still refused", func(t *testing.T) {
		rows, refusal := i342SearchCall(t, cfg, ctx, `{"category":"`+i342Category+`","types":["nicht-existent"]}`)
		if want := `unknown block type "nicht-existent"`; refusal != want {
			t.Fatalf("types=[nicht-existent]: refusal %q (rows %v), want %q", refusal, rows, want)
		}
		rows, refusal = i342SearchCall(t, cfg, ctx, `{"category":"`+i342Category+`","types":["checkpoint","nicht-existent"]}`)
		if want := `unknown block type "nicht-existent"`; refusal != want {
			t.Fatalf("types=[checkpoint,nicht-existent]: refusal %q (rows %v), want %q", refusal, rows, want)
		}
	})
}

// TestMCPSearchTypesForeignTenantTypeIsUnknown_Integration: the snapshot the
// tool checks a name against is the request's tenant-resolved one. A
// retrieval-excluded type tenant B registered for itself is filterable for B,
// and for tenant A it is answered byte for byte like a name nobody registered —
// the refusal says nothing about another tenant's types.
func TestMCPSearchTypesForeignTenantTypeIsUnknown_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	bg := context.Background()
	// The production request-scope hook (cmd/ctxd): without it no tenant's own
	// type is ever in a request snapshot.
	blocktype.SetRequestScopeHook(RequestTenantScope)
	t.Cleanup(func() { blocktype.SetRequestScopeHook(nil) })

	const own = "i342-b-transcript"
	if _, err := store.CreateBlockType(bg, pool, store.BlockTypeWrite{
		Name: own, Scope: "i342-b", Config: json.RawMessage(`{"v":1,"retrieval":{"policy":"excluded"}}`),
	}, nil, ""); err != nil {
		t.Fatalf("create tenant B's type: %v", err)
	}
	i342Seed(t, pool, "i342-b", own, "knowledge")
	i342Seed(t, pool, "i342-a", "knowledge")
	cfg := vw6Cfg(t, pool)

	member := func(scope string) context.Context {
		return context.WithValue(bg, authResultKey, &auth.AuthResult{
			IsValid: true, TenantRole: auth.RoleMember, HomeScope: scope, ReadScopes: []string{scope},
		})
	}
	ctxA, ctxB := member("i342-a"), member("i342-b")

	// Premise: B's type is in B's snapshot and not retrieval-visible there.
	setB := cfg.Blocktypes.SnapshotForRequest(ctxB)
	if p, ok := setB.Resolve(own); !ok || p.Retrieval.Kind != blocktype.RetrievalExcluded {
		t.Fatalf("fixture premise: tenant B's snapshot resolves %s = %v (ok %t), want retrieval excluded", own, p.Retrieval.Kind, ok)
	}
	if _, ok := cfg.Blocktypes.SnapshotForRequest(ctxA).Resolve(own); ok {
		t.Fatalf("fixture premise: tenant A's snapshot resolves tenant B's type %s", own)
	}

	t.Run("tenant B narrows to its own excluded type", func(t *testing.T) {
		raw := `{"category":"` + i342Category + `","types":["` + own + `"]}`
		rows, refusal := i342SearchCall(t, cfg, ctxB, raw)
		if refusal != "" {
			t.Fatalf("tenant B, types=[%s]: refused (%s)", own, refusal)
		}
		i342InCategory(t, raw, rows, i342Category)
		if got := i342TypesOf(rows); len(got) != 1 || got[own] != 1 {
			t.Fatalf("tenant B, types=[%s]: rows by type %v, want one row of %s", own, got, own)
		}
	})

	t.Run("tenant A gets the answer for an unregistered name", func(t *testing.T) {
		const nobody = "i342-unregistered"
		_, foreign := i342SearchCall(t, cfg, ctxA, `{"category":"`+i342Category+`","types":["`+own+`"]}`)
		_, unknown := i342SearchCall(t, cfg, ctxA, `{"category":"`+i342Category+`","types":["`+nobody+`"]}`)
		if foreign == "" || unknown == "" {
			t.Fatalf("tenant A: refusal for tenant B's type %q, for an unregistered name %q — want both refused", foreign, unknown)
		}
		if strings.ReplaceAll(foreign, own, "NAME") != strings.ReplaceAll(unknown, nobody, "NAME") {
			t.Fatalf("tenant A: the answer for tenant B's type (%q) differs from the answer for an unregistered name (%q)", foreign, unknown)
		}
	})
}
