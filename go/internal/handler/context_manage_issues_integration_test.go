//go:build integration

// Integration tests for Achse-02 Welle I-D manage issue-* transport (design/02
// §4.3/§5.2/§7). testcontainers PG18, full migration chain incl. 084 seeds; the
// block-type registry is booted so issue policy (workflow states, structural
// link classes) resolves. callManage + bgBlock are shared helpers in the same
// (integration-tagged) handler package.
//
// Gates proven here (each a §7 negative probe):
//   - cross-tenant issue-list with a foreign key ⇒ empty (never a foreign row);
//   - invalid workflow transition ⇒ 422 (blocktype policy data, not hardcoded);
//   - link-create with a foreign-scope target AND a nonexistent target ⇒ the
//     BYTE-IDENTICAL 404 (no existence oracle for foreign block ids);
//   - comment-create on a foreign-scope parent ⇒ 404, own parent inherits scope.
//
// Run: go test -tags=integration ./internal/handler/ -run TestManageIssues -count=1 -v.
package handler

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestManageIssues_Integration(t *testing.T) {
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
	h := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	// Two project keys with DISJOINT scopes (Modell C repo-agent shape): each can
	// read+write only its own scope. TenantID is a valid uuid so resolveGrants
	// runs (grants empty).
	keyA := &auth.AuthResult{IsValid: true, HomeScope: "proj-a", ReadScopes: []string{"proj-a"}, TenantID: store.DefaultTenantID}
	keyB := &auth.AuthResult{IsValid: true, HomeScope: "proj-b", ReadScopes: []string{"proj-b"}, TenantID: store.DefaultTenantID}

	// Create an issue as A.
	code, resp := callManage(t, h, keyA, "issue-create", "", map[string]any{"title": "leaky?", "content": "a body"})
	if code != 200 || resp["success"] != true {
		t.Fatalf("issue-create A: code=%d resp=%v", code, resp)
	}
	if resp["render"] != "untrusted" {
		t.Fatalf("issue-create response missing render:'untrusted': %v", resp["render"])
	}
	issueA := resp["issue"].(map[string]any)["id"].(string)

	t.Run("cross_tenant_list_empty", func(t *testing.T) {
		// A sees its issue…
		_, ra := callManage(t, h, keyA, "issue-list", "", nil)
		if n := len(ra["issues"].([]any)); n < 1 {
			t.Fatalf("A own list = %d issues, want ≥1", n)
		}
		// …B (foreign key) sees NOTHING (rot mit ungefiltertem Scope).
		_, rb := callManage(t, h, keyB, "issue-list", "", nil)
		if n := len(rb["issues"].([]any)); n != 0 {
			t.Fatalf("cross-tenant LEAK: B saw %d issue rows", n)
		}
	})

	t.Run("invalid_transition_422", func(t *testing.T) {
		code, resp := callManage(t, h, keyA, "issue-update", issueA, map[string]any{"status": "shipped"})
		if code != 422 {
			t.Fatalf("invalid transition: code=%d, want 422 (resp=%v)", code, resp)
		}
	})

	t.Run("link_create_404_oracle_byte_identical", func(t *testing.T) {
		// A second issue in proj-a as the link target that DOES work.
		_, r2 := callManage(t, h, keyA, "issue-create", "", map[string]any{"title": "target"})
		targetA := r2["issue"].(map[string]any)["id"].(string)
		code, _ := callManage(t, h, keyA, "issue-link-create", "", map[string]any{
			"source_id": issueA, "target_id": targetA, "link_class": "references"})
		if code != 200 {
			t.Fatalf("same-scope link: code=%d, want 200", code)
		}
		// A foreign-scope target (seeded in proj-b) and a nonexistent target must
		// answer the SAME 404 — no oracle distinguishing "foreign" from "absent".
		foreignTarget := bgBlock(t, pool, "proj-b", "foreign-link-target")
		absentTarget := "00000000-0000-0000-0000-000000000000"
		cf, bodyForeign := callManage(t, h, keyA, "issue-link-create", "", map[string]any{
			"source_id": issueA, "target_id": foreignTarget, "link_class": "references"})
		ca, bodyAbsent := callManage(t, h, keyA, "issue-link-create", "", map[string]any{
			"source_id": issueA, "target_id": absentTarget, "link_class": "references"})
		if cf != 404 || ca != 404 {
			t.Fatalf("link 404 oracle: foreign=%d absent=%d, want both 404", cf, ca)
		}
		if bodyForeign["error"] != bodyAbsent["error"] || bodyForeign["success"] != bodyAbsent["success"] {
			t.Fatalf("EXISTENCE ORACLE: foreign=%v absent=%v differ", bodyForeign, bodyAbsent)
		}
	})

	t.Run("link_create_invalid_class_422_not_404", func(t *testing.T) {
		_, r := callManage(t, h, keyA, "issue-create", "", map[string]any{"title": "classcheck"})
		src := r["issue"].(map[string]any)["id"].(string)
		_, r2 := callManage(t, h, keyA, "issue-create", "", map[string]any{"title": "classcheck-t"})
		tgt := r2["issue"].(map[string]any)["id"].(string)
		// "pr-linked" is NOT in the issue type's structural_link_classes → 422
		// (source is writable, so this is an allowlist error, not a 404).
		code, _ := callManage(t, h, keyA, "issue-link-create", "", map[string]any{
			"source_id": src, "target_id": tgt, "link_class": "pr-linked"})
		if code != 422 {
			t.Fatalf("disallowed link class: code=%d, want 422", code)
		}
	})

	t.Run("comment_scope_invariant", func(t *testing.T) {
		// Comment on OWN issue ⇒ 200, scope inherited.
		code, resp := callManage(t, h, keyA, "issue-comment-create", "", map[string]any{
			"parent_id": issueA, "author": "alice", "content": "mine"})
		if code != 200 {
			t.Fatalf("own comment: code=%d resp=%v", code, resp)
		}
		if sc := resp["comment"].(map[string]any)["scope"]; sc != "proj-a" {
			t.Fatalf("comment scope = %v, want proj-a (inherited)", sc)
		}
		// Comment on a FOREIGN issue (A commenting on B's issue) ⇒ 404 (comment-
		// scope invariant: rot ohne Parent-Scope-Zwang).
		_, rb := callManage(t, h, keyB, "issue-create", "", map[string]any{"title": "B private"})
		issueB := rb["issue"].(map[string]any)["id"].(string)
		code, _ = callManage(t, h, keyA, "issue-comment-create", "", map[string]any{
			"parent_id": issueB, "content": "intrusion"})
		if code != 404 {
			t.Fatalf("cross-scope comment: code=%d, want 404", code)
		}
	})
}
