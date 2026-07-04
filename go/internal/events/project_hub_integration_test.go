//go:build integration

// W9 integration gates for the 081 NOTIFY triggers + the projectHub fan-out core
// against a real Postgres (testcontainers, PG18). Drives the ACTUAL trigger SQL
// and the ACTUAL hub Dispatch/flush over a live LISTEN connection, so the
// payload-shape, coalescing, cache and scope-isolation claims exercise exactly
// what production wires.
//
// Run: `go test -tags=integration ./internal/events/ -run TestW9 -count=1 -v`.
package events

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── seed helpers (events package can't reach the handler be5Seed* helpers) ─────.

func w9SeedTenant(t *testing.T, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("seed tenant %q: %v", slug, err)
	}
	return id
}

func w9SeedScope(t *testing.T, pool *pgxpool.Pool, scope, tenantID string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, tenantID); err != nil {
		t.Fatalf("seed scope %q: %v", scope, err)
	}
}

func w9SeedProject(t *testing.T, pool *pgxpool.Pool, slug string) (projectID, scope string) {
	t.Helper()
	tn := w9SeedTenant(t, pool, slug)
	scope = slug + ":repo"
	w9SeedScope(t, pool, scope, tn)
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		tn, scope, "github:acme/"+slug).Scan(&projectID); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return projectID, scope
}

func w9InsertIssue(t *testing.T, pool *pgxpool.Pool, scope, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source)
		 VALUES ('issue', $1, 'a body', $2, 'issue', 'manual') RETURNING id::text`, title, scope).Scan(&id); err != nil {
		t.Fatalf("insert issue: %v", err)
	}
	return id
}

// ── (A) 081 trigger payload shapes ────────────────────────────────────────────.

func TestW9TriggerPayloads(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, scope := w9SeedProject(t, pool, "w9trig")

	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire LISTEN conn: %v", err)
	}
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN "+channelProjectWrite); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	next := func() projectNotifyPayload {
		t.Helper()
		wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		n, err := lc.Conn().WaitForNotification(wctx)
		if err != nil {
			t.Fatalf("WaitForNotification: %v", err)
		}
		var p projectNotifyPayload
		if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
			t.Fatalf("payload %q: %v", n.Payload, err)
		}
		return p
	}

	// INSERT → op=INSERT, scope+type carried, id present, no content leak.
	id := w9InsertIssue(t, pool, scope, "first")
	if p := next(); p.Op != "INSERT" || p.Scope != scope || p.Type != "issue" || p.ID != id {
		t.Fatalf("INSERT payload = %+v (want op=INSERT scope=%s type=issue id=%s)", p, scope, id)
	}
	// UPDATE (incl. archive-"delete") → op=UPDATE.
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET title = 'edited' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("update: %v", err)
	}
	if p := next(); p.Op != "UPDATE" || p.ID != id {
		t.Fatalf("UPDATE payload = %+v (want op=UPDATE id=%s)", p, id)
	}
	// Physical DELETE (single row) → statement-level trigger → op=DELETE bulk=true.
	if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if p := next(); p.Op != "DELETE" || !p.Bulk || p.Scope != scope {
		t.Fatalf("DELETE payload = %+v (want op=DELETE bulk=true scope=%s)", p, scope)
	}
}

// TestW9TriggerNonWorkflowSilent proves a NON issue/comment write fires NO
// ctx_project_write notify (the WHEN type-filter keeps the whole corpus quiet).
func TestW9TriggerNonWorkflowSilent(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, scope := w9SeedProject(t, pool, "w9quiet")

	lc, _ := pool.Acquire(ctx)
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN "+channelProjectWrite); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}
	// A knowledge block in the same scope must NOT notify the project channel.
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source)
		 VALUES ('knowledge', 'k', 'b', $1, 'knowledge', 'manual')`, scope); err != nil {
		t.Fatalf("insert knowledge: %v", err)
	}
	// Then a real issue, so we have a positive terminator to wait for.
	id := w9InsertIssue(t, pool, scope, "issue")

	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	n, err := lc.Conn().WaitForNotification(wctx)
	if err != nil {
		t.Fatalf("WaitForNotification: %v", err)
	}
	var p projectNotifyPayload
	_ = json.Unmarshal([]byte(n.Payload), &p)
	if p.Type != "issue" || p.ID != id {
		t.Fatalf("first project notify = %+v, want the issue (knowledge write leaked into the channel)", p)
	}
}

// TestW9BatchDeleteCoalesced is the T6-Befund gate: a bulk DELETE statement over
// N issue rows fires the STATEMENT-level trigger ONCE → exactly ONE per-scope
// notify, NOT N. RED with a row-level DELETE trigger (N notifies = the O(n²)
// PreCommit storm the prune-batch path would light).
func TestW9BatchDeleteCoalesced(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, scope := w9SeedProject(t, pool, "w9batch")

	const n = 25
	for i := 0; i < n; i++ {
		w9InsertIssue(t, pool, scope, "bulk-"+strconv.Itoa(i))
	}

	// LISTEN only AFTER the inserts so their notifies are not counted.
	lc, _ := pool.Acquire(ctx)
	defer lc.Release()
	if _, err := lc.Exec(ctx, "LISTEN "+channelProjectWrite); err != nil {
		t.Fatalf("LISTEN: %v", err)
	}

	// ONE DELETE statement over all N issue rows.
	if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE scope = $1 AND type_name = 'issue'`, scope); err != nil {
		t.Fatalf("batch delete: %v", err)
	}

	// Count notifications until idle. Expect EXACTLY 1 (per-scope coalesced).
	count := 0
	for {
		wctx, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		nfy, err := lc.Conn().WaitForNotification(wctx)
		cancel()
		if err != nil {
			break // idle timeout → done
		}
		var p projectNotifyPayload
		_ = json.Unmarshal([]byte(nfy.Payload), &p)
		if p.Op == "DELETE" && p.Bulk {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("batch DELETE of %d rows produced %d DELETE notifies, want 1 (statement-level coalesce; RED=%d with a row-level trigger)", n, count, n)
	}
}

// TestW9ListenerDiscard is the "old binary" probe: a write against the 081
// triggers with NO listener on ctx_project_write commits normally — the notify
// is a Postgres no-op. Proven by NOT establishing any LISTEN and asserting the
// write succeeds (an old binary that never LISTENs runs byte-for-byte unchanged).
func TestW9ListenerDiscard(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	_, scope := w9SeedProject(t, pool, "w9discard")

	// No LISTEN anywhere. Write an issue AND batch-delete it.
	id := w9InsertIssue(t, pool, scope, "orphan")
	tag, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, id)
	if err != nil {
		t.Fatalf("write against un-listened trigger failed: %v", err)
	}
	if tag.RowsAffected() != 1 {
		t.Fatalf("delete affected %d rows, want 1 (write did not commit)", tag.RowsAffected())
	}
}

// ── (B) end-to-end hub fan-out over a live LISTEN feed ────────────────────────.

// startHubFeed wires a dedicated LISTEN conn to the hub exactly as the pgxlisten
// handler does in production: every ctx_project_write notify → hub.Dispatch.
func startHubFeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool, hub *ProjectHub) {
	t.Helper()
	lc, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire hub-feed conn: %v", err)
	}
	if _, err := lc.Exec(ctx, "LISTEN "+channelProjectWrite); err != nil {
		t.Fatalf("hub-feed LISTEN: %v", err)
	}
	go func() {
		defer lc.Release()
		for {
			n, err := lc.Conn().WaitForNotification(ctx)
			if err != nil {
				return // ctx cancelled
			}
			hub.Dispatch(n.Payload)
		}
	}()
}

func fastEventsConfig() *config.Store {
	return config.NewStore(&config.Config{Project: config.ProjectConfig{Events: config.ProjectEventsConfig{
		MaxConnections:    8,
		FlushInterval:     40 * time.Millisecond,
		PingInterval:      time.Second,
		CoalesceThreshold: 20,
	}}})
}

func waitFrame(t *testing.T, sub *projectSub, d time.Duration) (ProjectFrame, bool) {
	t.Helper()
	select {
	case f := <-sub.ch:
		return f, true
	case <-time.After(d):
		return ProjectFrame{}, false
	}
}

// TestW9EndToEndFanout: a live INSERT flows trigger → NOTIFY → hub → flush → sub.
func TestW9EndToEndFanout(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	projA, scopeA := w9SeedProject(t, pool, "w9e2e")
	hub := NewProjectHub(ctx, pool, fastEventsConfig())
	startHubFeed(t, ctx, pool, hub)

	sub, ok := hub.Subscribe("tenant-a", false, []string{scopeA}, 8)
	if !ok {
		t.Fatal("subscribe rejected")
	}
	defer hub.Unsubscribe(sub)

	id := w9InsertIssue(t, pool, scopeA, "live")
	f, ok := waitFrame(t, sub, 3*time.Second)
	if !ok {
		t.Fatal("no frame within 3s (trigger→hub→flush→sub path broken)")
	}
	if f.ProjectID != projA || f.Kind != "issue" {
		t.Fatalf("frame = %+v, want project=%s kind=issue", f, projA)
	}
	found := false
	for _, bid := range f.BlockIDs {
		if bid == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("frame block_ids %v missing the inserted issue %s", f.BlockIDs, id)
	}
}

// TestW9TwoTenantAbsence is the ACTIVE absence window: a write in tenant B's
// scope must NEVER reach a subscriber on tenant A over a multi-flush window.
func TestW9TwoTenantAbsence(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, scopeA := w9SeedProject(t, pool, "w9tta")
	projB, scopeB := w9SeedProject(t, pool, "w9ttb")
	hub := NewProjectHub(ctx, pool, fastEventsConfig())
	startHubFeed(t, ctx, pool, hub)

	subA, _ := hub.Subscribe("tenant-a", false, []string{scopeA}, 8)
	defer hub.Unsubscribe(subA)
	subB, _ := hub.Subscribe("tenant-b", false, []string{scopeB}, 8)
	defer hub.Unsubscribe(subB)

	// Write in scope B only.
	w9InsertIssue(t, pool, scopeB, "b-write")

	// B receives it (proves the feed is live) ...
	if f, ok := waitFrame(t, subB, 3*time.Second); !ok || f.ProjectID != projB {
		t.Fatalf("B did not receive its own write: ok=%v frame=%+v", ok, f)
	}
	// ... A stays SILENT across a full active window (many flushes at 40ms).
	if f, ok := waitFrame(t, subA, 1*time.Second); ok {
		t.Fatalf("CROSS-TENANT LEAK: A received %+v for a tenant-B write", f)
	}
}

// TestW9CacheInvalidation: a scope with no project is negative-cached and
// produces nothing; after the project is created AND InvalidateProjects() runs
// (the MountProject write-path hook), writes in that scope start fanning out.
// RED with a stale map: the negative entry would keep dropping the writes.
func TestW9CacheInvalidation(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A tenant + scope WITHOUT a project row yet.
	tn := w9SeedTenant(t, pool, "w9cache")
	scope := "w9cache:repo"
	w9SeedScope(t, pool, scope, tn)

	hub := NewProjectHub(ctx, pool, fastEventsConfig())
	startHubFeed(t, ctx, pool, hub)
	sub, _ := hub.Subscribe("tenant-c", false, []string{scope}, 8)
	defer hub.Unsubscribe(sub)

	// Write BEFORE the project exists → negative-cached, no frame.
	w9InsertIssue(t, pool, scope, "pre")
	if f, ok := waitFrame(t, sub, 800*time.Millisecond); ok {
		t.Fatalf("frame before project exists: %+v", f)
	}

	// Create the project row, then invalidate the cache (the write-path hook).
	var projID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid, $2, 'manual:w9cache') RETURNING id::text`,
		tn, scope).Scan(&projID); err != nil {
		t.Fatalf("create project: %v", err)
	}
	hub.InvalidateProjects()

	// Now a write resolves to the project and fans out.
	w9InsertIssue(t, pool, scope, "post")
	f, ok := waitFrame(t, sub, 3*time.Second)
	if !ok || f.ProjectID != projID {
		t.Fatalf("after create+invalidate: ok=%v frame=%+v want project=%s (stale negative cache?)", ok, f, projID)
	}
}
