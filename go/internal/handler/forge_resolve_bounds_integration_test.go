//go:build integration

// Bounds of the forge resolve actions (issue #509): the spelling of a block
// id, an archived block, a project-wide reset with nothing to reset, and a
// project id the database cannot take as text.
//
//	go test -tags=integration ./internal/handler/ -run 'TestForgeResolve(BlockID|Archived|ResetAll|ProjectID)' -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// A block id in upper case names the same block: the resolution takes the
// push lock the push holds (the lock key was the text of the id as sent,
// so an upper-case id passed a push on the wire), and the audit row is keyed
// on the block id as the database spells it.
func TestForgeResolveBlockIDInUpperCase(t *testing.T) {
	e := newFCMEnv(t)
	ctx := context.Background()
	proj := e.project(t, "rbu")
	id := e.flag(t, proj, 1)
	upper := strings.ToUpper(id)
	conn, err := e.pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := store.TryPushLock(ctx, conn, id); err != nil || !ok {
		t.Fatalf("push lock: %v %v", ok, err)
	}
	if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": upper, "direction": "local"}); code != http.StatusConflict {
		t.Errorf("an upper-case block_id while a push holds the lock: %d %s, want 409", code, raw)
	}
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_unlock_all()`); err != nil {
		t.Fatal(err)
	}
	conn.Release()
	if code, _, raw := e.do(t, e.h, e.adminA, "forge-conflict-resolve", map[string]any{"project_id": proj.ID, "block_id": upper, "direction": "local"}); code != http.StatusOK {
		t.Fatalf("an upper-case block_id: %d %s", code, raw)
	}
	e.checkAudit(t, id, "resolve-local", proj.Scope)
	if code, body, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "block_id": upper}); code != http.StatusOK || body["reset"] != float64(1) {
		t.Fatalf("a stamp reset with an upper-case block_id: %d %s", code, raw)
	}
	if rows := e.auditRows(t, id); len(rows) != 2 || rows[1].action != "stamp-reset" {
		t.Errorf("audit rows keyed on the stored block id: %+v", rows)
	}
}
