//go:build integration

// Gap-C0-a (RC-1 W-B1): UpsertBlob's INSERT bound $6 in two different type
// contexts — the bytea `data` column and digest($6, 'sha256') — which
// PostgreSQL refuses to PREPARE (42P08, "could not determine data type of
// parameter $6"). pgxpool's default exec mode prepares every statement, so
// the Go backend has never successfully written a single blob.
//
// The probes below pin, in order:
//   - the row is actually written (the 42P08 regression itself),
//   - the ON CONFLICT branch updates the SAME row and carries the NEW
//     checksum (a fix that casts only the INSERT position and drops
//     EXCLUDED.checksum passes the first probe and fails this one),
//   - checksum == hex(sha256(data)) recomputed in Go (pins pgcrypto
//     semantics: digest over the raw bytes, not over a text rendering),
//   - a size sweep across the bytea toast boundaries.
//
//	go test -tags=integration ./internal/store/ -run TestUpsertBlob -count=1 -v
package store_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// blobBytes builds a deterministic, non-repeating payload of n bytes so a
// truncated or re-encoded write cannot accidentally match the original.
func blobBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// goSHA256Hex is the reference implementation the pgcrypto checksum must match.
func goSHA256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// blobRowData reads the stored payload back out of the table, bypassing
// UpsertBlob's own RETURNING clause.
func blobRowData(t *testing.T, pool *pgxpool.Pool, id string) []byte {
	t.Helper()
	var data []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT data FROM context_blobs WHERE id = $1`, id).Scan(&data); err != nil {
		t.Fatalf("read back blob %s: %v", id, err)
	}
	return data
}

func TestUpsertBlob_42P08_Regression(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	t.Run("insert writes the row under the prepared-statement exec mode", func(t *testing.T) {
		data := blobBytes(32)
		bm, err := store.UpsertBlob(ctx, pool, "reference", "b1-insert", "probe.bin",
			"application/octet-stream", "private", data, []string{"w-b1"}, map[string]any{"probe": "insert"}, "")
		if err != nil {
			t.Fatalf("UpsertBlob: %v", err)
		}
		if bm.ID == "" {
			t.Fatal("UpsertBlob returned an empty id")
		}
		if bm.FileSize != int64(len(data)) {
			t.Errorf("file_size = %d, want %d", bm.FileSize, len(data))
		}
		if bm.StorageType != "db" {
			t.Errorf("storage_type = %q, want %q", bm.StorageType, "db")
		}
		if bm.Scope != "private" {
			t.Errorf("scope = %q, want %q", bm.Scope, "private")
		}
		if got := blobRowData(t, pool, bm.ID); !bytes.Equal(got, data) {
			t.Errorf("stored data = %x, want %x", got, data)
		}
	})

	t.Run("second write updates the same row including the checksum", func(t *testing.T) {
		first := blobBytes(32)
		bm1, err := store.UpsertBlob(ctx, pool, "reference", "b1-upsert", "first.bin",
			"application/octet-stream", "private", first, []string{"a"}, map[string]any{"gen": "1"}, "")
		if err != nil {
			t.Fatalf("first UpsertBlob: %v", err)
		}

		second := blobBytes(64)
		bm2, err := store.UpsertBlob(ctx, pool, "reference", "b1-upsert", "second.bin",
			"text/plain", "private", second, []string{"b"}, map[string]any{"gen": "2"}, "")
		if err != nil {
			t.Fatalf("second UpsertBlob: %v", err)
		}

		if bm2.ID != bm1.ID {
			t.Errorf("upsert created a new row: id %s -> %s", bm1.ID, bm2.ID)
		}
		if bm2.FileSize != int64(len(second)) {
			t.Errorf("file_size = %d, want %d", bm2.FileSize, len(second))
		}
		if bm2.Filename != "second.bin" || bm2.MimeType != "text/plain" {
			t.Errorf("filename/mime = %q/%q, want second.bin/text/plain", bm2.Filename, bm2.MimeType)
		}
		if bm2.Checksum == bm1.Checksum {
			t.Errorf("checksum unchanged after data change: %q (EXCLUDED.checksum not carried)", bm2.Checksum)
		}
		if want := goSHA256Hex(second); bm2.Checksum != want {
			t.Errorf("checksum after update = %q, want %q", bm2.Checksum, want)
		}
		if got := blobRowData(t, pool, bm2.ID); !bytes.Equal(got, second) {
			t.Errorf("stored data after update = %x, want %x", got, second)
		}

		var rows int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE category = 'reference' AND title = 'b1-upsert' AND scope = 'private'`,
		).Scan(&rows); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if rows != 1 {
			t.Errorf("row count = %d, want 1", rows)
		}
	})

	t.Run("checksum equals hex sha256 of the raw bytes", func(t *testing.T) {
		// A payload whose bytes are NOT valid UTF-8: a digest computed over a
		// text rendering of the parameter would differ here.
		data := []byte{0x00, 0xff, 0xfe, 0x80, 0x41, 0x0a, 0x00, 0xc3}
		bm, err := store.UpsertBlob(ctx, pool, "reference", "b1-checksum", "raw.bin",
			"application/octet-stream", "private", data, nil, nil, "")
		if err != nil {
			t.Fatalf("UpsertBlob: %v", err)
		}
		if want := goSHA256Hex(data); bm.Checksum != want {
			t.Errorf("checksum = %q, want %q", bm.Checksum, want)
		}
	})

	t.Run("size sweep across the toast boundaries", func(t *testing.T) {
		for _, size := range []int{30, 1 << 20, 15 << 20} {
			t.Run(fmt.Sprintf("%dB", size), func(t *testing.T) {
				data := blobBytes(size)
				bm, err := store.UpsertBlob(ctx, pool, "reference", fmt.Sprintf("b1-size-%d", size),
					"sweep.bin", "application/octet-stream", "private", data, nil, nil, "")
				if err != nil {
					t.Fatalf("UpsertBlob(%d bytes): %v", size, err)
				}
				if bm.FileSize != int64(size) {
					t.Errorf("file_size = %d, want %d", bm.FileSize, size)
				}
				if want := goSHA256Hex(data); bm.Checksum != want {
					t.Errorf("checksum = %q, want %q", bm.Checksum, want)
				}
				if got := blobRowData(t, pool, bm.ID); !bytes.Equal(got, data) {
					t.Errorf("stored data differs from input (%d bytes)", size)
				}
			})
		}
	})
}
