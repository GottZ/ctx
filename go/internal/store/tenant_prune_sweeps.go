package store

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/pgxdb"
)

// pruneOwnedSQL is the owner test of every scope-keyed statement of the
// prune (issue #555, "SECOND DELETE" at PruneTenant): the scope names the
// prune read at its start that the tenant $2 still owns. A fragment of
// constant statements only — no value is ever interpolated.
const pruneOwnedSQL = `(SELECT ts.scope::text FROM context_tenant_scopes ts WHERE ts.tenant_id = $2::uuid AND ts.scope = ANY($1::text[]))`

// pruneScopeSweeps are the statements the prune runs once after the block
// pass and again in every closing round, each in one piece (no batches): $1
// = the tenant's scopes, $2 = the tenant. They drain every row that names a
// scope of the tenant and that no other step takes (issue #555): the
// tenant's configuration — what a tenant-admin writes into its home scope,
// which the next owner of the scope name would inherit, down to the backends
// its model calls go to —, the graph's aggregates (block titles and topic
// labels), the measurements of its scopes, and the rows of OTHER tenants and
// keys that name one of its scopes. Small tables, or tables whose rows of
// one tenant are few next to its blocks; the large ones are batched
// (pruneScopeDeletes). ORDER IS LOAD-BEARING where noted:
//
//   - the graph nodes before their topics (graph_cluster_node.topic_id is NO
//     ACTION; the centroids go with their topic by CASCADE, and on their own
//     before it);
//   - the settings audit LAST: every configuration delete above writes its
//     audit row through its table's trigger, in the scope.
//
// The settings audit keeps every row that is not a setting's: those are read
// by tenant id (ListAudit, metadata.tenant_id — the deleted tenant's id,
// which never returns) or by the server admin, never by scope, and they are
// the record of the offboarding. A setting's audit rows are read by scope
// (ListSettingAudit, GET /api/settings/{key} of a tenant-admin) and go.
var pruneScopeSweeps = []struct{ table, rel, sql string }{
	// PROJECT-SECRET-DRAIN (K14, design/02 §5.2): context_secrets rows carry
	// the scope ONLY as a plain column (no FK to context_blocks /
	// context_tenant_scopes, 051), so NOTHING cascades them — a naked prune
	// would leave a live forge PAT (I-F seals it under
	// 'forge.token.<project_id>' in the PROJECT scope, sync.go) AND any
	// webhook secret ('webhook.github.<id>', W13) alive in a scope whose
	// owning tenant is gone.
	{"context_secrets", "context_secrets", `DELETE FROM context_secrets WHERE scope IN ` + pruneOwnedSQL},
	// A stage of a key of another tenant whose lists named the scope (the
	// tenant's own keys' stages go with them in the key step).
	{"context_pending_writes", "context_pending_writes", `DELETE FROM context_pending_writes WHERE scope IN ` + pruneOwnedSQL},
	// A chat session of another tenant that read one of the scopes (through
	// a cross-tenant grant): its messages carry what it read there, and its
	// gate (read_scopes) names a scope the next owner of the name gets.
	{"context_chat_sessions (read_scopes)", "context_chat_sessions", `DELETE FROM context_chat_sessions WHERE read_scopes && $1::text[] AND read_scopes && ARRAY` + pruneOwnedSQL},
	{"context_settings", "context_settings", `DELETE FROM context_settings WHERE scope IN ` + pruneOwnedSQL},
	{"context_disable_profiles", "context_disable_profiles", `DELETE FROM context_disable_profiles WHERE scope IN ` + pruneOwnedSQL},
	{"context_backends", "context_backends", `DELETE FROM context_backends WHERE scope IN ` + pruneOwnedSQL},
	{"context_model_aliases", "context_model_aliases", `DELETE FROM context_model_aliases WHERE scope IN ` + pruneOwnedSQL},
	{"context_graph_category_hues", "context_graph_category_hues", `DELETE FROM context_graph_category_hues WHERE scope IN ` + pruneOwnedSQL},
	{"context_block_types", "context_block_types", `DELETE FROM context_block_types WHERE scope IN ` + pruneOwnedSQL},
	{"context_tenant_quota", "context_tenant_quota", `DELETE FROM context_tenant_quota WHERE scope IN ` + pruneOwnedSQL},
	{"context_recall_runs", "context_recall_runs", `DELETE FROM context_recall_runs WHERE scope IN ` + pruneOwnedSQL},
	{"graph_cluster_node", "graph_cluster_node", `DELETE FROM graph_cluster_node WHERE scope IN ` + pruneOwnedSQL},
	{"graph_cluster_edge", "graph_cluster_edge", `DELETE FROM graph_cluster_edge WHERE scope_s IN ` + pruneOwnedSQL + ` OR scope_t IN ` + pruneOwnedSQL},
	{"graph_cluster_topic_edge", "graph_cluster_topic_edge", `DELETE FROM graph_cluster_topic_edge WHERE scope_a IN ` + pruneOwnedSQL + ` OR scope_b IN ` + pruneOwnedSQL},
	{"graph_cluster_super_member", "graph_cluster_super_member", `DELETE FROM graph_cluster_super_member WHERE scope IN ` + pruneOwnedSQL},
	{"graph_cluster_super", "graph_cluster_super", `DELETE FROM graph_cluster_super WHERE scope IN ` + pruneOwnedSQL},
	{"graph_cluster_centroid", "graph_cluster_centroid", `DELETE FROM graph_cluster_centroid WHERE scope IN ` + pruneOwnedSQL},
	{"graph_cluster_topic", "graph_cluster_topic", `DELETE FROM graph_cluster_topic WHERE scope IN ` + pruneOwnedSQL},
	{"graph_overview_meta", "graph_overview_meta", `DELETE FROM graph_overview_meta WHERE scope IN ` + pruneOwnedSQL},
	{"graph_overview_run", "graph_overview_run", `DELETE FROM graph_overview_run WHERE scope_set && $1::text[] AND scope_set && ARRAY` + pruneOwnedSQL},
	{"context_settings_audit", "context_settings_audit", `DELETE FROM context_settings_audit WHERE entity_type = 'setting' AND scope IN ` + pruneOwnedSQL},
}

// pruneTablesSQL names which of the relations $1 exist. The prune runs on the
// schema the binary migrated to; tests of a migration run it on the schema of
// that migration, before some of these tables were created.
const pruneTablesSQL = `SELECT COALESCE(array_agg(c.relname::text), '{}') FROM pg_class c
 JOIN pg_namespace n ON n.oid = c.relnamespace
 WHERE n.nspname = current_schema() AND c.relname = ANY($1::text[])`

// pruneExisting is the set of the tables among rels that exist.
func pruneExisting(ctx context.Context, q pgxdb.Rower, rels []string) (map[string]bool, error) {
	var have []string
	if err := q.QueryRow(ctx, pruneTablesSQL, rels).Scan(&have); err != nil {
		return nil, fmt.Errorf("prune tables: %w", err)
	}
	out := make(map[string]bool, len(have))
	for _, r := range have {
		out[r] = true
	}
	return out, nil
}

// pruneSweepRels are the tables of pruneScopeSweeps and of pruneTenantLast.
func pruneSweepRels() []string {
	rels := []string{"context_block_counts", "context_block_count_deltas", "context_embed_excluded_queue"}
	for _, s := range pruneScopeSweeps {
		rels = append(rels, s.rel)
	}
	return rels
}

// pruneQuerier is what pruneSweep runs on: the pool (the pass) or the closing
// round's transaction.
type pruneQuerier interface {
	pgxdb.Execer
	pgxdb.Rower
}

// pruneSweep runs pruneScopeSweeps in q (a pool for the pass, the closing
// round's transaction) and returns the rows they deleted.
func pruneSweep(ctx context.Context, q pruneQuerier, tenantID string, scopes []string) (int64, error) {
	have, err := pruneExisting(ctx, q, pruneSweepRels())
	if err != nil {
		return 0, err
	}
	var n int64
	for _, s := range pruneScopeSweeps {
		if !have[s.rel] {
			continue
		}
		tag, err := q.Exec(ctx, s.sql, scopes, tenantID)
		if err != nil {
			return n, fmt.Errorf("%s: %w", s.table, err)
		}
		n += tag.RowsAffected()
	}
	return n, nil
}

// pruneCountsSQL deletes the block counter rows and the pending deltas of the
// scopes $1 the tenant $2 still owns (issue #555): once a scope's blocks are
// gone its counter rows stand at 0 and only the daily reconcile would remove
// them. Run in the closing round that finds nothing, after the last block of
// the scopes is gone, under the reconcile's scope locks and the fold's lock
// (pruneCountLocks), so no fold or reconcile writes a counter row of the
// scopes beside it.
const pruneCountsSQL = `WITH d AS (DELETE FROM context_block_count_deltas WHERE scope IN ` + pruneOwnedSQL + ` RETURNING 1)
DELETE FROM context_block_counts WHERE scope IN ` + pruneOwnedSQL

// pruneQueueSQL deletes the embed-exclusion reconciles queued for the tenant
// (grp 't:<tenant>') or one of its scopes (grp 's:<scope>'): the tenant's
// types and blocks are gone, and a reconcile of a scope name runs for the
// next owner on its own type writes. The group names the tenant as PostgreSQL
// writes a uuid (ctx_embed_excluded_group), which is the spelling PruneTenant
// passes on.
const pruneQueueSQL = `DELETE FROM context_embed_excluded_queue
 WHERE grp = 't:' || $2::text OR grp IN (SELECT 's:' || s FROM ` + pruneOwnedSQL + ` AS o(s))`

// pruneCountLocks takes, in tx, the block-count reconcile's lock of every
// scope (sorted) and then the fold's lock, in the order the reconcile takes
// them (correctScopeCounts): neither the fold nor the reconcile takes a
// supersedes scope lock, which the closing round holds before these.
func pruneCountLocks(ctx context.Context, tx pgx.Tx, scopes []string) error {
	for _, scope := range scopes {
		if _, err := tx.Exec(ctx, blockCountReconcileLockSQL, scope); err != nil {
			return fmt.Errorf("block count reconcile lock: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, blockCountPruneFoldLockSQL); err != nil {
		return fmt.Errorf("block count fold lock: %w", err)
	}
	return nil
}

// pruneTenantLast is what the closing round that finds nothing deletes before
// the tenant row: the counter rows of the scopes and the queued reconciles.
func pruneTenantLast(ctx context.Context, tx pgx.Tx, tenantID string, scopes []string) error {
	have, err := pruneExisting(ctx, tx, pruneSweepRels())
	if err != nil {
		return err
	}
	if have["context_block_counts"] && have["context_block_count_deltas"] {
		if err := pruneCountLocks(ctx, tx, scopes); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, pruneCountsSQL, scopes, tenantID); err != nil {
			return fmt.Errorf("block counts: %w", err)
		}
	}
	if have["context_embed_excluded_queue"] {
		if _, err := tx.Exec(ctx, pruneQueueSQL, scopes, tenantID); err != nil {
			return fmt.Errorf("embed exclusion queue: %w", err)
		}
	}
	return nil
}
