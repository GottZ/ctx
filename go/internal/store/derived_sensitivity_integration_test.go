//go:build integration

// Wave W01-5 (design/01 §4.8.1, §4.8.2, §4.3.1 + §7 W01-5 gates 1, 2, 6 and the
// server-path badge) against a real PG18 testcontainer.
//
// The write half of the wave, and it is measured at the COLUMN, never at the
// returned struct: the fold lives inside the ON CONFLICT expression, so a
// probe that reads what UpsertBlock returns would be reading the same
// arithmetic twice instead of the row it produced.
//
//	go test -tags=integration ./internal/store/ -run TestDerivedSensitivity -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// dsRow reads the two columns the fold writes, straight out of the table.
func dsRow(t *testing.T, pool *pgxpool.Pool, category, title, scope string) (sens, source string) {
	t.Helper()
	if err := pool.QueryRow(context.Background(),
		`SELECT sensitivity, sensitivity_source FROM context_blocks
		  WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived`,
		category, title, scope).Scan(&sens, &source); err != nil {
		t.Fatalf("read %s/%s: %v", category, title, err)
	}
	return
}

// dsHasProvenance reports whether the live row under this identity still carries
// the provenance key — the predicate S3 itself hangs on.
func dsHasProvenance(t *testing.T, pool *pgxpool.Pool, title, scope string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_blocks
		  WHERE category='learnings' AND title=$1 AND scope=$2 AND NOT is_archived
		    AND metadata ? '`+derived.MetadataKey+`'`, title, scope).Scan(&n); err != nil {
		t.Fatalf("provenance probe %s: %v", title, err)
	}
	return n == 1
}

// dsProvenanceMD is the metadata a derived writer carries: one namespaced key,
// the same shape handler/derived_write_lock_integration_test.go seeds. It is
// what makes the I7/S3 guard bite, so every badge probe below runs against a
// row that really is a derivative.
func dsProvenanceMD() map[string]any { return dsProvenanceOf("w015-arm", derived.StratumDerived) }

// dsProvenanceOf names the WRITER. arm and stratum are the identity the guard
// binds a regeneration to (review finding #2): without them the badge opened
// every provenance row, and a second arm could take over the first one's block
// under the same (category, title, scope).
func dsProvenanceOf(arm string, stratum derived.Stratum) map[string]any {
	return map[string]any{
		derived.MetadataKey: map[string]any{
			"v":       derived.ContractVersion,
			"stratum": int(stratum),
			"arm":     arm,
		},
	}
}

func TestDerivedSensitivityWrite_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const scope = "w015w"
	// Deliberately free of anything sensitivity.Scan matches: the G40 detector
	// runs on EVERY upsert path since V-W8, and a hit would set
	// sensitivity_source='pattern' and make these probes measure the scanner.
	const body = "Drei Quellen, eine Aussage je Quelle, nichts weiter."

	set := rsRegistry(t, pool)
	noFloor := config.ScopeFloor{}.Apply

	// foldOf seeds real source blocks, resolves them and hands back the value the
	// arm would write. This is the whole point of gate 1 and the reason the first
	// version of these two subtests proved nothing: they passed a hand-picked
	// SensitivityWrite.Value, so the chain ResolveSources → FlooredMax → column
	// was never connected. The reviewer replaced the fold with a constant and
	// both subtests stayed green (review finding #5).
	foldOf := func(t *testing.T, prefix string, levels ...string) backends.Sensitivity {
		t.Helper()
		ids := make([]string, 0, len(levels))
		for i, lvl := range levels {
			ids = append(ids, rsInsert(t, pool, rsSeed{
				title: fmt.Sprintf("%s-src-%02d", prefix, i), scope: scope, sensitivity: lvl,
			}))
		}
		got, err := store.ResolveSources(ctx, pool, set, noFloor, ids, scope)
		if err != nil {
			t.Fatalf("resolve %s: %v", prefix, err)
		}
		if got.Len() != len(levels) {
			t.Fatalf("resolve %s found %d of %d sources", prefix, got.Len(), len(levels))
		}
		return backends.Sensitivity(got.FlooredMax())
	}

	nineInternal := []string{"internal", "internal", "internal", "internal", "internal",
		"internal", "internal", "internal", "internal"}

	t.Run("gate1a_one_credentials_source_among_nine_internal_folds_up", func(t *testing.T) {
		const title = "w015-fold-up"
		folded := foldOf(t, "w015-1a", append(append([]string{}, nineInternal...), "credentials")...)
		if folded != backends.SensCredentials {
			t.Fatalf("fold over nine internal + one credentials = %q, want credentials — §4.8.1", folded)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: folded, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		sens, source := dsRow(t, pool, "learnings", title, scope)
		if sens != "credentials" || source != "derived" {
			t.Fatalf("row = %s/%s, want credentials/derived", sens, source)
		}
	})

	t.Run("gate1b_all_internal_sources_stay_internal_not_the_DDL_default", func(t *testing.T) {
		const title = "w015-fold-flat"
		folded := foldOf(t, "w015-1b", nineInternal...)
		if folded != backends.SensInternal {
			t.Fatalf("fold over nine internal sources = %q, want internal — this is the direction that is red without the fold", folded)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: folded, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		sens, source := dsRow(t, pool, "learnings", title, scope)
		if sens != "internal" || source != "derived" {
			t.Fatalf("row = %s/%s, want internal/derived", sens, source)
		}

		// The control that gives the assertion above its meaning: the very same
		// write WITHOUT the fold takes the DDL defaults, and 'credentials' then
		// looks like a correct classification while being an accident
		// (113_baseline.sql:5474/:5476).
		const unfolded = "w015-fold-flat-unfolded"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", unfolded, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{}, derived.TypeCatalog); err != nil {
			t.Fatalf("upsert control: %v", err)
		}
		if s, src := dsRow(t, pool, "learnings", unfolded, scope); s != "credentials" || src != "default" {
			t.Fatalf("control row = %s/%s, want credentials/default — if this is not the DDL default, gate1b proves nothing", s, src)
		}
	})

	t.Run("gate2_regeneration_without_the_credentials_source_lowers_nothing", func(t *testing.T) {
		const title = "w015-ratchet"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensCredentials, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("first run: %v", err)
		}
		// Second run, credentials source gone: the fold now says internal.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (Regeneration)", nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("regeneration: %v", err)
		}
		sens, source := dsRow(t, pool, "learnings", title, scope)
		if sens != "credentials" || source != "derived" {
			t.Fatalf("row = %s/%s, want credentials/derived — B6: the OLD content is still in the block, so the level must not fall",
				sens, source)
		}
	})

	t.Run("gate6_manual_survives_an_equal_fold", func(t *testing.T) {
		const title = "w015-manual-survives"
		// A human classified this derivative: sensitivity_source='manual'.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Manual: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("manual write: %v", err)
		}
		if s, src := dsRow(t, pool, "learnings", title, scope); s != "internal" || src != "manual" {
			t.Fatalf("precondition = %s/%s, want internal/manual", s, src)
		}
		// Regeneration folds to the SAME level. sensitivity and
		// sensitivity_source sit in one CASE expression (blocks.go), so '>='
		// would re-stamp 'derived' here without the value moving at all — a
		// human decision lost silently. With '>' both stay.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (Regeneration)", nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("regeneration: %v", err)
		}
		sens, source := dsRow(t, pool, "learnings", title, scope)
		if source != "manual" {
			t.Fatalf("sensitivity_source = %q, want manual — §4.8.2: '>=' makes this red", source)
		}
		if sens != "internal" {
			t.Fatalf("sensitivity = %q, want internal", sens)
		}

		// And the ratchet still works the other way: a REAL elevation takes the
		// block, manual or not. '>' is an upgrade rule, not a manual veto.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (Eskalation)", nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensCredentials, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("elevation: %v", err)
		}
		if s, src := dsRow(t, pool, "learnings", title, scope); s != "credentials" || src != "derived" {
			t.Fatalf("after elevation = %s/%s, want credentials/derived", s, src)
		}
	})

	t.Run("badge_the_server_path_may_rewrite_its_own_derivative", func(t *testing.T) {
		const title = "w015-badge-rewrite"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("first run: %v", err)
		}
		// The conflict path — this is the write every regeneration makes
		// (§4.7.2), and before the badge it died on the unconditional guard.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (v2)", nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("regeneration with the badge: %v — the arm cannot maintain its own block", err)
		}
		var content string
		if err := pool.QueryRow(ctx,
			`SELECT content FROM context_blocks WHERE category='learnings' AND title=$1 AND scope=$2 AND NOT is_archived`,
			title, scope).Scan(&content); err != nil {
			t.Fatalf("read content: %v", err)
		}
		if content != body+" (v2)" {
			t.Fatalf("content = %q, want the regenerated body", content)
		}

		// NEGATIVE, the same write without the badge: the guard is unchanged
		// for everyone else. This is the store-level twin of the 403 the seven
		// client surfaces answer (handler/derived_server_path_integration_test.go).
		_, err := store.UpsertBlock(ctx, pool, "learnings", title, "ATTACKER text wearing a derivative's identity",
			nil, nil, scope, true, store.SensitivityWrite{}, "")
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("unbadged write = %v, want ErrProvenanceProtected", err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT content FROM context_blocks WHERE category='learnings' AND title=$1 AND scope=$2 AND NOT is_archived`,
			title, scope).Scan(&content); err != nil {
			t.Fatalf("re-read content: %v", err)
		}
		if content != body+" (v2)" {
			t.Fatalf("content = %q — the refused write landed anyway", content)
		}
	})

	t.Run("badge_revival_after_archiving_is_a_fresh_upsert", func(t *testing.T) {
		const title = "w015-badge-revival"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("first run: %v", err)
		}
		// §4.7.5: a dead topic archives its catalogue block. Reviving it with
		// is_archived=false would collide on the partial unique index (B11), so
		// revival is a NEW upsert — which lands on the INSERT-path guard that
		// looks at archived rows (W01-2a review finding #5).
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE category='learnings' AND title=$1 AND scope=$2`,
			title, scope); err != nil {
			t.Fatalf("archive: %v", err)
		}

		// NEGATIVE first: without the badge the archived derivative still holds
		// its identity against everyone.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "ATTACKER text on a freed title",
			nil, nil, scope, true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("unbadged revival = %v, want ErrProvenanceProtected", err)
		}
		// With the badge the arm revives its own block.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (wiederbelebt)", nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("badged revival: %v — an archived derivative would be a permanent tombstone on its own title", err)
		}
		var active, archived int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE NOT is_archived), count(*) FILTER (WHERE is_archived)
			   FROM context_blocks WHERE category='learnings' AND title=$1 AND scope=$2`,
			title, scope).Scan(&active, &archived); err != nil {
			t.Fatalf("count: %v", err)
		}
		if active != 1 || archived != 1 {
			t.Fatalf("after revival: %d active / %d archived, want 1/1 (§4.7.5 — they coexist by design)", active, archived)
		}
	})

	t.Run("badge_does_not_reach_a_block_without_provenance", func(t *testing.T) {
		// The badge opens the PROVENANCE guard, nothing else. An ordinary block
		// was never protected by it, so this write behaves exactly as it did
		// before the wave — asserted rather than assumed, because "the badge
		// changes nothing else" is the claim a reviewer has to be able to check.
		const title = "w015-badge-plain"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, nil,
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Manual: true}, ""); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (v2)", nil, nil,
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true}, ""); err != nil {
			t.Fatalf("derived write over a plain block: %v", err)
		}
		if s, src := dsRow(t, pool, "learnings", title, scope); s != "internal" || src != "manual" {
			t.Fatalf("row = %s/%s, want internal/manual — the strict '>' holds here too", s, src)
		}
	})

	// --- the two narrowing clauses the review asked for ---------------------

	t.Run("badge_without_own_provenance_cannot_strip_the_guard", func(t *testing.T) {
		// Review finding #1. The ON CONFLICT clause replaces metadata WHOLESALE
		// (`metadata = EXCLUDED.metadata - 'guard_checked_at'`), so a badged write
		// that carries no provenance of its own used to DELETE the target's — and
		// from that moment the title was client-writable again, because S3 hangs
		// on `metadata ? 'provenance'`. One aborted arm run was enough to reopen
		// B14; no attacker needed.
		const title = "w015-badge-strip"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("seed: %v", err)
		}
		for _, tc := range []struct {
			name string
			md   map[string]any
		}{
			{"no metadata at all", nil},
			{"metadata without the provenance key", map[string]any{"note": "abgebrochener Lauf"}},
			{"provenance of an unknown contract version", map[string]any{
				derived.MetadataKey: map[string]any{"v": derived.ContractVersion + 1, "stratum": 1, "arm": "w015-arm"},
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (entkernt)", nil, tc.md,
					scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
					derived.TypeCatalog)
				if !errors.Is(err, store.ErrProvenanceProtected) {
					t.Fatalf("badged write without own provenance = %v, want ErrProvenanceProtected", err)
				}
			})
		}
		// The case where "the write must name a v=1 provenance" is the ONLY
		// clause that holds: a target whose provenance object is empty (an
		// aborted run wrote `{}`) has the same empty identity a metadata-less
		// write would present, so the identity comparison cannot separate them.
		t.Run("an empty provenance object on both sides", func(t *testing.T) {
			const hollow = "w015-badge-hollow"
			hollowMD := map[string]any{derived.MetadataKey: map[string]any{}}
			if _, err := store.UpsertBlock(ctx, pool, "learnings", hollow, body, nil, hollowMD,
				scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
				derived.TypeCatalog); err != nil {
				t.Fatalf("seed hollow: %v", err)
			}
			if !dsHasProvenance(t, pool, hollow, scope) {
				t.Fatal("precondition: the hollow row carries the provenance key")
			}
			if _, err := store.UpsertBlock(ctx, pool, "learnings", hollow, body+" (entkernt)", nil, nil,
				scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
				derived.TypeCatalog); !errors.Is(err, store.ErrProvenanceProtected) {
				t.Fatalf("badged write against a hollow provenance = %v, want ErrProvenanceProtected", err)
			}
			if !dsHasProvenance(t, pool, hollow, scope) {
				t.Fatal("the hollow provenance was stripped — the title is client-writable again")
			}
		})

		// The provenance survived, so the guard still holds the title against
		// every client form — the second half of the reviewer's chain.
		if !dsHasProvenance(t, pool, title, scope) {
			t.Fatal("the provenance is gone — S3 no longer holds this title")
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "ATTACKER text", nil, nil,
			scope, true, store.SensitivityWrite{}, ""); !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("unbadged write = %v, want ErrProvenanceProtected", err)
		}
	})

	t.Run("badge_without_own_provenance_cannot_claim_an_archived_identity", func(t *testing.T) {
		const title = "w015-badge-strip-archived"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE category='learnings' AND title=$1 AND scope=$2`,
			title, scope); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (ohne Provenienz)", nil, nil,
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("badged revival without own provenance = %v, want ErrProvenanceProtected", err)
		}
	})

	t.Run("badge_does_not_open_a_foreign_derivative", func(t *testing.T) {
		// Review finding #2: docs/api.md promised "a derivative it owns" and the
		// code had no notion of ownership at all. D-02 and D-03 are two arms; on
		// a shared identity the old semantics were "whoever writes last wins".
		const title = "w015-badge-foreign"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil,
			dsProvenanceOf("arm-a", derived.StratumDerived),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("seed arm-a: %v", err)
		}
		for _, tc := range []struct {
			name string
			md   map[string]any
		}{
			{"another arm", dsProvenanceOf("arm-b", derived.StratumDerived)},
			{"another level", dsProvenanceOf("arm-a", derived.StratumSuper)},
		} {
			t.Run(tc.name, func(t *testing.T) {
				_, err := store.UpsertBlock(ctx, pool, "learnings", title, "arm-b hat arm-a überschrieben", nil, tc.md,
					scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
					derived.TypeInsight)
				if !errors.Is(err, store.ErrProvenanceProtected) {
					t.Fatalf("foreign badged write = %v, want ErrProvenanceProtected", err)
				}
			})
		}
		// An UNNAMED provenance is not an identity. Without this the pair
		// ("", stratum) would compare equal between any two unnamed derivatives,
		// and the binding would hold for every arm except the ones that forgot
		// to name themselves — the worst possible split.
		t.Run("a provenance without an arm is not an identity", func(t *testing.T) {
			const unnamed = "w015-badge-unnamed"
			nameless := map[string]any{
				derived.MetadataKey: map[string]any{
					"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
				},
			}
			if _, err := store.UpsertBlock(ctx, pool, "learnings", unnamed, body, nil, nameless,
				scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
				derived.TypeCatalog); err != nil {
				t.Fatalf("seed unnamed: %v", err)
			}
			if _, err := store.UpsertBlock(ctx, pool, "learnings", unnamed, "ein zweiter namenloser Schreiber", nil, nameless,
				scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
				derived.TypeCatalog); !errors.Is(err, store.ErrProvenanceProtected) {
				t.Fatalf("unnamed badged rewrite = %v, want ErrProvenanceProtected", err)
			}
		})

		// Control: the OWNER still regenerates. The clause binds identity, it
		// does not close the path.
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (v2)", nil,
			dsProvenanceOf("arm-a", derived.StratumDerived),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("owner regeneration: %v", err)
		}
		var content string
		if err := pool.QueryRow(ctx,
			`SELECT content FROM context_blocks WHERE category='learnings' AND title=$1 AND scope=$2 AND NOT is_archived`,
			title, scope).Scan(&content); err != nil {
			t.Fatalf("read content: %v", err)
		}
		if content != body+" (v2)" {
			t.Fatalf("content = %q, want the owner's regenerated body", content)
		}
	})

	t.Run("badge_does_not_revive_a_foreign_archived_identity", func(t *testing.T) {
		const title = "w015-badge-foreign-archived"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body, nil,
			dsProvenanceOf("arm-a", derived.StratumDerived),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("seed arm-a: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE context_blocks SET is_archived = true WHERE category='learnings' AND title=$1 AND scope=$2`,
			title, scope); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, "arm-b belegt den Titel", nil,
			dsProvenanceOf("arm-b", derived.StratumDerived),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeInsight); !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("foreign badged revival = %v, want ErrProvenanceProtected", err)
		}
		// The owner revives its own archived block (§4.7.5 / B11).
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, body+" (wiederbelebt)", nil,
			dsProvenanceOf("arm-a", derived.StratumDerived),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("owner revival: %v", err)
		}
	})

	t.Run("badge_yields_to_the_G40_detector_for_the_source_value", func(t *testing.T) {
		// A derivative whose verbatim quote carries a key: the scanner fires on
		// EVERY upsert path (V-W8), and its verdict is the sharper statement —
		// it is about THIS block's content, the fold is about its sources. The
		// badge is unaffected: who writes does not change.
		const title = "w015-badge-detector"
		leaky := body + "\naws_access_key_id = AKIA" + "QQQQQQQQQQQQQQQQ"
		if _, err := store.UpsertBlock(ctx, pool, "learnings", title, leaky, nil, dsProvenanceMD(),
			scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true},
			derived.TypeCatalog); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		sens, source := dsRow(t, pool, "learnings", title, scope)
		if sens != "credentials" || source != "pattern" {
			t.Fatalf("row = %s/%s, want credentials/pattern — the detector outranks the fold for the source value", sens, source)
		}
		var md map[string]any
		if err := pool.QueryRow(ctx,
			`SELECT metadata FROM context_blocks WHERE category='learnings' AND title=$1 AND scope=$2 AND NOT is_archived`,
			title, scope).Scan(&md); err != nil {
			t.Fatalf("read metadata: %v", err)
		}
		if _, ok := md[derived.MetadataKey]; !ok {
			raw, _ := json.Marshal(md)
			t.Fatalf("provenance key gone from %s — the derivative would stop being one", raw)
		}
	})
}
