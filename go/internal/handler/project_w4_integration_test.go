//go:build integration

// W4 gates (design/03-workflow-api-cli.md §7-W4): the project register — schema
// 079 + MountProject (list/create/get/patch/delete) + fail-closed quota +
// tenant_id targeting + forge.api_base SSRF validation + the K14 PruneTenant
// drain. Every mutation gate is proven RED-then-GREEN against the PRODUCTION
// mount chain (a chi router with MountProject + an AuthResult injector), so the
// 401/403/404/422/429 probes exercise exactly what server.go wires.
//
//	go test -tags=integration ./internal/handler/ -run TestProjectW4 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w4Do drives ONE request through the production MountProject chain with ar
// injected (nil ar = the unauthenticated probe: no AuthResult in context).
func w4Do(t *testing.T, pool *pgxpool.Pool, ar *auth.AuthResult, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(raw)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	r := chi.NewRouter()
	if ar != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
			})
		})
	}
	MountProject(r, NewProjectHandler(pool))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// w4Field pulls the nested project.<key> string out of a response body.
func w4ProjectField(t *testing.T, rec *httptest.ResponseRecorder, key string) string {
	t.Helper()
	var resp struct {
		Project map[string]any `json:"project"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal %q: %v", rec.Body.String(), err)
	}
	s, _ := resp.Project[key].(string)
	return s
}

// w4TenantAdmin builds a tenant-admin AuthResult with an explicit read set (so a
// GET after create can see the freshly created scope).
func w4TenantAdmin(tenantID string, readScopes ...string) *auth.AuthResult {
	return &auth.AuthResult{
		IsValid: true, IsAdmin: false,
		TenantID: tenantID, TenantRole: auth.RoleAdmin,
		ReadScopes: readScopes,
	}
}

func TestProjectW4_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// ── Happy path: compound create → GET → list ────────────────────────────
	t.Run("CreateGetList", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4happy")
		admin := w4TenantAdmin(tn, "w4happy:repo")
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:acme/repo", "scope": "repo", "display_name": "Repo",
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status %d, want 200 (body=%s)", rec.Code, rec.Body.String())
		}
		if got := w4ProjectField(t, rec, "scope"); got != "w4happy:repo" {
			t.Fatalf("scope = %q, want w4happy:repo (body=%s)", got, rec.Body.String())
		}
		id := w4ProjectField(t, rec, "id")
		if id == "" {
			t.Fatalf("no id in create response (body=%s)", rec.Body.String())
		}
		// The scope was actually registered under this tenant (compound create).
		var scopeTenant string
		if err := pool.QueryRow(ctx,
			`SELECT tenant_id::text FROM context_tenant_scopes WHERE scope=$1`, "w4happy:repo").Scan(&scopeTenant); err != nil {
			t.Fatalf("scope lookup: %v", err)
		}
		if scopeTenant != tn {
			t.Fatalf("scope owner = %s, want %s", scopeTenant, tn)
		}
		// GET detail (member scope-read).
		getRec := w4Do(t, pool, admin, http.MethodGet, "/api/project/"+id, nil)
		if getRec.Code != http.StatusOK || w4ProjectField(t, getRec, "id") != id {
			t.Fatalf("get: status %d id-mismatch (body=%s)", getRec.Code, getRec.Body.String())
		}
		// List + ?identity= resolution.
		listRec := w4Do(t, pool, admin, http.MethodGet, "/api/project?identity=github:acme/repo", nil)
		var listResp struct {
			Projects []map[string]any `json:"projects"`
		}
		if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
			t.Fatalf("list unmarshal: %v", err)
		}
		if len(listResp.Projects) != 1 {
			t.Fatalf("identity list = %d projects, want 1 (body=%s)", len(listResp.Projects), listRec.Body.String())
		}
	})

	// ── Idempotency: re-init identical identity ⇒ 200, no duplicate ──────────
	t.Run("IdempotentReInit", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4idem")
		admin := w4TenantAdmin(tn, "w4idem:repo")
		body := map[string]any{"identity": "github:acme/idem", "scope": "repo"}
		r1 := w4Do(t, pool, admin, http.MethodPost, "/api/project", body)
		if r1.Code != http.StatusOK {
			t.Fatalf("first create: %d (body=%s)", r1.Code, r1.Body.String())
		}
		id1 := w4ProjectField(t, r1, "id")
		r2 := w4Do(t, pool, admin, http.MethodPost, "/api/project", body)
		if r2.Code != http.StatusOK {
			t.Fatalf("re-init: status %d, want idempotent 200 (body=%s)", r2.Code, r2.Body.String())
		}
		if id2 := w4ProjectField(t, r2, "id"); id2 != id1 {
			t.Fatalf("re-init returned a DIFFERENT id (%s != %s) — not idempotent", id2, id1)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_projects WHERE tenant_id=$1::uuid AND identity=$2`, tn, "github:acme/idem").Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("re-init produced %d rows, want exactly 1 (duplicate)", n)
		}
	})

	// ── Foreign / unknown / malformed id ⇒ 404 uniform (no oracle) ──────────
	t.Run("ForeignProject404Uniform", func(t *testing.T) {
		// A project owned by tenant A.
		tnA := be5SeedTenant(t, pool, "w4owner")
		adminA := w4TenantAdmin(tnA, "w4owner:repo")
		rec := w4Do(t, pool, adminA, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:a/secret", "scope": "repo",
		})
		id := w4ProjectField(t, rec, "id")
		// Tenant B: no read scope for A's project, no ownership.
		tnB := be5SeedTenant(t, pool, "w4other")
		adminB := w4TenantAdmin(tnB) // empty ReadScopes

		get := w4Do(t, pool, adminB, http.MethodGet, "/api/project/"+id, nil)
		patch := w4Do(t, pool, adminB, http.MethodPatch, "/api/project/"+id, map[string]any{"display_name": "hijack"})
		del := w4Do(t, pool, adminB, http.MethodDelete, "/api/project/"+id, nil)
		unknown := w4Do(t, pool, adminB, http.MethodGet, "/api/project/00000000-0000-0000-0000-000000000000", nil)
		malformed := w4Do(t, pool, adminB, http.MethodGet, "/api/project/not-a-uuid", nil)

		for name, r := range map[string]*httptest.ResponseRecorder{
			"foreign-get": get, "foreign-patch": patch, "foreign-delete": del,
			"unknown-uuid": unknown, "malformed-uuid": malformed,
		} {
			if r.Code != http.StatusNotFound {
				t.Fatalf("%s: status %d, want 404 uniform (body=%s)", name, r.Code, r.Body.String())
			}
		}
		// The foreign project still exists (B could not delete it).
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_projects WHERE id=$1::uuid`, id).Scan(&n); err != nil {
			t.Fatalf("survivor count: %v", err)
		}
		if n != 1 {
			t.Fatalf("foreign DELETE removed the row (n=%d) — isolation breach", n)
		}
	})

	// ── Quota fail-closed: create over max_scopes ⇒ 429 (RED w/ maxScopes=-1) ─
	t.Run("QuotaExceeded429", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4quota")
		be5SeedScope(t, pool, "w4quota:existing", tn) // count = 1
		be5SetMaxScopes(t, pool, tn, 1)               // cap = 1 → next assign blocked
		admin := w4TenantAdmin(tn, "w4quota:repo")
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:acme/quota", "scope": "repo",
		})
		// A -1 (cap-free) passthrough would make this a 200 — the RED baseline.
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("over-quota create: status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
		}
		// And no orphan scope leaked from the blocked create.
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_tenant_scopes WHERE scope=$1`, "w4quota:repo").Scan(&n); err != nil {
			t.Fatalf("orphan scope count: %v", err)
		}
		if n != 0 {
			t.Fatalf("blocked create leaked a scope (n=%d)", n)
		}
	})

	// ── Fail-closed on a DB fault: create NEVER silently succeeds ⇒ 500 ──────
	// A closed pool makes every read fail. The create must 500 (fail-closed),
	// never fall through to a silent unlimited create — the same guard shape the
	// TenantLimits load carries (a transient TenantLimits error is a 500, never
	// silent unlimited; §4.2, mirror of handleScopeCreate tenant_manage.go:638-642,
	// which is not deterministically isolable in-process since GetTenant and
	// TenantLimits read the same row).
	t.Run("FailClosedOnDBFault500", func(t *testing.T) {
		faultPool := testdb.SetupTestDB(t)
		tn := be5SeedTenant(t, faultPool, "w4fault")
		admin := w4TenantAdmin(tn, "w4fault:repo")
		faultPool.Close()
		rec := w4Do(t, faultPool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:acme/fault", "scope": "repo",
		})
		if rec.Code != http.StatusInternalServerError {
			t.Fatalf("create on closed pool: status %d, want 500 (fail-closed; body=%s)", rec.Code, rec.Body.String())
		}
	})

	// ── member ⇒ 403; tenant-admin with tenant_id field ⇒ 403 (self-escalation) ─
	t.Run("MemberAndSelfEscalation403", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4roles")
		member := be5Member(tn)
		mrec := w4Do(t, pool, member, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/m", "scope": "repo"})
		if mrec.Code != http.StatusForbidden {
			t.Fatalf("member create: status %d, want 403 (body=%s)", mrec.Code, mrec.Body.String())
		}
		// A tenant-admin passing tenant_id is a foreign-target self-escalation ⇒ 403.
		admin := w4TenantAdmin(tn)
		erec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:a/esc", "scope": "repo", "tenant_id": tn,
		})
		if erec.Code != http.StatusForbidden {
			t.Fatalf("tenant-admin w/ tenant_id: status %d, want 403 (body=%s)", erec.Code, erec.Body.String())
		}
	})

	// ── PATCH forbidden fields ⇒ 422 ────────────────────────────────────────
	t.Run("PatchForbiddenFields422", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4patch")
		admin := w4TenantAdmin(tn, "w4patch:repo")
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/p", "scope": "repo"})
		id := w4ProjectField(t, rec, "id")
		for _, k := range []string{"webhook_secret_ref", "scope", "tenant_id"} {
			pr := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{k: "x"})
			if pr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH %s: status %d, want 422 (body=%s)", k, pr.Code, pr.Body.String())
			}
		}
	})

	// ── PATCH forge.api_base SSRF deny-list ⇒ 422; valid https public ⇒ 200 ──
	t.Run("PatchForgeAPIBaseSSRF", func(t *testing.T) {
		tn := be5SeedTenant(t, pool, "w4ssrf")
		admin := w4TenantAdmin(tn, "w4ssrf:repo")
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/s", "scope": "repo"})
		id := w4ProjectField(t, rec, "id")
		bad := []string{
			"http://10.0.0.1",                // scheme + RFC1918
			"https://10.0.0.1",               // RFC1918
			"https://127.0.0.1",              // loopback
			"https://169.254.169.254/latest", // link-local cloud metadata
			"https://localhost",              // loopback name
			"http://ghe.example.com/api/v3",  // non-https scheme
		}
		for _, ab := range bad {
			pr := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{
				"forge": map[string]any{"kind": "github", "api_base": ab},
			})
			if pr.Code != http.StatusUnprocessableEntity {
				t.Fatalf("PATCH api_base=%q: status %d, want 422 (body=%s)", ab, pr.Code, pr.Body.String())
			}
		}
		// A legitimate GitHub-Enterprise https URL with a public hostname passes.
		ok := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{
			"forge": map[string]any{"kind": "github", "api_base": "https://ghe.example.com/api/v3"},
		})
		if ok.Code != http.StatusOK {
			t.Fatalf("PATCH legit GHE api_base: status %d, want 200 (body=%s)", ok.Code, ok.Body.String())
		}
	})

	// ── Mount-gate negative probe over the production chain (no auth) ────────
	t.Run("MountGateNegativeProbe", func(t *testing.T) {
		// RequireMember read routes ⇒ 401; RequireAdminOrTenantAdmin write ⇒ 403.
		if r := w4Do(t, pool, nil, http.MethodGet, "/api/project", nil); r.Code != http.StatusUnauthorized {
			t.Fatalf("unauth GET: status %d, want 401 (body=%s)", r.Code, r.Body.String())
		}
		if r := w4Do(t, pool, nil, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/x", "scope": "repo"}); r.Code != http.StatusForbidden {
			t.Fatalf("unauth POST: status %d, want 403 (body=%s)", r.Code, r.Body.String())
		}
	})
}

// ── Store-level gates: atomicity + K14 prune drain ──────────────────────────

func TestProjectW4_CreateAtomicRollback_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn := be5SeedTenant(t, pool, "w4atom")

	// A created_by pointing at a NON-EXISTENT key forces the register INSERT
	// (step 3) to fail on the context_api_keys FK (23503) AFTER the scope was
	// assigned (step 2) — the atomicity fault. In the single-tx CreateProject the
	// scope MUST roll back with it. A two-call (pool-assign then pool-insert)
	// implementation would leave the scope committed — this is the RED baseline.
	_, _, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID:  tn,
		ScopeName: "w4atom:repo",
		Identity:  "github:a/atom",
		CreatedBy: "00000000-0000-0000-0000-000000000000", // valid UUID, no such key
	})
	if err == nil {
		t.Fatalf("create with bogus created_by unexpectedly succeeded — FK not enforced")
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_tenant_scopes WHERE scope=$1`, "w4atom:repo").Scan(&n); err != nil {
		t.Fatalf("scope count: %v", err)
	}
	if n != 0 {
		t.Fatalf("ATOMICITY BREACH: scope survived a failed compound create (n=%d) — the insert-fault did not roll the scope back", n)
	}
}

func TestProjectW4_PruneTenantDrainsProjects_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn := be5SeedTenant(t, pool, "w4prune")

	// Seed a project (real compound create) + a sync-run row.
	row, created, err := store.CreateProject(ctx, pool, store.CreateProjectParams{
		TenantID:  tn,
		ScopeName: "w4prune:repo",
		Identity:  "github:a/prune",
	})
	if err != nil || !created {
		t.Fatalf("seed project: created=%v err=%v", created, err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_project_sync_runs (project_id) VALUES ($1::uuid)`, row.ID); err != nil {
		t.Fatalf("seed sync run: %v", err)
	}

	// PruneTenant MUST drain context_projects + context_project_sync_runs (K14).
	// Without the drain step the tenant delete would 23503 against the surviving
	// project (FK tenant_id NO ACTION) — the RED baseline against the pre-079 prune.
	if err := store.PruneTenant(ctx, pool, tn); err != nil {
		t.Fatalf("PruneTenant: %v (K14 drain missing ⇒ FK violation on tenant delete)", err)
	}
	var projects, runs int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_projects WHERE tenant_id=$1::uuid`, tn).Scan(&projects); err != nil {
		t.Fatalf("project count: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_project_sync_runs`).Scan(&runs); err != nil {
		t.Fatalf("sync run count: %v", err)
	}
	if projects != 0 || runs != 0 {
		t.Fatalf("PruneTenant left %d projects + %d sync_runs of the tenant (want 0/0)", projects, runs)
	}
}
