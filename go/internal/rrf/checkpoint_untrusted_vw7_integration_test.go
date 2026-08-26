//go:build integration

// V-W7 non-regression (design/05 §7 row V-W7): setting retrieval.untrusted on
// checkpoint must leave RETRIEVAL untouched. The flag is a presentation
// property — handler/query.go reads it per source when it builds the synthesis
// prompt — and ctx_rrf never sees it: the function is fed VisibleTypes() and
// DampedTypesFor(), both derived from the retrieval POLICY, which V-W7 does not
// change.
//
// The probe is built so that the wave's plausible mis-implementation is the one
// thing it cannot survive: a migration that flips the type to `damped` while
// setting the flag would make 5 900 near-duplicate transcript parts retrievable
// corpus-wide. Phase 2 performs exactly that mutation against the live registry
// row and requires the result set to CHANGE — a probe that stayed identical
// under it would be measuring nothing.
//
//	go test -tags=integration ./internal/rrf/ -run TestVW7 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// The fixture: three knowledge blocks and three checkpoint blocks, one shared
// embedding, ids in uuidv7 order so the gen-17 tiebreak has a deterministic
// last word.
const (
	vw7Know1 = "019f2207-0000-7000-9000-00000000b001"
	vw7Know2 = "019f2207-0000-7000-9000-00000000b002"
	vw7Know3 = "019f2207-0000-7000-9000-00000000b003"
	vw7Chk1  = "019f2207-0000-7000-9000-00000000b101"
	vw7Chk2  = "019f2207-0000-7000-9000-00000000b102"
	vw7Chk3  = "019f2207-0000-7000-9000-00000000b103"
)

// vw7Query hits the title text of every fixture row through the lexical arms as
// well as the semantic one, so a checkpoint row that becomes visible cannot be
// missed for lack of relevance.
const vw7Query = "vw7 fixture retrieval probe"

// vw7Expected is the retrieval end state V-W7 must not move: the three
// knowledge blocks, in rank order, and nothing else. Written out rather than
// computed so that a change is visible in the diff of this file.
const vw7Expected = "1:019f2207-0000-7000-9000-00000000b001/knowledge " +
	"2:019f2207-0000-7000-9000-00000000b002/knowledge " +
	"3:019f2207-0000-7000-9000-00000000b003/knowledge"

func vw7Insert(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id, typeName, title string, emb []float32, ts time.Time) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
			(id, category, title, content, scope, embedding, created_at, updated_at, type_name)
		 VALUES ($1::uuid, $2, $3, $4, $5, $6, $7, $7, $8)`,
		id, "knowledge", title, "vw7 fixture retrieval probe content", "private",
		pgvec.NewVector(emb), ts, typeName); err != nil {
		t.Fatalf("insert vw7 block %s: %v", id, err)
	}
}

// vw7Render collapses a result list into one comparable line: rank, id and the
// registry type. Scores stay out on purpose — the claim under test is about
// WHICH blocks reach the caller and in WHICH order, and a float rendering would
// turn an unrelated fusion tweak into a red here.
func vw7Render(res []rrf.SearchResult) string {
	parts := make([]string, 0, len(res))
	for i, r := range res {
		parts = append(parts, fmt.Sprintf("%d:%s/%s", i+1, r.ID, r.TypeName))
	}
	return strings.Join(parts, " ")
}

func vw7Search(t *testing.T, ctx context.Context, pool *pgxpool.Pool, set *blocktype.Set, emb []float32) []rrf.SearchResult {
	t.Helper()
	damped, factors := set.DampedTypesFor(vw7Query)
	res, _, err := rrf.Search(ctx, pool, emb, vw7Query, vw7Query,
		[]string{"private"}, nil, nil, 20, "", "", set.VisibleTypes(), damped, factors,
		nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search: %v", err)
	}
	return res
}

func vw7Snapshot(t *testing.T, ctx context.Context, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	return reg.Snapshot()
}

// TestVW7CheckpointRetrievalUnchanged is the non-regression gate plus its
// negative probe.
func TestVW7CheckpointRetrievalUnchanged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	emb := t40bEmbedding()
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)

	vw7Insert(t, ctx, pool, vw7Know1, "knowledge", "vw7 fixture retrieval probe alpha", emb, now)
	vw7Insert(t, ctx, pool, vw7Know2, "knowledge", "vw7 fixture retrieval probe beta", emb, now)
	vw7Insert(t, ctx, pool, vw7Know3, "knowledge", "vw7 fixture retrieval probe gamma", emb, now)
	vw7Insert(t, ctx, pool, vw7Chk1, "checkpoint", "vw7 fixture retrieval probe part one", emb, now)
	vw7Insert(t, ctx, pool, vw7Chk2, "checkpoint", "vw7 fixture retrieval probe part two", emb, now)
	vw7Insert(t, ctx, pool, vw7Chk3, "checkpoint", "vw7 fixture retrieval probe part three", emb, now)

	set := vw7Snapshot(t, ctx, pool)
	got := vw7Render(vw7Search(t, ctx, pool, set, emb))
	t.Logf("phase 1 (checkpoint excluded, untrusted=%v): %s", set.IsUntrusted("checkpoint"), got)
	if got != vw7Expected {
		t.Errorf("ctx_rrf end state moved:\n  got:  %s\n  want: %s", got, vw7Expected)
	}

	// ── Phase 2: the negative probe ─────────────────────────────────────────
	// Flip the registry row to `damped` — the mis-implementation the wave brief
	// names — and require the very same call to answer differently.
	if _, err := pool.Exec(ctx,
		`UPDATE context_block_types
		    SET config = jsonb_set(
		                   jsonb_set(config, '{retrieval,policy}', '"damped"'::jsonb),
		                   '{retrieval,damping_factor}', '0.3'::jsonb)
		  WHERE name = 'checkpoint' AND scope = '_global'`); err != nil {
		t.Fatalf("sonde: flip checkpoint to damped: %v", err)
	}
	sonde := vw7Snapshot(t, ctx, pool)
	if !sonde.IsUntrusted("checkpoint") {
		t.Error("sonde lost the untrusted flag — jsonb_set on {retrieval,policy} must leave it alone")
	}
	sondeGot := vw7Render(vw7Search(t, ctx, pool, sonde, emb))
	t.Logf("phase 2 (checkpoint damped — the mis-implementation): %s", sondeGot)
	if sondeGot == vw7Expected {
		t.Fatalf("the damped variant delivers the identical result set %q — this probe cannot "+
			"tell a policy change from a flag change and proves nothing", sondeGot)
	}
	var sawCheckpoint bool
	for _, id := range []string{vw7Chk1, vw7Chk2, vw7Chk3} {
		if strings.Contains(sondeGot, id) {
			sawCheckpoint = true
		}
	}
	if !sawCheckpoint {
		t.Fatalf("the damped variant surfaced no checkpoint block at all (%q) — the sonde is not "+
			"exercising the failure it claims to", sondeGot)
	}
}
