package rrf

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// TestRerankJudgeFailureHandsOutTheError (issue #483): the judge stays
// fail-open — a failed call or an answer that does not parse keeps the input
// order, unscored — and hands its error to the caller, which marks a wait for
// an admission slot its client gave up (the query handler's 499 line).
func TestRerankJudgeFailureHandsOutTheError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter)
	}{
		{"the call fails", func(w http.ResponseWriter) { http.Error(w, "i483 judge down", http.StatusInternalServerError) }},
		{"the answer does not parse", func(w http.ResponseWriter) {
			fmt.Fprint(w, `{"message":{"role":"assistant","content":"no scores here"},"eval_count":1,"prompt_eval_count":1}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { tc.answer(w) }))
			defer srv.Close()
			bpool := backends.NewPool(nil, nil)
			bpool.SeedSnapshotForTest([]backends.Backend{{
				ID: "judge", Name: "judge", Host: srv.URL, Protocol: backends.ProtocolOllama,
				Trust: backends.TrustFull, Enabled: true, Priority: 1,
				Roles:    []string{backends.RoleSynthesis},
				ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
			}})
			// The input order is not the score order a judge would give: a
			// failure that re-sorted would show.
			results := []SearchResult{rc("C", 0.004, "c"), rc("A", 0.008, "a"), rc("B", 0.006, "b")}
			out, err := Rerank(context.Background(), nil, bpool, nil, "", backends.SensPublic, "q", results, "", rerankTestAdmission(t))
			if err == nil {
				t.Fatal("Rerank swallowed the judge's failure: err = nil")
			}
			if len(out) != 3 || out[0].ID != "C" || out[1].ID != "A" || out[2].ID != "B" {
				t.Fatalf("order after a failed judge = %v, want the input order C A B", ids(out))
			}
			for _, r := range out {
				if r.RerankScore != nil {
					t.Errorf("%s carries a rerank score after a failed judge", r.ID)
				}
			}
		})
	}
}
