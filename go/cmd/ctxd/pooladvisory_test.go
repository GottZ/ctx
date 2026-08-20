package main

// A02-W4 (design/02 §4.1c, §7 W4) — the boot half of the empty-pool advisory.
//
// The wire half lives in internal/handler (TestStatusAdvisoryEmptyPool for the
// named admin reason, TestHealthBodyOmitsPoolAdvisory for the public-surface
// needle). This file pins the log half: after the β-Schnitt removes the seed
// from the boot, an empty context_backends becomes the ordinary state of every
// fresh install — and a boot that says nothing about it leaves the operator
// with a server that answers every query with an embed failure and no trail
// back to the cause.

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// captureBootLog swaps the process logger for a buffer and restores it after
// the test. Text handler, not JSON: the assertions read attribute pairs.
func captureBootLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// TestAdvisePoolEmptyBoot is the A02-W4 boot gate: an empty pool logs the WARN
// carrying the NAMED reason `backend_pool: empty`, a pool with rows logs
// nothing at all.
//
// The negative half is the load-bearing one. An advisory that also fires on a
// seeded system is noise every operator learns to skip — which is precisely how
// a real fresh-install signal would be missed. Mutation probe: drop the
// early return in advisePoolEmptyBoot and the "seeded pool" subtest goes red.
func TestAdvisePoolEmptyBoot(t *testing.T) {
	t.Run("empty pool warns with the named reason", func(t *testing.T) {
		buf := captureBootLog(t)

		p := backends.NewPool(nil, nil)
		p.SeedSnapshotForTest(nil)
		advisePoolEmptyBoot(p)

		out := buf.String()
		if !strings.Contains(out, "level=WARN") {
			t.Errorf("log = %q, want a WARN — an unserviceable pool is not an INFO", out)
		}
		// The named reason, in the SAME vocabulary the admin status frame uses.
		// A prose-only warning would leave the two surfaces describing one state
		// with two wordings.
		for _, want := range []string{
			"advisory=" + backends.AdvisorySubjectPool,
			"state=" + backends.AdvisoryStateEmpty,
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log = %q, want the named reason %q", out, want)
			}
		}
		// The advisory has to be actionable: it names the way OUT, because the
		// operator reading it on a fresh install has no other pointer (the seed
		// left the boot).
		if !strings.Contains(out, "ctx backends seed") {
			t.Errorf("log = %q, want the seed command — an advisory without a next step is just an alarm", out)
		}
	})

	t.Run("seeded pool stays silent", func(t *testing.T) {
		buf := captureBootLog(t)

		p := backends.NewPool(nil, nil)
		p.SeedSnapshotForTest([]backends.Backend{{
			ID: "b1", Name: "chat-primary", Host: "http://backend.example.com",
			Trust: backends.TrustFull, Roles: []string{backends.RoleSynthesis},
			Priority: 100, Enabled: true,
		}})
		advisePoolEmptyBoot(p)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a seeded pool, want silence", out)
		}
	})

	// A disabled row is still a row. The advisory is a POOL-EMPTINESS signal,
	// not a serving-eligibility one — eligibility already has its own named
	// states (channel_probe.state, effective_state), and one name must not
	// carry two meanings.
	t.Run("disabled rows are not an empty pool", func(t *testing.T) {
		buf := captureBootLog(t)

		p := backends.NewPool(nil, nil)
		p.SeedSnapshotForTest([]backends.Backend{{
			ID: "b1", Name: "chat-primary", Host: "http://backend.example.com",
			Trust: backends.TrustFull, Roles: []string{backends.RoleSynthesis},
			Priority: 100, Enabled: false,
		}})
		advisePoolEmptyBoot(p)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — a disabled row is not an empty pool", out)
		}
	})
}
