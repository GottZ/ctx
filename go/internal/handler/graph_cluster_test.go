// Wave C7 (Cluster-Topic-Map, design/03 §4.3 + §7 "C7") — the gates that need
// no database and prove it by running against a nil pool: every arm below
// returns BEFORE the store call, so a regression that lets one through panics
// instead of quietly passing.
//
//	(ii) CEILINGS ARE 400, never a silent clamp — the read-cap discipline of the
//	     other graph routes, applied to this route's OWN caps;
//	(iv) cluster.route_enabled=false ⇒ 404, indistinguishable from an absent
//	     route, and NO access-log row (nothing is written on this path at all);
//	(v)  graph_overview.enabled=false ⇒ the same 404 — the inherited gate, whose
//	     reasoning handler/overview.go already codifies: the flag also gates the
//	     rebuild job, so "off" means the tables are stale or empty anyway.
package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
)

const c7Handle = "aaaaaaaa-0000-4000-8000-0000000000c7"

func c7Get(t *testing.T, overviewEnabled, routeEnabled bool, query string) *httptest.ResponseRecorder {
	t.Helper()
	h := NewGraphClusterHandler(nil, ovStubConfig{&config.Config{
		GraphOverview: config.GraphOverviewConfig{Enabled: overviewEnabled},
		ClusterOps:    config.ClusterOpsConfig{RouteEnabled: routeEnabled},
	}}, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/graph/cluster?"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		IsValid: true, ApiKeyID: "019d0000-0000-7000-9000-0000000000ff",
		HomeScope: "private", ReadScopes: []string{"private"},
	}))
	rec := httptest.NewRecorder()
	h.HandleCluster(rec, req)
	return rec
}

// Gates (iv) + (v): both gates answer the same 404 as an absent route, and the
// bodies are byte-identical — a caller cannot tell "off" from "never existed",
// nor which of the two flags is closed.
//
// ROT-PROBE: register the route unconditionally (drop either half of the
// condition) ⇒ the request reaches the nil pool ⇒ panic instead of 404 ⇒ red.
func TestHandleCluster_BothGatesAre404(t *testing.T) {
	bodies := map[string]string{}
	for name, tc := range map[string]struct{ overview, route bool }{
		"both off":     {false, false},
		"route off":    {true, false},
		"overview off": {false, true},
	} {
		rec := c7Get(t, tc.overview, tc.route, "cluster="+c7Handle)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", name, rec.Code)
		}
		bodies[name] = rec.Body.String()
	}
	if bodies["route off"] != bodies["overview off"] || bodies["both off"] != bodies["route off"] {
		t.Errorf("the three closed states must be indistinguishable: %#v", bodies)
	}
	if !strings.Contains(bodies["both off"], `"error":"Not found"`) {
		t.Errorf("closed gate must answer the generic Not found: %s", bodies["both off"])
	}
}

// Gate (ii): ceilings are a 400 with a plain reason, and a malformed or missing
// handle is a 400 BEFORE any DB roundtrip. All of it with both gates OPEN, so
// the only thing standing between the request and the nil pool is the parser.
//
// ROT-PROBE: clamp instead of rejecting (e.g. `if limit > max { limit = max }`)
// ⇒ the request proceeds to the nil pool ⇒ panic instead of 400 ⇒ red.
func TestHandleCluster_CeilingsAre400NotClamped(t *testing.T) {
	for name, q := range map[string]string{
		"limit over ceiling":     "cluster=" + c7Handle + "&limit=9999",
		"limit zero":             "cluster=" + c7Handle + "&limit=0",
		"neighbor over ceiling":  "cluster=" + c7Handle + "&neighbor_limit=201",
		"neighbor not a number":  "cluster=" + c7Handle + "&neighbor_limit=many",
		"handle malformed":       "cluster=not-a-uuid",
		"handle missing":         "limit=10",
		"handle truncated uuid":  "cluster=aaaaaaaa-0000-4000-8000",
		"handle with extra char": "cluster=" + c7Handle + "x",
	} {
		rec := c7Get(t, true, true, q)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s (%s): status = %d, want 400", name, q, rec.Code)
		}
	}
	// The accepted edges of the ranges must NOT be rejected — otherwise the test
	// above would pass for a parser that rejects everything.
	for name, q := range map[string]string{
		"limit at ceiling":    "cluster=" + c7Handle + "&limit=1500",
		"neighbor at ceiling": "cluster=" + c7Handle + "&neighbor_limit=200",
	} {
		func() {
			defer func() {
				if r := recover(); r == nil {
					t.Errorf("%s: expected the request to reach the (nil) store, got a clean return", name)
				}
			}()
			c7Get(t, true, true, q)
		}()
	}
}
