//go:build integration

// W13 store-layer gates (design/03-workflow-api-cli.md §3.4/§5.6, §7-W13):
//
//   - redelivery-idempotency: InsertWebhookEvent twice on one (project,delivery)
//     ⇒ inserted true then false, exactly 1 row; a NEW delivery ⇒ a 2nd row;
//   - PruneTenant drains the per-project webhook secret (§5.2 credential-outlives-
//     tenant leak — the blanket context_secrets-by-scope drain covers it);
//   - DeleteProject drains BOTH project-scoped credentials (webhook.github.<id>,
//     forge.token.<id>) while the SCOPE survives — RED against a naked register-
//     delete that orphans the sealed secret in an unreferenced scope;
//   - Retention eviction is INDEX-driven: EXPLAIN names idx_webhook_done, never a
//     Seq Scan (§3.4), and EvictWebhookEvents removes only OLD processed rows.
//
// Run: go test -tags=integration ./internal/store/ -run TestWebhookW13 -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func w13ProvisionStore(t *testing.T, pool *pgxpool.Pool, slug string) *store.ProvisionResult {
	t.Helper()
	res, err := store.ProvisionProject(context.Background(), pool, store.ProvisionParams{
		Slug: slug, DisplayName: slug + "/repo", Scope: slug + ":main",
		Identity: "github:" + slug + "/repo",
	})
	if err != nil {
		t.Fatalf("provision %s: %v", slug, err)
	}
	return res
}

func TestWebhookW13_RedeliveryIdempotent_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	res := w13ProvisionStore(t, pool, "w13store")

	ins, err := store.InsertWebhookEvent(ctx, pool, res.Project.ID, "guid-1", "issues", json.RawMessage(`{"a":1}`))
	if err != nil || !ins {
		t.Fatalf("first insert: inserted=%v err=%v, want true nil", ins, err)
	}
	// Same (project, delivery) ⇒ ON CONFLICT DO NOTHING: not inserted, still 1 row.
	ins, err = store.InsertWebhookEvent(ctx, pool, res.Project.ID, "guid-1", "issues", json.RawMessage(`{"a":2}`))
	if err != nil || ins {
		t.Fatalf("redelivery insert: inserted=%v err=%v, want false nil", ins, err)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_webhook_events WHERE project_id=$1::uuid`, res.Project.ID).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 1 {
		t.Fatalf("after redelivery: %d rows, want 1", n)
	}
	// A NEW delivery id ⇒ a second row.
	if ins, err = store.InsertWebhookEvent(ctx, pool, res.Project.ID, "guid-2", "issues", json.RawMessage(`{}`)); err != nil || !ins {
		t.Fatalf("new delivery: inserted=%v err=%v, want true nil", ins, err)
	}
}

func TestWebhookW13_PruneTenantDrainsSecret_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	res := w13ProvisionStore(t, pool, "w13prune")

	// Seal a webhook secret in the PROJECT scope (raw insert mirrors the sealed row).
	name := store.WebhookSecretName(res.Project.ID)
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_secrets (name, scope, nonce, ciphertext, key_version)
		 VALUES ($1, $2, '\x00'::bytea, '\x01'::bytea, 1)`, name, res.Scope); err != nil {
		t.Fatalf("seed webhook secret: %v", err)
	}
	if err := store.PruneTenant(ctx, pool, res.Tenant.ID); err != nil {
		t.Fatalf("PruneTenant: %v", err)
	}
	var after int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_secrets WHERE scope=$1`, res.Scope).Scan(&after); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if after != 0 {
		t.Errorf("webhook secret survived its tenant prune: %d rows, want 0 (§5.2 leak)", after)
	}
}

func TestWebhookW13_DeleteProjectDrainsSecret_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	res := w13ProvisionStore(t, pool, "w13del")

	// Seal BOTH project-scoped credentials in the project scope.
	for _, name := range []string{store.WebhookSecretName(res.Project.ID), "forge.token." + res.Project.ID} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_secrets (name, scope, nonce, ciphertext, key_version)
			 VALUES ($1, $2, '\x00'::bytea, '\x01'::bytea, 1)`, name, res.Scope); err != nil {
			t.Fatalf("seed secret %s: %v", name, err)
		}
	}
	deleted, err := store.DeleteProject(ctx, pool, res.Project.ID)
	if err != nil || !deleted {
		t.Fatalf("DeleteProject: deleted=%v err=%v, want true nil", deleted, err)
	}
	// Both credentials gone (RED against the naked register-delete).
	var secrets int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_secrets WHERE scope=$1`, res.Scope).Scan(&secrets); err != nil {
		t.Fatalf("count secrets: %v", err)
	}
	if secrets != 0 {
		t.Errorf("project-scoped secrets survived project delete: %d rows, want 0", secrets)
	}
	// The project row is gone …
	var projects int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_projects WHERE id=$1::uuid`, res.Project.ID).Scan(&projects); err != nil {
		t.Fatalf("count projects: %v", err)
	}
	if projects != 0 {
		t.Errorf("project row survived delete: %d, want 0", projects)
	}
	// … but the SCOPE survives (scope teardown is a tenant-lifecycle concern, §4.2).
	var scopes int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_tenant_scopes WHERE scope=$1`, res.Scope).Scan(&scopes); err != nil {
		t.Fatalf("count scopes: %v", err)
	}
	if scopes != 1 {
		t.Errorf("scope was torn down by project delete: %d, want 1 (blocks+scope must survive)", scopes)
	}
}

func TestWebhookW13_RetentionIndexed_Integration(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res := w13ProvisionStore(t, pool, "w13ret")
	pid := res.Project.ID

	// 4000 recent PROCESSED (kept), 30 OLD processed (evicted), 200 PENDING (kept).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_webhook_events (project_id, delivery_id, event, payload, processed_at, received_at)
		 SELECT $1::uuid, 'recent-'||i, 'issues', '{}'::jsonb, now(), now()
		 FROM generate_series(1,4000) g(i)`, pid); err != nil {
		t.Fatalf("seed recent processed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_webhook_events (project_id, delivery_id, event, payload, processed_at, received_at)
		 SELECT $1::uuid, 'old-'||i, 'issues', '{}'::jsonb, now()-interval '30 days', now()-interval '30 days'
		 FROM generate_series(1,30) g(i)`, pid); err != nil {
		t.Fatalf("seed old processed: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_webhook_events (project_id, delivery_id, event, payload)
		 SELECT $1::uuid, 'pend-'||i, 'issues', '{}'::jsonb
		 FROM generate_series(1,200) g(i)`, pid); err != nil {
		t.Fatalf("seed pending: %v", err)
	}
	if _, err := pool.Exec(ctx, `ANALYZE context_webhook_events`); err != nil {
		t.Fatalf("analyze: %v", err)
	}

	// EXPLAIN the eviction predicate: it must ride idx_webhook_done, never Seq Scan.
	rows, err := pool.Query(ctx,
		`EXPLAIN (FORMAT TEXT) DELETE FROM context_webhook_events
		  WHERE processed_at IS NOT NULL
		    AND received_at < now() - make_interval(secs => 1209600)`) // 14d
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	var plan strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			rows.Close()
			t.Fatalf("explain scan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteString("\n")
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		t.Fatalf("explain rows: %v", err)
	}
	p := plan.String()
	t.Logf("retention eviction plan:\n%s", p)
	if !strings.Contains(p, "idx_webhook_done") {
		t.Errorf("eviction plan does not name idx_webhook_done:\n%s", p)
	}
	if strings.Contains(p, "Seq Scan on context_webhook_events") {
		t.Errorf("eviction plan seq-scans the queue:\n%s", p)
	}

	// EvictWebhookEvents removes only the 30 OLD processed rows.
	evicted, err := store.EvictWebhookEvents(ctx, pool, 14*24*time.Hour)
	if err != nil {
		t.Fatalf("evict: %v", err)
	}
	if evicted != 30 {
		t.Errorf("evicted %d rows, want 30 (only old processed)", evicted)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_webhook_events WHERE project_id=$1::uuid`, pid).Scan(&remaining); err != nil {
		t.Fatalf("count remaining: %v", err)
	}
	if remaining != 4200 { // 4000 recent processed + 200 pending
		t.Errorf("remaining %d rows, want 4200 (recent processed + pending kept)", remaining)
	}
	// retention=0 is a no-op (operator opt-out).
	if n, err := store.EvictWebhookEvents(ctx, pool, 0); err != nil || n != 0 {
		t.Errorf("retention=0 evicted %d (err=%v), want 0 no-op", n, err)
	}
}
