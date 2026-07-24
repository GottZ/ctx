//go:build integration

// W02-2 gate G3 (design/02-strategy-selektor.md §7 "W02-2", §4.3a, §4.6)
// against a real PG18 testcontainer: rrf.Search with an ENABLED policy on a
// small scope decides exact, and the delivered result set equals a brute-force
// reference SELECT that carries the identical visibility predicate and the
// same cosine distance — computed WITHOUT ctx_rrf.
//
// The complement pins the W02-2 staffelung against a live DB: a scope larger
// than the (clamped) ExactMax falls through the probe stage into the pg_stats
// STUB and therefore lands on stats_stale → plain ann (W02-4 implements the
// stage).
//
//	go test -tags=integration ./internal/rrf/ -run TestSelectorW022 -count=1 -v
package rrf_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// w022ReferenceSQL is the brute-force ground truth of the exact arm: the
// Gen-15 exact-arm predicate block (migration 112, `exact_pool`) written out
// directly against context_blocks, ordered by cosine distance with the same
// (dist, id) tiebreak — no ctx_rrf, no HNSW index, no RRF fusion.
const w022ReferenceSQL = `
	SELECT cb.id::text
	FROM context_blocks cb
	WHERE cb.embedding IS NOT NULL
	  AND NOT cb.is_archived
	  AND cb.type_name = ANY($2::text[])
	  AND cb.scope = ANY($3::text[])
	ORDER BY cb.embedding::halfvec(1024) <=> $1, cb.id
	LIMIT 75`

func w022Reference(t *testing.T, pool *pgxpool.Pool, emb []float32, visible, scopes []string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), w022ReferenceSQL,
		pgvec.NewHalfVector(emb), visible, scopes)
	if err != nil {
		t.Fatalf("reference SQL: %v", err)
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("reference scan: %v", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("reference rows: %v", err)
	}
	return ids
}

func w022IDs(res []rrf.SearchResult) []string {
	ids := make([]string, len(res))
	for i, r := range res {
		ids[i] = r.ID
	}
	return ids
}

func w022Set(ids []string) map[string]bool {
	s := make(map[string]bool, len(ids))
	for _, id := range ids {
		s[id] = true
	}
	return s
}

// TestSelectorW022_G3_ExactOnSmallScope: enabled policy + small scope →
// Decision{exact, probe<=exact_max, n} and result set == brute-force reference.
func TestSelectorW022_G3_ExactOnSmallScope(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)
	emb := w021Query()

	const (
		scope   = "w022s"
		foreign = "w022f"
	)
	// Five visible, embedded blocks — the expected result set.
	visibleIDs := make([]string, 5)
	for i := range visibleIDs {
		visibleIDs[i] = fmt.Sprintf("019f9101-0000-7000-9000-0000000010%02d", i+1)
		w021Insert(t, pool, visibleIDs[i], scope, "knowledge", false, w021Embedding(i), now)
	}
	const (
		idRogue   = "019f9101-0000-7000-9000-000000001101" // in scope, rogue type
		idNull    = "019f9101-0000-7000-9000-000000001102" // in scope, NULL embedding
		idArch    = "019f9101-0000-7000-9000-000000001103" // in scope, archived
		idForeign = "019f9101-0000-7000-9000-000000001104" // foreign scope
	)
	w021Insert(t, pool, idRogue, scope, "rogue", false, w021Embedding(5), now)
	w021Insert(t, pool, idNull, scope, "knowledge", false, nil, now)
	w021Insert(t, pool, idArch, scope, "knowledge", true, w021Embedding(6), now)
	w021Insert(t, pool, idForeign, foreign, "knowledge", false, w021Embedding(7), now)

	// The probe counts scope × NOT archived — type and embedding filters live
	// in the arm, not in the probe (§4.3a: an upper bound, deliberately).
	const wantEstimate = 7 // 5 visible + rogue + NULL-embedding; archived and foreign excluded

	policy := rrf.SelectorPolicy{
		Enabled:        true,
		ExactMax:       4096,
		GreyMax:        65536,
		GreyScanTuples: 60000,
		StatsTTL:       60 * time.Second,
	}
	res, dec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{scope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, policy)
	if err != nil {
		t.Fatalf("rrf.Search (enabled policy): %v", err)
	}

	if dec.Mode != rrf.ModeExact || dec.Reason != rrf.ReasonProbeExact {
		t.Errorf("decision = {%q, %q}, want {exact, probe<=exact_max}", dec.Mode, dec.Reason)
	}
	if dec.Estimate != wantEstimate {
		t.Errorf("estimate = %d, want %d (scope blocks that are not archived)", dec.Estimate, wantEstimate)
	}
	if dec.ProbeMs <= 0 {
		t.Errorf("probe_ms = %v, want > 0 for a real roundtrip", dec.ProbeMs)
	}

	// Ground truth: identical visibility predicate + cosine distance, no ctx_rrf.
	want := w022Reference(t, pool, emb, testVisibleTypes, []string{scope})
	got := w022IDs(res)
	if len(want) != len(visibleIDs) {
		t.Fatalf("reference returned %d ids (%v), want the %d visible fixtures", len(want), want, len(visibleIDs))
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("exact result != brute-force reference\n  got  = %v\n  want = %v", got, want)
	} else {
		t.Logf("G3 GREEN: exact arm == brute-force reference (%d rows, identical order): %v", len(got), got)
	}

	// Declared Semantik-Delta 2 (§4.5): the exact arm never surfaces the
	// NULL-embedding block; the Ist/ann path (zero policy) does on this corpus.
	istRes, istDec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{scope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("rrf.Search (zero policy): %v", err)
	}
	if istDec.Mode != rrf.ModeANN || istDec.Reason != rrf.ReasonDisabled || istDec.ProbeMs != 0 {
		t.Errorf("zero-policy decision = %+v, want {ann, disabled, probe_ms 0}", istDec)
	}
	istSet, exactSet := w022Set(w022IDs(istRes)), w022Set(got)
	if !istSet[idNull] {
		t.Error("ann path did not surface the NULL-embedding block (declared Gen-14 seq-scan behaviour)")
	}
	if exactSet[idNull] {
		t.Error("exact path surfaced the NULL-embedding block — embedding filter missing")
	}
	delete(istSet, idNull)
	if len(istSet) != len(exactSet) {
		t.Fatalf("parity after the declared delta: ann=%v exact=%v", istSet, exactSet)
	}
	for id := range exactSet {
		if !istSet[id] {
			t.Errorf("parity: %s present in exact, missing in ann", id)
		}
	}
	for _, leaked := range []string{idRogue, idArch, idForeign} {
		if exactSet[leaked] || istSet[leaked] {
			t.Errorf("LEAK: block %s visible through the Go path", leaked)
		}
	}
}

// TestSelectorW022_G3_StubStaleOnLargeScope is the complement: a scope beyond
// the clamped ExactMax passes the probe stage and hits the W02-2 pg_stats STUB
// → stats_stale → plain ann. Estimate carries the capped probe count
// (ExactMax+1), proving the probe is LIMIT-capped rather than a full count.
func TestSelectorW022_G3_StubStaleOnLargeScope(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 25, 11, 0, 0, 0, time.UTC)
	emb := w021Query()

	// ExactMax floor is 64 (§5.4 clamp), so the fixture scope needs > 64 rows.
	const scope = "w022big"
	const n = 70
	for i := 0; i < n; i++ {
		w021Insert(t, pool, fmt.Sprintf("019f9102-0000-7000-9000-0000000020%02d", i+1),
			scope, "knowledge", false, w021Embedding(i%20), now)
	}

	// ExactMax=1 is clamped up to the floor 64 — below the 70 fixtures.
	policy := rrf.SelectorPolicy{Enabled: true, ExactMax: 1, GreyMax: 65536, GreyScanTuples: 10, StatsTTL: time.Minute}
	res, dec, err := rrf.Search(ctx, pool, emb, "zzqqxx", "zzqqxx",
		[]string{scope}, nil, nil, 50, "", "", testVisibleTypes, nil, nil, nil, nil, nil, policy)
	if err != nil {
		t.Fatalf("rrf.Search (large scope): %v", err)
	}
	if dec.Mode != rrf.ModeANN || dec.Reason != rrf.ReasonStatsStale {
		t.Errorf("decision = {%q, %q}, want {ann, stats_stale} (W02-2 stub)", dec.Mode, dec.Reason)
	}
	if dec.Estimate != 65 {
		t.Errorf("estimate = %d, want 65 (clamped ExactMax 64 + 1 — the probe is LIMIT-capped, not a full count of %d)", dec.Estimate, n)
	}
	if len(res) == 0 {
		t.Error("degraded ann path returned no rows — the query must never be lost")
	}
	t.Logf("G3 complement: %d fixtures → decision %+v, %d rows via the Ist path", n, dec, len(res))
}
