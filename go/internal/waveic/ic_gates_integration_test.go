//go:build integration

// Integration gates for Achse-02 Welle I-C (design/02 §7-I-C) against a real
// PG18 testcontainer. Every probe drives a REAL pipeline consumer through its
// exported entrypoint (guard.RunGuardBatch, dream.PickBlock, overview.Rebuild)
// or the exact SQL filter a private consumer uses (digest.fetchBlockMeta →
// digest.go:159, mirrored via DigestTypes()), proving the migration-084 issue/
// comment seeds take effect end-to-end. Each is a NEGATIVE probe: the comment
// names the seed value whose flip would turn it red.
//
// The registry is booted from the migrated DB — issue/comment come from the 084
// seed, NOT a test literal (that is the point: the shipped seed drives the
// pipelines). testdb applies the full migrations.FS chain incl. 084.
//
// Run with:
//
//	GOTMPDIR=/compose/n8n/.gocache GOCACHE=/compose/n8n/.gocache/build \
//	  go test -tags=integration ./internal/waveic/ -count=1 -v
package waveic

import (
	"context"
	"testing"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/guard"
	"github.com/GottZ/ctx/internal/overview"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// vec1024 returns a 1024-dim vector with a single axis set — distinct seeds are
// near-orthogonal (cosine ≈ 0), so guarded blocks come out clean (no unintended
// flag/archive side effects).
func vec1024(axis int) pgvec.Vector {
	v := make([]float32, 1024)
	v[axis%1024] = 1.0
	return pgvec.NewVector(v)
}

// seedBlock inserts one context_blocks row with an explicit type_name and
// lifecycle_state='knowledge' (guard/dream eligibility). emb may be nil (NULL
// embedding — fine for digest/overview which do not read it).
func seedBlock(t *testing.T, pool *pgxpool.Pool, id, typeName, scope, category, title string, emb *pgvec.Vector) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks
		   (id, category, title, content, scope, embedding, lifecycle_state, type_name)
		 VALUES ($1::uuid, $2, $3, 'body', $4, $5, 'knowledge', $6)`,
		id, category, title, scope, emb, typeName)
	if err != nil {
		t.Fatalf("seed block %s (%s): %v", id, typeName, err)
	}
}

func bootSet(t *testing.T, pool *pgxpool.Pool) *blocktype.Set {
	t.Helper()
	reg := blocktype.NewRegistry()
	if err := reg.Reload(context.Background(), pool); err != nil {
		t.Fatalf("registry reload: %v", err)
	}
	set := reg.Snapshot()
	// Sanity: the 084 seeds resolved from the DB with the intended policies.
	if _, ok := set.Resolve("issue"); !ok {
		t.Fatal("issue type not resolved from DB registry (migration 084 missing?)")
	}
	if _, ok := set.Resolve("comment"); !ok {
		t.Fatal("comment type not resolved from DB registry (migration 084 missing?)")
	}
	return set
}

func metaCheckedAt(t *testing.T, pool *pgxpool.Pool, id string) *string {
	t.Helper()
	var at *string
	if err := pool.QueryRow(context.Background(),
		`SELECT metadata->>'guard_checked_at' FROM context_blocks WHERE id = $1::uuid`, id).Scan(&at); err != nil {
		t.Fatalf("read guard_checked_at %s: %v", id, err)
	}
	return at
}

// TestICGuardBatchSkipsComment — a seeded comment block with an embedding is
// NEVER entered into the guard batch (guard.check=false), while a seeded issue
// IS checked (guard.check=true). RED if comment shipped guard.check/candidate
// true: comment would get a guard_checked_at stamp.
func TestICGuardBatchSkipsComment(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := bootSet(t, pool)

	const (
		gIssue   = "019f2c00-0000-7000-9000-0000000000a1"
		gComment = "019f2c00-0000-7000-9000-0000000000a2"
	)
	seedBlock(t, pool, gIssue, "issue", "private", "projects", "guard-issue", ptr(vec1024(10)))
	seedBlock(t, pool, gComment, "comment", "private", "projects", "guard-comment", ptr(vec1024(20)))

	if _, err := guard.RunGuardBatch(ctx, pool, set, 100); err != nil {
		t.Fatalf("RunGuardBatch: %v", err)
	}

	if metaCheckedAt(t, pool, gIssue) == nil {
		t.Error("issue block was NOT guard-checked — guard.check=true not effective")
	}
	if at := metaCheckedAt(t, pool, gComment); at != nil {
		t.Errorf("comment block WAS guard-checked (guard_checked_at=%q) — guard.check=false not effective", *at)
	}
}

// TestICDreamPicksIssueNotComment — draining dream.PickBlock over the linkable
// allowlist picks the issue (dream.linkable=true) and NEVER the comment
// (dream.linkable=false). RED if comment shipped dream.linkable=true.
func TestICDreamPicksIssueNotComment(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := bootSet(t, pool)

	const (
		dIssue   = "019f2c00-0000-7000-9000-0000000000b1"
		dComment = "019f2c00-0000-7000-9000-0000000000b2"
	)
	seedBlock(t, pool, dIssue, "issue", "private", "projects", "dream-issue", ptr(vec1024(30)))
	seedBlock(t, pool, dComment, "comment", "private", "projects", "dream-comment", ptr(vec1024(40)))

	linkable := set.DreamLinkableTypes()
	picked := map[string]bool{}
	for i := 0; i < 200; i++ {
		b, err := dream.PickBlock(ctx, pool, linkable)
		if err != nil {
			t.Fatalf("PickBlock: %v", err)
		}
		if b == nil {
			break
		}
		picked[b.ID] = true
	}
	if !picked[dIssue] {
		t.Error("issue block was NOT picked by dream — dream.linkable=true not effective")
	}
	if picked[dComment] {
		t.Error("comment block WAS picked by dream — dream.linkable=false not effective")
	}
}

// TestICDigestExcludesIssueTitles — the digest source selection (mirroring the
// private digest.fetchBlockMeta filter, digest.go:159, with DigestTypes()) skips
// issue AND comment titles while keeping knowledge. RED if issue/comment shipped
// digest.include=true: a 10k-issue repo would flood the topic-map (§6.8).
func TestICDigestExcludesIssueTitles(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := bootSet(t, pool)

	const (
		diKnow    = "019f2c00-0000-7000-9000-0000000000c1"
		diIssue   = "019f2c00-0000-7000-9000-0000000000c2"
		diComment = "019f2c00-0000-7000-9000-0000000000c3"
	)
	seedBlock(t, pool, diKnow, "knowledge", "private", "learnings", "digest-knowledge", nil)
	seedBlock(t, pool, diIssue, "issue", "private", "projects", "digest-issue-title", nil)
	seedBlock(t, pool, diComment, "comment", "private", "projects", "digest-comment-title", nil)

	// Exact filter of digest.fetchBlockMeta (digest.go:159-167): the digest
	// source is `scope = ANY AND NOT is_archived AND type_name = ANY(DigestTypes)`.
	rows, err := pool.Query(ctx,
		`SELECT title FROM context_blocks
		 WHERE scope = ANY($1::text[]) AND NOT is_archived AND type_name = ANY($2::text[])
		 ORDER BY category, title`,
		[]string{"private"}, set.DigestTypes())
	if err != nil {
		t.Fatalf("digest source query: %v", err)
	}
	defer rows.Close()
	titles := map[string]bool{}
	for rows.Next() {
		var title string
		if err := rows.Scan(&title); err != nil {
			t.Fatalf("scan title: %v", err)
		}
		titles[title] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	if !titles["digest-knowledge"] {
		t.Error("knowledge title missing from digest source — filter over-excludes")
	}
	if titles["digest-issue-title"] {
		t.Error("issue title present in digest source — digest.include=false not effective")
	}
	if titles["digest-comment-title"] {
		t.Error("comment title present in digest source — digest.include=false not effective")
	}
}

// TestICOverviewExcludesIssueComment — the LOOP overview gate: overview.Rebuild
// clusters knowledge blocks but NEVER admits issue/comment as nodes
// (overview.include=false), even when they carry a dream edge to a clustered
// knowledge block. RED if issue/comment shipped overview.include=true: they
// would appear in graph_cluster_member and flood the Louvain overview (§6.8).
func TestICOverviewExcludesIssueComment(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	set := bootSet(t, pool)

	const (
		ovKa      = "019f2c00-0000-7000-9000-0000000000d1"
		ovKb      = "019f2c00-0000-7000-9000-0000000000d2"
		ovIssue   = "019f2c00-0000-7000-9000-0000000000d3"
		ovComment = "019f2c00-0000-7000-9000-0000000000d4"
	)
	seedBlock(t, pool, ovKa, "knowledge", "private", "learnings", "ov-know-a", nil)
	seedBlock(t, pool, ovKb, "knowledge", "private", "learnings", "ov-know-b", nil)
	seedBlock(t, pool, ovIssue, "issue", "private", "projects", "ov-issue", nil)
	seedBlock(t, pool, ovComment, "comment", "private", "projects", "ov-comment", nil)
	// Edges: kA—kB (clustered pair) plus issue→kA and comment→kA. If the type
	// filter did NOT exclude issue/comment, these edges would pull them into a
	// cluster — so their absence proves the overview.include node cut, not a
	// missing edge.
	insDreamLink(t, pool, ovKa, ovKb)
	insDreamLink(t, pool, ovIssue, ovKa)
	insDreamLink(t, pool, ovComment, ovKa)

	if _, err := overview.Rebuild(ctx, pool, 1.0, set.VisibleTypes(), set.OverviewTypes()); err != nil {
		t.Fatalf("overview.Rebuild: %v", err)
	}

	members := clusterMembers(t, pool)
	if !members[ovKa] && !members[ovKb] {
		t.Error("no knowledge block clustered — Rebuild produced no members (test setup broke)")
	}
	if members[ovIssue] {
		t.Error("issue block clustered — overview.include=false not effective (LOOP gate)")
	}
	if members[ovComment] {
		t.Error("comment block clustered — overview.include=false not effective (LOOP gate)")
	}
}

func insDreamLink(t *testing.T, pool *pgxpool.Pool, src, dst string) {
	t.Helper()
	_, err := pool.Exec(context.Background(),
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', 0.9, 0.9, 'private')`, src, dst)
	if err != nil {
		t.Fatalf("insert dream link %s->%s: %v", src, dst, err)
	}
}

func clusterMembers(t *testing.T, pool *pgxpool.Pool) map[string]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT block_id::text FROM graph_cluster_member`)
	if err != nil {
		t.Fatalf("dump cluster members: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan member: %v", err)
		}
		out[id] = true
	}
	return out
}

func ptr(v pgvec.Vector) *pgvec.Vector { return &v }
