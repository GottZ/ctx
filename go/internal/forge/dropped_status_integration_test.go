//go:build integration

// Issue #491 point 4: a terminal workflow status that a change of the type's
// workflow dropped. It is no member of the new terminal set, so the block
// projected to "open" against the closed base: the next push of any local
// edit sent state=open and reopened the forge issue, and a remote edit of the
// closed issue read as a conflict.
//
// A status the resolved workflow does not carry decides nothing about the
// forge state any more: the block projects the state of its base (the last
// version both sides held). Reopening an issue the forge holds closed takes a
// local status change against that base — a move to a status of the workflow
// that is not terminal — and that still pushes state=open.
//
//	go test -tags=integration ./internal/forge/ -run TestDroppedStatus -count=1 -v
package forge

import (
	"encoding/json"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

// i491WorkflowWithoutWontfix is a later version of i486TenantWorkflow that
// no longer has wontfix.
const i491WorkflowWithoutWontfix = `{"v":1,"overview":{"include":false},"digest":{"include":false},
	"workflow":{"states":["triage","backlog","in-progress","review","done","duplicate"],
	            "initial":"triage","terminal":["done","duplicate"],
	            "forge_state_map":{"open":"triage","closed":"done"}},
	"structural_link_classes":["references","duplicate-of"]}`

// i491ClosedWithWontfix: the issue was closed locally with wontfix and the
// push closed it on the forge; then the tenant's workflow dropped wontfix.
func i491ClosedWithWontfix(t *testing.T, slug string) i491Env {
	t.Helper()
	e := i491Setup(t, slug, i486TenantWorkflow, fakeIssue{Title: "Crane", Body: "forge body"})
	e.move(t, e.id(t), "wontfix")
	if r := e.pushOn(t, e.pool); r.Pushed != 1 || e.gh.issue(t, 5).State != "closed" {
		t.Fatalf("precondition: the push of the local close: %+v, forge state %q", r, e.gh.issue(t, 5).State)
	}
	e.setTenantType(t, i491WorkflowWithoutWontfix)
	return e
}

func i491StateSent(p fakePatch) (string, bool) {
	raw, ok := p.Fields["state"]
	if !ok {
		return "", false
	}
	var s string
	_ = json.Unmarshal(raw, &s)
	return s, true
}

func TestDroppedStatusDoesNotReopenTheForgeIssue(t *testing.T) {
	t.Run("local title edit", func(t *testing.T) {
		e := i491ClosedWithWontfix(t, "i491droptitle")
		e.edit(t, store.IssueUpdate{Title: str("Crane, renamed")})
		landed := len(e.gh.landed())
		e.pushOn(t, e.pool)
		got := e.gh.landed()[landed:]
		if len(got) != 1 {
			t.Fatalf("the push landed %d PATCH(es), want 1 (the title)", len(got))
		}
		if st, ok := i491StateSent(got[0]); ok {
			t.Errorf("the PATCH sends state=%q: a status the workflow dropped must not reopen the forge issue", st)
		}
		if it := e.gh.issue(t, 5); it.State != "closed" || it.Title != "Crane, renamed" {
			t.Errorf("the forge holds %q / %s, want the title pushed and the issue closed", it.Title, it.State)
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
	})
	t.Run("local tags edit", func(t *testing.T) {
		e := i491ClosedWithWontfix(t, "i491droptags")
		e.edit(t, store.IssueUpdate{Tags: []string{"field-work"}})
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs after a tags-only edit landed %d PATCH(es), want 0", n)
		}
		if it := e.gh.issue(t, 5); it.State != "closed" {
			t.Errorf("the forge issue is %s, want closed", it.State)
		}
	})
	// A remote edit of the closed issue is a forge-ahead update, not a
	// conflict, and the pull writes the status the forge state maps to
	// (the dropped one cannot be moved by any local verb).
	t.Run("remote body edit", func(t *testing.T) {
		e := i491ClosedWithWontfix(t, "i491dropremote")
		e.gh.edit(t, 5, func(it *fakeIssue) { it.Body = "body edited on the forge" })
		if r := e.pullOn(t, e.pool); r.Applied != 1 || r.Conflicts != 0 {
			t.Fatalf("pull of the remote edit: %+v, want one applied, no conflict", r)
		}
		b := getBlock(t, e.pool, e.proj.Scope, e.id(t))
		if b.WorkflowStatus != "done" || b.Content != "body edited on the forge" {
			t.Errorf("after the pull: status %q body %q, want done and the remote body", b.WorkflowStatus, b.Content)
		}
		if n := e.cycles(t, 3); n != 0 {
			t.Errorf("three runs landed %d PATCH(es), want 0", n)
		}
	})
}

// TestDroppedStatusLocalReopenStillPushes: the wanted direction. A local
// move out of a terminal status to one the workflow carries and that is not
// terminal reopens the forge issue — with the builtin workflow and with a
// tenant's own one.
func TestDroppedStatusLocalReopenStillPushes(t *testing.T) {
	for _, tc := range []struct {
		name, tenantType, closedAs, reopenTo string
	}{
		{"builtin, done to backlog", "", "done", "backlog"},
		{"tenant, wontfix to review", i486TenantWorkflow, "wontfix", "review"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := i491Setup(t, "i491reopen", tc.tenantType, fakeIssue{Title: "Crane", Body: "forge body", State: "closed"})
			if tc.closedAs != "done" {
				e.move(t, e.id(t), tc.closedAs)
				if n := e.cycles(t, 1); n != 0 {
					t.Fatalf("precondition: a move within the closed state landed %d PATCH(es)", n)
				}
			}
			e.move(t, e.id(t), tc.reopenTo)
			landed := len(e.gh.landed())
			e.pushOn(t, e.pool)
			got := e.gh.landed()[landed:]
			if len(got) != 1 {
				t.Fatalf("the push of the local reopen landed %d PATCH(es), want 1", len(got))
			}
			if st, ok := i491StateSent(got[0]); !ok || st != "open" {
				t.Errorf("the PATCH sends state %q (present %v), want open", st, ok)
			}
			if it := e.gh.issue(t, 5); it.State != "open" {
				t.Errorf("the forge issue is %s, want open", it.State)
			}
			if n := e.cycles(t, 3); n != 0 {
				t.Errorf("three runs landed %d PATCH(es), want 0", n)
			}
		})
	}
}
