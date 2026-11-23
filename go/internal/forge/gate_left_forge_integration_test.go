//go:build integration

// Issue #509, the follow-up list of 2026-11-23: a push gate whose entity the
// forge no longer lists. A remote resolution keeps the mapping out of the
// push until a pull decides over the entity (synced_at 'infinity'); an issue
// deleted on the forge, or transferred to another repository, is never
// listed again, so the gate held for good: a local edit was never pushed,
// no list showed the row, and a resolution answered changed:false. A local
// resolution of a gated row now opens the gate — with the locks and the
// audit row of every resolution — and the push diffs the block against the
// base the remote resolution wrote, as for any mapping. What the forge then
// answers for an entity that left it is the push's answer for any mapping
// of such an entity (executed below and logged: 404 for a deleted issue, a
// refused 301 for a transferred one).
//
//	go test -tags=integration ./internal/forge/ -run TestAGateOverAnIssueThatLeftTheForge -count=1 -v
package forge

import (
	"context"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

func TestAGateOverAnIssueThatLeftTheForge(t *testing.T) {
	const later = "body edited locally after the issue left the forge"
	for _, tc := range []struct{ name, movedTo string }{
		{"deleted", ""},
		{"transferred", "/repos/o/elsewhere/issues/1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := i509Diverged(t, "glf"+tc.name[:1], true)
			if r := e.resolve(t, ResolveRemote); !r.Changed {
				t.Fatalf("resolve remote: %+v", r)
			}
			e.gh.remove(t, 5, tc.movedTo)
			e.edit(t, store.IssueUpdate{Content: str(later)})
			n := len(e.gh.landed())
			for run := 1; run <= 2; run++ {
				e.i509MustRun(t)
				if r := e.pushOn(t, e.pool); r.Pushed != 0 {
					t.Fatalf("run %d: the push sent %+v behind the gate", run, r)
				}
			}
			gated, err := store.GatedCount(ctx, e.pool, e.proj.ID, e.proj.Scope)
			if err != nil || gated != 1 || len(e.gh.landed()) != n {
				t.Fatalf("two runs after the issue left the forge: gated %d %v, PATCHes %d — want the gate counted and nothing sent", gated, err, len(e.gh.landed())-n)
			}
			gl, _, err := e.resolverOn(e.pool).ListGated(ctx, e.proj, "", 0)
			if err != nil || len(gl) != 1 || gl[0].BlockID != e.id(t) || !gl[0].Gated {
				t.Fatalf("the gated list: %+v %v", gl, err)
			}
			if r := e.resolve(t, ResolveRemote); r.Changed {
				t.Errorf("a remote resolution of the gated row changed it: %+v", r)
			}
			r, err := e.resolverOn(e.pool).ResolveConflict(ctx, e.proj, e.id(t), ResolveLocal, gl[0].Flag, store.AuditStamp{})
			if err != nil || !r.Changed || r.Basis != "gate" {
				t.Fatalf("resolve local of the gated row: %+v %v — want the gate opened", r, err)
			}
			if gated, err := store.GatedCount(ctx, e.pool, e.proj.ID, e.proj.Scope); err != nil || gated != 0 {
				t.Errorf("after the local resolution: gated %d %v", gated, err)
			}
			cands, err := store.ListIssuePushCandidates(ctx, e.pool, e.proj.ID, 10)
			if err != nil || len(cands) != 1 || cands[0].Block.ID != e.id(t) {
				t.Errorf("the push candidates after the gate opened: %+v %v — want the edited block", cands, err)
			}
			var audit int
			if err := e.pool.QueryRow(ctx, `SELECT count(*) FROM context_settings_audit WHERE entity_type = 'forge_sync' AND entity_key = $1 AND action = 'resolve-local' AND new_value->>'basis' = 'gate'`, e.id(t)).Scan(&audit); err != nil || audit != 1 {
				t.Errorf("audit rows of the local resolution of the gate: %d %v, want 1", audit, err)
			}
			// What the forge answers the push for an entity that left it is
			// the answer for any such mapping (not decided here): logged.
			res, err := NewPusher(e.pool, e.reg.SnapshotForTenant).PushProject(ctx, e.proj, e.gh.client(), e.gh.repo(), alwaysAllow)
			t.Logf("the push after the gate opened (%s): %+v, error %v", tc.name, res, err)
		})
	}
}
