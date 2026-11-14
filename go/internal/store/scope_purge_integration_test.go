//go:build integration

// Integration tests of the scope purge (issue #202): the manage action that
// hard-deletes block data inside ONE scope, narrower than tenant-delete.
//
// The fixtures go through the production write paths where one exists —
// store.UpsertBlock (incl. the server badge for a derivative), UpsertBlob,
// InsertIssueBlock/InsertCommentBlock, DeleteBlock (the archive), the
// block-type registry writers. Dream links are seeded by INSERT like the
// tenant prune test does: their only writer is the dream cycle's model
// output. The multi-batch case seeds its rows in bulk (generate_series), as a
// write path per row would only measure the fixture.
//
//	go test -tags=integration ./internal/store/ -run TestScopePurge -count=1 -v
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// spFixture is one test database with two tenants and their scopes.
type spFixture struct {
	pool       *pgxpool.Pool
	ctx        context.Context
	tenantA    string // owns spScopeA… scopes
	tenantB    string // owns the bystander scope
	adminKeyID string // a server-admin key, the audit actor
}

const spBystander = "sp-b:keep"

func spSetup(t *testing.T) *spFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	a, err := store.CreateTenant(ctx, pool, "sp-a", "purge A")
	if err != nil {
		t.Fatalf("tenant a: %v", err)
	}
	b, err := store.CreateTenant(ctx, pool, "sp-b", "purge B")
	if err != nil {
		t.Fatalf("tenant b: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, b.ID, spBystander, nil); err != nil {
		t.Fatalf("bystander scope: %v", err)
	}
	var key string
	if err := pool.QueryRow(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('sp-admin') RETURNING id)
		 INSERT INTO context_api_keys (key_hash, label, home_scope, tenant_id, principal_id, is_admin)
		 SELECT 'sp-admin-hash', 'sp-admin', 'private', $1::uuid, p.id, true FROM p
		 RETURNING id::text`, defaultTenantID).Scan(&key); err != nil {
		t.Fatalf("admin key: %v", err)
	}
	return &spFixture{pool: pool, ctx: ctx, tenantA: a.ID, tenantB: b.ID, adminKeyID: key}
}

// scope registers a fresh scope of tenant A.
func (f *spFixture) scope(t *testing.T, name string) string {
	t.Helper()
	s := "sp-a:" + name
	if _, err := store.AssignTenantScope(f.ctx, f.pool, f.tenantA, s, nil); err != nil {
		t.Fatalf("scope %s: %v", s, err)
	}
	return s
}

func (f *spFixture) block(t *testing.T, scope, title string, tags []string) string {
	t.Helper()
	b, err := store.UpsertBlock(f.ctx, f.pool, "learnings", title, "body of "+title, tags, nil,
		scope, true, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	return b.ID
}

// derivative writes a block the way a derived arm does: the server badge plus
// a provenance object (dsProvenanceOf, derived_sensitivity_integration_test.go).
func (f *spFixture) derivative(t *testing.T, scope, title string, tags []string) string {
	t.Helper()
	b, err := store.UpsertBlock(f.ctx, f.pool, "learnings", title, "derived "+title, tags,
		dsProvenanceOf("sp-arm", derived.StratumDerived), scope, true,
		store.SensitivityWrite{Derived: true}, derived.TypeCatalog)
	if err != nil {
		t.Fatalf("derivative %s: %v", title, err)
	}
	return b.ID
}

func (f *spFixture) link(t *testing.T, src, dst, scope string) {
	t.Helper()
	if _, err := f.pool.Exec(f.ctx,
		`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope)
		 VALUES ($1::uuid, $2::uuid, 'topical', $3)`, src, dst, scope); err != nil {
		t.Fatalf("link: %v", err)
	}
}

func (f *spFixture) blob(t *testing.T, scope, title, blockID string) {
	t.Helper()
	if _, err := store.UpsertBlob(f.ctx, f.pool, "learnings", title, title+".txt", "text/plain", scope,
		[]byte("blob "+title), nil, nil, blockID); err != nil {
		t.Fatalf("blob %s: %v", title, err)
	}
}

func (f *spFixture) count(t *testing.T, q string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := f.pool.QueryRow(f.ctx, q, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func (f *spFixture) blocksIn(t *testing.T, scope string) int64 {
	return f.count(t, `SELECT count(*) FROM context_blocks WHERE scope = $1`, scope)
}

func (f *spFixture) admin() store.PurgeQuery {
	return store.PurgeQuery{AllTenants: true, Stamp: store.APIStamp(f.adminKeyID, "req-sp")}
}

func confirm(n int64) *int64 { return &n }

func TestScopePurge_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	keep := f.block(t, spBystander, "bystander", []string{"import"})

	// A dry run counts per table and deletes nothing; a filter narrows to the
	// matching blocks; a derivative is skipped and counted; links count when
	// they reference a matched block; a filtered purge counts no blobs.
	t.Run("dry_run_counts_and_deletes_nothing", func(t *testing.T) {
		s := f.scope(t, "dry")
		a := f.block(t, s, "imp-a", []string{"import", "x"})
		b := f.block(t, s, "imp-b", []string{"import"})
		f.block(t, s, "imp-c", []string{"import"})
		other := f.block(t, s, "own", []string{"mine"})
		f.derivative(t, s, "derived-import", []string{"import"})
		f.link(t, a, b, s)
		f.link(t, other, a, s)
		f.blob(t, s, "blob-dry", "")

		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Tags: []string{"import"}}
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("dry run: %v", err)
		}
		if !res.DryRun || res.Deleted != nil {
			t.Fatalf("dry run reported a deletion: %+v", res)
		}
		want := store.PurgeCounts{Blocks: 3, DreamLinks: 2, Blobs: 0, DerivedSkipped: 1}
		if res.Counted != want {
			t.Fatalf("counted = %+v, want %+v", res.Counted, want)
		}
		if n := f.blocksIn(t, s); n != 5 {
			t.Fatalf("blocks after dry run = %d, want 5 (nothing deleted)", n)
		}
		if n := f.count(t, `SELECT count(*) FROM context_settings_audit WHERE entity_type = 'scope_purge' AND entity_key = $1`, s); n != 0 {
			t.Fatalf("a dry run wrote %d audit rows, want 0", n)
		}
	})

	// The confirm is bound to the count: another number deletes nothing.
	t.Run("confirm_mismatch_deletes_nothing", func(t *testing.T) {
		s := f.scope(t, "mismatch")
		f.block(t, s, "m-1", nil)
		f.block(t, s, "m-2", nil)
		q := f.admin()
		q.Scope = s
		q.ConfirmBlocks = confirm(1)
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if !errors.Is(err, store.ErrPurgeConfirmMismatch) {
			t.Fatalf("err = %v, want ErrPurgeConfirmMismatch", err)
		}
		if res == nil || res.Counted.Blocks != 2 {
			t.Fatalf("mismatch must answer the fresh count: %+v", res)
		}
		if n := f.blocksIn(t, s); n != 2 {
			t.Fatalf("blocks after a refused confirm = %d, want 2", n)
		}
	})

	// The filtered run removes the matched blocks and every dream link that
	// references one of them, keeps the rest of the scope, the derivative, the
	// scope's blobs (a blob linked to a purged block loses the link) and every
	// other scope; it leaves one audit row naming actor and request id.
	t.Run("filtered_run_removes_matched_and_their_links", func(t *testing.T) {
		s := f.scope(t, "filtered")
		a := f.block(t, s, "f-a", []string{"import"})
		b := f.block(t, s, "f-b", []string{"import"})
		other := f.block(t, s, "f-own", nil)
		drv := f.derivative(t, s, "f-derived", []string{"import"})
		f.link(t, a, b, s)
		f.link(t, other, b, s)
		f.blob(t, s, "f-blob", a)

		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Tags: []string{"import"}}
		q.ConfirmBlocks = confirm(2)
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if res.DryRun || res.Deleted == nil {
			t.Fatalf("real run reported no deletion: %+v", res)
		}
		if *res.Deleted != (store.PurgeDeleted{Blocks: 2, DreamLinks: 2, Blobs: 0}) {
			t.Fatalf("deleted = %+v, want blocks 2, links 2, blobs 0", *res.Deleted)
		}
		for _, id := range []string{a, b} {
			if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id); n != 0 {
				t.Errorf("matched block %s survived", id)
			}
		}
		for _, id := range []string{other, drv, keep} {
			if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id); n != 1 {
				t.Errorf("block %s outside the match was deleted", id)
			}
		}
		if n := f.count(t, `SELECT count(*) FROM context_dream_links WHERE scope = $1`, s); n != 0 {
			t.Errorf("dream links left in %s: %d", s, n)
		}
		if n := f.count(t, `SELECT count(*) FROM context_blobs WHERE scope = $1 AND context_block_id IS NULL`, s); n != 1 {
			t.Errorf("the scope's blob must stay with its block link cleared, got %d such rows", n)
		}
		var actor, reqID, tenant string
		var newValue []byte
		if err := f.pool.QueryRow(f.ctx,
			`SELECT api_key_id::text, metadata->>'request_id', metadata->>'tenant_id', new_value
			   FROM context_settings_audit WHERE entity_type = 'scope_purge' AND entity_key = $1 AND action = 'purge'`, s).
			Scan(&actor, &reqID, &tenant, &newValue); err != nil {
			t.Fatalf("audit row: %v", err)
		}
		if actor != f.adminKeyID || reqID != "req-sp" || tenant != f.tenantA {
			t.Errorf("audit row actor=%q request_id=%q tenant_id=%q, want %q/req-sp/%q", actor, reqID, tenant, f.adminKeyID, f.tenantA)
		}
		var nv map[string]any
		if err := json.Unmarshal(newValue, &nv); err != nil || nv["counted"] == nil || nv["filter"] == nil {
			t.Errorf("audit new_value lacks counted/filter: %s", newValue)
		}
	})

	// Without a filter the purge drains the scope: blocks, their links and the
	// scope's blobs. A derivative stays and is counted, it is no failure.
	t.Run("unfiltered_drain_takes_blobs_and_skips_derivatives", func(t *testing.T) {
		s := f.scope(t, "drain")
		x := f.block(t, s, "d-x", nil)
		y := f.block(t, s, "d-y", nil)
		drv := f.derivative(t, s, "d-derived", nil)
		f.link(t, x, y, s)
		f.blob(t, s, "d-blob-1", x)
		f.blob(t, s, "d-blob-2", "")
		q := f.admin()
		q.Scope = s
		dry, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("dry: %v", err)
		}
		if dry.Counted != (store.PurgeCounts{Blocks: 2, DreamLinks: 1, Blobs: 2, DerivedSkipped: 1}) {
			t.Fatalf("dry counts = %+v", dry.Counted)
		}
		q.ConfirmBlocks = confirm(dry.Counted.Blocks)
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("drain: %v", err)
		}
		if *res.Deleted != (store.PurgeDeleted{Blocks: 2, DreamLinks: 1, Blobs: 2}) {
			t.Fatalf("deleted = %+v", *res.Deleted)
		}
		if n := f.blocksIn(t, s); n != 1 {
			t.Fatalf("blocks left = %d, want 1 (the derivative)", n)
		}
		if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, drv); n != 1 {
			t.Fatal("the derivative was deleted")
		}
		if n := f.count(t, `SELECT count(*) FROM context_blobs WHERE scope = $1`, s); n != 0 {
			t.Fatalf("blobs left = %d, want 0", n)
		}
		if n := f.blocksIn(t, spBystander); n != 1 {
			t.Fatalf("bystander scope = %d blocks, want 1", n)
		}
	})

	// The goal of the issue: after a purge of mis-imported rows, a block type
	// only they used can be deleted. Archiving alone does not free it — the
	// type delete counts archived rows too.
	t.Run("archived_only_frees_the_block_type", func(t *testing.T) {
		s := f.scope(t, "types")
		const typ = "sp-misimport"
		row, err := store.CreateBlockType(f.ctx, f.pool, store.BlockTypeWrite{
			Name: typ, Scope: store.GlobalScope, DisplayName: typ, Config: json.RawMessage(`{"v":1}`),
		}, nil, "")
		if err != nil {
			t.Fatalf("type: %v", err)
		}
		ids := make([]string, 0, 3)
		for i := 0; i < 3; i++ {
			b, err := store.UpsertBlock(f.ctx, f.pool, "learnings", fmt.Sprintf("mis-%d", i), "imported by mistake",
				nil, nil, s, true, store.SensitivityWrite{}, typ)
			if err != nil {
				t.Fatalf("typed upsert: %v", err)
			}
			ids = append(ids, b.ID)
		}
		live := f.block(t, s, "live-one", nil)
		for _, id := range ids {
			if _, err := store.DeleteBlock(f.ctx, f.pool, id, []string{s}); err != nil {
				t.Fatalf("archive %s: %v", id, err)
			}
		}
		_, err = store.DeleteBlockType(f.ctx, f.pool, row.ID, []string{store.GlobalScope}, nil, "")
		var inUse *store.BlockTypeInUseError
		if !errors.As(err, &inUse) || inUse.Archived != 3 {
			t.Fatalf("type delete before the purge = %v, want in use by 3 archived", err)
		}
		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{ArchivedOnly: true}
		q.ConfirmBlocks = confirm(3)
		if _, err := store.PurgeScope(f.ctx, f.pool, q); err != nil {
			t.Fatalf("purge archived: %v", err)
		}
		found, err := store.DeleteBlockType(f.ctx, f.pool, row.ID, []string{store.GlobalScope}, nil, "")
		if err != nil || !found {
			t.Fatalf("type delete after the purge = (%v, %v), want (true, nil)", found, err)
		}
		if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, live); n != 1 {
			t.Fatal("archived_only deleted a live block")
		}
	})

	// A block whose parent is purged goes with it (parent_id is ON DELETE
	// CASCADE): the count names it. A child that lives in another scope is
	// never removed through a purge of this one — the run refuses.
	t.Run("children_cascade_counted_and_cross_scope_child_refused", func(t *testing.T) {
		s := f.scope(t, "issues")
		other := f.scope(t, "elsewhere")
		var issueID, commentID string
		if err := pgxdb.Write(f.ctx, f.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			iss, err := store.InsertIssueBlock(f.ctx, tx, store.IssueFields{Scope: s, Title: "imported issue", Content: "x", Tags: []string{"import"}}, []string{s})
			if err != nil {
				return err
			}
			c, err := store.InsertCommentBlock(f.ctx, tx, iss.ID, store.CommentFields{Author: "a", Content: "c"}, []string{s})
			if err != nil {
				return err
			}
			issueID, commentID = iss.ID, c.ID
			return nil
		}); err != nil {
			t.Fatalf("issue + comment: %v", err)
		}
		f.link(t, commentID, f.block(t, s, "linked-to-comment", nil), s)
		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Tags: []string{"import"}}
		dry, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("dry: %v", err)
		}
		if dry.Counted.Blocks != 2 || dry.Counted.CascadedChildren != 1 || dry.Counted.DreamLinks != 1 {
			t.Fatalf("counts = %+v, want blocks 2 (issue + comment), cascaded 1, links 1", dry.Counted)
		}

		// Move the comment to another scope of the same tenant: purging the
		// issue's scope would now cascade into a block outside it.
		if _, _, err := store.UpdateBlock(f.ctx, f.pool, commentID, store.UpdateBlockData{Scope: &other}, []string{s, other}); err != nil {
			t.Fatalf("move comment: %v", err)
		}
		dry, err = store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("dry after move: %v", err)
		}
		if dry.Counted.ChildrenOutsideScope != 1 {
			t.Fatalf("children_outside_scope = %d, want 1", dry.Counted.ChildrenOutsideScope)
		}
		q.ConfirmBlocks = confirm(dry.Counted.Blocks)
		if _, err := store.PurgeScope(f.ctx, f.pool, q); !errors.Is(err, store.ErrPurgeChildOutsideScope) {
			t.Fatalf("run with a child outside the scope = %v, want ErrPurgeChildOutsideScope", err)
		}
		for _, id := range []string{issueID, commentID} {
			if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id); n != 1 {
				t.Errorf("block %s deleted by a refused run", id)
			}
		}
	})

	// The count fixes a time: a row created after it (here: dated after it) is
	// neither counted nor deleted, so the confirm binds to the counted set.
	t.Run("rows_newer_than_the_count_stay", func(t *testing.T) {
		s := f.scope(t, "asof")
		f.block(t, s, "old-row", nil)
		newer := f.block(t, s, "newer-row", nil)
		f.blob(t, s, "newer-blob", "")
		if _, err := f.pool.Exec(f.ctx, `UPDATE context_blocks SET created_at = now() + interval '1 hour' WHERE id = $1::uuid`, newer); err != nil {
			t.Fatalf("date the row: %v", err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE context_blobs SET created_at = now() + interval '1 hour' WHERE scope = $1`, s); err != nil {
			t.Fatalf("date the blob: %v", err)
		}
		q := f.admin()
		q.Scope = s
		q.ConfirmBlocks = confirm(1)
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("purge: %v", err)
		}
		if res.Counted.Blocks != 1 || res.Counted.Blobs != 0 || *res.Deleted != (store.PurgeDeleted{Blocks: 1}) {
			t.Fatalf("counted %+v deleted %+v, want only the older block", res.Counted, *res.Deleted)
		}
		if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, newer); n != 1 {
			t.Fatal("a block dated after the count was deleted")
		}
		if n := f.count(t, `SELECT count(*) FROM context_blobs WHERE scope = $1`, s); n != 1 {
			t.Fatal("a blob dated after the count was deleted")
		}
	})

	// category and the time window narrow: a neighbour outside each bound
	// survives.
	t.Run("category_and_time_window_narrow", func(t *testing.T) {
		s := f.scope(t, "window")
		mk := func(category, title string, age string) string {
			b, err := store.UpsertBlock(f.ctx, f.pool, category, title, "body "+title, nil, nil, s, true, store.SensitivityWrite{}, "")
			if err != nil {
				t.Fatalf("upsert %s: %v", title, err)
			}
			if _, err := f.pool.Exec(f.ctx, `UPDATE context_blocks SET created_at = now() - $2::interval WHERE id = $1::uuid`, b.ID, age); err != nil {
				t.Fatalf("date %s: %v", title, err)
			}
			return b.ID
		}
		hit := mk("imports", "in-window", "2 days")
		tooOld := mk("imports", "too-old", "10 days")
		tooNew := mk("imports", "too-new", "1 hour")
		otherCat := mk("learnings", "other-category", "2 days")
		var after, before time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT now() - interval '5 days', now() - interval '1 day'`).Scan(&after, &before); err != nil {
			t.Fatal(err)
		}
		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Category: "imports", CreatedAfter: &after, CreatedBefore: &before}
		q.ConfirmBlocks = confirm(1)
		if _, err := store.PurgeScope(f.ctx, f.pool, q); err != nil {
			t.Fatalf("purge: %v", err)
		}
		if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, hit); n != 0 {
			t.Error("the block inside category and window survived")
		}
		for name, id := range map[string]string{"too old": tooOld, "too new": tooNew, "other category": otherCat} {
			if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id); n != 1 {
				t.Errorf("the %s neighbour was deleted", name)
			}
		}
	})

	// A child that appears below a matched block after the count (dated after
	// it here) stops the run: the run never deletes what it did not count.
	t.Run("child_newer_than_the_count_refuses_the_run", func(t *testing.T) {
		s := f.scope(t, "late-child")
		var issueID, commentID string
		if err := pgxdb.Write(f.ctx, f.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			iss, err := store.InsertIssueBlock(f.ctx, tx, store.IssueFields{Scope: s, Title: "issue", Content: "x", Tags: []string{"import"}}, []string{s})
			if err != nil {
				return err
			}
			c, err := store.InsertCommentBlock(f.ctx, tx, iss.ID, store.CommentFields{Author: "a", Content: "late"}, []string{s})
			if err != nil {
				return err
			}
			issueID, commentID = iss.ID, c.ID
			return nil
		}); err != nil {
			t.Fatalf("issue + comment: %v", err)
		}
		if _, err := f.pool.Exec(f.ctx, `UPDATE context_blocks SET created_at = now() + interval '1 hour' WHERE id = $1::uuid`, commentID); err != nil {
			t.Fatalf("date the comment: %v", err)
		}
		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Tags: []string{"import"}}
		q.ConfirmBlocks = confirm(2)
		if _, err := store.PurgeScope(f.ctx, f.pool, q); !errors.Is(err, store.ErrPurgeConfirmMismatch) {
			t.Fatalf("run reaching a child newer than the count = %v, want ErrPurgeConfirmMismatch", err)
		}
		for _, id := range []string{issueID, commentID} {
			if n := f.count(t, `SELECT count(*) FROM context_blocks WHERE id = $1::uuid`, id); n != 1 {
				t.Errorf("block %s deleted by a refused run", id)
			}
		}
	})

	// A purged superseder releases its target: the target of a removed
	// supersedes link is reconciled like dream-link-resolve does it, so a
	// surviving block does not stay a snapshot pointing at a deleted row.
	t.Run("purged_superseder_releases_its_target", func(t *testing.T) {
		s := f.scope(t, "supersedes")
		older := f.block(t, s, "older-version", nil)
		newer := f.block(t, s, "newer-import", []string{"import"})
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope, confidence, raw_confidence)
			 VALUES ($1::uuid, $2::uuid, 'supersedes', $3, 0.9, 0.9)`, newer, older, s); err != nil {
			t.Fatalf("supersedes link: %v", err)
		}
		if err := pgxdb.Write(f.ctx, f.pool, pgxdb.Stages{}, func(tx pgx.Tx) error {
			_, err := store.ReconcileSupersedesTargets(f.ctx, tx, []string{older}, newer)
			return err
		}); err != nil {
			t.Fatalf("mark snapshot: %v", err)
		}
		var state string
		if err := f.pool.QueryRow(f.ctx, `SELECT lifecycle_state FROM context_blocks WHERE id = $1::uuid`, older).Scan(&state); err != nil || state != "snapshot" {
			t.Fatalf("precondition: older is %q (%v), want snapshot", state, err)
		}
		q := f.admin()
		q.Scope = s
		q.Filter = store.PurgeFilter{Tags: []string{"import"}}
		q.ConfirmBlocks = confirm(1)
		if _, err := store.PurgeScope(f.ctx, f.pool, q); err != nil {
			t.Fatalf("purge: %v", err)
		}
		var pointer *string
		if err := f.pool.QueryRow(f.ctx, `SELECT lifecycle_state, superseded_by::text FROM context_blocks WHERE id = $1::uuid`, older).Scan(&state, &pointer); err != nil {
			t.Fatalf("read older: %v", err)
		}
		if state != "knowledge" || pointer != nil {
			t.Fatalf("older after its superseder was purged: state=%q superseded_by=%v, want knowledge/NULL", state, pointer)
		}
	})

	// Scope resolution: a tenant reader reaches only its own tenant's scopes; a
	// foreign and an unknown scope answer alike; system scopes are refused for
	// everyone; a query naming no reader reads nothing.
	t.Run("scope_resolution_is_fail_closed", func(t *testing.T) {
		s := f.scope(t, "own")
		f.block(t, s, "own-1", nil)
		tenantA := store.PurgeQuery{TenantID: f.tenantA}
		for _, sc := range []string{spBystander, "sp-a:never-registered"} {
			q := tenantA
			q.Scope = sc
			if _, err := store.PurgeScope(f.ctx, f.pool, q); !errors.Is(err, store.ErrPurgeScopeNotFound) {
				t.Errorf("tenant A on %q = %v, want ErrPurgeScopeNotFound", sc, err)
			}
		}
		q := tenantA
		q.Scope = s
		if res, err := store.PurgeScope(f.ctx, f.pool, q); err != nil || res.Counted.Blocks != 1 {
			t.Errorf("tenant A on its own scope = (%+v, %v), want one block", res, err)
		}
		if _, err := store.PurgeScope(f.ctx, f.pool, store.PurgeQuery{Scope: s}); !errors.Is(err, store.ErrPurgeScopeNotFound) {
			t.Errorf("no reader = %v, want ErrPurgeScopeNotFound", err)
		}
		for _, sc := range []string{"", "_global"} {
			q := f.admin()
			q.Scope = sc
			if _, err := store.PurgeScope(f.ctx, f.pool, q); !errors.Is(err, store.ErrPurgeScopeInvalid) {
				t.Errorf("server-admin on %q = %v, want ErrPurgeScopeInvalid", sc, err)
			}
		}
		q = f.admin()
		q.Scope = spBystander
		if res, err := store.PurgeScope(f.ctx, f.pool, q); err != nil || res.Counted.Blocks != 1 {
			t.Errorf("server-admin on a foreign tenant's scope = (%+v, %v), want one block", res, err)
		}
	})

	// More rows than one batch: the purge loops until the match is empty, and
	// the counts add up across batches.
	t.Run("multi_batch_drain", func(t *testing.T) {
		s := f.scope(t, "bulk")
		const n = 4500
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO context_blocks (category, title, content, scope)
			 SELECT 'learnings', 'bulk-' || g, 'bulk body ' || g, $1 FROM generate_series(1, $2) g`, s, n); err != nil {
			t.Fatalf("bulk blocks: %v", err)
		}
		if _, err := f.pool.Exec(f.ctx,
			`INSERT INTO context_dream_links (source_block_id, target_block_id, relationship, scope)
			 SELECT a.id, b.id, 'topical', $1
			   FROM (SELECT id, row_number() OVER (ORDER BY title) r FROM context_blocks WHERE scope = $1) a
			   JOIN (SELECT id, row_number() OVER (ORDER BY title) r FROM context_blocks WHERE scope = $1) b ON b.r = a.r + 1`, s); err != nil {
			t.Fatalf("bulk links: %v", err)
		}
		q := f.admin()
		q.Scope = s
		q.ConfirmBlocks = confirm(n)
		res, err := store.PurgeScope(f.ctx, f.pool, q)
		if err != nil {
			t.Fatalf("bulk purge: %v", err)
		}
		if res.Deleted.Blocks != n || res.Deleted.DreamLinks != n-1 {
			t.Fatalf("deleted = %+v, want %d blocks, %d links", *res.Deleted, n, n-1)
		}
		if left := f.blocksIn(t, s); left != 0 {
			t.Fatalf("%d blocks left after a multi-batch drain", left)
		}
	})
}
