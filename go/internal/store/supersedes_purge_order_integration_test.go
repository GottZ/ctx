//go:build integration

// Issue #284, point 2: a scope purge against a declaration of the same scope.
// The purge batch locks its blocks FOR UPDATE in scan order; a declaration
// locks its source (the write's identity, FOR UPDATE) and then its target
// (FOR NO KEY UPDATE). On a pair whose target the purge scans first, the
// purge holds the target and waits for the source while the declaration holds
// the source and waits for the target: PostgreSQL ends one of them with 40P01
// (the purge repeats its batch; a declaration that loses is a failed store).
// The declaration is driven by hand up to its target lock, with the
// production scope lock and the statements of store.lockSupersedesPair: the
// store function cannot be paused between its two row locks. It runs with a
// short deadlock_timeout, so if the cycle forms it is the declaration that
// detects it.
//
//	go test -tags=integration -p 1 ./internal/store/ -run TestSupersedesPurgeAndDeclarationOfOneScope -count=1 -v
package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/store"
)

func TestSupersedesPurgeAndDeclarationOfOneScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := spSetup(t)
	s := f.scope(t, "decl-order")
	tgt := f.block(t, s, "purge-target", []string{"import"}) // first in scan order
	src := f.block(t, s, "purge-source", []string{"import"})

	q := f.admin()
	q.Scope = s
	q.Filter = store.PurgeFilter{Tags: []string{"import"}}
	dry, err := store.PurgeScope(f.ctx, f.pool, q)
	if err != nil || dry.Counted.Blocks != 2 {
		t.Fatalf("dry run = (%+v, %v), want blocks 2", dry, err)
	}

	decl, err := f.pool.Begin(f.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = decl.Rollback(context.Background()) }()
	if _, err := decl.Exec(f.ctx, `SET LOCAL deadlock_timeout = '100ms'`); err != nil {
		t.Fatalf("deadlock_timeout: %v", err)
	}
	if err := store.LockSupersedesScope(f.ctx, decl, s); err != nil {
		t.Fatalf("scope lock: %v", err)
	}
	if _, err := decl.Exec(f.ctx,
		`SELECT id FROM context_blocks WHERE category = 'learnings' AND title = 'purge-source' AND scope = $1 AND NOT is_archived FOR UPDATE`, s); err != nil {
		t.Fatalf("identity lock: %v", err)
	}

	cfg := f.pool.Config().Copy()
	cfg.MaxConns = 1
	cfg.ConnConfig.RuntimeParams["application_name"] = "p2-purge"
	actor, err := pgxpool.NewWithConfig(f.ctx, cfg)
	if err != nil {
		t.Fatalf("actor pool: %v", err)
	}
	t.Cleanup(actor.Close)
	q.ConfirmBlocks = confirm(dry.Counted.Blocks)
	purged := make(chan error, 1)
	go func() {
		_, err := store.PurgeScope(f.ctx, actor, q)
		purged <- err
	}()
	for deadline := time.Now().Add(15 * time.Second); ; {
		var n int
		if err := f.pool.QueryRow(f.ctx,
			`SELECT count(*)::int FROM pg_stat_activity
			  WHERE application_name = 'p2-purge' AND datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("poll: %v", err)
		}
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the purge never waited for the declaration")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// The declaration's target lock, then its commit.
	wctx, cancel := context.WithTimeout(f.ctx, 30*time.Second)
	defer cancel()
	if _, err := decl.Exec(wctx,
		`SELECT 1 FROM context_blocks WHERE id = $1::uuid AND scope = $2 AND NOT is_archived FOR NO KEY UPDATE`, tgt, s); err != nil {
		t.Errorf("declaration target lock: %v", err)
	} else if err := decl.Commit(f.ctx); err != nil {
		t.Errorf("declaration commit: %v", err)
	}
	select {
	case err := <-purged:
		if err != nil {
			t.Errorf("purge: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("the purge did not finish")
	}
	if !t.Failed() && (f.exists(t, tgt) || f.exists(t, src)) {
		t.Errorf("purge left blocks of scope %s behind", s)
	}
}
