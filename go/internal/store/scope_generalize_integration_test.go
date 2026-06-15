//go:build integration

// Integration test for Multi-Tenant wave T01 (migration 058 "scope_generalize").
//
// 058 generalizes the legacy 3-value scope discriminator so that arbitrary,
// tenant-prefixed scope names become storable on the four tables that today
// still pin scope to VARCHAR(20) and (for blobs/sources) to a
// CHECK scope IN ('private','work','shared'):
//
//	context_blobs       — DROP chk_blob_scope   + scope VARCHAR(20)→(50)
//	context_sources     — DROP chk_source_scope + scope VARCHAR(20)→(50)
//	context_dream_links — scope VARCHAR(20)→(50)
//	context_write_log   — scope VARCHAR(20)→(50)
//
// (context_blocks.scope is already VARCHAR(50) via 047 and chk_scope was dropped
// in 045; context_api_keys.home_scope is VARCHAR(50) via 052 — none are touched
// by 058 and none are exercised here.)
//
// RED (today, WITHOUT 058): every sub-case fails with a DISTINCT Postgres error
// that proves exactly which guard discriminates the tenant scope —
//   - blobs/sources  → check constraint "chk_blob_scope"/"chk_source_scope"
//     (SQLSTATE 23514) even though the value is short, isolating the CHECK drop.
//   - dream_links/write_log → "value too long for type character varying(20)"
//     (SQLSTATE 22001) using a 23-char scope, isolating the typmod widening.
// GREEN (after 058): all four INSERTs succeed.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestScopeGeneralize -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/testdb"
)

// longScope is 23 characters — deliberately >20 so the bare VARCHAR(20) columns
// (dream_links, write_log) reject it with 22001 today, while staying ≤50 so the
// post-058 VARCHAR(50) accepts it. It is also tenant-prefixed, the exact shape
// 047/052 cite as the multi-tenant target ('tenant:crag-research-q1').
const longScope = "tenant:crag-research-q1"

// shortTenantScope is only 8 chars: it passes any length bound but is NOT in
// {private,work,shared}, so it isolates the chk_blob_scope/chk_source_scope
// drop from the typmod widening — a length failure here would be a false signal.
const shortTenantScope = "tenant:x"

// seedBlock inserts one minimal context_blocks row and returns its id. Two
// distinct blocks are needed for the dream_links FK + the
// source_block_id != target_block_id CHECK.
func seedBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ('learnings', $1, 'content of ' || $1, 'private')
		 RETURNING id`, title).Scan(&id); err != nil {
		t.Fatalf("seed block %s: %v", title, err)
	}
	return id
}

func TestScopeGeneralize_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	// context_blobs: storage_type='db' branch of the storage CHECK requires
	// data NOT NULL + file_path NULL. shortTenantScope keeps length out of the
	// picture so only chk_blob_scope can reject this row today.
	t.Run("blobs_tenant_scope", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_blobs
				(category, title, filename, mime_type, file_size, storage_type, data, scope)
			 VALUES
				('t01-blob', 't01-blob-title', 'f.bin', 'application/octet-stream',
				 3, 'db', '\x010203'::bytea, $1)`,
			shortTenantScope,
		)
		if err != nil {
			t.Fatalf("INSERT context_blobs scope=%q failed (expected RED without 058, GREEN with): %v",
				shortTenantScope, err)
		}
	})

	// context_sources: file_path + file_hash are the only NOT NULL/no-default
	// columns besides scope. UNIQUE(file_path, scope) is satisfied trivially.
	t.Run("sources_tenant_scope", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope)
			 VALUES ('/ingest/t01.md', 'deadbeef', $1)`,
			shortTenantScope,
		)
		if err != nil {
			t.Fatalf("INSERT context_sources scope=%q failed (expected RED without 058, GREEN with): %v",
				shortTenantScope, err)
		}
	})

	// context_dream_links: needs two real, distinct blocks (FK + src!=dst CHECK).
	// raw_confidence/dream_version are NOT NULL but carry defaults (024), so the
	// minimal column set is (source, target, relationship, scope). The 23-char
	// scope is what overflows VARCHAR(20) today.
	t.Run("dream_links_long_scope", func(t *testing.T) {
		src := seedBlock(t, pool, "t01-dream-src")
		dst := seedBlock(t, pool, "t01-dream-dst")
		_, err := pool.Exec(ctx,
			`INSERT INTO context_dream_links
				(source_block_id, target_block_id, relationship, scope)
			 VALUES ($1::uuid, $2::uuid, 'topical', $3)`,
			src, dst, longScope,
		)
		if err != nil {
			t.Fatalf("INSERT context_dream_links scope=%q (len %d) failed (expected RED without 058, GREEN with): %v",
				longScope, len(longScope), err)
		}
	})

	// context_write_log: decision is the only NOT NULL/no-default column besides
	// the PK; scope is nullable but we set it to the 23-char value to exercise
	// the VARCHAR(20) overflow.
	t.Run("write_log_long_scope", func(t *testing.T) {
		_, err := pool.Exec(ctx,
			`INSERT INTO context_write_log (decision, scope)
			 VALUES ('stored', $1)`,
			longScope,
		)
		if err != nil {
			t.Fatalf("INSERT context_write_log scope=%q (len %d) failed (expected RED without 058, GREEN with): %v",
				longScope, len(longScope), err)
		}
	})
}
