// B-W2 unit gates for the measurement seam (design/04 §4.4). No build tag:
// these run in the short loop, the DB-backed halves live in
// arms_seam_bw2_integration_test.go.
package rrf_test

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v4"

	"github.com/GottZ/ctx/internal/rrf"
)

// bw2ArmCols is the ctx_rrf_arms projection, in order.
var bw2ArmCols = []string{"id", "rank_semantic", "rank_fts_de", "rank_fts_en",
	"rank_trigram", "cos_sim", "mass_factor", "type_factor"}

// bw2MockArgs is the $1..$15 matcher prefix; the three selector positions are
// spelled out by each test.
func bw2MockArgs(tail ...any) []any {
	out := make([]any, 0, 15+len(tail))
	for i := 0; i < 15; i++ {
		out = append(out, pgxmock.AnyArg())
	}
	return append(out, tail...)
}

// bw2CallArms invokes ArmRanksTx over a mock with a fixed argument surface.
func bw2CallArms(ctx context.Context, q rrf.Querier, dec rrf.SelectorDecision, policy rrf.SelectorPolicy) ([]rrf.ArmRow, error) {
	return rrf.ArmRanksTx(ctx, q, dec, policy, []float32{0.1, 0.2}, "q", "q",
		[]string{"private"}, nil, nil, 20, "", "", []string{"knowledge"}, nil, nil, nil, nil, nil)
}

// TestArmRanksTxScan pins the ArmRow scan, NULL ranks included. A nil rank is
// the load-bearing case: it means "this candidate is not in this arm", and a
// scan that flattened it to 0 would invent a best-possible rank out of an
// absence — the one mis-scan an offline weight sweep could not detect.
//
// RED before B-W2: undefined: rrf.ArmRanksTx / rrf.ArmRow / rrf.Querier.
func TestArmRanksTxScan(t *testing.T) {
	mock, err := pgxmock.NewPool()
	if err != nil {
		t.Fatalf("pgxmock.NewPool: %v", err)
	}
	defer mock.Close()

	rows := mock.NewRows(bw2ArmCols).
		AddRow("11111111-1111-7000-9000-000000000001", ptrInt(1), ptrInt(3), nil, nil, bw2Float(0.87), 1.0, 0.3).
		AddRow("11111111-1111-7000-9000-000000000002", nil, nil, ptrInt(2), ptrInt(7), nil, 0.5, 1.0)
	mock.ExpectQuery(`FROM ctx_rrf_arms\(`).
		WithArgs(bw2MockArgs(rrf.ModeANN, nil, nil)...).
		WillReturnRows(rows)

	got, err := bw2CallArms(context.Background(), mock, rrf.SelectorDecision{Mode: rrf.ModeANN}, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("ArmRanksTx: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("rows: %d, want 2", len(got))
	}
	if got[0].RankSemantic == nil || *got[0].RankSemantic != 1 {
		t.Errorf("row 0 rank_semantic = %v, want 1", got[0].RankSemantic)
	}
	if got[0].RankFTSEn != nil || got[0].RankTrigram != nil {
		t.Errorf("row 0: absent arms must stay nil, got fts_en=%v trigram=%v", got[0].RankFTSEn, got[0].RankTrigram)
	}
	if got[0].CosSim == nil || *got[0].CosSim != 0.87 {
		t.Errorf("row 0 cos_sim = %v, want 0.87", got[0].CosSim)
	}
	if got[0].TypeFactor != 0.3 || got[1].MassFactor != 0.5 {
		t.Errorf("factors mis-scanned: type=%v mass=%v", got[0].TypeFactor, got[1].MassFactor)
	}
	// A lexical-only candidate: no cosine, and that is data, not an error.
	if got[1].CosSim != nil {
		t.Errorf("row 1 cos_sim = %v, want nil", got[1].CosSim)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Errorf("unmet expectations: %v", err)
	}
}

// TestArmRanksTxUsesTheExecutedSelectorArguments pins the point of the whole
// seam: ctx_rrf_arms is called with the arguments the fusion ACTUALLY ran
// with. The exact_cap_hit case is the one that matters — runSelected degrades
// the decision to ann after the in-body cap guard fires, and a measurement
// that replayed the ORIGINAL exact request would describe a candidate space
// the live call abandoned.
func TestArmRanksTxUsesTheExecutedSelectorArguments(t *testing.T) {
	policy := rrf.SelectorPolicy{Enabled: true, ExactMax: 4096, GreyMax: 65536, GreyScanTuples: 60000}

	cases := []struct {
		name string
		dec  rrf.SelectorDecision
		tail []any
	}{
		{"exact", rrf.SelectorDecision{Mode: rrf.ModeExact, Reason: rrf.ReasonProbeExact}, []any{rrf.ModeExact, nil, 4096}},
		{"grey", rrf.SelectorDecision{Mode: rrf.ModeGrey, Reason: rrf.ReasonStatsGrey}, []any{rrf.ModeANN, 60000, nil}},
		{"ann", rrf.SelectorDecision{Mode: rrf.ModeANN, Reason: rrf.ReasonStatsLarge}, []any{rrf.ModeANN, nil, nil}},
		// Post-retry: Mode was rewritten to ann by runSelected.
		{"exact_cap_hit", rrf.SelectorDecision{Mode: rrf.ModeANN, Reason: rrf.ReasonExactCapHit}, []any{rrf.ModeANN, nil, nil}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mock, err := pgxmock.NewPool()
			if err != nil {
				t.Fatalf("pgxmock.NewPool: %v", err)
			}
			defer mock.Close()

			mock.ExpectQuery(`FROM ctx_rrf_arms\(`).
				WithArgs(bw2MockArgs(tc.tail...)...).
				WillReturnRows(mock.NewRows(bw2ArmCols))

			if _, err := bw2CallArms(context.Background(), mock, tc.dec, policy); err != nil {
				t.Fatalf("ArmRanksTx: %v", err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Errorf("selector arguments: %v", err)
			}
		})
	}
}

// TestSearchDelegatesToSearchTx pins the delegation at the only place a unit
// test can observe it without a database: the fail-closed core. Search does no
// work of its own before handing over — so for every rejected input it must
// produce the SAME error as SearchTx, and it must do so without touching the
// pool at all (hence the nil pool: a Search that dereferenced it before
// delegating would panic here instead of returning).
func TestSearchDelegatesToSearchTx(t *testing.T) {
	ctx := context.Background()
	types := []string{"knowledge"}

	cases := []struct {
		name    string
		emb     []float32
		scopes  []string
		visible []string
	}{
		{"empty embedding", nil, []string{"private"}, types},
		{"empty scopes", []float32{0.1}, nil, types},
		{"empty visible types", []float32{0.1}, []string{"private"}, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, decA, errA := rrf.Search(ctx, nil, tc.emb, "q", "q", tc.scopes, nil, nil, 5, "", "",
				tc.visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
			_, decB, errB := rrf.SearchTx(ctx, nil, nil, tc.emb, "q", "q", tc.scopes, nil, nil, 5, "", "",
				tc.visible, nil, nil, nil, nil, nil, rrf.SelectorPolicy{})
			if errA == nil || errB == nil {
				t.Fatalf("expected rejection, got Search=%v SearchTx=%v", errA, errB)
			}
			if errA.Error() != errB.Error() {
				t.Errorf("Search %q != SearchTx %q", errA, errB)
			}
			if decA != decB {
				t.Errorf("decisions differ: %+v vs %+v", decA, decB)
			}
		})
	}
}

// bw2Float is the *float64 helper; the *int sibling ptrInt already lives in
// arms_fusion_test.go, same test package.
func bw2Float(v float64) *float64 { return &v }
