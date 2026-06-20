package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"context"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
)

// ovStubConfig is a minimal ConfigStore for the gate tests.
type ovStubConfig struct{ cfg *config.Config }

func (s ovStubConfig) Snapshot() *config.Config { return s.cfg }
func (s ovStubConfig) SnapshotForRequest(context.Context) *config.Config { return s.cfg }
func (s ovStubConfig) SnapshotForTenant(context.Context, string) *config.Config { return s.cfg }

// overviewReq drives HandleOverview without a DB — only the early-return gates
// (auth, enabled) are exercised, which never touch the pool.
func overviewReq(ar *auth.AuthResult, enabled bool) *httptest.ResponseRecorder {
	h := NewGraphOverviewHandler(nil, ovStubConfig{&config.Config{
		GraphOverview: config.GraphOverviewConfig{Enabled: enabled},
	}})
	req := httptest.NewRequest(http.MethodGet, "/api/graph/overview", nil)
	if ar != nil {
		req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	}
	rec := httptest.NewRecorder()
	h.HandleOverview(rec, req)
	return rec
}

// TestHandleOverview_Unauthorized: a request with no AuthResult → 401, never
// reaching the (nil) pool.
func TestHandleOverview_Unauthorized(t *testing.T) {
	if rec := overviewReq(nil, true); rec.Code != http.StatusUnauthorized {
		t.Errorf("nil auth: want 401, got %d", rec.Code)
	}
}

// TestHandleOverview_DisabledIs404: graph_overview.enabled=false → 404,
// indistinguishable from an absent route (design §3.4 — gates the endpoint).
// Returns before the store call, so the nil pool is never dereferenced.
func TestHandleOverview_DisabledIs404(t *testing.T) {
	ar := &auth.AuthResult{IsValid: true, ApiKeyID: "019d0000-0000-7000-9000-0000000000ff", HomeScope: "private", ReadScopes: []string{"private"}}
	rec := overviewReq(ar, false)
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled feature: want 404, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"success":false`) {
		t.Errorf("disabled body should be an error envelope: %s", rec.Body.String())
	}
}

const (
	ovClusterA = "019d0000-0000-7000-9000-0000000000aa" // internal cluster_id (must NOT leak)
	ovClusterB = "019d0000-0000-7000-9000-0000000000bb"
	ovReprA    = "019d0000-0000-7000-9000-0000000000c1" // a visible repr block (may appear)
	ovReprB    = "019d0000-0000-7000-9000-0000000000c2"
)

func sampleOverviewResult() *store.OverviewResult {
	return &store.OverviewResult{
		Nodes: []store.OverviewNode{
			{ClusterID: ovClusterA, Size: 142, TopCategories: []string{"learnings", "decisions"}, ReprID: ovReprA, ReprTitle: "Alpha", ScopeMix: []string{"private", "shared"}},
			{ClusterID: ovClusterB, Size: 88, TopCategories: []string{"infrastructure"}, ReprID: ovReprB, ReprTitle: "Beta", ScopeMix: []string{"private"}},
		},
		Edges:      []store.OverviewEdge{{A: ovClusterA, B: ovClusterB, LinkCount: 318, Weight: 0.7399}},
		ComputedAt: time.Date(2026, 6, 14, 3, 0, 11, 0, time.UTC),
	}
}

// TestBuildOverviewResponse_NoClusterIDLeak is the OpSec gate (design §6.1): the
// internal cluster_id (= min member uuid, scope-agnostic) must NEVER appear in
// the wire payload — only the per-request ordinal. repr_id (a visible block) may.
func TestBuildOverviewResponse_NoClusterIDLeak(t *testing.T) {
	resp := buildOverviewResponse(sampleOverviewResult(), store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}, 7)
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	js := string(b)
	for _, cid := range []string{ovClusterA, ovClusterB} {
		if strings.Contains(js, cid) {
			t.Errorf("raw cluster_id %s leaked into wire payload:\n%s", cid, js)
		}
	}
	// The visible representative IS expected on the wire.
	if !strings.Contains(js, ovReprA) {
		t.Errorf("repr_id missing from payload: %s", js)
	}
}

// TestBuildOverviewResponse_OrdinalMapping: nodes get dense 0..N ordinals in
// order, and edges are remapped onto those ordinals.
func TestBuildOverviewResponse_OrdinalMapping(t *testing.T) {
	resp := buildOverviewResponse(sampleOverviewResult(), store.OverviewParams{MinClusterSize: 1, NodeLimit: 500, EdgeLimit: 2000}, 7)
	if resp.Nodes[0].Cluster != 0 || resp.Nodes[1].Cluster != 1 {
		t.Errorf("ordinals not dense/in-order: %d, %d", resp.Nodes[0].Cluster, resp.Nodes[1].Cluster)
	}
	if len(resp.Edges) != 1 {
		t.Fatalf("expected 1 edge, got %d", len(resp.Edges))
	}
	if resp.Edges[0].Src != 0 || resp.Edges[0].Dst != 1 || resp.Edges[0].LinkCount != 318 {
		t.Errorf("edge ordinal mapping wrong: %+v", resp.Edges[0])
	}
}

// TestOverviewEdgeWire_Marshal pins the compact tuple + 3-decimal rounding.
func TestOverviewEdgeWire_Marshal(t *testing.T) {
	b, err := overviewEdgeWire{Src: 0, Dst: 3, LinkCount: 318, Weight: 0.7399}.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(b); got != "[0,3,318,0.740]" {
		t.Errorf("got %s, want [0,3,318,0.740]", got)
	}
}

// TestBuildOverviewResponse_WireContract pins the envelope shape (field names,
// nesting, empty edges as [] not null, computed_at present).
func TestBuildOverviewResponse_WireContract(t *testing.T) {
	res := sampleOverviewResult()
	res.Edges = nil // exercise the empty-slice → [] path
	resp := buildOverviewResponse(res, store.OverviewParams{MinClusterSize: 1, MinInterWeight: 0, NodeLimit: 500, EdgeLimit: 2000}, 7)
	b, _ := json.Marshal(resp)
	js := string(b)
	for _, want := range []string{
		`"success":true`, `"params":`, `"min_cluster_size":1`, `"node_limit":500`,
		`"nodes":`, `"edges":[]`, `"stats":`, `"truncated":false`,
		`"computed_at":"2026-06-14T03:00:11Z"`, `"scope_mix":["private","shared"]`,
		`"cluster":0`, `"size":142`,
	} {
		if !strings.Contains(js, want) {
			t.Errorf("wire contract missing %s in:\n%s", want, js)
		}
	}
}

// TestBuildOverviewResponse_ComputedAtNull: never-built (zero time) → null.
func TestBuildOverviewResponse_ComputedAtNull(t *testing.T) {
	res := &store.OverviewResult{Nodes: nil, Edges: nil} // ComputedAt zero
	b, _ := json.Marshal(buildOverviewResponse(res, store.OverviewParams{NodeLimit: 500, EdgeLimit: 2000}, 1))
	js := string(b)
	if !strings.Contains(js, `"computed_at":null`) {
		t.Errorf("zero ComputedAt should marshal to null: %s", js)
	}
	if !strings.Contains(js, `"nodes":[]`) || !strings.Contains(js, `"edges":[]`) {
		t.Errorf("nil node/edge slices should marshal to []: %s", js)
	}
}

func TestParseOverviewParams(t *testing.T) {
	p, err := parseOverviewParams(url.Values{})
	if err != nil {
		t.Fatalf("defaults should parse: %v", err)
	}
	if p.MinClusterSize != 1 || p.NodeLimit != overviewDefaultNodeLimit || p.EdgeLimit != overviewDefaultEdgeLimit {
		t.Errorf("unexpected defaults: %+v", p)
	}

	for _, tc := range []struct {
		name string
		q    url.Values
	}{
		{"node_limit too high", url.Values{"node_limit": {"9999"}}},
		{"min_cluster_size zero", url.Values{"min_cluster_size": {"0"}}},
		{"edge_limit too high", url.Values{"edge_limit": {"999999"}}},
		{"node_limit non-int", url.Values{"node_limit": {"abc"}}},
		{"weight negative", url.Values{"min_inter_cluster_weight": {"-1"}}},
	} {
		if _, err := parseOverviewParams(tc.q); err == nil {
			t.Errorf("%s: expected 400 error, got nil", tc.name)
		}
	}
}
