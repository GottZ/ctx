//go:build integration

// Wave C6 (Cluster-Topic-Map, design/03 §4.8/§5.7 + §7 "C6") — the WIRE half of
// the facet, against a real database.
//
//	(i)  NO EXISTENCE ORACLE: "handle does not exist" and "handle exists, all its
//	     members are foreign-scoped" answer BYTE-IDENTICALLY (modulo the echoed
//	     handle itself). If they differed, the handle space would be enumerable —
//	     and because handles are stable per topic, the NUMBER of foreign topics
//	     would be derivable (§5.7);
//	(vi) DARK STATE: with cluster.facet_enabled off the response is byte-identical
//	     to the same request WITHOUT the field — the pre-C6 behaviour, where
//	     `cluster` was an unknown JSON key the decoder dropped.
//
//	go test -tags=integration ./internal/handler/ -run TestSearchClusterFacet -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

const (
	c6Cluster      = "019e7000-0000-7000-9000-00000000c001"
	c6TopicPrivate = "aaaaaaaa-0000-4000-8000-00000000e001"
	c6TopicWork    = "bbbbbbbb-0000-4000-8000-00000000e002"
	c6TopicUnknown = "cccccccc-0000-4000-8000-00000000e003" // never inserted
)

func c6Seed(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed (%s): %v", sql, err)
		}
	}
	for _, tp := range []struct{ id, scope, label string }{
		{c6TopicPrivate, "private", "privat"},
		{c6TopicWork, "work", "fremd"},
	} {
		exec(`INSERT INTO graph_cluster_topic (topic_id, scope, label, label_source, label_built_at, label_stale)
		      VALUES ($1::uuid, $2, $3, 'fallback', now(), false)`, tp.id, tp.scope, tp.label)
	}
	for _, n := range []struct {
		scope, topic string
	}{{"private", c6TopicPrivate}, {"work", c6TopicWork}} {
		exec(`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts, topic_id)
		      VALUES ($1::uuid, $2, 1, $1::uuid, 'repr', 1, '{"facet":1}'::jsonb, $3::uuid)`, c6Cluster, n.scope, n.topic)
	}
	for _, b := range []struct{ id, scope string }{
		{"019e7000-0000-7000-9000-0000000000a1", "private"},
		{"019e7000-0000-7000-9000-0000000000a2", "work"},
	} {
		exec(`INSERT INTO context_blocks (id, category, title, content, scope)
		      VALUES ($1::uuid, 'facet', $2, 'facet fixture', $3)`, b.id, "blk-"+b.id, b.scope)
		exec(`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, $3)`,
			b.id, c6Cluster, b.scope)
	}
}

func c6Handler(pool *pgxpool.Pool, facet bool) *SearchHandler {
	return NewSearchHandler(pool, staticConfigStore{cfg: &config.Config{
		ClusterOps: config.ClusterOpsConfig{FacetEnabled: facet},
	}})
}

func c6Do(t *testing.T, h *SearchHandler, body map[string]any) (int, string) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/search", strings.NewReader(string(raw)))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, &auth.AuthResult{
		IsValid: true, ApiKeyID: "019d0000-0000-7000-9000-0000000000ff",
		HomeScope: "private", ReadScopes: []string{"private"},
	}))
	rec := httptest.NewRecorder()
	h.HandleSearch(rec, req)
	return rec.Code, rec.Body.String()
}

// Gate (i): the two unresolvable cases are one answer. The comparison replaces
// the handle itself with a placeholder — the echo is the ONLY legitimate
// difference, and leaving it in would let the test pass for the wrong reason.
//
// ROT-PROBE: add a 404 (or any distinct body) for "handle not resolvable" ⇒ the
// two responses stop matching ⇒ red. That branch is precisely the enumeration
// oracle of §5.7.
func TestSearchClusterFacet_NoExistenceOracle(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c6Seed(t, pool)
	h := c6Handler(pool, true)

	codeUnknown, bodyUnknown := c6Do(t, h, map[string]any{"category": "facet", "cluster": c6TopicUnknown})
	codeForeign, bodyForeign := c6Do(t, h, map[string]any{"category": "facet", "cluster": c6TopicWork})

	if codeUnknown != http.StatusOK || codeForeign != http.StatusOK {
		t.Fatalf("both unresolvable cases must be 200: unknown %d, foreign %d", codeUnknown, codeForeign)
	}
	norm := func(s, handle string) string { return strings.ReplaceAll(s, handle, "<handle>") }
	if norm(bodyUnknown, c6TopicUnknown) != norm(bodyForeign, c6TopicWork) {
		t.Errorf("unknown and foreign handle must be indistinguishable:\n unknown %s\n foreign %s", bodyUnknown, bodyForeign)
	}
	if !strings.Contains(bodyUnknown, `"count":0`) {
		t.Errorf("unresolvable handle must yield an empty result set: %s", bodyUnknown)
	}

	// And the resolvable one really does filter — otherwise the asserts above
	// would hold for a facet that never worked.
	code, body := c6Do(t, h, map[string]any{"category": "facet", "cluster": c6TopicPrivate})
	if code != http.StatusOK || !strings.Contains(body, `"count":1`) {
		t.Fatalf("the own-partition handle must return its member: %d %s", code, body)
	}
}

// Gate (vi): dark state. Facet off ⇒ the response is byte-identical to the same
// request without the field, INCLUDING the filters echo (no `cluster` key).
//
// ROT-PROBE: echo the requested handle unconditionally, or apply the filter
// regardless of the flag ⇒ the bytes diverge ⇒ red.
func TestSearchClusterFacet_DisabledIsByteIdentical(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	c6Seed(t, pool)
	off := c6Handler(pool, false)

	_, withField := c6Do(t, off, map[string]any{"category": "facet", "cluster": c6TopicPrivate})
	_, without := c6Do(t, off, map[string]any{"category": "facet"})
	if withField != without {
		t.Errorf("with the facet off the cluster field must not change a single byte:\n with %s\n without %s", withField, without)
	}
	if !strings.Contains(withField, `"count":1`) {
		t.Errorf("the disabled facet must not filter (private has one visible member): %s", withField)
	}

	// With the facet ON the same request DOES filter and DOES echo — otherwise
	// the byte-identity above would be the identity of a dead feature.
	_, on := c6Do(t, c6Handler(pool, true), map[string]any{"category": "facet", "cluster": c6TopicPrivate})
	if !strings.Contains(on, `"cluster":"`+c6TopicPrivate+`"`) {
		t.Errorf("the enabled facet must echo the applied filter: %s", on)
	}
}
