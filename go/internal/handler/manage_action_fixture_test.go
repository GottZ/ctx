package handler

import (
	"encoding/json"
	"testing"
)

// manageActionFixture is the /api/manage contract, PINNED FROM THE PRE-TABLE
// TREE (T03-11, root 7b30b9e1) before the dispatch switch was replaced by
// manageActions. It is deliberately a hand-written list, NOT derived from the
// production table: a fixture generated from the thing it guards proves
// nothing (design/03 §5.4 Bruchpfad 1 — "aus dem Ist-Stand extrahiert und
// gepinnt, nicht aus der neuen Tabelle abgeleitet").
//
// Three columns, three different failure modes:
//
//   - action: the enumeration. A row that disappears from manageActions is an
//     action that answers 400 instead of its handler (§5.4 Bruchpfad 1).
//   - data + tier: the privilege map. The tier is a function of (action,
//     data shape), not of the action name alone — dream-mode, gaming-mode and
//     eject-mode gate only their MUTATING shape (context_manage.go:611-613 /
//     :635-647 in the pre-table tree), so they carry TWO rows each. The tiers
//     were pinned by running this fixture against the OLD actionTier(req)
//     resolver, which is why the eleven actions that had no explicit arm back
//     then (they fell through to the fail-open default) appear here as
//     tierOpen: their behaviour is unchanged, their classification is now
//     deliberate instead of a fall-through (§5.4 Bruchpfad 2).
//   - handler: the routing. An adapter closure calling the wrong handler
//     compiles cleanly (§5.4 Bruchpfad 5); manage_table_gate_test.go reads the
//     handler names back out of manage_table.go's AST and compares them here.
type manageActionFixtureRow struct {
	action  string
	data    string // raw req.Data for the tier probe; "" = field absent
	tier    adminTier
	handler string // the *ManageHandler method the dispatch chain reaches
}

var manageActionFixture = []manageActionFixtureRow{
	{"api-key-create", "", tierTenantAdmin, "handleApiKeyCreate"},
	{"api-key-delete", "", tierTenantAdmin, "handleApiKeyDelete"},
	{"api-key-list", "", tierTenantAdmin, "handleApiKeyList"},
	{"api-key-update", "", tierTenantAdmin, "handleApiKeyUpdate"},
	{"backend-create", "", tierTenantAdmin, "handleBackendCreate"},
	{"backend-delete", "", tierTenantAdmin, "handleBackendDelete"},
	{"backend-list", "", tierTenantAdmin, "handleBackendList"},
	{"backend-reorder", "", tierTenantAdmin, "handleBackendReorder"},
	{"backend-test", "", tierServerAdmin, "handleBackendTest"},
	{"backend-update", "", tierTenantAdmin, "handleBackendUpdate"},
	{"block-grant-create", "", tierServerAdmin, "handleBlockGrantCreate"},
	{"block-grant-list", "", tierServerAdmin, "handleBlockGrantList"},
	{"block-grant-revoke", "", tierServerAdmin, "handleBlockGrantRevoke"},
	{"blocks-audit-start", "", tierServerAdmin, "handleBlocksAuditStart"},
	{"blocks-audit-status", "", tierServerAdmin, "handleBlocksAuditStatus"},
	{"blocks-classify-start", "", tierServerAdmin, "handleBlocksClassifyStart"},
	{"blocks-classify-status", "", tierServerAdmin, "handleBlocksClassifyStatus"},
	{"delete", "", tierOpen, "handleDelete"},
	{"disable-profile-create", "", tierTenantAdmin, "handleDisableProfileCreate"},
	{"disable-profile-delete", "", tierTenantAdmin, "handleDisableProfileDelete"},
	{"disable-profile-list", "", tierTenantAdmin, "handleDisableProfileList"},
	{"disable-profile-toggle", "", tierTenantAdmin, "handleDisableProfileToggle"},
	{"disable-profile-update", "", tierTenantAdmin, "handleDisableProfileUpdate"},
	{"dream-backoff-restamp", "", tierTenantAdmin, "handleDreamBackoffRestamp"},
	{"dream-link-resolve", "", tierOpen, "handleDreamLinkResolve"},
	// dream-mode: read shape open, mutating shape server-admin
	// (isDreamModeMutation, context_manage.go).
	{"dream-mode", "", tierOpen, "handleDreamMode"},
	{"dream-mode", `{"mode":"off"}`, tierServerAdmin, "handleDreamMode"},
	{"dream-review", "", tierOpen, "handleDreamReview"},
	{"dream-stats", "", tierOpen, "handleDreamStats"},
	// eject-mode / gaming-mode: same read/write split, isGamingModeMutation.
	{"eject-mode", `{}`, tierOpen, "handleGamingMode"},
	{"eject-mode", `{"mode":"on"}`, tierServerAdmin, "handleGamingMode"},
	{"embed-migration-abort", "", tierServerAdmin, "handleEmbedMigrationAbort"},
	{"embed-migration-cleanup", "", tierServerAdmin, "handleEmbedMigrationCleanup"},
	{"embed-migration-confirm", "", tierServerAdmin, "handleEmbedMigrationConfirm"},
	{"embed-migration-create", "", tierServerAdmin, "handleEmbedMigrationCreate"},
	{"embed-migration-failures", "", tierServerAdmin, "handleEmbedMigrationFailures"},
	// embed-migration-pause is the one row whose handler takes derived
	// PARAMETERS (running → paused): the pre-table tree derived them in
	// dispatchEmbedMigrationAction, the table derives them in the row.
	{"embed-migration-pause", "", tierServerAdmin, "handleEmbedMigrationTransition"},
	{"embed-migration-purge", "", tierServerAdmin, "handleEmbedMigrationPurge"},
	{"embed-migration-resume", "", tierServerAdmin, "handleEmbedMigrationResume"},
	{"embed-migration-rollback", "", tierServerAdmin, "handleEmbedMigrationRollback"},
	{"embed-migration-status", "", tierServerAdmin, "handleEmbedMigrationStatus"},
	{"forge-sync-start", "", tierTenantAdmin, "handleForgeSyncStart"},
	{"forge-sync-status", "", tierTenantAdmin, "handleForgeSyncStatus"},
	{"forge-token-set", "", tierTenantAdmin, "handleForgeTokenSet"},
	{"gaming-mode", `{}`, tierOpen, "handleGamingMode"},
	{"gaming-mode", `{"mode":"on"}`, tierServerAdmin, "handleGamingMode"},
	{"get", "", tierOpen, "handleGet"},
	{"guard-list", "", tierOpen, "handleGuardList"},
	{"guard-resolve", "", tierOpen, "handleGuardResolve"},
	{"guard-stats", "", tierOpen, "handleGuardStats"},
	{"issue-comment-create", "", tierOpen, "handleIssueCommentCreate"},
	{"issue-create", "", tierOpen, "handleIssueCreate"},
	{"issue-get", "", tierOpen, "handleIssueGet"},
	{"issue-link-create", "", tierOpen, "handleIssueLinkCreate"},
	{"issue-link-delete", "", tierOpen, "handleIssueLinkDelete"},
	{"issue-list", "", tierOpen, "handleIssueList"},
	{"issue-update", "", tierOpen, "handleIssueUpdate"},
	{"list-categories", "", tierOpen, "handleListCategories"},
	{"list-meta", "", tierOpen, "handleListMeta"},
	{"mcp-client-create", "", tierServerAdmin, "handleMCPClientCreate"},
	{"mcp-client-delete", "", tierServerAdmin, "handleMCPClientDelete"},
	{"mcp-client-list", "", tierServerAdmin, "handleMCPClientList"},
	// model-alias-* (W02-04, design/02 §4.4.2): tenant-admin tier with the
	// field tiers in the handler (_global and foreign scopes 404-equivalent,
	// lowering a sensitivity server-admin + confirm).
	{"model-alias-create", "", tierTenantAdmin, "handleModelAliasCreate"},
	{"model-alias-delete", "", tierTenantAdmin, "handleModelAliasDelete"},
	{"model-alias-list", "", tierTenantAdmin, "handleModelAliasList"},
	{"model-alias-preview", "", tierServerAdmin, "handleModelAliasPreview"},
	{"model-alias-update", "", tierTenantAdmin, "handleModelAliasUpdate"},
	// W01-5: the model consumer administration — NEW surface, not pre-table
	// (design/01 §4.6). Granting a credential is tierSelfService, reading and
	// revoking tierTenantAdmin; the field tiers live in the handlers.
	{"model-consumer-create", "", tierSelfService, "handleModelConsumerCreate"},
	{"model-consumer-delete", "", tierTenantAdmin, "handleModelConsumerDelete"},
	{"model-consumer-get", "", tierTenantAdmin, "handleModelConsumerGet"},
	{"model-consumer-list", "", tierTenantAdmin, "handleModelConsumerList"},
	{"model-consumer-update", "", tierTenantAdmin, "handleModelConsumerUpdate"},
	{"model-key-history", "", tierTenantAdmin, "handleModelKeyHistory"},
	{"model-key-revoke", "", tierTenantAdmin, "handleModelKeyRevoke"},
	{"model-key-rotate", "", tierSelfService, "handleModelKeyRotate"},
	{"oauth-identity-link", "", tierServerAdmin, "handleOAuthIdentityLink"},
	{"oauth-identity-list", "", tierServerAdmin, "handleOAuthIdentityList"},
	{"oauth-identity-unlink", "", tierServerAdmin, "handleOAuthIdentityUnlink"},
	{"oauth-provider-create", "", tierServerAdmin, "handleOAuthProviderCreate"},
	{"oauth-provider-delete", "", tierServerAdmin, "handleOAuthProviderDelete"},
	{"oauth-provider-list", "", tierServerAdmin, "handleOAuthProviderList"},
	{"overview-rebuild-start", "", tierServerAdmin, "handleOverviewRebuildStart"},
	{"project-provision", "", tierServerAdmin, "handleProjectProvision"},
	{"scope-create", "", tierTenantAdmin, "handleScopeCreate"},
	{"scope-list", "", tierTenantAdmin, "handleScopeList"},
	{"scope-overview", "", tierServerAdmin, "handleScopeOverview"},
	{"stats", "", tierOpen, "handleStats"},
	{"tenant-create", "", tierServerAdmin, "handleTenantCreate"},
	{"tenant-delete", "", tierServerAdmin, "handleTenantDelete"},
	{"tenant-get", "", tierServerAdmin, "handleTenantGet"},
	{"tenant-grant-create", "", tierServerAdmin, "handleTenantGrantCreate"},
	{"tenant-grant-delete", "", tierServerAdmin, "handleTenantGrantDelete"},
	{"tenant-grant-list", "", tierServerAdmin, "handleTenantGrantList"},
	{"tenant-limit-set", "", tierServerAdmin, "handleTenantLimitSet"},
	{"tenant-list", "", tierServerAdmin, "handleTenantList"},
	{"tenant-quota-get", "", tierTenantAdmin, "handleTenantQuotaGet"},
	{"tenant-quota-set", "", tierServerAdmin, "handleTenantQuotaSet"},
	{"tenant-update", "", tierServerAdmin, "handleTenantUpdate"},
	{"tenant-usage-get", "", tierTenantAdmin, "handleTenantUsageGet"},
	{"type-create", "", tierServerAdmin, "handleTypeCreate"},
	{"type-delete", "", tierServerAdmin, "handleTypeDelete"},
	{"type-get", "", tierOpen, "handleTypeGet"},
	{"type-list", "", tierOpen, "handleTypeList"},
	{"type-update", "", tierServerAdmin, "handleTypeUpdate"},
	{"update", "", tierOpen, "handleUpdate"},
}

// fixtureActionNames is the deduplicated enumeration (101 names: the 89 of the
// pre-table tree plus the four model alias actions of W02-04 and the eight model
// consumer actions of W01-5; three
// actions carry two rows for their two data shapes).
func fixtureActionNames() map[string]struct{} {
	names := make(map[string]struct{}, len(manageActionFixture))
	for _, row := range manageActionFixture {
		names[row.action] = struct{}{}
	}
	return names
}

// TestManageActionFixture_Shape guards the fixture itself: 101 distinct actions
// in 104 rows, alphabetically ordered so a merge conflict is a real conflict.
func TestManageActionFixture_Shape(t *testing.T) {
	if got := len(manageActionFixture); got != 104 {
		t.Fatalf("fixture rows = %d, want 104 (101 actions, three of them twice)", got)
	}
	if got := len(fixtureActionNames()); got != 101 {
		t.Fatalf("fixture actions = %d, want 101", got)
	}
	for i := 1; i < len(manageActionFixture); i++ {
		if manageActionFixture[i-1].action > manageActionFixture[i].action {
			t.Errorf("fixture not sorted at row %d: %q after %q",
				i, manageActionFixture[i].action, manageActionFixture[i-1].action)
		}
	}
}

// TestManageActionFixture_TierEquivalence is the (Action, Data-Shape) → Tier
// equivalence gate (design/03 §5.4 Bruchpfad 2). It ran GREEN against the
// pre-table resolver (root 7b30b9e1, actionTierExplicit's 211-line switch) and
// must stay green against the table lookup — that is the whole point: the
// fixture is the bridge between the two implementations.
func TestManageActionFixture_TierEquivalence(t *testing.T) {
	for _, row := range manageActionFixture {
		req := manageRequest{Action: row.action}
		if row.data != "" {
			req.Data = json.RawMessage(row.data)
		}
		if got := actionTier(req); got != row.tier {
			t.Errorf("actionTier(%q, data=%q) = %d, want %d",
				row.action, row.data, got, row.tier)
		}
	}
}
