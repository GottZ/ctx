package oai_test

// Pins of the follow-up hardening after the Copilot review on PR #144 and
// Fremd-Review 4 (W02-07): B-3 the watcher starts from the alias generation
// read right before the entry Resolve, so a change between the Resolve and
// the watcher start is seen on the first tick; B-4 a run ended after a
// successful Plan — or after the acquire, before the wire — writes no
// llmlog row and ends its reservation with Abort; B-5 the hull check of
// every tool, not only tools[0].

import (
	"context"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// fuPlanHook runs hook after the real Plan succeeded.
type fuPlanHook struct {
	oai.AdmissionPlanner
	hook func()
}

func (p fuPlanHook) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		p.hook()
	}
	return plan, err
}

// fuFirstHook runs hook after the real First acquire succeeded: the run
// ends with the lease held, before the wire call.
type fuFirstHook struct {
	oai.RequestAcquirer
	hook func()
}

func (a fuFirstHook) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	l, runCtx, err := a.RequestAcquirer.First(ctx, b, role, timeout)
	if err == nil {
		a.hook()
	}
	return l, runCtx, err
}

// fuAcquireHook puts fuFirstHook around the real plan's acquirer.
type fuAcquireHook struct {
	oai.AdmissionPlanner
	hook func()
}

func (p fuAcquireHook) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = fuFirstHook{RequestAcquirer: plan.Acquirer, hook: p.hook}
	}
	return plan, err
}

// TestWatcherSeesAChangeBetweenResolveAndStart (B-3, Copilot review on PR
// #144): the alias is disabled or deleted after the entry Resolve and before
// the watcher starts (inside Plan). The watcher used to read the registry
// generation at its start — the change already in it — and skipped every
// Resolve; now it starts from the generation read before the entry Resolve:
// the run ends within one tick plus 1 s, 404 model_not_found, the lease is
// free. Control: the kill switch (read fresh on every tick) behaves alike.
func TestWatcherSeesAChangeBetweenResolveAndStart(t *testing.T) {
	const tick = 50 * time.Millisecond
	defer oai.SetWatchEvery(tick)()
	for _, tc := range []struct {
		name   string
		change func(*chatRig)
		status int
		code   string
	}{
		{"alias disabled", func(r *chatRig) {
			rows := chatAliases()
			rows[0].enabled = false
			r.reg.reload(r.t, rows)
		}, http.StatusNotFound, oai.CodeModelNotFound},
		{"alias deleted", func(r *chatRig) { r.reg.reload(r.t, chatAliases()[1:]) }, http.StatusNotFound, oai.CodeModelNotFound},
		{"alias disabled during the entry Resolve", nil, http.StatusNotFound, oai.CodeModelNotFound},
		{"kill switch (control)", func(r *chatRig) { r.setSettings(func(s *oai.Settings) { s.Enabled = false }) }, http.StatusNotFound, oai.CodeNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			b := newBlocker()
			defer b.open()
			u.setRespond(b.respond(answerSGLang))
			var rig *chatRig
			rig = newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, func(r *chatRig) {
				if tc.change != nil {
					r.deps.Planner = fuPlanHook{AdmissionPlanner: r.planner, hook: func() { tc.change(rig) }}
					return
				}
				// The change lands inside the entry Resolve: its grant probe
				// (a consumer without all_models) disables the alias after
				// the snapshot the Resolve already took — the generation
				// the watcher starts from must be the one read before.
				ca := urgentApp()
				ca.AllModels = false
				r.ca.Store(ca)
				var once atomic.Bool
				r.deps.HasGrant = func(context.Context, string, string) (bool, error) {
					if once.CompareAndSwap(false, true) {
						rows := chatAliases()
						rows[0].enabled = false
						r.reg.reload(r.t, rows)
					}
					return true, nil
				}
			})
			done := make(chan chatAnswer, 1)
			go func() { done <- rig.post(t, chatBody("chat")) }()
			var a chatAnswer
			select {
			case a = <-done:
			case <-time.After(tick + time.Second):
				b.open()
				a = <-done
				t.Fatalf("the run was not ended within one tick + 1 s of the change (answer after the upstream finished: %d %s)", a.status, a.body)
			}
			code, _, _ := a.envelope(t)
			if a.status != tc.status || code != tc.code {
				t.Fatalf("answer %d %s, want %d %s", a.status, a.body, tc.status, tc.code)
			}
			if held, _ := heldOn(rig.disp, u.srv.URL); held != 0 {
				t.Errorf("held = %d after the abort, want 0", held)
			}
		})
	}
}

// fuRunEnded runs the request n times with the lifecycle ended by the hook
// the planner gets (wrap) and asserts the shutdown answer, no wire contact,
// no llmlog row and every reservation ended by exactly one Abort; maxAcquires
// bounds the acquires the recorder saw (0: the run never reaches one).
func fuRunEnded(t *testing.T, n, maxAcquires int, wrap func(r *chatRig, stop func()) oai.AdmissionPlanner) {
	t.Helper()
	bad := 0
	for i := 0; i < n; i++ {
		u := newUpstream(t, answerSGLang)
		life, cancel := context.WithCancel(context.Background())
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, func(r *chatRig) {
			r.deps.Lifecycle = life
			r.deps.Planner = wrap(r, cancel)
		})
		_ = rig.recorder.Take()
		a := rig.post(t, chatBody("chat"))
		cancel()
		acq := acquires(rig.recorder.Take())
		code := ""
		if len(a.body) > 0 {
			code, _, _ = a.envelope(t)
		}
		l := rig.limiter
		rows := len(rig.rows.all())
		if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || u.hits.Load() != 0 || rows != 0 || acq > maxAcquires ||
			l.admits.Load() != 1 || l.aborts.Load() != 1 || l.settles.Load()+l.rejects.Load() != 0 {
			bad++
			t.Errorf("run %d: %d %s acquires %d wire %d rows %d admits %d settles %d aborts %d rejects %d, want 503 internal_error, ≤ %d acquires, 0/0, one Abort",
				i, a.status, code, acq, u.hits.Load(), rows, l.admits.Load(), l.settles.Load(), l.aborts.Load(), l.rejects.Load(), maxAcquires)
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d runs wrong", bad, n)
	}
}

// TestShutdownAfterASuccessfulPlanWritesNoRow (B-4 = Fremd-Review 4 R4-1): a
// shutdown that lands after a successful Plan used to reach the acquire and
// an attempt on the ended run — no wire contact, but a gateway-chat row
// (finish aborted, 0/0 tokens) and Settle instead of Abort. Now the route's
// verdict after the Plan answers it: 20 of 20 runs without a row.
func TestShutdownAfterASuccessfulPlanWritesNoRow(t *testing.T) {
	fuRunEnded(t, 20, 0, func(r *chatRig, stop func()) oai.AdmissionPlanner {
		return fuPlanHook{AdmissionPlanner: r.planner, hook: stop}
	})
}

// TestShutdownAfterTheAcquireBeforeTheWireWritesNoRow (B-4, the same case
// inside the walk): the run ends after the First acquire granted the lease
// and before the wire call — the attempt does not dial, no row, Abort.
func TestShutdownAfterTheAcquireBeforeTheWireWritesNoRow(t *testing.T) {
	fuRunEnded(t, 20, 1, func(r *chatRig, stop func()) oai.AdmissionPlanner {
		return fuAcquireHook{AdmissionPlanner: r.planner, hook: stop}
	})
}

// TestEveryToolHullIsChecked (B-5 = Fremd-Review 4 R4-3): the hull check of
// tools runs over every element — a case variant in tools[1] and a repeated
// key in tools[17] are 400 like one in tools[0].
func TestEveryToolHullIsChecked(t *testing.T) {
	tool := `{"type":"function","function":{"name":"f"}}`
	for _, tc := range []struct {
		name, tools, code, param string
	}{
		{"tools[1] Type", tool + `,{"type":"function","Type":"x","function":{"name":"g"}}`, oai.CodeUnsupportedParameter, "tools[1].Type"},
		{"tools[17] repeated function", strings.Repeat(tool+",", 17) + `{"type":"function","function":{"name":"g"},"function":{"name":"h"}}`, oai.CodeInvalidJSON, "tools[17].function"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			a := rig.post(t, chatBody("chat", `"tools":[`+tc.tools+`]`))
			code, param, _ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != tc.code || param != tc.param || u.hits.Load() != 0 {
				t.Fatalf("%d %s %s (hits %d), want 400 %s %s, 0 hits", a.status, code, param, u.hits.Load(), tc.code, tc.param)
			}
		})
	}
}
