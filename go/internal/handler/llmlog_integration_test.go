//go:build integration

// Privacy + filter probes for GET /api/llmlog against a real PG18
// testcontainer. The endpoint must NEVER return the M025 body columns
// (request_system/request_user/response_content = full prompts incl. block
// content, a shadow corpus) and must cap the error detail (the raw error can
// embed up to 1 KiB of provider body with prompt fragments).
//
//	go test -tags=integration ./internal/handler/ -run TestLLMLog -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// serveLLMLog runs the handler as a SERVER-admin (sees every tenant's rows) —
// the privacy/filter probes are tenant-agnostic. Per-tenant scoping is pinned
// separately in TestLLMLogTenantScoped.
func serveLLMLog(t *testing.T, h *LLMLogHandler, query string) *httptest.ResponseRecorder {
	return serveLLMLogAs(t, h, query, &auth.AuthResult{IsValid: true, IsAdmin: true})
}

// serveLLMLogAs runs the handler with a specific AuthResult in the request
// context (the RequireAdminOrTenantAdmin gate normally puts it there).
func serveLLMLogAs(t *testing.T, h *LLMLogHandler, query string, ar *auth.AuthResult) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/llmlog"+query, nil)
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleLLMLog(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/llmlog%s: status %d, body %s", query, rec.Code, rec.Body.String())
	}
	return rec
}

// TestLLMLogNoPrompts is the core privacy guard: seeded prompt/response bodies
// never appear in the response, while the telemetry row IS returned. Adding the
// body columns to the SELECT list (or struct) turns this red.
func TestLLMLogNoPrompts(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	_, err := pool.Exec(ctx, `INSERT INTO context_llm_log
		(pipeline, model, host, duration_ms, prompt_tokens, completion_tokens,
		 request_system, request_user, response_content, backend_name)
		VALUES ('query-synthesize','qwen3.6-27b','herbert',8123,9800,412,
		        $1,$2,$3,'herbert-chat')`,
		"SYS-SECRET-MARKER", "USER-SECRET-MARKER", "RESP-SECRET-MARKER")
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	body := serveLLMLog(t, h, "").Body.String()

	for _, marker := range []string{"SYS-SECRET-MARKER", "USER-SECRET-MARKER", "RESP-SECRET-MARKER"} {
		if strings.Contains(body, marker) {
			t.Errorf("response leaked prompt body %q (the SELECT list grew body columns): %s", marker, body)
		}
	}
	// Proof it actually returned the row (not just an empty result hiding a bug).
	if !strings.Contains(body, "query-synthesize") || !strings.Contains(body, "herbert-chat") {
		t.Errorf("telemetry row missing from response: %s", body)
	}
}

// TestLLMLogDispatchColumns is the MW12 llmlog exposure gate: the three 091
// dispatch-telemetry columns (queue_wait_ms/dispatch_class/dispatch_abort) reach
// the list response — and the body-exclusion invariant stays intact (no prompt
// marker). Dropping any column from the SELECT/scan turns this red.
func TestLLMLogDispatchColumns(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// A wired background row: queue_wait_ms 0 is a REAL measurement (not NULL),
	// dispatch_class background, and a K9 rejection kind.
	_, err := pool.Exec(ctx, `INSERT INTO context_llm_log
		(pipeline, model, host, duration_ms, backend_name,
		 queue_wait_ms, dispatch_class, dispatch_abort, request_user)
		VALUES ('embed-backfill','qwen3-embed','herbert',NULL,'llama-embed',
		        1234,'background','queue_full','USER-SECRET-MARKER')`)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	handler := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	body := serveLLMLog(t, handler, "").Body.String()

	var resp struct {
		Entries []llmlogEntry `json:"entries"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("unmarshal: %v — body %s", err, body)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.QueueWaitMs == nil || *e.QueueWaitMs != 1234 {
		t.Errorf("queue_wait_ms missing/wrong: %v", e.QueueWaitMs)
	}
	if e.DispatchClass == nil || *e.DispatchClass != "background" {
		t.Errorf("dispatch_class missing/wrong: %v", e.DispatchClass)
	}
	if e.DispatchAbort == nil || *e.DispatchAbort != "queue_full" {
		t.Errorf("dispatch_abort missing/wrong: %v", e.DispatchAbort)
	}
	// Body-exclusion invariant holds even with the new columns.
	if strings.Contains(body, "USER-SECRET-MARKER") {
		t.Errorf("body column leaked alongside dispatch columns: %s", body)
	}
}

// TestLLMLogErrorCapped proves the error side-channel is closed: a prompt marker
// placed beyond the 256-rune cap is dropped, the class survives.
func TestLLMLogErrorCapped(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	marker := "ERR-SECRET-PROMPT-MARKER"
	longErr := "llm: unexpected status 403: " + strings.Repeat("x", 300) + marker
	_, err := pool.Exec(ctx, `INSERT INTO context_llm_log
		(pipeline, model, host, duration_ms, error, backend_name)
		VALUES ('query-synthesize','qwen','herbert',10,$1,'herbert-chat')`, longErr)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	body := serveLLMLog(t, h, "").Body.String()

	if strings.Contains(body, marker) {
		t.Errorf("error marker beyond the cap leaked: %s", body)
	}
	if !strings.Contains(body, "http_403") {
		t.Errorf("error class should survive normalization: %s", body)
	}
}

// TestLLMLogTenantScoped pins the per-tenant view (T37b, 04-W5 §4.6): a
// server-admin sees every row; a tenant-admin sees ONLY rows attributed to its
// own tenant's keys (never a foreign tenant's, never the api_key_id-NULL
// background rows); a tenant with no keys sees nothing (fail-closed, not an
// unfiltered view). Red against an unscoped handler (the pre-T37b state).
func TestLLMLogTenantScoped(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insertTenant := func(slug string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
			t.Fatalf("insert tenant %s: %v", slug, err)
		}
		return id
	}
	insertKey := func(hash, tenantID string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id)
			 VALUES ($1,$2,'private',$3::uuid) RETURNING id::text`, hash, hash, tenantID).Scan(&id); err != nil {
			t.Fatalf("insert key %s: %v", hash, err)
		}
		return id
	}
	insertLog := func(apiKeyID *string, backend string) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, backend_name)
			 VALUES ('query-synthesize','m','h',10,$1,$2)`, apiKeyID, backend); err != nil {
			t.Fatalf("insert log %s: %v", backend, err)
		}
	}

	tenantA := insertTenant("t37b-tenant-a")
	tenantB := insertTenant("t37b-tenant-b")
	tenantEmpty := insertTenant("t37b-tenant-empty")
	keyA := insertKey("t37b-key-a", tenantA)
	keyB := insertKey("t37b-key-b", tenantB)
	insertLog(&keyA, "backend-a")
	insertLog(&keyB, "backend-b")
	insertLog(nil, "backend-bg") // background row, no caller

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	type resp struct {
		Entries []llmlogEntry `json:"entries"`
	}
	backendsOf := func(ar *auth.AuthResult) map[string]bool {
		var r resp
		if err := json.Unmarshal(serveLLMLogAs(t, h, "", ar).Body.Bytes(), &r); err != nil {
			t.Fatalf("decode: %v", err)
		}
		set := map[string]bool{}
		for _, e := range r.Entries {
			set[e.Backend] = true
		}
		return set
	}

	// server-admin: every row incl. the background one.
	srv := backendsOf(&auth.AuthResult{IsValid: true, IsAdmin: true})
	for _, b := range []string{"backend-a", "backend-b", "backend-bg"} {
		if !srv[b] {
			t.Errorf("server-admin should see %q: %v", b, srv)
		}
	}

	// tenant-A admin: only its own row.
	a := backendsOf(&auth.AuthResult{IsValid: true, TenantID: tenantA, TenantRole: auth.RoleAdmin})
	if !a["backend-a"] {
		t.Errorf("tenant-A admin lost its own row: %v", a)
	}
	if a["backend-b"] {
		t.Errorf("telemetry leak: tenant-A admin saw tenant-B's row: %v", a)
	}
	if a["backend-bg"] {
		t.Errorf("tenant-A admin saw an api_key_id-NULL background row: %v", a)
	}

	// tenant-B admin: only its own row.
	b := backendsOf(&auth.AuthResult{IsValid: true, TenantID: tenantB, TenantRole: auth.RoleOwner})
	if !b["backend-b"] || b["backend-a"] || b["backend-bg"] {
		t.Errorf("tenant-B admin view wrong (want only backend-b): %v", b)
	}

	// tenant with no keys: empty filter → zero rows (fail-closed).
	empty := backendsOf(&auth.AuthResult{IsValid: true, TenantID: tenantEmpty, TenantRole: auth.RoleAdmin})
	if len(empty) != 0 {
		t.Errorf("keyless tenant admin should see nothing (fail-closed), saw: %v", empty)
	}
}

// TestLLMLogFilters pins the pipeline + errors_only query params.
func TestLLMLogFilters(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	seed := func(pipeline string, withErr bool) {
		errVal := "NULL"
		if withErr {
			errVal = "'boom'"
		}
		_, err := pool.Exec(ctx, `INSERT INTO context_llm_log
			(pipeline, model, host, duration_ms, error, backend_name)
			VALUES ($1,'qwen','herbert',10,`+errVal+`,'herbert-chat')`, pipeline)
		if err != nil {
			t.Fatalf("seed %s: %v", pipeline, err)
		}
	}
	seed("query-synthesize", false)
	seed("query-synthesize", true)
	seed("dream-eval", false)

	type resp struct {
		Success bool          `json:"success"`
		Entries []llmlogEntry `json:"entries"`
	}
	decode := func(query string) resp {
		var r resp
		if err := json.Unmarshal(serveLLMLog(t, NewLLMLogHandler(pool, config.NewStore(&config.Config{})), query).Body.Bytes(), &r); err != nil {
			t.Fatalf("decode %s: %v", query, err)
		}
		return r
	}

	if got := decode("?pipeline=query-synthesize"); len(got.Entries) != 2 {
		t.Errorf("pipeline filter: got %d entries, want 2", len(got.Entries))
	}
	errsOnly := decode("?errors_only=true")
	if len(errsOnly.Entries) != 1 {
		t.Errorf("errors_only: got %d entries, want 1", len(errsOnly.Entries))
	} else if errsOnly.Entries[0].Error == nil {
		t.Errorf("errors_only entry must carry a non-nil error")
	}
	if got := decode(""); len(got.Entries) != 3 {
		t.Errorf("no filter: got %d entries, want 3", len(got.Entries))
	}
}

// TestLLMLogDetailTenantGate pins the per-tenant gate of the D1b detail
// endpoint (MW12b) — the half TestLLMLogTenantScoped covers for the LIST.
// Added by the integrator after a red-probe (dropping the api_key_id
// predicate from the detail query) stayed GREEN: nothing pinned the gate.
// A tenant-admin fetches ONLY rows attributed to its own keys; a foreign,
// background (api_key_id NULL), unknown or malformed id answers a UNIFORM
// 404 (no existence oracle); a keyless tenant is fail-closed. The
// server-admin counter-probe proves the test could see a foreign body.
func TestLLMLogDetailTenantGate(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insertTenant := func(slug string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_tenants (slug, display_name) VALUES ($1,$2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
			t.Fatalf("insert tenant %s: %v", slug, err)
		}
		return id
	}
	insertKey := func(hash, tenantID string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id)
			 VALUES ($1,$2,'private',$3::uuid) RETURNING id::text`, hash, hash, tenantID).Scan(&id); err != nil {
			t.Fatalf("insert key %s: %v", hash, err)
		}
		return id
	}
	insertLog := func(apiKeyID *string, backend, body string) string {
		var id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_llm_log (pipeline, model, host, duration_ms, api_key_id, backend_name, required_sensitivity, request_system)
			 VALUES ('query-synthesize','m','h',10,$1,$2,'internal',$3) RETURNING id::text`, apiKeyID, backend, body).Scan(&id); err != nil {
			t.Fatalf("insert log %s: %v", backend, err)
		}
		return id
	}

	tenantA := insertTenant("d1b-tenant-a")
	tenantB := insertTenant("d1b-tenant-b")
	tenantEmpty := insertTenant("d1b-tenant-empty")
	keyA := insertKey("d1b-key-a", tenantA)
	keyB := insertKey("d1b-key-b", tenantB)
	rowA := insertLog(&keyA, "backend-a", "body-a")
	rowB := insertLog(&keyB, "backend-b", "body-b")
	rowBG := insertLog(nil, "backend-bg", "body-bg")

	h := NewLLMLogHandler(pool, config.NewStore(&config.Config{}))
	fetch := func(id string, ar *auth.AuthResult) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/llmlog/"+id, nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", id)
		reqCtx := context.WithValue(req.Context(), chi.RouteCtxKey, rctx)
		req = req.WithContext(context.WithValue(reqCtx, authResultKey, ar))
		rec := httptest.NewRecorder()
		h.HandleLLMLogDetail(rec, req)
		return rec
	}
	admA := &auth.AuthResult{IsValid: true, TenantID: tenantA, TenantRole: auth.RoleAdmin}

	// Own row: 200 with the body present.
	if rec := fetch(rowA, admA); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "body-a") {
		t.Errorf("tenant-A own row: status %d body %s; want 200 with body-a", rec.Code, rec.Body.String())
	}
	// Foreign row, background row, unknown id, malformed id: UNIFORM 404,
	// and the foreign body never appears anywhere in the response.
	for name, id := range map[string]string{
		"foreign":    rowB,
		"background": rowBG,
		"unknown":    "01920000-0000-7000-8000-000000000000",
		"malformed":  "not-a-uuid",
	} {
		rec := fetch(id, admA)
		if rec.Code != http.StatusNotFound {
			t.Errorf("tenant-A %s id: status %d, want uniform 404", name, rec.Code)
		}
		if strings.Contains(rec.Body.String(), "body-b") || strings.Contains(rec.Body.String(), "body-bg") {
			t.Errorf("tenant-A %s id: foreign body leaked: %s", name, rec.Body.String())
		}
	}
	// Keyless tenant: fail-closed 404 even for an existing row.
	if rec := fetch(rowA, &auth.AuthResult{IsValid: true, TenantID: tenantEmpty, TenantRole: auth.RoleAdmin}); rec.Code != http.StatusNotFound {
		t.Errorf("keyless tenant admin: status %d, want 404", rec.Code)
	}
	// Server-admin counter-probe: the foreign row IS servable (the 404s above
	// come from the gate, not from a broken fetch).
	if rec := fetch(rowB, &auth.AuthResult{IsValid: true, IsAdmin: true}); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "body-b") {
		t.Errorf("server-admin foreign row: status %d body %s; want 200 with body-b", rec.Code, rec.Body.String())
	}
}
