//go:build integration

// Gates for the Evokoa-Clean-Room-Plan Achse 04 W04-6 (design/04 §4.8/
// §4.9/§4.10, §7 wave row W04-6): cutover, rollback, cleanup.
//
//   - Rot 1 (confirm_refusals): confirm with a non-findable/non-uniquely-
//     updatable 'embed' model_map row refuses BEFORE any write. RED
//     evidence: against an intermediate build without the flip-target
//     validation the confirm sailed through and the CASE-ELSE branch
//     INVENTED an embed row — see the wave report.
//   - Rot 2 (rollback_refusals): rollback with missing _old column/index
//     refuses fail-closed. RED evidence: without the preflight the mirror
//     tx died mid-flight on SQLSTATE 42703 AFTER the cache flush write.
//   - Rot 3 (watermark_livelock_probe): a write stream DURING verifying
//     (post-watermark pending block present at confirm time) must NOT block
//     the confirm. The in-test negative probe shows the SAME completeness
//     predicate WITHOUT the watermark clause counts >0 — a watermark-less
//     confirm would refuse forever (green_memo_exception pattern).
//   - Rot 4 (in e2e): after rollback, create on the same to_model refuses
//     without ReuseExisting (rest-data check) AND with it (rollback data is
//     on record as suspicious). See also reuse_purge_integration_test.go.
//   - Grün (e2e): nearest-neighbor space flips old → new → old across
//     confirm/rollback; ctx_rrf pg_proc oid+def UNCHANGED (attnum binding,
//     no function generation); EXPLAIN shows the canonical partial indexes
//     carrying backfill/dream/guard scans AFTER the swap while the _old
//     exemplars' predicates provably followed embedding_old (the (3b)
//     Attnum class made visible); cache flushed; model_map flipped and the
//     pool snapshot synchronously reloaded; double-confirm fails the CAS
//     precondition. Plus memo re-homing, sweep, and cleanup probes.
//
// Run: go test -tags=integration ./internal/events/ -run TestEmbedCutover_Integration -count=1 -v
package events

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedmigration"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"
)

const cutoverBackendName = "embed-cutover-a"

// abundantCutoverDisk mirrors the embedmigration test stub: the create
// calls in Rot 4 exercise the REUSE rules, never the disk gate.
func abundantCutoverDisk() (uint64, error) { return 1 << 40, nil }

// resetCutoverFixture extends resetMigrationFixture with the two tables the
// cutover touches beyond the migration trio: backends (flip targets) and
// the embed cache (§4.9 flush).
func resetCutoverFixture(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	resetMigrationFixture(t, pool)
	ctx := context.Background()
	for _, sql := range []string{
		`DELETE FROM context_backends`,
		`DELETE FROM context_embed_cache`,
	} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("reset cutover fixture (%s): %v", sql, err)
		}
	}
}

// ensureNextIndexes builds the four _next indexes with the PRODUCTION DDL
// minus CONCURRENTLY (a test container has no concurrent writers to
// protect; the names — the part the swap tx renames — stay identical).
func ensureNextIndexes(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	builds := append([]struct{ name, ddl string }{{verifyNextHNSWIndexName, verifyNextHNSWIndexDDL}}, verifySisterIndexes...)
	for _, b := range builds {
		ddl := strings.Replace(b.ddl, " CONCURRENTLY", "", 1)
		if _, err := pool.Exec(context.Background(), ddl); err != nil {
			t.Fatalf("ensure next index %s: %v", b.name, err)
		}
	}
}

// seedCutoverBackend inserts a context_backends row via the production
// CreateBackend path (RoleEmbed, local, global-scoped) — the flip target.
func seedCutoverBackend(t *testing.T, pool *pgxpool.Pool, name string, modelMap map[string]backends.ModelSpec) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	b := &backends.Backend{
		Name: name, Host: "http://127.0.0.1:11434", Protocol: backends.ProtocolOllama,
		ProviderClass: backends.ProviderGeneric, Trust: backends.TrustFull,
		Locality: backends.LocalityLocal, Roles: []string{backends.RoleEmbed},
		ModelMap: modelMap, Enabled: true, Scope: backends.GlobalScope,
	}
	if _, err := store.CreateBackend(ctx, tx, b, nil); err != nil {
		t.Fatalf("create backend %s: %v", name, err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit backend %s: %v", name, err)
	}
}

// flippableModelMap is the armed state the §4.8 precondition wants: an
// explicit 'embed' row (updatable in place) plus the embed_next arming.
func flippableModelMap() map[string]backends.ModelSpec {
	return map[string]backends.ModelSpec{
		"default":    {Model: migFromModel},
		"embed":      {Model: migFromModel},
		"embed_next": {Model: migToModel},
	}
}

// stampGreenReport writes a green verify_report onto the verifying row —
// the confirm's precondition (the REAL report is W04-5's job; the cutover
// tests pin the confirm mechanics, not the gate content).
func stampGreenReport(t *testing.T, pool *pgxpool.Pool, migID string, visLoss int64) {
	t.Helper()
	rep := verifyReport{
		Result: verifyGreen, StartedAt: time.Now().UTC(), FinishedAt: time.Now().UTC(),
		ToModel: migToModel,
		Completeness: verifyCompletenessSection{
			Result: verifyGreen, VisibilityLoss: visLoss,
		},
	}
	body, err := json.Marshal(rep)
	if err != nil {
		t.Fatalf("marshal green report: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_embed_migrations SET verify_report = $2 WHERE id = $1::uuid`,
		migID, string(body)); err != nil {
		t.Fatalf("stamp green report: %v", err)
	}
}

// seedConfirmReady arranges the standard confirm-ready world: flippable
// backend, all four _next indexes, a verifying migration row with a green
// report. Returns the migration id.
func seedConfirmReady(t *testing.T, pool *pgxpool.Pool, visLoss int64) string {
	t.Helper()
	seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
	ensureNextIndexes(t, pool)
	migID := seedMigrationRow(t, pool, "verifying")
	stampGreenReport(t, pool, migID, visLoss)
	return migID
}

// seedBothSpaces inserts a pre-watermark block carrying distinct old/new
// space vectors (age hours in the past — the watermark is the verifying
// row's insert time).
func seedBothSpaces(t *testing.T, pool *pgxpool.Pool, title string, age time.Duration, oldVec, nextVec []float32, nextModel string) string {
	t.Helper()
	return seedMigratedBlock(t, pool, title, age, oldVec, nextVec, nextModel)
}

// seedPostWatermarkPending inserts a block CREATED AFTER the watermark that
// only carries an old-space vector — the organic write stream during
// verifying (§4.1 drain semantics / Rot 3).
func seedPostWatermarkPending(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	vec := verifyUnitVec(0xA11CE)
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, sensitivity,
		        embedding, embed_model, created_at, updated_at)
		 VALUES ('learnings', $1, 'post-wm content', 'shared', 'internal',
		        $2, $3, now() + interval '1 hour', now())
		 RETURNING id::text`,
		title, pgvec.NewVector(vec), migFromModel).Scan(&id); err != nil {
		t.Fatalf("seed post-watermark block %s: %v", title, err)
	}
	return id
}

func nearestTitle(t *testing.T, pool *pgxpool.Pool, probe []float32) string {
	t.Helper()
	var title string
	if err := pool.QueryRow(context.Background(),
		`SELECT title FROM context_blocks WHERE embedding IS NOT NULL
		 ORDER BY embedding <=> $1 LIMIT 1`, pgvec.NewVector(probe)).Scan(&title); err != nil {
		t.Fatalf("nearest probe: %v", err)
	}
	return title
}

// rrfFunctionPins fingerprints every ctx_rrf overload: oid (identity — a
// DROP+CREATE would mint a new one) + md5 of the full definition.
func rrfFunctionPins(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT p.oid::text || ':' || md5(pg_get_functiondef(p.oid))
		 FROM pg_proc p WHERE p.proname = 'ctx_rrf' ORDER BY p.oid`)
	if err != nil {
		t.Fatalf("ctx_rrf pins: %v", err)
	}
	defer rows.Close()
	var pins []string
	for rows.Next() {
		var pin string
		if err := rows.Scan(&pin); err != nil {
			t.Fatalf("ctx_rrf pin row: %v", err)
		}
		pins = append(pins, pin)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("ctx_rrf pins iterate: %v", err)
	}
	if len(pins) == 0 {
		t.Fatal("no ctx_rrf function found — fixture broken")
	}
	return pins
}

// explainPlan returns the EXPLAIN output of sql with seq scans disabled for
// the statement (SET LOCAL inside a throwaway tx — pooled connections must
// not keep the GUC).
func explainPlan(t *testing.T, pool *pgxpool.Pool, sql string) string {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("explain tx: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `SET LOCAL enable_seqscan = off`); err != nil {
		t.Fatalf("disable seqscan: %v", err)
	}
	rows, err := tx.Query(ctx, "EXPLAIN "+sql)
	if err != nil {
		t.Fatalf("explain: %v", err)
	}
	defer rows.Close()
	var lines []string
	for rows.Next() {
		var l string
		if err := rows.Scan(&l); err != nil {
			t.Fatalf("explain row: %v", err)
		}
		lines = append(lines, l)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("explain iterate: %v", err)
	}
	return strings.Join(lines, "\n")
}

func columnExistsT(t *testing.T, pool *pgxpool.Pool, col string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_attribute
		 WHERE attrelid = 'context_blocks'::regclass AND attname = $1
		   AND attnum > 0 AND NOT attisdropped)`, col).Scan(&ok); err != nil {
		t.Fatalf("column probe %s: %v", col, err)
	}
	return ok
}

func indexExistsT(t *testing.T, pool *pgxpool.Pool, name string) bool {
	t.Helper()
	var ok bool
	if err := pool.QueryRow(context.Background(),
		`SELECT EXISTS(SELECT 1 FROM pg_class WHERE relname = $1 AND relkind = 'i')`, name).Scan(&ok); err != nil {
		t.Fatalf("index probe %s: %v", name, err)
	}
	return ok
}

func TestEmbedCutover_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	newBPool := func() *backends.Pool { return backends.NewPool(pool, nil) }

	// Rot 1 — the §4.8 precondition: the intended target configuration must
	// be findable and uniquely updatable, otherwise the confirm refuses
	// BEFORE any write (status stays verifying, no column moved).
	t.Run("confirm_refusals_flip_target", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		// (a) No explicit 'embed' row — resolution runs via the default
		// fallback; there is nothing to update in place.
		seedCutoverBackend(t, pool, cutoverBackendName, map[string]backends.ModelSpec{
			"default":    {Model: migFromModel},
			"embed_next": {Model: migToModel},
		})
		ensureNextIndexes(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")
		stampGreenReport(t, pool, migID, 0)

		_, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID)
		if !errors.Is(err, ErrConfirmEmbedKeyNotFlippable) {
			t.Fatalf("Confirm error = %v, want ErrConfirmEmbedKeyNotFlippable", err)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Status != "verifying" {
			t.Errorf("status = %q, want verifying (refusal must not transition)", st.Status)
		}
		if columnExistsT(t, pool, "embedding_old") {
			t.Error("embedding_old exists — refusal must not touch the catalog")
		}

		// (b) embed_next armed to a FOREIGN model — flipping would relabel
		// a foreign space as serving.
		if _, err := pool.Exec(ctx,
			`UPDATE context_backends SET model_map = '{"default":"`+migFromModel+`","embed":"`+migFromModel+`","embed_next":"some-other-model"}'::jsonb
			 WHERE name = $1`, cutoverBackendName); err != nil {
			t.Fatalf("re-arm foreign embed_next: %v", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmEmbedNextForeign) {
			t.Fatalf("Confirm error = %v, want ErrConfirmEmbedNextForeign", err)
		}

		// (c) No RoleEmbed backend with an embed_next key at all — no
		// intended target configuration exists.
		if _, err := pool.Exec(ctx, `DELETE FROM context_backends`); err != nil {
			t.Fatalf("clear backends: %v", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmNoFlipTarget) {
			t.Fatalf("Confirm error = %v, want ErrConfirmNoFlipTarget", err)
		}
	})

	// Confirm's remaining fail-closed preconditions: status, report,
	// report color, _next index readiness.
	t.Run("confirm_refusals_state_and_indexes", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		seedCutoverBackend(t, pool, cutoverBackendName, flippableModelMap())
		ensureNextIndexes(t, pool)
		migID := seedMigrationRow(t, pool, "verifying")

		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmVerifyReportMissing) {
			t.Fatalf("Confirm error = %v, want ErrConfirmVerifyReportMissing", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET verify_report = '{"result":"red"}'::jsonb WHERE id = $1::uuid`,
			migID); err != nil {
			t.Fatalf("stamp red report: %v", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmVerifyNotGreen) {
			t.Fatalf("Confirm error = %v, want ErrConfirmVerifyNotGreen", err)
		}
		stampGreenReport(t, pool, migID, 0)
		if _, err := pool.Exec(ctx, `DROP INDEX idx_guard_pending_next`); err != nil {
			t.Fatalf("drop sister index: %v", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmNextIndexNotReady) {
			t.Fatalf("Confirm error = %v, want ErrConfirmNextIndexNotReady", err)
		}
		// Wrong status last (needs a row that is NOT verifying).
		if _, err := pool.Exec(ctx,
			`UPDATE context_embed_migrations SET status = 'paused' WHERE id = $1::uuid`, migID); err != nil {
			t.Fatalf("pause row: %v", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmNotVerifying) {
			t.Fatalf("Confirm error = %v, want ErrConfirmNotVerifying", err)
		}
	})

	// Rot 2 — rollback preflight: every _old resource must exist, and the
	// reason is mandatory BEFORE any write.
	t.Run("rollback_refusals", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedMigrationRow(t, pool, "done")

		if _, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "  "); !errors.Is(err, ErrRollbackReasonRequired) {
			t.Fatalf("Rollback error = %v, want ErrRollbackReasonRequired", err)
		}
		_, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "operator test")
		if !errors.Is(err, ErrRollbackOldResourcesMissing) {
			t.Fatalf("Rollback error = %v, want ErrRollbackOldResourcesMissing", err)
		}
		// The refusal names the missing anchor objects (operator surface).
		if !strings.Contains(err.Error(), "embedding_old") || !strings.Contains(err.Error(), "idx_embedding_hnsw_old") {
			t.Errorf("Rollback error %q should name the missing _old resources", err)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Status != "done" {
			t.Errorf("status = %q, want done (refusal must not transition)", st.Status)
		}
	})

	// Rot 3 — the livelock probe: a post-watermark pending block exists at
	// confirm time. The NEGATIVE probe shows the identical predicate
	// WITHOUT the watermark clause counts it (a watermark-less confirm
	// would refuse forever under a write stream); the ACTUAL confirm, being
	// watermark-scoped (§5 Bruchpfad 4), proceeds and reports the block as
	// the post-swap transient.
	t.Run("watermark_livelock_probe", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedConfirmReady(t, pool, 0)
		v := verifyUnitVec(11)
		seedBothSpaces(t, pool, "pre-wm-clean", 2*time.Hour, v, verifyUnitVec(12), migToModel)
		seedPostWatermarkPending(t, pool, "post-wm-write")

		var unscoped int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE `+verifyCompletenessWhere(false), migID,
		).Scan(&unscoped); err != nil {
			t.Fatalf("unscoped negative probe: %v", err)
		}
		if unscoped == 0 {
			t.Fatal("negative probe: watermark-less pending count = 0, fixture does not prove the livelock class")
		}

		res, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID)
		if err != nil {
			t.Fatalf("Confirm must not refuse under post-watermark writes (got %v)", err)
		}
		if res.PostWatermarkPending != 1 {
			t.Errorf("PostWatermarkPending = %d, want 1 (the declared transient)", res.PostWatermarkPending)
		}
		if st := readMigrationRow(t, pool, migID); st.Status != "done" {
			t.Errorf("status = %q, want done", st.Status)
		}

		// Restore baseline for the following subtests via the designed
		// inverse (also exercises rollback on a swept-free world).
		if _, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "test baseline restore"); err != nil {
			t.Fatalf("baseline rollback: %v", err)
		}
	})

	// Grün — the full e2e arc: space flip, function stability, index
	// carriage, flip/unflip, straddle sweep, Rot-4 create refusals.
	t.Run("e2e_cutover_and_rollback", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedConfirmReady(t, pool, 7)
		probe := verifyUnitVec(1)
		// alpha serves the probe in the OLD space, beta in the NEW space.
		alphaID := seedBothSpaces(t, pool, "alpha", 3*time.Hour, verifyUnitVec(1), verifyUnitVec(2), migToModel)
		seedBothSpaces(t, pool, "beta", 2*time.Hour, verifyUnitVec(3), verifyUnitVec(1), migToModel)
		gammaID := seedBothSpaces(t, pool, "gamma", time.Hour, verifyUnitVec(5), verifyUnitVec(6), migToModel)
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_embed_cache (text_hash, model, embedding, text_preview)
			 VALUES ($1, $2, $3, 'cutover probe')`,
			[]byte("cutover-hash"), migFromModel, pgvec.NewVector(verifyUnitVec(9))); err != nil {
			t.Fatalf("seed cache row: %v", err)
		}

		pinsBefore := rrfFunctionPins(t, pool)
		if got := nearestTitle(t, pool, probe); got != "alpha" {
			t.Fatalf("pre-swap nearest = %q, want alpha (old space serves)", got)
		}

		bpool := newBPool()
		res, err := ConfirmEmbedMigration(ctx, pool, bpool, migID)
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}

		// Space flipped, statemachine done, report numbers passed through.
		if got := nearestTitle(t, pool, probe); got != "beta" {
			t.Errorf("post-swap nearest = %q, want beta (new space serves)", got)
		}
		if st := readMigrationRow(t, pool, migID); st.Status != "done" {
			t.Errorf("status = %q, want done", st.Status)
		}
		if res.VisibilityLoss != 7 {
			t.Errorf("VisibilityLoss = %d, want 7 (verify_report passthrough)", res.VisibilityLoss)
		}
		if len(res.FlippedBackends) != 1 || res.FlippedBackends[0] != cutoverBackendName {
			t.Errorf("FlippedBackends = %v, want [%s]", res.FlippedBackends, cutoverBackendName)
		}

		// §4.9: cache flushed inside the tx.
		var cacheRows int64
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_embed_cache`).Scan(&cacheRows); err != nil {
			t.Fatalf("cache count: %v", err)
		}
		if cacheRows != 0 {
			t.Errorf("embed cache rows = %d, want 0 (flush is part of the swap tx)", cacheRows)
		}

		// ctx_rrf untouched: same oid, same definition — the attnum binding
		// did the work, no function generation (§4.8 "Warum das trägt").
		pinsAfter := rrfFunctionPins(t, pool)
		if strings.Join(pinsBefore, "|") != strings.Join(pinsAfter, "|") {
			t.Errorf("ctx_rrf pins changed across the swap:\n before %v\n after  %v", pinsBefore, pinsAfter)
		}

		// Provenance swapped: serving pair carries to_model, _old pair
		// keeps the from_model anchor.
		var servingModel, oldModel string
		if err := pool.QueryRow(ctx,
			`SELECT embed_model, embed_model_old FROM context_blocks WHERE id = $1`, alphaID).
			Scan(&servingModel, &oldModel); err != nil {
			t.Fatalf("read alpha models: %v", err)
		}
		if servingModel != migToModel || oldModel != migFromModel {
			t.Errorf("alpha models = (%q, %q), want (%q, %q)", servingModel, oldModel, migToModel, migFromModel)
		}

		// model_map flipped in the SAME tx + snapshot synchronously reloaded.
		var embedModel string
		var hasNext bool
		if err := pool.QueryRow(ctx,
			`SELECT model_map->'embed'->>'model', model_map ? 'embed_next'
			 FROM context_backends WHERE name = $1`, cutoverBackendName).
			Scan(&embedModel, &hasNext); err != nil {
			t.Fatalf("read flipped model_map: %v", err)
		}
		if embedModel != migToModel || hasNext {
			t.Errorf("model_map post-flip = (embed=%q, embed_next present=%t), want (%q, false)",
				embedModel, hasNext, migToModel)
		}
		var snapModel string
		for _, b := range bpool.Snapshot() {
			if b.Name == cutoverBackendName {
				snapModel = b.ModelFor(backends.RoleEmbed).Model
			}
		}
		if snapModel != migToModel {
			t.Errorf("pool snapshot embed model = %q, want %q (synchronous in-process reload)", snapModel, migToModel)
		}

		// (3b) Attnum class: the canonical partial indexes carry the three
		// scans AFTER the swap...
		for _, probe := range []struct{ needle, sql string }{
			{"idx_embedding_pending on", `SELECT id FROM context_blocks WHERE embedding IS NULL AND NOT is_archived ORDER BY created_at LIMIT 1`},
			{"idx_dream_pending on", `SELECT id FROM context_blocks WHERE NOT is_archived AND embedding IS NOT NULL ORDER BY dream_checked_at ASC NULLS FIRST, quality_score ASC LIMIT 1`},
			{"idx_guard_pending on", `SELECT id FROM context_blocks WHERE NOT is_archived AND (metadata->>'guard_checked_at') IS NULL AND embedding IS NOT NULL ORDER BY created_at ASC LIMIT 1`},
		} {
			if plan := explainPlan(t, pool, probe.sql); !strings.Contains(plan, probe.needle) {
				t.Errorf("post-swap EXPLAIN misses %q — plan:\n%s", probe.needle, plan)
			}
		}
		// ...while the _old exemplars PROVABLY followed the renamed column:
		// without the (3b) renames these predicates would sit under the
		// canonical names and no scan could use them (the seq-scan class).
		var oldPred string
		if err := pool.QueryRow(ctx,
			`SELECT pg_get_expr(i.indpred, i.indrelid) FROM pg_index i
			 JOIN pg_class c ON c.oid = i.indexrelid
			 WHERE c.relname = 'idx_embedding_pending_old'`).Scan(&oldPred); err != nil {
			t.Fatalf("read _old predicate: %v", err)
		}
		if !strings.Contains(oldPred, "embedding_old") {
			t.Errorf("idx_embedding_pending_old predicate = %q, want it bound to embedding_old (attnum follow)", oldPred)
		}

		// Fresh dual pair present and empty (§3.2b invariant holds at every
		// instant — ClearEmbeddingTx/create/purge stay valid post-cutover).
		if !columnExistsT(t, pool, "embedding_next") || !columnExistsT(t, pool, "embed_model_next") {
			t.Fatal("fresh embedding_next pair missing after confirm")
		}
		var freshData int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_blocks WHERE embedding_next IS NOT NULL OR embed_model_next IS NOT NULL`).
			Scan(&freshData); err != nil {
			t.Fatalf("fresh pair probe: %v", err)
		}
		if freshData != 0 {
			t.Errorf("fresh _next pair rows = %d, want 0", freshData)
		}

		// Double confirm: the second call fails the status precondition
		// (and the in-tx CAS would catch a true race the same way).
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); !errors.Is(err, ErrConfirmNotVerifying) {
			t.Fatalf("second Confirm error = %v, want ErrConfirmNotVerifying", err)
		}

		// Simulate an in-flight straddler for the ROLLBACK window: gamma's
		// anchor pair carries a to_model label — after the inverse rename it
		// would SERVE a foreign-labeled vector; the inverse sweep must clear
		// it (§4.10 "inverser Nachlauf-Sweep").
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET embed_model_old = $2 WHERE id = $1`, gammaID, migToModel); err != nil {
			t.Fatalf("plant straddler label: %v", err)
		}

		rres, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "e2e test rollback")
		if err != nil {
			t.Fatalf("Rollback: %v", err)
		}
		if got := nearestTitle(t, pool, probe); got != "alpha" {
			t.Errorf("post-rollback nearest = %q, want alpha (old space serves again)", got)
		}
		st := readMigrationRow(t, pool, migID)
		if st.Status != "rolled_back" {
			t.Errorf("status = %q, want rolled_back", st.Status)
		}
		var reason *string
		if err := pool.QueryRow(ctx,
			`SELECT rollback_reason FROM context_embed_migrations WHERE id = $1::uuid`, migID).Scan(&reason); err != nil {
			t.Fatalf("read rollback_reason: %v", err)
		}
		if reason == nil || *reason != "e2e test rollback" {
			t.Errorf("rollback_reason = %v, want the mandatory operator reason", reason)
		}
		if rres.SweepCleared != 1 {
			t.Errorf("inverse SweepCleared = %d, want 1 (the planted straddler)", rres.SweepCleared)
		}
		var gammaServing *string
		if err := pool.QueryRow(ctx,
			`SELECT embed_model FROM context_blocks WHERE id = $1 AND embedding IS NULL`, gammaID).Scan(&gammaServing); err != nil {
			t.Fatalf("gamma post-sweep read (embedding should be NULL): %v", err)
		}
		if gammaServing != nil {
			t.Errorf("gamma embed_model = %v, want NULL (swept)", *gammaServing)
		}
		// model_map restored: embed → from_model, embed_next re-armed.
		if err := pool.QueryRow(ctx,
			`SELECT model_map->'embed'->>'model', model_map ? 'embed_next'
			 FROM context_backends WHERE name = $1`, cutoverBackendName).
			Scan(&embedModel, &hasNext); err != nil {
			t.Fatalf("read un-flipped model_map: %v", err)
		}
		if embedModel != migFromModel || !hasNext {
			t.Errorf("model_map post-rollback = (embed=%q, embed_next present=%t), want (%q, true)",
				embedModel, hasNext, migFromModel)
		}

		// Rot 4 (§7 row): the rolled-back leftovers block a re-create —
		// without reuse via the rest-data check, with reuse via the
		// rollback refusal (data is on record as suspicious).
		params := embedmigration.CreateParams{
			FromModel: migFromModel, ToModel: migToModel, ToBackend: cutoverBackendName,
		}
		if _, err := embedmigration.Create(ctx, pool, params, abundantCutoverDisk); !errors.Is(err, embedmigration.ErrRestEmbeddingNextData) {
			t.Fatalf("Create (no reuse) error = %v, want ErrRestEmbeddingNextData", err)
		}
		params.ReuseExisting = true
		if _, err := embedmigration.Create(ctx, pool, params, abundantCutoverDisk); !errors.Is(err, embedmigration.ErrReuseAfterRollback) {
			t.Fatalf("Create (reuse) error = %v, want ErrReuseAfterRollback", err)
		}
	})

	// Memo re-homing (§4.8 Restmengen): an infinity-parked skip block
	// neither blocks the confirm (memo exception in the re-check) nor loops
	// after the swap (backfill-scoped infinity memo).
	t.Run("memo_copy_probe", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedConfirmReady(t, pool, 1)
		seedBothSpaces(t, pool, "memo-clean", 3*time.Hour, verifyUnitVec(21), verifyUnitVec(22), migToModel)
		skipID := seedMigratableBlock(t, pool, "memo-skip", "oversize body", "internal", 2*time.Hour)
		if err := store.RecordEmbedFailureForMigration(ctx, pool, skipID, migID,
			store.EmbedFailureOversize, "pre-wire estimate 99999 tokens > max_tokens 24000",
			time.Minute, time.Hour); err != nil {
			t.Fatalf("plant infinity memo: %v", err)
		}

		res, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID)
		if err != nil {
			t.Fatalf("Confirm must pass with a declared skip (memo exception): %v", err)
		}
		if res.MemosCopied != 1 {
			t.Errorf("MemosCopied = %d, want 1", res.MemosCopied)
		}
		var parked bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM context_embed_failures
			 WHERE block_id = $1 AND migration_id IS NULL AND next_attempt_at = 'infinity')`,
			skipID).Scan(&parked); err != nil {
			t.Fatalf("backfill memo probe: %v", err)
		}
		if !parked {
			t.Error("skip block has no backfill infinity memo — it would wire-retry forever post-swap")
		}
		// The park keeps it OUT of the post-swap pending transient.
		if res.PostWatermarkPending != 0 {
			t.Errorf("PostWatermarkPending = %d, want 0 (parked block excluded)", res.PostWatermarkPending)
		}
		var servingNull bool
		if err := pool.QueryRow(ctx,
			`SELECT embedding IS NULL FROM context_blocks WHERE id = $1`, skipID).Scan(&servingNull); err != nil {
			t.Fatalf("skip serving probe: %v", err)
		}
		if !servingNull {
			t.Error("skip block still serves a vector — visibility loss not realized as designed")
		}
		if _, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "test baseline restore"); err != nil {
			t.Fatalf("baseline rollback: %v", err)
		}
	})

	// Nachlauf-Sweep (§4.8): a reload-latency window write — a from_model-
	// labeled vector that lands in the NEW serving column — is cleared by
	// the sweep right after the swap (M109 provenance makes it detectable).
	t.Run("window_write_sweep_probe", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedConfirmReady(t, pool, 0)
		seedBothSpaces(t, pool, "sweep-clean", 3*time.Hour, verifyUnitVec(31), verifyUnitVec(32), migToModel)
		// Post-watermark block whose _next slot carries a FROM-labeled
		// vector: after the rename that pair IS the serving pair — the
		// simulated Pfad-A/B write of the reload-latency window.
		var windowID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, sensitivity,
			        embedding, embed_model, embedding_next, embed_model_next, created_at, updated_at)
			 VALUES ('learnings', 'window-write', 'body', 'shared', 'internal',
			        $1, $2, $3, $2, now() + interval '1 hour', now())
			 RETURNING id::text`,
			pgvec.NewVector(verifyUnitVec(33)), migFromModel, pgvec.NewVector(verifyUnitVec(34))).
			Scan(&windowID); err != nil {
			t.Fatalf("seed window-write block: %v", err)
		}

		res, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID)
		if err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if res.SweepCleared != 1 {
			t.Errorf("SweepCleared = %d, want 1 (the window write)", res.SweepCleared)
		}
		var emb, mod *string
		if err := pool.QueryRow(ctx,
			`SELECT embedding::text, embed_model FROM context_blocks WHERE id = $1`, windowID).
			Scan(&emb, &mod); err != nil {
			t.Fatalf("window block read: %v", err)
		}
		if emb != nil || mod != nil {
			t.Errorf("window block serving pair = (%v, %v), want (NULL, NULL) — sweep must clear the foreign label", emb, mod)
		}
		if _, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "test baseline restore"); err != nil {
			t.Fatalf("baseline rollback: %v", err)
		}
	})

	// Cleanup: only from done, drops the anchor BY NAME, and afterwards the
	// rollback path is provably gone. LAST confirm of the suite — cleanup
	// intentionally leaves the post-cutover schema in place.
	t.Run("cleanup_probe", func(t *testing.T) {
		defer resetCutoverFixture(t, pool)
		migID := seedConfirmReady(t, pool, 0)
		seedBothSpaces(t, pool, "cleanup-clean", 2*time.Hour, verifyUnitVec(41), verifyUnitVec(42), migToModel)

		// Fail-closed: not done yet.
		if err := CleanupEmbedMigration(ctx, pool, migID); !errors.Is(err, ErrCleanupNotDone) {
			t.Fatalf("Cleanup error = %v, want ErrCleanupNotDone", err)
		}
		if _, err := ConfirmEmbedMigration(ctx, pool, newBPool(), migID); err != nil {
			t.Fatalf("Confirm: %v", err)
		}
		if err := CleanupEmbedMigration(ctx, pool, migID); err != nil {
			t.Fatalf("Cleanup: %v", err)
		}
		for _, col := range []string{"embedding_old", "embed_model_old"} {
			if columnExistsT(t, pool, col) {
				t.Errorf("column %s still exists after cleanup", col)
			}
		}
		for _, idx := range []string{"idx_embedding_hnsw_old", "idx_embedding_pending_old", "idx_dream_pending_old", "idx_guard_pending_old"} {
			if indexExistsT(t, pool, idx) {
				t.Errorf("index %s still exists after cleanup", idx)
			}
		}
		// The permanent dual pair survives cleanup (§3.2b) — the next
		// migration needs it.
		if !columnExistsT(t, pool, "embedding_next") || !columnExistsT(t, pool, "embed_model_next") {
			t.Error("permanent _next pair missing after cleanup")
		}
		// Rollback is now impossible — and says so.
		if _, err := RollbackEmbedMigration(ctx, pool, newBPool(), migID, "too late"); !errors.Is(err, ErrRollbackOldResourcesMissing) {
			t.Fatalf("post-cleanup Rollback error = %v, want ErrRollbackOldResourcesMissing", err)
		}
		// Second cleanup: nothing left to drop, named refusal.
		if err := CleanupEmbedMigration(ctx, pool, migID); !errors.Is(err, ErrCleanupOldResourcesMissing) {
			t.Fatalf("second Cleanup error = %v, want ErrCleanupOldResourcesMissing", err)
		}
	})
}
