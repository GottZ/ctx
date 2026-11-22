//go:build integration

package handler

// Issue #505: the cut test of embed-migration-cleanup, which TestWriteOutcomeCutAdminPaths
// did not have (the classifier of the embed-migration writes was pinned for a
// purge after one cleared block only). The cleanup drops the rollback anchor
// of a done migration in one labeled bracket (events.CleanupEmbedMigration:
// pgxdb.Write with a commit label) and takes writeOutcome: a COMMIT whose
// answer is cut off says unknown — the held commit lands, the anchor is gone.
// Its bracket writes no row, only DDL, so the commit is held through an event
// trigger that marks each DDL statement of the handlers' connections in a
// table that carries the fixture's hold trigger.
//
//	go test -tags=integration ./internal/handler/ -run 'TestWriteOutcomeCutEmbedMigrationCleanup' -count=1 -v

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

func TestWriteOutcomeCutEmbedMigrationCleanup(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE i505_ddl_marks (id bigserial PRIMARY KEY, tag text NOT NULL)`,
		`CREATE CONSTRAINT TRIGGER i483_hold AFTER INSERT ON i505_ddl_marks
		   DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i483_hold('i505_ddl_marks')`,
		`CREATE FUNCTION i505_mark_ddl() RETURNS event_trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF current_setting('application_name') = 'i483-cut' THEN
		     INSERT INTO i505_ddl_marks (tag) VALUES (tg_tag);
		   END IF;
		 END $$`,
		`CREATE EVENT TRIGGER i505_mark_ddl ON ddl_command_end EXECUTE FUNCTION i505_mark_ddl()`,
	} {
		if _, err := f.direct.Exec(ctx, q); err != nil {
			t.Fatalf("ddl hold fixture: %v", err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.direct.Exec(context.Background(), `DROP EVENT TRIGGER i505_mark_ddl`)
	})

	// A done migration with its rollback anchor: a verifying row with a green
	// report, confirmed through the API on the direct pool (as
	// TestEmbedMigrationControl_ConfirmRollback does).
	emcSeedModels(t, f.direct)
	emcSeedBackend(t, f.direct)
	emcNextIndexes(t, f.direct)
	var id string
	if err := f.direct.QueryRow(ctx,
		`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status,
		        total_blocks, verify_started_at, verify_report)
		 VALUES ($1, $2, $3, 'verifying', 0, now(), $4::jsonb) RETURNING id::text`,
		emcFromModel, emcToModel, emcBackendName,
		`{"result":"green","to_model":"`+emcToModel+`","completeness":{"result":"green","visibility_loss":0}}`,
	).Scan(&id); err != nil {
		t.Fatalf("seed verifying migration: %v", err)
	}
	mustSuccess(t, manageAdmin(t, emcHandler(f.direct), map[string]any{"action": "embed-migration-confirm"}))
	anchor := `SELECT count(*)::int FROM information_schema.columns
	            WHERE table_name = 'context_blocks' AND column_name = 'embedding_old'`
	var n int
	if err := f.direct.QueryRow(ctx, anchor).Scan(&n); err != nil || n != 1 {
		t.Fatalf("rollback anchor after confirm: %d columns (%v), want 1", n, err)
	}

	k, adminPlain, err := store.CreateApiKey(ctx, f.direct, "i505-cut-admin", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.direct.Exec(ctx, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, k.ID); err != nil {
		t.Fatal(err)
	}
	if ar, err := auth.AuthenticateRaw(ctx, f.direct, adminPlain); err != nil || !ar.IsValid {
		t.Fatalf("authenticate admin: %v", err)
	}
	f.cutREST(t, emcHandler(f.pool).HandleManage, adminPlain, "i505_ddl_marks", "/api/manage",
		`{"action":"embed-migration-cleanup","id":"`+id+`"}`, "manage: embed-migration-cleanup failed")
	f.waitLanded(t, `SELECT (count(*) = 0)::int FROM information_schema.columns
	                  WHERE table_name = 'context_blocks' AND column_name = 'embedding_old'`)
}
