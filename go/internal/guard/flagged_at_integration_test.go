//go:build integration

// FlaggedSQL (issue #238) reads metadata.guard_checked_at once per row and
// casts it once. This file pins that the rewrite answers what the expression
// it replaced answered, row by row, for every form the stamp takes in a
// database: the guard's own stamp, and whatever a caller wrote into the key
// before the write path kept it to the guard.
//
//	go test -tags=integration -p 1 ./internal/guard/ -run TestFlaggedSQL -count=1 -v
package guard

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// baseFlaggedAtSQL is FlaggedAtSQL as of 749ee2a0, the base of issue #238,
// with its isfinite test: the reference the rewrite is compared with.
const baseFlaggedAtSQL = `CASE WHEN pg_input_is_valid(metadata->>'guard_checked_at', 'timestamptz')
		THEN CASE WHEN isfinite((metadata->>'guard_checked_at')::timestamptz)
				AND (metadata->>'guard_checked_at')::timestamptz >= date_trunc('second', created_at)
				AND (metadata->>'guard_checked_at')::timestamptz <= now()
			THEN (metadata->>'guard_checked_at')::timestamptz
			ELSE updated_at END
		ELSE updated_at END`

// TestFlaggedSQLMatchesTheBaseRowByRow writes flagged blocks through the
// production path (store.UpsertBlock, store.StoreEmbedding, RunGuardBatch),
// then gives each one stamp form by hand — the forms a caller could write
// before the write path kept the key to the guard; no write path produces
// them any more. Half of the rows are created two years back (by hand as
// well), so the relative literals land on both sides of the lower bound.
//
// The rewrite and the base expression are evaluated in ONE statement, so
// 'now', 'today' and the reader's now() mean the same instant on both sides.
func TestFlaggedSQLMatchesTheBaseRowByRow(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	const scope = "fa-diff"

	// The first half of the rows (backdated below) gets the forms that can
	// lie after the row's creation; the second half the ones that cannot be
	// a flag time at all. nil = the key is removed; every other entry is the
	// JSON value written.
	forms := []*string{
		ptr(`"now"`), ptr(`"today"`), ptr(`"yesterday"`), ptr(`"tomorrow"`), ptr(`"epoch"`),
		ptr(`"1900-01-01T00:00:00Z"`),
		ptr(`"2026-01-15T10:00:00+05:00"`), ptr(`"2026-01-15 10:00:00.987654-03"`), ptr(`"2026-01-15 10:00:00"`),
		ptr(`"2026-01-15T10:00:00 Europe/Berlin"`), ptr(`"Jan 15 2026 10:00 PST"`), ptr(`"J2461056"`),
		ptr(`20260115`), ptr(`1768471200`), ptr(`"2025-06-30T23:59:60Z"`),
		nil,
		ptr(`""`), ptr(`"not a timestamp"`), ptr(`"2999-01-01T00:00:00Z"`),
		ptr(`"infinity"`), ptr(`"-infinity"`), ptr(`"+infinity"`), ptr(`"allballs"`),
		ptr(`"0001-01-01 00:00:00 BC"`), ptr(`"294276-12-31 23:59:59+00"`),
		ptr(`true`), ptr(`null`),
		ptr(`{"at": "2026-01-15T10:00:00Z"}`), ptr(`["2026-01-15T10:00:00Z"]`),
	}
	n := len(forms) + 3 // three rows keep the guard's own stamp
	ids := make([]string, 0, n)
	for i := 0; i < (n+1)/2; i++ {
		for j, vec := range [][]float32{diffVec(2*i, 1.0, 0, 0), diffVec(2*i, 0.96, 2*i+1, 0.28)} {
			title := "fa-diff-" + string(rune('A'+i%26)) + string(rune('a'+i/26)) + string(rune('0'+j))
			b, err := store.UpsertBlock(ctx, pool, "learnings", title, "flag time body of "+title, nil, nil,
				scope, true, store.SensitivityWrite{}, "knowledge")
			if err != nil {
				t.Fatalf("upsert %s: %v", title, err)
			}
			if err := store.StoreEmbedding(ctx, pool, b.ID, "flagged-at-model", vec); err != nil {
				t.Fatalf("embed %s: %v", title, err)
			}
			ids = append(ids, b.ID)
		}
	}
	processed, err := RunGuardBatch(ctx, pool, blocktype.NewRegistry().Snapshot(), len(ids)+10, Search{EfSearch: 800, ExactMaxCandidates: 400})
	if err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}
	if processed != len(ids) {
		t.Fatalf("processed %d, want %d", processed, len(ids))
	}
	if _, err := pool.Exec(ctx,
		`UPDATE context_blocks SET created_at = now() - interval '2 years', updated_at = now() - interval '1 year'
		  WHERE id = ANY($1::uuid[])`, ids[:len(ids)/2]); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	for i, form := range forms {
		id := ids[i]
		var err error
		if form == nil {
			_, err = pool.Exec(ctx, `UPDATE context_blocks SET metadata = metadata - 'guard_checked_at' WHERE id = $1`, id)
		} else {
			_, err = pool.Exec(ctx,
				`UPDATE context_blocks SET metadata = jsonb_set(metadata, '{guard_checked_at}', $2::jsonb) WHERE id = $1`, id, *form)
		}
		if err != nil {
			t.Fatalf("stamp form %v: %v", form, err)
		}
	}

	rows, err := pool.Query(ctx, `SELECT f.id::text, f.flagged_at,
			(SELECT `+baseFlaggedAtSQL+` FROM context_blocks WHERE context_blocks.id = f.id),
			(SELECT metadata->'guard_checked_at' FROM context_blocks WHERE context_blocks.id = f.id),
			(SELECT updated_at FROM context_blocks WHERE context_blocks.id = f.id)
		   FROM `+FlaggedSQL(" AND scope = $1")+` AS f`, scope)
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	defer rows.Close()
	seen := 0
	tookStamp := 0
	for rows.Next() {
		var id string
		var head, base, updated time.Time
		var stamp *string
		if err := rows.Scan(&id, &head, &base, &stamp, &updated); err != nil {
			t.Fatalf("scan: %v", err)
		}
		seen++
		if !head.Equal(base) {
			s := "<absent>"
			if stamp != nil {
				s = *stamp
			}
			t.Errorf("block %s, stamp %s: FlaggedSQL answers %s, the base answered %s",
				id, s, head.Format(time.RFC3339Nano), base.Format(time.RFC3339Nano))
		}
		if !head.Equal(updated) {
			tookStamp++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("compare rows: %v", err)
	}
	if seen != len(ids) {
		t.Fatalf("FlaggedSQL returned %d rows, want every flagged block (%d)", seen, len(ids))
	}
	// The bestand must exercise both answers, or it compares nothing.
	if tookStamp == 0 || tookStamp == seen {
		t.Fatalf("%d of %d rows took the stamp; the fixture does not exercise both answers", tookStamp, seen)
	}
	t.Logf("%d rows compared, %d took the stamp", seen, tookStamp)

	// ±infinity, explicitly: both fall back to updated_at without isfinite.
	for _, form := range []string{`"infinity"`, `"-infinity"`, `"+infinity"`} {
		var head, updated time.Time
		if err := pool.QueryRow(ctx, `SELECT f.flagged_at, b.updated_at
			   FROM `+FlaggedSQL(" AND scope = $1")+` AS f JOIN context_blocks b ON b.id = f.id
			  WHERE b.metadata->'guard_checked_at' = $2::jsonb`, scope, form).Scan(&head, &updated); err != nil {
			t.Fatalf("read %s: %v", form, err)
		}
		if !head.Equal(updated) {
			t.Errorf("stamp %s: flagged_at %s, want updated_at %s", form, head, updated)
		}
	}
}

func ptr(s string) *string { return &s }

func diffVec(aIdx int, aW float32, bIdx int, bW float32) []float32 {
	v := make([]float32, 1024)
	v[aIdx] = aW
	if bW != 0 {
		v[bIdx] = bW
	}
	return v
}
