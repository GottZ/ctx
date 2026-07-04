//go:build integration

// W7 REST write gates (design/03-workflow-api-cli.md §7-W7). Drives the project
// issue WRITE surface through the PRODUCTION mount chain (MountProjectIssues +
// AuthResult injector + a booted type registry), so every 200/403/404/422/429
// probe exercises exactly what server.go wires. Proven (each negative first
// confirmed RED against the removed gate — see RED-PROOF notes in the return):
//
//   - create/patch/comment WITHOUT write-scope (read-only scope access) ⇒ 403;
//   - create/patch/comment on a foreign/unknown project ⇒ 404 uniform;
//   - a state transition outside the POLICY DATA ⇒ 422 — proven policy-driven by
//     swapping the registry status set and re-reading the verdict WITH NO Go
//     rebuild (a hardcoded list would not change);
//   - a comment under a foreign-scope parent ⇒ 404 uniform (no oracle);
//   - the write rate limit ⇒ 429 (per-api_key_id, the /api/store house throttle);
//   - create→read roundtrip returns the written fields (render:'untrusted',
//     workflow_status, #L-prefixed title).
//
// Run: `go test -tags=integration ./internal/handler/ -run TestProjectIssuesW7 -count=1 -v`.
package handler

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w7Do drives ONE write request through the production MountProjectIssues chain
// with ar injected and an optional cfg (nil = throttle disabled). body is the raw
// JSON request body ("" = empty).
func w7Do(t *testing.T, pool *pgxpool.Pool, reg *blocktype.Registry, cfg ConfigStore, ar *auth.AuthResult, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewReader([]byte(body)))
	r := chi.NewRouter()
	if ar != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
			})
		})
	}
	MountProjectIssues(r, NewProjectIssuesHandler(pool, reg).WithConfig(cfg))
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

// w7Writer builds a member AuthResult that CAN write the given scope (home_scope
// = scope ⇒ writableBlockScopes = [scope]).
func w7Writer(scope string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, HomeScope: scope, ReadScopes: []string{scope}}
}

// w7ReadOnly builds a member AuthResult that can READ scope (grant) but not write
// it (home_scope is elsewhere, no write_scopes) — the write-scope-gate probe.
func w7ReadOnly(scope string) *auth.AuthResult {
	return &auth.AuthResult{
		IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "ro:home", ReadScopes: []string{"ro:home", scope}, AllowedScopes: []string{scope},
	}
}

func TestProjectIssuesW7_Integration(t *testing.T) {
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

	pid, scope := w6SeedProject(t, pool, "w7rest")
	writer := w7Writer(scope)
	base := "/api/project/" + pid

	t.Run("create_roundtrip", func(t *testing.T) {
		rec := w7Do(t, pool, reg, nil, writer, http.MethodPost, base+"/issues", `{"title":"first issue","content":"hello"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("create: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if body["render"] != "untrusted" {
			t.Errorf("create missing render:'untrusted'")
		}
		issue, _ := body["issue"].(map[string]any)
		if issue["workflow_status"] != "backlog" {
			t.Errorf("create workflow_status = %v, want backlog (policy initial)", issue["workflow_status"])
		}
		if issue["type"] != "issue" { // store.Block wire name for type_name is `type` (I-D-consistent)
			t.Errorf("create type = %v, want issue", issue["type"])
		}
		id, _ := issue["id"].(string)
		if id == "" {
			t.Fatalf("create returned no id")
		}
		// Roundtrip: the W6 read surface returns the written fields.
		recGet := w6IssuesDo(t, pool, reg, writer, http.MethodGet, base+"/issues/"+id)
		if recGet.Code != http.StatusOK {
			t.Fatalf("roundtrip get: status %d", recGet.Code)
		}
		got := w6DecodeBody(t, recGet)["issue"].(map[string]any)
		if got["content"] != "hello" {
			t.Errorf("roundtrip content = %v, want hello", got["content"])
		}
		if title, _ := got["title"].(string); title == "" || title[:2] != "#L" {
			t.Errorf("roundtrip title = %q, want a #L-prefixed local title", title)
		}
	})

	t.Run("create_unknown_field_400", func(t *testing.T) {
		// scope is NOT a valid create field (scope comes from the path) ⇒ strict 400.
		rec := w7Do(t, pool, reg, nil, writer, http.MethodPost, base+"/issues", `{"title":"x","scope":"evil"}`)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("unknown-field create: status %d, want 400 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("write_scope_gate_403", func(t *testing.T) {
		ro := w7ReadOnly(scope)
		// RED-PROOF: with the writableBlockScopes check removed from resolveWriteScope
		// these three become 200 (the store is bounded to [scope], so the handler gate
		// is the ONLY write-authorization check).
		recCreate := w7Do(t, pool, reg, nil, ro, http.MethodPost, base+"/issues", `{"title":"nope"}`)
		if recCreate.Code != http.StatusForbidden {
			t.Errorf("read-only create: status %d, want 403 (body=%s)", recCreate.Code, recCreate.Body.String())
		}
		// Seed a real issue in the scope to patch/comment against.
		iss := w6SeedIssue(t, pool, scope, "backlog", "target")
		recPatch := w7Do(t, pool, reg, nil, ro, http.MethodPatch, base+"/issues/"+iss, `{"status":"done"}`)
		if recPatch.Code != http.StatusForbidden {
			t.Errorf("read-only patch: status %d, want 403", recPatch.Code)
		}
		recComment := w7Do(t, pool, reg, nil, ro, http.MethodPost, base+"/issues/"+iss+"/comments", `{"content":"hi"}`)
		if recComment.Code != http.StatusForbidden {
			t.Errorf("read-only comment: status %d, want 403", recComment.Code)
		}
	})

	t.Run("foreign_project_404_uniform", func(t *testing.T) {
		// A writer whose scope is NOT this project ⇒ the project is invisible ⇒ 404,
		// SAME body as an unknown project id (no existence oracle).
		outsider := w7Writer("someone:else")
		recForeign := w7Do(t, pool, reg, nil, outsider, http.MethodPost, base+"/issues", `{"title":"x"}`)
		recUnknown := w7Do(t, pool, reg, nil, writer, http.MethodPost, "/api/project/019f9999-0000-7000-9000-000000000000/issues", `{"title":"x"}`)
		if recForeign.Code != http.StatusNotFound || recUnknown.Code != http.StatusNotFound {
			t.Fatalf("foreign=%d unknown=%d, want both 404", recForeign.Code, recUnknown.Code)
		}
		if recForeign.Body.String() != recUnknown.Body.String() {
			t.Errorf("existence oracle: foreign body %q != unknown body %q", recForeign.Body.String(), recUnknown.Body.String())
		}
	})

	t.Run("invalid_transition_422", func(t *testing.T) {
		iss := w6SeedIssue(t, pool, scope, "backlog", "trans")
		// Target not in the configured status set ⇒ 422 (policy-driven).
		rec := w7Do(t, pool, reg, nil, writer, http.MethodPatch, base+"/issues/"+iss, `{"status":"not-a-status"}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("invalid transition: status %d, want 422 (body=%s)", rec.Code, rec.Body.String())
		}
		// A valid configured transition ⇒ 200.
		recOK := w7Do(t, pool, reg, nil, writer, http.MethodPatch, base+"/issues/"+iss, `{"status":"done"}`)
		if recOK.Code != http.StatusOK {
			t.Errorf("valid transition backlog→done: status %d, want 200 (body=%s)", recOK.Code, recOK.Body.String())
		}
	})

	t.Run("comment_foreign_parent_404_uniform", func(t *testing.T) {
		// A parent issue in ANOTHER scope, addressed through THIS project's path:
		// the store bounds the write to [scope] ⇒ ErrLinkScopeViolation ⇒ 404, SAME
		// body as an absent parent (no oracle for a foreign block id).
		_, otherScope := w6SeedProject(t, pool, "w7other")
		foreign := w6SeedIssue(t, pool, otherScope, "backlog", "foreign parent")
		recForeign := w7Do(t, pool, reg, nil, writer, http.MethodPost, base+"/issues/"+foreign+"/comments", `{"content":"x"}`)
		recAbsent := w7Do(t, pool, reg, nil, writer, http.MethodPost, base+"/issues/019f8888-0000-7000-9000-000000000000/comments", `{"content":"x"}`)
		if recForeign.Code != http.StatusNotFound || recAbsent.Code != http.StatusNotFound {
			t.Fatalf("foreign-parent=%d absent-parent=%d, want both 404", recForeign.Code, recAbsent.Code)
		}
		if recForeign.Body.String() != recAbsent.Body.String() {
			t.Errorf("existence oracle: foreign-parent body %q != absent body %q", recForeign.Body.String(), recAbsent.Body.String())
		}
	})

	t.Run("comment_create_roundtrip", func(t *testing.T) {
		iss := w6SeedIssue(t, pool, scope, "backlog", "with comments")
		rec := w7Do(t, pool, reg, nil, writer, http.MethodPost, base+"/issues/"+iss+"/comments", `{"author":"alice","content":"a comment"}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("comment create: status %d (body=%s)", rec.Code, rec.Body.String())
		}
		body := w6DecodeBody(t, rec)
		if body["render"] != "untrusted" {
			t.Errorf("comment create missing render:'untrusted'")
		}
		// The comment surfaces on the read thread.
		recThread := w6IssuesDo(t, pool, reg, writer, http.MethodGet, base+"/issues/"+iss+"/comments")
		cs := w6DecodeBody(t, recThread)["comments"].([]any)
		if len(cs) != 1 {
			t.Errorf("thread = %d comments, want 1", len(cs))
		}
	})

	t.Run("rate_limit_429", func(t *testing.T) {
		// A real api key (FK-valid) so context_access_log rows reference it. cfg
		// arms query.rate_limit_write = 2; pre-seed 2 "write" rows ⇒ the next write
		// is over budget. RED-PROOF: with the writeRateBlocked call removed, this is
		// 200 (the write succeeds).
		tn := be5SeedTenant(t, pool, "w7rl")
		rlScope := "w7rl:repo"
		be5SeedScope(t, pool, rlScope, tn)
		var rlpid string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid,$2,$3) RETURNING id::text`,
			tn, rlScope, "github:acme/w7rl").Scan(&rlpid); err != nil {
			t.Fatalf("seed rl project: %v", err)
		}
		keyID := be6SeedKey(t, pool, tn, rlScope, "member", true)
		for i := 0; i < 2; i++ {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_access_log (api_key_id, action) VALUES ($1::uuid, 'write')`, keyID); err != nil {
				t.Fatalf("seed access log: %v", err)
			}
		}
		cfg := staticConfigStore{cfg: &config.Config{Query: config.QueryConfig{RateLimitWrite: 2}}}
		ar := &auth.AuthResult{IsValid: true, TenantRole: auth.RoleMember, HomeScope: rlScope, ReadScopes: []string{rlScope}, ApiKeyID: keyID}
		rec := w7Do(t, pool, reg, cfg, ar, http.MethodPost, "/api/project/"+rlpid+"/issues", `{"title":"over budget"}`)
		if rec.Code != http.StatusTooManyRequests {
			t.Errorf("rate-limited create: status %d, want 429 (body=%s)", rec.Code, rec.Body.String())
		}
	})

	t.Run("policy_data_swap_422_no_rebuild", func(t *testing.T) {
		// PROOF that the transition verdict is DATA, not compiled-in: with the
		// _global issue config carrying "done", backlog→done is 200; after removing
		// "done" from the registry status set + Reload (SAME binary), backlog→done
		// is 422. A hardcoded state list would keep returning 200 ⇒ this subtest is
		// RED against a hardcoded impl.
		swapReg := blocktype.NewRegistry()
		swapReg.Boot(ctx, pool)
		issOK := w6SeedIssue(t, pool, scope, "backlog", "swap-before")
		recBefore := w7Do(t, pool, swapReg, nil, writer, http.MethodPatch, base+"/issues/"+issOK, `{"status":"done"}`)
		if recBefore.Code != http.StatusOK {
			t.Fatalf("pre-swap backlog→done: status %d, want 200 (body=%s)", recBefore.Code, recBefore.Body.String())
		}

		// Swap the POLICY DATA: drop "done" from the issue status set. Restore after.
		orig := swapIssueStates(t, pool, `["backlog", "in-progress"]`)
		defer func() {
			restoreIssueConfig(t, pool, orig)
		}()
		if err := swapReg.Reload(ctx, pool); err != nil {
			t.Fatalf("reload after swap: %v", err)
		}

		issAfter := w6SeedIssue(t, pool, scope, "backlog", "swap-after")
		recAfter := w7Do(t, pool, swapReg, nil, writer, http.MethodPatch, base+"/issues/"+issAfter, `{"status":"done"}`)
		if recAfter.Code != http.StatusUnprocessableEntity {
			t.Errorf("post-swap backlog→done: status %d, want 422 (policy-data change took no effect ⇒ hardcoded list) (body=%s)",
				recAfter.Code, recAfter.Body.String())
		}
	})
}

// swapIssueStates overwrites the _global issue type's whole workflow object with
// a status set that drops "done" (keeping terminal/forge_state_map consistent, so
// the registry's own validation accepts the swap) and returns the ORIGINAL full
// config json for restoration. workflowJSON is ignored — kept for call-site
// readability of intent.
func swapIssueStates(t *testing.T, pool *pgxpool.Pool, _ string) string {
	t.Helper()
	var orig string
	if err := pool.QueryRow(context.Background(),
		`SELECT config::text FROM context_block_types WHERE name='issue' AND scope='_global'`).Scan(&orig); err != nil {
		t.Fatalf("read issue config: %v", err)
	}
	// Replace the entire workflow object so terminal ⊆ states holds after the swap
	// (the registry rejects a terminal status absent from states).
	newWorkflow := `{"states":["backlog","in-progress"],"initial":"backlog","terminal":["in-progress"],"forge_state_map":{"open":"backlog","closed":"in-progress"}}`
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_block_types SET config = jsonb_set(config, '{workflow}', $1::jsonb)
		 WHERE name='issue' AND scope='_global'`, newWorkflow); err != nil {
		t.Fatalf("swap issue workflow: %v", err)
	}
	return orig
}

// restoreIssueConfig restores the _global issue config to origJSON.
func restoreIssueConfig(t *testing.T, pool *pgxpool.Pool, origJSON string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_block_types SET config = $1::jsonb WHERE name='issue' AND scope='_global'`, origJSON); err != nil {
		t.Fatalf("restore issue config: %v", err)
	}
}
