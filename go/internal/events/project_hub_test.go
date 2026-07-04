package events

// Unit tests for the projectHub fan-out core (workflow W9). These run under
// `go test -short` — NO database: resolveProject is pre-seeded into the cache so
// Dispatch never touches the (nil) pool, and flush() is driven manually instead
// of by the timed loop. The DB-backed paths (trigger payloads, live LISTEN
// end-to-end, cache re-resolve after create) live in the integration suite.

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/config"
)

// newTestHub builds a hub WITHOUT the timed flush loop (the test drives flush()
// directly) and with a real zero-config store (the <=0 guards supply defaults).
func newTestHub() *ProjectHub {
	return &ProjectHub{
		cfg:     config.NewStore(&config.Config{}),
		life:    context.Background(),
		subs:    map[*projectSub]struct{}{},
		perTn:   map[string]int{},
		cache:   map[string]cachedProject{},
		pending: map[string]*pendingScope{},
	}
}

// seedProject pre-populates the scope→project cache so Dispatch resolves without
// the pool (id=="" seeds a NEGATIVE / non-project entry).
func (h *ProjectHub) seedProject(scope, projectID string) {
	h.cache[scope] = cachedProject{id: projectID, at: time.Now()}
}

// drain returns every frame currently buffered on a sub (non-blocking).
func drain(s *projectSub) []ProjectFrame {
	var out []ProjectFrame
	for {
		select {
		case f := <-s.ch:
			out = append(out, f)
		default:
			return out
		}
	}
}

// TestProjectHubScopeFanoutIsolation is the unit-level two-tenant absence probe:
// a write in scope B reaches the B subscriber and NEVER the A subscriber.
func TestProjectHubScopeFanoutIsolation(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	h.seedProject("b:repo", "proj-b")

	subA, ok := h.Subscribe("tenant-a", false, []string{"a:repo"}, 8)
	if !ok {
		t.Fatal("subscribe A rejected")
	}
	subB, ok := h.Subscribe("tenant-b", false, []string{"b:repo"}, 8)
	if !ok {
		t.Fatal("subscribe B rejected")
	}

	// A write in scope B only.
	h.Dispatch(`{"id":"blk-1","op":"INSERT","scope":"b:repo","type":"issue"}`)
	h.flush()

	if got := drain(subA); len(got) != 0 {
		t.Fatalf("A LEAK: subscriber on tenant A received %d frames for a tenant-B write: %+v", len(got), got)
	}
	bFrames := drain(subB)
	if len(bFrames) != 1 || bFrames[0].ProjectID != "proj-b" {
		t.Fatalf("B miss: want 1 frame for proj-b, got %+v", bFrames)
	}
}

// TestProjectHubPerTenantCap proves the cap is counted PER TENANT: tenant A
// saturating its ceiling does not stop tenant B from connecting. A global
// counter (the RED design) would reject B.
func TestProjectHubPerTenantCap(t *testing.T) {
	h := newTestHub()
	const cap = 2

	if _, ok := h.Subscribe("tenant-a", false, nil, cap); !ok {
		t.Fatal("A #1 rejected")
	}
	if _, ok := h.Subscribe("tenant-a", false, nil, cap); !ok {
		t.Fatal("A #2 rejected")
	}
	if _, ok := h.Subscribe("tenant-a", false, nil, cap); ok {
		t.Fatal("A #3 admitted — per-tenant cap not enforced")
	}
	// B still connects — RED with a global counter (A already holds `cap`).
	if _, ok := h.Subscribe("tenant-b", false, nil, cap); !ok {
		t.Fatal("B rejected — cap is GLOBAL, not per-tenant (regression)")
	}
}

// TestProjectHubCoalesceBurst proves a burst collapses to ONE content-free
// issues-bulk frame instead of O(writes) id frames (§6.2 — the 10k-import
// storm defense at the hub).
func TestProjectHubCoalesceBurst(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	sub, _ := h.Subscribe("tenant-a", false, []string{"a:repo"}, 8)

	const writes = 500 // far above coalesce_threshold default 20
	for i := 0; i < writes; i++ {
		h.Dispatch(`{"id":"blk-` + strconv.Itoa(i) + `","op":"UPDATE","scope":"a:repo","type":"issue"}`)
	}
	h.flush()

	frames := drain(sub)
	if len(frames) != 1 {
		t.Fatalf("burst produced %d frames, want 1 coalesced (frame flood)", len(frames))
	}
	f := frames[0]
	if f.Kind != "issues-bulk" {
		t.Fatalf("burst frame kind=%q, want issues-bulk", f.Kind)
	}
	if len(f.BlockIDs) != 0 {
		t.Fatalf("issues-bulk frame carries %d block_ids, want 0 (refetch signal)", len(f.BlockIDs))
	}
	if f.Count != writes {
		t.Fatalf("issues-bulk count=%d, want %d", f.Count, writes)
	}
}

// TestProjectHubNormalIDFrame proves a small write set emits an id-list frame
// (not coalesced), grouped by (kind, op).
func TestProjectHubNormalIDFrame(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	sub, _ := h.Subscribe("tenant-a", false, []string{"a:repo"}, 8)

	h.Dispatch(`{"id":"blk-1","op":"INSERT","scope":"a:repo","type":"issue"}`)
	h.Dispatch(`{"id":"blk-2","op":"INSERT","scope":"a:repo","type":"issue"}`)
	h.flush()

	frames := drain(sub)
	if len(frames) != 1 {
		t.Fatalf("want 1 grouped frame, got %d: %+v", len(frames), frames)
	}
	f := frames[0]
	if f.Kind != "issue" || f.Op != "INSERT" || len(f.BlockIDs) != 2 {
		t.Fatalf("frame = %+v, want kind=issue op=INSERT 2 ids", f)
	}
}

// TestProjectHubBulkDeleteFrame proves a prune/DELETE (bulk:true) emits a
// content-free issues-bulk frame even for a single scope.
func TestProjectHubBulkDeleteFrame(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	sub, _ := h.Subscribe("tenant-a", false, []string{"a:repo"}, 8)

	h.Dispatch(`{"op":"DELETE","scope":"a:repo","type":"issue","bulk":true}`)
	h.flush()

	frames := drain(sub)
	if len(frames) != 1 || frames[0].Kind != "issues-bulk" {
		t.Fatalf("delete frame = %+v, want one issues-bulk", frames)
	}
}

// TestProjectHubNonProjectScopeDropped proves the whole knowledge corpus (a
// scope with no project) never produces a frame (negative-cached).
func TestProjectHubNonProjectScopeDropped(t *testing.T) {
	h := newTestHub()
	h.seedProject("private", "") // negative entry: not a project scope
	sub, _ := h.Subscribe("tenant-a", true, nil, 8) // global sub sees ALL project frames

	h.Dispatch(`{"id":"blk-1","op":"UPDATE","scope":"private","type":"knowledge"}`)
	h.flush()

	if got := drain(sub); len(got) != 0 {
		t.Fatalf("non-project scope produced %d frames, want 0", len(got))
	}
}

// TestProjectHubFramesIdsOnly is the content-leak guard (K16): a serialized frame
// carries ONLY {kind, project_id, op, block_ids} — never content/title/body. The
// RED probe: the ProjectFrame struct has no content field, so this key-set
// assertion breaks the moment one is added.
func TestProjectHubFramesIdsOnly(t *testing.T) {
	f := ProjectFrame{Kind: "issue", ProjectID: "p1", Op: "UPDATE", BlockIDs: []string{"blk-1"}}
	b, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{"kind": true, "project_id": true, "op": true, "block_ids": true, "count": true}
	for k := range m {
		if !allowed[k] {
			t.Fatalf("frame carries disallowed key %q (potential content leak): %s", k, b)
		}
	}
	for _, forbidden := range []string{"content", "title", "body"} {
		if _, ok := m[forbidden]; ok {
			t.Fatalf("frame leaks %q: %s", forbidden, b)
		}
	}
}

// TestProjectHubInvalidateProjects proves InvalidateProjects wipes the cache
// (create/delete write-path hook).
func TestProjectHubInvalidateProjects(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	if _, ok := h.resolveProject("a:repo"); !ok {
		t.Fatal("seed not resolvable")
	}
	h.InvalidateProjects()
	h.mu.Lock()
	n := len(h.cache)
	h.mu.Unlock()
	if n != 0 {
		t.Fatalf("cache not cleared: %d entries remain", n)
	}
}

// TestProjectHubSlowConsumerDropped proves a sub whose mailbox overflows is
// dropped (done closed) so it cannot stall the fan-out to others.
func TestProjectHubSlowConsumerDropped(t *testing.T) {
	h := newTestHub()
	h.seedProject("a:repo", "proj-a")
	sub, _ := h.Subscribe("tenant-a", false, []string{"a:repo"}, 8)

	// Fill the mailbox past capacity WITHOUT draining, each in its own flush.
	for i := 0; i < projectSubMailbox+5; i++ {
		h.Dispatch(`{"id":"blk-` + strconv.Itoa(i) + `","op":"INSERT","scope":"a:repo","type":"issue"}`)
		h.flush()
	}
	select {
	case <-sub.done:
		// dropped, as expected
	default:
		t.Fatal("slow consumer not dropped after mailbox overflow")
	}
}
