//go:build integration

// W6 REST gates (design/03-workflow-api-cli.md §7-W6). Drives the project issue
// READ surface through the PRODUCTION mount chain (a chi router with
// MountProjectIssues + an AuthResult injector + a booted type registry), so the
// 200/400/401/404 probes exercise exactly what server.go wires. Proven:
//
//   - list / detail+thread / comments / board return render:'untrusted' + the
//     workflow_status/type fields;
//   - a foreign-scope project ⇒ 404 uniform (no existence oracle);
//   - a foreign/absent/malformed issue id ⇒ 404 uniform;
//   - the mount gate rejects an unauthenticated request (401) over the real chain;
//   - unmapped status (not in type config): absent from the board AND the default
//     merge list, but reachable via ?state=<unmapped> and the lossless ?sort=created;
//   - a malformed after-cursor ⇒ 400 (never a 500).
//
// Run: `go test -tags=integration ./internal/handler/ -run TestProjectIssuesW6 -count=1 -v`.
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w6IssuesDo drives ONE request through the production MountProjectIssues chain
// with ar injected (nil ar = the unauthenticated probe).
func w6IssuesDo(t *testing.T, pool *pgxpool.Pool, reg *blocktype.Registry, ar *auth.AuthResult, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	r := chi.NewRouter()
	if ar != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
			})
		})
	}
	MountProjectIssues(r, NewProjectIssuesHandler(pool, reg))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// w6SeedProject registers tenant+scope+project and returns (projectID, scope).
func w6SeedProject(t *testing.T, pool *pgxpool.Pool, slug string) (string, string) {
	t.Helper()
	ctx := context.Background()
	tn := be5SeedTenant(t, pool, slug)
	scope := slug + ":repo"
	be5SeedScope(t, pool, scope, tn)
	var pid string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid, $2, $3) RETURNING id::text`,
		tn, scope, "github:acme/"+slug).Scan(&pid); err != nil {
		t.Fatalf("seed project: %v", err)
	}
	return pid, scope
}

// w6SeedIssue inserts one issue block (given status) and returns its id.
func w6SeedIssue(t *testing.T, pool *pgxpool.Pool, scope, status, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, workflow_status)
		 VALUES ('issue', $1, 'a body', $2, 'issue', 'manual', $3) RETURNING id::text`,
		title, scope, status).Scan(&id); err != nil {
		t.Fatalf("seed issue: %v", err)
	}
	return id
}

// w6SeedComment inserts one comment block under parentID in the parent's scope.
func w6SeedComment(t *testing.T, pool *pgxpool.Pool, scope, parentID, title string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name, type_source, parent_id)
		 VALUES ('comment', $1, 'c body', $2, 'comment', 'manual', $3::uuid)`,
		title, scope, parentID); err != nil {
		t.Fatalf("seed comment: %v", err)
	}
}

func w6Member(scope string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, ReadScopes: []string{scope}}
}

func w6DecodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode %q: %v", rec.Body.String(), err)
	}
	return m
}

func TestProjectIssuesW6_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}

	pid, scope := w6SeedProject(t, pool, "w6rest")
	member := w6Member(scope)
	i1 := w6SeedIssue(t, pool, scope, "backlog", "first")
	_ = w6SeedIssue(t, pool, scope, "in-progress", "second")
	_ = w6SeedIssue(t, pool, scope, "done", "third")
	w6SeedComment(t, pool, scope, i1, "a comment")

	base := "/api/project/" + pid

	t.Run("list_untrusted_and_fields", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues")
		if rec.Code != http.StatusOK {
			t.Fatalf("list: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if body["render"] != "untrusted" {
			t.Errorf("list missing render:'untrusted'")
		}
		issues, _ := body["issues"].([]any)
		if len(issues) != 3 {
			t.Fatalf("list = %d issues, want 3 (body=%s)", len(issues), rec.Body.String())
		}
		row0, _ := issues[0].(map[string]any)
		if _, ok := row0["workflow_status"]; !ok {
			t.Errorf("list row missing workflow_status")
		}
		if _, ok := row0["type_name"]; !ok {
			t.Errorf("list row missing type_name")
		}
	})

	t.Run("state_filter", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues?state=backlog")
		body := w6DecodeBody(t, rec)
		issues, _ := body["issues"].([]any)
		if len(issues) != 1 {
			t.Fatalf("state=backlog = %d, want 1", len(issues))
		}
	})

	t.Run("detail_plus_thread", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues/"+i1)
		if rec.Code != http.StatusOK {
			t.Fatalf("detail: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if body["render"] != "untrusted" {
			t.Errorf("detail missing render:'untrusted'")
		}
		issue, _ := body["issue"].(map[string]any)
		if issue["id"] != i1 {
			t.Errorf("detail id = %v, want %s", issue["id"], i1)
		}
		comments, _ := body["comments"].([]any)
		if len(comments) != 1 {
			t.Errorf("detail comments = %d, want 1", len(comments))
		}
	})

	t.Run("comments_endpoint", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues/"+i1+"/comments")
		if rec.Code != http.StatusOK {
			t.Fatalf("comments: status %d", rec.Code)
		}
		body := w6DecodeBody(t, rec)
		comments, _ := body["comments"].([]any)
		if len(comments) != 1 {
			t.Errorf("comments = %d, want 1", len(comments))
		}
	})

	t.Run("board_columns_and_counts", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/board")
		if rec.Code != http.StatusOK {
			t.Fatalf("board: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		cols, _ := body["columns"].([]any)
		if len(cols) != 3 {
			t.Fatalf("board = %d columns, want 3 (backlog/in-progress/done)", len(cols))
		}
		total := 0
		for _, c := range cols {
			cm, _ := c.(map[string]any)
			cnt, _ := cm["count"].(float64)
			total += int(cnt)
		}
		if total != 3 {
			t.Errorf("board total count = %d, want 3", total)
		}
	})

	t.Run("foreign_scope_project_404", func(t *testing.T) {
		// A member without the project scope in ReadScopes: every read ⇒ 404 uniform.
		outsider := w6Member("someone:else")
		for _, path := range []string{"/issues", "/issues/" + i1, "/issues/" + i1 + "/comments", "/board"} {
			rec := w6IssuesDo(t, pool, reg, outsider, http.MethodGet, base+path)
			if rec.Code != http.StatusNotFound {
				t.Errorf("foreign %s: status %d, want 404", path, rec.Code)
			}
		}
	})

	t.Run("foreign_and_malformed_issue_id_404", func(t *testing.T) {
		// A block id from another scope (seed a foreign issue) ⇒ 404 uniform.
		_, otherScope := w6SeedProject(t, pool, "w6other")
		foreign := w6SeedIssue(t, pool, otherScope, "backlog", "foreign")
		recForeign := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues/"+foreign)
		recAbsent := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues/019f9999-0000-7000-9000-000000000000")
		recMalformed := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues/not-a-uuid")
		if recForeign.Code != http.StatusNotFound || recAbsent.Code != http.StatusNotFound || recMalformed.Code != http.StatusNotFound {
			t.Errorf("issue-id 404: foreign=%d absent=%d malformed=%d, want all 404",
				recForeign.Code, recAbsent.Code, recMalformed.Code)
		}
		// No oracle: foreign and absent share the SAME body.
		if recForeign.Body.String() != recAbsent.Body.String() {
			t.Errorf("existence oracle: foreign body %q != absent body %q", recForeign.Body.String(), recAbsent.Body.String())
		}
	})

	t.Run("mount_gate_unauthenticated_401", func(t *testing.T) {
		rec := w6IssuesDo(t, pool, reg, nil, http.MethodGet, base+"/issues")
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("no-auth list: status %d, want 401", rec.Code)
		}
	})

	t.Run("malformed_cursor_400", func(t *testing.T) {
		// '!!!' survives URL parsing but is not a valid base64url token ⇒ decode error.
		rec := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues?after=!!!bad")
		if rec.Code != http.StatusBadRequest {
			t.Errorf("malformed after: status %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("unmapped_status_behavior", func(t *testing.T) {
		// Seed an issue with a status NOT in the type config (weird).
		weird := w6SeedIssue(t, pool, scope, "weird", "unmapped")

		// Board: config statuses only — no 'weird' column, total unchanged (3).
		recBoard := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/board")
		bboard := w6DecodeBody(t, recBoard)
		for _, c := range bboard["columns"].([]any) {
			if c.(map[string]any)["status"] == "weird" {
				t.Errorf("board surfaced an unmapped 'weird' column")
			}
		}

		// Default merge list (config-set): does NOT include the weird issue.
		recList := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues")
		if listHasID(t, recList, weird) {
			t.Errorf("default list surfaced the unmapped issue (should be config-bound)")
		}

		// Explicit ?state=weird: single-status query DOES return it.
		recWeird := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues?state=weird")
		if !listHasID(t, recWeird, weird) {
			t.Errorf("?state=weird did not return the unmapped issue")
		}

		// Lossless ?sort=created (no state): surfaces ALL statuses incl. unmapped.
		recCreated := w6IssuesDo(t, pool, reg, member, http.MethodGet, base+"/issues?sort=created")
		if !listHasID(t, recCreated, weird) {
			t.Errorf("?sort=created did not surface the unmapped issue (lossless traversal broken)")
		}
	})
}

// listHasID reports whether the list response contains an issue with id.
func listHasID(t *testing.T, rec *httptest.ResponseRecorder, id string) bool {
	t.Helper()
	var body struct {
		Issues []map[string]any `json:"issues"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode list %q: %v", rec.Body.String(), err)
	}
	for _, r := range body.Issues {
		if r["id"] == id {
			return true
		}
	}
	return false
}
