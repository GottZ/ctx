package events

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestJanitorArmsContainPanics is the T04-10 red probe, written against the
// PRE-wave tree so that it proves something: green before the wave, green
// after it, and blind to HOW the recover is spelled — an inline closure today,
// guardPanic afterwards.
//
// What it pins is the wave's one silent failure mode (design/04 §5.5).
// recover() only yields the panic value when it is called DIRECTLY by the
// deferred function. `defer guardPanic("x")` satisfies that; `defer func() {
// guardPanic("x") }()` does not — there the closure is the deferred function,
// guardPanic sits one frame lower, recover() returns nil, and the panic walks
// out of the arm and takes the whole ctxd process down with it. That mistake
// compiles, lints and passes every test that never panics. Here it does not:
// the arm's panic escapes into this test's own recover and the case fails by
// name.
//
// Panic source is a nil *pgxpool.Pool — pgxpool.Pool.Exec dereferences its
// receiver before it can return an error, a property overview_worker_test.go
// already relies on. Eight arms reach their store call and deref it there;
// llmlog retention cannot, because llmlog.EvictBodies returns early on
// `pool == nil` (llmlog/llmlog.go:129). That one gets a nil config store
// instead and panics a step earlier, in the hot-config read — same arm, same
// guard, same assertion.
func TestJanitorArmsContainPanics(t *testing.T) {
	// Non-zero retention everywhere: the four retention arms short-circuit
	// before touching the pool when their window is 0.
	withCfg := func() *config.Store {
		c := &config.Config{}
		c.WebChat.SessionRetention = 24
		c.Project.Webhook.Retention = 24
		c.Writes.ConfirmRetention = 24
		c.RecallCheck.RetentionDays = 365
		return config.NewStore(c)
	}

	cases := []struct {
		arm     string
		wantLog string // the ERROR message byte for byte, without its attributes
		cfg     *config.Store
		run     func(*Scheduler, context.Context)
	}{
		{"embed cache eviction", "scheduler: panic in embed cache eviction", withCfg(), (*Scheduler).runEmbedCacheEviction},
		{"llmlog retention", "scheduler: panic in llmlog retention", nil, (*Scheduler).runLLMLogRetention},
		{"webchat retention", "scheduler: panic in webchat retention", withCfg(), (*Scheduler).runWebChatRetention},
		{"webhook retention", "scheduler: panic in webhook retention", withCfg(), (*Scheduler).runWebhookRetention},
		{"oauth code gc", "scheduler: panic in oauth code gc", withCfg(), (*Scheduler).runOAuthCodeGC},
		{"oauth token gc", "scheduler: panic in oauth token gc", withCfg(), (*Scheduler).runOAuthTokenGC},
		{"sso state gc", "scheduler: panic in sso state gc", withCfg(), (*Scheduler).runSSOStateGC},
		{"recall retention", "scheduler: panic in recall retention", withCfg(), (*Scheduler).runRecallRetention},
		{"pending-write eviction", "scheduler: panic in pending-write eviction", withCfg(), (*Scheduler).runPendingWriteEviction},
	}

	for _, tc := range cases {
		t.Run(tc.arm, func(t *testing.T) {
			var logBuf bytes.Buffer
			prev := slog.Default()
			slog.SetDefault(slog.New(slog.NewTextHandler(&logBuf, nil)))
			defer slog.SetDefault(prev)

			s := &Scheduler{cfg: tc.cfg}
			escaped := true
			func() {
				defer func() {
					if r := recover(); r != nil {
						t.Errorf("arm %q let the panic escape its recover — in ctxd that kills the process, not the arm: %v", tc.arm, r)
					}
				}()
				tc.run(s, context.Background())
				escaped = false
			}()
			if escaped {
				return
			}

			got := logBuf.String()
			if !strings.Contains(got, tc.wantLog) {
				t.Fatalf("arm %q recovered but logged the wrong line\nwant substring: %s\ngot: %s", tc.arm, tc.wantLog, got)
			}
			if !strings.Contains(got, "level=ERROR") {
				t.Fatalf("arm %q logged the panic below ERROR; got: %s", tc.arm, got)
			}
			if !strings.Contains(got, "stack=") {
				t.Fatalf("arm %q logged the panic without a stack attribute; got: %s", tc.arm, got)
			}
		})
	}
}

// TestJanitorArmContract pins what janitorArm renders for each of the nine arms:
// the success line with its count KEY and its attribute order, the failure line
// derived from the arm name, and the silence when a sweep removed nothing.
//
// The nine rows below mirror the parameters the arm methods pass, and the
// wantFail strings are the WARN literals the arms carried before the fold —
// they are runbook text, so they are spelled out here rather than derived a
// second time.
//
// This is also where runPendingWriteEviction's "chunks" key gets its first
// runtime cover. That arm owns a minute ticker instead of riding
// runSixHourJanitor, so no integration test drives it; before the fold its key
// was held only by a grep for `"chunks", dropped`, and the local variable that
// literal named is gone by construction once the count comes back from a
// closure.
func TestJanitorArmContract(t *testing.T) {
	arms := []struct {
		name     string
		doneMsg  string
		countKey string
		attrs    []any
		wantDone string
		wantFail string
	}{
		{"embed cache eviction", "scheduler: embed cache evicted", "rows", nil,
			`msg="scheduler: embed cache evicted" rows=7`, `msg="scheduler: embed cache eviction failed"`},
		{"llmlog retention", "scheduler: llmlog bodies evicted", "rows", []any{"retention_days", 90},
			`msg="scheduler: llmlog bodies evicted" rows=7 retention_days=90`, `msg="scheduler: llmlog retention failed"`},
		{"webchat retention", "scheduler: webchat sessions deleted", "rows", []any{"retention_hours", float64(24)},
			`msg="scheduler: webchat sessions deleted" rows=7 retention_hours=24`, `msg="scheduler: webchat retention failed"`},
		{"webhook retention", "scheduler: webhook events evicted", "rows", []any{"retention_hours", float64(24)},
			`msg="scheduler: webhook events evicted" rows=7 retention_hours=24`, `msg="scheduler: webhook retention failed"`},
		{"oauth code gc", "scheduler: oauth codes evicted", "rows", nil,
			`msg="scheduler: oauth codes evicted" rows=7`, `msg="scheduler: oauth code gc failed"`},
		{"oauth token gc", "scheduler: oauth tokens evicted", "rows", nil,
			`msg="scheduler: oauth tokens evicted" rows=7`, `msg="scheduler: oauth token gc failed"`},
		{"sso state gc", "scheduler: sso states evicted", "rows", nil,
			`msg="scheduler: sso states evicted" rows=7`, `msg="scheduler: sso state gc failed"`},
		{"recall retention", "scheduler: recall runs evicted", "rows", []any{"retention_days", 365},
			`msg="scheduler: recall runs evicted" rows=7 retention_days=365`, `msg="scheduler: recall retention failed"`},
		{"pending-write eviction", "scheduler: pending-write chunks dropped", "chunks", []any{"retention_hours", float64(24)},
			`msg="scheduler: pending-write chunks dropped" chunks=7 retention_hours=24`, `msg="scheduler: pending-write eviction failed"`},
	}

	capture := func(t *testing.T, fn func()) string {
		t.Helper()
		var buf bytes.Buffer
		prev := slog.Default()
		slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
		defer slog.SetDefault(prev)
		fn()
		return buf.String()
	}

	for _, a := range arms {
		t.Run(a.name, func(t *testing.T) {
			swept := capture(t, func() {
				janitorArm(context.Background(), a.name, a.doneMsg, a.countKey,
					func(context.Context) (int64, error) { return 7, nil }, a.attrs...)
			})
			if !strings.Contains(swept, a.wantDone) {
				t.Errorf("success line\nwant substring: %s\ngot: %s", a.wantDone, swept)
			}

			failed := capture(t, func() {
				janitorArm(context.Background(), a.name, a.doneMsg, a.countKey,
					func(context.Context) (int64, error) { return 0, errors.New("boom") }, a.attrs...)
			})
			want := fmt.Sprintf("level=WARN %s error=boom", a.wantFail)
			if !strings.Contains(failed, want) {
				t.Errorf("failure line\nwant substring: %s\ngot: %s", want, failed)
			}
			if strings.Contains(failed, a.doneMsg) {
				t.Errorf("a failed sweep still logged the success line: %s", failed)
			}

			empty := capture(t, func() {
				janitorArm(context.Background(), a.name, a.doneMsg, a.countKey,
					func(context.Context) (int64, error) { return 0, nil }, a.attrs...)
			})
			if empty != "" {
				t.Errorf("an empty sweep must stay silent; got: %s", empty)
			}
		})
	}
}
