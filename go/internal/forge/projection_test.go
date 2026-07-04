// Unit gates for the canonical hash projection (design/02 §3.6, W16). No DB —
// these run under `go test -short`. They rope off the two drift traps the I-G
// §7 gate names: the TIMESTAMP trap (updated_at must not enter the hash) and the
// TITLE-PREFIX trap (the "#<nr>" derivat must be stripped before hashing).
package forge

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// TestProjection_TimestampIndependent (W16): two payloads that differ ONLY in
// updated_at hash identically — a projection that folded the timestamp in would
// re-fire on every re-fetch (the idempotency trap).
func TestProjection_TimestampIndependent(t *testing.T) {
	iss := IssueRemote{Number: 1, Title: "T", Body: "B", State: "open"}
	a := iss
	a.UpdatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	b := iss
	b.UpdatedAt = time.Date(2026, 7, 4, 12, 0, 0, 0, time.UTC)
	ha, _, _ := ForgeIssueHash(a)
	hb, _, _ := ForgeIssueHash(b)
	if ha != hb {
		t.Fatalf("projection is timestamp-dependent (W16): %s != %s", ha, hb)
	}
}

// TestProjection_PrefixStripped (§3.6 drift-killer): a stored block titled
// "#12 Fix bug" projects to the SAME hash as the prefix-free forge title
// "Fix bug". A raw-block-title projection would make ctxH != forgeH forever.
func TestProjection_PrefixStripped(t *testing.T) {
	forgeH, _, _ := ForgeIssueHash(IssueRemote{Number: 12, Title: "Fix bug", Body: "x", State: "open"})
	b := &store.Block{
		Title: "#12 Fix bug", Content: "x", WorkflowStatus: "backlog",
		Metadata: map[string]any{"forge_state": "open"},
	}
	ctxH := CtxIssueHash(b, []string{"done"}, true)
	if ctxH != forgeH {
		t.Fatalf("prefix not stripped ⇒ drift: ctxH=%s forgeH=%s", ctxH, forgeH)
	}
	// The local draft prefix "#L7 " must strip too.
	b.Title = "#L7 Fix bug"
	if got := CtxIssueHash(b, []string{"done"}, true); got != forgeH {
		t.Fatalf("local #L prefix not stripped: %s != %s", got, forgeH)
	}
}

// TestProjection_LabelsAssigneesSorted: set order does not change the hash.
func TestProjection_LabelsAssigneesSorted(t *testing.T) {
	h1, _, _ := ForgeIssueHash(IssueRemote{Number: 1, Title: "T", State: "open",
		Labels: []string{"b", "a"}, Assignees: []string{"y", "x"}})
	h2, _, _ := ForgeIssueHash(IssueRemote{Number: 1, Title: "T", State: "open",
		Labels: []string{"a", "b"}, Assignees: []string{"x", "y"}})
	if h1 != h2 {
		t.Fatalf("label/assignee order changed the hash: %s != %s", h1, h2)
	}
}

// TestProjection_StateFallback: with the registry unresolvable (registryOK=false)
// the ctx state comes from metadata.forge_state — so a freshly-pulled block still
// projects to its forge state (§4.5.4 fail-safe), matching forgeH.
func TestProjection_StateFallback(t *testing.T) {
	forgeH, _, _ := ForgeIssueHash(IssueRemote{Number: 3, Title: "T", Body: "B", State: "closed"})
	b := &store.Block{Title: "#3 T", Content: "B", WorkflowStatus: "",
		Metadata: map[string]any{"forge_state": "closed"}}
	if got := CtxIssueHash(b, nil, false); got != forgeH {
		t.Fatalf("fallback state projection mismatch: %s != %s", got, forgeH)
	}
}

// TestProjection_TerminalMeansClosed: a terminal workflow_status projects to
// "closed", a non-terminal to "open" (§4.5.4 binary forge state).
func TestProjection_TerminalMeansClosed(t *testing.T) {
	closedH, _, _ := ForgeIssueHash(IssueRemote{Number: 4, Title: "T", State: "closed"})
	openH, _, _ := ForgeIssueHash(IssueRemote{Number: 4, Title: "T", State: "open"})
	done := &store.Block{Title: "#4 T", WorkflowStatus: "done", Metadata: map[string]any{"forge_state": "open"}}
	if got := CtxIssueHash(done, []string{"done"}, true); got != closedH {
		t.Fatalf("terminal status did not project to closed: %s != %s", got, closedH)
	}
	prog := &store.Block{Title: "#4 T", WorkflowStatus: "in-progress", Metadata: map[string]any{"forge_state": "closed"}}
	if got := CtxIssueHash(prog, []string{"done"}, true); got != openH {
		t.Fatalf("non-terminal status did not project to open: %s != %s", got, openH)
	}
}

// TestParseIssueRefs pins the §4.5.7 body parser: bare "#<nr>" tokens are refs;
// word-adjacent, entity and double-hash forms are NOT.
func TestParseIssueRefs(t *testing.T) {
	body := "see #12 and #34, not word#5, not &#39; entity, dup #12, ##7 nope, end #8"
	got := parseIssueRefs(body)
	want := map[int64]bool{12: true, 34: true, 8: true}
	if len(got) != len(want) {
		t.Fatalf("refs = %v, want %v", got, want)
	}
	for n := range want {
		if !got[n] {
			t.Fatalf("missing ref #%d in %v", n, got)
		}
	}
	for _, bad := range []int64{5, 39, 7} {
		if got[bad] {
			t.Fatalf("false ref #%d parsed from %q", bad, body)
		}
	}
}
