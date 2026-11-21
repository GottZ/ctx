//go:build integration

// Issue #426: the classification afterwork of a write (store.ClassifyBlockAfterUpsert)
// derives the block's type from the title and metadata the write's own
// statement returned, and it runs after that statement committed. Two writes
// A, B of one block that commit in that order can finish their afterwork in
// the order B, A; A's verdict must then not land on a block that holds B's
// title and metadata.
//
// The writes go through the production functions the update doors call one
// after the other (store.UpdateBlock, then finishBlockUpdate) and the ones the
// store batch calls (store.UpsertBlock, then finishStoredBlock); a test holds
// one afterwork back by calling it later.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run 'TestUpdateReclassify|TestStoreReclassify' -count=1 -v
package handler

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/store"
)

// i426Type reads the type of a block as stored.
func i426Type(t *testing.T, f *i406Fixture, id string) (name, source string) {
	t.Helper()
	if err := f.pool.QueryRow(f.ctx,
		`SELECT type_name, type_source FROM context_blocks WHERE id = $1`, id).Scan(&name, &source); err != nil {
		t.Fatalf("read type: %v", err)
	}
	return name, source
}

// i426AssertType fails unless the block holds the type name with source auto
// (or the given source).
func i426AssertType(t *testing.T, f *i406Fixture, id, wantName, wantSource, label string) {
	t.Helper()
	name, source := i426Type(t, f, id)
	if name != wantName || source != wantSource {
		t.Errorf("%s: (type_name, type_source) = (%q, %q), want (%q, %q)", label, name, source, wantName, wantSource)
	}
}

// i426Update runs the write half of an update (what every update door runs
// before its afterwork) and returns the block it returned.
func i426Update(t *testing.T, f *i406Fixture, id string, data store.UpdateBlockData) *store.Block {
	t.Helper()
	b, _, err := store.UpdateBlock(f.ctx, f.pool, id, data, writableBlockScopes(f.directAR))
	if err != nil || b == nil {
		t.Fatalf("update %s: %v (block %v)", id, err, b)
	}
	return b
}

func i426Metadata(m map[string]any) store.UpdateBlockData { return store.UpdateBlockData{Metadata: m} }

func i426Title(s string) store.UpdateBlockData { return store.UpdateBlockData{Title: &s} }

// The verdicts of the builtin registry the tests lean on: the is_meta flag
// (system-meta, priority 10), a dream-* source and an audit title pattern
// (audit-trail, priority 20). A block stored without either is knowledge.
const (
	i426Meta     = "system-meta"
	i426Audit    = "audit-trail"
	i426Default  = "knowledge"
	i426AuditTtl = "Session 9 Handover i426"
)

// i426LockWait bounds the waits of the tests with a second connection.
const i426LockWait = 10 * time.Second

// TestUpdateReclassifyStaleVerdictSkipped is the interleaving of the issue: A
// sets metadata that classifies as system-meta, B metadata that classifies as
// audit-trail; they commit in that order, B's afterwork finishes first, A's
// runs last. The block holds B's metadata and must keep B's type — system-meta
// would take it out of retrieval.
func TestUpdateReclassifyStaleVerdictSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i426-stale", "i426 body", nil)
	i426AssertType(t, f, id, i426Default, "auto", "after the store")

	dataA := i426Metadata(map[string]any{"is_meta": true})
	dataB := i426Metadata(map[string]any{"source": "dream-probe"})
	blockA := i426Update(t, f, id, dataA)
	blockB := i426Update(t, f, id, dataB)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i426-b")
	i426AssertType(t, f, id, i426Audit, "auto", "after B's afterwork")

	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
	i426AssertType(t, f, id, i426Audit, "auto", "after A's late afterwork")
}

// TestUpdateReclassifyStaleInputSkipped takes the classifier's inputs one at a
// time: in each case A's verdict was computed from a block that differs from
// the one its afterwork finds in exactly that input.
func TestUpdateReclassifyStaleInputSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)

	// Title: A gains an audit title, B replaces it with one that matches no
	// rule (B's afterwork classifies nothing). The block keeps the type it had.
	t.Run("title", func(t *testing.T) {
		id := f.block(t, "i426-title", "i426 body", nil)
		dataA, dataB := i426Title(i426AuditTtl), i426Title("i426 plain notes")
		blockA := i426Update(t, f, id, dataA)
		blockB := i426Update(t, f, id, dataB)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i426-b")
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
		i426AssertType(t, f, id, i426Default, "auto", "after A's late afterwork")
	})

	// Source: A's dream source classifies as audit-trail, B's source matches
	// no prefix.
	t.Run("source", func(t *testing.T) {
		id := f.block(t, "i426-source", "i426 body", nil)
		dataA := i426Metadata(map[string]any{"source": "dream-i426"})
		dataB := i426Metadata(map[string]any{"source": "claude-code-i426"})
		blockA := i426Update(t, f, id, dataA)
		blockB := i426Update(t, f, id, dataB)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i426-b")
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
		i426AssertType(t, f, id, i426Default, "auto", "after A's late afterwork")
	})

	// Flag: A's audit title classifies as audit-trail; B merges the is_meta
	// flag in, and the flag's type comes first. Title and source are the same
	// on both sides — only the flag tells A's block from B's.
	t.Run("flag", func(t *testing.T) {
		id := f.block(t, "i426-flag", "i426 body", nil)
		dataA := i426Title(i426AuditTtl)
		dataB := store.UpdateBlockData{MetadataMerge: map[string]any{"is_meta": true}}
		blockA := i426Update(t, f, id, dataA)
		blockB := i426Update(t, f, id, dataB)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataB, blockB, "i426-b")
		i426AssertType(t, f, id, i426Meta, "auto", "after B's afterwork")
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
		i426AssertType(t, f, id, i426Meta, "auto", "after A's late afterwork")
	})
}

// TestUpdateReclassifySingleUpdateWrites is the normal case: one update and its
// afterwork write the type of the block's new metadata — also when the block
// carries keys no rule reads, and a title with multi-byte characters.
func TestUpdateReclassifySingleUpdateWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i426-single", "i426 body", map[string]any{"source": "claude-code-i426", "pages": 12.5})
	title := "Überprüfung — i426"
	data := store.UpdateBlockData{
		Title:         &title,
		MetadataMerge: map[string]any{"is_meta": true},
	}
	b := i426Update(t, f, id, data)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, data, b, "i426")
	i426AssertType(t, f, id, i426Meta, "auto", "after the update")
}

// TestUpdateReclassifyOtherWriteKeepsVerdict: a write that changes neither
// the title nor a metadata key a rule reads leaves a verdict standing that is
// still on its way — the version of the verdict is its input, not updated_at.
func TestUpdateReclassifyOtherWriteKeepsVerdict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)

	// Tags only: no afterwork classification of its own.
	t.Run("tags", func(t *testing.T) {
		id := f.block(t, "i426-tags", "i426 body", nil)
		dataA := i426Metadata(map[string]any{"is_meta": true})
		blockA := i426Update(t, f, id, dataA)
		dataC := store.UpdateBlockData{Tags: []string{"i426"}}
		blockC := i426Update(t, f, id, dataC)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataC, blockC, "i426-c")
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
		i426AssertType(t, f, id, i426Meta, "auto", "after A's afterwork")
	})

	// A metadata key no rule reads, merged in between A's write and A's
	// afterwork; A's afterwork runs before C's, so A's verdict is the one
	// that has to land.
	t.Run("unread metadata key", func(t *testing.T) {
		id := f.block(t, "i426-unread", "i426 body", nil)
		dataA := i426Metadata(map[string]any{"is_meta": true})
		blockA := i426Update(t, f, id, dataA)
		dataC := store.UpdateBlockData{MetadataMerge: map[string]any{"note": "i426-c"}}
		blockC := i426Update(t, f, id, dataC)
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
		i426AssertType(t, f, id, i426Meta, "auto", "after A's afterwork")
		finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataC, blockC, "i426-c")
		i426AssertType(t, f, id, i426Meta, "auto", "after C's afterwork")
	})
}

// TestUpdateReclassifyManualTypeUntouched: a type set by hand between a write
// and its afterwork stays — the classifier writes auto types only.
func TestUpdateReclassifyManualTypeUntouched(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	id := f.block(t, "i426-manual", "i426 body", nil)
	dataA := i426Metadata(map[string]any{"is_meta": true})
	blockA := i426Update(t, f, id, dataA)
	ref := "reference"
	dataM := store.UpdateBlockData{Type: &ref}
	blockM := i426Update(t, f, id, dataM)
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataM, blockM, "i426-m")
	finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
	i426AssertType(t, f, id, "reference", "manual", "after A's afterwork")
}

// TestUpdateReclassifyWaitsForInFlightWrite holds a write of the block open on
// a second connection while A's afterwork runs: the afterwork has to wait for
// it and is then judged against the row that write leaves behind.
func TestUpdateReclassifyWaitsForInFlightWrite(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)

	cases := []struct {
		name   string
		write  string // the held statement; $1 is the block id
		commit bool
		want   string
	}{
		// The metadata A classified is replaced: A's verdict must not land.
		{"inputs replaced", `UPDATE context_blocks SET metadata = '{"source": "claude-code-i426"}', updated_at = now() WHERE id = $1`, true, i426Default},
		// The same write rolled back: the row still holds A's inputs.
		{"inputs replaced, rolled back", `UPDATE context_blocks SET metadata = '{"source": "claude-code-i426"}', updated_at = now() WHERE id = $1`, false, i426Meta},
		// A write that leaves the inputs alone: A's verdict still holds.
		{"tags only", `UPDATE context_blocks SET tags = '{i426}', updated_at = now() WHERE id = $1`, true, i426Meta},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := f.block(t, "i426-inflight-"+string(rune('a'+i)), "i426 body", nil)
			dataA := i426Metadata(map[string]any{"is_meta": true})
			blockA := i426Update(t, f, id, dataA)

			tx, err := f.pool.Begin(f.ctx)
			if err != nil {
				t.Fatalf("begin holder: %v", err)
			}
			defer func() { _ = tx.Rollback(f.ctx) }()
			var holderPID int32
			if err := tx.QueryRow(f.ctx, `SELECT pg_backend_pid()`).Scan(&holderPID); err != nil {
				t.Fatalf("holder pid: %v", err)
			}
			if _, err := tx.Exec(f.ctx, tc.write, id); err != nil {
				t.Fatalf("holder write: %v", err)
			}

			done := make(chan struct{})
			go func() {
				defer close(done)
				finishBlockUpdate(f.ctx, f.pool, f.mcpCfg.Blocktypes, dataA, blockA, "i426-a")
			}()
			// Release the holder only once A's afterwork waits on it: one that
			// finishes first did not wait for the held row, and one that has not
			// waited by the deadline proves nothing either.
			waited := false
			for deadline := time.Now().Add(i426LockWait); !waited && time.Now().Before(deadline); {
				select {
				case <-done:
					t.Fatalf("A's afterwork finished while the write was still in flight: it did not wait for the row lock")
				default:
				}
				if err := f.pool.QueryRow(f.ctx,
					`SELECT EXISTS (SELECT 1 FROM pg_stat_activity WHERE $1::int = ANY(pg_blocking_pids(pid)))`, holderPID).Scan(&waited); err != nil {
					t.Fatalf("poll lock wait: %v", err)
				}
				if !waited {
					time.Sleep(2 * time.Millisecond)
				}
			}
			if !waited {
				t.Fatalf("A's afterwork never waited on the in-flight write within %s", i426LockWait)
			}
			if tc.commit {
				err = tx.Commit(f.ctx)
			} else {
				err = tx.Rollback(f.ctx)
			}
			if err != nil {
				t.Fatalf("end holder: %v", err)
			}
			select {
			case <-done:
			case <-time.After(i426LockWait):
				t.Fatalf("A's afterwork did not finish within %s after the held write ended", i426LockWait)
			}
			i426AssertType(t, f, id, tc.want, "auto", "after A's afterwork")
		})
	}
}

// TestStoreReclassifyStaleVerdictSkipped is the store path's form of the issue:
// two upserts of one identity commit in order A, B and their afterwork
// (finishStoredBlock, the batch store's; /api/store, the store tool, ingest
// and the confirm of a staged store call the same classifier on the block
// their upsert returned) finishes B, A.
func TestStoreReclassifyStaleVerdictSkipped(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)
	set := f.mcpCfg.Blocktypes.SnapshotForRequest(f.ctx)
	// Each upsert carries a content of its own: on an identical content the
	// conflict branch merges the metadata instead of replacing it.
	upsert := func(content string, metadata map[string]any) *store.Block {
		t.Helper()
		b, err := store.UpsertBlock(f.ctx, f.pool, "i426", "i426-store", content, nil, metadata, "private", false, store.SensitivityWrite{}, "")
		if err != nil || b == nil {
			t.Fatalf("upsert: %v", err)
		}
		return b
	}
	blockA := upsert("i426 body A", map[string]any{"is_meta": true})
	blockB := upsert("i426 body B", map[string]any{"source": "dream-probe"})
	if blockA.ID != blockB.ID {
		t.Fatalf("upserts landed on different rows: %s %s", blockA.ID, blockB.ID)
	}
	finishStoredBlock(f.ctx, f.pool, set, blockB, "i426-b")
	i426AssertType(t, f, blockB.ID, i426Audit, "auto", "after B's afterwork")
	finishStoredBlock(f.ctx, f.pool, set, blockA, "i426-a")
	i426AssertType(t, f, blockB.ID, i426Audit, "auto", "after A's late afterwork")
}
