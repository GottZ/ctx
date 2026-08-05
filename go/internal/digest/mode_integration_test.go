//go:build integration

// Wave W-E gates 1/2/4 (Cluster-Topic-Map, design/02 §7 "W-E"): `full` stays
// byte-identical, `stub` is a small pointer, `off` touches nothing.
//
//	go test -tags=integration ./internal/digest/ -run TestDigestMode -count=1 -v
package digest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/digest"
	"github.com/GottZ/ctx/internal/testdb"
)

func weRegistry(t *testing.T, pool *pgxpool.Pool) *blocktype.Registry {
	t.Helper()
	reg := blocktype.NewRegistry()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reg.Boot(ctx, pool)
	return reg
}

func weSeed(t *testing.T, pool *pgxpool.Pool, category, title string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(),
		`INSERT INTO context_blocks (category, title, content, scope)
		 VALUES ($1, $2, 'we fixture', 'private') RETURNING id::text`,
		category, title).Scan(&id); err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return id
}

type weBlock struct {
	content   string
	typeName  string
	updatedAt time.Time
}

func weReadMap(t *testing.T, pool *pgxpool.Pool) (weBlock, bool) {
	t.Helper()
	var b weBlock
	err := pool.QueryRow(context.Background(),
		`SELECT content, COALESCE(type_name, ''), updated_at FROM context_blocks
		  WHERE category = 'index' AND title = 'topic-map-private' AND scope = 'private'`).
		Scan(&b.content, &b.typeName, &b.updatedAt)
	if err != nil {
		return weBlock{}, false
	}
	return b, true
}

// TestDigestModeFullIsUnchanged is gate 1: the golden is built from the FIXTURE,
// not from the implementation, so a branch that leaks into the default path
// shows up as a byte difference rather than as a green tautology.
//
// RED against a version whose mode switch changes the full path in any way —
// a reordered header, a lost scope annotation, a different truncation.
func TestDigestModeFullIsUnchanged(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := weRegistry(t, pool)

	bID := weSeed(t, pool, "decisions", "b-decision")
	aID := weSeed(t, pool, "learnings", "a-learning")
	cID := weSeed(t, pool, "learnings", "c-learning")

	if err := digest.RunDigest(ctx, pool, reg, "full", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(full): %v", err)
	}
	got, ok := weReadMap(t, pool)
	if !ok {
		t.Fatal("full mode wrote no topic map")
	}

	want := fmt.Sprintf("Context Store Index | scope:private | 3 blocks | 2 categories | %s\n",
		time.Now().UTC().Format("2006-01-02")) +
		"\ndecisions (1)\n" +
		"  " + bID[:8] + " b-decision\n" +
		"\nlearnings (2)\n" +
		"  " + aID[:8] + " a-learning\n" +
		"  " + cID[:8] + " c-learning\n"

	if got.content != want {
		t.Errorf("full mode drifted:\n--- got ---\n%s\n--- want ---\n%s", got.content, want)
	}
	if got.typeName != "system-meta" {
		t.Errorf("type_name = %q, want system-meta", got.typeName)
	}
}

// TestDigestModeStub is gate 2: the map stops growing and starts pointing.
//
// It replaces an EXISTING full map in place — the same conflict key, so no
// consumer's `ctx get <id>` breaks — and it is small enough that the block can
// never again be the 80 KB slot thief it is today.
func TestDigestModeStub(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := weRegistry(t, pool)
	weSeed(t, pool, "learnings", "stub-fixture")

	if err := digest.RunDigest(ctx, pool, reg, "full", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(full): %v", err)
	}
	full, _ := weReadMap(t, pool)

	if err := digest.RunDigest(ctx, pool, reg, "stub", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(stub): %v", err)
	}
	stub, ok := weReadMap(t, pool)
	if !ok {
		t.Fatal("stub mode left no block at all — consumers would find nothing AND no hint")
	}
	if len(stub.content) > 512 {
		t.Errorf("stub is %d B, over the 512 B gate", len(stub.content))
	}
	if stub.content == full.content {
		t.Error("stub mode wrote the full map")
	}
	for _, want := range []string{"root-map-private", "ctx search index query:root-map"} {
		if !strings.Contains(stub.content, want) {
			t.Errorf("stub does not carry %q:\n%s", want, stub.content)
		}
	}
	if stub.typeName != "system-meta" {
		t.Errorf("stub type_name = %q, want system-meta (it must stay out of retrieval)", stub.typeName)
	}

	// A second stub run writes NOTHING: the text has no moving part, so the
	// 60 s debounce cannot turn it into a rewrite treadmill.
	if err := digest.RunDigest(ctx, pool, reg, "stub", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(stub, again): %v", err)
	}
	again, _ := weReadMap(t, pool)
	if !again.updatedAt.Equal(stub.updatedAt) {
		t.Errorf("the unchanged stub was rewritten: %v → %v", stub.updatedAt, again.updatedAt)
	}
}

// TestDigestModeOff is gate 4: the existing block stays byte-identical, and no
// new one appears where none was.
func TestDigestModeOff(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := weRegistry(t, pool)
	weSeed(t, pool, "learnings", "off-fixture")

	if err := digest.RunDigest(ctx, pool, reg, "full", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(full): %v", err)
	}
	before, _ := weReadMap(t, pool)

	weSeed(t, pool, "learnings", "off-fixture-2") // a corpus change off must ignore
	if err := digest.RunDigest(ctx, pool, reg, "off", "private", "private", []string{"private"}); err != nil {
		t.Fatalf("RunDigest(off): %v", err)
	}
	after, ok := weReadMap(t, pool)
	if !ok {
		t.Fatal("off mode DELETED the topic map")
	}
	if after.content != before.content || !after.updatedAt.Equal(before.updatedAt) {
		t.Errorf("off mode touched the block:\n--- before ---\n%s\n--- after ---\n%s", before.content, after.content)
	}

	// And on a scope that never had a map, off creates none.
	if err := digest.RunDigest(ctx, pool, reg, "off", "work", "work", []string{"work"}); err != nil {
		t.Fatalf("RunDigest(off, fresh scope): %v", err)
	}
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*)::int FROM context_blocks WHERE title = 'topic-map-work'`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Errorf("off mode created %d blocks on a fresh scope", n)
	}
}
