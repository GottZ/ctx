package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
)

// TestMCPQueryQuotaRefusalIsToolError: the MCP query tool delegates to the
// REAL query handler (DB-less; the broken pool is never reached here). A
// tenant over its call budget is refused at the embed gate with a 429 whose
// body carries only error_code — the tool must return an isError result
// naming quota_exceeded, not an empty answer, and the embed backend is never
// reached.
func TestMCPQueryQuotaRefusalIsToolError(t *testing.T) {
	zero := 0
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("acme:work", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)
	srv, hits := fakeEmbedServer(t)
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(brokenPool(t), st, embedPool(srv.URL), acc, blocktype.NewRegistry(), snapshotTestAdmitter(t))

	ctx := context.WithValue(context.Background(), authResultKey, &auth.AuthResult{
		ApiKeyID: "019e0011-aaaa-7000-9000-000000000001", IsValid: true, TenantRole: auth.RoleMember,
		HomeScope: "acme:work", ReadScopes: []string{"acme:work"},
	})
	res, _, err := mcpQueryHandler(MCPConfig{QueryHandler: http.HandlerFunc(h.HandleQuery)})(ctx, nil, queryInput{Question: "alpha bravo charlie"})
	if err != nil {
		t.Fatalf("query tool: transport error %v", err)
	}
	if !res.IsError || !strings.Contains(mcpTextOf(res), "quota_exceeded") {
		t.Fatalf("query tool: IsError=%v text=%q; want an isError result naming quota_exceeded", res.IsError, mcpTextOf(res))
	}
	if env, ok := res.StructuredContent.(mcpErrorEnvelope); !ok || env.Code != "quota_exceeded" {
		t.Errorf("structured content = %#v; want the quota_exceeded code", res.StructuredContent)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("embed hits = %d; want 0", n)
	}
}

// TestMCPQueryRefusalInCommittedBody: on the synthesis path the heartbeat has
// committed the 200 before a refusal is known, so only the body carries the
// code. The tool still returns isError with the code — and only the code: the
// role and required sensitivity a no_eligible_backend body carries stay out
// of the tool text.
func TestMCPQueryRefusalInCommittedBody(t *testing.T) {
	body := `  {"success":false,"error_code":"no_eligible_backend","role":"synthesis","required_sensitivity":"personal"}`
	res, _, err := mcpQueryHandler(MCPConfig{QueryHandler: &cannedQueryDelegate{body: body}})(typeFilterCtx(), nil, queryInput{Question: "q"})
	if err != nil {
		t.Fatalf("query tool: transport error %v", err)
	}
	text := mcpTextOf(res)
	if !res.IsError || !strings.Contains(text, "no_eligible_backend") {
		t.Fatalf("query tool: IsError=%v text=%q; want an isError result naming no_eligible_backend", res.IsError, text)
	}
	if strings.Contains(text, "synthesis") || strings.Contains(text, "personal") {
		t.Errorf("tool text %q leaks role / required sensitivity", text)
	}
}

// statusQueryDelegate answers a fixed status with a body that names no error.
type statusQueryDelegate struct{ status int }

func (h statusQueryDelegate) ServeHTTP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, h.status, map[string]any{"success": false})
}

// TestMCPQueryHTTPErrorIsToolError: a delegated answer with an error status
// and neither error nor error_code is a tool error, never an empty answer.
func TestMCPQueryHTTPErrorIsToolError(t *testing.T) {
	res, _, err := mcpQueryHandler(MCPConfig{QueryHandler: statusQueryDelegate{status: http.StatusBadGateway}})(typeFilterCtx(), nil, queryInput{Question: "q"})
	if err != nil {
		t.Fatalf("query tool: transport error %v", err)
	}
	if !res.IsError || !strings.Contains(mcpTextOf(res), "502") {
		t.Fatalf("query tool: IsError=%v text=%q; want an isError result with the status", res.IsError, mcpTextOf(res))
	}
}
