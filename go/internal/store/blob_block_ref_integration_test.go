//go:build integration

// W02-10 gates 1-3 at the store layer: the blob-to-block edge is WRITTEN.
//
// Ist-Stand before this wave: context_blobs.context_block_id existed
// (113_baseline.sql:131) and was indexed (:261), and no Go code path wrote it —
// UpsertBlob named the column in neither its INSERT list nor its RETURNING, so
// every blob the Go backend ever stored carried NULL there and the two-phase
// write of design/02 sec. 4.2 had no second phase to run.
//
// What the probes pin, in order:
//   - UpsertBlob writes the column, returns it, and CLEARS it on a re-upsert
//     without one (the edge is what the last write said, gate 1),
//   - UpdateBlobBlockRef sets it WITHOUT rewriting the payload — file_size,
//     checksum and updated_at are identical before and after, which is the
//     whole reason phase 2 is a link and not a second store (gate 2),
//   - the schema's ON DELETE SET NULL is what we rely on, not something we
//     reinterpret: deleting the block leaves the blob alive with a NULL edge
//     (gate 3),
//   - BlockVisible / BlobWriteScope answer the no-oracle way: absent, foreign
//     and malformed are ONE answer, an empty scope set is an error.
//
//	go test -tags=integration ./internal/store/ -run TestBlobBlockRef -count=1 -v
package store_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// w210Block seeds one block in the given scope and returns its id.
func w210Block(t *testing.T, pool *pgxpool.Pool, title, scope string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "reference", title, "content of "+title,
		nil, nil, scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed block %q in %q: %v", title, scope, err)
	}
	return b.ID
}

// w210Edge reads context_block_id straight out of the table, bypassing every
// RETURNING clause: the claim is about the COLUMN, not about what a Go function
// says it wrote.
func w210Edge(t *testing.T, pool *pgxpool.Pool, blobID string) (string, bool) {
	t.Helper()
	var ref *string
	if err := pool.QueryRow(context.Background(),
		`SELECT context_block_id FROM context_blobs WHERE id = $1`, blobID).Scan(&ref); err != nil {
		t.Fatalf("read edge of blob %s: %v", blobID, err)
	}
	if ref == nil {
		return "", false
	}
	return *ref, true
}

func TestBlobBlockRef(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("a_UpsertWritesAndClearsTheEdge", func(t *testing.T) {
		blockID := w210Block(t, pool, "w210-manifest-a", "private")

		bm, err := store.UpsertBlob(ctx, pool, "reference", "w210-upsert", "payload.ndjson",
			"application/x-ndjson", "private", []byte("line one\n"), nil, nil, blockID)
		if err != nil {
			t.Fatalf("UpsertBlob with a block ref: %v", err)
		}
		if bm.ContextBlockID == nil || *bm.ContextBlockID != blockID {
			t.Fatalf("UpsertBlob returned context_block_id %v, want %s — the RETURNING clause must carry the column it writes", bm.ContextBlockID, blockID)
		}
		if got, ok := w210Edge(t, pool, bm.ID); !ok || got != blockID {
			t.Fatalf("stored context_block_id = %q (set %v), want %s", got, ok, blockID)
		}

		// A re-upsert WITHOUT a block id clears the edge: the row is what the
		// last write handed over (design/02 sec. 4.2 — phase 1 always precedes
		// phase 2, so a payload rewrite starts the pair over).
		if _, err := store.UpsertBlob(ctx, pool, "reference", "w210-upsert", "payload.ndjson",
			"application/x-ndjson", "private", []byte("line two\n"), nil, nil, ""); err != nil {
			t.Fatalf("re-upsert without a block ref: %v", err)
		}
		if got, ok := w210Edge(t, pool, bm.ID); ok {
			t.Errorf("re-upsert without a block ref left the edge at %q, want NULL", got)
		}
	})

	t.Run("b_LinkDoesNotRewriteThePayload", func(t *testing.T) {
		blockID := w210Block(t, pool, "w210-manifest-b", "private")
		before, err := store.UpsertBlob(ctx, pool, "reference", "w210-link", "payload.ndjson",
			"application/x-ndjson", "private", []byte("payload bytes"), nil, nil, "")
		if err != nil {
			t.Fatalf("seed blob: %v", err)
		}
		if before.ContextBlockID != nil {
			t.Fatalf("seeded blob already carries an edge (%v)", *before.ContextBlockID)
		}

		after, err := store.UpdateBlobBlockRef(ctx, pool, before.ID, blockID, []string{"private"})
		if err != nil {
			t.Fatalf("UpdateBlobBlockRef: %v", err)
		}
		if after == nil {
			t.Fatal("UpdateBlobBlockRef answered not-found for a blob in a writable scope")
		}
		if after.ContextBlockID == nil || *after.ContextBlockID != blockID {
			t.Fatalf("linked blob carries context_block_id %v, want %s", after.ContextBlockID, blockID)
		}

		// Phase 2 is a LINK, not a second write of the payload. Anything that
		// re-upserted would move file_size/checksum (new bytes) or updated_at
		// (now()) — there is no trigger on context_blobs, so an unchanged
		// updated_at proves the statement did not touch it.
		if after.FileSize != before.FileSize {
			t.Errorf("file_size moved %d -> %d across a link", before.FileSize, after.FileSize)
		}
		if after.Checksum != before.Checksum {
			t.Errorf("checksum moved %q -> %q across a link", before.Checksum, after.Checksum)
		}
		if !after.UpdatedAt.Equal(before.UpdatedAt) {
			t.Errorf("updated_at moved %s -> %s across a link — phase 2 must not read as a payload rewrite",
				before.UpdatedAt, after.UpdatedAt)
		}
		if !after.CreatedAt.Equal(before.CreatedAt) {
			t.Errorf("created_at moved %s -> %s across a link", before.CreatedAt, after.CreatedAt)
		}
	})

	t.Run("c_LinkIsScopedAndNeverAnOracle", func(t *testing.T) {
		blockID := w210Block(t, pool, "w210-manifest-c", "private")
		foreign, err := store.UpsertBlob(ctx, pool, "reference", "w210-foreign", "f.bin",
			"application/octet-stream", "w210x", []byte("foreign"), nil, nil, "")
		if err != nil {
			t.Fatalf("seed foreign blob: %v", err)
		}

		// A blob outside the write scopes, a blob that does not exist and a
		// malformed id are ONE answer: (nil, nil).
		for name, id := range map[string]string{
			"foreign scope": foreign.ID,
			"absent":        "00000000-0000-0000-0000-0000000000ff",
			"malformed":     "not-a-uuid",
		} {
			got, err := store.UpdateBlobBlockRef(ctx, pool, id, blockID, []string{"private"})
			if err != nil {
				t.Errorf("UpdateBlobBlockRef(%s): error %v, want the not-found answer", name, err)
			}
			if got != nil {
				t.Errorf("UpdateBlobBlockRef(%s) answered a row (%s) — that is an oracle", name, got.ID)
			}
		}
		if got, ok := w210Edge(t, pool, foreign.ID); ok {
			t.Errorf("the foreign-scope blob was linked anyway (edge %q)", got)
		}

		// The same three collapse in the stage-time twin.
		for name, id := range map[string]string{
			"foreign scope": foreign.ID,
			"absent":        "00000000-0000-0000-0000-0000000000ff",
			"malformed":     "not-a-uuid",
		} {
			scope, err := store.BlobWriteScope(ctx, pool, id, []string{"private"})
			if err != nil {
				t.Errorf("BlobWriteScope(%s): error %v, want the empty answer", name, err)
			}
			if scope != "" {
				t.Errorf("BlobWriteScope(%s) = %q, want the empty string", name, scope)
			}
		}
		if scope, err := store.BlobWriteScope(ctx, pool, foreign.ID, []string{"w210x"}); err != nil || scope != "w210x" {
			t.Errorf("BlobWriteScope of a writable blob = %q (%v), want w210x", scope, err)
		}
		if _, err := store.BlobWriteScope(ctx, pool, foreign.ID, nil); err == nil {
			t.Error("BlobWriteScope with an empty scope set returned no error — the read must fail closed")
		}
	})

	t.Run("d_DeletingTheBlockNullsTheEdge", func(t *testing.T) {
		// The FK is ON DELETE SET NULL (113_baseline.sql:131). This wave relies
		// on that semantics; it does not reinterpret it. A blob is evidence and
		// outlives the block that pointed at it — losing the payload with the
		// manifest would be exactly the data loss the externalization exists to
		// avoid.
		blockID := w210Block(t, pool, "w210-manifest-d", "private")
		bm, err := store.UpsertBlob(ctx, pool, "reference", "w210-orphan", "o.ndjson",
			"application/x-ndjson", "private", []byte("evidence"), nil, nil, blockID)
		if err != nil {
			t.Fatalf("seed linked blob: %v", err)
		}
		if _, ok := w210Edge(t, pool, bm.ID); !ok {
			t.Fatal("seeded blob carries no edge")
		}

		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1`, blockID); err != nil {
			t.Fatalf("delete block: %v", err)
		}

		var alive int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE id = $1`, bm.ID).Scan(&alive); err != nil {
			t.Fatalf("count blob after block delete: %v", err)
		}
		if alive != 1 {
			t.Fatalf("the blob did not survive its block (%d rows) — ON DELETE SET NULL, not CASCADE", alive)
		}
		if got, ok := w210Edge(t, pool, bm.ID); ok {
			t.Errorf("edge after block delete = %q, want NULL", got)
		}
	})

	t.Run("e_BlockVisibleIsTheGateAndFailsClosed", func(t *testing.T) {
		mine := w210Block(t, pool, "w210-visible", "private")
		foreign := w210Block(t, pool, "w210-invisible", "w210y")
		archived := w210Block(t, pool, "w210-archived", "private")
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1`, archived); err != nil {
			t.Fatalf("archive block: %v", err)
		}

		if ok, err := store.BlockVisible(ctx, pool, mine, []string{"private"}); err != nil || !ok {
			t.Errorf("BlockVisible(own scope) = %v (%v), want true", ok, err)
		}
		// Foreign, absent, malformed and archived all answer false — the write
		// surfaces render ONE verdict over all four (W02-10 A1).
		for name, id := range map[string]string{
			"foreign scope": foreign,
			"absent":        "00000000-0000-0000-0000-0000000000ff",
			"malformed":     "not-a-uuid",
			"archived":      archived,
		} {
			ok, err := store.BlockVisible(ctx, pool, id, []string{"private"})
			if err != nil {
				t.Errorf("BlockVisible(%s): error %v, want (false, nil)", name, err)
			}
			if ok {
				t.Errorf("BlockVisible(%s) = true, want false", name)
			}
		}
		if _, err := store.BlockVisible(ctx, pool, mine, nil); err == nil {
			t.Error("BlockVisible with an empty scope set returned no error — the gate must fail closed")
		}
	})
}
