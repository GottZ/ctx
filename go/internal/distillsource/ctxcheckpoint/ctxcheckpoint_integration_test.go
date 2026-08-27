//go:build integration

// Gate A02-3, fixture half: watermark group completeness, double listing,
// part-1 invariant and the read semantics that only a real Postgres can show.
// The scale and live halves live in the two sibling files.

package ctxcheckpoint_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/distillsource"
	"github.com/GottZ/ctx/internal/distillsource/ctxcheckpoint"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	fxScope    = "private"
	fxCategory = "compaction-checkpoints"
	fxRoot     = "20260712_205012_837f2c"
)

// fxBoilerplate mirrors the live head: both marker strings the boilerplate gate
// checks for, in front of the transcript marker.
const fxBoilerplate = "# Compaction checkpoint " + fxRoot + " part %d\n\n" +
	"## Compaction source evidence\n\n" +
	"- Transcript SHA-256: 6f1c2d3e4a5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f9\n" +
	"- Source blocks: %d\n\n" +
	"## Direct transcript\n\n"

func fxOpts() ctxcheckpoint.Options {
	return ctxcheckpoint.Options{
		Label:        "ctx-checkpoint",
		Scope:        fxScope,
		Category:     fxCategory,
		MaxSessions:  4,
		MaxManifests: 2,
	}
}

// insertPart writes one part block and returns its id.
func insertPart(t *testing.T, ctx context.Context, pool *pgxpool.Pool, root, title, body string, at time.Time) string {
	t.Helper()
	content := fmt.Sprintf(fxBoilerplate, 1, 1) + body
	var id string
	err := pool.QueryRow(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, $3, $4, 'checkpoint',
        jsonb_build_object('root_session_id', $5::text, 'part', '1'), $6)
RETURNING id::text`, fxCategory, title, content, fxScope, root, at).Scan(&id)
	if err != nil {
		t.Fatalf("insert part %q: %v", title, err)
	}
	return id
}

// insertManifest writes one manifest listing partIDs in order.
func insertManifest(t *testing.T, ctx context.Context, pool *pgxpool.Pool, root, title string, partIDs []string, at time.Time) string {
	t.Helper()
	var id string
	err := pool.QueryRow(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, $2, 'manifest', $3, 'checkpoint',
        jsonb_build_object('root_session_id', $4::text,
                           'source_block_ids', to_jsonb($5::text[])), $6)
RETURNING id::text`, fxCategory, title, fxScope, root, partIDs, at).Scan(&id)
	if err != nil {
		t.Fatalf("insert manifest %q: %v", title, err)
	}
	return id
}

func wmOf(at time.Time) int64 { return at.UnixMicro() }

// TestWatermarkGroupCompleteness is the group gate.
//
// Three manifests, the second and third sharing one microsecond, read through a
// window of two. A naive keyset would deliver M1+M2, advance to their shared
// watermark and lose M3 for good, because the next read starts strictly above
// it. The reader instead ends in front of the group and says so.
func TestWatermarkGroupCompleteness(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
	collide := t0.Add(time.Hour) // M2 and M3 share this microsecond exactly

	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\nalpha", t0)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — assistant\nbeta", collide)
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\ngamma", collide)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, collide)
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, collide)

	src, err := ctxcheckpoint.New(pool, fxOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if b.Complete {
		t.Error("Complete = true, want false — the batch ended inside a watermark group")
	}
	if want := wmOf(t0); b.Watermark != want {
		t.Errorf("Watermark = %d, want %d (in front of the collided group)", b.Watermark, want)
	}
	if len(b.Items) != 1 || !strings.Contains(b.Items[0].Text, "alpha") {
		t.Fatalf("batch 1 delivered %d items, want only M1's", len(b.Items))
	}

	// The load-bearing half: continuing above the reported watermark must still
	// see BOTH members of the group.
	b2, err := src.Read(ctx, fxRoot, b.Watermark, 100, 4000)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	var got []string
	for _, it := range b2.Items {
		got = append(got, strings.TrimSpace(it.Text))
	}
	joined := strings.Join(got, "|")
	if !strings.Contains(joined, "beta") || !strings.Contains(joined, "gamma") {
		t.Errorf("batch 2 lost a member of the collided group: %q", joined)
	}
	if b2.Watermark != wmOf(collide) {
		t.Errorf("batch 2 watermark = %d, want %d", b2.Watermark, wmOf(collide))
	}
}

// TestWatermarkGroupWiderThanWindow: a group that fills the entire window is
// taken whole. Shrinking it away would leave nothing to read and no way to move
// the watermark — a stall no configuration could clear.
func TestWatermarkGroupWiderThanWindow(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	at := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\nalpha", at)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — user\nbeta", at)
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\ngamma", at)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, at)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, at)
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, at)

	opt := fxOpts()
	opt.MaxManifests = 1 // narrower than the group
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 3 {
		t.Errorf("got %d items, want 3 — the group must be taken whole", len(b.Items))
	}
	if !b.Complete {
		t.Error("Complete = false, want true — the whole group was delivered")
	}
	if b.Watermark != wmOf(at) {
		t.Errorf("Watermark = %d, want %d", b.Watermark, wmOf(at))
	}
}

// TestItemCapStopIsStillComplete: a batch that stops at the item cap is
// COMPLETE, even when a watermark group was cut at the far end of the window.
// Its watermark covers whole atoms, and the next read starts well below the cut
// group and meets it in full. Reporting incomplete here would make every batch
// incomplete whenever a group happens to sit at the window edge, and the arm
// would never advance.
func TestItemCapStopIsStillComplete(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 7, 10, 0, 0, 0, time.UTC)
	collide := t0.Add(3 * time.Hour)

	// M1 and M2 at distinct watermarks, M3+M4 sharing one at the far end.
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\n"+strings.Repeat("a", 300), t0)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — user\n"+strings.Repeat("b", 300), t0.Add(time.Hour))
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\n"+strings.Repeat("c", 300), collide)
	p4 := insertPart(t, ctx, pool, fxRoot, "p4", "### Message 4 — user\n"+strings.Repeat("d", 300), collide)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, t0.Add(time.Hour))
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, collide)
	insertManifest(t, ctx, pool, fxRoot, "m4", []string{p4}, collide)

	opt := fxOpts()
	opt.MaxManifests = 3 // window ends inside the collided group
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// maxItems=1 makes the loop stop right after M1, long before the cut group.
	b, err := src.Read(ctx, fxRoot, 0, 1, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !b.Complete {
		t.Error("Complete = false — a cap stop before the cut group must stay complete")
	}
	if b.Watermark != wmOf(t0) {
		t.Errorf("Watermark = %d, want %d (M1)", b.Watermark, wmOf(t0))
	}
	if len(b.Items) != 1 {
		t.Errorf("got %d items, want 1", len(b.Items))
	}

	// Walking the window to its end must still report the cut.
	b2, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read 2: %v", err)
	}
	if b2.Complete {
		t.Error("Complete = true on a full-window read that ends inside a group")
	}
	if b2.Watermark != wmOf(t0.Add(time.Hour)) {
		t.Errorf("Watermark = %d, want %d (M2, in front of the group)",
			b2.Watermark, wmOf(t0.Add(time.Hour)))
	}
}

// TestItemCapNeverCutsAWatermarkGroup_WindowPath is the review's probe A, kept
// as a permanent test.
//
// Two manifests share one microsecond and a third follows later, with a window
// wide enough to hold all three — so manifestHeads reports NO cut and its
// end-of-window protection never engages. With maxItems=1 the loop stops after
// the first manifest, and the watermark then already stands on a group whose
// second member was never delivered. The next read starts strictly above it,
// so that member is unreachable for good, and Complete=true tells the caller
// nothing is wrong.
//
// The fix delivers a group to its end before honouring the cap, which is the
// same rule watermarkGroup already applies at the other edge.
func TestItemCapNeverCutsAWatermarkGroup_WindowPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 8, 10, 0, 0, 0, time.UTC)
	later := t0.Add(time.Hour)

	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\nAAA", t0)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — user\nBBB", t0)
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\nCCC", later)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, t0) // same microsecond as m1
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, later)

	opt := fxOpts()
	opt.MaxManifests = 3 // the window holds the whole group: no cut is reported
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	delivered := drainAll(t, ctx, src, fxRoot, 1, 4000)
	for _, want := range []string{"AAA", "BBB", "CCC"} {
		if !strings.Contains(delivered, want) {
			t.Errorf("material %q was never delivered across the batches; got %q", want, delivered)
		}
	}
}

// TestItemCapNeverCutsAWatermarkGroup_GroupPath is the review's probe B.
//
// Three manifests on one microsecond with MaxManifests=1: manifestHeads cannot
// shrink the group away, so watermarkGroup deliberately loads it whole to avoid
// a stall — and then the item cap cut exactly that group. The second read
// returned zero items while two of three manifests were already lost.
func TestItemCapNeverCutsAWatermarkGroup_GroupPath(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	at := time.Date(2026, 8, 9, 10, 0, 0, 0, time.UTC)
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\nAAA", at)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — user\nBBB", at)
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\nCCC", at)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, at)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, at)
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, at)

	opt := fxOpts()
	opt.MaxManifests = 1 // forces the watermarkGroup path
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	delivered := drainAll(t, ctx, src, fxRoot, 1, 4000)
	for _, want := range []string{"AAA", "BBB", "CCC"} {
		if !strings.Contains(delivered, want) {
			t.Errorf("material %q was never delivered across the batches; got %q", want, delivered)
		}
	}
}

// TestItemCapNeverCutsAWatermarkGroup_GroupInTheMiddle puts the collided group
// in the MIDDLE of the delivered set rather than at either edge, with a cap
// small enough to stop inside it. Neither the end-of-window guard nor the
// watermarkGroup path applies here — only the cap rule itself.
func TestItemCapNeverCutsAWatermarkGroup_GroupInTheMiddle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	mid := t0.Add(time.Hour)
	last := t0.Add(2 * time.Hour)

	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\nAAA", t0)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", "### Message 2 — user\nBBB", mid)
	p3 := insertPart(t, ctx, pool, fxRoot, "p3", "### Message 3 — user\nCCC", mid)
	p4 := insertPart(t, ctx, pool, fxRoot, "p4", "### Message 4 — user\nDDD", last)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0)
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{p2}, mid) // group, middle
	insertManifest(t, ctx, pool, fxRoot, "m3", []string{p3}, mid) // group, middle
	insertManifest(t, ctx, pool, fxRoot, "m4", []string{p4}, last)

	opt := fxOpts()
	opt.MaxManifests = 4
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	delivered := drainAll(t, ctx, src, fxRoot, 1, 4000)
	for _, want := range []string{"AAA", "BBB", "CCC", "DDD"} {
		if !strings.Contains(delivered, want) {
			t.Errorf("material %q was never delivered across the batches; got %q", want, delivered)
		}
	}
}

// drainAll reads a session to exhaustion the way an arm would — advancing to
// Batch.Watermark and never to a value of its own — and returns everything it
// received. A watermark that covers material the batch did not deliver shows up
// here as missing text, which is exactly the silent loss under test.
func drainAll(t *testing.T, ctx context.Context, src *ctxcheckpoint.Source, sess string, maxItems, maxRunes int) string {
	t.Helper()
	var got []string
	after := int64(0)
	for round := range 12 {
		b, err := src.Read(ctx, sess, after, maxItems, maxRunes)
		if err != nil {
			t.Fatalf("Read round %d: %v", round+1, err)
		}
		for _, it := range b.Items {
			got = append(got, strings.TrimSpace(it.Text))
		}
		if !b.Complete {
			// The contract forbids advancing past an incomplete batch; an arm
			// would retry the same range, so draining stops here.
			t.Logf("round %d incomplete at watermark %d", round+1, b.Watermark)
		}
		if b.Watermark == after {
			break
		}
		after = b.Watermark
	}
	return strings.Join(got, "|")
}

// TestDoubleListedPartIsDeliveredTwice is the double-listing gate.
//
// One part listed by two manifests is read twice and the reader does not fall
// over. Dropping the repeat is the ledger's job (A02-6); a reader that
// deduplicated here would hide from the ledger the very load it is measured on.
func TestDoubleListedPartIsDeliveredTwice(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 3, 10, 0, 0, 0, time.UTC)
	shared := insertPart(t, ctx, pool, fxRoot, "shared", "### Message 1 — user\nshared body", t0)
	other := insertPart(t, ctx, pool, fxRoot, "other", "### Message 2 — assistant\nother body", t0.Add(time.Minute))
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{shared}, t0.Add(2*time.Minute))
	insertManifest(t, ctx, pool, fxRoot, "m2", []string{shared, other}, t0.Add(3*time.Minute))

	src, err := ctxcheckpoint.New(pool, fxOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}

	seen := 0
	for _, it := range b.Items {
		if it.Origin.BlockID == shared {
			seen++
		}
	}
	if seen != 2 {
		t.Errorf("shared part delivered %d times, want 2", seen)
	}
	if len(b.Items) != 3 {
		t.Errorf("got %d items, want 3 (shared twice + other once)", len(b.Items))
	}
}

// TestReadCarriesRoleAcrossParts is the carry gate against a real manifest:
// part 2 has no header and must inherit part 1's attribution.
func TestReadCarriesRoleAcrossParts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 4, 10, 0, 0, 0, time.UTC)
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 12 — assistant\n"+strings.Repeat("a", 200), t0)
	p2 := insertPart(t, ctx, pool, fxRoot, "p2", strings.Repeat("b", 200), t0.Add(time.Second))
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1, p2}, t0.Add(time.Minute))

	src, err := ctxcheckpoint.New(pool, fxOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(b.Items))
	}
	for i, it := range b.Items {
		if it.Origin.Role != "assistant" || it.Origin.Ordinal != 12 {
			t.Errorf("item %d attribution = %d/%q, want 12/assistant",
				i+1, it.Origin.Ordinal, it.Origin.Role)
		}
		if it.Origin.ChunkIndex != 1 {
			t.Errorf("item %d chunk index = %d, want 1", i+1, it.Origin.ChunkIndex)
		}
		for _, marker := range []string{"Compaction source evidence", "Transcript SHA-256"} {
			if strings.Contains(it.Text, marker) {
				t.Errorf("item %d still carries boilerplate marker %q", i+1, marker)
			}
		}
	}
	if b.Items[0].Origin.BlockID != p1 || b.Items[1].Origin.BlockID != p2 {
		t.Error("parts were not delivered in source_block_ids order")
	}
}

// TestSourceContractDefaults pins the classification the contract calls
// fail-closed and the invariants a caller may rely on without checking.
func TestSourceContractDefaults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 5, 10, 0, 0, 0, time.UTC)
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\n"+strings.Repeat("a", 300), t0)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0.Add(time.Minute))

	src, err := ctxcheckpoint.New(pool, fxOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if src.Label() != "ctx-checkpoint" {
		t.Errorf("Label = %q", src.Label())
	}
	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(b.Items) != 1 {
		t.Fatalf("got %d items, want 1", len(b.Items))
	}
	it := b.Items[0]
	if it.Sensitivity != "credentials" {
		t.Errorf("Sensitivity = %q, want credentials (fail-closed)", it.Sensitivity)
	}
	if !it.Untrusted {
		t.Error("Untrusted = false, want true")
	}
	if it.Truncated {
		t.Error("Truncated = true — this source splits, it never head-caps")
	}
	if len(it.Attrs) != 2 || it.Attrs[0].Name != "block" || it.Attrs[1].Name != "chunk" {
		t.Errorf("Attrs = %+v, want code-owned block/chunk", it.Attrs)
	}

	// Non-positive caps yield an empty, incomplete batch — never a
	// source-chosen default.
	if eb, err := src.Read(ctx, fxRoot, 0, 0, 4000); err != nil || len(eb.Items) != 0 || eb.Complete {
		t.Errorf("Read with maxItems=0 = (%+v, %v), want empty incomplete batch", eb, err)
	}
	if eb, err := src.Read(ctx, fxRoot, 0, 100, 0); err != nil || len(eb.Items) != 0 || eb.Complete {
		t.Errorf("Read with maxRunes=0 = (%+v, %v), want empty incomplete batch", eb, err)
	}

	// HasNew / Head / QuietFor over the same rows.
	if ok, err := src.HasNew(ctx, fxRoot, 0); err != nil || !ok {
		t.Errorf("HasNew(0) = (%v, %v), want true", ok, err)
	}
	if ok, err := src.HasNew(ctx, fxRoot, b.Watermark); err != nil || ok {
		t.Errorf("HasNew(head) = (%v, %v), want false", ok, err)
	}
	if head, err := src.Head(ctx, fxRoot); err != nil || head != b.Watermark {
		t.Errorf("Head = (%d, %v), want %d", head, err, b.Watermark)
	}
	if head, err := src.Head(ctx, "no-such-root"); err != nil || head != 0 {
		t.Errorf("Head of unknown session = (%d, %v), want (0, nil)", head, err)
	}

	// The newest live block of the root is the manifest at t0+1m, not the part
	// at t0 — QuietFor measures over every live block, because a part written
	// seconds ago answers "is someone still working here" just as well.
	now := t0.Add(90 * time.Minute)
	d, err := src.QuietFor(ctx, fxRoot, now)
	if err != nil || d != 89*time.Minute {
		t.Errorf("QuietFor = (%v, %v), want 89m", d, err)
	}
	// A clock regression must not read as negative idle time.
	if d, err := src.QuietFor(ctx, fxRoot, t0.Add(-time.Hour)); err != nil || d != 0 {
		t.Errorf("QuietFor with a past now = (%v, %v), want 0", d, err)
	}
	if _, err := src.QuietFor(ctx, "no-such-root", now); err == nil {
		t.Error("QuietFor of an unknown session returned nil error, want ErrNoActiveRows")
	} else if !isNoActiveRows(err) {
		t.Errorf("QuietFor error = %v, want ErrNoActiveRows", err)
	}
	if err := src.Close(); err != nil {
		t.Errorf("Close = %v", err)
	}
	// Close borrows the pool and must leave it usable.
	if _, err := src.Head(ctx, fxRoot); err != nil {
		t.Errorf("pool unusable after Close: %v", err)
	}
}

func isNoActiveRows(err error) bool {
	return err != nil && strings.Contains(err.Error(), distillsource.ErrNoActiveRows.Error())
}

// TestSessionsRanksByManifestAndHonoursHorizon covers the candidate query: it
// ranks by newest MANIFEST, ignores archived rows and other scopes, and the
// horizon hides an old root from the list without making it unreadable.
func TestSessionsRanksByManifestAndHonoursHorizon(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	now := time.Now().UTC()
	recent, old := "root-recent", "root-old"

	pr := insertPart(t, ctx, pool, recent, "pr", "### Message 1 — user\nrecent", now.Add(-2*time.Hour))
	po := insertPart(t, ctx, pool, old, "po", "### Message 1 — user\nold", now.Add(-100*24*time.Hour))
	insertManifest(t, ctx, pool, recent, "mr", []string{pr}, now.Add(-time.Hour))
	insertManifest(t, ctx, pool, old, "mo", []string{po}, now.Add(-99*24*time.Hour))

	// A pointer block (no source_block_ids) with a very fresh timestamp on the
	// OLD root: it must not lift that root into the candidate list, because it
	// carries no new work.
	if _, err := pool.Exec(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, 'pointer', 'pointer', $2, 'checkpoint',
        jsonb_build_object('root_session_id', $3::text, 'latest_manifest_id', 'x'), $4)`,
		fxCategory, fxScope, old, now); err != nil {
		t.Fatalf("insert pointer: %v", err)
	}

	opt := fxOpts()
	opt.SessionHorizon = 30 * 24 * time.Hour
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs, err := src.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(refs) != 1 || refs[0].Session != recent {
		t.Errorf("Sessions with a 30d horizon = %+v, want only %q", refs, recent)
	}
	if len(refs) > 0 && refs[0].Watermark != 0 {
		t.Errorf("Ref.Watermark = %d, want 0 — Head answers that question", refs[0].Watermark)
	}

	// The horizon hides a root from the LIST; reading it still works.
	if head, err := src.Head(ctx, old); err != nil || head == 0 {
		t.Errorf("Head of a root outside the horizon = (%d, %v), want a real watermark", head, err)
	}

	// Without a horizon both roots appear, newest manifest first.
	opt.SessionHorizon = 0
	src2, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	refs2, err := src2.Sessions(ctx)
	if err != nil {
		t.Fatalf("Sessions: %v", err)
	}
	if len(refs2) != 2 || refs2[0].Session != recent || refs2[1].Session != old {
		t.Errorf("Sessions without horizon = %+v, want [%q %q]", refs2, recent, old)
	}
}

// TestScopeIsolation: material in another scope is invisible to every entry
// point. The arm holds one scope for read and write, and a reader that leaked
// across would open exactly the propagation path the write-side gate exists to
// close.
func TestScopeIsolation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 6, 10, 0, 0, 0, time.UTC)
	p1 := insertPart(t, ctx, pool, fxRoot, "p1", "### Message 1 — user\n"+strings.Repeat("a", 300), t0)
	insertManifest(t, ctx, pool, fxRoot, "m1", []string{p1}, t0.Add(time.Minute))

	opt := fxOpts()
	opt.Scope = "hth" // a scope nothing was written to
	src, err := ctxcheckpoint.New(pool, opt)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if refs, err := src.Sessions(ctx); err != nil || len(refs) != 0 {
		t.Errorf("Sessions in a foreign scope = (%+v, %v), want empty", refs, err)
	}
	if ok, err := src.HasNew(ctx, fxRoot, 0); err != nil || ok {
		t.Errorf("HasNew in a foreign scope = (%v, %v), want false", ok, err)
	}
	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err != nil || len(b.Items) != 0 {
		t.Errorf("Read in a foreign scope = (%d items, %v), want none", len(b.Items), err)
	}
}

// TestNewRejectsIncompleteOptions: a reader without a scope or a label would
// read the wrong rows or merge two watermark series, so it does not exist.
func TestNewRejectsIncompleteOptions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	_ = ctx
	pool := testdb.SetupTestDB(t)

	valid := fxOpts()
	noLabel, noScope, noCategory := valid, valid, valid
	noLabel.Label, noScope.Scope, noCategory.Category = "", "", ""
	zeroSessions, negSessions, zeroManifests := valid, valid, valid
	zeroSessions.MaxSessions, negSessions.MaxSessions, zeroManifests.MaxManifests = 0, -1, 0

	for name, opt := range map[string]ctxcheckpoint.Options{
		"no label":          noLabel,
		"no scope":          noScope,
		"no category":       noCategory,
		"zero max sessions": zeroSessions,
		"negative sessions": negSessions,
		"zero manifests":    zeroManifests,
	} {
		if _, err := ctxcheckpoint.New(pool, opt); err == nil {
			t.Errorf("New with %s returned no error", name)
		}
	}
	if _, err := ctxcheckpoint.New(nil, fxOpts()); err == nil {
		t.Error("New with a nil pool returned no error")
	}
}

// TestBrokenPartIDIsSchemaUntrusted pins the classification of foreign data
// that is not the agreed shape: a manifest listing something that is not a UUID
// yields schema_untrusted, not query_failed. The arm needs the distinction —
// query_failed reads as "retry", and retrying this is the starvation the class
// exists to make visible.
//
// The HANDLING (skip, quarantine, cooldown) is the arm's in A02-5; this test
// fixes only that the class is right and that the reader does not pretend to
// have made progress.
func TestBrokenPartIDIsSchemaUntrusted(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	pool := testdb.SetupTestDB(t)

	t0 := time.Date(2026, 8, 11, 10, 0, 0, 0, time.UTC)
	if _, err := pool.Exec(ctx, `
INSERT INTO context_blocks (category, title, content, scope, type_name, metadata, created_at)
VALUES ($1, 'broken manifest', 'manifest', $2, 'checkpoint',
        jsonb_build_object('root_session_id', $3::text,
                           'source_block_ids', to_jsonb(ARRAY['not-a-uuid']::text[])), $4)`,
		fxCategory, fxScope, fxRoot, t0); err != nil {
		t.Fatalf("insert broken manifest: %v", err)
	}

	src, err := ctxcheckpoint.New(pool, fxOpts())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	b, err := src.Read(ctx, fxRoot, 0, 100, 4000)
	if err == nil {
		t.Fatal("Read over a manifest with a malformed part id returned no error")
	}
	if !errors.Is(err, distillsource.ErrSchemaUntrusted) {
		t.Errorf("error class = %v, want ErrSchemaUntrusted", err)
	}
	if errors.Is(err, distillsource.ErrQueryFailed) {
		t.Error("error is classified query_failed — that reads as 'retry' and starves the root")
	}
	// No false progress: the watermark must not move past material that was
	// never delivered.
	if b.Watermark != 0 || b.Complete || len(b.Items) != 0 {
		t.Errorf("batch = %+v, want empty/incomplete at watermark 0", b)
	}
}
