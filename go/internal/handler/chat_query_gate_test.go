package handler

import (
	"context"
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
)

// chatToolQuery runs the web-chat ctx_query delegation against the REAL query
// handler (DB-less: the broken pool ends a request at the search stage, after
// the embed wire) for a session owned by scope. The continuing caller is homed
// in another scope, so only the session's scope can bind the request.
func chatToolQuery(t *testing.T, bpool *backends.Pool, acc *backends.QuotaAccountant, scope, query string) error {
	t.Helper()
	st := &countingStore{}
	st.cfg.Store(snapshotTestConfig())
	h := NewQueryHandler(brokenPool(t), st, bpool, acc, blocktype.NewRegistry(), snapshotTestAdmitter(t))
	qr := &chatQueryRunner{handler: http.HandlerFunc(h.HandleQuery)}
	caller := &auth.AuthResult{ApiKeyID: "019e0011-aaaa-7000-9000-000000000001", HomeScope: "caller-home", ReadScopes: []string{scope}, IsValid: true}
	_, err := qr.RunQuery(context.WithValue(context.Background(), authResultKey, caller), scope, []string{scope}, query, 5)
	return err
}

// TestChatQueryToolQuotaGate (§C2): ctx_query in the web chat runs under the
// session owner tenant's quota. A tenant over its call budget gets a failed
// tool call and the embed backend is never reached; a scope without a policy
// reaches the embed wire (control — the gated run is not a broken fixture).
func TestChatQueryToolQuotaGate(t *testing.T) {
	zero := 0
	acc := backends.NewQuotaAccountant(nil, 0)
	acc.SeedForTest("acme:work", backends.TenantQuota{DailyCalls: &zero, Enabled: true, OnExceed: backends.QuotaExceedExternalOff}, 0, 0)

	srv, hits := fakeEmbedServer(t)
	if err := chatToolQuery(t, embedPool(srv.URL), acc, "acme:work", "alpha bravo charlie"); err == nil {
		t.Fatal("ctx_query of a tenant over its call budget must fail")
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("embed hits = %d; want 0 — the tenant quota must stop ctx_query before the embed wire", n)
	}

	srv2, hits2 := fakeEmbedServer(t)
	_ = chatToolQuery(t, embedPool(srv2.URL), acc, "free:work", "delta echo foxtrot")
	if n := hits2.Load(); n != 1 {
		t.Fatalf("control: embed hits = %d; want 1 for a scope without a quota policy", n)
	}
}

// TestChatQueryToolSeesTenantEmbedBackend (§C2 side effect): with the owner's
// scope as home scope, the embed chain of ctx_query includes the tenant's own
// embed backend. With an empty home scope a tenant-private backend is
// invisible (backends.VisibleTo) and the tool failed with "embedding failed"
// before any attempt. A tenant row dials through ssrfguard (GHSA 2026-10-31
// §3), so the loopback fake is refused at dial time: the proof is the
// attempt the pool recorded against the row, not a served vector.
func TestChatQueryToolSeesTenantEmbedBackend(t *testing.T) {
	srv, hits := fakeEmbedServer(t)
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "e", Name: "acme-embed", Host: srv.URL, Protocol: backends.ProtocolOllama,
		Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
		Roles: []string{backends.RoleEmbed}, Scope: "acme:work",
	}})
	if err := chatToolQuery(t, bpool, nil, "acme:work", "golf hotel india"); err == nil {
		t.Fatal("ctx_query must fail: the loopback tenant row is refused by the SSRF guard")
	}
	var fails int
	for _, st := range bpool.Status() {
		if st.ID == "e" {
			fails = st.ConsecutiveFails
		}
	}
	if fails != 1 {
		t.Fatalf("tenant embed row: consecutive fails = %d; want 1 — ctx_query must resolve the owner tenant's private embed backend", fails)
	}
	if n := hits.Load(); n != 0 {
		t.Fatalf("embed hits = %d; want 0 (the guard refuses the loopback dial)", n)
	}
}
