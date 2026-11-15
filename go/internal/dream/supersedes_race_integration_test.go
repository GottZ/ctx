//go:build integration

// Issue #197, review threads 2 and 3: a dream supersedes guess must not close
// a cycle with writer-declared (pinned) supersedes links.
//
//   - TestDreamSupersedesWaitsForADeclaration: a declaration "B supersedes A"
//     is in flight (a second transaction holds the scope's supersedes lock
//     exclusively and B, as UpsertBlockSupersedes does, and has written its
//     link) while dream
//     writes the guess "A supersedes B". The guess must see the declaration
//     once it commits and be dropped; checked before it, the pair would end as
//     a two-block cycle with B a snapshot of A.
//   - TestDreamSupersedesRejectsAPinnedChain: B supersedes X supersedes Y
//     supersedes A, all declared; the guess "A supersedes B" closes the chain
//     and is dropped. A probe of the direct reverse link alone misses it.
//
// The in-flight declaration is written by hand on the second connection with
// the production lock function: the store function runs its own transaction
// and cannot be paused before its commit, which is the interleaving under test.
//
//	go test -tags=integration ./internal/dream/ -run 'TestDreamSupersedesWaitsForADeclaration|TestDreamSupersedesRejectsAPinnedChain' -count=20 -v
package dream_test

import (
	"context"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func raceSetup(t *testing.T) (*pgxpool.Pool, *blocktype.Set) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	reg := blocktype.NewRegistry()
	reg.Boot(context.Background(), pool)
	return pool, reg.Snapshot()
}

func raceBlock(t *testing.T, pool *pgxpool.Pool, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), pool, "race", title, title+" content", nil, nil, "private", true,
		store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

// raceSupersedesRow reports whether source → target is a supersedes link.
func raceSupersedesRow(t *testing.T, pool *pgxpool.Pool, source, target string) bool {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_dream_links
		  WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid AND relationship = 'supersedes'`,
		source, target).Scan(&n); err != nil {
		t.Fatalf("read link: %v", err)
	}
	return n > 0
}

func raceLifecycle(t *testing.T, pool *pgxpool.Pool, id string) string {
	t.Helper()
	var l string
	if err := pool.QueryRow(context.Background(), `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, id).Scan(&l); err != nil {
		t.Fatalf("read lifecycle: %v", err)
	}
	return l
}

func TestDreamSupersedesWaitsForADeclaration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	b := raceBlock(t, pool, "race-note")    // older
	a := raceBlock(t, pool, "race-note v2") // newer: dream may guess "a supersedes b"

	decl, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = decl.Rollback(ctx) }()
	if err := store.LockSupersedesScope(ctx, decl, "private"); err != nil {
		t.Fatalf("scope lock: %v", err)
	}
	for _, st := range []struct {
		sql  string
		args []any
	}{
		{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR UPDATE`, []any{b}},
		{`SELECT 1 FROM context_blocks WHERE id = $1::uuid FOR NO KEY UPDATE`, []any{a}},
		{`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, confidence, raw_confidence, scope, metadata, pinned)
		  VALUES ($1::uuid, $2::uuid, 'supersedes', 1, 1, 'private', '{"origin":"declared"}', true)`, []any{b, a}},
		{`UPDATE context_blocks SET lifecycle_state = 'snapshot', superseded_by = $1::uuid WHERE id = $2::uuid`, []any{b, a}},
	} {
		if _, err := decl.Exec(ctx, st.sql, st.args...); err != nil {
			t.Fatalf("declaration: %v", err)
		}
	}

	done := make(chan error, 1)
	go func() {
		_, err := dream.WriteLinks(ctx, pool, set, a, "private", 1.0,
			[]dream.Link{{TargetID: b, Relationship: "supersedes", Confidence: 0.9}})
		done <- err
	}()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var waiting int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM pg_stat_activity WHERE wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&waiting); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the dream batch never waited for the declaration")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := decl.Commit(ctx); err != nil {
		t.Fatalf("commit declaration: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("write links: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("the dream batch did not finish")
	}

	if raceSupersedesRow(t, pool, a, b) {
		t.Errorf("dream wrote %s supersedes %s against the committed declaration %s supersedes %s — a two-block cycle", a, b, b, a)
	}
	if l := raceLifecycle(t, pool, b); l == "snapshot" {
		t.Errorf("the declared successor %s became a snapshot", b)
	}
}

func TestDreamSupersedesRejectsAPinnedChain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, set := raceSetup(t)
	ctx := context.Background()
	decl := func(source, target string) {
		t.Helper()
		src, err := store.GetBlock(ctx, pool, nil, source, []string{"private"}, nil)
		if err != nil || src == nil {
			t.Fatalf("read %s: %v", source, err)
		}
		if _, err := store.UpsertBlockSupersedes(ctx, pool, src.Category, src.Title, src.Content, nil, nil, "private", true,
			store.SensitivityWrite{}, "", store.SupersedesDeclaration{TargetID: target, APIKeyID: "race", Types: set}); err != nil {
			t.Fatalf("declare %s supersedes %s: %v", source, target, err)
		}
	}
	b := raceBlock(t, pool, "chain-note")    // older
	a := raceBlock(t, pool, "chain-note v2") // newer
	y := raceBlock(t, pool, "chain-y")
	x := raceBlock(t, pool, "chain-x")
	decl(y, a) // y supersedes a
	decl(x, y) // x supersedes y
	decl(b, x) // b supersedes x: b ⇝ a through two blocks

	if _, err := dream.WriteLinks(ctx, pool, set, a, "private", 1.0,
		[]dream.Link{{TargetID: b, Relationship: "supersedes", Confidence: 0.9}}); err != nil {
		t.Fatalf("write links: %v", err)
	}
	if raceSupersedesRow(t, pool, a, b) {
		t.Errorf("dream wrote %s supersedes %s, closing the declared chain %s ⇝ %s", a, b, b, a)
	}
	if l := raceLifecycle(t, pool, b); l == "snapshot" {
		t.Errorf("the head of the declared chain %s became a snapshot", b)
	}
}
