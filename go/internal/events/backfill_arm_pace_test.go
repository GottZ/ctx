package events

import (
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/embedcache"
)

// TestBackfillArmPace pins the per-tenant pacing rules of the embed-backfill
// arm (issue #228) without a database: which tenant is due when, how long
// the arm sleeps, and that holds of unlisted tenants are dropped.
func TestBackfillArmPace(t *testing.T) {
	const iv = 10 * time.Second
	t0 := time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)
	at := func(d time.Duration) time.Time { return t0.Add(d) }
	paceOf := func(scopes ...string) *backfillArmPace {
		p := newBackfillArmPace()
		tenants := make([]backgroundTenant, len(scopes))
		for i, sc := range scopes {
			tenants[i] = backgroundTenant{scope: sc}
		}
		p.setTenants(tenants, t0)
		p.begin(t0, iv) // round at t0
		return p
	}

	t.Run("unheld_tenant_is_due", func(t *testing.T) {
		p := paceOf("a")
		if i, ok := p.nextDue(t0, iv); !ok || i != 0 {
			t.Fatalf("nextDue = %d, %v, want 0, true", i, ok)
		}
		if got := p.untilDue(t0, iv); got != 0 {
			t.Fatalf("untilDue with an unheld tenant = %v, want 0", got)
		}
	})

	t.Run("empty_pick_holds_its_tenant_until_the_next_round", func(t *testing.T) {
		p := paceOf("a", "b")
		p.begin(at(4*time.Second), iv) // a turn inside the round
		p.record("a", backfillIdle, at(4*time.Second))
		if p.due("a", at(9*time.Second), iv) {
			t.Fatal("idle tenant due before the next round")
		}
		if i, ok := p.nextDue(at(5*time.Second), iv); !ok || i != 1 {
			t.Fatalf("nextDue = %d, %v, want 1 (b), true: one idle tenant must not hold another", i, ok)
		}
		p.record("b", backfillIdle, at(5*time.Second))
		// Both fell idle inside the round at t0 and are due with the next
		// round at t0+10s, not one interval after their own picks: a tenant
		// whose backlog ran dry is looked at again within one interval, and
		// the idle tenants fall due together.
		if got := p.untilDue(at(6*time.Second), iv); got != 4*time.Second {
			t.Fatalf("untilDue = %v, want 4s (the next round at t0+10s)", got)
		}
		p.begin(at(iv), iv)
		if !p.due("a", at(iv), iv) || !p.due("b", at(iv), iv) {
			t.Fatal("idle tenants not due at the next round")
		}
	})

	t.Run("a_pick_that_took_a_block_leaves_its_tenant_due", func(t *testing.T) {
		p := paceOf("a")
		p.record("a", backfillIdle, t0)
		for _, o := range []backfillOutcome{backfillEmbedded, backfillParked} {
			p.record("a", backfillIdle, t0)
			p.record("a", o, at(time.Second))
			if !p.due("a", at(time.Second), iv) {
				t.Fatalf("tenant not due after outcome %d", o)
			}
		}
	})

	t.Run("wire_hold_counts_from_the_end_of_its_pick", func(t *testing.T) {
		p := paceOf("f", "g")
		p.record("f", backfillFailed, at(3*time.Second))
		p.begin(at(iv), iv) // a new round at t0+10s
		if p.due("f", at(iv), iv) {
			t.Fatal("wire-held tenant due at the next round, before one interval since its probe")
		}
		if !p.due("g", at(iv), iv) {
			t.Fatal("another tenant held back by a tenant-private wire failure")
		}
		if !p.due("f", at(13*time.Second), iv) {
			t.Fatal("wire-held tenant not due one interval after its probe")
		}
		p.record("g", backfillIdle, at(iv))
		if got := p.untilDue(at(11*time.Second), iv); got != 2*time.Second {
			t.Fatalf("untilDue = %v, want 2s (end of the wire hold)", got)
		}
	})

	t.Run("shared_backend_failure_holds_every_tenant", func(t *testing.T) {
		p := paceOf("a", "b", "c")
		p.record("a", backfillFailedShared, at(2*time.Second))
		if _, ok := p.nextDue(at(5*time.Second), iv); ok {
			t.Fatal("a tenant is due while a shared backend hold runs")
		}
		if got := p.untilDue(at(5*time.Second), iv); got != 7*time.Second {
			t.Fatalf("untilDue = %v, want 7s (end of the shared hold)", got)
		}
		if i, ok := p.nextDue(at(12*time.Second), iv); !ok || i != 0 {
			t.Fatalf("nextDue after the shared hold = %d, %v, want 0, true", i, ok)
		}
	})

	t.Run("cursor_serves_due_tenants_round_robin", func(t *testing.T) {
		p := paceOf("a", "b", "c")
		p.cursor = 2
		if i, ok := p.nextDue(t0, iv); !ok || i != 2 {
			t.Fatalf("nextDue from cursor 2 = %d, %v, want 2", i, ok)
		}
		p.record("c", backfillIdle, t0)
		p.cursor = 3
		if i, ok := p.nextDue(t0, iv); !ok || i != 0 {
			t.Fatalf("nextDue past the end = %d, %v, want 0 (wraps)", i, ok)
		}
	})

	t.Run("sleep_is_capped_by_the_poll_and_never_negative", func(t *testing.T) {
		const long = time.Hour
		p := newBackfillArmPace()
		p.setTenants([]backgroundTenant{{scope: "a"}}, t0)
		p.begin(t0, long)
		p.record("a", backfillIdle, t0)
		if got := p.untilDue(t0, long); got != backfillArmPoll {
			t.Fatalf("untilDue under a one-hour interval = %v, want backfillArmPoll %v", got, backfillArmPoll)
		}
		if got := p.untilDue(t0.Add(2*long), long); got != 0 {
			t.Fatalf("untilDue past the next round = %v, want 0", got)
		}
	})

	t.Run("a_new_list_drops_unlisted_holds", func(t *testing.T) {
		p := paceOf("a", "gone")
		p.record("a", backfillIdle, t0)
		p.record("gone", backfillFailed, t0)
		p.setTenants([]backgroundTenant{{scope: "a"}}, at(time.Second))
		if _, ok := p.held["gone"]; ok {
			t.Fatal("hold of an unlisted tenant kept")
		}
		if _, ok := p.held["a"]; !ok {
			t.Fatal("hold of a listed tenant dropped")
		}
	})

	t.Run("an_empty_list_is_the_tenant_less_pass", func(t *testing.T) {
		p := newBackfillArmPace()
		p.setTenants(nil, t0)
		if len(p.tenants) != 1 || p.tenants[0].scope != "_global" || p.tenants[0].owned != nil {
			t.Fatalf("tenants = %+v, want the single unbounded _global pass", p.tenants)
		}
	})
}

// TestWireFailedOnShared: a failed chain is a shared-backend suspect only
// when an attempt that reached the wire went to a shared row.
func TestWireFailedOnShared(t *testing.T) {
	shared := backends.Backend{Name: "shared"}
	global := backends.Backend{Name: "global", Scope: backends.GlobalScope}
	private := backends.Backend{Name: "private", Scope: "tenant-a"}
	cases := []struct {
		name     string
		chain    []backends.Backend
		attempts []embedcache.WireAttempt
		want     bool
	}{
		{"private_only", []backends.Backend{private}, []embedcache.WireAttempt{{Backend: "private", Class: "network"}}, false},
		{"shared_row", []backends.Backend{shared}, []embedcache.WireAttempt{{Backend: "shared", Class: "server"}}, true},
		{"global_scope_row", []backends.Backend{global}, []embedcache.WireAttempt{{Backend: "global", Class: "timeout"}}, true},
		{"private_then_shared", []backends.Backend{private, shared}, []embedcache.WireAttempt{{Backend: "private", Class: "network"}, {Backend: "shared", Class: "network"}}, true},
		{"shared_without_model", []backends.Backend{private, shared}, []embedcache.WireAttempt{{Backend: "private", Class: "network"}, {Backend: "shared", Class: "no_model"}}, false},
		{"no_attempt", []backends.Backend{shared}, nil, false},
	}
	for _, c := range cases {
		if got := wireFailedOnShared(c.chain, c.attempts); got != c.want {
			t.Errorf("%s: wireFailedOnShared = %v, want %v", c.name, got, c.want)
		}
	}
}
