//go:build integration

// GHSA §5 (advisory 2026-10-31): POST /api/digest built the topic map and the
// root map over the key's RAW read_scopes. ctx_auth folds every scope another
// tenant granted via context_tenant_grants into those, and both artefacts are
// written under the caller's HOME scope — so the granting tenant's titles, id
// prefixes and [scope] tags landed in a block every reader of the grantee's
// home scope can see, granted or not. The request path now clamps the window
// to read_scopes ∩ TenantScopes(tenant), the background pipeline's semantics.
//
//	go test -tags=integration ./internal/handler/ -run TestDigestGrantWindow -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	s5HomeScope    = "s5a-home"
	s5ForeignScope = "s5b-data"
	s5OwnTitle     = "s5-own-visible-title"
	s5ForeignTitle = "s5-foreign-confidential-title"
	s5ForeignLabel = "s5-foreign-cluster-label"
)

// s5PostDigest drives the real handler with the given auth result (the route's
// only gate is handler.Auth, which lands the same *AuthResult in the context).
func s5PostDigest(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult) map[string]any {
	t.Helper()
	reg := blocktype.NewRegistry()
	bctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Boot(bctx, pool)

	h := NewDigestHandler(pool, reg, staticConfigStore{cfg: wdDigestConfig()})
	req := httptest.NewRequest(http.MethodPost, "/api/digest", strings.NewReader(`{}`))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleDigest(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /api/digest = %d: %s", rec.Code, rec.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not JSON: %s", rec.Body.String())
	}
	return body
}

// s5Seed builds the two-tenant fixture: tenant A owns s5a-home, tenant B owns
// s5b-data and A holds a scope grant on it. Returns A's real, ctx_auth-resolved
// auth result — read_scopes carries the grant, exactly as on the wire.
func s5Seed(t *testing.T, pool *pgxpool.Pool) *auth.AuthResult {
	t.Helper()
	ctx := context.Background()
	tenantA, err := store.CreateTenant(ctx, pool, "s5-tenant-a", "S5 A")
	if err != nil {
		t.Fatalf("create tenant A: %v", err)
	}
	tenantB, err := store.CreateTenant(ctx, pool, "s5-tenant-b", "S5 B")
	if err != nil {
		t.Fatalf("create tenant B: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid), ($3, $4::uuid)`,
		s5HomeScope, tenantA.ID, s5ForeignScope, tenantB.ID); err != nil {
		t.Fatalf("map scopes: %v", err)
	}
	if _, err := store.CreateTenantGrant(ctx, pool, tenantA.ID, s5ForeignScope, ""); err != nil {
		t.Fatalf("grant A read on %s: %v", s5ForeignScope, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'own body', $2), ('learnings', $3, 'foreign body', $4)`,
		s5OwnTitle, s5HomeScope, s5ForeignTitle, s5ForeignScope); err != nil {
		t.Fatalf("seed blocks: %v", err)
	}

	s5SeedClusters(t, pool)

	_, plain, err := store.CreateApiKey(ctx, pool, "s5-a-key", s5HomeScope, []string{}, tenantA.ID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || ar == nil || !ar.IsValid {
		t.Fatalf("authenticate: ar=%v err=%v", ar, err)
	}
	// Precondition: the grant IS in the key's read_scopes — the key may read B's
	// scope. The fix must not change that; it bounds what is written home.
	if ar.TenantID != tenantA.ID || !slices.Contains(ar.ReadScopes, s5ForeignScope) {
		t.Fatalf("fixture: tenant=%q read_scopes=%v, want tenant A with %s folded in", ar.TenantID, ar.ReadScopes, s5ForeignScope)
	}
	return ar
}

// s5SeedClusters gives each scope one labelled cluster (topic, node, member,
// overview meta) so the root map has cluster lines to render (size 5 clears
// root_map.small_cluster_max = 2 of wdDigestConfig) — the root-map arm
// of the leak is the foreign cluster's label and representative title.
func s5SeedClusters(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed clusters (%s): %v", strings.TrimSpace(sql[:40]), err)
		}
	}
	for _, c := range []struct{ cluster, topic, block, scope, label, title string }{
		{"019e8000-0000-7000-9000-0000000005a1", "aaaaaaaa-0000-4000-8000-0000000005a1",
			"019e8000-0000-7000-9000-0000000005a2", s5HomeScope, "s5-own-cluster-label", "s5-own-repr-title"},
		{"019e8000-0000-7000-9000-0000000005b1", "bbbbbbbb-0000-4000-8000-0000000005b1",
			"019e8000-0000-7000-9000-0000000005b2", s5ForeignScope, s5ForeignLabel, s5ForeignTitle + "-repr"},
	} {
		exec(`INSERT INTO graph_cluster_topic (topic_id, scope, label, label_source, label_built_at, label_stale, label_model)
		      VALUES ($1::uuid, $2, $3, 'llm', now(), false, 'qwen3.5:9b')`, c.topic, c.scope, c.label)
		exec(`INSERT INTO context_blocks (id, category, title, content, scope)
		      VALUES ($1::uuid, 'learnings', $2, 's5 cluster member', $3)`, c.block, c.title, c.scope)
		exec(`INSERT INTO graph_cluster_node (cluster_id, scope, size, repr_block_id, repr_title, repr_quality, category_counts, topic_id)
		      VALUES ($1::uuid, $2, 5, $3::uuid, $4, 1, '{"learnings":5}'::jsonb, $5::uuid)`,
			c.cluster, c.scope, c.block, c.title, c.topic)
		exec(`INSERT INTO graph_cluster_member (block_id, cluster_id, scope) VALUES ($1::uuid, $2::uuid, $3)`,
			c.block, c.cluster, c.scope)
		exec(`INSERT INTO graph_overview_meta (scope, cluster_n, node_n, edge_n, modularity, computed_at)
		      VALUES ($1, 1, 1, 0, 0.5, now())
		      ON CONFLICT (scope) DO UPDATE SET computed_at = EXCLUDED.computed_at`, c.scope)
	}
}

func s5MapContent(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	var content string
	if err := pool.QueryRow(context.Background(),
		`SELECT content FROM context_blocks WHERE category = 'index' AND title = $1 AND scope = $2`,
		title, s5HomeScope).Scan(&content); err != nil {
		t.Fatalf("read %s under %s: %v", title, s5HomeScope, err)
	}
	return content
}

func s5AssertNoForeign(t *testing.T, what, content string) {
	t.Helper()
	for _, leak := range []string{s5ForeignTitle, s5ForeignScope, s5ForeignLabel} {
		if strings.Contains(content, leak) {
			t.Errorf("%s under %s carries %q from the granting tenant's scope (GHSA §5 leak):\n%s",
				what, s5HomeScope, leak, content)
		}
	}
}

// TestDigestGrantWindow_ClampsToOwnedScopes: a key of tenant A with a grant on
// tenant B's scope rebuilds its maps — neither the topic map nor the root map
// under A's home scope carries B's title or scope tag, and the envelope counts
// the clamped window.
//
// RED against the unfixed handler: the topic map lists
// "s5-foreign-confidential-title [s5b-data]" and blockCount counts B's block.
func TestDigestGrantWindow_ClampsToOwnedScopes(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ar := s5Seed(t, pool)

	body := s5PostDigest(t, pool, ar)

	topic := s5MapContent(t, pool, "topic-map-"+s5HomeScope)
	if !strings.Contains(topic, s5OwnTitle) {
		t.Fatalf("topic map lost the tenant's own block (over-clamped):\n%s", topic)
	}
	s5AssertNoForeign(t, "topic map", topic)
	root := s5MapContent(t, pool, "root-map-"+s5HomeScope)
	if !strings.Contains(root, "s5-own-cluster-label") {
		t.Fatalf("root map lost the tenant's own cluster (over-clamped or fixture inert):\n%s", root)
	}
	s5AssertNoForeign(t, "root map", root)

	// The envelope counts the clamped window: the home scope's own active rows
	// (the seeded block plus the two maps just written), not B's on top.
	var homeActive int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_blocks WHERE scope = $1 AND NOT is_archived`,
		s5HomeScope).Scan(&homeActive); err != nil {
		t.Fatalf("count home blocks: %v", err)
	}
	if got, _ := body["blockCount"].(float64); int(got) != homeActive {
		t.Errorf("blockCount = %v, want %d (the clamped window, not the grant-widened one)", body["blockCount"], homeActive)
	}
}

// TestDigestGrantWindow_LookupErrorFailsClosed: when the tenant-scope lookup
// fails, the window collapses to the home scope — never the raw read_scopes.
// A malformed tenant id makes TenantScopes raise 22P02, the cheapest real error.
func TestDigestGrantWindow_LookupErrorFailsClosed(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ar := s5Seed(t, pool)
	broken := *ar
	broken.TenantID = "not-a-uuid"

	s5PostDigest(t, pool, &broken)

	topic := s5MapContent(t, pool, "topic-map-"+s5HomeScope)
	if !strings.Contains(topic, s5OwnTitle) {
		t.Fatalf("fail-closed window lost the home scope:\n%s", topic)
	}
	s5AssertNoForeign(t, "topic map", topic)
}
