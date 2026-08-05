// Unit gates of the Substanz-Kern (Amendment A01-7 / decision E4-01). DB-free,
// so they run under -short like the computeClustering gates next door.
package overview

import (
	"math/rand"
	"reflect"
	"testing"
)

// degOf builds an intraDegree map from parallel slices.
func degOf(ids []string, ds []float64) map[string]float64 {
	m := make(map[string]float64, len(ids))
	for i, id := range ids {
		m[id] = ds[i]
	}
	return m
}

// TestCoreOf_HalfOfTheSubstance is the positive gate: the core is the smallest
// prefix carrying half of the internal substance, no more and no less.
func TestCoreOf_HalfOfTheSubstance(t *testing.T) {
	cases := []struct {
		name string
		ids  []string
		degs []float64
		want []string
	}{
		{
			// Flat cluster of equals: half the members carry half the weight.
			name: "flat cluster of four equals → two",
			ids:  []string{"a", "b", "c", "d"},
			degs: []float64{1, 1, 1, 1},
			want: []string{"a", "b"},
		},
		{
			// Hub cluster: one block holds most of the internal weight.
			name: "hub cluster → the hub alone",
			ids:  []string{"a", "b", "c", "d"},
			degs: []float64{10, 1, 1, 1},
			want: []string{"a"},
		},
		{
			// Zero substance (singleton / edge-free): never an empty core.
			name: "zero substance → one member, never empty",
			ids:  []string{"b", "a"},
			degs: []float64{0, 0},
			want: []string{"a"}, // uuid tiebreak decides
		},
		{
			name: "single member",
			ids:  []string{"a"},
			degs: []float64{3},
			want: []string{"a"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, hash := coreOf(tc.ids, degOf(tc.ids, tc.degs))
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("core = %v, want %v", got, tc.want)
			}
			if len(hash) != 64 {
				t.Fatalf("hash = %q, want 64 hex chars (sha256)", hash)
			}
		})
	}
}

// TestCoreOf_HeavyTailIsAdaptive is the A01-7 negative probe in its executable
// form: the SAME core rule must produce DIFFERENT core sizes for a hub cluster
// and a flat cluster of the same member count. That is the property a K
// constant cannot have — with `LIMIT K` (the retired topic_core_size) both
// sides return K members and the assertion below fails, which is exactly the
// red probe the amendment demands.
func TestCoreOf_HeavyTailIsAdaptive(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g", "h"}
	hub := []float64{100, 1, 1, 1, 1, 1, 1, 1}
	flat := []float64{1, 1, 1, 1, 1, 1, 1, 1}

	hubCore, _ := coreOf(ids, degOf(ids, hub))
	flatCore, _ := coreOf(ids, degOf(ids, flat))

	if len(hubCore) != 1 {
		t.Fatalf("hub core = %v (%d), want the single hub", hubCore, len(hubCore))
	}
	if len(flatCore) != 4 {
		t.Fatalf("flat core = %v (%d), want half of eight equals", flatCore, len(flatCore))
	}
	if len(hubCore) == len(flatCore) {
		t.Fatal("hub and flat cluster produced the same core size — this is the LIMIT-K behaviour A01-7 replaced")
	}
}

// TestCoreOf_HashIgnoresPureReranking pins the hash contract: the hash is taken
// over the core sorted by ID, so two members swapping degree RANK inside an
// unchanged core must not move it. Without that, every rebuild would re-label
// topics whose membership never changed.
func TestCoreOf_HashIgnoresPureReranking(t *testing.T) {
	ids := []string{"a", "b", "c", "d"}
	coreA, hashA := coreOf(ids, degOf(ids, []float64{5, 4, 1, 1}))
	coreB, hashB := coreOf(ids, degOf(ids, []float64{4, 5, 1, 1}))

	if !reflect.DeepEqual(coreA, coreB) {
		t.Fatalf("core membership changed under a pure re-ranking: %v vs %v", coreA, coreB)
	}
	if hashA != hashB {
		t.Fatalf("core_hash changed under a pure re-ranking: %s vs %s", hashA, hashB)
	}

	// Control: a real membership change MUST move the hash — otherwise the
	// drift anchor would be blind and the label could never be refreshed.
	_, hashC := coreOf([]string{"a", "b", "c", "z"}, degOf([]string{"a", "b", "c", "z"}, []float64{5, 4, 1, 9}))
	if hashC == hashA {
		t.Fatal("core_hash unchanged although the core membership changed — drift anchor is blind")
	}
}

// TestCoreOf_DeterministicUnderPermutation is the reproducibility gate: the
// input order of the member slice comes out of a Go map in persist and is
// therefore randomized per run. Core and hash must not notice.
func TestCoreOf_DeterministicUnderPermutation(t *testing.T) {
	ids := []string{"a", "b", "c", "d", "e", "f", "g"}
	degs := degOf(ids, []float64{3, 3, 2, 2, 1, 1, 1})
	wantCore, wantHash := coreOf(ids, degs)

	rng := rand.New(rand.NewSource(1)) //nolint:gosec // test shuffling, not crypto
	for i := 0; i < 50; i++ {
		perm := make([]string, len(ids))
		copy(perm, ids)
		rng.Shuffle(len(perm), func(a, b int) { perm[a], perm[b] = perm[b], perm[a] })
		gotCore, gotHash := coreOf(perm, degs)
		if !reflect.DeepEqual(gotCore, wantCore) || gotHash != wantHash {
			t.Fatalf("permutation %d changed the core: %v/%s vs %v/%s", i, gotCore, gotHash, wantCore, wantHash)
		}
	}
}

// TestBuildCores_ScopePure is the unit half of W3-G3: the grouping key is
// (cluster, SCOPE), so no core can mix scopes even when one community spans
// two of them. The red probe sits in the same test — a core built over the
// cluster ALONE does mix, which is what the scope-keyed grouping prevents.
func TestBuildCores_ScopePure(t *testing.T) {
	const cluster = "c1"
	assign := map[string]string{"a1": cluster, "a2": cluster, "b1": cluster, "b2": cluster}
	scopes := map[string]string{"a1": "alpha", "a2": "alpha", "b1": "beta", "b2": "beta"}
	deg := map[string]float64{"a1": 5, "a2": 1, "b1": 5, "b2": 1}

	cores := buildCores(clustering{blockToCluster: assign, intraDegree: deg}, scopes)
	if cores.len() != 2 {
		t.Fatalf("scope-crossing cluster produced %d core rows, want 2 (one per scope)", cores.len())
	}
	for i := range cores.clusters {
		want := "{a1}"
		if cores.scopes[i] == "beta" {
			want = "{b1}"
		}
		if cores.blocks[i] != want {
			t.Fatalf("core of (%s,%s) = %s, want %s — a core must never mix scopes (I2)",
				cores.clusters[i], cores.scopes[i], cores.blocks[i], want)
		}
	}

	// Red probe: the same members grouped by cluster_id ALONE.
	all := []string{"a1", "a2", "b1", "b2"}
	mixed, _ := coreOf(all, deg)
	sawAlpha, sawBeta := false, false
	for _, id := range mixed {
		if id[0] == 'a' {
			sawAlpha = true
		} else {
			sawBeta = true
		}
	}
	if !sawAlpha || !sawBeta {
		t.Fatalf("red probe did not reproduce the mixed core (%v) — the gate would not prove anything", mixed)
	}
}
