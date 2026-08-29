package armsweep_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// Wave C4-3a — the G-GLOB pool (design/05a §C3-2-D05-8 k, §C3-4b).
//
// Befund N1 (reports/bau/x-w1a.md, fortgeschrieben in reports/bau/c2-6.md:223-230):
// buildPool returned an empty PoolEntry for every slice except G-REAL, so the
// priming run wrote a pool file that carried 150 G-REAL cases and nothing else —
// measured on the standing artefact pool-xw1-v580.jsonl, whose prime stamp names
// all seven slices. Without arm heads for G-GLOB no judgement template can be
// built for it, and B-Substanz stays structurally "nicht entschieden".
//
// The tests below drive the PRODUCTION path — Runner.Prime against an httptest
// instance — rather than calling the pool builder with a hand-made response, so
// what they pin is what a real priming run writes.

// ---------------------------------------------------------------- fixture.

// c43Rows is the arm result one fixture query produces. Twenty-four candidates
// b01..b24 carry semantic ranks 1..24, so the semantic head has to be CUT at
// PoolDepth; the four ids beyond the cut re-enter the union through the other
// three arms, which is the property a union-of-solo-heads pool exists for.
//
// b20 and b21 share fts_en rank 1: a tie is broken by id, never by row order,
// and a pool whose bytes depended on row order would not be regenerable.
func c43Rows() []rrf.ArmRow {
	rank := func(n int) *int { return &n }
	rows := make([]rrf.ArmRow, 0, 24)
	for i := 1; i <= 24; i++ {
		rows = append(rows, rrf.ArmRow{
			ID: c43ID(i), RankSemantic: rank(i), MassFactor: 1, TypeFactor: 1,
		})
	}
	byID := map[string]func(*rrf.ArmRow){
		c43ID(6):  func(r *rrf.ArmRow) { r.RankFTSDe = rank(1) },
		c43ID(5):  func(r *rrf.ArmRow) { r.RankFTSDe = rank(2) },
		c43ID(7):  func(r *rrf.ArmRow) { r.RankFTSDe = rank(3) },
		c43ID(20): func(r *rrf.ArmRow) { r.RankFTSEn = rank(1) },
		c43ID(21): func(r *rrf.ArmRow) { r.RankFTSEn = rank(1) },
		c43ID(24): func(r *rrf.ArmRow) { r.RankTrigram = rank(1) },
		c43ID(23): func(r *rrf.ArmRow) { r.RankTrigram = rank(2) },
		c43ID(22): func(r *rrf.ArmRow) { r.RankTrigram = rank(3) },
	}
	for i := range rows {
		if fn, ok := byID[rows[i].ID]; ok {
			fn(&rows[i])
		}
	}
	return rows
}

func c43ID(n int) string { return "b" + string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// c43WantSemantic is the semantic head: ranks 1..20, the four worse candidates
// dropped at the cap.
func c43WantSemantic() []string {
	out := make([]string, 0, armsweep.PoolDepth)
	for i := 1; i <= armsweep.PoolDepth; i++ {
		out = append(out, c43ID(i))
	}
	return out
}

var (
	c43WantFTSDe   = []string{"b06", "b05", "b07"}
	c43WantFTSEn   = []string{"b20", "b21"}
	c43WantTrigram = []string{"b24", "b23", "b22"}
)

// c43Server serves /api/query with the fixture arm rows. reverse flips the row
// order the instance reports, which a rank-sorted pool must not notice.
func c43Server(t *testing.T, reverse bool) *httptest.Server {
	t.Helper()
	rows := c43Rows()
	if reverse {
		for i, j := 0, len(rows)-1; i < j; i, j = i+1, j-1 {
			rows[i], rows[j] = rows[j], rows[i]
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/query" {
			t.Errorf("unexpected path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"sources": []any{},
			"arm_ranks": map[string]any{
				"rows": rows, "fusion_order": []string{},
				"effective_query": "q", "embed_model": "c43-fixture",
				"selector": map[string]any{"mode": "ann", "reason": "fixture"},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func c43Runner(t *testing.T, reverse bool) *armsweep.Runner {
	t.Helper()
	return &armsweep.Runner{
		Client: armsweep.NewClient(c43Server(t, reverse).URL, "k", 5*time.Second),
		RunID:  "c43-fixture",
	}
}

// c43Case builds one gold case of the named slice.
func c43Case(slice string, index int) goldset.Case {
	q := slice + "-frage-" + string(rune('a'+index))
	return goldset.Case{Slice: slice, Index: index, Query: q, QuerySHA256: goldset.SHA256Hex(q)}
}

func c43Prime(t *testing.T, r *armsweep.Runner, cases []goldset.Case) []armsweep.PoolEntry {
	t.Helper()
	_, pools, _, err := r.Prime(context.Background(), cases)
	if err != nil {
		t.Fatalf("Prime: %v", err)
	}
	return pools
}

// -------------------------------------------------------------- gate (rot).

// TestPrimeGivesTheGlobSliceAPool is the N1 gate: a priming run over G-GLOB
// must produce one pool entry per case, with all four arm heads filled and the
// per-arm head cut at PoolDepth.
//
// RED before C4-3a: buildPool answered every slice except G-REAL with an empty
// PoolEntry, sweep dropped it (`out[i].pool.Slice != ""`), and Prime returned
// no pool at all — "pool entries = 0, want 80".
func TestPrimeGivesTheGlobSliceAPool(t *testing.T) {
	t.Parallel()
	cases := []goldset.Case{c43Case(goldset.SliceGlob, 0), c43Case(goldset.SliceGlob, 1)}
	pools := c43Prime(t, c43Runner(t, false), cases)

	if len(pools) != len(cases) {
		t.Fatalf("pool entries = %d, want %d — G-GLOB has no pool (N1)", len(pools), len(cases))
	}
	for i, got := range pools {
		if got.Slice != goldset.SliceGlob {
			t.Errorf("entry %d: slice = %q, want %q", i, got.Slice, goldset.SliceGlob)
		}
		if got.Key() != cases[i].Key() {
			t.Errorf("entry %d: key = %q, want %q", i, got.Key(), cases[i].Key())
		}
		c43WantHeads(t, got)
	}
}

// c43WantHeads asserts the four arm heads of one entry.
func c43WantHeads(t *testing.T, got armsweep.PoolEntry) {
	t.Helper()
	for _, tc := range []struct {
		arm  string
		got  []string
		want []string
	}{
		{"semantic", got.Semantic, c43WantSemantic()},
		{"fts_de", got.FTSDe, c43WantFTSDe},
		{"fts_en", got.FTSEn, c43WantFTSEn},
		{"trigram", got.Trigram, c43WantTrigram},
	} {
		if len(tc.got) != len(tc.want) {
			t.Errorf("%s head = %v, want %v", tc.arm, tc.got, tc.want)
			continue
		}
		for i := range tc.want {
			if tc.got[i] != tc.want[i] {
				t.Errorf("%s head = %v, want %v", tc.arm, tc.got, tc.want)
				break
			}
		}
	}
}

// TestGlobPoolIsBuiltLikeTheRealPool pins the SEMANTICS decision of the wave:
// G-GLOB is pooled by the same construction as G-REAL — top-PoolDepth per arm
// by that arm's own rank — not by a second, G-GLOB-specific rule. Two slices
// judged from two differently built pools would not be comparable, and the κ,
// π and HT figures of C3-4a and C3-4b are meant to be read side by side.
func TestGlobPoolIsBuiltLikeTheRealPool(t *testing.T) {
	t.Parallel()
	r := c43Runner(t, false)
	real0 := c43Prime(t, r, []goldset.Case{c43Case(goldset.SliceReal, 0)})
	glob0 := c43Prime(t, r, []goldset.Case{c43Case(goldset.SliceGlob, 0)})
	if len(real0) != 1 || len(glob0) != 1 {
		t.Fatalf("pool entries: G-REAL=%d G-GLOB=%d, want 1 each", len(real0), len(glob0))
	}
	// Everything but the case identity must be equal.
	a, b := real0[0], glob0[0]
	a.Slice, a.Index, a.QuerySHA256 = "", 0, ""
	b.Slice, b.Index, b.QuerySHA256 = "", 0, ""
	if !c43SameEntry(a, b) {
		t.Errorf("G-GLOB pool differs from G-REAL pool over identical arm rows:\n G-REAL %+v\n G-GLOB %+v", a, b)
	}
}

// TestPrimeLeavesTheUnpooledSlicesEmpty is the NON-REGRESSION gate and the
// falsification probe of this wave: the change is scoped to G-GLOB, and a pool
// widened to every slice makes this test red.
//
// The five slices below carry CONSTRUCTIVE gold — paraphrased titles, generated
// questions, session windows, dream bridges, cluster members. Pooling them
// would write candidate heads nobody judges.
func TestPrimeLeavesTheUnpooledSlicesEmpty(t *testing.T) {
	t.Parallel()
	unpooled := []string{
		goldset.SliceKI, goldset.SliceQ, goldset.SliceSess,
		goldset.SliceMH, goldset.SliceGlobKonstr,
	}
	var cases []goldset.Case
	for i, s := range unpooled {
		cases = append(cases, c43Case(s, i))
	}
	if pools := c43Prime(t, c43Runner(t, false), cases); len(pools) != 0 {
		t.Errorf("pool entries = %d, want 0 — only G-REAL and G-GLOB are pooled, got %+v", len(pools), pools)
	}
}

// TestGlobPoolRegeneratesByteIdentically is the determinism gate. The C3-4a
// lesson was that a timestamp inside a draw key broke reproducibility; a pool
// entry carries no clock at all, and the head order is decided by (rank, id) —
// so a second priming run over the same instance answer must write the same
// bytes even when the instance reports its rows in a different order.
func TestGlobPoolRegeneratesByteIdentically(t *testing.T) {
	t.Parallel()
	cases := []goldset.Case{
		c43Case(goldset.SliceGlob, 0), c43Case(goldset.SliceGlob, 1), c43Case(goldset.SliceGlob, 2),
	}
	first := c43WritePool(t, "first.jsonl", c43Prime(t, c43Runner(t, false), cases))
	second := c43WritePool(t, "second.jsonl", c43Prime(t, c43Runner(t, true), cases))
	if string(first) != string(second) {
		t.Errorf("pool bytes differ between two runs:\n--- first\n%s\n--- second\n%s", first, second)
	}
	if len(first) == 0 {
		t.Fatal("pool file is empty — nothing was pooled for G-GLOB")
	}
}

// c43WritePool persists a pool through the production writer and returns the
// bytes it wrote.
func c43WritePool(t *testing.T, name string, entries []armsweep.PoolEntry) []byte {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := armsweep.WritePool(p, entries); err != nil {
		t.Fatalf("WritePool: %v", err)
	}
	b, err := os.ReadFile(p) //nolint:gosec // G304: path built from t.TempDir()
	if err != nil {
		t.Fatalf("read pool: %v", err)
	}
	return b
}

// c43SameEntry compares two pool entries field by field.
func c43SameEntry(a, b armsweep.PoolEntry) bool {
	ab, err := json.Marshal(a)
	if err != nil {
		return false
	}
	bb, err := json.Marshal(b)
	if err != nil {
		return false
	}
	return string(ab) == string(bb)
}
