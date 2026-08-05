//go:build integration

// Wave C7 (Cluster-Topic-Map, design/03 §4.3 + §7 "C7", Masterplan K2 / A03-2)
// — the route against a real database.
//
//	(i)   404 WITHOUT ORACLE: "handle unknown" and "handle known but its scope is
//	      unreadable" answer byte-identically AND write no access-log row. A
//	      telemetry row on the 404 path would make invisible handles bumpable by
//	      UUID probing — the oracle in slow motion;
//	(iii) NEIGHBOUR SCOPE PURITY: a neighbouring topic whose meta-edge touches an
//	      invisible endpoint scope does not appear;
//	(vi)  PARTITION-SCHARF (K2): cluster.size and nodes[] describe the SAME set.
//	      A handle names one scope-pure topic, so the other half of a
//	      scope-crossing cluster is a different handle — and must not be counted
//	      here nor delivered here, even when the caller could read it.
//
//	go test -tags=integration ./internal/handler/ -run TestClusterRoute -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	c7ClusterMain    = "019e8000-0000-7000-9000-00000000c001"
	c7ClusterNeighA  = "019e8000-0000-7000-9000-00000000c002" // visible neighbour
	c7ClusterNeighB  = "019e8000-0000-7000-9000-00000000c003" // neighbour in an unreadable scope
	c7TopicPrivate   = "aaaaaaaa-0000-4000-8000-00000000d001" // the main topic (private half)
	c7TopicWork      = "bbbbbbbb-0000-4000-8000-00000000d002" // the SAME cluster's work half
	c7TopicNeighborA = "cccccccc-0000-4000-8000-00000000d003"
	c7TopicNeighborB = "dddddddd-0000-4000-8000-00000000d004"
	c7TopicUnknown   = "eeeeeeee-0000-4000-8000-00000000d005" // never inserted
)

// c7Seed builds: one cluster with a private (2 members) and a work (1 member)
// partition, a visible neighbour topic in `private`, and an invisible one in
// `secret` — plus the meta-edges to both.
func c7Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", strings.TrimSpace(sql[:40]), err)
		}
	}
	topic := func(id, scope, label, source string) {
		exec(`INSERT INTO graph_cluster_topic (topic_id, scope, label, label_source, label_built_at, label_stale, label_model)
		      VALUES ($1::uuid, $2, $3, $4, now(), false, 'qwen3.5:9b')`, id, scope, label, source)
	}
	node := func(cluster, scope, topicID string, size int) {
		exec(`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts, topic_id)
		      VALUES ($1::uuid, $2, $3, $1::uuid, 'repr '||$2, 1, '{"learnings":1}'::jsonb, $4::uuid)`,
			cluster, scope, size, topicID)
	}
	member := func(id, scope, cluster string) {
		exec(`INSERT INTO context_blocks (id, category, title, content, scope)
		      VALUES ($1::uuid, 'learnings', $2, 'c7 fixture', $3)`, id, "blk-"+id, scope)
		exec(`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, $3)`,
			id, cluster, scope)
	}

	topic(c7TopicPrivate, "private", "Retrieval-Architektur", "llm")
	topic(c7TopicWork, "work", "fremde Hälfte", "fallback")
	topic(c7TopicNeighborA, "private", "Nachbar sichtbar", "fallback")
	topic(c7TopicNeighborB, "secret", "Nachbar unsichtbar", "fallback")

	node(c7ClusterMain, "private", c7TopicPrivate, 2)
	node(c7ClusterMain, "work", c7TopicWork, 1)
	node(c7ClusterNeighA, "private", c7TopicNeighborA, 5)
	node(c7ClusterNeighB, "secret", c7TopicNeighborB, 9)

	member("019e8000-0000-7000-9000-0000000000a1", "private", c7ClusterMain)
	member("019e8000-0000-7000-9000-0000000000a2", "private", c7ClusterMain)
	member("019e8000-0000-7000-9000-0000000000b1", "work", c7ClusterMain)
	member("019e8000-0000-7000-9000-0000000000c1", "private", c7ClusterNeighA)

	// Meta-edges. cluster_a < cluster_b holds by construction of the ids above,
	// and scope_s/scope_t are positional to that pair.
	exec(`INSERT INTO graph_cluster_edge (cluster_a, cluster_b, scope_s, scope_t, link_count, weight_sum)
	      VALUES ($1::uuid, $2::uuid, 'private', 'private', 7, 4.8115)`, c7ClusterMain, c7ClusterNeighA)
	exec(`INSERT INTO graph_cluster_edge (cluster_a, cluster_b, scope_s, scope_t, link_count, weight_sum)
	      VALUES ($1::uuid, $2::uuid, 'private', 'secret', 99, 42.0)`, c7ClusterMain, c7ClusterNeighB)

	exec(`INSERT INTO graph_overview_meta (scope, cluster_n, node_n, edge_n, modularity, computed_at)
	      VALUES ('private', 3, 4, 2, 0.87, now())
	      ON CONFLICT (scope) DO UPDATE SET computed_at = EXCLUDED.computed_at`)
}

func c7Handler(t *testing.T, pool *pgxpool.Pool, routeEnabled bool) *GraphClusterHandler {
	t.Helper()
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)
	return NewGraphClusterHandler(pool, staticConfigStore{cfg: &config.Config{
		GraphOverview: config.GraphOverviewConfig{Enabled: true},
		ClusterOps:    config.ClusterOpsConfig{RouteEnabled: routeEnabled},
	}}, reg)
}

// c7Key mints a REAL context_api_keys row: context_access_log.api_key_id is
// FK-bound, so a synthetic id would make every telemetry insert fail silently
// (best effort, logged not returned) — and the "no row on the 404 path" gate
// would then pass for the wrong reason.
func c7Key(t *testing.T, pool *pgxpool.Pool) *auth.AuthResult {
	t.Helper()
	row, _, err := store.CreateApiKey(context.Background(), pool, "c7-route", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create api key: %v", err)
	}
	return &auth.AuthResult{IsValid: true, ApiKeyID: row.ID, HomeScope: "private", ReadScopes: []string{"private"}}
}

func c7Call(t *testing.T, h *GraphClusterHandler, ar *auth.AuthResult, query string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/graph/cluster?"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleCluster(rec, req)
	return rec.Code, rec.Body.String()
}

func c7AccessRows(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_access_log WHERE action = 'graph-cluster'`).Scan(&n); err != nil {
		t.Fatalf("access log count: %v", err)
	}
	return n
}

// Gate (i): the two invisible cases are one answer, and neither leaves a trace.
//
// ROT-PROBE: give the two cases different error texts (or log access before the
// visibility check) ⇒ red. Both are the classic ways this oracle comes back.
func TestClusterRoute_404WithoutOracleAndWithoutAccessLog(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c7Seed(t, pool)
	h := c7Handler(t, pool, true)
	ar := c7Key(t, pool)

	before := c7AccessRows(t, pool)
	codeUnknown, bodyUnknown := c7Call(t, h, ar, "cluster="+c7TopicUnknown)
	codeForeign, bodyForeign := c7Call(t, h, ar, "cluster="+c7TopicWork) // exists, scope unreadable

	if codeUnknown != http.StatusNotFound || codeForeign != http.StatusNotFound {
		t.Fatalf("both must be 404: unknown %d, foreign %d (%s / %s)", codeUnknown, codeForeign, bodyUnknown, bodyForeign)
	}
	if bodyUnknown != bodyForeign {
		t.Errorf("404 bodies must be byte-identical:\n unknown %s\n foreign %s", bodyUnknown, bodyForeign)
	}
	if after := c7AccessRows(t, pool); after != before {
		t.Errorf("the 404 path wrote %d access-log rows — invisible handles must not be telemetrically bumpable", after-before)
	}

	// A successful call DOES log — otherwise the assert above would hold for a
	// route that never logs at all.
	if code, body := c7Call(t, h, ar, "cluster="+c7TopicPrivate); code != http.StatusOK {
		t.Fatalf("visible handle: %d %s", code, body)
	}
	if after := c7AccessRows(t, pool); after != before+1 {
		t.Errorf("successful call wrote %d rows, want exactly 1", after-before)
	}
}

// Gates (iii) + (vi): the envelope of a visible topic.
//
// ROT-PROBE for (iii): drop either `scope_s = ANY($2)` or `scope_t = ANY($2)`
// from clusterEgoNeighborsSQL ⇒ the secret neighbour (link_count 99) appears ⇒
// red. ROT-PROBE for (vi): read the members of the whole CLUSTER instead of the
// partition (drop `m.scope = $3`) ⇒ nodes[] carries the work member while size
// stays 2 ⇒ red.
func TestClusterRoute_EnvelopeIsPartitionScharf(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c7Seed(t, pool)
	h := c7Handler(t, pool, true)
	ar := c7Key(t, pool)

	code, body := c7Call(t, h, ar, "cluster="+c7TopicPrivate)
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	var env struct {
		Cluster struct {
			Handle        string   `json:"handle"`
			Label         string   `json:"label"`
			LabelSource   string   `json:"label_source"`
			LabelModel    string   `json:"label_model"`
			Size          int      `json:"size"`
			ScopeMix      []string `json:"scope_mix"`
			TopCategories []string `json:"top_categories"`
			ComputedAt    *string  `json:"computed_at"`
		} `json:"cluster"`
		Nodes []struct {
			ID    string `json:"id"`
			Scope string `json:"scope"`
		} `json:"nodes"`
		Neighbors []struct {
			Handle    string  `json:"handle"`
			Size      int     `json:"size"`
			LinkCount int     `json:"link_count"`
			Weight    float64 `json:"weight"`
		} `json:"neighbors"`
		Stats struct {
			Nodes     int  `json:"nodes"`
			Neighbors int  `json:"neighbors"`
			Truncated bool `json:"truncated"`
		} `json:"stats"`
	}
	if err := json.Unmarshal([]byte(body), &env); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}

	// (vi) size and nodes[] describe the same set.
	if env.Cluster.Size != 2 || env.Stats.Nodes != 2 || len(env.Nodes) != 2 {
		t.Errorf("size %d vs nodes %d — a handle names ONE partition, both numbers must describe it", env.Cluster.Size, len(env.Nodes))
	}
	for _, n := range env.Nodes {
		if n.Scope != "private" {
			t.Errorf("node %s has scope %s — the work half is a different handle", n.ID, n.Scope)
		}
	}
	if len(env.Cluster.ScopeMix) != 1 || env.Cluster.ScopeMix[0] != "private" {
		t.Errorf("scope_mix = %v, want exactly the handle's own scope", env.Cluster.ScopeMix)
	}

	// Label provenance on the DETAIL route (E4-02) — deliberately not on the
	// overview wire.
	if env.Cluster.LabelSource != "llm" || env.Cluster.LabelModel != "qwen3.5:9b" {
		t.Errorf("label provenance = %q/%q, want llm/qwen3.5:9b", env.Cluster.LabelSource, env.Cluster.LabelModel)
	}
	if env.Cluster.Label != "Retrieval-Architektur" || env.Cluster.Handle != c7TopicPrivate {
		t.Errorf("cluster meta = %+v", env.Cluster)
	}
	if env.Cluster.ComputedAt == nil {
		t.Error("computed_at must ride along unconditionally — this route hands freshness to its viewer")
	}
	if len(env.Cluster.TopCategories) != 1 || env.Cluster.TopCategories[0] != "learnings" {
		t.Errorf("top_categories = %v", env.Cluster.TopCategories)
	}

	// (iii) exactly ONE neighbour: the private one. The secret one is filtered by
	// the doubled scope condition, and it is deliberately the LOUDER edge
	// (link_count 99 vs 7) so a leak cannot hide behind the ordering.
	if len(env.Neighbors) != 1 {
		t.Fatalf("neighbors = %+v, want only the visible one", env.Neighbors)
	}
	nb := env.Neighbors[0]
	if nb.Handle != c7TopicNeighborA || nb.LinkCount != 7 || nb.Size != 5 {
		t.Errorf("neighbour = %+v, want the private topic (link_count 7, size 5)", nb)
	}
	if nb.Weight != 4.812 {
		t.Errorf("weight = %v, want 4.812 (rounded to three decimals like every other graph surface)", nb.Weight)
	}
	if env.Stats.Truncated {
		t.Error("stats.truncated must be false below the ceiling")
	}

	// (vi) THE K2 CASE that only a two-scope caller can show: with `work`
	// readable too, a union over the cluster's partitions WOULD deliver the work
	// member — and size, which is the partition's, would stop describing nodes[].
	// The handle names one scope-pure topic, so the answer must not change.
	both := &auth.AuthResult{IsValid: true, ApiKeyID: ar.ApiKeyID,
		HomeScope: "private", ReadScopes: []string{"private", "work"}}
	code, body = c7Call(t, h, both, "cluster="+c7TopicPrivate)
	if code != http.StatusOK {
		t.Fatalf("two-scope caller: %d %s", code, body)
	}
	var wide struct {
		Cluster struct {
			Size     int      `json:"size"`
			ScopeMix []string `json:"scope_mix"`
		} `json:"cluster"`
		Nodes []struct {
			Scope string `json:"scope"`
		} `json:"nodes"`
	}
	if err := json.Unmarshal([]byte(body), &wide); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if wide.Cluster.Size != 2 || len(wide.Nodes) != 2 {
		t.Errorf("two-scope caller got size %d / %d nodes — a handle must stay partition-scharf regardless of what else the caller may read",
			wide.Cluster.Size, len(wide.Nodes))
	}
	for _, n := range wide.Nodes {
		if n.Scope != "work" {
			continue
		}
		t.Error("the work half arrived under the private handle — that is the pre-K2 union semantics")
	}
	if len(wide.Cluster.ScopeMix) != 1 {
		t.Errorf("scope_mix = %v, want exactly one scope", wide.Cluster.ScopeMix)
	}
}

// The route stays dark by default: with cluster.route_enabled off a HANDLE THAT
// EXISTS answers exactly like an unknown one — proven here against the same
// database that would otherwise serve it.
//
// ROT-PROBE: register the route without the flag ⇒ the visible handle answers
// 200 ⇒ red.
func TestClusterRoute_DarkByDefault(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c7Seed(t, pool)
	off := c7Handler(t, pool, false)
	ar := c7Key(t, pool)

	before := c7AccessRows(t, pool)
	code, body := c7Call(t, off, ar, "cluster="+c7TopicPrivate)
	if code != http.StatusNotFound {
		t.Fatalf("dark route must answer 404, got %d: %s", code, body)
	}
	if !strings.Contains(body, `"error":"Not found"`) {
		t.Errorf("dark route must answer the generic Not found (not 'Cluster not found', which would confirm the route exists): %s", body)
	}
	if after := c7AccessRows(t, pool); after != before {
		t.Errorf("the dark route wrote %d access-log rows", after-before)
	}
}
