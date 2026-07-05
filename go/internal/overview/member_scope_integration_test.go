//go:build integration

package overview_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

// TestMemberScope_DenormalizedFromLouvainInput is the B-W2 DB gate: after a
// rebuild, EVERY member row carries exactly its block's scope (JOIN assert),
// across a multi-scope corpus. Red probe (documented in the wave report): a
// mutation that writes NULL/a wrong scope trips either the 087 NOT NULL
// constraint or the mismatch assert below — both red.
func TestMemberScope_DenormalizedFromLouvainInput(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-0000000000b1" // private
		B = "019d0000-0000-7000-9000-0000000000b2" // private
		C = "019d0000-0000-7000-9000-0000000000b3" // shared
		D = "019d0000-0000-7000-9000-0000000000b4" // work
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "private", "learnings", "B")
	insBlock(t, pool, C, "shared", "learnings", "C")
	insBlock(t, pool, D, "work", "learnings", "D")
	insLink(t, pool, A, B, 0.9)
	insLink(t, pool, B, C, 0.8)
	insLink(t, pool, C, D, 0.7)

	types := []string{"knowledge"}
	stats, err := overview.Rebuild(ctx, pool, overview.Options{
		Resolution: 1.0, VisibleTypes: types, OverviewTypes: types,
	})
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if stats.Skipped || stats.NodeCount != 4 {
		t.Fatalf("rebuild skipped=%v nodes=%d, want 4 members", stats.Skipped, stats.NodeCount)
	}

	// JOIN assert: no member without a scope, none disagreeing with its block.
	var mismatches int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM graph_cluster_member m
		JOIN context_blocks b ON b.id = m.block_id
		WHERE m.scope IS NULL OR m.scope <> b.scope`).Scan(&mismatches); err != nil {
		t.Fatal(err)
	}
	if mismatches != 0 {
		t.Fatalf("%d member rows with NULL/mismatched scope, want 0", mismatches)
	}

	// Spot check the partition values themselves (not just agreement).
	var workCount int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM graph_cluster_member WHERE scope = 'work'`).Scan(&workCount); err != nil {
		t.Fatal(err)
	}
	if workCount != 1 {
		t.Fatalf("work-scope members = %d, want 1", workCount)
	}
}

// TestMemberScope_Migration087Backfill proves the 087 backfill path against
// the REAL migration file: reconstruct the pre-087 shape (drop the column),
// insert legacy member rows, re-apply 087 from the embedded FS, and assert
// the rows got their block's scope and the column is NOT NULL again. The
// file's idempotency (ADD COLUMN IF NOT EXISTS / NULL-only UPDATE / ON
// CONFLICT version insert) is what makes this replay legitimate.
func TestMemberScope_Migration087Backfill(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	const (
		A = "019d0000-0000-7000-9000-0000000000c1" // private
		B = "019d0000-0000-7000-9000-0000000000c2" // work
	)
	insBlock(t, pool, A, "private", "learnings", "A")
	insBlock(t, pool, B, "work", "learnings", "B")

	// Pre-087 shape: no scope column, legacy rows without scope.
	if _, err := pool.Exec(ctx, `ALTER TABLE graph_cluster_member DROP COLUMN scope`); err != nil {
		t.Fatalf("drop column (pre-087 shape): %v", err)
	}
	if _, err := pool.Exec(ctx, `
		INSERT INTO graph_cluster_member (block_id, cluster_id) VALUES
		($1::uuid, '019d0000-0000-7000-9000-00000000dddd'),
		($2::uuid, '019d0000-0000-7000-9000-00000000dddd')`, A, B); err != nil {
		t.Fatalf("insert legacy members: %v", err)
	}

	// Re-apply the real 087 file (embedded FS = what the runner executes).
	sql, err := migrations.FS.ReadFile("087_member_scope.sql")
	if err != nil {
		t.Fatalf("read embedded 087: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		t.Fatalf("re-apply 087: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}

	// Backfill assert: both legacy rows carry their block's scope.
	rows, err := pool.Query(ctx, `
		SELECT m.block_id::text, m.scope, b.scope FROM graph_cluster_member m
		JOIN context_blocks b ON b.id = m.block_id ORDER BY m.block_id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id, got, want string
		if err := rows.Scan(&id, &got, &want); err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Fatalf("backfill: member %s scope=%q, want %q", id, got, want)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("backfilled members = %d, want 2", n)
	}

	// NOT NULL restored: a scope-less insert must fail with 23502 (the block
	// EXISTS, so an FK error cannot green-wash this assert — rollback note, 087).
	const E = "019d0000-0000-7000-9000-0000000000c3"
	insBlock(t, pool, E, "private", "learnings", "E")
	_, err = pool.Exec(ctx, `
		INSERT INTO graph_cluster_member (block_id, cluster_id)
		VALUES ($1::uuid, '019d0000-0000-7000-9000-00000000dddd')`, E)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23502" {
		t.Fatalf("scope-less member insert: err=%v, want SQLSTATE 23502 (NOT NULL restored by 087 replay)", err)
	}
}
