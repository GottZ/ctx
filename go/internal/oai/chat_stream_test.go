package oai_test

// The stream route's gates (W02-08; design/02 §4.6, §4.7, §4.10.3,
// §4.11.3, §4.12). Each test names its gate and its red overlay; the
// overlays run against a copy of the tree (report reports/bau/W02-08.md).

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/promptguard"
)

// chunkKeys is the stream key allowlist (design/02 §4.6).
var chunkKeys = map[string]bool{"id": true, "object": true, "created": true, "model": true, "choices": true, "usage": true, "service_tier": true}

// streamCanaries are the engine-side values no client byte may carry.
var streamCanaries = []string{"ID-CANARY", "MODEL-CANARY", "HOST-CANARY", "PROVIDER-CANARY", "THINK-CANARY", "INSTANCE-CANARY", "LOOP-CANARY", "HEADER-CANARY", "OPENROUTER PROCESSING"}

// checkRelay checks the relay contract on a served stream: SSE headers,
// no upstream header, every chunk in the allowlist with one id/created/model,
// [DONE] exactly once and last, no event: line, no canary. It returns the
// decoded chunks.
func checkRelay(t *testing.T, a streamAnswer) []map[string]json.RawMessage {
	t.Helper()
	if a.status != http.StatusOK || a.readErr != nil {
		t.Fatalf("answer %d, read error %v: %s", a.status, a.readErr, a.raw)
	}
	for k, want := range map[string]string{"Content-Type": "text/event-stream", "Cache-Control": "no-cache", "X-Accel-Buffering": "no"} {
		if got := a.header.Get(k); got != want {
			t.Errorf("header %s = %q, want %q", k, got, want)
		}
	}
	for k, vs := range a.header {
		for _, v := range vs {
			for _, c := range streamCanaries {
				if strings.Contains(v, c) {
					t.Errorf("response header %s: %q carries the upstream's %s (no upstream header is relayed)", k, v, c)
				}
			}
		}
	}
	for _, c := range streamCanaries {
		if strings.Contains(a.raw, c) {
			t.Errorf("the stream carries %s: %s", c, a.raw)
		}
	}
	var out []map[string]json.RawMessage
	var id, created string
	for i, e := range a.events {
		if e.other != "" {
			t.Errorf("event %d: line %q — the relay writes data: and comments only", i, e.other)
		}
	}
	if n := a.dones(); n != 1 {
		t.Errorf("[DONE] %d times, want exactly once", n)
	}
	if last := a.events[len(a.events)-1]; last.data != "[DONE]" {
		t.Errorf("last event %+v, want [DONE]", last)
	}
	for _, c := range a.chunks() {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(c), &m); err != nil {
			t.Fatalf("chunk %q: %v", c, err)
		}
		for k := range m {
			if !chunkKeys[k] {
				t.Errorf("chunk key %q outside the allowlist: %s", k, c)
			}
		}
		if id == "" {
			id, created = string(m["id"]), string(m["created"])
		}
		if string(m["id"]) != id || string(m["created"]) != created || string(m["model"]) != `"chat"` || string(m["object"]) != `"chat.completion.chunk"` {
			t.Errorf("chunk %s: id/created/model/object differ from the first chunk (%s %s) or the alias", c, id, created)
		}
		out = append(out, m)
	}
	var sid string
	_ = json.Unmarshal([]byte(id), &sid)
	suffix, ok := strings.CutPrefix(sid, "chatcmpl-")
	if u, err := uuid.Parse(suffix); !ok || err != nil || u.Version() != 7 {
		t.Errorf("id %q, want chatcmpl-<UUIDv7>", sid)
	}
	return out
}

// streamContent joins the content deltas of the chunks.
func streamContent(chunks []map[string]json.RawMessage) string {
	var b strings.Builder
	for _, m := range chunks {
		var ch []struct {
			Delta struct {
				Content *string `json:"content"`
			} `json:"delta"`
		}
		_ = json.Unmarshal(m["choices"], &ch)
		for _, c := range ch {
			if c.Delta.Content != nil {
				b.WriteString(*c.Delta.Content)
			}
		}
	}
	return b.String()
}

// TestStreamRelayShapes is the relay contract (briefing (a), (b), (d), (h);
// G2 and the header half of G12) over the three engine shapes, with and
// without include_usage: SSE headers, nothing of the upstream (no header,
// no canary), the allowlist, one id/created/model, [DONE] once; the usage
// chunk (choices: []) right before [DONE] only for include_usage, no usage
// key at all without it; upstream always asked for usage; one llmlog row
// with stream true, a TTFT and the backend's tokens in both cases.
// Red (G2): the passthrough without the forced include_usage — the row's
// tokens are 0 without the client's include_usage. Red (G12 headers): a
// commit that copies an upstream header.
func TestStreamRelayShapes(t *testing.T) {
	for _, sh := range streamShapes {
		for _, include := range []bool{true, false} {
			t.Run(sh.name+map[bool]string{true: "/include_usage", false: "/no_usage"}[include], func(t *testing.T) {
				u := newUpstream(t, "")
				u.setRespond(sseRespond(sh.steps(), nil, nil))
				rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
				srv := serveHTTP(t, rig.router)
				extra := []string{}
				if include {
					extra = append(extra, `"stream_options":{"include_usage":true}`)
				}
				a := postStream(context.Background(), t, srv, streamBody("chat", extra...))
				chunks := checkRelay(t, a)
				if got := streamContent(chunks); got != streamText {
					t.Errorf("content %q, want %q", got, streamText)
				}
				usageChunks := 0
				for i, m := range chunks {
					u, has := m["usage"]
					switch {
					case !include && has:
						t.Errorf("chunk %d carries usage without include_usage: %s", i, u)
					case include && has && string(u) != "null":
						usageChunks++
						if string(m["choices"]) != "[]" || i != len(chunks)-1 {
							t.Errorf("usage chunk %d of %d: choices %s, want [] and the last chunk", i, len(chunks), m["choices"])
						}
						var us struct {
							PromptTokens     int `json:"prompt_tokens"`
							CompletionTokens int `json:"completion_tokens"`
						}
						if err := json.Unmarshal(u, &us); err != nil || us.PromptTokens != 11 || us.CompletionTokens != 3 || strings.Contains(string(u), "cost") {
							t.Errorf("usage %s, want 11/3 without cost", u)
						}
					}
				}
				if include && usageChunks != 1 {
					t.Errorf("%d usage chunks, want 1", usageChunks)
				}
				up := u.lastBody(t)
				if up["stream"] != true || !strings.Contains(string(u.raws[0]), `"stream_options":{"include_usage":true}`) {
					t.Errorf("upstream body stream=%v, raw %s: want stream true with include_usage", up["stream"], u.raws[0])
				}
				rows := rig.rows.all()
				if len(rows) != 1 {
					t.Fatalf("%d llmlog rows, want 1", len(rows))
				}
				r := rows[0]
				if !r.Gateway.Stream || r.Gateway.TTFTMs == nil || r.Gateway.Usage != llmlog.UsageBackend || r.Gateway.Finish != llmlog.FinishStop ||
					r.Telemetry.PromptTokens != 11 || r.Telemetry.CompletionTokens != 3 || r.Err != nil {
					t.Errorf("row gw %+v tokens %d/%d err %v, want stream, ttft, backend usage 11/3, finish stop",
						r.Gateway, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens, r.Err)
				}
				if rig.limiter.settles.Load() != 1 || rig.limiter.aborts.Load()+rig.limiter.rejects.Load() != 0 {
					t.Errorf("reservation settle/abort/reject %d/%d/%d, want 1/0/0", rig.limiter.settles.Load(), rig.limiter.aborts.Load(), rig.limiter.rejects.Load())
				}
				rec := <-rig.recs
				if !rec.Stream || rec.Reason != "" || rec.PromptTokens != 11 || rec.Usage != llmlog.UsageBackend {
					t.Errorf("access record %+v, want stream, no reason, 11 prompt tokens, backend usage", rec)
				}
			})
		}
	}
}

// fastPhaseB sets a short grace clamp for one test: distance d, the wait
// budget w (route and dispatcher), the configured grace g — grace_eff =
// clamp(g, w + d, 50 s).
func fastPhaseB(t *testing.T, d time.Duration) {
	t.Helper()
	t.Cleanup(oai.SetCommitDistance(d))
}

// TestStreamUsageEstimated is G2's last clause and design/06 §4.2 B-06-8:
// an upstream stream without a usage chunk books usage=estimated — prompt
// = TokenEstimate(request runes) (or llama.cpp's timings.prompt_n),
// completion = TokenEstimate(generated runes) — in the token COLUMNS of the
// row, in Settle and on the access record; a stream the client aborted
// books at least what it streamed. Red: no estimate (the row's tokens 0,
// no usage source).
func TestStreamUsageEstimated(t *testing.T) {
	gen := promptguard.TokenEstimate(len([]rune(streamText)))
	for _, tc := range []struct {
		name       string
		steps      []sseStep
		wantPrompt func(body []byte) int
	}{
		{"no usage, no timings", []sseStep{{data: sglangChunk("hello", "")}, {data: sglangChunk(" from", "")}, {data: sglangChunk(" the engine", "stop")}, {data: "[DONE]"}},
			func(body []byte) int { return promptguard.TokenEstimate(len([]rune(messagesOf(body)))) }},
		{"timings only", []sseStep{{data: llamaChunk("hello", "")}, {data: llamaChunk(" from", "")},
			{data: strings.Replace(llamaChunk(" the engine", "stop"), `"object"`, `"timings":{"prompt_n":11,"predicted_n":3},"object"`, 1)}, {data: "[DONE]"}},
			func([]byte) int { return 11 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			u.setRespond(sseRespond(tc.steps, nil, nil))
			lim := &streamUsageLimiter{}
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withUsageLimiter(lim))
			body := streamBody("chat")
			a := postStream(context.Background(), t, serveHTTP(t, rig.router), body)
			checkRelay(t, a)
			wantP := tc.wantPrompt([]byte(body))
			rows := rig.rows.all()
			if len(rows) != 1 {
				t.Fatalf("%d rows, want 1", len(rows))
			}
			r := rows[0]
			if r.Gateway.Usage != llmlog.UsageEstimated || r.Telemetry.PromptTokens != wantP || r.Telemetry.CompletionTokens != gen {
				t.Errorf("row usage %q tokens %d/%d, want estimated %d/%d in the columns", r.Gateway.Usage, r.Telemetry.PromptTokens, r.Telemetry.CompletionTokens, wantP, gen)
			}
			if us := lim.usages(); len(us) != 1 || us[0].PromptTokens != wantP || us[0].CompletionTokens != gen {
				t.Errorf("Settle %+v, want one Settle of %d/%d", us, wantP, gen)
			}
			if rec := <-rig.recs; rec.Usage != llmlog.UsageEstimated || rec.CompletionTokens != gen {
				t.Errorf("access record usage %q completion %d, want estimated %d", rec.Usage, rec.CompletionTokens, gen)
			}
		})
	}
	t.Run("a refusal before the stream books nothing", func(t *testing.T) {
		// Fremd-Review 1 F-2: an engine that refuses the request with a
		// status (here 400) processed no prompt — no estimate, as
		// non-stream. Red: the estimate for every wired attempt. The
		// reservation: the backend had the request (wire contact) and no
		// usage came — it keeps its charge, as non-stream (W06-10 H-7;
		// the W02-08 rebase round changed this pin from one Settle of 0,
		// a full refund, to Keep).
		u := newUpstream(t, "")
		u.setRespond(chRespond(http.StatusBadRequest, `{"error":{"message":"bad"}}`))
		lim := &streamUsageLimiter{}
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withUsageLimiter(lim))
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		if a.status != http.StatusBadRequest {
			t.Fatalf("answer %d %s, want the 400 before the commit", a.status, a.raw)
		}
		rows := rig.rows.all()
		if len(rows) != 1 || rows[0].Gateway.Usage != "" || rows[0].Telemetry.PromptTokens != 0 || rows[0].Telemetry.CompletionTokens != 0 {
			t.Errorf("rows %+v, want one row without usage and 0 tokens", rows)
		}
		if us := lim.usages(); len(us) != 0 || lim.keeps.Load() != 1 || lim.aborts.Load()+lim.rejects.Load() != 0 {
			t.Errorf("Settle %+v, keep/abort/reject %d/%d/%d, want no Settle and one Keep", us, lim.keeps.Load(), lim.aborts.Load(), lim.rejects.Load())
		}
	})
	t.Run("client abort books at least what was streamed", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		end := newUpstreamEnd()
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {data: sglangChunk(" from", "")}, {hold: hold}}, nil, end))
		lim := &streamUsageLimiter{}
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings, withUsageLimiter(lim))
		srv := serveHTTP(t, rig.router)
		ctx, cancel := context.WithCancel(context.Background())
		resp, err := openStream(ctx, srv, streamBody("chat"))
		if err != nil {
			t.Fatal(err)
		}
		each := make(chan sseEvent, 16)
		go func() {
			_, _ = readEvents(resp.Body, each)
			close(each)
		}()
		for got := 0; got < 2; got++ {
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatalf("frame %d of 2 did not arrive (status %d)", got+1, resp.StatusCode)
			}
		}
		cancel()
		_ = resp.Body.Close()
		if !end.wait(2 * time.Second) {
			t.Fatal("the upstream did not see the abort")
		}
		if !waitFor(2*time.Second, func() bool { return len(rig.rows.all()) == 1 }) {
			t.Fatal("no row after the abort")
		}
		r := rig.rows.all()[0]
		streamed := 2 // two tokens left the engine
		if r.Gateway.Finish != llmlog.FinishAborted || r.Gateway.Usage != llmlog.UsageEstimated || r.Telemetry.CompletionTokens < streamed {
			t.Errorf("aborted row finish %q usage %q completion %d, want aborted, estimated, >= %d", r.Gateway.Finish, r.Gateway.Usage, r.Telemetry.CompletionTokens, streamed)
		}
		if lim.settles.Load() != 1 || lim.aborts.Load() != 0 {
			t.Errorf("settle/abort %d/%d, want 1/0 (the abort reached the wire)", lim.settles.Load(), lim.aborts.Load())
		}
	})
}

// messagesOf is the raw messages value of a request body.
func messagesOf(body []byte) string {
	var m map[string]json.RawMessage
	_ = json.Unmarshal(body, &m)
	return string(m["messages"])
}

// TestStreamReasoningOffStripsEveryChunk is lead decision O-08-3 (b): a
// reasoning-off alias relays no reasoning trace in any chunk — the
// reasoning-only chunks are dropped, not relayed emptied — while the
// content arrives whole. Red: chunks verbatim (THINK-CANARY and
// reasoning_content reach the client).
func TestStreamReasoningOffStripsEveryChunk(t *testing.T) {
	u := newUpstream(t, "")
	steps := []sseStep{{data: sglangThink}, {data: sglangThink}, {data: sglangThink}}
	for i, d := range streamDeltas {
		fin := ""
		if i == len(streamDeltas)-1 {
			fin = "stop"
		}
		steps = append(steps, sseStep{data: sglangChunk(d, fin)})
	}
	u.setRespond(sseRespond(append(steps, sseStep{data: "[DONE]"}), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	chunks := checkRelay(t, a)
	if strings.Contains(a.raw, "reasoning") || strings.Contains(a.raw, "THINK-CANARY") {
		t.Errorf("a reasoning trace reached the client of a reasoning-off alias: %s", a.raw)
	}
	if got := streamContent(chunks); got != streamText {
		t.Errorf("content %q, want %q", got, streamText)
	}
	if len(chunks) != len(streamDeltas) {
		t.Errorf("%d chunks relayed, want %d (the reasoning-only chunks dropped, not relayed emptied)", len(chunks), len(streamDeltas))
	}
}

// TestStreamRejectionIsHTTP429 is G3 (B24) and the byte invariant against
// the real relay (K7; the template is handler's
// TestByteInvariantNoByteBeforeAdmission, A03-W8 deviation 2): the external
// partition full for the whole wait budget ⇒ a real HTTP 429 server_busy
// with Retry-After and retry-after-ms, and the status is the first byte;
// admitted late ⇒ the first byte comes after the acquire returned. Red: a
// commit timer armed at entry with the raw grace (200 + an error frame
// after the wait budget ran out).
func TestStreamRejectionIsHTTP429(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	for _, tc := range []struct {
		name    string
		release time.Duration // the occupying lease's release (0 = never during the request)
	}{{"refused after the wait budget", 0}, {"admitted late", 150 * time.Millisecond}} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			u.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
			clock := &acqClock{}
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings,
				withWait(300*time.Millisecond), withStream(50*time.Millisecond, time.Second, 5*time.Second), withAcqClock(clock))
			var bc byteClock
			srv := serveHTTP(t, bc.wrap(rig.router))
			release := occupyExternal(t, rig.disp, u.srv.URL)
			if tc.release > 0 {
				time.AfterFunc(tc.release, release)
			} else {
				defer release()
			}
			a := postStream(context.Background(), t, srv, streamBody("chat"))
			first := bc.at()
			if tc.release == 0 {
				if a.status != http.StatusTooManyRequests || a.header.Get("Retry-After") == "" || a.header.Get("retry-after-ms") == "" ||
					a.header.Get("x-should-retry") != "true" || strings.Contains(a.raw, "data:") {
					t.Fatalf("answer %d %v %s, want 429 with Retry-After, retry-after-ms, x-should-retry true and no SSE byte", a.status, a.header, a.raw)
				}
				var env struct{ Error struct{ Code string } }
				if err := json.Unmarshal([]byte(a.raw), &env); err != nil || env.Error.Code != oai.CodeServerBusy {
					t.Errorf("body %s, want the server_busy envelope", a.raw)
				}
				if d := first.Sub(a.start); d < 250*time.Millisecond {
					t.Errorf("first byte %v after the request, before the wait budget (300 ms) ran out", d)
				}
				if u.hits.Load() != 0 {
					t.Errorf("upstream hits %d, want 0", u.hits.Load())
				}
				return
			}
			checkRelay(t, a)
			acq := clock.first()
			if acq.IsZero() || first.Before(acq) {
				t.Fatalf("first byte at %v, acquire returned at %v: a byte before the admission (K7)", first.Sub(a.start), acq.Sub(a.start))
			}
		})
	}
}

// TestStreamCommitGraceClampLive is G4, scaled to milliseconds (the clamp's
// distance shortened to 100 ms; the pure clamp at the briefing's 40-s
// numbers is TestStreamCommitClampPins): wait budget 400 ms, configured
// grace 200 ms ⇒ grace_eff 500 ms; the target is busy until 300 ms, the
// first data frame comes at about 1 s. No byte before the admission, the
// commit (status + a keepalive) at about entry + 500 ms, one WARN "grace
// clamped" for two such requests (throttled, O-08-4). Red: the raw grace
// from entry (the status at 200 ms, before the admission at 300 ms).
func TestStreamCommitGraceClampLive(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	oai.ResetGraceWarner()
	logs := chCaptureLog(t)
	for i := 0; i < 2; i++ {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(append([]sseStep{{wait: 700 * time.Millisecond}}, streamShapes[0].steps()...), nil, nil))
		clock := &acqClock{}
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 2, 1, withStreamSettings,
			withWait(400*time.Millisecond), withStream(200*time.Millisecond, 10*time.Second, 5*time.Second), withAcqClock(clock))
		var bc byteClock
		srv := serveHTTP(t, bc.wrap(rig.router))
		release := occupyExternal(t, rig.disp, u.srv.URL)
		time.AfterFunc(300*time.Millisecond, release)
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		checkRelay(t, a)
		first, acq := bc.at(), clock.first()
		if acq.IsZero() || first.Before(acq) {
			t.Fatalf("request %d: first byte at %v, acquire returned at %v — a byte before the admission", i, first.Sub(a.start), acq.Sub(a.start))
		}
		if d := first.Sub(a.start); d < 450*time.Millisecond || d > 700*time.Millisecond {
			t.Errorf("request %d: commit at %v after entry, want about grace_eff 500 ms", i, d)
		}
		if n := a.commentsBeforeData(); n < 1 {
			t.Errorf("request %d: %d keepalives before the first frame, want the commit's keepalive", i, n)
		}
	}
	if n := strings.Count(logs.String(), "stream commit grace clamped"); n != 1 {
		t.Errorf("WARN grace clamped %d times for two requests, want 1 (throttled): %s", n, logs.String())
	}
}

// TestStreamKeepaliveDuringPrefill is G5: an engine silent for three times
// the grace after the admission ⇒ the commit at grace_eff, then only
// ": keepalive" comments (never an event: line or a data frame without
// choices) every keepalive_interval, then the chunks. Red: an event: queued
// keepalive (what the house writer would send) — a chunk without choices.
func TestStreamKeepaliveDuringPrefill(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	u := newUpstream(t, "")
	u.setRespond(sseRespond(append([]sseStep{{wait: 600 * time.Millisecond}}, streamShapes[0].steps()...), nil, nil))
	rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		withWait(100*time.Millisecond), withStream(0, 100*time.Millisecond, 5*time.Second))
	a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
	checkRelay(t, a)
	if n := a.commentsBeforeData(); n < 3 {
		t.Errorf("%d keepalives during 600 ms of silence (grace 200 ms, interval 100 ms), want >= 3: %s", n, a.raw)
	}
	for _, e := range a.events {
		if e.comment != "" && e.comment != "keepalive" {
			t.Errorf("comment %q, want keepalive", e.comment)
		}
	}
	for _, c := range a.chunks() {
		var m map[string]json.RawMessage
		if json.Unmarshal([]byte(c), &m) != nil || len(m["choices"]) == 0 {
			t.Errorf("data frame %q without choices (client code reads choices[0])", c)
		}
	}
}

// status502 answers 502 after wait (the request's end cuts it short); at
// is when it answered.
func status502(wait time.Duration, at *atomic.Int64) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-time.After(wait):
		case <-r.Context().Done():
			return
		}
		at.Store(time.Now().UnixNano())
		w.WriteHeader(http.StatusBadGateway)
	}
}

// TestStreamFailoverWindow is G6: failover only before the first data
// frame, and the failover acquire never waits. (a) the first row fails
// after the grace commit, the second is full ⇒ an error frame at once
// (< 50 ms), no [DONE], the second row never dialed — the code is the one
// the admission writer binds to ErrWouldBlock, server_busy (K6), where the
// briefing names rate_limit_exceeded (report §Abweichungen); (b) the first
// row fails before the commit, the second is free ⇒ one 200 stream from
// the second row; (c) the first row breaks after a data frame ⇒ an error
// frame, no failover.
func TestStreamFailoverWindow(t *testing.T) {
	fastPhaseB(t, 100*time.Millisecond)
	opts := []rigOption{withStreamSettings, withWait(100 * time.Millisecond), withStream(0, 10*time.Second, 5*time.Second)}
	t.Run("after the commit, next row full", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		var failedAt atomic.Int64
		u1.setRespond(status502(400*time.Millisecond, &failedAt))
		u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1, opts...)
		var bc byteClock
		srv := serveHTTP(t, bc.wrap(rig.router))
		defer occupyExternal(t, rig.disp, u2.srv.URL)()
		a := postStream(context.Background(), t, srv, streamBody("chat"))
		code, last := a.errorFrame(t)
		if a.status != http.StatusOK || code != oai.CodeServerBusy || !last || a.dones() != 0 {
			t.Fatalf("answer %d, error frame %q (last %v), [DONE] %d: %s — want 200, the server_busy frame last, no [DONE]", a.status, code, last, a.dones(), a.raw)
		}
		var frameAt time.Time
		for _, e := range a.events {
			if strings.Contains(e.data, `"error"`) {
				frameAt = e.at
			}
		}
		if d := frameAt.Sub(time.Unix(0, failedAt.Load())); d > 50*time.Millisecond {
			t.Errorf("error frame %v after the first row's 502, want < 50 ms (Next never waits)", d)
		}
		if u2.hits.Load() != 0 || bc.headers.Load() != 1 {
			t.Errorf("second row hits %d, WriteHeader calls %d — want 0 and 1", u2.hits.Load(), bc.headers.Load())
		}
	})
	t.Run("before the commit, next row free", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		var failedAt atomic.Int64
		u1.setRespond(status502(0, &failedAt))
		u2.setRespond(sseRespond(streamShapes[1].steps(), nil, nil))
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1, opts...)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		chunks := checkRelay(t, a)
		if streamContent(chunks) != streamText || u1.hits.Load() != 1 || u2.hits.Load() != 1 {
			t.Errorf("content %q, hits %d/%d — want the second row's stream after one failed attempt", streamContent(chunks), u1.hits.Load(), u2.hits.Load())
		}
		if rows := rig.rows.all(); len(rows) != 1 || len(rows[0].Attempts) != 2 {
			t.Errorf("rows %+v, want one row with two attempts", rows)
		}
	})
	t.Run("after a data frame, no failover", func(t *testing.T) {
		u1, u2 := newUpstream(t, ""), newUpstream(t, "")
		u1.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {wait: 50 * time.Millisecond, drop: true}}, nil, nil))
		u2.setRespond(sseRespond(streamShapes[0].steps(), nil, nil))
		rig := newChatRig(t, []backends.Backend{u1.row("one", backends.TrustFull, 20), u2.row("two", backends.TrustFull, 10)}, 2, 1, opts...)
		a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
		code, last := a.errorFrame(t)
		if a.status != http.StatusOK || code == "" || !last || a.dones() != 0 || u2.hits.Load() != 0 {
			t.Errorf("answer %d, frame %q (last %v), [DONE] %d, second row hits %d: %s — want an error frame, no [DONE], no failover",
				a.status, code, last, a.dones(), u2.hits.Load(), a.raw)
		}
	})
}

// TestStreamErrorFrame is G7: a mid-stream upstream error (an error event,
// a finish_reason "error") ⇒ the chunks so far, then the route's error
// frame (all four envelope keys, a code of the vocabulary — nothing of the
// upstream's own error) as the last data event, no [DONE], the response
// ends. Red: a writer that sends [DONE] after the error frame.
func TestStreamErrorFrame(t *testing.T) {
	for _, tc := range []struct{ name, tail string }{
		{"error event", `{"error":{"message":"boom UPSTREAM-CANARY","type":"server_error","code":"internal"}}`},
		{"finish_reason error", sglangChunk("", "error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := newUpstream(t, "")
			u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {data: sglangChunk(" from", "")}, {data: tc.tail}, {data: "[DONE]"}}, nil, nil))
			rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings)
			a := postStream(context.Background(), t, serveHTTP(t, rig.router), streamBody("chat"))
			code, last := a.errorFrame(t)
			if a.status != http.StatusOK || a.readErr != nil || !last || a.dones() != 0 || len(a.chunks()) != 3 {
				t.Fatalf("answer %d read %v, frame %q last %v, [DONE] %d, %d data frames: %s — want two chunks, the error frame last, no [DONE]",
					a.status, a.readErr, code, last, a.dones(), len(a.chunks()), a.raw)
			}
			if !strings.HasPrefix(code, "upstream_") || strings.Contains(a.raw, "UPSTREAM-CANARY") || strings.Contains(a.raw, `"internal"`) {
				t.Errorf("frame code %q in %s, want an upstream_* code of the vocabulary and nothing of the upstream's error", code, a.raw)
			}
			rows := rig.rows.all()
			if len(rows) != 1 || rows[0].Gateway.Finish != llmlog.FinishError || rows[0].Err == nil || rows[0].Err.Code != code {
				t.Errorf("rows %+v, want one row finish=error with the frame's code", rows)
			}
		})
	}
}
