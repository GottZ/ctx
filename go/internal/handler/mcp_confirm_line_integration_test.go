//go:build integration

package handler

// Issue #483 points 2 and 13: the confirm tool's server line notes its write
// where the write ends, as the other write tools do — committed right after
// the commit, before the afterwork (a panic there leaves the line saying what
// was written), and on a failed execute the block or blob the staged card
// names.
//
//	go test -tags=integration ./internal/handler/ -run 'TestMCPConfirmLine' -count=1 -v

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// panicOnMsg is a capture that panics once on the record with msg — a fault in
// the code that writes that record.
type panicOnMsg struct {
	*rejectLogCapture
	msg  string
	once sync.Once
}

func (p *panicOnMsg) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == p.msg {
		p.once.Do(func() { panic("i483 probe panic after the commit") })
	}
	return p.rejectLogCapture.Handle(ctx, r)
}

var confirmLineHashRE = regexp.MustCompile(`payload_hash: ([0-9a-f]{64})`)

// TestMCPConfirmLine — the confirm tool's line after its commit and on a
// failed execute.
func TestMCPConfirmLine(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newMCPLineFixture(t)
	ctx := context.Background()
	ar, err := auth.AuthenticateRaw(ctx, f.pool, f.plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate fixture key: %v", err)
	}
	ar.ConfirmWrites = true
	stage := func(t *testing.T, tool string, args map[string]any) string {
		t.Helper()
		installRejectLogCapture(t)
		url, done := f.serve(t, ar, nil)
		text, _ := callTool(t, url, "", tool, args)
		waitDone(t, done)
		m := confirmLineHashRE.FindStringSubmatch(text)
		if m == nil {
			t.Fatalf("%s did not stage: %s", tool, text)
		}
		return m[1]
	}

	t.Run("a panic in the afterwork of a confirmed store", func(t *testing.T) {
		// classifyHoldTitle's pattern: the title classifies to audit-trail,
		// so the afterwork's classification writes type_name — and fails.
		const title = "Session 483 Handover Protokoll panic"
		if name, ok := f.reg.Snapshot().Classify(title, nil); !ok || name != "audit-trail" {
			t.Fatalf("fixture: %q classifies to (%q, %v), want audit-trail", title, name, ok)
		}
		for _, q := range []string{
			`CREATE FUNCTION i483_fail_classify() RETURNS trigger LANGUAGE plpgsql AS $$
			 BEGIN RAISE EXCEPTION 'i483 probe classify fault'; END $$`,
			`CREATE TRIGGER i483_fail_classify BEFORE UPDATE OF type_name ON context_blocks FOR EACH ROW
			 WHEN (NEW.title = 'Session 483 Handover Protokoll panic' AND OLD.type_name IS DISTINCT FROM NEW.type_name)
			 EXECUTE FUNCTION i483_fail_classify()`,
		} {
			if _, err := f.pool.Exec(ctx, q); err != nil {
				t.Fatalf("fault fixture: %v", err)
			}
		}
		hash := stage(t, "store", storeArgs(title, "confirmed, then the afterwork panics"))
		logs := &panicOnMsg{rejectLogCapture: &rejectLogCapture{}, msg: "confirm: auto-classify failed"}
		prev := slog.Default()
		slog.SetDefault(slog.New(logs))
		t.Cleanup(func() { slog.SetDefault(prev) })
		url, done := f.serve(t, ar, nil)
		text, isErr := callTool(t, url, "", "confirm", map[string]any{"payload_hash": hash})
		waitDone(t, done)
		if !isErr || len(logs.withMsg("mcp: panic recovered")) != 1 {
			t.Fatalf("fixture: the afterwork did not panic (answer %q)", text)
		}
		id := blockIDByTitle(t, f.pool, title)
		line := toolLine(t, logs.rejectLogCapture)
		if line.attrs["write_outcome"] != writeCommitted || line.attrs["block_id"] != id {
			t.Errorf("tool line = %+v, want write_outcome=committed block_id=%s (the block stands)", line.attrs, id)
		}
	})

	execFault := func(t *testing.T, table, when string) {
		t.Helper()
		name := fmt.Sprintf("i483_exec_fault_%s", table)
		failWritesOn(t, f.pool, name, table, when)
		t.Cleanup(func() {
			if _, err := f.pool.Exec(context.Background(), fmt.Sprintf(`DROP TRIGGER %s ON %s`, name, table)); err != nil {
				t.Errorf("drop %s: %v", name, err)
			}
			if _, err := f.pool.Exec(context.Background(), fmt.Sprintf(`DROP FUNCTION %s()`, name)); err != nil {
				t.Errorf("drop %s(): %v", name, err)
			}
		})
	}
	confirmFault := func(t *testing.T, hash, msg, key, id string) {
		t.Helper()
		logs := installRejectLogCapture(t)
		url, done := f.serve(t, ar, nil)
		if text, isErr := callTool(t, url, "", "confirm", map[string]any{"payload_hash": hash}); !isErr {
			t.Fatalf("fixture: the confirm executed (%s)", text)
		}
		waitDone(t, done)
		if recs := logs.withMsg(msg); len(recs) != 1 || recs[0].attrs["write_outcome"] != writeRolledBack {
			t.Errorf("%q = %+v, want one line with write_outcome=rolled_back", msg, recs)
		}
		line := toolLine(t, logs)
		if line.attrs["write_outcome"] != writeRolledBack || line.attrs[key] != id {
			t.Errorf("tool line = %+v, want write_outcome=rolled_back %s=%s", line.attrs, key, id)
		}
	}
	seed := func(t *testing.T, title string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, f.pool, "i483", title, "seeded: "+title, nil, nil, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed %q: %v", title, err)
		}
		return b.ID
	}

	t.Run("a confirmed update whose execute fails names its block", func(t *testing.T) {
		id := seed(t, "i483 confirm fault update")
		hash := stage(t, "update", map[string]any{"id": id, "content": "refused at the confirm"})
		execFault(t, "context_blocks", `NEW.title = 'i483 confirm fault update'`)
		confirmFault(t, hash, "confirm: confirmed update execute failed", "block_id", id)
	})
	t.Run("a confirmed archive whose execute fails names its block", func(t *testing.T) {
		id := seed(t, "i483 confirm fault archive")
		hash := stage(t, "archive", map[string]any{"id": id})
		execFault(t, "context_blocks", `NEW.title = 'i483 confirm fault archive'`)
		confirmFault(t, hash, "confirm: confirmed archive execute failed", "block_id", id)
	})
	t.Run("a confirmed blob_link whose execute fails names its blob", func(t *testing.T) {
		blob, err := store.UpsertBlob(ctx, f.pool, "i483", "i483 confirm fault blob", "probe.txt", "text/plain", "private", []byte("payload"), nil, nil, "")
		if err != nil {
			t.Fatalf("seed blob: %v", err)
		}
		block := seed(t, "i483 confirm fault link target")
		hash := stage(t, "blob_link", map[string]any{"id": blob.ID, "context_block_id": block})
		execFault(t, "context_blobs", `NEW.title = 'i483 confirm fault blob'`)
		confirmFault(t, hash, "confirm: confirmed blob link execute failed", "blob_id", blob.ID)
	})
}
