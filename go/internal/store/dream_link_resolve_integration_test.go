//go:build integration

// DreamLinkResolve contract (dream-link curation wave, 2026-07-26):
//   - confirm pins the link (pinned=true) and stores the rationale; confirm
//     without rationale keeps an earlier justification
//   - delete removes the link; for relationship=supersedes the target's
//     snapshot marking is reverted (lifecycle_state → 'knowledge',
//     superseded_by → NULL) — only while it still points at THIS source
//   - foreign-scope, absent, malformed ids and a relationship mismatch all
//     collapse into (nil, nil) — uniform not found, no existence oracle
//   - empty write-scope set fails closed with ErrNoScopes (T07 line)
//
//	go test -tags=integration ./internal/store/ -run TestDreamLinkResolve -count=1 -v
package store_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func seedDreamBlock(t *testing.T, pool *pgxpool.Pool, title, scope string) string {
	t.Helper()
	var id string
	err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope, type_name)
		 VALUES ('learnings', $1, 'content of '||$1, $2, 'knowledge')
		 RETURNING id::text`,
		title, scope,
	).Scan(&id)
	if err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func seedDreamLink(t *testing.T, pool *pgxpool.Pool, sourceID, targetID, relationship, scope string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, $3, 0.8, 0.8, $4)`,
		sourceID, targetID, relationship, scope,
	)
	if err != nil {
		t.Fatalf("seed link %s->%s: %v", sourceID, targetID, err)
	}
}

func readLinkState(t *testing.T, pool *pgxpool.Pool, sourceID, targetID string) (exists, pinned bool, rationale *string) {
	t.Helper()
	err := pool.QueryRow(context.Background(),
		`SELECT pinned, rationale FROM context_dream_links
		 WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`,
		sourceID, targetID,
	).Scan(&pinned, &rationale)
	if err != nil {
		return false, false, nil
	}
	return true, pinned, rationale
}

func TestDreamLinkResolve_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	writeScopes := []string{"private"}

	t.Run("confirm pins and stores rationale", func(t *testing.T) {
		src := seedDreamBlock(t, pool, "resolve-confirm-src", "private")
		tgt := seedDreamBlock(t, pool, "resolve-confirm-tgt", "private")
		seedDreamLink(t, pool, src, tgt, "topical", "private")

		res, err := store.DreamLinkResolve(ctx, pool, src, tgt, "topical", "confirm", "human-verified pairing", writeScopes)
		if err != nil {
			t.Fatalf("confirm: %v", err)
		}
		if res == nil || !res.Pinned || res.Rationale == nil || *res.Rationale != "human-verified pairing" {
			t.Fatalf("confirm result = %+v, want pinned with rationale", res)
		}

		// State assertion against the database, not the return value.
		exists, pinned, rationale := readLinkState(t, pool, src, tgt)
		if !exists || !pinned || rationale == nil || *rationale != "human-verified pairing" {
			t.Errorf("after confirm: exists=%v pinned=%v rationale=%v", exists, pinned, rationale)
		}

		// Re-confirm WITHOUT rationale keeps the earlier justification.
		res, err = store.DreamLinkResolve(ctx, pool, src, tgt, "topical", "confirm", "", writeScopes)
		if err != nil {
			t.Fatalf("re-confirm: %v", err)
		}
		if res == nil || res.Rationale == nil || *res.Rationale != "human-verified pairing" {
			t.Errorf("re-confirm result = %+v, want earlier rationale kept", res)
		}
	})

	t.Run("delete removes link and reverts supersedes", func(t *testing.T) {
		src := seedDreamBlock(t, pool, "resolve-del-sup-src", "private")
		tgt := seedDreamBlock(t, pool, "resolve-del-sup-tgt", "private")
		seedDreamLink(t, pool, src, tgt, "supersedes", "private")
		// ApplySupersedes side-effect as WriteLinks produces it.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`,
			src, tgt); err != nil {
			t.Fatalf("mark snapshot: %v", err)
		}

		res, err := store.DreamLinkResolve(ctx, pool, src, tgt, "supersedes", "delete", "", writeScopes)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if res == nil || !res.SupersedesReverted {
			t.Fatalf("delete result = %+v, want supersedes_reverted=true", res)
		}

		if exists, _, _ := readLinkState(t, pool, src, tgt); exists {
			t.Error("link still exists after delete")
		}
		var lifecycle string
		var supersededBy *string
		if err := pool.QueryRow(ctx,
			`SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, tgt,
		).Scan(&lifecycle, &supersededBy); err != nil {
			t.Fatalf("read target: %v", err)
		}
		if lifecycle != "knowledge" || supersededBy != nil {
			t.Errorf("target after revert: lifecycle=%q superseded_by=%v, want knowledge/nil", lifecycle, supersededBy)
		}
	})

	t.Run("delete of supersedes leaves foreign snapshot marking alone", func(t *testing.T) {
		src := seedDreamBlock(t, pool, "resolve-del-keep-src", "private")
		tgt := seedDreamBlock(t, pool, "resolve-del-keep-tgt", "private")
		other := seedDreamBlock(t, pool, "resolve-del-keep-other", "private")
		seedDreamLink(t, pool, src, tgt, "supersedes", "private")
		// Snapshot marking points at ANOTHER source — must NOT be reverted.
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`,
			other, tgt); err != nil {
			t.Fatalf("mark snapshot: %v", err)
		}

		res, err := store.DreamLinkResolve(ctx, pool, src, tgt, "supersedes", "delete", "", writeScopes)
		if err != nil {
			t.Fatalf("delete: %v", err)
		}
		if res == nil || res.SupersedesReverted {
			t.Fatalf("delete result = %+v, want supersedes_reverted=false (foreign marking)", res)
		}
		var lifecycle string
		if err := pool.QueryRow(ctx,
			`SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, tgt).Scan(&lifecycle); err != nil {
			t.Fatalf("read target: %v", err)
		}
		if lifecycle != "snapshot" {
			t.Errorf("foreign snapshot marking was reverted: lifecycle=%q", lifecycle)
		}
	})

	t.Run("uniform not found", func(t *testing.T) {
		src := seedDreamBlock(t, pool, "resolve-nf-src", "hth")
		tgt := seedDreamBlock(t, pool, "resolve-nf-tgt", "hth")
		seedDreamLink(t, pool, src, tgt, "topical", "hth")

		cases := map[string][3]string{
			"foreign scope":         {src, tgt, "topical"},
			"relationship mismatch": {src, tgt, "causal"},
			"absent link":           {tgt, src, "topical"},
			"malformed id":          {"not-a-uuid", tgt, "topical"},
		}
		for name, c := range cases {
			res, err := store.DreamLinkResolve(ctx, pool, c[0], c[1], c[2], "confirm", "", writeScopes)
			if err != nil || res != nil {
				t.Errorf("%s: res=%v err=%v, want nil/nil (uniform not found)", name, res, err)
			}
		}

		// The foreign link must be untouched.
		if exists, pinned, _ := readLinkState(t, pool, src, tgt); !exists || pinned {
			t.Errorf("foreign link touched: exists=%v pinned=%v", exists, pinned)
		}
	})

	t.Run("fails closed on empty write scopes", func(t *testing.T) {
		src := seedDreamBlock(t, pool, "resolve-noscope-src", "private")
		tgt := seedDreamBlock(t, pool, "resolve-noscope-tgt", "private")
		seedDreamLink(t, pool, src, tgt, "topical", "private")

		_, err := store.DreamLinkResolve(ctx, pool, src, tgt, "topical", "confirm", "", []string{})
		if !errors.Is(err, store.ErrNoScopes) {
			t.Fatalf("err = %v, want ErrNoScopes", err)
		}
	})

	t.Run("rejects invalid resolution", func(t *testing.T) {
		if _, err := store.DreamLinkResolve(ctx, pool, "00000000-0000-7000-8000-000000000001", "00000000-0000-7000-8000-000000000002", "topical", "archive", "", writeScopes); err == nil {
			t.Error("invalid resolution accepted")
		}
	})
}
