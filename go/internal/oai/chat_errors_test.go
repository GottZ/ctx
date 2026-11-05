package oai_test

// W02-07 gates G9 (topology and texts: B4, B5, B20, the context-length
// detector, the attempt mapping) and G14 (error-envelope pins, K5/K29)
// against the real chat route of chat_harness_test.go.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// chLogBuffer captures the process logger (slog.Default) for one test.
type chLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *chLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *chLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func chCaptureLog(t *testing.T) *chLogBuffer {
	t.Helper()
	b := &chLogBuffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(b, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return b
}

// chAnswerText is everything of an answer a client sees: status line,
// headers and body.
func chAnswerText(a chatAnswer) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d\n", a.status)
	for k, vs := range a.header {
		fmt.Fprintf(&b, "%s: %s\n", k, strings.Join(vs, ","))
	}
	b.Write(a.body)
	return b.String()
}

func chNoneIn(t *testing.T, where, text string, canaries ...string) {
	t.Helper()
	low := strings.ToLower(text)
	for _, c := range canaries {
		if c != "" && strings.Contains(low, strings.ToLower(c)) {
			t.Errorf("%s carries %q:\n%s", where, c, text)
		}
	}
}

// chIdentityCanaries are the canaries of the answer shapes, the fake's
// response headers and the row's own identity strings.
func chIdentityCanaries(u *upstream, rowName string) []string {
	host := strings.TrimPrefix(u.srv.URL, "http://")
	return []string{"MODEL-CANARY", "HOST-CANARY", "ID-CANARY", "PROVIDER-CANARY", "THINK-CANARY",
		"INSTANCE-CANARY", "LOOP-CANARY", "HEADER-CANARY", rowName, u.srv.URL, host}
}

// chRespond answers status with body (and optional header pairs).
func chRespond(status int, body string, hdr ...string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, _ *http.Request) {
		for i := 0; i+1 < len(hdr); i += 2 {
			w.Header().Set(hdr[i], hdr[i+1])
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// G9 B4: no backend identity reaches the client — in none of the three
// answer shapes and in no upstream 4xx text, with the relay switch off and
// on; no upstream response header is relayed.
func TestNoBackendIdentityInResponse(t *testing.T) {
	const rowName = "rowname-canary"
	for _, relay := range []bool{false, true} {
		for _, shape := range []struct{ name, answer string }{
			{"sglang", answerSGLang}, {"llamacpp", answerLlamaCpp}, {"openrouter", answerOpenRouter},
		} {
			t.Run(fmt.Sprintf("%s/relay=%v", shape.name, relay), func(t *testing.T) {
				u := newUpstream(t, shape.answer)
				r := newChatRig(t, []backends.Backend{u.row(rowName, backends.TrustFull, 10)}, 4, 1)
				r.setSettings(func(s *oai.Settings) { s.RelayUpstream4xxMessages = relay })
				a := r.post(t, chatBody("chat"))
				if a.status != http.StatusOK || !bytes.Contains(a.body, []byte("hello from the engine")) {
					t.Fatalf("status %d: %s", a.status, a.body)
				}
				chNoneIn(t, "the answer", chAnswerText(a), chIdentityCanaries(u, rowName)...)
			})
		}
		t.Run(fmt.Sprintf("upstream400/relay=%v", relay), func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			row := u.row(rowName, backends.TrustFull, 10)
			row.ModelMap = map[string]backends.ModelSpec{"chat": {Model: "MAPPED-CANARY"}}
			r := newChatRig(t, []backends.Backend{row}, 4, 1)
			r.setSettings(func(s *oai.Settings) { s.RelayUpstream4xxMessages = relay })
			host := strings.TrimPrefix(u.srv.URL, "http://")
			text := fmt.Sprintf("%s at %s (%s) rejected model MAPPED-CANARY; see https://internal.example/x from 10.1.2.3, file /srv/models/secret.gguf",
				rowName, u.srv.URL, host)
			u.setRespond(chRespond(http.StatusBadRequest, fmt.Sprintf(`{"error":{"message":%q,"type":"invalid_request_error"}}`, text)))
			a := r.post(t, chatBody("chat"))
			if code, _, _ := a.envelope(t); a.status != http.StatusBadRequest || code != oai.CodeUpstreamRejected {
				t.Fatalf("upstream 400: %d %s, want 400 upstream_rejected", a.status, a.body)
			}
			if relay && !bytes.Contains(a.body, []byte("rejected model")) {
				t.Fatalf("relay on: the sanitized upstream text is missing: %s", a.body)
			}
			chNoneIn(t, "the 400 answer", chAnswerText(a), append(chIdentityCanaries(u, rowName),
				"MAPPED-CANARY", "internal.example", "10.1.2.3", "/srv/models", "secret.gguf")...)
		})
	}
}

// G9 B5: ctx's own row credentials failing upstream are never the client's
// 401/403.
func TestUpstreamAuthNeverRelayedAs401(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		u := newUpstream(t, answerSGLang)
		u.setRespond(chRespond(status, `{"error":{"message":"invalid api key","type":"authentication_error"}}`))
		r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		a := r.post(t, chatBody("chat"))
		if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable {
			t.Fatalf("upstream %d: %d %s, want 503 upstream_unavailable", status, a.status, a.body)
		}
	}
}

// G9 B20: an upstream error body (which can echo the prompt) reaches neither
// the llmlog row — any field, the persisted entry included — nor the log.
func TestOAIErrorNeverPersistsUpstreamBody(t *testing.T) {
	const canary = "PROMPT-CANARY"
	logs := chCaptureLog(t)
	u := newUpstream(t, answerSGLang)
	u.setRespond(chRespond(http.StatusBadRequest, `{"error":{"message":"cannot parse: `+canary+` is not allowed"}}`))
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	if r.settings.Load().BodyLogging != llmlog.BodyLoggingNone {
		t.Fatal("the fixture must log no bodies")
	}
	a := r.post(t, `{"model":"chat","messages":[{"role":"user","content":"say `+canary+`"}]}`)
	if a.status != http.StatusBadRequest {
		t.Fatalf("status %d: %s", a.status, a.body)
	}
	rows := r.rows.all()
	if len(rows) != 1 || rows[0].Err == nil {
		t.Fatalf("rows %+v, want one error row", rows)
	}
	row := rows[0]
	chNoneIn(t, "the gateway row", fmt.Sprintf("%+v", row), canary)
	chNoneIn(t, "the row's error", fmt.Sprintf("%+v %s", *row.Err, row.Err.Error()), canary)
	chNoneIn(t, "the persisted entry", fmt.Sprintf("%+v", llmlog.GatewayEntry(row)), canary)
	chNoneIn(t, "the client answer", chAnswerText(a), canary)
	chNoneIn(t, "the log", logs.String(), canary)
}

// TestContextLengthDetectorFixtures: the engines' context-length wordings
// map to context_length_exceeded with the ALIAS's number (chat: 262144),
// never the upstream's; another 400 text is upstream_rejected.
func TestContextLengthDetectorFixtures(t *testing.T) {
	for _, f := range []struct{ engine, body, want string }{
		{"vllm", `{"object":"error","message":"This model's maximum context length is 32768 tokens. However, you requested 40000 tokens (39000 in the messages, 1000 in the completion). Please reduce the length of the messages or completion.","type":"BadRequestError","param":null,"code":400}`, oai.CodeContextLengthExceeded},
		{"sglang", `{"object":"error","message":"The input (40000 tokens) is longer than the model's context length (32768 tokens).","type":"BadRequestError","param":null,"code":400}`, oai.CodeContextLengthExceeded},
		{"llamacpp", `{"error":{"code":400,"message":"the request exceeds the available context size, try increasing it","type":"exceed_context_size_error"}}`, oai.CodeContextLengthExceeded},
		{"openrouter", `{"error":{"message":"This endpoint's maximum context length is 32768 tokens. However, you requested about 40000 tokens (39000 of text input, 1000 in the output). Please reduce the length of either one.","code":400}}`, oai.CodeContextLengthExceeded},
		{"control", `{"error":{"message":"temperature must be between 0 and 2","code":400}}`, oai.CodeUpstreamRejected},
	} {
		t.Run(f.engine, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			u.setRespond(chRespond(http.StatusBadRequest, f.body))
			r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
			a := r.post(t, chatBody("chat"))
			code, _, _ := a.envelope(t)
			if a.status != http.StatusBadRequest || code != f.want {
				t.Fatalf("%d %s, want 400 %s", a.status, a.body, f.want)
			}
			if f.want == oai.CodeContextLengthExceeded && !bytes.Contains(a.body, []byte("262144")) {
				t.Fatalf("the text does not carry the alias's context_length: %s", a.body)
			}
			chNoneIn(t, "the answer", string(a.body), "32768", "40000")
		})
	}
}

// chSlowAliases adds a _global chat alias "slow" with timeout_s 1.
func chSlowAliases() []chatAlias {
	return append(chatAliases(), chatAlias{"00000000-0000-4000-8000-0000000c4a7f", backends.GlobalScope, "slow", "chat", "chat",
		backends.SensCredentials, `{"timeout_s":1}`, true})
}

// G9: the attempt mapping of upstream 500, a wire timeout and an exhausted
// chain (design/02 §4.10.2).
func TestAttemptFailureMapping(t *testing.T) {
	t.Run("upstream500", func(t *testing.T) {
		u := newUpstream(t, answerSGLang)
		u.setRespond(chRespond(http.StatusInternalServerError, `{"error":{"message":"boom"}}`))
		r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		a := r.post(t, chatBody("chat"))
		if code, _, typ := a.envelope(t); a.status != http.StatusInternalServerError || code != oai.CodeUpstreamError || typ != oai.TypeServer {
			t.Fatalf("%d %s, want 500 upstream_error", a.status, a.body)
		}
	})
	t.Run("timeout", func(t *testing.T) {
		u := newUpstream(t, answerSGLang)
		u.setRespond(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		})
		r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
		r.reg.reload(t, chSlowAliases())
		start := time.Now()
		a := r.post(t, chatBody("slow"))
		if code, _, _ := a.envelope(t); a.status != http.StatusGatewayTimeout || code != oai.CodeUpstreamTimeout {
			t.Fatalf("%d %s, want 504 upstream_timeout", a.status, a.body)
		}
		if d := time.Since(start); d > 2500*time.Millisecond {
			t.Fatalf("timeout_s 1 answered after %v", d)
		}
	})
	t.Run("exhausted", func(t *testing.T) {
		u1 := newUpstream(t, answerSGLang)
		u2 := newUpstream(t, answerSGLang)
		u1.setRespond(chRespond(http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, "Retry-After", "7"))
		u2.setRespond(chRespond(http.StatusServiceUnavailable, `{"error":{"message":"overloaded"}}`, "Retry-After", "3"))
		r := newChatRig(t, []backends.Backend{u1.row("a", backends.TrustFull, 20), u2.row("b", backends.TrustFull, 10)}, 4, 1)
		a := r.post(t, chatBody("chat"))
		if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeUpstreamUnavailable {
			t.Fatalf("%d %s, want 503 upstream_unavailable", a.status, a.body)
		}
		if got := a.header.Get("Retry-After"); got != "3" {
			t.Fatalf("Retry-After = %q, want the smallest upstream value 3", got)
		}
		if u1.hits.Load() != 1 || u2.hits.Load() != 1 {
			t.Fatalf("hits %d/%d, want both rows tried once", u1.hits.Load(), u2.hits.Load())
		}
	})
}

// chInternal asserts the K29 envelope: 503 internal_error, type
// service_unavailable_error, x-should-retry false.
func chInternal(t *testing.T, what string, a chatAnswer) {
	t.Helper()
	code, _, typ := a.envelope(t)
	if a.status != http.StatusServiceUnavailable || code != oai.CodeInternalError || typ != oai.TypeServiceUnavailable {
		t.Fatalf("%s: %d %s, want 503 internal_error service_unavailable_error", what, a.status, a.body)
	}
	if got := a.header.Get("x-should-retry"); got != "false" {
		t.Fatalf("%s: x-should-retry = %q, want false", what, got)
	}
}

// G14: no planner ⇒ 503 no_eligible_backend before any acquire; no
// fallback class.
func TestPlannerNilRefusesNoEligible(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Planner = nil }))
	a := r.post(t, chatBody("chat"))
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeNoEligibleBackend {
		t.Fatalf("%d %s, want 503 no_eligible_backend", a.status, a.body)
	}
	if u.hits.Load() != 0 || len(r.serveBy.all()) != 0 {
		t.Fatalf("hits %d acquires %d, want 0 and 0", u.hits.Load(), len(r.serveBy.all()))
	}
	if r.limiter.aborts.Load() != 1 || r.limiter.settles.Load() != 0 {
		t.Fatalf("limiter aborts %d settles %d, want 1 and 0", r.limiter.aborts.Load(), r.limiter.settles.Load())
	}
}

// G14: no limiter ⇒ 503 service_unavailable with the largest Retry-After
// step of dispatch.external_retry_after_steps.
func TestLimiterNilRefusesServiceUnavailable(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withDeps(func(d *oai.Deps) { d.Limiter = nil }))
	a := r.post(t, chatBody("chat"))
	if code, _, _ := a.envelope(t); a.status != http.StatusServiceUnavailable || code != oai.CodeServiceUnavailable {
		t.Fatalf("%d %s, want 503 service_unavailable", a.status, a.body)
	}
	steps := r.disp.ExternalRetryAfterSteps()
	if len(steps) == 0 {
		t.Fatal("the dispatcher has no Retry-After steps")
	}
	want := strconv.Itoa(int(math.Ceil(steps[len(steps)-1].Seconds())))
	if got := a.header.Get("Retry-After"); got != want {
		t.Fatalf("Retry-After = %q, want the largest step %s", got, want)
	}
	if u.hits.Load() != 0 {
		t.Fatalf("hits %d, want 0", u.hits.Load())
	}
}

// chFaultPlanner wraps the real planner: its acquirer's First fails with
// err, its class list can be replaced, its chain can be replaced.
type chFaultPlanner struct {
	oai.AdmissionPlanner
	err     error
	classes []dispatch.Class
	chain   []backends.Backend
}

func (p chFaultPlanner) AllowedClasses() []dispatch.Class {
	if p.classes != nil {
		return p.classes
	}
	return p.AdmissionPlanner.AllowedClasses()
}

func (p chFaultPlanner) Plan(ctx context.Context, ca *auth.ConsumerAuth, in oai.AdmissionInput) (oai.Plan, error) {
	plan, err := p.AdmissionPlanner.Plan(ctx, ca, in)
	if err != nil {
		return plan, err
	}
	if p.err != nil {
		plan.Acquirer = chFaultAcquirer{RequestAcquirer: plan.Acquirer, err: p.err}
	}
	if p.chain != nil {
		plan.Chain = p.chain
	}
	return plan, nil
}

type chFaultAcquirer struct {
	oai.RequestAcquirer
	err error
}

func (a chFaultAcquirer) First(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, &dispatch.AdmissionError{Err: a.err}
}

// G14 K29: the code-bug sentinels of the admission are 503 internal_error,
// never retried.
func TestAdmissionSentinelsAreInternal(t *testing.T) {
	for _, c := range []struct {
		name string
		p    func(oai.AdmissionPlanner) chFaultPlanner
	}{
		{"ErrClassUnauthorized", func(in oai.AdmissionPlanner) chFaultPlanner {
			return chFaultPlanner{AdmissionPlanner: in, err: dispatch.ErrClassUnauthorized}
		}},
		{"ErrPrincipalConflict", func(in oai.AdmissionPlanner) chFaultPlanner {
			return chFaultPlanner{AdmissionPlanner: in, err: dispatch.ErrPrincipalConflict}
		}},
		{"ErrInvalidClass", func(in oai.AdmissionPlanner) chFaultPlanner {
			return chFaultPlanner{AdmissionPlanner: in, err: dispatch.ErrInvalidClass}
		}},
		{"ErrWalkClass", func(in oai.AdmissionPlanner) chFaultPlanner {
			return chFaultPlanner{AdmissionPlanner: in, classes: []dispatch.Class{dispatch.ClassBackground}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := newUpstream(t, answerSGLang)
			r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1,
				withDeps(func(d *oai.Deps) { d.Planner = c.p(d.Planner) }))
			chInternal(t, c.name, r.post(t, chatBody("chat")))
			if u.hits.Load() != 0 {
				t.Fatalf("hits %d, want 0", u.hits.Load())
			}
		})
	}
	t.Run("ErrPassthroughProtocol", func(t *testing.T) {
		ant := newUpstream(t, answerSGLang)
		oa := newUpstream(t, answerSGLang)
		antRow := ant.row("anthropic-row", backends.TrustFull, 30)
		antRow.Protocol = backends.ProtocolAnthropic
		r := newChatRig(t, []backends.Backend{antRow, oa.row("openai-row", backends.TrustFull, 10)}, 4, 1,
			withDeps(func(d *oai.Deps) {
				d.Planner = chFaultPlanner{AdmissionPlanner: d.Planner, chain: []backends.Backend{antRow}}
			}))
		chInternal(t, "ErrPassthroughProtocol", r.post(t, chatBody("chat")))
		if ant.hits.Load() != 0 || oa.hits.Load() != 0 {
			t.Fatalf("hits %d/%d, want 0/0", ant.hits.Load(), oa.hits.Load())
		}
	})
}

// G14: a failed grant read is 503 internal_error — and a ctx-B in front of
// this instance (backends.Classify over the answer) goes to its next row.
func TestGrantReadErrorIsInternalAndFailsOver(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	ca := urgentApp()
	ca.AllModels = false
	r.ca.Store(ca)
	grantErr := errors.New("grants: connection refused")
	r.grantErr.Store(&grantErr)
	a := r.post(t, chatBody("chat"))
	chInternal(t, "grant read error", a)
	if cls := backends.Classify(&httpx.StatusError{Code: a.status, Body: string(a.body)}, ""); !cls.Next() {
		t.Fatalf("a ctx-B classifies the answer as %s (Next false), want failover", cls)
	}
	if u.hits.Load() != 0 {
		t.Fatalf("hits %d, want 0", u.hits.Load())
	}
}

// chDirect serves the chat handler without the /v1 stack (the handler's own
// invariant check is the subject; PeerIngress in front refuses a nil
// consumer itself).
func chDirect(t *testing.T, h http.Handler, ca *auth.ConsumerAuth, method, path, body string) chatAnswer {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if ca != nil {
		req = req.WithContext(context.WithValue(req.Context(), consumerKey{}, ca))
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)
	return chatAnswer{status: w.Code, header: w.Header(), body: w.Body.Bytes()}
}

// G14: an invariant broken behind the door — a knowledge AuthResult, no
// tenant, no consumer — is 503 internal_error with an ERROR line.
func TestInvariantBreakIsInternalWithError(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	knowledge := r.deps
	knowledge.KnowledgeAuth = func(context.Context) bool { return true }
	for _, c := range []struct {
		name string
		d    oai.Deps
		ca   *auth.ConsumerAuth
	}{
		{"knowledge auth", knowledge, urgentApp()},
		{"no tenant", r.deps, chNoTenant()},
		{"no consumer", r.deps, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			logs := chCaptureLog(t)
			a := chDirect(t, oai.ChatCompletions(c.d), c.ca, http.MethodPost, "/v1/chat/completions", chatBody("chat"))
			chInternal(t, c.name, a)
			if !strings.Contains(logs.String(), "level=ERROR") {
				t.Fatalf("no ERROR line: %q", logs.String())
			}
		})
	}
	if u.hits.Load() != 0 || r.gate.calls.Load() != 0 {
		t.Fatalf("hits %d gate calls %d, want 0", u.hits.Load(), r.gate.calls.Load())
	}
}

// G14 (A03-W8 contract form): a missing inference dependency takes the
// inference route down (503 + ERROR), never GET /v1/models.
func TestMissingPoolSparesModelList(t *testing.T) {
	u := newUpstream(t, answerSGLang)
	r := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1)
	d := r.deps
	d.Pool = nil
	logs := chCaptureLog(t)
	chInternal(t, "Pool nil", chDirect(t, oai.ChatCompletions(d), urgentApp(), http.MethodPost, "/v1/chat/completions", chatBody("chat")))
	if !strings.Contains(logs.String(), "level=ERROR") || !strings.Contains(logs.String(), "Pool") {
		t.Fatalf("no ERROR naming Pool: %q", logs.String())
	}
	m := chDirect(t, oai.ListModels(d), urgentApp(), http.MethodGet, "/v1/models", "")
	if m.status != http.StatusOK || !bytes.Contains(m.body, []byte(`"object":"list"`)) {
		t.Fatalf("GET /v1/models with Pool nil: %d %s, want 200", m.status, m.body)
	}
}
