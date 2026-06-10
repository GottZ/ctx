//go:build integration

// Integration tests for migration 051 (context_settings, context_settings_audit,
// context_secrets) and the store CRUD in settings.go/sealbox.go, against a
// real PG18 testcontainer.
//
// Covers the G02 gate probes:
//   - _migrations records version 51
//   - trigger audit, API shape: SET LOCAL actor => api_key_id + actor_label + via='api'
//   - trigger audit, psql shape: naked SQL write => api_key_id NULL + via='sql'
//   - secret audit rows: old_value/new_value ALWAYS NULL — negatively probed
//     (a naive audit function demonstrably leaks ciphertext into the audit
//     table; the 051 function does not)
//   - NOTIFY ctx_settings_write fires on BOTH tables, payload carries no values
//   - audit attribution survives hard deletion of the acting api key (no FK)
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestSettingsStore -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// auditRow mirrors one context_settings_audit row for assertions.
type auditRow struct {
	EntityType string
	EntityKey  string
	Scope      string
	Action     string
	OldValue   *string
	NewValue   *string
	ApiKeyID   *string
	ActorLabel *string
	Via        *string
	RequestID  *string
}

func lastAudit(t *testing.T, pool *pgxpool.Pool, entityKey string) auditRow {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var a auditRow
	err := pool.QueryRow(ctx,
		`SELECT entity_type, entity_key, scope, action, old_value::text, new_value::text,
		        api_key_id::text, actor_label, metadata->>'via', metadata->>'request_id'
		 FROM context_settings_audit
		 WHERE entity_key = $1
		 ORDER BY created_at DESC, id DESC
		 LIMIT 1`, entityKey,
	).Scan(&a.EntityType, &a.EntityKey, &a.Scope, &a.Action, &a.OldValue, &a.NewValue,
		&a.ApiKeyID, &a.ActorLabel, &a.Via, &a.RequestID)
	if err != nil {
		t.Fatalf("fetch last audit row for %s: %v", entityKey, err)
	}
	return a
}

func auditCount(t *testing.T, pool *pgxpool.Pool, entityKey string) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_settings_audit WHERE entity_key = $1`, entityKey,
	).Scan(&n); err != nil {
		t.Fatalf("count audit rows for %s: %v", entityKey, err)
	}
	return n
}

func TestSettingsStore_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// Actor key for the API-shaped probes.
	actor, _, err := store.CreateApiKey(ctx, pool, "settings-it-actor", "private", nil)
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}

	t.Run("MigrationVersion51Recorded", func(t *testing.T) {
		var filename string
		err := pool.QueryRow(ctx,
			`SELECT filename FROM _migrations WHERE version = 51`).Scan(&filename)
		if err != nil {
			t.Fatalf("migration 51 not recorded: %v", err)
		}
		if filename != "051_settings_store.sql" {
			t.Errorf("version 51 filename = %q", filename)
		}
	})

	t.Run("UpsertSetting_ApiShape_AuditAndAttribution", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // rollback after commit is a no-op

		if err := store.SetTxRequestID(ctx, tx, "deadbeef01"); err != nil {
			t.Fatalf("set request id: %v", err)
		}
		if err := store.UpsertSetting(ctx, tx, "rerank.blend_weight", store.GlobalScope,
			json.RawMessage(`0.7`), &actor.ID); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		a := lastAudit(t, pool, "rerank.blend_weight")
		if a.EntityType != "setting" || a.Action != "set" {
			t.Errorf("audit shape = %s/%s, want setting/set", a.EntityType, a.Action)
		}
		if a.OldValue != nil {
			t.Errorf("old_value on create = %v, want NULL", *a.OldValue)
		}
		if a.NewValue == nil || *a.NewValue != "0.7" {
			t.Errorf("new_value = %v, want 0.7", a.NewValue)
		}
		if a.ApiKeyID == nil || *a.ApiKeyID != actor.ID {
			t.Errorf("api_key_id = %v, want %s", a.ApiKeyID, actor.ID)
		}
		if a.ActorLabel == nil || *a.ActorLabel != "settings-it-actor" {
			t.Errorf("actor_label = %v, want settings-it-actor", a.ActorLabel)
		}
		if a.Via == nil || *a.Via != "api" {
			t.Errorf("via = %v, want api", a.Via)
		}
		if a.RequestID == nil || *a.RequestID != "deadbeef01" {
			t.Errorf("request_id = %v, want deadbeef01", a.RequestID)
		}
	})

	t.Run("UpsertSetting_UpdatePath_OldAndNewValue", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := store.UpsertSetting(ctx, tx, "rerank.blend_weight", store.GlobalScope,
			json.RawMessage(`0.9`), &actor.ID); err != nil {
			t.Fatalf("upsert update: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		a := lastAudit(t, pool, "rerank.blend_weight")
		if a.Action != "set" {
			t.Errorf("action = %s, want set", a.Action)
		}
		if a.OldValue == nil || *a.OldValue != "0.7" {
			t.Errorf("old_value = %v, want 0.7", a.OldValue)
		}
		if a.NewValue == nil || *a.NewValue != "0.9" {
			t.Errorf("new_value = %v, want 0.9", a.NewValue)
		}

		// Only one override row exists (UNIQUE(key, scope) upsert identity).
		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings WHERE key = 'rerank.blend_weight'`,
		).Scan(&rows); err != nil {
			t.Fatalf("count: %v", err)
		}
		if rows != 1 {
			t.Errorf("override rows = %d, want 1", rows)
		}
	})

	t.Run("LoadSettingOverrides_ReadsBack", func(t *testing.T) {
		overrides, err := store.LoadSettingOverrides(ctx, pool, store.GlobalScope)
		if err != nil {
			t.Fatalf("load: %v", err)
		}
		var found bool
		for _, o := range overrides {
			if o.Key == "rerank.blend_weight" {
				found = true
				if string(o.Value) != "0.9" {
					t.Errorf("value = %s, want 0.9", o.Value)
				}
				if o.UpdatedBy == nil || *o.UpdatedBy != actor.ID {
					t.Errorf("updated_by = %v, want %s", o.UpdatedBy, actor.ID)
				}
			}
		}
		if !found {
			t.Error("rerank.blend_weight not in overrides")
		}
	})

	t.Run("NakedSQLWrite_AuditViaSql", func(t *testing.T) {
		// psql-direct-edit shape: no SET LOCAL, plain pool Exec.
		if _, err := pool.Exec(ctx,
			`UPDATE context_settings SET value = '0.5'::jsonb WHERE key = 'rerank.blend_weight'`,
		); err != nil {
			t.Fatalf("naked update: %v", err)
		}

		a := lastAudit(t, pool, "rerank.blend_weight")
		if a.ApiKeyID != nil {
			t.Errorf("api_key_id = %v, want NULL for sql-direct edit", *a.ApiKeyID)
		}
		if a.ActorLabel != nil {
			t.Errorf("actor_label = %v, want NULL", *a.ActorLabel)
		}
		if a.Via == nil || *a.Via != "sql" {
			t.Errorf("via = %v, want sql", a.Via)
		}
	})

	t.Run("DeleteSetting_AuditUnset", func(t *testing.T) {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		found, err := store.DeleteSetting(ctx, tx, "rerank.blend_weight", store.GlobalScope, &actor.ID)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if !found {
			t.Fatal("delete found = false, want true")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		a := lastAudit(t, pool, "rerank.blend_weight")
		if a.Action != "unset" {
			t.Errorf("action = %s, want unset", a.Action)
		}
		if a.OldValue == nil || *a.OldValue != "0.5" {
			t.Errorf("old_value = %v, want 0.5", a.OldValue)
		}
		if a.NewValue != nil {
			t.Errorf("new_value = %v, want NULL on unset", *a.NewValue)
		}
		if a.ApiKeyID == nil || *a.ApiKeyID != actor.ID {
			t.Errorf("api_key_id = %v, want %s (DELETE carries actor via SET LOCAL)", a.ApiKeyID, actor.ID)
		}

		// Row is gone; a second delete reports found=false.
		tx2, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin2: %v", err)
		}
		defer tx2.Rollback(ctx) //nolint:errcheck
		found, err = store.DeleteSetting(ctx, tx2, "rerank.blend_weight", store.GlobalScope, &actor.ID)
		if err != nil {
			t.Fatalf("delete2: %v", err)
		}
		if found {
			t.Error("second delete found = true, want false")
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatalf("commit2: %v", err)
		}
	})

	t.Run("Secrets_CreateRotateDelete_AuditNullValues", func(t *testing.T) {
		nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}
		ct := []byte("sealed-bytes-create")

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		created, err := store.PutSecret(ctx, tx, "openrouter-main", store.GlobalScope, nonce, ct, 1, &actor.ID)
		if err != nil {
			t.Fatalf("put secret: %v", err)
		}
		if !created {
			t.Error("created = false on first put, want true")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		a := lastAudit(t, pool, "openrouter-main")
		if a.EntityType != "secret" || a.Action != "create" {
			t.Errorf("audit = %s/%s, want secret/create", a.EntityType, a.Action)
		}
		if a.OldValue != nil || a.NewValue != nil {
			t.Errorf("secret audit values old=%v new=%v, want NULL/NULL", a.OldValue, a.NewValue)
		}
		if a.ApiKeyID == nil || *a.ApiKeyID != actor.ID {
			t.Errorf("api_key_id = %v, want %s", a.ApiKeyID, actor.ID)
		}

		// Rotate: same name+scope, new sealed bytes, bumped key_version.
		tx2, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin rotate: %v", err)
		}
		defer tx2.Rollback(ctx) //nolint:errcheck
		created, err = store.PutSecret(ctx, tx2, "openrouter-main", store.GlobalScope,
			[]byte{12, 11, 10, 9, 8, 7, 6, 5, 4, 3, 2, 1}, []byte("sealed-bytes-rotate"), 2, &actor.ID)
		if err != nil {
			t.Fatalf("rotate secret: %v", err)
		}
		if created {
			t.Error("created = true on rotate, want false")
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatalf("commit rotate: %v", err)
		}

		a = lastAudit(t, pool, "openrouter-main")
		if a.Action != "rotate" {
			t.Errorf("action = %s, want rotate", a.Action)
		}
		if a.OldValue != nil || a.NewValue != nil {
			t.Errorf("rotate audit values old=%v new=%v, want NULL/NULL", a.OldValue, a.NewValue)
		}

		// Metadata view: rotated_at stamped, created_at preserved, version bumped.
		metas, err := store.ListSecretMeta(ctx, pool, store.GlobalScope)
		if err != nil {
			t.Fatalf("list meta: %v", err)
		}
		if len(metas) != 1 {
			t.Fatalf("metas = %d, want 1", len(metas))
		}
		m := metas[0]
		if m.Name != "openrouter-main" || m.KeyVersion != 2 || m.RotatedAt == nil {
			t.Errorf("meta = %+v, want name=openrouter-main key_version=2 rotated_at set", m)
		}

		// Delete: audit action + found semantics.
		tx3, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin delete: %v", err)
		}
		defer tx3.Rollback(ctx) //nolint:errcheck
		found, err := store.DeleteSecret(ctx, tx3, "openrouter-main", store.GlobalScope, &actor.ID)
		if err != nil {
			t.Fatalf("delete secret: %v", err)
		}
		if !found {
			t.Error("delete found = false, want true")
		}
		if err := tx3.Commit(ctx); err != nil {
			t.Fatalf("commit delete: %v", err)
		}

		a = lastAudit(t, pool, "openrouter-main")
		if a.Action != "delete" {
			t.Errorf("action = %s, want delete", a.Action)
		}
		if a.OldValue != nil || a.NewValue != nil {
			t.Errorf("delete audit values old=%v new=%v, want NULL/NULL", a.OldValue, a.NewValue)
		}
		if n := auditCount(t, pool, "openrouter-main"); n != 3 {
			t.Errorf("audit rows = %d, want 3 (create/rotate/delete)", n)
		}
	})

	t.Run("SecretAudit_NegativeProbe_NaiveTriggerLeaksCiphertext", func(t *testing.T) {
		// RED proof: replace the audit function with the naive variant
		// (copies the full row into old/new for ALL tables). A secret write
		// then leaks sealed material into the append-only audit table —
		// exactly the failure mode the 051 CASE guard exists to prevent.
		if _, err := pool.Exec(ctx, naiveAuditFn); err != nil {
			t.Fatalf("install naive audit fn: %v", err)
		}

		marker := []byte("LEAKMARKER-ciphertext-bytes")
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := store.PutSecret(ctx, tx, "leak-probe", store.GlobalScope,
			[]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, marker, 1, &actor.ID); err != nil {
			t.Fatalf("put secret under naive fn: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		a := lastAudit(t, pool, "leak-probe")
		// to_jsonb(bytea) renders PG's hex form ("\x4c45414b..."). Match on
		// the hex of the marker to prove the ciphertext landed in the audit.
		markerHex := "4c45414b4d41524b4552" // hex("LEAKMARKER")
		if a.NewValue == nil || !strings.Contains(strings.ToLower(*a.NewValue), markerHex) {
			t.Fatalf("RED-proof failed: naive audit fn did NOT leak ciphertext (new_value=%v) — probe is not demonstrating the guarded failure mode", a.NewValue)
		}

		// GREEN: restore the real 051 function (idempotent re-apply of the
		// migration file) and prove the same write shape audits NULL/NULL.
		sql, err := migrations.FS.ReadFile("051_settings_store.sql")
		if err != nil {
			t.Fatalf("read 051: %v", err)
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Fatalf("re-apply 051: %v", err)
		}

		tx2, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin2: %v", err)
		}
		defer tx2.Rollback(ctx) //nolint:errcheck
		if _, err := store.PutSecret(ctx, tx2, "leak-probe", store.GlobalScope,
			[]byte{2, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}, marker, 1, &actor.ID); err != nil {
			t.Fatalf("put secret under 051 fn: %v", err)
		}
		if err := tx2.Commit(ctx); err != nil {
			t.Fatalf("commit2: %v", err)
		}

		a = lastAudit(t, pool, "leak-probe")
		if a.OldValue != nil || a.NewValue != nil {
			t.Errorf("051 audit fn leaked values: old=%v new=%v, want NULL/NULL", a.OldValue, a.NewValue)
		}
	})

	t.Run("Notify_BothTables_PayloadCarriesNoValues", func(t *testing.T) {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire listener conn: %v", err)
		}
		defer conn.Release()
		if _, err := conn.Exec(ctx, "LISTEN ctx_settings_write"); err != nil {
			t.Fatalf("listen: %v", err)
		}

		secretMarker := "NOTIFYLEAK-secret-value"
		valueMarker := `"NOTIFYLEAK-setting-value"`

		// One settings write + one secrets write.
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := store.UpsertSetting(ctx, tx, "notify.probe", store.GlobalScope,
			json.RawMessage(valueMarker), &actor.ID); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if _, err := store.PutSecret(ctx, tx, "notify-probe", store.GlobalScope,
			[]byte{9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9, 9}, []byte(secretMarker), 1, &actor.ID); err != nil {
			t.Fatalf("put secret: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		seen := map[string]string{} // entity -> op
		for i := 0; i < 2; i++ {
			waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			n, err := conn.Conn().WaitForNotification(waitCtx)
			cancel()
			if err != nil {
				t.Fatalf("notification %d not received: %v", i, err)
			}
			if n.Channel != "ctx_settings_write" {
				t.Errorf("channel = %s", n.Channel)
			}
			if strings.Contains(n.Payload, "NOTIFYLEAK") {
				t.Errorf("payload leaks a value: %s", n.Payload)
			}
			var p struct {
				Entity string `json:"entity"`
				Key    string `json:"key"`
				Op     string `json:"op"`
			}
			if err := json.Unmarshal([]byte(n.Payload), &p); err != nil {
				t.Fatalf("payload not JSON: %v (%s)", err, n.Payload)
			}
			seen[p.Entity] = p.Op
			switch p.Entity {
			case "context_settings":
				if p.Key != "notify.probe" {
					t.Errorf("settings notify key = %s", p.Key)
				}
			case "context_secrets":
				if p.Key != "notify-probe" {
					t.Errorf("secrets notify key = %s", p.Key)
				}
			default:
				t.Errorf("unexpected entity %q", p.Entity)
			}
		}
		if len(seen) != 2 {
			t.Errorf("expected notifications from both tables, got %v", seen)
		}
	})

	t.Run("AuditAttribution_SurvivesKeyHardDelete", func(t *testing.T) {
		victim, _, err := store.CreateApiKey(ctx, pool, "doomed-actor", "private", nil)
		if err != nil {
			t.Fatalf("create victim key: %v", err)
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if err := store.UpsertSetting(ctx, tx, "attribution.probe", store.GlobalScope,
			json.RawMessage(`true`), &victim.ID); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}

		// Hard-delete the actor row (not the soft-delete path) — the audit
		// table has NO FK, so history must keep id + label snapshot.
		// Side effect by design: the FK ON DELETE SET NULL mutates the
		// context_settings row (updated_by -> NULL), which itself fires the
		// audit trigger — so the key deletion appends a via='sql' row. The
		// ORIGINAL attribution row must stay untouched underneath it.
		if _, err := pool.Exec(ctx,
			`DELETE FROM context_api_keys WHERE id = $1::uuid`, victim.ID); err != nil {
			t.Fatalf("hard delete key: %v", err)
		}

		var a auditRow
		err = pool.QueryRow(ctx,
			`SELECT entity_type, entity_key, scope, action, old_value::text, new_value::text,
			        api_key_id::text, actor_label, metadata->>'via', metadata->>'request_id'
			 FROM context_settings_audit
			 WHERE entity_key = 'attribution.probe'
			 ORDER BY created_at ASC, id ASC
			 LIMIT 1`,
		).Scan(&a.EntityType, &a.EntityKey, &a.Scope, &a.Action, &a.OldValue, &a.NewValue,
			&a.ApiKeyID, &a.ActorLabel, &a.Via, &a.RequestID)
		if err != nil {
			t.Fatalf("fetch original audit row: %v", err)
		}
		if a.ApiKeyID == nil || *a.ApiKeyID != victim.ID {
			t.Errorf("audit api_key_id = %v, want %s after key deletion", a.ApiKeyID, victim.ID)
		}
		if a.ActorLabel == nil || *a.ActorLabel != "doomed-actor" {
			t.Errorf("audit actor_label = %v, want doomed-actor", a.ActorLabel)
		}

		// The SET-NULL side-effect row exists and is sql-attributed.
		last := lastAudit(t, pool, "attribution.probe")
		if last.Via == nil || *last.Via != "sql" || last.ApiKeyID != nil {
			t.Errorf("SET-NULL side-effect row = via %v / actor %v, want sql/NULL", last.Via, last.ApiKeyID)
		}

		// The MAIN table's FK is ON DELETE SET NULL by design (convenience
		// column) — only the audit attribution is deletion-proof.
		var updatedBy *string
		if err := pool.QueryRow(ctx,
			`SELECT updated_by::text FROM context_settings WHERE key = 'attribution.probe'`,
		).Scan(&updatedBy); err != nil {
			t.Fatalf("read updated_by: %v", err)
		}
		if updatedBy != nil {
			t.Errorf("context_settings.updated_by = %v, want NULL after key deletion", *updatedBy)
		}
	})
}

// naiveAuditFn is the deliberately broken audit variant for the RED proof:
// it copies the entire row JSON into old/new for every table — for
// context_secrets that includes ciphertext+nonce. Never ship this.
const naiveAuditFn = `
CREATE OR REPLACE FUNCTION audit_settings_write() RETURNS TRIGGER AS $$
DECLARE
    v_new JSONB := to_jsonb(NEW);
    v_old JSONB := to_jsonb(OLD);
BEGIN
    INSERT INTO context_settings_audit
        (entity_type, entity_key, scope, action, old_value, new_value)
    VALUES (
        CASE TG_TABLE_NAME WHEN 'context_settings' THEN 'setting' ELSE 'secret' END,
        COALESCE(v_new->>'key', v_new->>'name', v_old->>'key', v_old->>'name'),
        COALESCE(v_new->>'scope', v_old->>'scope'),
        lower(TG_OP),
        v_old, v_new
    );
    RETURN COALESCE(NEW, OLD);
END;
$$ LANGUAGE plpgsql;
`
