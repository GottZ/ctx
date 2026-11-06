package oai_test

// The harness of the stream route (W02-08): fake upstream streams in the
// three engine shapes of the live chat chain (SGLang, llama.cpp,
// OpenRouter), a scriptable SSE responder, a stream rig on top of the chat
// rig (chat_harness_test.go — reused, not moved), a real HTTP server around
// it (deadlines, flush and hijack need a real connection), an SSE client
// that timestamps every event, a byte clock on the response writer and an
// acquire clock on the dispatcher (the byte invariant, K7).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
)

// The engine text of every shape: three deltas, three completion tokens in
// the usage chunk.
var streamDeltas = []string{"hello", " from", " the engine"}

const streamText = "hello from the engine"

// sglangChunk, llamaChunk and openrouterChunk render one content chunk of
// their engine (canaries in every field that is not model output).
func sglangChunk(content, finish string) string {
	return fmt.Sprintf(`{"id":"ID-CANARY-sglang","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY",`+
		`"choices":[{"index":0,"delta":{"role":null,"content":%q,"reasoning_content":null,"tool_calls":null},"logprobs":null,`+
		`"finish_reason":%s,"matched_stop":null}],"usage":null}`, content, jsonOrNull(finish))
}

const sglangThink = `{"id":"ID-CANARY-sglang","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY",` +
	`"choices":[{"index":0,"delta":{"role":"assistant","content":null,"reasoning_content":"THINK-CANARY","tool_calls":null},` +
	`"logprobs":null,"finish_reason":null,"matched_stop":null}],"usage":null}`

const sglangUsage = `{"id":"ID-CANARY-sglang","object":"chat.completion.chunk","created":1700000000,"model":"MODEL-CANARY",` +
	`"choices":[],"usage":{"prompt_tokens":11,"total_tokens":14,"completion_tokens":3,"prompt_tokens_details":null}}`

func llamaChunk(content, finish string) string {
	return fmt.Sprintf(`{"choices":[{"finish_reason":%s,"index":0,"delta":{"content":%q}}],"created":1700000000,`+
		`"id":"chatcmpl-ID-CANARY","model":"MODEL-CANARY","system_fingerprint":"b6000-HOST-CANARY","object":"chat.completion.chunk"}`,
		jsonOrNull(finish), content)
}

const llamaUsage = `{"choices":[],"created":1700000000,"id":"chatcmpl-ID-CANARY","model":"MODEL-CANARY",` +
	`"system_fingerprint":"b6000-HOST-CANARY","object":"chat.completion.chunk",` +
	`"usage":{"completion_tokens":3,"prompt_tokens":11,"total_tokens":14},"timings":{"prompt_n":11,"predicted_n":3,"prompt_ms":1.5}}`

func openrouterChunk(content, finish string) string {
	return fmt.Sprintf(`{"id":"gen-ID-CANARY","provider":"PROVIDER-CANARY","model":"MODEL-CANARY","object":"chat.completion.chunk",`+
		`"created":1700000000,"choices":[{"index":0,"delta":{"role":"assistant","content":%q,"reasoning":null},"finish_reason":%s,`+
		`"native_finish_reason":%s,"logprobs":null}]}`, content, jsonOrNull(finish), jsonOrNull(finish))
}

const openrouterUsage = `{"id":"gen-ID-CANARY","provider":"PROVIDER-CANARY","model":"MODEL-CANARY","object":"chat.completion.chunk",` +
	`"created":1700000000,"choices":[],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14,"cost":0.00042}}`

func jsonOrNull(s string) string {
	if s == "" {
		return "null"
	}
	return fmt.Sprintf("%q", s)
}

// streamShape is one engine family's stream: its content chunks (the last
// carries finish_reason stop), its usage chunk, its leading comment.
type streamShape struct {
	name    string
	chunk   func(content, finish string) string
	usage   string
	comment string // a leading SSE comment the engine sends ("" = none)
}

var streamShapes = []streamShape{
	{"sglang", sglangChunk, sglangUsage, ""},
	{"llamacpp", llamaChunk, llamaUsage, ""},
	{"openrouter", openrouterChunk, openrouterUsage, ": OPENROUTER PROCESSING\n\n"},
}

// sseStep is one step of a scripted upstream stream.
type sseStep struct {
	wait time.Duration   // sleep first (the request's end cuts it short)
	hold <-chan struct{} // then block until closed (or the request ends)
	data string          // then a data frame
	raw  string          // or raw bytes as they are
	// drop ends the response by closing the connection (a broken stream).
	drop bool
}

// steps renders the shape's full stream: comment, content chunks, usage
// chunk, [DONE].
func (s streamShape) steps() []sseStep {
	var out []sseStep
	if s.comment != "" {
		out = append(out, sseStep{raw: s.comment})
	}
	for i, d := range streamDeltas {
		fin := ""
		if i == len(streamDeltas)-1 {
			fin = "stop"
		}
		out = append(out, sseStep{data: s.chunk(d, fin)})
	}
	return append(out, sseStep{data: s.usage}, sseStep{data: "[DONE]"})
}

// sseRespond answers a request with the scripted stream. entered (optional)
// gets one value when the request arrives; ended (optional) is closed when
// the handler returns, with how it ended ("canceled" when the request's
// context ended first).
func sseRespond(steps []sseStep, entered chan<- struct{}, ended *upstreamEnd) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		if entered != nil {
			entered <- struct{}{}
		}
		canceled := false
		defer func() {
			if ended != nil {
				ended.mark(canceled)
			}
		}()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		rc := http.NewResponseController(w)
		_ = rc.Flush()
		for _, s := range steps {
			if s.wait > 0 {
				select {
				case <-time.After(s.wait):
				case <-r.Context().Done():
					canceled = true
					return
				}
			}
			if s.hold != nil {
				select {
				case <-s.hold:
				case <-r.Context().Done():
					canceled = true
					return
				}
			}
			if s.drop {
				if conn, _, err := rc.Hijack(); err == nil {
					_ = conn.Close()
				}
				return
			}
			if s.data != "" {
				if _, err := fmt.Fprintf(w, "data: %s\n\n", s.data); err != nil {
					canceled = true
					return
				}
			}
			if s.raw != "" {
				if _, err := io.WriteString(w, s.raw); err != nil {
					canceled = true
					return
				}
			}
			_ = rc.Flush()
		}
	}
}

// upstreamEnd records how and when an upstream handler ended.
type upstreamEnd struct {
	once     sync.Once
	done     chan struct{}
	at       time.Time
	canceled bool
}

func newUpstreamEnd() *upstreamEnd { return &upstreamEnd{done: make(chan struct{})} }

func (e *upstreamEnd) mark(canceled bool) {
	e.once.Do(func() {
		e.at, e.canceled = time.Now(), canceled
		close(e.done)
	})
}

// wait returns once the upstream handler ended (false: not within d).
func (e *upstreamEnd) wait(d time.Duration) bool {
	select {
	case <-e.done:
		return true
	case <-time.After(d):
		return false
	}
}

// streamSettings are the production defaults of the stream keys
// (config.GatewayConfig): grace 40 s, keepalive 15 s, write window 30 s,
// frame cap 1 MiB.
func withStreamSettings(r *chatRig) {
	s := *r.settings.Load()
	s.StreamCommitGrace, s.KeepaliveInterval, s.ClientWriteWindow, s.MaxStreamFrameBytes = 40*time.Second, 15*time.Second, 30*time.Second, 1<<20
	r.settings.Store(&s)
}

// withWait sets the wait budget the route reads (Deps.MaxWait) and the one
// the dispatcher enforces (dispatch.external_max_wait) to d.
func withWait(d time.Duration) rigOption {
	return func(r *chatRig) {
		r.deps.MaxWait = func() time.Duration { return d }
		set := dispatch.DefaultSettings()
		set.ExternalMaxWait = d
		r.disp.UpdateSettings(set)
	}
}

// withStream sets the stream keys of the rig's settings.
func withStream(grace, keepalive, window time.Duration) rigOption {
	return func(r *chatRig) {
		s := *r.settings.Load()
		s.StreamCommitGrace, s.KeepaliveInterval, s.ClientWriteWindow = grace, keepalive, window
		r.settings.Store(&s)
	}
}

// acqClock records when each blocking acquire of the planner returned with
// a lease (the byte invariant compares the first byte against it).
type acqClock struct {
	handler.V1Dispatch
	mu       sync.Mutex
	returned []time.Time
	tries    atomic.Int64 // non-blocking acquires that returned a lease
}

func (c *acqClock) TryAcquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	l, run, err := c.V1Dispatch.TryAcquire(ctx, req)
	if err == nil {
		c.tries.Add(1)
	}
	return l, run, err
}

func (c *acqClock) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	l, run, err := c.V1Dispatch.Acquire(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.returned = append(c.returned, time.Now())
		c.mu.Unlock()
	}
	return l, run, err
}

func (c *acqClock) first() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.returned) == 0 {
		return time.Time{}
	}
	return c.returned[0]
}

// withAcqClock puts an acquire clock between the planner and the rig's
// census recorder.
func withAcqClock(c *acqClock) rigOption {
	return func(r *chatRig) {
		c.V1Dispatch = r.recorder
		get := func() oai.Settings { return *r.settings.Load() }
		r.planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: c, Settings: get})
		r.deps.Planner = r.planner
	}
}

// byteClock wraps the route's response writer: the time of the first
// WriteHeader or Write (the first byte decision) and every WriteHeader.
type byteClock struct {
	first   atomic.Int64 // unix ns of the first write, 0 = none
	headers atomic.Int32 // WriteHeader calls with a final status
}

func (b *byteClock) wrap(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.ServeHTTP(&clockWriter{ResponseWriter: w, c: b}, r)
	})
}

func (b *byteClock) at() time.Time {
	if n := b.first.Load(); n != 0 {
		return time.Unix(0, n)
	}
	return time.Time{}
}

type clockWriter struct {
	http.ResponseWriter
	c *byteClock
}

func (w *clockWriter) mark() { w.c.first.CompareAndSwap(0, time.Now().UnixNano()) }

func (w *clockWriter) WriteHeader(code int) {
	w.mark()
	if code >= http.StatusOK {
		w.c.headers.Add(1)
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *clockWriter) Write(p []byte) (int, error) {
	w.mark()
	return w.ResponseWriter.Write(p)
}

func (w *clockWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serve runs h on a real HTTP server.
func serveHTTP(t testing.TB, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

// sseEvent is one event the client read.
type sseEvent struct {
	at      time.Time
	data    string // the data payload ("" for a comment)
	comment string // the comment text without ": "
	other   string // any other line (event:, id:, …) — never sent by the route
}

// streamAnswer is one streamed answer as a client saw it.
type streamAnswer struct {
	start   time.Time
	status  int
	header  http.Header
	raw     string
	events  []sseEvent
	readErr error // the body's read error (nil = a clean end)
	end     time.Time
}

// streamBody is a request with stream true (and the extra keys).
func streamBody(model string, extra ...string) string {
	return chatBody(model, append([]string{`"stream":true`}, extra...)...)
}

// postStream sends body to srv and reads the answer to its end (or until
// ctx ends). hdr are extra request headers.
func postStream(ctx context.Context, t testing.TB, srv *httptest.Server, body string, hdr ...string) streamAnswer {
	t.Helper()
	a := streamAnswer{start: time.Now()}
	resp, err := openStream(ctx, srv, body, hdr...)
	if err != nil {
		a.readErr = err
		a.end = time.Now()
		return a
	}
	defer func() { _ = resp.Body.Close() }()
	a.status, a.header = resp.StatusCode, resp.Header
	var raw strings.Builder
	a.events, a.readErr = readEvents(io.TeeReader(resp.Body, &raw), nil)
	a.raw, a.end = raw.String(), time.Now()
	return a
}

func openStream(ctx context.Context, srv *httptest.Server, body string, hdr ...string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := srv.Client()
	if c == nil { // a server not started by httptest
		c = &http.Client{}
	}
	return c.Do(req)
}

// readEvents parses an SSE body into events; each is also sent on each
// (optional) as it arrives.
func readEvents(r io.Reader, each chan<- sseEvent) ([]sseEvent, error) {
	br := bufio.NewReader(r)
	var out []sseEvent
	var data []string
	for {
		line, err := br.ReadString('\n')
		line = strings.TrimRight(line, "\r\n")
		switch {
		case line == "" && len(data) > 0:
			ev := sseEvent{at: time.Now(), data: strings.Join(data, "\n")}
			data = nil
			out = append(out, ev)
			if each != nil {
				each <- ev
			}
		case strings.HasPrefix(line, "data:"):
			data = append(data, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		case strings.HasPrefix(line, ":"):
			ev := sseEvent{at: time.Now(), comment: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
			out = append(out, ev)
			if each != nil {
				each <- ev
			}
		case line != "":
			ev := sseEvent{at: time.Now(), other: line}
			out = append(out, ev)
			if each != nil {
				each <- ev
			}
		}
		if err != nil {
			if err == io.EOF {
				return out, nil
			}
			return out, err
		}
	}
}

// chunks are the data payloads of the answer, [DONE] excluded.
func (a streamAnswer) chunks() []string {
	var out []string
	for _, e := range a.events {
		if e.data != "" && e.data != "[DONE]" {
			out = append(out, e.data)
		}
	}
	return out
}

// dones counts the [DONE] frames.
func (a streamAnswer) dones() int {
	n := 0
	for _, e := range a.events {
		if e.data == "[DONE]" {
			n++
		}
	}
	return n
}

// errorFrame returns the code of the answer's error frame ("" = none) and
// whether the frame is the last data event.
func (a streamAnswer) errorFrame(t testing.TB) (code string, last bool) {
	t.Helper()
	var datas []string
	for _, e := range a.events {
		if e.data != "" {
			datas = append(datas, e.data)
		}
	}
	for i, d := range datas {
		var env struct {
			Error *struct {
				Message string  `json:"message"`
				Type    string  `json:"type"`
				Param   *string `json:"param"`
				Code    *string `json:"code"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(d), &env) != nil || env.Error == nil {
			continue
		}
		var keys map[string]map[string]json.RawMessage
		_ = json.Unmarshal([]byte(d), &keys)
		if len(keys["error"]) != 4 {
			t.Errorf("error frame %s: %d keys in error, want message/type/param/code", d, len(keys["error"]))
		}
		if env.Error.Code != nil {
			code = *env.Error.Code
		}
		return code, i == len(datas)-1
	}
	return "", false
}

// comments counts the comment events (keepalives) before the first data
// event.
func (a streamAnswer) commentsBeforeData() int {
	n := 0
	for _, e := range a.events {
		if e.data != "" {
			break
		}
		if e.comment != "" {
			n++
		}
	}
	return n
}

// streamUsageLimiter is a fake limiter that records the usage of every Settle.
type streamUsageLimiter struct {
	fakeLimiter
	mu      sync.Mutex
	settled []dispatch.Usage
}

type streamUsageReservation struct {
	l    *streamUsageLimiter
	held int64
}

func (r streamUsageReservation) Settle(u dispatch.Usage) {
	r.l.settles.Add(1)
	r.l.mu.Lock()
	r.l.settled = append(r.l.settled, u)
	r.l.mu.Unlock()
}
func (r streamUsageReservation) Keep()  { r.l.keeps.Add(1) }
func (r streamUsageReservation) Abort() { r.l.aborts.Add(1) }
func (r streamUsageReservation) Reject(reason string, _ time.Duration) {
	r.l.rejects.Add(1)
	r.l.lastReason.Store(reason)
}
func (r streamUsageReservation) Headers() http.Header { return nil }

// Held is what the rig meters charge for the request (fakeChatCharge).
func (r streamUsageReservation) Held() int64 { return r.held }

func (l *streamUsageLimiter) Admit(_ context.Context, _ *auth.ConsumerAuth, _ modelalias.Alias, e oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	l.admits.Add(1)
	return streamUsageReservation{l: l, held: fakeChatCharge(e)}, nil
}

func (l *streamUsageLimiter) usages() []dispatch.Usage {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]dispatch.Usage(nil), l.settled...)
}

func withUsageLimiter(l *streamUsageLimiter) rigOption {
	return func(r *chatRig) { r.deps.Limiter = l }
}

// waitHeld waits until host's held lease count is n (false: not within d).
func waitHeld(d *dispatch.Dispatcher, host string, n int, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if held, _ := heldOn(d, host); held == n {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

// waitFor polls cond every millisecond up to d.
func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
	return true
}
