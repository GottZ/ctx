package oai_test

// Pins of Fremd-Review 2 F2-4 (Härtung W02-07) against the real chat route
// of chat_harness_test.go: each pins one property a surviving mutant of the
// review's mutation run showed unpinned — the inference-dependency check per
// term (IM-*), the request frame (D7), the write deadline per admission
// (D11) and two rows of the attempt mapping (U3, U4).

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/oai"
)

// F2-4 IM-*: every term of inferenceMissing on its own — the field nil ⇒
// 503 internal_error, one ERROR line naming exactly that field, and nothing
// served: no wire attempt and no tenant gate (a missing Record would
// otherwise serve the request and lose its row).
func TestInferenceMissingRefusesPerTerm(t *testing.T) {
	for _, c := range []struct {
		name  string
		unset func(*oai.Deps)
	}{
		{"Settings", func(d *oai.Deps) { d.Settings = nil }},
		{"Pool", func(d *oai.Deps) { d.Pool = nil }},
		{"Quota", func(d *oai.Deps) { d.Quota = nil }},
		{"Federation.Floor", func(d *oai.Deps) { d.Federation.Floor = nil }},
		{"Federation.Scan", func(d *oai.Deps) { d.Federation.Scan = nil }},
		{"Federation.Filter", func(d *oai.Deps) { d.Federation.Filter = nil }},
		{"TenantSensitivity", func(d *oai.Deps) { d.TenantSensitivity = nil }},
		{"Record", func(d *oai.Deps) { d.Record = nil }},
		{"MaxWait", func(d *oai.Deps) { d.MaxWait = nil }},
		{"RetryAfterSteps", func(d *oai.Deps) { d.RetryAfterSteps = nil }},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			d := r.deps
			c.unset(&d)
			logs := chCaptureLog(t)
			// Through the /v1 stack (oai.Recover in front): a term that
			// slipped out of the check panics later, and the recovered
			// answer must not pass for the check's own.
			h := r.mount(oai.ChatCompletions(d))
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatBody("chat")))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Errorf("%s nil: the handler panicked (%v) — the field slipped past the check", c.name, p)
					}
				}()
				h.ServeHTTP(w, req)
			}()
			chInternal(t, c.name+" nil", chatAnswer{status: w.Code, header: w.Result().Header, body: w.Body.Bytes()})
			var named bool
			for _, line := range strings.Split(logs.String(), "\n") {
				if strings.Contains(line, "level=ERROR") && strings.Contains(line, " missing="+c.name+" ") {
					named = true
				}
			}
			if !named {
				t.Errorf("no ERROR line with missing=%s:\n%s", c.name, logs.String())
			}
			if u.hits.Load() != 0 || r.gate.calls.Load() != 0 || len(r.rows.all()) != 0 {
				t.Errorf("%s nil: wire hits %d, gate calls %d, rows %d — want 0/0/0 (refused before anything is served)",
					c.name, u.hits.Load(), r.gate.calls.Load(), len(r.rows.all()))
			}
		})
	}
}

// pinHangingUpstream answers only when its request ends (or after 3 s).
func pinHangingUpstream(t *testing.T) *upstream {
	t.Helper()
	u := newUpstream(t, answerSGLang)
	u.setRespond(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	return u
}

// F2-4 D7: the request frame is gateway.request_deadline, not
// gateway.request_timeout — a short deadline next to a long timeout ends
// the request at the deadline (504 upstream_timeout), and the acquire's
// ServeBy is entry + request_deadline.
func TestRequestDeadlineIsTheRequestFrame(t *testing.T) {
	const deadline = 400 * time.Millisecond
	u := pinHangingUpstream(t)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	r.setSettings(func(s *oai.Settings) { s.RequestDeadline, s.RequestTimeout = deadline, 60*time.Second })
	start := time.Now()
	a := r.post(t, chatBody("chat"))
	took := time.Since(start)
	if code, _, _ := a.envelope(t); a.status != http.StatusGatewayTimeout || code != oai.CodeUpstreamTimeout {
		t.Fatalf("%d %s, want 504 upstream_timeout at the request deadline", a.status, a.body)
	}
	if took > 2*time.Second {
		t.Fatalf("answered after %v, want ≈ request_deadline %v", took, deadline)
	}
	sb := r.serveBy.all()
	if len(sb) != 1 {
		t.Fatalf("acquires %d, want 1", len(sb))
	}
	// entry ≥ start; the slack covers a loaded -race run (the mutant is 60 s off).
	if off := sb[0].Sub(start); off < deadline || off > deadline+time.Second {
		t.Fatalf("ServeBy = entry + %v, want entry + request_deadline %v", off, deadline)
	}
}

// F2-4 D11: the write deadline set at each admission is the rest of the
// request frame PLUS writeMargin — a 504 upstream_timeout written at the end
// of the frame reaches a client over a real net/http connection (without the
// margin the deadline has passed when it is written: the client sees a
// broken connection).
func TestDeadlineTimeoutReachesTheClientOverRealHTTP(t *testing.T) {
	u := pinHangingUpstream(t)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	r.setSettings(func(s *oai.Settings) { s.RequestDeadline = 300 * time.Millisecond })
	srv := httptest.NewServer(r.router)
	t.Cleanup(srv.Close)
	resp, err := srv.Client().Post(srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(chatBody("chat")))
	if err != nil {
		t.Fatalf("client: %v (the answer was written after the write deadline)", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("client body: %v (the answer was cut by the write deadline)", err)
	}
	a := chatAnswer{status: resp.StatusCode, header: resp.Header, body: body}
	if code, _, _ := a.envelope(t); a.status != http.StatusGatewayTimeout || code != oai.CodeUpstreamTimeout {
		t.Fatalf("%d %s, want 504 upstream_timeout", a.status, a.body)
	}
}

// F2-4 U3 (design/02 §4.10.2 "Kette erschöpft … letzter Versuch 404"): a
// chain whose last attempt answered 404 is exhausted — 503
// upstream_unavailable, retryable — never 400 upstream_rejected.
func TestLastAttempt404IsUpstreamUnavailable(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	u.setRespond(chRespond(http.StatusNotFound, `{"error":{"message":"model not found"}}`))
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	a := r.post(t, chatBody("chat"))
	if code, _, typ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable || typ != oai.TypeServiceUnavailable {
		t.Fatalf("%d %s, want 503 upstream_unavailable", a.status, a.body)
	}
	if got := a.header.Get("x-should-retry"); got != "true" {
		t.Fatalf("x-should-retry = %q, want true", got)
	}
	if u.hits.Load() != 1 {
		t.Fatalf("wire hits %d, want 1", u.hits.Load())
	}
}

// pinAbortPlanner wraps the real planner: the first lease's run context is
// ended with cause (the dispatcher's reaper or preemption, design/03 §4.5).
type pinAbortPlanner struct {
	oai.AdmissionPlanner
	cause error
}

func (p pinAbortPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err == nil {
		plan.Acquirer = pinAbortAcquirer{RequestAcquirer: plan.Acquirer, cause: p.cause}
	}
	return plan, err
}

type pinAbortAcquirer struct {
	oai.RequestAcquirer
	cause error
}

func (a pinAbortAcquirer) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	lease, runCtx, err := a.RequestAcquirer.First(ctx, b, role, timeout)
	if err != nil {
		return lease, runCtx, err
	}
	rc, cancel := context.WithCancelCause(runCtx)
	cancel(a.cause)
	return lease, rc, nil
}

// F2-4 U4 (design/02 §4.10.2 "Lease vom Reaper beendet"): a lease the
// dispatcher ended is an A-internal outcome a retry can fix — 503
// internal_error with x-should-retry: true.
func TestDispatcherEndedLeaseIsRetryableInternal(t *testing.T) {
	for name, cause := range map[string]error{"reaped": dispatch.ErrReaped, "preempted": dispatch.ErrPreempted} {
		t.Run(name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
				withDeps(func(d *oai.Deps) { d.Planner = pinAbortPlanner{AdmissionPlanner: d.Planner, cause: cause} }))
			a := r.post(t, chatBody("chat"))
			if code, _, typ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || typ != oai.TypeServiceUnavailable {
				t.Fatalf("%d %s, want 503 internal_error", a.status, a.body)
			}
			if got := a.header.Get("x-should-retry"); got != "true" {
				t.Fatalf("x-should-retry = %q, want true (a retry can fix a %s lease)", got, name)
			}
			rows := r.rows.all()
			if len(rows) != 1 || len(rows[0].Attempts) != 1 || rows[0].Attempts[0].Class != name {
				raw, _ := json.Marshal(rows)
				t.Fatalf("rows %s, want one row whose one attempt has class %s", raw, name)
			}
		})
	}
}
