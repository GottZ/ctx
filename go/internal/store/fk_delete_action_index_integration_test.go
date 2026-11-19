//go:build integration

package store_test

// Issue #277: the catalog gate of #255 held the foreign keys on
// context_blocks to a serving index. The same cost sits on every key whose
// ON DELETE action writes the referencing table: deleting a referenced row
// runs `UPDATE … SET col = NULL WHERE col = $1` (SET NULL, SET DEFAULT) or
// `DELETE … WHERE col = $1` (CASCADE) there, and without an index whose
// leading columns are the key's that statement scans the referencing table,
// once per deleted row. TestForeignKeysWithADeleteActionHaveAServingIndex
// reads the catalog of a fresh chain and fails on every such key without a
// serving index and on every such key on a TimescaleDB hypertable (each chunk
// carries a copy of the key, and a delete probes every copy), unless the key
// is in fkDeleteActionExceptions with its reason. An exception that no longer
// matches a violation fails as well, so the list shrinks with the fixes.
//
//	go test -tags=integration ./internal/store/ -run 'TestForeignKeysWithADeleteAction' -count=1 -v

import (
	"context"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

// fkDeleteActionExceptions are the keys with a delete action the gate lets
// stand, by "table(cols) -> ref", each with the reason it may cost a scan.
//
// Three kinds. An operator-managed registry stays small by what it holds (one
// row per tenant, setting, secret, provider, project, …): a delete of the
// referenced row scans a few rows, and an index would cost every write for
// nothing. A key whose referenced row no code path deletes costs its scan only
// on a manual delete. The rest grow with use: migrations 184 and 185 gave
// their keys an index and removed their lines here (an exception that no
// longer matches a violation fails the gate); the one left is a key on a
// hypertable, which no index serves.
var fkDeleteActionExceptions = map[string]string{
	// Operator-managed registries.
	"context_disable_profile_backends(backend_id) -> context_backends":   "registry: one row per (disable profile, backend)",
	"context_gateway_limits(updated_by) -> context_api_keys":             "registry: one row per limited consumer or tenant",
	"context_gateway_limits(updated_by_principal) -> context_principals": "registry: one row per limited consumer or tenant",
	"context_oauth_clients(created_by) -> context_api_keys":              "registry: one row per registered OAuth client",
	"context_oauth_providers(created_by) -> context_api_keys":            "registry: one row per configured identity provider",
	"context_projects(created_by) -> context_api_keys":                   "registry: one row per workflow project",
	"context_secrets(created_by) -> context_api_keys":                    "registry: one row per stored secret",
	"context_secrets(rotated_by) -> context_api_keys":                    "registry: one row per stored secret",
	"context_settings(updated_by) -> context_api_keys":                   "registry: one row per setting and tenant override",
	"context_tenant_grants(created_by) -> context_api_keys":              "registry: one row per cross-tenant scope grant",
	"context_tenant_grants(granted_scope) -> context_tenant_scopes":      "registry: one row per cross-tenant scope grant",
	"context_tenant_quota(updated_by) -> context_api_keys":               "registry: one row per tenant",
	// Referenced rows no code path deletes.
	"context_embed_failures(migration_id) -> context_embed_migrations": "no code path deletes an embed migration (the row is the migration's history); a manual delete pays one scan",
	"context_access_tokens(principal_id) -> context_principals":        "no code path deletes a principal; a manual delete pays one scan",
	"context_oauth_codes(principal_id) -> context_principals":          "no code path deletes a principal; a manual delete pays one scan",
	// A table that grows with use, the key on a hypertable.
	"context_pending_writes(api_key_id) -> context_api_keys": "issue #277: a key on a hypertable; replacement of the key drafted",
}

// extensionOwned reports a key the schema does not own: a TimescaleDB chunk
// copy (reported once, through its hypertable) or a key of the extension's
// own catalog.
func extensionOwned(f blockFK) bool {
	return strings.HasPrefix(f.table, "_timescaledb_internal.") || strings.HasPrefix(f.table, "_timescaledb_catalog.")
}

// fkDeleteActionViolations is what the delete-action gate refuses in an
// inventory, keyed like fkDeleteActionExceptions.
func fkDeleteActionViolations(fks []blockFK) map[string]string {
	out := map[string]string{}
	for _, f := range fks {
		if f.onDelete != "c" && f.onDelete != "n" && f.onDelete != "d" {
			continue // NO ACTION / RESTRICT only check; the block gate covers context_blocks
		}
		if extensionOwned(f) {
			continue
		}
		key := f.table + "(" + f.cols + ") -> " + f.ref
		switch {
		case f.hypertable:
			out[key] = f.String() + ": a key on a hypertable is copied onto every chunk, and a delete of the referenced row probes each copy"
		case f.serving == "":
			out[key] = f.String() + ": no index serves the lookup, a delete of the referenced row scans the table"
		}
	}
	return out
}

func TestForeignKeysWithADeleteActionHaveAServingIndex(t *testing.T) {
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	fks := readFKs(t, ctx, pool)

	var b strings.Builder
	actions := 0
	for _, f := range fks {
		if (f.onDelete == "c" || f.onDelete == "n" || f.onDelete == "d") && !extensionOwned(f) {
			actions++
			b.WriteString("  " + f.String() + " -> " + f.ref + "\n")
		}
	}
	t.Logf("%d foreign keys with a delete action:\n%s", actions, b.String())
	// The inventory must see keys of more than one referenced table, so a
	// query that matches nothing, or only context_blocks, cannot pass.
	for _, want := range []string{
		"context_write_log(matched_block_id) -> context_blocks",
		"context_write_log(api_key_id) -> context_api_keys",
	} {
		found := false
		for _, f := range fks {
			found = found || f.table+"("+f.cols+") -> "+f.ref == want
		}
		if !found {
			t.Errorf("the inventory does not list %s", want)
		}
	}

	violations := fkDeleteActionViolations(fks)
	keys := make([]string, 0, len(violations))
	for k := range violations {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if _, ok := fkDeleteActionExceptions[k]; ok {
			t.Logf("excepted: %s (%s)", violations[k], fkDeleteActionExceptions[k])
			continue
		}
		t.Errorf("%s", violations[k])
	}
	for k := range fkDeleteActionExceptions {
		if _, ok := violations[k]; !ok {
			t.Errorf("exception %q matches no violation any more: remove it from fkDeleteActionExceptions", k)
		}
	}
}
