//go:build integration

package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/testdb"
)

// IDs für das Welle-46 (M042) Full-Reset Szenario.
const (
	frBlockAID = "019e8888-0001-7000-9000-000000000001" // A: eligible knowledge block (mit dream-state)
	frBlockBID = "019e8888-0001-7000-9000-000000000002" // B: eligible knowledge block (mit dream-state)
	frBlockCID = "019e8888-0001-7000-9000-000000000003" // C: meta-block — NICHT eligible, bleibt unverändert
)

// insertFullResetBlock schreibt einen context_blocks-Row mit konfigurierbaren
// is_meta und dream-state Feldern. Die Felder dream_checked_at, dream_cooldown_until,
// dream_keywords und dream_temporal_validated_at werden gesetzt, damit der Test
// nachweisen kann, dass die Migration sie NULLed (für eligible blocks) bzw.
// unverändert lässt (für meta-blocks).
func insertFullResetBlock(
	t *testing.T,
	pool *pgxpool.Pool,
	id, category, title string,
	isMeta bool,
	checkedAt, cooldownUntil, temporalValidatedAt *time.Time,
	keywords []string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	embedding := make([]float32, 1024)
	for i := range embedding {
		embedding[i] = 0.01
	}
	vec := pgvec.NewVector(embedding)

	_, err := pool.Exec(ctx,
		`INSERT INTO context_blocks
		   (id, category, title, content, scope, embedding, is_meta, lifecycle_state,
		    dream_checked_at, dream_cooldown_until, dream_keywords,
		    dream_temporal_validated_at, created_at, updated_at)
		 VALUES ($1::uuid, $2, $3, $4, 'private', $5, $6, 'knowledge',
		    $7, $8, $9, $10, now(), now())`,
		id, category, title, "full-reset-test-content", vec, isMeta,
		checkedAt, cooldownUntil, keywords, temporalValidatedAt,
	)
	if err != nil {
		t.Fatalf("insert block %s: %v", id, err)
	}
}

// insertFullResetLink legt einen dream_link zwischen zwei Blocks an. Wird vom
// Full-Reset (TRUNCATE) sowieso gelöscht — die Insertion dient nur dazu,
// das Pre-State-Setup realistisch zu machen.
func insertFullResetLink(t *testing.T, pool *pgxpool.Pool, src, tgt string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := pool.Exec(ctx,
		`INSERT INTO context_dream_links
		   (source_block_id, target_block_id, relationship, confidence,
		    raw_confidence, scope, dream_version)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'private', 5)`,
		src, tgt,
	)
	if err != nil {
		t.Fatalf("insert link %s->%s: %v", src, tgt, err)
	}
}

// applyM042 führt das Migration-Statement inline aus. Mirror von
// go/migrations/042_dream_full_reset.sql — jede Diskrepanz hier ist ein
// Hinweis dass die SQL-File auseinanderläuft → in Migration-Review prüfen.
// Inline statt RunMigrations, weil zum Zeitpunkt von testdb-Setup unsere
// Test-Blocks noch nicht existieren und M042 keinerlei Effekt hätte.
// Seit M070 ist der Mirror aufs Post-Rename-Schema adaptiert (Alt-Spalte →
// lifecycle_state, NULL backfilled, 'source' tot): die Test-DB ist voll
// migriert, das historische 042-SQL würde hier mit 42703 brechen.
func applyM042(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := pool.Exec(ctx, `TRUNCATE TABLE context_dream_links`); err != nil {
		t.Fatalf("apply M042 (TRUNCATE): %v", err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE context_blocks
		   SET dream_checked_at = NULL,
		       dream_cooldown_until = NULL,
		       dream_keywords = NULL,
		       dream_temporal_validated_at = NULL
		 WHERE NOT is_archived
		   AND embedding IS NOT NULL
		   AND lifecycle_state IN ('knowledge', 'canonical')
		   AND NOT is_meta
	`); err != nil {
		t.Fatalf("apply M042 (UPDATE): %v", err)
	}
}

// TestMigration042_FullReset_BehaviourMatchesContract verifiziert Welle 46:
// Der gesamte dream-Korpus wird auf eine saubere Baseline reset. Alle
// dream_links werden gelöscht, alle eligible knowledge-Blocks bekommen
// dream_checked_at/dream_cooldown_until/dream_keywords/
// dream_temporal_validated_at = NULL. Meta-Blocks bleiben unverändert.
//
// Setup:
//   - A (eligible): is_meta=false, dream-state komplett gesetzt → muss reset werden
//   - B (eligible): is_meta=false, dream-state komplett gesetzt → muss reset werden
//   - C (meta):     is_meta=true,  dream-state gesetzt          → bleibt unverändert
//
// Inserts 2 dream_links A↔B; nach TRUNCATE sind 0 dream_links übrig.
func TestMigration042_FullReset_BehaviourMatchesContract(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	checked := time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)
	cooldown := time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC)
	temporal := time.Date(2026, 5, 2, 9, 30, 0, 0, time.UTC)
	keywords := []string{"alpha", "beta", "gamma"}

	insertFullResetBlock(t, pool, frBlockAID, "learnings", "block-A", false,
		&checked, &cooldown, &temporal, keywords)
	insertFullResetBlock(t, pool, frBlockBID, "learnings", "block-B", false,
		&checked, &cooldown, &temporal, keywords)
	insertFullResetBlock(t, pool, frBlockCID, "reference", "meta-C", true,
		&checked, &cooldown, &temporal, keywords)

	insertFullResetLink(t, pool, frBlockAID, frBlockBID)
	insertFullResetLink(t, pool, frBlockBID, frBlockAID)

	// Pre-Assert: 2 dream_links da, A+B+C dream-state nicht-null.
	var linkCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links
		 WHERE source_block_id IN ($1::uuid, $2::uuid)
		    OR target_block_id IN ($1::uuid, $2::uuid)`,
		frBlockAID, frBlockBID,
	).Scan(&linkCount); err != nil {
		t.Fatalf("count A/B links pre: %v", err)
	}
	if linkCount != 2 {
		t.Fatalf("setup: want 2 links A↔B, got %d", linkCount)
	}

	// Apply M042.
	applyM042(t, pool)

	// Assert: 0 dream_links für A/B (eigentlich für die ganze Tabelle).
	var totalLinks int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links`,
	).Scan(&totalLinks); err != nil {
		t.Fatalf("count total links post: %v", err)
	}
	if totalLinks != 0 {
		t.Errorf("dream_links post-reset: want 0, got %d", totalLinks)
	}

	// Assert: A reset (alle vier dream-Felder NULL).
	assertBlockReset(t, pool, "A", frBlockAID)
	// Assert: B reset.
	assertBlockReset(t, pool, "B", frBlockBID)
	// Assert: C unverändert (meta-block).
	assertBlockUnchanged(t, pool, "C", frBlockCID, checked, cooldown, temporal, keywords)

	// Idempotenz: zweiter Apply ändert nichts mehr. A/B sind schon NULL, C
	// bleibt durch is_meta=true ausgeschlossen, dream_links-Tabelle ist leer.
	applyM042(t, pool)

	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_dream_links`,
	).Scan(&totalLinks); err != nil {
		t.Fatalf("count total links post idempotent: %v", err)
	}
	if totalLinks != 0 {
		t.Errorf("dream_links post idempotent: want 0, got %d", totalLinks)
	}
	assertBlockReset(t, pool, "A (idempotent)", frBlockAID)
	assertBlockReset(t, pool, "B (idempotent)", frBlockBID)
	assertBlockUnchanged(t, pool, "C (idempotent)", frBlockCID, checked, cooldown, temporal, keywords)
}

// assertBlockReset prüft, dass alle vier dream-State-Felder NULL sind.
func assertBlockReset(t *testing.T, pool *pgxpool.Pool, label, id string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var checked, cooldown, temporal *time.Time
	var keywords []string
	if err := pool.QueryRow(ctx,
		`SELECT dream_checked_at, dream_cooldown_until,
		        dream_temporal_validated_at, dream_keywords
		   FROM context_blocks WHERE id = $1::uuid`,
		id,
	).Scan(&checked, &cooldown, &temporal, &keywords); err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	if checked != nil {
		t.Errorf("%s.dream_checked_at: want NULL, got %v", label, *checked)
	}
	if cooldown != nil {
		t.Errorf("%s.dream_cooldown_until: want NULL, got %v", label, *cooldown)
	}
	if temporal != nil {
		t.Errorf("%s.dream_temporal_validated_at: want NULL, got %v", label, *temporal)
	}
	if keywords != nil {
		t.Errorf("%s.dream_keywords: want NULL, got %v", label, keywords)
	}
}

// assertBlockUnchanged prüft, dass alle vier dream-State-Felder unverändert
// zum erwarteten Wert sind.
func assertBlockUnchanged(
	t *testing.T,
	pool *pgxpool.Pool,
	label, id string,
	wantChecked, wantCooldown, wantTemporal time.Time,
	wantKeywords []string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var checked, cooldown, temporal *time.Time
	var keywords []string
	if err := pool.QueryRow(ctx,
		`SELECT dream_checked_at, dream_cooldown_until,
		        dream_temporal_validated_at, dream_keywords
		   FROM context_blocks WHERE id = $1::uuid`,
		id,
	).Scan(&checked, &cooldown, &temporal, &keywords); err != nil {
		t.Fatalf("read %s: %v", label, err)
	}
	if checked == nil || !checked.Equal(wantChecked) {
		t.Errorf("%s.dream_checked_at: want %v (unchanged), got %v", label, wantChecked, checked)
	}
	if cooldown == nil || !cooldown.Equal(wantCooldown) {
		t.Errorf("%s.dream_cooldown_until: want %v (unchanged), got %v", label, wantCooldown, cooldown)
	}
	if temporal == nil || !temporal.Equal(wantTemporal) {
		t.Errorf("%s.dream_temporal_validated_at: want %v (unchanged), got %v", label, wantTemporal, temporal)
	}
	if !stringSliceEqual(keywords, wantKeywords) {
		t.Errorf("%s.dream_keywords: want %v (unchanged), got %v", label, wantKeywords, keywords)
	}
}

// stringSliceEqual is a small helper to avoid pulling in reflect for a tiny
// keyword slice comparison.
func stringSliceEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
