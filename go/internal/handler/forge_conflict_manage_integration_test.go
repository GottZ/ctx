//go:build integration

// Issue #509: the resolve surface of the forge sync through the production
// HandleManage chain against PG18 — forge-conflict-list, forge-conflict-resolve
// and forge-stamp-reset. The flagged mappings are made by the real pull
// (forge.Applier: pull-create, a local edit through store.UpdateIssueBlock, a
// second pull of a remote edit), the resolver is the production one. Pinned:
// both sides on the list and its keyset pages, both directions and their
// idempotent repeat, the expect token, the push lock, the tier (403 for a
// member), the input checks ahead of any database access (400), the tenant and
// project boundary (404 without a side effect), and the audit rows a
// tenant-admin reads back through GET /api/audit and another tenant does not.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestForgeConflictManage -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// fcmEnv is one database with two tenants: A owns the projects, B is the
// foreign tenant. adminA, memberA and adminB carry real key rows, so the
// audit rows name an actor the audit surface can attribute.
type fcmEnv struct {
	pool    *pgxpool.Pool
	reg     *blocktype.Registry
	applier *forge.Applier
	h       *ManageHandler
	tenantA string
	slugA   string
	adminA  *auth.AuthResult
	memberA *auth.AuthResult
	adminB  *auth.AuthResult
}

func newFCMEnv(t *testing.T) *fcmEnv {
	t.Helper()
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	reg := blocktype.NewRegistry()
	e := &fcmEnv{pool: pool, reg: reg, applier: forge.NewApplier(pool, reg.SnapshotForTenant), slugA: "fcm-a"}
	e.h = NewManageHandler(pool, nil, nil, nil, nil, nil, nil, nil)
	e.h.SetForgeResolver(forge.NewResolver(pool, reg.SnapshotForTenant))

	tenant := func(slug string) (string, string) {
		tn, err := store.CreateTenant(ctx, pool, slug, slug)
		if err != nil {
			t.Fatalf("tenant %s: %v", slug, err)
		}
		home := tn.Slug + ":home"
		if _, err := store.AssignTenantScope(ctx, pool, tn.ID, home, nil); err != nil {
			t.Fatalf("scope %s: %v", home, err)
		}
		return tn.ID, home
	}
	key := func(tenantID, home, label, role string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`WITH p AS (INSERT INTO context_principals (display_name) VALUES ($3) RETURNING id)
			 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id, tenant_role)
			 SELECT $3 || '-hash', $3, $1, $2::uuid, p.id, $4 FROM p RETURNING id::text`,
			home, tenantID, label, role).Scan(&id); err != nil {
			t.Fatalf("key %s: %v", label, err)
		}
		return id
	}
	tA, homeA := tenant(e.slugA)
	tB, homeB := tenant("fcm-b")
	e.tenantA = tA
	e.adminA = &auth.AuthResult{IsValid: true, ApiKeyID: key(tA, homeA, "fcm-a-admin", "admin"), HomeScope: homeA, TenantID: tA, TenantRole: auth.RoleAdmin}
	e.memberA = &auth.AuthResult{IsValid: true, ApiKeyID: key(tA, homeA, "fcm-a-member", "member"), HomeScope: homeA, TenantID: tA, TenantRole: auth.RoleMember}
	e.adminB = &auth.AuthResult{IsValid: true, ApiKeyID: key(tB, homeB, "fcm-b-admin", "admin"), HomeScope: homeB, TenantID: tB, TenantRole: auth.RoleAdmin}
	return e
}

// project registers a forge project of tenant A in a scope of its own.
func (e *fcmEnv) project(t *testing.T, name string) store.ProjectRow {
	t.Helper()
	row, _, err := store.CreateProject(context.Background(), e.pool, store.CreateProjectParams{
		TenantID: e.tenantA, ScopeName: e.slugA + ":" + name, Identity: "github:o/" + name,
		Forge: json.RawMessage(`{"kind":"github","owner":"o","repo":"` + name + `"}`),
	})
	if err != nil {
		t.Fatalf("create project %s: %v", name, err)
	}
	return *row
}

// blockOf is the block id of issue n's mapping in proj.
func (e *fcmEnv) blockOf(t *testing.T, proj store.ProjectRow, n int64) string {
	t.Helper()
	var id string
	if err := e.pool.QueryRow(context.Background(),
		`SELECT block_id::text FROM context_project_sync_map WHERE project_id = $1::uuid AND entity_kind = 'issue' AND forge_id = $2`,
		proj.ID, n).Scan(&id); err != nil {
		t.Fatalf("mapping of issue %d: %v", n, err)
	}
	return id
}

// flag makes issue n of proj a flagged mapping the way a sync does: a pull
// creates the block, a local edit moves the block, a pull of a remote edit
// finds both sides apart from the base and flags the mapping.
func (e *fcmEnv) flag(t *testing.T, proj store.ProjectRow, n int64) string {
	t.Helper()
	ctx := context.Background()
	iss := forge.IssueRemote{Number: n, Title: fmt.Sprintf("Crane %d", n), Body: "forge body", State: "open",
		UpdatedAt: time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)}
	if res, err := e.applier.ApplyIssues(ctx, proj, []forge.IssueRemote{iss}); err != nil || res.Applied != 1 {
		t.Fatalf("pull-create of issue %d: %+v %v", n, res, err)
	}
	id := e.blockOf(t, proj, n)
	tx, err := e.pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	body := "body edited locally"
	if _, err := store.UpdateIssueBlock(ctx, tx, id, store.IssueUpdate{Content: &body}, e.reg.Snapshot(), []string{proj.Scope}); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("local edit of issue %d: %v", n, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit local edit: %v", err)
	}
	iss.Body = "body edited on the forge"
	iss.UpdatedAt = time.Date(2026, 11, 1, 11, 0, 0, 0, time.UTC)
	if res, err := e.applier.ApplyIssues(ctx, proj, []forge.IssueRemote{iss}); err != nil || res.Conflicts != 1 {
		t.Fatalf("pull of the remote edit of issue %d: %+v %v, want the mapping flagged", n, res, err)
	}
	return id
}

// do sends one manage action through HandleManage of h.
func (e *fcmEnv) do(t *testing.T, h *ManageHandler, ar *auth.AuthResult, action string, data map[string]any) (int, map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"action": action, "data": data})
	r := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw))
	r.Header.Set("Content-Type", "application/json")
	r = r.WithContext(context.WithValue(r.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, r)
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body, rec.Body.String()
}

type fcmPage struct {
	Success    bool             `json:"success"`
	ProjectID  string           `json:"project_id"`
	Conflicts  []forge.Conflict `json:"conflicts"`
	NextCursor *string          `json:"next_cursor"`
}

// list reads one page of proj's flagged mappings as adminA.
func (e *fcmEnv) list(t *testing.T, proj store.ProjectRow, extra map[string]any) (fcmPage, string) {
	t.Helper()
	data := map[string]any{"project_id": proj.ID}
	for k, v := range extra {
		data[k] = v
	}
	code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-list", data)
	var p fcmPage
	if code != http.StatusOK || json.Unmarshal([]byte(raw), &p) != nil || !p.Success || p.ProjectID != proj.ID {
		t.Fatalf("forge-conflict-list %v: %d %s", data, code, raw)
	}
	return p, raw
}

// resolveResult decodes the result object of a resolve answer.
func resolveResult(t *testing.T, raw string) forge.ResolveResult {
	t.Helper()
	var b struct {
		Result forge.ResolveResult `json:"result"`
	}
	if err := json.Unmarshal([]byte(raw), &b); err != nil {
		t.Fatalf("resolve answer %s: %v", raw, err)
	}
	return b.Result
}

// snapshot is what a refused call must leave untouched: the mapping rows and
// blocks of blockIDs, the sync_cursor of projectIDs, and the whole audit table.
func (e *fcmEnv) snapshot(t *testing.T, blockIDs, projectIDs []string) string {
	t.Helper()
	ctx := context.Background()
	var maps, blocks, projects, audit string
	if err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(jsonb_agg(to_jsonb(m) ORDER BY m.block_id), '[]')::text
		   FROM context_project_sync_map m WHERE m.block_id = ANY($1::uuid[])`, blockIDs).Scan(&maps); err != nil {
		t.Fatalf("snapshot mappings: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(jsonb_agg(jsonb_build_object('id', b.id, 'title', b.title, 'content', b.content,
		        'metadata', b.metadata, 'updated_at', b.updated_at) ORDER BY b.id), '[]')::text
		   FROM context_blocks b WHERE b.id = ANY($1::uuid[])`, blockIDs).Scan(&blocks); err != nil {
		t.Fatalf("snapshot blocks: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT COALESCE(jsonb_agg(jsonb_build_object('id', p.id, 'sync_cursor', p.sync_cursor) ORDER BY p.id), '[]')::text
		   FROM context_projects p WHERE p.id = ANY($1::uuid[])`, projectIDs).Scan(&projects); err != nil {
		t.Fatalf("snapshot projects: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`SELECT count(*)::text || ':' || COALESCE(md5(string_agg(to_jsonb(a)::text, '|' ORDER BY a.id)), '')
		   FROM context_settings_audit a`).Scan(&audit); err != nil {
		t.Fatalf("snapshot audit: %v", err)
	}
	return maps + "\n" + blocks + "\n" + projects + "\n" + audit
}

type fcmAuditRow struct {
	key, action, apiKey, tenant, scope string
}

// auditRows are the forge_sync audit rows of one entity key, oldest first.
func (e *fcmEnv) auditRows(t *testing.T, key string) []fcmAuditRow {
	t.Helper()
	rows, err := e.pool.Query(context.Background(),
		`SELECT entity_key, action, COALESCE(api_key_id::text, ''), COALESCE(metadata->>'tenant_id', ''), scope
		   FROM context_settings_audit WHERE entity_type = 'forge_sync' AND entity_key = $1
		  ORDER BY created_at, id`, key)
	if err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	defer rows.Close()
	var out []fcmAuditRow
	for rows.Next() {
		var r fcmAuditRow
		if err := rows.Scan(&r.key, &r.action, &r.apiKey, &r.tenant, &r.scope); err != nil {
			t.Fatalf("audit scan: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// checkAudit asserts exactly one forge_sync row for key, written by adminA.
func (e *fcmEnv) checkAudit(t *testing.T, key, action, scope string) {
	t.Helper()
	rows := e.auditRows(t, key)
	want := fcmAuditRow{key: key, action: action, apiKey: e.adminA.ApiKeyID, tenant: e.tenantA, scope: scope}
	if len(rows) != 1 || rows[0] != want {
		t.Fatalf("forge_sync audit rows of %s = %+v, want exactly %+v", key, rows, want)
	}
}

func TestForgeConflictManage(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	e := newFCMEnv(t)
	ctx := context.Background()

	t.Run("Gates", func(t *testing.T) {
		proj := e.project(t, "gates")
		id := e.flag(t, proj, 1)
		before := e.snapshot(t, []string{id}, []string{proj.ID})

		// A member of the owning tenant is stopped by the tier.
		for _, c := range []struct {
			action string
			data   map[string]any
		}{
			{"forge-conflict-list", map[string]any{"project_id": proj.ID}},
			{"forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}},
			{"forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": id}},
		} {
			if code, _, raw := e.do(t, e.h, e.memberA, c.action, c.data); code != http.StatusForbidden {
				t.Errorf("member %s: %d %s, want 403", c.action, code, raw)
			}
		}

		// The input checks answer 400 before any database access.
		for _, c := range []struct {
			name, action string
			data         map[string]any
		}{
			{"list without project_id", "forge-conflict-list", map[string]any{}},
			{"list after not a uuid", "forge-conflict-list", map[string]any{"project_id": proj.ID, "after": "not-a-uuid"}},
			{"list negative limit", "forge-conflict-list", map[string]any{"project_id": proj.ID, "limit": -1}},
			{"list unknown field", "forge-conflict-list", map[string]any{"project_id": proj.ID, "cursor": id}},
			{"resolve without block_id", "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "direction": "local"}},
			{"resolve block_id not a uuid", "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": "not-a-uuid", "direction": "local"}},
			{"resolve without direction", "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id}},
			{"resolve unknown direction", "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "both"}},
			{"resolve misspelt expect", "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local", "expected": "x"}},
			{"reset without block_id and all", "forge-stamp-reset", map[string]any{"project_id": proj.ID}},
			{"reset all false", "forge-stamp-reset", map[string]any{"project_id": proj.ID, "all": false}},
			{"reset block_id and all", "forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": id, "all": true}},
			{"reset block_id not a uuid", "forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": "1"}},
		} {
			if code, body, raw := e.do(t, e.h, e.adminA, c.action, c.data); code != http.StatusBadRequest || body["error"] == "Unknown action" {
				t.Errorf("%s: %d %s, want the action's 400", c.name, code, raw)
			}
		}

		// An unknown project is the uniform 404.
		for _, c := range []struct {
			action string
			data   map[string]any
		}{
			{"forge-conflict-list", map[string]any{"project_id": uuid.NewString()}},
			{"forge-conflict-resolve", map[string]any{"project_id": uuid.NewString(), "block_id": id, "direction": "remote"}},
			{"forge-stamp-reset", map[string]any{"project_id": uuid.NewString(), "all": true}},
		} {
			if code, body, raw := e.do(t, e.h, e.adminA, c.action, c.data); code != http.StatusNotFound || body["error"] != "Project not found" {
				t.Errorf("unknown project %s: %d %s, want the uniform 404", c.action, code, raw)
			}
		}

		// Without a wired resolver the family answers the forge family's 503.
		bare := NewManageHandler(e.pool, nil, nil, nil, nil, nil, nil, nil)
		for _, action := range []string{"forge-conflict-list", "forge-conflict-resolve", "forge-stamp-reset"} {
			code, body, raw := e.do(t, bare, e.adminA, action, map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"})
			if code != http.StatusServiceUnavailable || body["error"] != "Sync engine not enabled" {
				t.Errorf("no resolver, %s: %d %s, want 503 Sync engine not enabled", action, code, raw)
			}
		}

		if after := e.snapshot(t, []string{id}, []string{proj.ID}); after != before {
			t.Errorf("a refused call changed state:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("ListShowsBothSidesAndPaginates", func(t *testing.T) {
		proj := e.project(t, "list")
		a, b := e.flag(t, proj, 1), e.flag(t, proj, 2)
		ids := map[string]int64{a: 1, b: 2}
		first, second := a, b // the list's order: block id
		if b < a {
			first, second = b, a
		}

		all, _ := e.list(t, proj, nil)
		if len(all.Conflicts) != 2 || all.NextCursor != nil {
			t.Fatalf("default page: %d conflict(s), next %v — want both and no next page", len(all.Conflicts), all.NextCursor)
		}
		c := all.Conflicts[0]
		n := ids[c.BlockID]
		if c.BlockID != first || c.EntityKind != "issue" || c.ForgeID != n || c.Title != fmt.Sprintf("#%d Crane %d", n, n) {
			t.Errorf("first conflict %+v, want block %s, issue #%d", c, first, n)
		}
		if c.Flag == "" || c.ConflictAt == nil || c.Cell != "diverged" || c.Forge.UpdatedAt == nil {
			t.Errorf("flag %q at %v cell %q forge stamp %v — want a token, a time, the diverged cell and the forge stamp", c.Flag, c.ConflictAt, c.Cell, c.Forge.UpdatedAt)
		}
		if !c.Forge.SeenCurrent || c.Forge.Seen == nil {
			t.Fatalf("forge side %+v, want the version the flagging pull saw, current", c.Forge)
		}
		if !strings.Contains(string(c.Block.Fields), "body edited locally") ||
			!strings.Contains(string(c.Forge.Seen.Fields), "body edited on the forge") ||
			!strings.Contains(string(c.Base.Fields), "forge body") {
			t.Errorf("sides: block %s, forge %s, base %s — want the local edit, the remote edit and the pulled body", c.Block.Fields, c.Forge.Seen.Fields, c.Base.Fields)
		}
		if c.Block.Hash == c.Base.Hash || c.Forge.Seen.Hash == c.Base.Hash || c.Block.Hash == c.Forge.Seen.Hash {
			t.Errorf("hashes block %s base %s forge %s, want three different ones", c.Block.Hash, c.Base.Hash, c.Forge.Seen.Hash)
		}

		p1, _ := e.list(t, proj, map[string]any{"limit": 1})
		if len(p1.Conflicts) != 1 || p1.Conflicts[0].BlockID != first || p1.NextCursor == nil || *p1.NextCursor != first {
			t.Fatalf("page 1: %+v, want %s with next_cursor %s", p1, first, first)
		}
		p2, _ := e.list(t, proj, map[string]any{"limit": 1, "after": *p1.NextCursor})
		if len(p2.Conflicts) != 1 || p2.Conflicts[0].BlockID != second || p2.NextCursor != nil {
			t.Fatalf("page 2: %+v, want %s and no next page", p2, second)
		}
		p3, raw := e.list(t, proj, map[string]any{"limit": 1, "after": second})
		if len(p3.Conflicts) != 0 || p3.NextCursor != nil || !strings.Contains(raw, `"conflicts":[]`) {
			t.Fatalf("page after the last: %s, want conflicts [] and no next page", raw)
		}
	})

	t.Run("ResolveLocal", func(t *testing.T) {
		proj := e.project(t, "local")
		id := e.flag(t, proj, 1)
		page, _ := e.list(t, proj, nil)
		listed := page.Conflicts[0]

		code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"})
		res := resolveResult(t, raw)
		if code != http.StatusOK || !res.Changed || res.Direction != "local" || res.BlockID != id || res.Basis != "forge_seen" {
			t.Fatalf("resolve local: %d %s, want 200 changed on the forge version seen", code, raw)
		}
		var base string
		var conflict, atGone, minusInf bool
		if err := e.pool.QueryRow(ctx,
			`SELECT base_hash, conflict, conflict_at IS NULL, synced_at = '-infinity'::timestamptz
			   FROM context_project_sync_map WHERE block_id = $1::uuid`, id).Scan(&base, &conflict, &atGone, &minusInf); err != nil {
			t.Fatal(err)
		}
		if conflict || !atGone || !minusInf || base != listed.Forge.Seen.Hash {
			t.Errorf("mapping: flagged %v, conflict_at gone %v, synced_at -infinity %v, base is the forge's %v — want the flag gone and the block a push candidate over the forge version",
				conflict, atGone, minusInf, base == listed.Forge.Seen.Hash)
		}
		e.checkAudit(t, id, "resolve-local", proj.Scope)

		code, _, raw = e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"})
		if res := resolveResult(t, raw); code != http.StatusOK || res.Changed {
			t.Errorf("repeat: %d %s, want 200 changed:false", code, raw)
		}
		e.checkAudit(t, id, "resolve-local", proj.Scope)
		if p, _ := e.list(t, proj, nil); len(p.Conflicts) != 0 {
			t.Errorf("list after the resolution: %d conflict(s), want none", len(p.Conflicts))
		}
	})

	t.Run("ResolveRemote", func(t *testing.T) {
		proj := e.project(t, "remote")
		id := e.flag(t, proj, 1)
		page, _ := e.list(t, proj, nil)
		listed := page.Conflicts[0]

		code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "remote"})
		res := resolveResult(t, raw)
		if code != http.StatusOK || !res.Changed || res.Direction != "remote" || res.Leg != "issues" || res.HeldSince == nil {
			t.Fatalf("resolve remote: %d %s, want 200 changed with the issue leg held", code, raw)
		}
		var base string
		var conflict, pushGated, held bool
		if err := e.pool.QueryRow(ctx,
			`SELECT m.base_hash, m.conflict, m.synced_at = 'infinity'::timestamptz, p.sync_cursor->'holds'->'issues' IS NOT NULL
			   FROM context_project_sync_map m
			   JOIN context_projects p ON p.id = m.project_id
			  WHERE m.block_id = $1::uuid`, id).Scan(&base, &conflict, &pushGated, &held); err != nil {
			t.Fatal(err)
		}
		if conflict || !pushGated || !held || base != listed.Block.Hash {
			t.Errorf("flagged %v, push gated until a pull %v, issue leg held %v, base is the block's %v — want the flag gone, the base at the block, the push gated and the leg held",
				conflict, pushGated, held, base == listed.Block.Hash)
		}
		e.checkAudit(t, id, "resolve-remote", proj.Scope)

		code, _, raw = e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "remote"})
		if res := resolveResult(t, raw); code != http.StatusOK || res.Changed {
			t.Errorf("repeat: %d %s, want 200 changed:false", code, raw)
		}
		e.checkAudit(t, id, "resolve-remote", proj.Scope)
	})

	t.Run("ExpectNamesTheFlag", func(t *testing.T) {
		proj := e.project(t, "expect")
		id := e.flag(t, proj, 1)
		page, _ := e.list(t, proj, nil)
		listed := page.Conflicts[0]
		before := e.snapshot(t, []string{id}, []string{proj.ID})

		code, body, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve",
			map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local", "expect": "0000000000000000"})
		if code != http.StatusConflict || body["flag"] != listed.Flag || body["success"] != false {
			t.Fatalf("a stale expect: %d %s, want 409 naming the current flag %s", code, raw, listed.Flag)
		}
		if after := e.snapshot(t, []string{id}, []string{proj.ID}); after != before {
			t.Errorf("the refused resolution changed state:\nbefore %s\nafter  %s", before, after)
		}
		code, _, raw = e.do(t, e.h, e.adminA, "forge-conflict-resolve",
			map[string]any{"project_id": proj.ID, "block_id": id, "direction": "remote", "expect": listed.Flag})
		if res := resolveResult(t, raw); code != http.StatusOK || !res.Changed {
			t.Errorf("the listed flag as expect: %d %s, want 200 changed", code, raw)
		}
	})

	t.Run("PushInFlight", func(t *testing.T) {
		proj := e.project(t, "inflight")
		id := e.flag(t, proj, 1)
		conn, err := e.pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		locked, err := store.TryPushLock(ctx, conn, id)
		if err != nil || !locked {
			conn.Release()
			t.Fatalf("take the push lock: %v %v", locked, err)
		}
		before := e.snapshot(t, []string{id}, []string{proj.ID})
		code, body, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"})
		if _, err := store.UnlockPushEntity(ctx, conn, id); err != nil {
			t.Errorf("unlock: %v", err)
		}
		conn.Release()
		if code != http.StatusConflict || body["retry_after_s"] != float64(forgePushInFlightRetryAfterS) {
			t.Fatalf("resolve under a push lock: %d %s, want 409 with retry_after_s", code, raw)
		}
		if after := e.snapshot(t, []string{id}, []string{proj.ID}); after != before {
			t.Errorf("the refused resolution changed state:\nbefore %s\nafter  %s", before, after)
		}
		if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}); code != http.StatusOK {
			t.Errorf("after the lock went: %d %s, want 200", code, raw)
		}
	})

	t.Run("ForeignTenantIsNotFoundWithoutSideEffect", func(t *testing.T) {
		proj := e.project(t, "foreign-tenant")
		id := e.flag(t, proj, 1)
		before := e.snapshot(t, []string{id}, []string{proj.ID})
		for _, c := range []struct {
			action string
			data   map[string]any
		}{
			{"forge-conflict-list", map[string]any{"project_id": proj.ID}},
			{"forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}},
			{"forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "remote"}},
			{"forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": id}},
			{"forge-stamp-reset", map[string]any{"project_id": proj.ID, "all": true}},
		} {
			if code, body, raw := e.do(t, e.h, e.adminB, c.action, c.data); code != http.StatusNotFound || body["error"] != "Project not found" {
				t.Errorf("tenant-admin of B, %s %v: %d %s, want the uniform 404", c.action, c.data, code, raw)
			}
		}
		if after := e.snapshot(t, []string{id}, []string{proj.ID}); after != before {
			t.Errorf("another tenant's calls changed state:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("ForeignProjectIsNotFoundWithoutSideEffect", func(t *testing.T) {
		p1, p2 := e.project(t, "fp-one"), e.project(t, "fp-two")
		own, other := e.flag(t, p1, 1), e.flag(t, p2, 1)
		before := e.snapshot(t, []string{own, other}, []string{p1.ID, p2.ID})
		for _, data := range []map[string]any{
			{"project_id": p1.ID, "block_id": other, "direction": "local"},
			{"project_id": p1.ID, "block_id": other, "direction": "remote"},
		} {
			if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", data); code != http.StatusNotFound {
				t.Errorf("resolve %v: %d %s, want 404", data, code, raw)
			}
		}
		if code, _, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": p1.ID, "block_id": other}); code != http.StatusNotFound {
			t.Errorf("stamp-reset of another project's mapping: %d %s, want 404", code, raw)
		}
		if page, _ := e.list(t, p1, nil); len(page.Conflicts) != 1 || page.Conflicts[0].BlockID != own {
			t.Errorf("list of p1: %+v, want only its own mapping", page.Conflicts)
		}
		if after := e.snapshot(t, []string{own, other}, []string{p1.ID, p2.ID}); after != before {
			t.Errorf("calls naming another project's mapping changed state:\nbefore %s\nafter  %s", before, after)
		}
	})

	t.Run("StampReset", func(t *testing.T) {
		proj := e.project(t, "reset")
		id, id2 := e.flag(t, proj, 1), e.flag(t, proj, 2)
		stamped := func(block string) bool {
			var ok bool
			if err := e.pool.QueryRow(ctx, `SELECT forge_updated_at IS NOT NULL FROM context_project_sync_map WHERE block_id = $1::uuid`, block).Scan(&ok); err != nil {
				t.Fatal(err)
			}
			return ok
		}
		held := func() string {
			var at string
			if err := e.pool.QueryRow(ctx, `SELECT COALESCE(sync_cursor->'holds'->'issues'->>'at', '') FROM context_projects WHERE id = $1::uuid`, proj.ID).Scan(&at); err != nil {
				t.Fatal(err)
			}
			return at
		}

		code, body, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": id})
		if code != http.StatusOK || body["reset"] != float64(1) || fmt.Sprint(body["legs"]) != "[issues]" || body["project_id"] != proj.ID {
			t.Fatalf("stamp-reset of one mapping: %d %s, want reset 1 on the issue leg", code, raw)
		}
		if stamped(id) || !stamped(id2) {
			t.Errorf("forge stamps: reset mapping %v, other mapping %v — want NULL and kept", stamped(id), stamped(id2))
		}
		if at := held(); !strings.HasPrefix(at, "0001-01-01") {
			t.Errorf("issue leg hold at %q, want a full listing (year 1)", at)
		}
		e.checkAudit(t, id, "stamp-reset", proj.Scope)

		before := e.snapshot(t, []string{id, id2}, []string{proj.ID})
		if code, _, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": uuid.NewString()}); code != http.StatusNotFound {
			t.Errorf("stamp-reset of a block without a mapping: %d %s, want 404", code, raw)
		}
		if after := e.snapshot(t, []string{id, id2}, []string{proj.ID}); after != before {
			t.Errorf("the refused reset changed state:\nbefore %s\nafter  %s", before, after)
		}

		code, body, raw = e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "all": true})
		// The first mapping's stamp is NULL already: only the second is
		// written and counted.
		if code != http.StatusOK || body["reset"] != float64(1) || fmt.Sprint(body["legs"]) != "[issues]" {
			t.Fatalf("stamp-reset of the project: %d %s, want reset 1 (the mapping with a stamp)", code, raw)
		}
		if stamped(id) || stamped(id2) {
			t.Error("a mapping kept its forge stamp after the project-wide reset")
		}
		e.checkAudit(t, proj.ID, "stamp-reset", proj.Scope)
	})

	t.Run("AuditSurface", func(t *testing.T) {
		proj := e.project(t, "audit")
		id := e.flag(t, proj, 1)
		if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": id, "direction": "local"}); code != http.StatusOK {
			t.Fatalf("resolve: %d %s", code, raw)
		}
		ah := NewAuditHandler(e.pool, config.NewStore(config.Defaults()))
		_, own := auditOK(t, ah, e.adminA, "?entity=forge_sync&id="+id)
		if len(own.Rows) != 1 || own.Entity != "forge_sync" {
			t.Fatalf("tenant-admin of A: %+v, want its one forge_sync row", own)
		}
		r := own.Rows[0]
		if r.Action != "resolve-local" || r.EntityKey != id || r.Scope != proj.Scope || r.APIKeyID == nil || *r.APIKeyID != e.adminA.ApiKeyID {
			t.Errorf("row %+v, want resolve-local of %s in %s by the acting key", r, id, proj.Scope)
		}
		if string(r.Metadata["tenant_id"]) != `"`+e.tenantA+`"` {
			t.Errorf("metadata.tenant_id = %s, want %s", r.Metadata["tenant_id"], e.tenantA)
		}
		for _, q := range []string{"?entity=forge_sync", "?entity=forge_sync&id=" + id} {
			if _, foreign := auditOK(t, ah, e.adminB, q); len(foreign.Rows) != 0 {
				t.Errorf("tenant-admin of B, %s: %d row(s), want none", q, len(foreign.Rows))
			}
		}
	})
}
