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

// A stamp reset of an archived block's mapping answers the 404 a resolution
// gives there, and writes nothing — no NULL stamp, no hold on the leg.
func TestForgeResolveArchivedBlockStampReset(t *testing.T) {
	e := newFCMEnv(t)
	ctx := context.Background()
	proj := e.project(t, "rba")
	id := e.flag(t, proj, 1)
	if _, err := e.pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, id); err != nil {
		t.Fatal(err)
	}
	before := e.snapshot(t, []string{id}, []string{proj.ID})
	for _, data := range []map[string]any{
		{"project_id": proj.ID, "block_id": id, "direction": "remote"},
		{"project_id": proj.ID, "block_id": id},
	} {
		action := "forge-conflict-resolve"
		if _, ok := data["direction"]; !ok {
			action = "forge-stamp-reset"
		}
		if code, _, raw := e.do(t, e.h, e.adminA, action, data); code != http.StatusNotFound {
			t.Errorf("%s on an archived block: %d %s, want 404", action, code, raw)
		}
	}
	if after := e.snapshot(t, []string{id}, []string{proj.ID}); after != before {
		t.Errorf("a refused call on an archived block changed state:\nbefore %s\nafter  %s", before, after)
	}
}

// A project-wide reset rewrites only the mappings that have a stamp or a
// snapshot to drop, and answers the count it changed: a second reset leaves
// every row version and the cursor as they are.
func TestForgeResolveResetAllWithNothingToReset(t *testing.T) {
	e := newFCMEnv(t)
	ctx := context.Background()
	proj := e.project(t, "rbr")
	ids := []string{e.flag(t, proj, 1), e.flag(t, proj, 2), e.flag(t, proj, 3)}
	if code, body, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "all": true}); code != http.StatusOK || body["reset"] != float64(3) {
		t.Fatalf("the first reset: %d %s", code, raw)
	}
	versions := func() string {
		var v string
		if err := e.pool.QueryRow(ctx, `SELECT string_agg(xmin::text, ',' ORDER BY block_id) FROM context_project_sync_map WHERE block_id = ANY($1::uuid[])`, ids).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	v0, s0 := versions(), e.snapshot(t, ids, []string{proj.ID})
	code, body, raw := e.do(t, e.h, e.adminA, "forge-stamp-reset", map[string]any{"project_id": proj.ID, "all": true})
	if code != http.StatusOK || body["reset"] != float64(0) {
		t.Errorf("the second reset: %d %s, want reset 0", code, raw)
	}
	if v1 := versions(); v1 != v0 {
		t.Errorf("the second reset rewrote mapping rows: row versions %s -> %s", v0, v1)
	}
	if s1 := e.snapshot(t, ids, []string{proj.ID}); s1 != s0 {
		t.Errorf("the second reset changed state:\nbefore %s\nafter  %s", s0, s1)
	}
}
