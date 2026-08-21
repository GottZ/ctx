package main

// The boot seam around the initial backend-pool read (bootLoadBackendPool,
// main.go). Two steps hang off that read — the A02-W4 empty-pool advisory and
// the A04-W4 coupled-fingerprint reconcile — and both interpret the pool
// SNAPSHOT. A failed reload leaves the snapshot untouched (pool.go), which at
// boot is the empty NewPool one: a state nobody observed, not a state anybody
// configured.
//
// Interpreting it anyway costs twice. The advisory tells the operator "pool is
// empty" about a pool that was merely unreadable, and the reconcile hashes the
// EMPTY coupled set — a legitimate fingerprint value — into a mismatch against
// the stand on record, flushes context_embed_cache whole and stamps the empty
// set as the new truth; the next healthy boot mismatches against THAT stamp and
// flushes again. Two cold-cache spikes out of a read that never happened. The
// listener has guarded this since A04-W3 (failed reload ⇒ no diff); these tests
// pin the same guard on the boot half.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
)

// bootSeamCalls records what the seam ran.
type bootSeamCalls struct {
	reloads    int
	reconciles int
}

// TestBootLoadBackendPoolReloadFailure is the guard itself: a reload that
// errors ends the seam. No advisory (the pool is unreadable, not empty), no
// reconcile (nothing was read, so nothing may be diffed or stamped), and the
// cause is on the record for the operator.
//
// Mutation probe: drop the `return` in bootLoadBackendPool's error arm and both
// halves of this test go red — the reconcile counter moves and the advisory
// appears in the log.
func TestBootLoadBackendPoolReloadFailure(t *testing.T) {
	buf := captureBootLog(t)
	var calls bootSeamCalls

	// Exactly the boot shape: a fresh pool that never loaded a row.
	p := backends.NewPool(nil, nil)
	bootLoadBackendPool(context.Background(), p,
		func(context.Context) error { calls.reloads++; return errors.New("dial tcp: connection refused") },
		func(context.Context) error { calls.reconciles++; return nil })

	if calls.reloads != 1 {
		t.Fatalf("reloads = %d, want 1", calls.reloads)
	}
	if calls.reconciles != 0 {
		t.Errorf("reconciles = %d after a failed reload, want 0 — the empty set is a legitimate fingerprint and would flush the whole embed cache", calls.reconciles)
	}

	out := buf.String()
	if !strings.Contains(out, "initial reload failed") || !strings.Contains(out, "connection refused") {
		t.Errorf("log = %q, want the reload failure with its cause", out)
	}
	// The advisory is a statement about the DATA ("nobody seeded this install"),
	// not about the reader. On an unreadable pool it points the operator at
	// `ctx backends seed` for a table that may be perfectly well populated.
	if strings.Contains(out, "advisory="+backends.AdvisorySubjectPool) {
		t.Errorf("log = %q, want no empty-pool advisory — the pool was unreadable, not empty", out)
	}
}

// TestBootLoadBackendPoolReloadSuccess is the positive half: on a successful
// read both steps run, and the advisory keeps deciding by the snapshot alone —
// it fires for the genuinely empty pool and stays silent for the seeded one.
func TestBootLoadBackendPoolReloadSuccess(t *testing.T) {
	t.Run("empty pool: advisory and reconcile both run", func(t *testing.T) {
		buf := captureBootLog(t)
		var calls bootSeamCalls

		p := backends.NewPool(nil, nil)
		bootLoadBackendPool(context.Background(), p,
			func(context.Context) error { calls.reloads++; p.SeedSnapshotForTest(nil); return nil },
			func(context.Context) error { calls.reconciles++; return nil })

		if calls.reloads != 1 || calls.reconciles != 1 {
			t.Fatalf("reloads/reconciles = %d/%d, want 1/1", calls.reloads, calls.reconciles)
		}
		if out := buf.String(); !strings.Contains(out, "advisory="+backends.AdvisorySubjectPool) {
			t.Errorf("log = %q, want the empty-pool advisory — an empty table READ is exactly the state W4 exists for", out)
		}
	})

	t.Run("seeded pool: reconcile runs, advisory stays silent", func(t *testing.T) {
		buf := captureBootLog(t)
		var calls bootSeamCalls

		p := backends.NewPool(nil, nil)
		bootLoadBackendPool(context.Background(), p,
			func(context.Context) error {
				calls.reloads++
				p.SeedSnapshotForTest([]backends.Backend{{
					ID: "b1", Name: "chat-primary", Host: "http://backend.example.com",
					Trust: backends.TrustFull, Roles: []string{backends.RoleSynthesis},
					Priority: 100, Enabled: true,
				}})
				return nil
			},
			func(context.Context) error { calls.reconciles++; return nil })

		if calls.reconciles != 1 {
			t.Fatalf("reconciles = %d, want 1", calls.reconciles)
		}
		if out := buf.String(); out != "" {
			t.Errorf("log = %q on a loaded, seeded pool, want silence", out)
		}
	})
}

// TestBootLoadBackendPoolReconcileFailureIsNonFatal: the reconcile keeps the
// posture of the steps around it — the error is logged and the boot continues.
// A server that answered queries yesterday must not be held hostage by a
// fingerprint read, and the reconcile stamps nothing on its failing paths
// (coupled_fingerprint.go), so the next boot retries against the same stand.
func TestBootLoadBackendPoolReconcileFailureIsNonFatal(t *testing.T) {
	buf := captureBootLog(t)

	p := backends.NewPool(nil, nil)
	bootLoadBackendPool(context.Background(), p,
		func(context.Context) error {
			p.SeedSnapshotForTest([]backends.Backend{{
				ID: "b1", Name: "embed-primary", Host: "http://embed.example.com",
				Trust: backends.TrustFull, Roles: []string{backends.RoleEmbed},
				Priority: 100, Enabled: true, Model: "pool-embed",
			}})
			return nil
		},
		func(context.Context) error { return errors.New("relation does not exist") })

	if out := buf.String(); !strings.Contains(out, "coupled fingerprint reconcile failed") ||
		!strings.Contains(out, "relation does not exist") {
		t.Errorf("log = %q, want the reconcile failure with its cause", out)
	}
}
