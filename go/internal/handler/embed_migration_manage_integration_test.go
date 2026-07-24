//go:build integration

// Integration test for Evokoa-Clean-Room W04-7 (design/04 §7): the re-embed
// migration CONTROL SURFACE over /api/manage against PG18. It drives the full
// operator steering path through the HTTP handler — proving the TRANSPORT
// (arithmetic pending §6.3, verbatim error pass-through, verify_report reachable
// only here) on top of the already-integration-tested mechanics (W04-3/4/5/6).
//
//	go test -tags=integration ./internal/handler/ -run TestEmbedMigrationControl -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const (
	emcFromModel   = "emc-embed"
	emcToModel     = "emc-embed-next"
	emcBackendName = "emc-embed-backend"
)

// manageAdmin drives one /api/manage POST through a real ManageHandler as a
// server-admin and returns the recorder. backendPool is wired (confirm/rollback
// need it); the other controllers are nil (this family never touches them).
func manageAdmin(t *testing.T, h *ManageHandler, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	jsonBody, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(jsonBody))
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, adminAR()))
	rec := httptest.NewRecorder()
	h.HandleManage(rec, req)
	return rec
}

// mustSuccess asserts a 200 envelope and returns the decoded map.
func mustSuccess(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal (%d): %v — body=%s", rec.Code, err, rec.Body.String())
	}
	if rec.Code != http.StatusOK || m["success"] != true {
		t.Fatalf("want 200 success, got %d: %s", rec.Code, rec.Body.String())
	}
	return m
}

func emcSeedModels(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, k := range []string{emcFromModel, emcToModel} {
		if _, err := pool.Exec(context.Background(),
			`INSERT INTO context_embed_models (model_key, family, native_dims, stored_dims)
			 VALUES ($1, 'emc', 1024, 1024) ON CONFLICT (model_key) DO NOTHING`, k); err != nil {
			t.Fatalf("seed model %s: %v", k, err)
		}
	}
}

func emcSeedBackend(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &backends.Backend{
		Name: emcBackendName, Host: "http://127.0.0.1:11434", Protocol: backends.ProtocolOllama,
		ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull,
		Locality: backends.LocalityLocal, Roles: []string{backends.RoleEmbed},
		ModelMap: map[string]backends.ModelSpec{
			"default":    {Model: emcFromModel},
			"embed":      {Model: emcFromModel},
			"embed_next": {Model: emcToModel},
		},
		Enabled: true, Scope: backends.GlobalScope,
	}
	if _, err := store.CreateBackend(ctx, tx, b, nil); err != nil {
		t.Fatalf("create backend: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backend: %v", err)
	}
}

// emcNextIndexes builds the four _next indexes (production DDL minus CONCURRENTLY
// — a test container has no concurrent writers; the names are what confirm's swap
// renames). Confirm refuses (ErrConfirmNextIndexNotReady) without a valid HNSW.
func emcNextIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	for _, ddl := range []string{
		`CREATE INDEX IF NOT EXISTS idx_embedding_next_hnsw
		   ON context_blocks USING hnsw ((embedding_next::halfvec(1024)) halfvec_cosine_ops)
		   WITH (m = 16, ef_construction = 128)`,
		`CREATE INDEX IF NOT EXISTS idx_embedding_pending_next
		   ON context_blocks (created_at) WHERE embedding_next IS NULL AND NOT is_archived`,
		`CREATE INDEX IF NOT EXISTS idx_dream_pending_next
		   ON context_blocks (dream_checked_at ASC NULLS FIRST, quality_score ASC)
		   WHERE NOT is_archived AND embedding_next IS NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_guard_pending_next
		   ON context_blocks (created_at ASC)
		   WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding_next IS NOT NULL`,
	} {
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("build next index: %v", err)
		}
	}
}

func emcHandler(pool *pgxpool.Pool) *ManageHandler {
	return NewManageHandler(pool, config.NewStore(&config.Config{}), nil,
		backends.NewPool(pool, nil), nil, nil, nil, nil)
}

// TestEmbedMigrationControl_SteeringPath drives create → status (arithmetic
// pending) → resume (pending→running) → pause → resume → abort(reason) → purge,
// all through the HTTP handler.
func TestEmbedMigrationControl_SteeringPath(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := emcHandler(pool)

	emcSeedModels(t, pool)
	emcSeedBackend(t, pool)

	// Seed 10 migratable blocks (old-space vector, no _next) → total_blocks=10.
	for i := 0; i < 10; i++ {
		vec := make([]float32, 1024)
		vec[0] = 1
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity,
			        embedding, embed_model, created_at, updated_at)
			 VALUES ('learnings', $1, 'c', 'shared', 'internal', $2, $3, now(), now())`,
			"emc-block-"+string(rune('a'+i)), pgvec.NewVector(vec), emcFromModel); err != nil {
			t.Fatalf("seed block: %v", err)
		}
	}

	// create
	m := mustSuccess(t, manageAdmin(t, h, map[string]any{
		"action": "embed-migration-create",
		"data":   map[string]any{"from_model": emcFromModel, "to_model": emcToModel, "to_backend": emcBackendName},
	}))
	id, _ := m["id"].(string)
	if id == "" {
		t.Fatal("create returned no id")
	}

	// status: pending, arithmetic pending = total(10) − 0 − 0 − 0 = 10
	st := mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-status"}))
	mig, _ := st["migration"].(map[string]any)
	if mig == nil {
		t.Fatal("status returned nil migration")
	}
	if mig["status"] != "pending" {
		t.Errorf("status = %v, want pending", mig["status"])
	}
	if pending, _ := mig["pending"].(float64); pending != 10 {
		t.Errorf("arithmetic pending = %v, want 10 (total − migrated − failed − skipped)", mig["pending"])
	}
	if _, hasReport := mig["verify_report"]; !hasReport {
		t.Error("rich manage status view must carry verify_report field (block-ID-bearing, admin-only)")
	}

	// resume (pending→running), pause (running→paused), resume (paused→running)
	mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-resume"}))
	if s := statusField(t, h); s != "running" {
		t.Fatalf("after resume: %s, want running", s)
	}
	mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-pause"}))
	if s := statusField(t, h); s != "paused" {
		t.Fatalf("after pause: %s, want paused", s)
	}
	mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-resume"}))
	if s := statusField(t, h); s != "running" {
		t.Fatalf("after 2nd resume: %s, want running", s)
	}

	// abort WITHOUT reason → verbatim ErrReasonRequired (400), fail-closed.
	rec := manageAdmin(t, h, map[string]any{"action": "embed-migration-abort"})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "reason is required") {
		t.Fatalf("abort without reason: %d %s, want 400 verbatim reason-required", rec.Code, rec.Body.String())
	}
	// abort WITH reason → aborted
	mustSuccess(t, manageAdmin(t, h, map[string]any{
		"action": "embed-migration-abort", "data": map[string]any{"reason": "operator test abort"},
	}))

	// no active migration now → status.migration is null
	st = mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-status"}))
	if st["migration"] != nil {
		t.Errorf("after abort, active status must be null, got %v", st["migration"])
	}

	// purge (no active migration → allowed) — 0 leftover _next rows to clear.
	pm := mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-purge"}))
	if _, ok := pm["cleared"]; !ok {
		t.Error("purge response missing cleared count")
	}
}

// statusField reads the active migration's status through the handler.
func statusField(t *testing.T, h *ManageHandler) string {
	t.Helper()
	st := mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-status"}))
	mig, _ := st["migration"].(map[string]any)
	if mig == nil {
		return ""
	}
	s, _ := mig["status"].(string)
	return s
}

// TestEmbedMigrationControl_ConfirmRollback seeds a verifying migration with a
// green report + the four _next indexes, confirms through the API (→ done, with
// the cutover numbers), then rolls back with a reason (→ rolled_back). No blocks
// are seeded, so the watermark-scoped completeness re-check is trivially 0 and
// visibility_loss is 0 — this pins the TRANSPORT + verbatim number pass-through,
// not the cutover mechanics (those are TestEmbedCutover_Integration's job).
func TestEmbedMigrationControl_ConfirmRollback(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	h := emcHandler(pool)

	emcSeedModels(t, pool)
	emcSeedBackend(t, pool)
	emcNextIndexes(t, pool)

	// A verifying migration row with a green verify_report (the report CONTENT is
	// W04-5's job; confirm only needs result=green + watermark).
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_embed_migrations (from_model, to_model, to_backend, status,
		        total_blocks, verify_started_at, verify_report)
		 VALUES ($1, $2, $3, 'verifying', 0, now(), $4::jsonb) RETURNING id::text`,
		emcFromModel, emcToModel, emcBackendName,
		`{"result":"green","to_model":"`+emcToModel+`","completeness":{"result":"green","visibility_loss":0}}`,
	).Scan(&id); err != nil {
		t.Fatalf("seed verifying migration: %v", err)
	}

	// confirm → done, with the operator numbers present.
	cm := mustSuccess(t, manageAdmin(t, h, map[string]any{"action": "embed-migration-confirm"}))
	for _, k := range []string{"visibility_loss", "post_watermark_pending", "sweep_cleared", "memos_copied", "flipped_backends"} {
		if _, ok := cm[k]; !ok {
			t.Errorf("confirm response missing operator number %q: %v", k, cm)
		}
	}
	var doneStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM context_embed_migrations WHERE id=$1::uuid`, id).Scan(&doneStatus); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if doneStatus != "done" {
		t.Fatalf("after confirm: status=%s, want done", doneStatus)
	}

	// rollback WITHOUT reason → verbatim refusal (400), before any write.
	rec := manageAdmin(t, h, map[string]any{"action": "embed-migration-rollback", "id": id})
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "rollback_reason is required") {
		t.Fatalf("rollback without reason: %d %s, want 400 verbatim", rec.Code, rec.Body.String())
	}
	// rollback WITH reason → rolled_back (a done migration is not "active", so the
	// id is required — Active() returns nil for terminal rows).
	mustSuccess(t, manageAdmin(t, h, map[string]any{
		"action": "embed-migration-rollback", "id": id, "data": map[string]any{"reason": "recall regression test"},
	}))
	var rbStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM context_embed_migrations WHERE id=$1::uuid`, id).Scan(&rbStatus); err != nil {
		t.Fatalf("read status: %v", err)
	}
	if rbStatus != "rolled_back" {
		t.Fatalf("after rollback: status=%s, want rolled_back", rbStatus)
	}
}
