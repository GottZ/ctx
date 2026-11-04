//go:build integration

// Classification pin of the audited tables (W06-6, design/06 §3.3 M-06-3,
// B-06-2, K16). Every column of a table whose writes land in
// context_settings_audit is either AUDITED (its value is part of the
// whitelist projection audit_state_<table>(row) and so reaches old_value /
// new_value) or EXCLUDED with a stated reason. A column in neither list fails
// the pin: a new column — a future secret above all — never travels into the
// tenant-admin-readable audit table just because nobody looked at it, which
// is what a blacklist projection (to_jsonb(NEW) - 'key_hash') would do.
//
// TestAuditClassificationPin compares the catalog with the data below: the
// live column list of the table, the keys the whitelist function emits, and —
// where the UPDATE trigger carries a column list — that OF list and the
// column names in its WHEN clause. TestAuditClassificationPerColumn binds the
// behaviour: an UPDATE of exactly one audited column writes exactly one row
// whose new_value carries that column's real value and differs from old_value
// in that key only; an UPDATE of exactly one excluded column writes none. A
// WHEN clause that names a column without comparing it, or a projection that
// emits the right key with the wrong value, passes the catalog check and fails
// this one.
//
// Since migration 161 (W01-1) the model consumers and model keys are
// classified too; their identity columns (consumer tenant_id, key
// consumer_id) are immutable by trigger, so their per-column probe proves the
// refusal and then the audit row with that trigger disabled. Since migration
// 162 (W02-03) the model aliases are, with the same probe for their three
// identity columns scope, name and kind. Since migration 163 (F-W2) the
// instance identity is: a singleton without an id column (its probes address
// the row by singleton) whose secret is excluded — the HMAC key of the hop
// tokens never reaches an audit row. Completeness:
// every table carrying a trigger that writes context_settings_audit is either
// classified here or named in auditLegacyTables — a new audited table without
// an entry turns TestAuditClassificationPin red.
//
// CTX_M160_PROBE_COLUMN=1 adds a column secret_probe to context_api_keys
// before the pin runs: the red run of Negativ 3. The subtest
// probe_column_turns_the_pin_red checks the same on a database of its own in
// every run. TestAuditClassificationPerColumn honours CTX_M160_OVERLAY
// (when-hole, value-swap), CTX_M161_OVERLAY, CTX_M162_OVERLAY
// (no-of-list, no-identity) and CTX_M163_OVERLAY (no-of-list) for its red
// runs; the blacklist overlays of 161, 162 and 163 stay green here (their
// projections carry every audited value) and turn
// TestModelAudit/state_is_the_whitelist,
// TestModelAliasAudit/state_is_the_whitelist and
// TestInstanceAudit/rotation_writes_one_row_without_the_secret red instead.
//
//	go test -tags=integration -p 1 -count=1 -run TestAuditClassification ./internal/store/
package store_test

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// auditedTable is the classification of one audited table.
type auditedTable struct {
	table    string
	stateFn  string            // whitelist projection audit_state_<table>(row)
	audited  []string          // columns whose values the projection carries
	excluded map[string]string // column -> why it never reaches the audit row
	updTrig  string            // UPDATE trigger whose OF list and WHEN must name exactly `audited` ("" = none)
}

var auditClassification = []auditedTable{
	{
		table:   "context_api_keys",
		stateFn: "audit_state_api_key",
		audited: []string{
			"label", "home_scope", "allowed_scopes", "write_scopes", "active", "is_admin",
			"tenant_id", "tenant_role", "confirm_writes", "principal_id",
		},
		excluded: map[string]string{
			"id":           "identity of the row, written as entity_key",
			"key_hash":     "credential: SHA-256 of the plaintext key, never leaves the table",
			"last_used_at": "per-request bookkeeping of ctx_auth_by_id (B-06-1)",
			"created_at":   "insert time; the audit row carries its own created_at",
			"updated_at":   "bookkeeping time the store writes next to an audited change",
		},
		updTrig: "trg_api_keys_audit_upd",
	},
	{
		table:   "context_tenant_quota",
		stateFn: "audit_state_tenant_quota",
		audited: []string{
			"scope", "daily_cost_usd", "monthly_cost_usd", "daily_calls", "on_exceed", "enabled",
		},
		excluded: map[string]string{
			"id":                   "surrogate key the API never exposes; a quota is addressed by scope (entity_key)",
			"created_at":           "insert time; the audit row carries its own created_at",
			"updated_at":           "bookkeeping time of the last write",
			"updated_by":           "actor attribution, carried by api_key_id / actor_label (and the trigger's actor fallback)",
			"updated_by_principal": "actor attribution, carried by metadata.actor_principal",
			"metadata":             "free-form JSONB that no writer sets; forwarding it would copy any future content unreviewed",
		},
	},
	{
		table:   "context_model_consumers",
		stateFn: "audit_state_model_consumer",
		audited: []string{
			"tenant_id", "label", "description", "kind", "tenant_scope", "all_models",
			"prompt_sensitivity", "fair_weight", "admission_lane", "allowed_cidrs",
			"allow_plaintext", "active", "peer_instance_id", "peer_bound_at",
		},
		excluded: map[string]string{
			"id":                   "identity of the row, written as entity_key and metadata.consumer_id",
			"created_at":           "insert time; the audit row carries its own created_at",
			"updated_at":           "bookkeeping time the store writes next to an audited change",
			"created_by_key":       "actor attribution, carried by api_key_id / actor_label; ON DELETE SET NULL must not write a row",
			"created_by_principal": "actor attribution, carried by metadata.actor_principal",
		},
		updTrig: "trg_model_consumers_audit_upd",
	},
	{
		table:   "context_model_keys",
		stateFn: "audit_state_model_key",
		audited: []string{"consumer_id", "key_hint", "expires_at", "revoked_at", "rotated_from"},
		excluded: map[string]string{
			"id":                   "identity of the row, written as entity_key",
			"key_hash":             "credential: SHA-256 of the model key, never leaves the table",
			"last_used_at":         "usage stamp of the batched flush; one audit row per flush would be noise",
			"created_at":           "insert time; the audit row carries its own created_at",
			"created_by_key":       "actor attribution, carried by api_key_id / actor_label; ON DELETE SET NULL must not write a row",
			"created_by_principal": "actor attribution, carried by metadata.actor_principal",
			"revoked_by_key":       "actor attribution of the revoke row (api_key_id / actor_label); ON DELETE SET NULL must not write a row",
			"revoked_by_principal": "actor attribution of the revoke row, carried by metadata.actor_principal",
		},
		updTrig: "trg_model_keys_audit_upd",
	},
	{
		table:   "context_model_aliases",
		stateFn: "audit_state_model_alias",
		audited: []string{
			"scope", "name", "kind", "role", "sensitivity", "policy", "description", "enabled",
		},
		excluded: map[string]string{
			"id":         "identity of the row, written as entity_key",
			"created_at": "insert time; the audit row carries its own created_at",
			"updated_at": "bookkeeping time the store writes next to an audited change",
		},
		updTrig: "trg_model_aliases_audit_upd",
	},
	{
		// No UPDATE trigger: the identity pin of 164 keeps both audited
		// columns, so a grant is only ever inserted or deleted, and those two
		// rows carry its state.
		table:   "context_model_consumer_grants",
		stateFn: "audit_state_model_grant",
		audited: []string{"consumer_id", "alias_id"},
		excluded: map[string]string{
			"created_at":           "insert time; the audit row carries its own created_at",
			"created_by_key":       "actor attribution, carried by api_key_id / actor_label; ON DELETE SET NULL must not write a row",
			"created_by_principal": "actor attribution, carried by metadata.actor_principal",
		},
	},
	{
		table:   "context_instance",
		stateFn: "audit_state_instance",
		audited: []string{"instance_id", "created_at", "rotated_at"},
		excluded: map[string]string{
			"singleton":       "row key of the one-row table (CHECK singleton); the audit row's entity_key is 'self'",
			"instance_secret": "HMAC key of the hop tokens (design/04 §4.1): never leaves the table — no log, no status, no audit row",
		},
		updTrig: "trg_instance_audit_upd",
	},
}

// auditLegacyTables carry audit triggers from before the whitelist doctrine
// (K16). Their rows stay off the tenant-admin read surface (GET /api/audit
// does not serve their entity types; settings keep their own surface), so
// they are named here instead of classified column by column. Every other
// table whose trigger writes context_settings_audit must be in
// auditClassification — a new audited table without an entry fails
// TestAuditClassificationPin.
var auditLegacyTables = map[string]string{
	"context_settings":                 "051 audit_settings_write; history on GET /api/settings/{key}, server-admin surfaces only",
	"context_secrets":                  "051 audit_settings_write; values are never recorded (NULL old/new)",
	"context_backends":                 "backend audit; admin-only manage surface",
	"context_block_types":              "block-type audit; server-admin surface",
	"context_disable_profiles":         "disable-profile audit; server-admin surface",
	"context_disable_profile_backends": "disable-profile audit; server-admin surface",
	"context_graph_category_hues":      "graph category hue audit; server-admin surface",
}

// checkAuditedTablesClassified compares the tables carrying a trigger whose
// function writes context_settings_audit with auditClassification ∪
// auditLegacyTables (one line per mismatch).
func checkAuditedTablesClassified(ctx context.Context, pool *pgxpool.Pool) []string {
	live, err := stringList(ctx, pool,
		`SELECT DISTINCT t.tgrelid::regclass::text
		   FROM pg_trigger t JOIN pg_proc p ON p.oid = t.tgfoid
		  WHERE NOT t.tgisinternal AND p.prosrc ILIKE '%context_settings_audit%'
		  ORDER BY 1`)
	if err != nil {
		return []string{fmt.Sprintf("read audited tables: %v", err)}
	}
	known := map[string]bool{}
	for _, c := range auditClassification {
		known[c.table] = true
	}
	var bad []string
	liveSet := map[string]bool{}
	for _, tbl := range live {
		liveSet[tbl] = true
		if _, legacy := auditLegacyTables[tbl]; !known[tbl] && !legacy {
			bad = append(bad, fmt.Sprintf("%s: an audit trigger writes context_settings_audit, but the table is not in auditClassification — classify its columns", tbl))
		}
	}
	for tbl := range known {
		if !liveSet[tbl] {
			bad = append(bad, fmt.Sprintf("%s: classified, but no trigger of it writes context_settings_audit", tbl))
		}
	}
	sort.Strings(bad)
	return bad
}

// auditedSet returns the audited columns of the classification for table.
func auditedSet(table string) map[string]bool {
	out := map[string]bool{}
	for _, c := range auditClassification {
		if c.table == table {
			for _, col := range c.audited {
				out[col] = true
			}
		}
	}
	return out
}

// checkAuditClassification compares the live schema with the classification
// and returns one line per mismatch (empty = pinned).
func checkAuditClassification(ctx context.Context, pool *pgxpool.Pool) []string {
	var bad []string
	for _, c := range auditClassification {
		audited := map[string]bool{}
		for _, col := range c.audited {
			audited[col] = true
			if _, both := c.excluded[col]; both {
				bad = append(bad, fmt.Sprintf("%s: column %s is listed as audited AND excluded", c.table, col))
			}
		}

		// (1) live columns == audited ∪ excluded.
		live, err := stringList(ctx, pool,
			`SELECT column_name::text FROM information_schema.columns
			  WHERE table_schema = current_schema() AND table_name = $1 ORDER BY column_name`, c.table)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: read columns: %v", c.table, err))
			continue
		}
		liveSet := map[string]bool{}
		for _, col := range live {
			liveSet[col] = true
			if _, ex := c.excluded[col]; !audited[col] && !ex {
				bad = append(bad, fmt.Sprintf("%s: column %s is neither audited nor excluded — classify it", c.table, col))
			}
		}
		for col := range audited {
			if !liveSet[col] {
				bad = append(bad, fmt.Sprintf("%s: audited column %s does not exist", c.table, col))
			}
		}
		for col := range c.excluded {
			if !liveSet[col] {
				bad = append(bad, fmt.Sprintf("%s: excluded column %s does not exist", c.table, col))
			}
		}

		// (2) the whitelist projection emits exactly the audited keys. A NULL row
		// still yields every key (jsonb_build_object keeps NULL values).
		keys, err := stringList(ctx, pool, fmt.Sprintf(
			`SELECT k FROM jsonb_object_keys(%s(NULL::%s)) AS k ORDER BY k`, c.stateFn, c.table))
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: %s(): %v", c.table, c.stateFn, err))
		} else if got, want := strings.Join(keys, ","), sortedJoin(c.audited); got != want {
			bad = append(bad, fmt.Sprintf("%s: %s() emits keys [%s], audited columns are [%s]", c.table, c.stateFn, got, want))
		}

		// (3) the UPDATE trigger fires on exactly the audited columns and its
		// WHEN clause compares every one of them.
		if c.updTrig == "" {
			continue
		}
		ofCols, err := stringList(ctx, pool,
			`SELECT a.attname::text
			   FROM pg_trigger t
			   JOIN pg_attribute a ON a.attrelid = t.tgrelid AND a.attnum = ANY (t.tgattr)
			  WHERE t.tgname = $1 AND t.tgrelid = $2::regclass
			  ORDER BY a.attname`, c.updTrig, c.table)
		if err != nil {
			bad = append(bad, fmt.Sprintf("%s: read OF list of %s: %v", c.table, c.updTrig, err))
			continue
		}
		if got, want := strings.Join(ofCols, ","), sortedJoin(c.audited); got != want {
			bad = append(bad, fmt.Sprintf("%s: %s fires on [%s], audited columns are [%s]", c.table, c.updTrig, got, want))
		}
		var def string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_triggerdef(oid) FROM pg_trigger WHERE tgname = $1 AND tgrelid = $2::regclass`,
			c.updTrig, c.table).Scan(&def); err != nil {
			bad = append(bad, fmt.Sprintf("%s: read %s: %v", c.table, c.updTrig, err))
			continue
		}
		for _, col := range c.audited {
			if !strings.Contains(def, "old."+col) || !strings.Contains(def, "new."+col) {
				bad = append(bad, fmt.Sprintf("%s: WHEN clause of %s does not compare %s: %s", c.table, c.updTrig, col, def))
			}
		}
	}
	return bad
}

func sortedJoin(in []string) string {
	s := append([]string(nil), in...)
	sort.Strings(s)
	return strings.Join(s, ",")
}

func stringList(ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) ([]string, error) {
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func TestAuditClassificationPin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)

	if os.Getenv("CTX_M160_PROBE_COLUMN") != "" {
		if _, err := pool.Exec(ctx, `ALTER TABLE context_api_keys ADD COLUMN secret_probe TEXT`); err != nil {
			t.Fatalf("add probe column: %v", err)
		}
		t.Log("CTX_M160_PROBE_COLUMN: context_api_keys.secret_probe added before the pin")
	}
	if bad := checkAuditClassification(ctx, pool); len(bad) > 0 {
		t.Fatalf("audit classification pin: %d mismatch(es):\n  %s", len(bad), strings.Join(bad, "\n  "))
	}
	if bad := checkAuditedTablesClassified(ctx, pool); len(bad) > 0 {
		t.Fatalf("audited tables: %d mismatch(es):\n  %s", len(bad), strings.Join(bad, "\n  "))
	}

	// Negativ 3 in every run: an unclassified column turns the pin red.
	t.Run("probe_column_turns_the_pin_red", func(t *testing.T) {
		probe := testdb.SetupTestDB(t)
		if _, err := probe.Exec(ctx, `ALTER TABLE context_api_keys ADD COLUMN secret_probe TEXT`); err != nil {
			t.Fatalf("add probe column: %v", err)
		}
		bad := checkAuditClassification(ctx, probe)
		want := "context_api_keys: column secret_probe is neither audited nor excluded — classify it"
		found := false
		for _, b := range bad {
			found = found || b == want
		}
		if !found || len(bad) != 1 {
			t.Fatalf("probe column: pin reported %v, want exactly [%s]", bad, want)
		}
	})
}

// auditColumnProbes: per audited table, for every column an UPDATE expression
// that changes exactly that column. %[1]s is a second tenant id, %[2]s a second
// principal id, %[3]s a key id, %[4]s a second model consumer id, %[5]s a
// second model key id, %[6]s a second model alias id (all existing FK
// targets). The audited probes must cover
// every audited column, the excluded probes every excluded column but the row
// identity.
//
// prep: a statement (same placeholders, $1 = the row id) run before a probe
// and not counted — it brings the row into a state in which the probe column
// alone may change (a CHECK couples kind and the peer binding). pinned: a
// column an identity trigger keeps immutable (23514); its probe first proves
// the refusal (no row), then runs once with that trigger disabled in its own
// transaction and expects the one audit row, so the audit covers the column
// even past the pin.
// keyCol/keyType address the probed row (default id/uuid; the instance
// singleton has no id column, a grant is addressed by its consumer, which
// holds exactly one). insDelOnly: the table has no UPDATE audit trigger —
// every audited column is pinned, a row is only inserted or deleted (those
// rows carry its state, gated in the table's own tests) — so a pinned probe
// proves the refusal and nothing past it: with the pin disabled the change
// writes no row either.
type auditProbeSet struct {
	audited, excluded map[string]string
	prep              map[string]string
	pinned            map[string]string // column -> identity trigger
	keyCol, keyType   string
	insDelOnly        bool
}

// rowKey returns the key column and its type of a probe set.
func (p auditProbeSet) rowKey() (col, typ string) {
	if p.keyCol == "" {
		return "id", "uuid"
	}
	return p.keyCol, p.keyType
}

var auditColumnProbes = map[string]auditProbeSet{
	"context_api_keys": {
		audited: map[string]string{
			"label":          "label || '-p'",
			"home_scope":     "home_scope || 'p'",
			"allowed_scopes": "allowed_scopes || '{pinprobe}'::text[]",
			"write_scopes":   "write_scopes || '{pinprobe}'::text[]",
			"active":         "NOT active",
			"is_admin":       "NOT is_admin",
			"tenant_id":      "'%[1]s'::uuid",
			"tenant_role":    "CASE WHEN tenant_role = 'member' THEN 'admin' ELSE 'member' END",
			"confirm_writes": "NOT confirm_writes",
			"principal_id":   "'%[2]s'::uuid",
		},
		excluded: map[string]string{
			"key_hash":     "key_hash || 'p'",
			"last_used_at": "now() - interval '1 minute'",
			"created_at":   "created_at - interval '1 minute'",
			"updated_at":   "updated_at - interval '1 minute'",
		},
	},
	"context_tenant_quota": {
		audited: map[string]string{
			"scope":            "scope || 'p'",
			"daily_cost_usd":   "COALESCE(daily_cost_usd, 0) + 1",
			"monthly_cost_usd": "COALESCE(monthly_cost_usd, 0) + 1",
			"daily_calls":      "COALESCE(daily_calls, 0) + 1",
			"on_exceed":        "CASE WHEN on_exceed = 'block' THEN 'external_off' ELSE 'block' END",
			"enabled":          "NOT enabled",
		},
		excluded: map[string]string{
			"created_at":           "created_at - interval '1 minute'",
			"updated_at":           "updated_at - interval '1 minute'",
			"updated_by":           "'%[3]s'::uuid",
			"updated_by_principal": "'%[2]s'::uuid",
			"metadata":             `metadata || '{"pin": "probe"}'::jsonb`,
		},
	},
	"context_model_consumers": {
		audited: map[string]string{
			"tenant_id":          "'%[1]s'::uuid",
			"label":              "label || '-p'",
			"description":        "description || 'p'",
			"kind":               "CASE WHEN kind = 'app' THEN 'ctx-peer' ELSE 'app' END",
			"tenant_scope":       "COALESCE(tenant_scope, 'pinprobe') || 'p'",
			"all_models":         "NOT all_models",
			"prompt_sensitivity": "CASE WHEN prompt_sensitivity = 'credentials' THEN 'public' ELSE 'credentials' END",
			"fair_weight":        "fair_weight % 100 + 1",
			"admission_lane":     "CASE WHEN admission_lane = 'bulk' THEN 'urgent' ELSE 'bulk' END",
			"allowed_cidrs":      "COALESCE(allowed_cidrs, '{}'::cidr[]) || '10.9.0.0/16'::cidr",
			"allow_plaintext":    "NOT allow_plaintext",
			"active":             "NOT active",
			"peer_instance_id":   "gen_random_uuid()",
			"peer_bound_at":      "peer_bound_at - interval '1 minute'",
		},
		excluded: map[string]string{
			"created_at":           "created_at - interval '1 minute'",
			"updated_at":           "updated_at - interval '1 minute'",
			"created_by_key":       "'%[3]s'::uuid",
			"created_by_principal": "'%[2]s'::uuid",
		},
		prep: map[string]string{
			// kind may only flip on an unbound consumer; the binding columns
			// only change pairwise-valid on a bound ctx-peer.
			"kind":             "UPDATE context_model_consumers SET peer_instance_id = NULL, peer_bound_at = NULL WHERE id = $1::uuid",
			"peer_instance_id": "UPDATE context_model_consumers SET kind = 'ctx-peer', peer_instance_id = gen_random_uuid(), peer_bound_at = now() WHERE id = $1::uuid",
			"peer_bound_at":    "UPDATE context_model_consumers SET kind = 'ctx-peer', peer_instance_id = gen_random_uuid(), peer_bound_at = now() WHERE id = $1::uuid",
		},
		pinned: map[string]string{"tenant_id": "trg_model_consumers_identity"},
	},
	"context_model_keys": {
		audited: map[string]string{
			"consumer_id":  "'%[4]s'::uuid",
			"key_hint":     "key_hint || 'p'",
			"expires_at":   "COALESCE(expires_at, now()) + interval '1 day'",
			"revoked_at":   "COALESCE(revoked_at, now()) + interval '1 second'",
			"rotated_from": "'%[5]s'::uuid",
		},
		excluded: map[string]string{
			"key_hash":             "md5(key_hash) || md5(key_hash)",
			"last_used_at":         "now() - interval '1 minute'",
			"created_at":           "created_at - interval '1 minute'",
			"created_by_key":       "'%[3]s'::uuid",
			"created_by_principal": "'%[2]s'::uuid",
			"revoked_by_key":       "'%[3]s'::uuid",
			"revoked_by_principal": "'%[2]s'::uuid",
		},
		pinned: map[string]string{"consumer_id": "trg_model_keys_identity"},
	},
	"context_model_aliases": {
		audited: map[string]string{
			"scope":       "scope || 'p'",
			"name":        "name || '-p'",
			"kind":        "CASE WHEN kind = 'chat' THEN 'embedding' ELSE 'chat' END",
			"role":        "role || '-p'",
			"sensitivity": "CASE WHEN sensitivity = 'credentials' THEN 'public' ELSE 'credentials' END",
			"policy":      `policy || '{"max_n": 2}'::jsonb`,
			"description": "description || 'p'",
			"enabled":     "NOT enabled",
		},
		excluded: map[string]string{
			"created_at": "created_at - interval '1 minute'",
			"updated_at": "updated_at - interval '1 minute'",
		},
		pinned: map[string]string{
			"scope": "trg_model_aliases_identity",
			"name":  "trg_model_aliases_identity",
			"kind":  "trg_model_aliases_identity",
		},
	},
	"context_model_consumer_grants": {
		audited: map[string]string{
			"consumer_id": "'%[4]s'::uuid",
			"alias_id":    "'%[6]s'::uuid",
		},
		excluded: map[string]string{
			"created_at":           "created_at - interval '1 minute'",
			"created_by_key":       "'%[3]s'::uuid",
			"created_by_principal": "'%[2]s'::uuid",
		},
		pinned: map[string]string{
			"consumer_id": "trg_model_grants_identity",
			"alias_id":    "trg_model_grants_identity",
		},
		keyCol:     "consumer_id",
		keyType:    "uuid",
		insDelOnly: true,
	},
	"context_instance": {
		audited: map[string]string{
			"instance_id": "gen_random_uuid()",
			"created_at":  "created_at - interval '1 minute'",
			"rotated_at":  "COALESCE(rotated_at, now()) + interval '1 second'",
		},
		excluded: map[string]string{
			"instance_secret": "gen_random_bytes(32)",
		},
		keyCol:  "singleton",
		keyType: "boolean",
	},
}

func TestAuditClassificationPerColumn(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	// 160 (honouring CTX_M160_OVERLAY), then 161 (honouring CTX_M161_OVERLAY),
	// then 162 (honouring CTX_M162_OVERLAY), then 163 (CTX_M163_OVERLAY),
	// then 164 (CTX_M164_OVERLAY).
	pool := kaSetup(t, nil)
	m161Apply(ctx, t, pool)
	m162Apply(ctx, t, pool)
	m163Apply(ctx, t, pool)
	m164Apply(ctx, t, pool)

	otherTenant := freshTenant(t, ctx, pool, "ka-pin-other")
	var otherPrincipal string
	if err := pool.QueryRow(ctx, `INSERT INTO context_principals (display_name) VALUES ('ka-pin-principal') RETURNING id::text`).Scan(&otherPrincipal); err != nil {
		t.Fatalf("insert principal: %v", err)
	}
	key, _, err := store.CreateApiKey(ctx, pool, "ka-pin-key", "private", nil, "")
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	var quotaID string
	if err := pool.QueryRow(ctx, `INSERT INTO context_tenant_quota (scope, daily_calls) VALUES ('ka-pin:main', 1) RETURNING id::text`).Scan(&quotaID); err != nil {
		t.Fatalf("insert quota: %v", err)
	}
	// Model consumer A (the probed row: a bound ctx-peer with a private
	// allowlist, so every probe has a valid target state) and consumer B, which
	// owns the probed key and a second key (rotated_from target).
	modelTenant, _ := mcTenant(ctx, t, pool, "ka-pin-model", "active")
	consumerA := mcConsumer(ctx, t, pool, modelTenant, "ka-pin-a", map[string]string{
		"kind": "'ctx-peer'", "peer_instance_id": "gen_random_uuid()", "peer_bound_at": "now()",
		"allowed_cidrs": "'{10.1.0.0/16}'"})
	consumerB := mcConsumer(ctx, t, pool, modelTenant, "ka-pin-b", nil)
	modelKey, _ := mcKey(ctx, t, pool, consumerB, "ka-pin-key", nil)
	secondModelKey, _ := mcKey(ctx, t, pool, consumerB, "ka-pin-key-2", nil)

	// The probed model alias: a tenant alias (scope 'ka-pin:main'), so the
	// scope probe moves it to another tenant scope.
	modelAlias := maAlias(ctx, t, pool, "ka-pin:main", "ka-pin-alias", nil)
	// The probed grant: the one grant of consumer G (its address), on a
	// _global alias; a second alias is the alias_id probe's target.
	grantAlias := maAlias(ctx, t, pool, "_global", "ka-pin-grant", nil)
	secondAlias := maAlias(ctx, t, pool, "_global", "ka-pin-grant-2", nil)
	consumerG := mcConsumer(ctx, t, pool, modelTenant, "ka-pin-g", nil)
	mgGrant(ctx, t, pool, consumerG, grantAlias)

	rowID := map[string]string{"context_api_keys": key.ID, "context_tenant_quota": quotaID,
		"context_model_consumers": consumerA, "context_model_keys": modelKey, "context_model_aliases": modelAlias,
		"context_model_consumer_grants": consumerG, "context_instance": "true"}
	entity := map[string]string{"context_api_keys": "api_key", "context_tenant_quota": "tenant_quota",
		"context_model_consumers": "model_consumer", "context_model_keys": "model_key", "context_model_aliases": "model_alias",
		"context_model_consumer_grants": "model_grant", "context_instance": "instance"}
	fill := func(expr string) string {
		if !strings.Contains(expr, "%[") {
			return expr
		}
		return fmt.Sprintf(expr, otherTenant, otherPrincipal, key.ID, consumerA, secondModelKey, secondAlias)
	}

	for _, c := range auditClassification {
		probes, ok := auditColumnProbes[c.table]
		if !ok {
			t.Fatalf("%s: no column probes — add them to auditColumnProbes", c.table)
		}
		// Coverage: a column added to either list needs its probe.
		if got, want := sortedKeys(probes.audited), sortedJoin(c.audited); got != want {
			t.Errorf("%s: audited probes [%s], audited columns [%s]", c.table, got, want)
		}
		// insDelOnly holds only while every audited column is pinned: an
		// audited column without pin and without UPDATE trigger would change
		// with no audit row while the probe below stays green.
		if probes.insDelOnly {
			for _, col := range c.audited {
				if _, ok := probes.pinned[col]; !ok {
					t.Errorf("%s: insDelOnly, but audited column %s has no identity pin", c.table, col)
				}
			}
		}
		keyCol, keyType := probes.rowKey()
		var excl []string
		for col := range c.excluded {
			if col != keyCol {
				excl = append(excl, col)
			}
		}
		if got, want := sortedKeys(probes.excluded), sortedJoin(excl); got != want {
			t.Errorf("%s: excluded probes [%s], excluded columns but %s [%s]", c.table, got, keyCol, want)
		}
		where := fmt.Sprintf("%s = $1::%s", keyCol, keyType)

		run := func(col, expr string, wantRows int) {
			t.Helper()
			if prep, ok := probes.prep[col]; ok {
				if _, err := pool.Exec(ctx, fill(prep), rowID[c.table]); err != nil {
					t.Fatalf("%s.%s: prep: %v", c.table, col, err)
				}
			}
			before := kaCount(ctx, t, pool, `entity_type = $1`, entity[c.table])
			update := fmt.Sprintf(`UPDATE %s SET %s = %s WHERE %s`, c.table, col, fill(expr), where)
			if trig, ok := probes.pinned[col]; ok {
				// The pin refuses the change and nothing is written ...
				if _, err := pool.Exec(ctx, update, rowID[c.table]); mcSQLState(err) != "23514" {
					t.Errorf("%s.%s: pinned column changed: err %v, want SQLSTATE 23514 from %s", c.table, col, err, trig)
				}
				if got := kaCount(ctx, t, pool, `entity_type = $1`, entity[c.table]) - before; got != 0 {
					t.Errorf("%s.%s: refused change wrote %d audit row(s), want 0", c.table, col, got)
				}
				if probes.insDelOnly {
					return // no UPDATE audit trigger: nothing past the pin to record
				}
				// ... and past the pin the audit trigger still records it.
				kaExecInTx(ctx, t, pool, fmt.Sprintf(`ALTER TABLE %[1]s DISABLE TRIGGER %[2]s;
					%[3]s;
					ALTER TABLE %[1]s ENABLE TRIGGER %[2]s;`, c.table, trig, strings.Replace(update, "$1::", "'"+rowID[c.table]+"'::", 1)))
			} else if tag, err := pool.Exec(ctx, update, rowID[c.table]); err != nil {
				t.Fatalf("%s.%s: update: %v", c.table, col, err)
			} else if tag.RowsAffected() != 1 {
				t.Fatalf("%s.%s: the probe updated %d row(s), want exactly the probed one", c.table, col, tag.RowsAffected())
			}
			if got := kaCount(ctx, t, pool, `entity_type = $1`, entity[c.table]) - before; got != wantRows {
				t.Errorf("%s.%s: an update of only this column wrote %d audit row(s), want %d", c.table, col, got, wantRows)
				return
			}
			if wantRows == 0 {
				return
			}
			// The newest row carries the column's real value, and old/new differ
			// in this key only.
			var valueMatches, changed, restEqual bool
			var newRaw, rowRaw string
			if err := pool.QueryRow(ctx, fmt.Sprintf(
				`SELECT COALESCE((a.new_value -> $2) = (to_jsonb(t) -> $2), false),
				        (a.old_value -> $2) IS DISTINCT FROM (a.new_value -> $2),
				        (a.old_value - $2) = (a.new_value - $2),
				        COALESCE((a.new_value -> $2)::text, '<absent>'), COALESCE((to_jsonb(t) -> $2)::text, '<absent>')
				   FROM %s t,
				        LATERAL (SELECT old_value, new_value FROM context_settings_audit
				                  WHERE entity_type = $3 ORDER BY created_at DESC, id DESC LIMIT 1) a
				  WHERE t.%s`, c.table, where), rowID[c.table], col, entity[c.table]).
				Scan(&valueMatches, &changed, &restEqual, &newRaw, &rowRaw); err != nil {
				t.Fatalf("%s.%s: read row: %v", c.table, col, err)
			}
			if !valueMatches || !changed || !restEqual {
				t.Errorf("%s.%s: new_value carries %s, the row holds %s; changed=%v, other keys equal=%v",
					c.table, col, newRaw, rowRaw, changed, restEqual)
			}
		}
		for _, col := range c.audited {
			if expr, ok := probes.audited[col]; ok {
				want := 1
				if probes.insDelOnly {
					want = 0
				}
				run(col, expr, want)
			}
		}
		for col, expr := range probes.excluded {
			run(col, expr, 0)
		}
	}
}

func sortedKeys(m map[string]string) string {
	s := make([]string, 0, len(m))
	for k := range m {
		s = append(s, k)
	}
	return sortedJoin(s)
}
