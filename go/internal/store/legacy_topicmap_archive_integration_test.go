//go:build integration

// Wave W-H (plan-cluster-topicmap design/02 §4.6/§7 "W-H"; user decision
// E2-02 A): the two dead linear maps `topic-map-work` and `topic-map-hth`
// disappear from the hit list — ARCHIVED, never deleted.
//
// The gate drives the real ops SQL (scripts/archive-legacy-topic-maps.sql), not
// a re-implementation of it. That is the whole point of a data step whose
// correctness is the difference between "gone from search" and "gone": a test
// that re-typed the statement would prove something about the test.
//
// It runs against a testcontainer and nothing else. The live run is an
// operations step after the deploy, with explicit approval (project deploy
// doctrine).
package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

type archiveRow struct {
	id, title, scope, sha, rollback string
	length, archivedN               int
}

func archiveScriptPath(t *testing.T) string {
	t.Helper()
	return filepath.Join("..", "..", "..", "scripts", "archive-legacy-topic-maps.sql")
}

func runArchiveScript(t *testing.T, pool *pgxpool.Pool) []archiveRow {
	t.Helper()
	body, err := os.ReadFile(archiveScriptPath(t))
	if err != nil {
		t.Fatalf("read ops script: %v", err)
	}
	rows, err := pool.Query(context.Background(), string(body))
	if err != nil {
		t.Fatalf("run ops script: %v", err)
	}
	defer rows.Close()
	var out []archiveRow
	for rows.Next() {
		var r archiveRow
		if err := rows.Scan(&r.id, &r.title, &r.scope, &r.length, &r.sha, &r.archivedN, &r.rollback); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func seedLegacyMaps(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, m := range []struct{ title, scope, content string }{
		{"topic-map-private", "private", strings.Repeat("p", 400)},
		{"topic-map-work", "work", strings.Repeat("w", 1344)},
		{"topic-map-hth", "work", strings.Repeat("h", 1132)},
	} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (category, title, content, scope, metadata)
			 VALUES ('index', $1, $2, $3, '{"is_meta": true}'::jsonb)`,
			m.title, m.content, m.scope); err != nil {
			t.Fatalf("seed %s: %v", m.title, err)
		}
	}
	// The live producer of the surviving map: an ACTIVE key whose home scope is
	// 'private'. The guard in the script keys on exactly this.
	seedActiveKey(t, pool, "wh-test-key-hash", "private")
}

// seedActiveKey inserts one minimal, ACTIVE key with the given home scope — the
// producer signal the ops script's guard keys on. principal_id is NOT NULL since
// migration 096, so the principal comes with it.
func seedActiveKey(t *testing.T, pool *pgxpool.Pool, keyHash, homeScope string) {
	t.Helper()
	if _, err := pool.Exec(context.Background(),
		`WITH p AS (
		     INSERT INTO context_principals (display_name) VALUES ($2) RETURNING id
		 )
		 INSERT INTO context_api_keys (key_hash, label, home_scope, allowed_scopes, active, principal_id)
		 SELECT $1, $2, $3::varchar, ARRAY[$3::text], true, p.id FROM p`,
		keyHash, keyHash, homeScope); err != nil {
		t.Fatalf("seed active key %s: %v", keyHash, err)
	}
}

func archivedState(t *testing.T, pool *pgxpool.Pool, title string) (archived, exists bool) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT is_archived FROM context_blocks WHERE category = 'index' AND title = $1`, title).Scan(&archived)
	if err != nil {
		return false, false
	}
	return archived, true
}

// W-H-1..5 — the five points of the reworked gate (design/02 §7 W-H).
func TestArchiveLegacyTopicMaps(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	seedLegacyMaps(t, pool)

	// (1) The BEFORE state is pinned by the script's own output: ids, lengths
	// and content hashes. Without that pin "the same block, only flagged" is an
	// assertion nobody can check afterwards.
	got := runArchiveScript(t, pool)
	if len(got) != 2 {
		t.Fatalf("script touched %d blocks, want exactly 2 (%v)", len(got), got)
	}
	byTitle := map[string]archiveRow{}
	for _, r := range got {
		byTitle[r.title] = r
		if r.scope != "work" {
			t.Errorf("%s reported scope %q, want work", r.title, r.scope)
		}
		if r.sha == "" || len(r.sha) != 64 {
			t.Errorf("%s: content_sha256 = %q, want a 64-hex pin", r.title, r.sha)
		}
		if r.archivedN != 2 {
			t.Errorf("%s: archived_n = %d, want 2", r.title, r.archivedN)
		}
	}
	if byTitle["topic-map-work"].length != 1344 || byTitle["topic-map-hth"].length != 1132 {
		t.Errorf("pinned lengths %d/%d, want 1344/1132",
			byTitle["topic-map-work"].length, byTitle["topic-map-hth"].length)
	}

	// (2) AFTER: the two carry is_archived, and they are gone from the shape a
	// search uses — while `ctx get <id>` still reads them. THAT is the rollback
	// path, and it is why the step is not a DELETE.
	for _, title := range []string{"topic-map-work", "topic-map-hth"} {
		archived, exists := archivedState(t, pool, title)
		if !exists {
			t.Fatalf("%s no longer exists — the step deleted instead of archiving", title)
		}
		if !archived {
			t.Errorf("%s is not archived", title)
		}
	}
	var findable int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_blocks
		  WHERE category = 'index' AND NOT is_archived AND title LIKE 'topic-map-%'`).Scan(&findable); err != nil {
		t.Fatalf("count findable: %v", err)
	}
	if findable != 1 {
		t.Errorf("%d findable topic maps, want 1 (only the active tenant's)", findable)
	}
	var content string
	if err := pool.QueryRow(ctx,
		`SELECT content FROM context_blocks WHERE id = $1::uuid`, byTitle["topic-map-hth"].id).Scan(&content); err != nil {
		t.Fatalf("get-by-id on the archived block: %v — the rollback path is broken", err)
	}
	if len(content) != 1132 {
		t.Errorf("archived content is %d chars, want the pinned 1132", len(content))
	}

	// (3) UNTOUCHED: the active tenant's map. Two belts hold here — the title
	// allowlist and the producer guard — and this asserts the result of both.
	if archived, exists := archivedState(t, pool, "topic-map-private"); !exists || archived {
		t.Errorf("topic-map-private: exists=%v archived=%v — the step reached the active tenant's map", exists, archived)
	}

	// (5) The ROLLBACK is a named one-liner, and it is named by the script
	// itself: every row carries its own. Executing it restores the before state
	// completely.
	for _, r := range got {
		if !strings.Contains(r.rollback, "is_archived = false") || !strings.Contains(r.rollback, r.id) {
			t.Fatalf("%s: rollback_sql %q is not a usable one-liner", r.title, r.rollback)
		}
		if _, err := pool.Exec(ctx, r.rollback); err != nil {
			t.Fatalf("%s: rollback failed: %v", r.title, err)
		}
	}
	for _, title := range []string{"topic-map-work", "topic-map-hth"} {
		if archived, _ := archivedState(t, pool, title); archived {
			t.Errorf("%s still archived after its rollback one-liner", title)
		}
	}
	// And the content hash is still the pinned one — the step never rewrote a byte.
	for _, r := range got {
		var sha string
		if err := pool.QueryRow(ctx,
			`SELECT encode(sha256(convert_to(content, 'UTF8')), 'hex') FROM context_blocks WHERE id = $1::uuid`,
			r.id).Scan(&sha); err != nil {
			t.Fatalf("%s: re-hash: %v", r.title, err)
		}
		if sha != r.sha {
			t.Errorf("%s: content hash changed %s → %s", r.title, r.sha, sha)
		}
	}
}

// The producer guard: as long as an ACTIVE key calls that scope home, something
// still writes the map there and archiving would fight a live producer. The
// script skips it — silently in the sense of "returns no row", loudly in the
// sense that the operator sees an empty result instead of a surprise.
func TestArchiveLegacyTopicMaps_ProducerGuard(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	seedLegacyMaps(t, pool)

	seedActiveKey(t, pool, "wh-work-key-hash", "work")

	if got := runArchiveScript(t, pool); len(got) != 0 {
		t.Errorf("script archived %d blocks although an active key calls 'work' home", len(got))
	}
	for _, title := range []string{"topic-map-work", "topic-map-hth"} {
		if archived, _ := archivedState(t, pool, title); archived {
			t.Errorf("%s archived despite a live producer in its scope", title)
		}
	}
}

// A second run is a no-op: the script only picks up blocks that are NOT yet
// archived. An ops step an operator can safely repeat is worth more than one
// that has to be run exactly once.
func TestArchiveLegacyTopicMaps_Repeatable(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	seedLegacyMaps(t, pool)

	if got := runArchiveScript(t, pool); len(got) != 2 {
		t.Fatalf("first run touched %d blocks, want 2", len(got))
	}
	if got := runArchiveScript(t, pool); len(got) != 0 {
		t.Errorf("second run touched %d blocks, want 0", len(got))
	}
}

// (4) THE RED PROBE, as a property of the artefact: a script that DELETEs makes
// point (2) — "still readable via ctx get, therefore reversible" — unsatisfiable.
// Pinning the absence of a destructive statement keeps that from being
// re-introduced by a later "cleanup".
func TestArchiveLegacyTopicMaps_NeverDeletes(t *testing.T) {
	body, err := os.ReadFile(archiveScriptPath(t))
	if err != nil {
		t.Fatalf("read ops script: %v", err)
	}
	var code []string
	for _, line := range strings.Split(string(body), "\n") {
		if s := strings.TrimSpace(line); !strings.HasPrefix(s, "--") {
			code = append(code, strings.ToUpper(s))
		}
	}
	sql := strings.Join(code, "\n")
	for _, forbidden := range []string{"DELETE", "TRUNCATE", "DROP "} {
		if strings.Contains(sql, forbidden) {
			t.Errorf("the ops script contains %q — the reversibility gate (get-by-id after the step) cannot hold", forbidden)
		}
	}
	if !strings.Contains(sql, "IS_ARCHIVED = TRUE") {
		t.Error("the ops script does not set is_archived — it is not the step W-H describes")
	}
}
