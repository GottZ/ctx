//go:build integration

// Issue #406, store side: UpdateBlock takes an ordered list of text edits and
// a shallow metadata merge. The edits resolve against the content of the row
// this transaction holds (SELECT … FOR NO KEY UPDATE), so two writers that edit
// one block one after the other both keep their edit; the merge is one jsonb
// expression of the UPDATE itself and reads the row the statement locked. All
// or none: a refused edit writes nothing.
//
// Run with:
//
//	go test -tags=integration ./internal/store/ -run TestUpdateBlockEdits -count=1 -v
package store_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

func i406Edit(old, repl string) store.ContentEdit { return store.ContentEdit{Old: old, New: &repl} }

// i406Waiters counts the backends of this database that wait for a lock.
func i406Waiters(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_stat_activity
		  WHERE datname = current_database() AND cardinality(pg_blocking_pids(pid)) > 0`).Scan(&n); err != nil {
		t.Fatalf("count lock waiters: %v", err)
	}
	return n
}

func TestUpdateBlockEdits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	scopes := []string{"private"}

	upsert := func(t *testing.T, title, content string, metadata map[string]any) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, "i406", title, content, nil, metadata, "private", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("upsert %s: %v", title, err)
		}
		return b
	}
	type row struct {
		content  string
		metadata map[string]any
		updated  time.Time
	}
	read := func(t *testing.T, id string) row {
		t.Helper()
		var r row
		if err := pool.QueryRow(ctx, `SELECT content, metadata, updated_at FROM context_blocks WHERE id = $1`, id).
			Scan(&r.content, &r.metadata, &r.updated); err != nil {
			t.Fatalf("read block: %v", err)
		}
		return r
	}

	t.Run("a unique match applies and marks the vector stale", func(t *testing.T) {
		b := upsert(t, "unique", "alpha beta gamma", nil)
		got, reEmbed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("beta", "BETA")}}, scopes)
		if err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if got.Content != "alpha BETA gamma" || read(t, b.ID).content != "alpha BETA gamma" {
			t.Fatalf("content = %q (stored %q), want the edited text", got.Content, read(t, b.ID).content)
		}
		if !reEmbed {
			t.Fatal("an edit that changed the content did not report a changed embed input")
		}
	})

	t.Run("zero or two matches write nothing and name the edit", func(t *testing.T) {
		b := upsert(t, "refused", "one two two", map[string]any{"k": "v"})
		before := read(t, b.ID)
		for _, tc := range []struct {
			edits   []store.ContentEdit
			index   int
			matches int
		}{
			{[]store.ContentEdit{i406Edit("one", "1"), i406Edit("three", "3")}, 1, 0},
			{[]store.ContentEdit{i406Edit("one", "1"), i406Edit("two", "2")}, 1, 2},
		} {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{
				Edits: tc.edits, MetadataMerge: map[string]any{"k": "changed"},
			}, scopes)
			var ee *store.ContentEditError
			if !errors.As(err, &ee) || ee.Index != tc.index || ee.Matches != tc.matches {
				t.Fatalf("err = %v, want *ContentEditError index %d matches %d", err, tc.index, tc.matches)
			}
			after := read(t, b.ID)
			if after.content != before.content || after.metadata["k"] != "v" || !after.updated.Equal(before.updated) {
				t.Fatalf("a refused edit list wrote: %+v, before %+v — the first edit and the merge must roll back with it", after, before)
			}
		}
	})

	t.Run("replace_all and order dependence", func(t *testing.T) {
		b := upsert(t, "order", "a-b-a", nil)
		two := "TWO"
		got, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{
			{Old: "a", New: &two, ReplaceAll: true},
			i406Edit("TWO-b", "first"),
		}}, scopes)
		if err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if got.Content != "first-TWO" {
			t.Fatalf("content = %q, want first-TWO", got.Content)
		}
	})

	t.Run("an edit that changes nothing keeps the vector", func(t *testing.T) {
		b := upsert(t, "same", "keep this", nil)
		got, reEmbed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("keep", "keep")}}, scopes)
		if err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if got.Content != "keep this" || reEmbed {
			t.Fatalf("content %q, re-embed %v — an unchanged content must keep the vector (issue #198)", got.Content, reEmbed)
		}
		// The match rule still holds for it: it asserts the text is there once.
		_, _, err = store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("gone", "gone")}}, scopes)
		var ee *store.ContentEditError
		if !errors.As(err, &ee) || ee.Matches != 0 {
			t.Fatalf("err = %v, want *ContentEditError with 0 matches", err)
		}
	})

	t.Run("the patch rules hold in the store", func(t *testing.T) {
		b := upsert(t, "rules", "text", nil)
		c := "whole"
		for _, data := range []store.UpdateBlockData{
			{Content: &c, Edits: []store.ContentEdit{i406Edit("text", "x")}},
			{Metadata: map[string]any{}, MetadataMerge: map[string]any{"k": 1}},
			{Edits: []store.ContentEdit{i406Edit("", "x")}},
		} {
			if _, _, err := store.UpdateBlock(ctx, pool, b.ID, data, scopes); err == nil {
				t.Fatalf("UpdateBlock(%+v) wrote, want a refusal", data)
			}
		}
		if read(t, b.ID).content != "text" {
			t.Fatal("a refused patch wrote")
		}
	})

	t.Run("an edit past the size cap writes nothing", func(t *testing.T) {
		b := upsert(t, "cap", "seed", nil)
		_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{
			Edits: []store.ContentEdit{i406Edit("seed", strings.Repeat("x", store.MaxBlockContentBytes+1))},
		}, scopes)
		if !errors.Is(err, store.ErrEditedContentTooLarge) {
			t.Fatalf("err = %v, want ErrEditedContentTooLarge", err)
		}
		if read(t, b.ID).content != "seed" {
			t.Fatal("an edit past the cap wrote")
		}
	})

	t.Run("metadata_merge sets, overwrites and removes keys and keeps the rest", func(t *testing.T) {
		b := upsert(t, "merge", "body", map[string]any{"keep": "k", "over": "old", "drop": "d"})
		got, reEmbed, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{
			MetadataMerge: map[string]any{"over": "new", "add": map[string]any{"nested": true}, "drop": nil, "absent": nil},
		}, scopes)
		if err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		m := read(t, b.ID).metadata
		if m["keep"] != "k" || m["over"] != "new" || m["add"] == nil {
			t.Fatalf("metadata = %v", m)
		}
		if _, ok := m["drop"]; ok {
			t.Fatalf("a null did not remove the key: %v", m)
		}
		if _, ok := m["absent"]; ok {
			t.Fatalf("a null for a key the block lacks wrote the key: %v", m)
		}
		if got.Metadata["over"] != "new" || reEmbed {
			t.Fatalf("answer metadata %v, re-embed %v", got.Metadata, reEmbed)
		}
	})

	t.Run("metadata_merge on a block whose metadata is NULL", func(t *testing.T) {
		b := upsert(t, "merge-null", "body", nil)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = NULL WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{MetadataMerge: map[string]any{"a": 1.0}}, scopes); err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if m := read(t, b.ID).metadata; m["a"] != 1.0 || len(m) != 1 {
			t.Fatalf("metadata = %v, want {a: 1}", m)
		}
	})

	t.Run("edits strip the guard mark like a content write", func(t *testing.T) {
		b := upsert(t, "guard-mark", "body", nil)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || '{"guard_checked_at":"2026-11-20T00:00:00Z"}' WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{
			Edits: []store.ContentEdit{i406Edit("body", "BODY")}, MetadataMerge: map[string]any{"x": "y"},
		}, scopes); err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		m := read(t, b.ID).metadata
		if _, ok := m["guard_checked_at"]; ok || m["x"] != "y" {
			t.Fatalf("metadata = %v, want the guard mark gone and x merged", m)
		}
	})

	t.Run("a block outside the write scopes is a miss, not an edit verdict", func(t *testing.T) {
		b, err := store.UpsertBlock(ctx, pool, "i406", "foreign", "foreign text", nil, nil, "other", true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatal(err)
		}
		got, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("absent", "x")}}, scopes)
		if err != nil || got != nil {
			t.Fatalf("UpdateBlock = (%v, %v), want (nil, nil): a match count would read text the key may not write", got, err)
		}
	})

	t.Run("edits beside a scope move", func(t *testing.T) {
		b := upsert(t, "move", "move me", nil)
		dest := "shared"
		got, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{
			Scope: &dest, Edits: []store.ContentEdit{i406Edit("move", "moved")},
		}, []string{"private", "shared"})
		if err != nil {
			t.Fatalf("UpdateBlock: %v", err)
		}
		if got.Scope != "shared" || got.Content != "moved me" {
			t.Fatalf("block = %s / %q, want shared / moved me", got.Scope, got.Content)
		}
	})

	t.Run("edits on a derivative are refused as provenance_protected", func(t *testing.T) {
		b := upsert(t, "derived", "derived text", nil)
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET metadata = metadata || '{"provenance":{"v":1}}' WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("absent", "x")}}, scopes)
		if !errors.Is(err, store.ErrProvenanceProtected) {
			t.Fatalf("err = %v, want ErrProvenanceProtected (and no match verdict)", err)
		}
	})

	t.Run("a pinned update writes only the row version it names", func(t *testing.T) {
		b := upsert(t, "pinned", "v1", nil)
		base := read(t, b.ID).updated
		v2 := "v2"
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &v2}, scopes); err != nil {
			t.Fatal(err)
		}
		v3 := "v3"
		_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &v3, ExpectUpdatedAt: &base}, scopes)
		if !errors.Is(err, store.ErrBlockChanged) {
			t.Fatalf("err = %v, want ErrBlockChanged", err)
		}
		if read(t, b.ID).content != "v2" {
			t.Fatal("a pinned update wrote over a newer row")
		}
		now := read(t, b.ID).updated
		if _, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Content: &v3, ExpectUpdatedAt: &now}, scopes); err != nil {
			t.Fatalf("a pin on the current version: %v", err)
		}
		if read(t, b.ID).content != "v3" {
			t.Fatal("a pin on the current version did not write")
		}
	})

	// The match runs on the row this transaction holds. A writer holding the
	// row changes it; two edit calls queue behind it; once it commits, each
	// call reads what the one before it left, and all three changes survive.
	// A read ahead of the lock would resolve both edits against the original
	// text and the later write would erase the earlier ones.
	t.Run("concurrent edits on one block all survive", func(t *testing.T) {
		b := upsert(t, "race", "alpha\nbeta\ngamma\n", map[string]any{"base": true})
		holder, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = holder.Rollback(ctx) }()
		if _, err := holder.Exec(ctx,
			`UPDATE context_blocks SET content = replace(content, 'alpha', 'ALPHA'),
			        metadata = metadata || '{"holder":1}', updated_at = now() WHERE id = $1`, b.ID); err != nil {
			t.Fatal(err)
		}
		errs := make(chan error, 3)
		go func() {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("beta", "BETA")}}, scopes)
			errs <- err
		}()
		go func() {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{Edits: []store.ContentEdit{i406Edit("gamma", "GAMMA")}}, scopes)
			errs <- err
		}()
		go func() {
			_, _, err := store.UpdateBlock(ctx, pool, b.ID, store.UpdateBlockData{MetadataMerge: map[string]any{"merge": 2}}, scopes)
			errs <- err
		}()
		deadline := time.Now().Add(10 * time.Second)
		for i406Waiters(t, ctx, pool) < 3 {
			if time.Now().After(deadline) {
				t.Fatalf("%d of 3 updates reached the row lock", i406Waiters(t, ctx, pool))
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := holder.Commit(ctx); err != nil {
			t.Fatal(err)
		}
		for range 3 {
			if err := <-errs; err != nil {
				t.Fatalf("UpdateBlock: %v", err)
			}
		}
		r := read(t, b.ID)
		if r.content != "ALPHA\nBETA\nGAMMA\n" {
			t.Fatalf("content = %q, want every change kept", r.content)
		}
		if r.metadata["base"] != true || r.metadata["holder"] != 1.0 || r.metadata["merge"] != 2.0 {
			t.Fatalf("metadata = %v, want base, holder and merge kept", r.metadata)
		}
	})
}
