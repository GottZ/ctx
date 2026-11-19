//go:build integration

// Issue #384: a block that enters a /api/query answer through graph expansion
// or cluster injection carried no type name, because the hydrate statements of
// those stages did not read type_name. The answer derives the foreign-text
// flag from the type (retrieval.untrusted), and the aggregate fold folds by
// type, so such a block lost the flag and a comment was not folded into its
// issue.
//
// Through the real handler, on the #225 fixture (query_poststage_filters_
// integration_test.go): blocks written through POST /api/store after the
// embedding pass (no vector, so they are no fusion hits and come in through
// the stage only), links through dream.WriteLinks.
//
//	go test -tags=integration ./internal/handler/ -run TestQueryStageBlocksCarryTheirType -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

const (
	stForeignType = "st-foreign" // untrusted and linkable, operator-registered
	stForeign     = "st ledger foreign tool output"
	stLateIssue   = "st ledger late issue"
	stLateComment = "st ledger late comment"
)

// stSource is one source of the answer as the wire carries it.
type stSource struct {
	Title     string `json:"title"`
	Untrusted bool   `json:"untrusted"`
}

// stSources runs one retrieval-only query and returns its sources by title.
func (f *pfFixture) stSources(t *testing.T, mutate func(*config.Config)) map[string]stSource {
	t.Helper()
	cfg := f.config()
	mutate(cfg)
	h := NewQueryHandler(f.pool, config.NewStore(cfg), bw2Pool(f.embed), nil, f.reg, snapshotTestAdmitter(t))
	h.SetClusterFreshness(f.fresh)
	raw, _ := json.Marshal(map[string]any{"query": bw2GoldenQuery, "synthesize": false, "limit": 20})
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(string(raw))).WithContext(f.ctx)
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query: status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Sources []stSource `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	out := map[string]stSource{}
	for _, s := range resp.Sources {
		out[s.Title] = s
	}
	return out
}

// stSetup is the #225 fixture with the aggregating type, plus an
// operator-registered type that is untrusted and linkable, a block of it, and
// an issue with one comment, all three without a vector.
func stSetup(t *testing.T) *pfFixture {
	t.Helper()
	f := pfSetup(t, true)
	if _, err := f.pool.Exec(context.Background(),
		`INSERT INTO context_block_types (name, scope, config) VALUES ($1, '_global', $2::jsonb)`,
		stForeignType, `{"v":1,"retrieval":{"untrusted":true},"dream":{"linkable":true}}`); err != nil {
		t.Fatalf("register %s: %v", stForeignType, err)
	}
	if err := f.reg.Reload(context.Background(), f.pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := f.reg.Snapshot()
	if !set.IsUntrusted(stForeignType) {
		t.Fatalf("premise: %s is not untrusted after the reload", stForeignType)
	}
	f.store(t, stForeign, "pf-open", stForeignType, "", "alpha")
	f.store(t, stLateIssue, "pf-open", "", "", "alpha")
	f.store(t, stLateComment, "pf-open", pfCommentType, f.ids[stLateIssue], "alpha")
	return f
}

func TestQueryStageBlocksCarryTheirType(t *testing.T) {
	t.Run("graph_expansion", func(t *testing.T) {
		f := stSetup(t)
		f.link(t, stForeign, stLateComment)
		graphOn := func(c *config.Config) {
			noBackfill(c)
			c.Graph = config.Defaults().Graph
			c.Graph.Enabled = true
		}
		if got := f.stSources(t, noBackfill); len(got) == 0 || got[stForeign].Title != "" || got[stLateIssue].Title != "" {
			t.Fatalf("graph off names %v — a stage block is a fusion hit, the probe proves nothing", got)
		}
		got := f.stSources(t, graphOn)
		src, ok := got[stForeign]
		if !ok {
			t.Fatalf("graph on: %q missing from %v — the neighbour did not come in", stForeign, got)
		}
		if !src.Untrusted {
			t.Errorf("graph neighbour of an untrusted type answered untrusted=false (a fusion hit of the same type is flagged)")
		}
		// The fold case: the comment that came in as a neighbour is
		// replaced by its issue.
		if _, ok := got[stLateIssue]; !ok {
			t.Errorf("graph on: the issue of the comment neighbour is missing from %v — the comment was not folded", got)
		}
		if _, ok := got[stLateComment]; ok {
			t.Errorf("graph on: the comment neighbour is delivered itself instead of its issue")
		}
	})

	t.Run("cluster_injection", func(t *testing.T) {
		f := stSetup(t)
		f.cluster(t, pfSeed, pfOpenBeta, stForeign)
		clusterOn := func(c *config.Config) {
			noBackfill(c)
			c.Cluster = config.Defaults().Cluster
			c.Cluster.Enabled = true
			c.Cluster.InjectMax = 5
			c.Cluster.MinShare = 0.1
			c.Cluster.SizeDamping = false
			c.ClusterOps.MaxStaleness = 24 * time.Hour
		}
		got := f.stSources(t, clusterOn)
		src, ok := got[stForeign]
		if !ok {
			t.Fatalf("cluster on: %q missing from %v — the sibling was not injected", stForeign, got)
		}
		if !src.Untrusted {
			t.Errorf("injected sibling of an untrusted type answered untrusted=false")
		}
	})
}
