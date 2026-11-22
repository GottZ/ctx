//go:build integration

package handler

// Issue #483 point 9, rest: the backend, model-alias and embed-migration
// write routes that fail write one line — `manage: <action> failed` with the
// error, write_outcome and request_id — where they answered without a trace
// before. Nothing of the request goes into it: a backend row can carry a
// secret-shaped value and a URL with userinfo in its free-form fields, and
// the database's refusal of such a row names them in its DETAIL (the failing
// row), which the line must not carry. A refusal that is the client's (a
// taken backend name, an embed-migration precondition) writes no line.
//
//	go test -tags=integration ./internal/handler/ -run 'TestBackendWriteFailureLines|TestModelAliasWriteFailureLines|TestEmbedMigrationWriteFailureLines' -count=1 -v

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/testdb"
)

// The secret-shaped values a backend request carries in fields the
// validation lets through (metadata, extra_body under a neutral name).
const (
	i483LeakUser = "i483user"
	i483LeakPass = "i483secret"
	i483LeakKey  = "sk-i483-leak-0123456789abcdef"
)

// adminFailLine checks the run's line: exactly one record msg at ERROR with
// write_outcome=outcome and the request id, and no record of the run
// carrying a leak value.
func adminFailLine(t *testing.T, logs *rejectLogCapture, msg, outcome, reqID string) rejectLogRecord {
	t.Helper()
	recs := logs.withMsg(msg)
	if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["write_outcome"] != outcome ||
		recs[0].attrs["request_id"] != reqID {
		t.Fatalf("%q = %+v, want one ERROR line with write_outcome=%s request_id=%s (all: %+v)", msg, recs, outcome, reqID, logs.all())
	}
	for _, r := range logs.all() {
		txt := r.text()
		for _, leak := range []string{i483LeakUser, i483LeakPass, i483LeakKey} {
			if strings.Contains(txt, leak) {
				t.Errorf("a line carries %q from the request: %s", leak, txt)
			}
		}
	}
	return recs[0]
}

// noAdminFailLine checks that no line of the run says msg.
func noAdminFailLine(t *testing.T, logs *rejectLogCapture, msg string) {
	t.Helper()
	if recs := logs.withMsg(msg); len(recs) != 0 {
		t.Errorf("a refusal of the client wrote %q: %+v", msg, recs)
	}
}

// dbFault installs ddl (CREATE ... statements) for the rest of the test and
// runs drop at its end.
func dbFault(t *testing.T, pool *pgxpool.Pool, drop []string, ddl ...string) {
	t.Helper()
	for _, q := range ddl {
		if _, err := pool.Exec(context.Background(), q); err != nil {
			t.Fatalf("fault fixture %q: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range drop {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("drop fault fixture %q: %v", q, err)
			}
		}
	})
}

// commitFault makes every COMMIT that wrote a row of table fail with an
// ERROR (a deferred constraint trigger) for the rest of the test.
func commitFault(t *testing.T, pool *pgxpool.Pool, table string) {
	t.Helper()
	name := "i483_commit_fault_" + table
	dbFault(t, pool,
		[]string{fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, table), fmt.Sprintf(`DROP FUNCTION %s()`, name)},
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i483 probe commit fault'; END $$`, name),
		fmt.Sprintf(`CREATE CONSTRAINT TRIGGER %s AFTER INSERT OR UPDATE OR DELETE ON %s
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION %s()`, name, table, name))
}

// rowFault makes the database refuse op on table (P0001, BEFORE trigger) for
// the rest of the test.
func rowFault(t *testing.T, pool *pgxpool.Pool, table, op string) {
	t.Helper()
	name := "i483_row_fault_" + strings.ToLower(op) + "_" + table
	dbFault(t, pool,
		[]string{fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, table), fmt.Sprintf(`DROP FUNCTION %s()`, name)},
		fmt.Sprintf(`CREATE FUNCTION %s() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'i483 probe fault'; END $$`, name),
		fmt.Sprintf(`CREATE TRIGGER %s BEFORE %s ON %s FOR EACH ROW EXECUTE FUNCTION %s()`, name, op, table, name))
}

// leakyBackend is a backend-create payload whose free-form fields carry the
// leak values.
func leakyBackend(name string) map[string]any {
	return map[string]any{
		"name": name, "base_url": "http://" + name, "roles": []string{"chat"},
		"model_map":  map[string]any{"default": "stub-model"},
		"metadata":   map[string]any{"i483_note": "https://" + i483LeakUser + ":" + i483LeakPass + "@leak.example/v1"},
		"extra_body": map[string]any{"i483_opt": i483LeakKey},
	}
}

// wantDetailCarriesLeak checks that the error of line is the database's
// refusal whose DETAIL names the leak values — the line is clean only if it
// leaves that DETAIL out.
func wantDetailCarriesLeak(t *testing.T, line rejectLogRecord) {
	t.Helper()
	err, _ := line.attrs["error"].(error)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || !strings.Contains(pgErr.Detail, i483LeakPass) || !strings.Contains(pgErr.Detail, i483LeakKey) {
		t.Fatalf("line error %v carries no DETAIL with the request's values — the leak probe tests nothing", err)
	}
}

func TestBackendWriteFailureLines(t *testing.T) {
	ph := setupProfileHarness(t)
	admin := ph.admin
	manage := func(t *testing.T, reqID string, payload map[string]any) (int, map[string]any, *rejectLogCapture) {
		t.Helper()
		logs := installRejectLogCapture(t)
		status, resp := kaManage(t, ph.h, admin, reqID, payload)
		return status, resp, logs
	}
	// The database refuses a backend row by name; the failing row in its
	// DETAIL carries every column.
	dbFault(t, ph.pool, []string{`ALTER TABLE context_backends DROP CONSTRAINT i483_refuse`},
		`ALTER TABLE context_backends ADD CONSTRAINT i483_refuse CHECK (name NOT LIKE 'i483-refused%') NOT VALID`)

	t.Run("backend-create refused by the database", func(t *testing.T) {
		status, resp, logs := manage(t, "i483-bc", map[string]any{"action": "backend-create", "data": leakyBackend("i483-refused-create")})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("backend-create answered %d %v, want the store error's 422", status, resp)
		}
		wantDetailCarriesLeak(t, adminFailLine(t, logs, "manage: backend-create failed", writeRolledBack, "i483-bc"))
	})
	t.Run("backend-create of a taken name writes no line", func(t *testing.T) {
		if status, resp, _ := manage(t, "i483-dup-1", map[string]any{"action": "backend-create", "data": leakyBackend("i483-dup")}); status != http.StatusOK {
			t.Fatalf("first backend-create answered %d %v", status, resp)
		}
		status, resp, logs := manage(t, "i483-dup-2", map[string]any{"action": "backend-create", "data": leakyBackend("i483-dup")})
		if status != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(resp["error"]), "already exists") {
			t.Fatalf("second backend-create answered %d %v, want 422 already exists", status, resp)
		}
		noAdminFailLine(t, logs, "manage: backend-create failed")
		// backend-reorder below must list every backend.
		if _, err := ph.pool.Exec(context.Background(), `DELETE FROM context_backends WHERE name = 'i483-dup'`); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("backend-create whose COMMIT fails", func(t *testing.T) {
		commitFault(t, ph.pool, "context_backends")
		status, resp, logs := manage(t, "i483-bcc", map[string]any{"action": "backend-create", "data": leakyBackend("i483-commit-create")})
		if status != http.StatusInternalServerError || resp["error"] != "commit failed" {
			t.Fatalf("backend-create answered %d %v, want 500 commit failed", status, resp)
		}
		adminFailLine(t, logs, "manage: backend-create failed", writeRolledBack, "i483-bcc")
	})
	t.Run("backend-update refused by the database", func(t *testing.T) {
		data := leakyBackend("i483-refused-update")
		status, resp, logs := manage(t, "i483-bu", map[string]any{"action": "backend-update", "id": ph.byID["chat-a"], "data": data})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("backend-update answered %d %v, want the store error's 422", status, resp)
		}
		wantDetailCarriesLeak(t, adminFailLine(t, logs, "manage: backend-update failed", writeRolledBack, "i483-bu"))
	})
	t.Run("backend-update whose COMMIT fails", func(t *testing.T) {
		commitFault(t, ph.pool, "context_backends")
		status, resp, logs := manage(t, "i483-buc", map[string]any{"action": "backend-update", "id": ph.byID["chat-b"],
			"data": map[string]any{"priority": 77}})
		if status != http.StatusInternalServerError || resp["error"] != "commit failed" {
			t.Fatalf("backend-update answered %d %v, want 500 commit failed", status, resp)
		}
		adminFailLine(t, logs, "manage: backend-update failed", writeRolledBack, "i483-buc")
	})
	t.Run("backend-delete refused by the database", func(t *testing.T) {
		rowFault(t, ph.pool, "context_backends", "DELETE")
		status, resp, logs := manage(t, "i483-bd", map[string]any{"action": "backend-delete", "id": ph.byID["only-rerank"]})
		if status != http.StatusInternalServerError {
			t.Fatalf("backend-delete answered %d %v, want 500", status, resp)
		}
		adminFailLine(t, logs, "manage: backend-delete failed", writeRolledBack, "i483-bd")
	})
	t.Run("backend-reorder refused by the database", func(t *testing.T) {
		rowFault(t, ph.pool, "context_backends", "UPDATE")
		seq := []string{"tenant-embed", "chat-b", "gpu-embed", "chat-a", "tenant-chat", "only-rerank"}
		status, resp, logs := manage(t, "i483-br", map[string]any{"action": "backend-reorder", "data": ph.reorderData(seq, nil)})
		if status != http.StatusInternalServerError {
			t.Fatalf("backend-reorder answered %d %v, want 500", status, resp)
		}
		adminFailLine(t, logs, "manage: backend-reorder failed", writeRolledBack, "i483-br")
	})
}

// TestModelAliasWriteFailureLines — the three model-alias routes share the
// backend surface's COMMIT failure answer, and its line.
func TestModelAliasWriteFailureLines(t *testing.T) {
	m := setupModelAliasHarness(t)
	upd := m.seed("ma-a", "i483-upd", "chat", "chat", "credentials", "{}")
	del := m.seed("ma-a", "i483-del", "chat", "chat", "credentials", "{}")
	commitFault(t, m.pool, "context_model_aliases")
	for _, tc := range []struct {
		action  string
		payload map[string]any
	}{
		{"model-alias-create", map[string]any{"action": "model-alias-create", "data": chatSpec("i483-new", nil)}},
		{"model-alias-update", map[string]any{"action": "model-alias-update", "id": upd, "data": map[string]any{"enabled": false}}},
		{"model-alias-delete", map[string]any{"action": "model-alias-delete", "id": del}},
	} {
		t.Run(tc.action, func(t *testing.T) {
			logs := installRejectLogCapture(t)
			status, resp := kaManage(t, m.h, m.ten, "i483-"+tc.action, tc.payload)
			if status != http.StatusInternalServerError || resp["error"] != "commit failed" {
				t.Fatalf("%s answered %d %v, want 500 commit failed", tc.action, status, resp)
			}
			adminFailLine(t, logs, "manage: "+tc.action+" failed", writeRolledBack, "i483-"+tc.action)
		})
	}
}

// TestEmbedMigrationWriteFailureLines — the eight embed-migration write
// routes log the failure they answer 500, also when it comes from the lookup
// of the migration before the write (resolveMigrationID, loadMigrationStatus);
// a refusal they answer 4xx writes no line.
func TestEmbedMigrationWriteFailureLines(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := NewManageHandler(pool, nil, nil, backends.NewPool(nil, nil), nil, nil, nil, nil)
	const missing = "01900000-0000-7000-8000-000000000483"
	run := func(t *testing.T, reqID string, payload map[string]any) (int, map[string]any, *rejectLogCapture) {
		t.Helper()
		logs := installRejectLogCapture(t)
		status, resp := kaManage(t, h, admin, reqID, payload)
		return status, resp, logs
	}

	t.Run("a refusal answered 4xx writes no line", func(t *testing.T) {
		status, resp, logs := run(t, "i483-em-400", map[string]any{"action": "embed-migration-rollback", "id": missing})
		if status != http.StatusBadRequest {
			t.Fatalf("rollback without a reason answered %d %v, want 400", status, resp)
		}
		noAdminFailLine(t, logs, "manage: embed-migration-rollback failed")
		status, resp, logs = run(t, "i483-em-404", map[string]any{"action": "embed-migration-resume", "id": missing})
		if status != http.StatusNotFound {
			t.Fatalf("resume of an unknown id answered %d %v, want 404", status, resp)
		}
		noAdminFailLine(t, logs, "manage: embed-migration-resume failed")
	})

	// From here on every route's first read fails: the tables it reads are gone.
	dbFault(t, pool, []string{
		`ALTER TABLE i483_gone_migrations RENAME TO context_embed_migrations`,
		`ALTER TABLE i483_gone_models RENAME TO context_embed_models`,
	},
		`ALTER TABLE context_embed_migrations RENAME TO i483_gone_migrations`,
		`ALTER TABLE context_embed_models RENAME TO i483_gone_models`)

	for _, tc := range []struct {
		name, action string
		payload      map[string]any
	}{
		{"create", "embed-migration-create", map[string]any{"data": map[string]any{"from_model": "i483-a", "to_model": "i483-b", "to_backend": "i483-x"}}},
		{"pause", "embed-migration-pause", map[string]any{"id": missing}},
		{"pause without an id (resolveMigrationID)", "embed-migration-pause", map[string]any{}},
		{"resume (loadMigrationStatus)", "embed-migration-resume", map[string]any{"id": missing}},
		{"abort (loadMigrationStatus)", "embed-migration-abort", map[string]any{"id": missing, "data": map[string]any{"reason": "i483"}}},
		{"confirm", "embed-migration-confirm", map[string]any{"id": missing}},
		{"rollback", "embed-migration-rollback", map[string]any{"id": missing, "data": map[string]any{"reason": "i483"}}},
		{"cleanup", "embed-migration-cleanup", map[string]any{"id": missing}},
		{"purge", "embed-migration-purge", map[string]any{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := map[string]any{"action": tc.action}
			for k, v := range tc.payload {
				payload[k] = v
			}
			reqID := "i483-em-" + strings.ReplaceAll(tc.name, " ", "-")
			status, resp, logs := run(t, reqID, payload)
			if status != http.StatusInternalServerError {
				t.Fatalf("%s answered %d %v, want 500", tc.name, status, resp)
			}
			adminFailLine(t, logs, "manage: "+tc.action+" failed", writeRolledBack, reqID)
		})
	}
}
