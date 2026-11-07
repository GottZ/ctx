package chat

// MW6 — the unified I-D1 invariant gate (design/01 §1.2 + §7 W5). The per-path
// coupling probes already live in each wire package (chain_admission_test,
// stream_admission_test, embedcache_admission_test,
// rerank_crossencoder_admission_test); this file CONSOLIDATES the coupling into
// ONE instrumented run across ALL SIX physical inference wire sites and pins the
// grep positive list.
//
// The two W5 deliverables:
//
//  1. Acquire==Wire count gate: one shared counting admitter + one shared wire
//     counter drive every one of the six sites (client.go non-stream ×2,
//     stream.go, embed.go ×2, rerank.go) through their REAL seams — including a
//     failover walk (two physical wire contacts, two acquires) and a
//     lease-free-without-wire path (the rerank early-out). The invariant
//     asserted globally: Σ acquires == Σ wire contacts. A wire call that skips
//     the admitter (or an acquire that never reaches the wire) breaks the sum.
//
//  2. Gepinnte grep-Positiv-Liste: the exact per-file count of
//     http.NewRequestWithContext across internal/{llm,embed,rerank}. A new
//     un-listed wire site — or a changed count in a listed file — turns the gate
//     red (robust against line drift: counts per FILE, no line numbers).
//
// Home: package chat is the only place that reaches the UNEXPORTED stream seam
// (Engine.runStream) while importing the other five wire-driving packages
// (llm, embedcache, rrf) acyclically — none of them import chat. The embed
// cache-hit clause (a hit acquires no lease) has a package-local test seam
// (embedcache.SetCacheProbeForTest, export_test.go) and stays pinned in
// embedcache_admission_test; the lease-free-without-wire property is
// demonstrated inside THIS run via the reachable rerank early-out.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/embedcache"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/rrf"
)

// countingAdmitter delegates to a real pass-through dispatcher and counts every
// physical Acquire pass. It is a dispatch.Admitter, so BOTH llm.Admission and
// embedcache.Admission wrap the SAME counter — the single acquire chokepoint
// under all six wire sites.
type countingAdmitter struct {
	d        *dispatch.Dispatcher
	acquires int64
}

func newCountingAdmitter(t *testing.T) *countingAdmitter {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return &countingAdmitter{d: d}
}

func (c *countingAdmitter) Acquire(ctx context.Context, req dispatch.Request) (*dispatch.Lease, context.Context, error) {
	atomic.AddInt64(&c.acquires, 1)
	return c.d.Acquire(ctx, req)
}

func (c *countingAdmitter) count() int64 { return atomic.LoadInt64(&c.acquires) }

// walkAcquirer is the test llm.Acquirer of the gateway walker (W02-06): First
// is the blocking Acquire, Next the non-blocking TryAcquire, both counted on
// the SAME counter as every other site. No production Acquirer exists yet
// (A03-W3/W5/W17, W02-07 build it).
type walkAcquirer struct {
	ca    *countingAdmitter
	class dispatch.Class
}

func (w walkAcquirer) Class() dispatch.Class { return w.class }

func (w walkAcquirer) req(b *backends.Backend, role string, timeout time.Duration) dispatch.Request {
	return dispatch.Request{Target: dispatch.Target{Origin: b.Host}, Class: w.class, Role: role, DeadlineIn: timeout}
}

func (w walkAcquirer) First(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	return w.ca.Acquire(ctx, w.req(b, role, timeout))
}

func (w walkAcquirer) Next(ctx context.Context, b *backends.Backend, role string, timeout time.Duration) (*dispatch.Lease, context.Context, error) {
	atomic.AddInt64(&w.ca.acquires, 1)
	return w.ca.d.TryAcquire(ctx, w.req(b, role, timeout))
}

func (w walkAcquirer) Continue(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, llm.ErrNoContinuation
}

// passthroughAttempt is the gateway attempt shape W02-07/W02-08 will wire:
// BuildUpstreamBody per row, then the raw wire under the walker's lease.
func passthroughAttempt(stream bool) llm.AttemptFunc {
	return func(runCtx context.Context, b *backends.Backend, _ *dispatch.Lease, timeout time.Duration) (dispatch.Usage, bool, error) {
		body, err := llm.BuildUpstreamBody(llm.UpstreamSpec{
			Messages: json.RawMessage(`[{"role":"user","content":"hi"}]`), Role: "chat", Stream: stream, MaxTokens: 16,
		}, b)
		if err != nil {
			return dispatch.Usage{}, false, err
		}
		if stream {
			res, err := llm.ChatRawStream(runCtx, *b, body, timeout, 1<<20, func([]byte) error { return nil })
			if res == nil {
				return dispatch.Usage{}, false, err
			}
			return dispatch.Usage{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens}, res.Frames > 0, err
		}
		res, err := llm.ChatRaw(runCtx, *b, body, timeout, httpx.MaxResponseBytes)
		if err != nil {
			return dispatch.Usage{}, false, err
		}
		return dispatch.Usage{PromptTokens: res.PromptTokens, CompletionTokens: res.CompletionTokens}, false, nil
	}
}

// walkRules admits the test class (ClassBackground, see the count gate).
var walkRules = llm.WalkRules{WindowRule: true, AllowedClasses: []dispatch.Class{dispatch.ClassBackground}}

// wireVec1024 is a quality-gate-passing raw vector (alternating 1,-1 has norm 1
// and variance 1/1024 after L2 normalization — mirrors embed/wire_test.go).
func wireVec1024() string {
	var sb strings.Builder
	for i := 0; i < embed.TargetDims; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		if i%2 == 0 {
			sb.WriteString("1")
		} else {
			sb.WriteString("-1")
		}
	}
	return sb.String()
}

// invariantSSE is the minimal OpenAI stream tail: one delta, one usage frame,
// [DONE] — enough for ChatStream to drain and release cleanly.
const invariantSSE = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n" +
	"data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
	"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
	"data: [DONE]\n\n"

// newWireServer answers every one of the six inference wire shapes and bumps
// wireHits on EVERY inbound request (an un-admitted contact is still counted —
// that is precisely how the invariant catches it). An unexpected path fails the
// test loudly.
func newWireServer(t *testing.T, wireHits *int64) *httptest.Server {
	t.Helper()
	vec := wireVec1024()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(wireHits, 1)
		body, _ := io.ReadAll(r.Body)
		serveWireShape(t, w, r, body, vec)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// serveWireShape answers one request in the shape of its inference wire
// (shared by the MW6 count server and the F-W4 capture server,
// wire_hop_capture_test.go). An unexpected path fails the test loudly.
func serveWireShape(t *testing.T, w http.ResponseWriter, r *http.Request, body []byte, vec string) {
	switch r.URL.Path {
	case "/api/chat": // client.go:190 (ollama non-stream)
		fmt.Fprint(w, `{"message":{"role":"assistant","content":"ok"},"eval_count":5,"prompt_eval_count":10}`)
	case "/v1/chat/completions": // client.go:263 (openai non-stream) OR stream.go:288
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, invariantSSE)
			return
		}
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`)
	case "/api/embed": // embed.go embedOllama
		fmt.Fprintf(w, `{"embeddings":[%s],"prompt_eval_count":7}`, wireVecs(body, vec, false))
	case "/v1/embeddings": // embed.go embedOpenAI (the internal one-string body, or a gateway sub-block's list — W02-09)
		fmt.Fprintf(w, `{"data":[%s]}`, wireVecs(body, vec, true))
	case "/v1/messages": // anthropic.go (non-stream) OR anthropic_stream.go
		if bytes.Contains(body, []byte(`"stream":true`)) {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"usage\":{\"input_tokens\":10}}}\n\n"+
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"ok\"}}\n\n"+
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n"+
				"data: {\"type\":\"message_stop\"}\n\n")
			return
		}
		fmt.Fprint(w, `{"id":"msg_1","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":5}}`)
	case "/v1/rerank": // rerank.go:96
		var req struct {
			Documents []string `json:"documents"`
		}
		_ = json.Unmarshal(body, &req)
		results := make([]map[string]any, len(req.Documents))
		for i := range req.Documents {
			results[i] = map[string]any{"index": i, "relevance_score": float64(len(req.Documents) - i)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"results": results,
			"usage":   map[string]any{"prompt_tokens": 11},
		})
	default:
		t.Errorf("unexpected wire path %q — a new inference wire site escaped the gate", r.URL.Path)
	}
}

// wireVecs answers one vector per input of an embed body: one for the bare
// string of the internal path, len(input) for a list (a gateway sub-block,
// W02-09) — as OpenAI data entries with their index, or as ollama's bare
// vectors.
func wireVecs(body []byte, vec string, indexed bool) string {
	var req struct {
		Input json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(body, &req)
	n := 1
	var list []string
	if json.Unmarshal(req.Input, &list) == nil {
		n = len(list)
	}
	parts := make([]string, n)
	for i := range parts {
		parts[i] = "[" + vec + "]"
		if indexed {
			parts[i] = fmt.Sprintf(`{"index":%d,"embedding":%s}`, i, parts[i])
		}
	}
	return strings.Join(parts, ",")
}

func invariantBackend(host string, proto backends.Protocol) backends.Backend {
	return backends.Backend{
		ID: "b-id", Name: "b", Host: host, Protocol: proto, Model: "m",
		Trust: backends.TrustFull, Roles: []string{"chat"}, Enabled: true,
		ModelMap: map[string]backends.ModelSpec{"default": {Model: "m"}},
	}
}

// noopSink drops every stream event — the invariant is on the wire/lease
// coupling, not the rendered turn.
type noopSink struct{}

func (noopSink) Event(string, any) error { return nil }

// TestDispatchAcquireEqualsWireAllSites is the MW6 count gate: it drives all
// six physical inference wire sites (plus a failover walk and a
// lease-free-without-wire early-out) through one shared counting admitter and
// one shared wire counter, then asserts the I-D1 invariant Σ acquires == Σ wire
// contacts. Every attempt runs as ClassBackground on pass-through policy —
// class is irrelevant to the coupling (an immediate admit either way), and
// background needs no principal hook, keeping the gate free of B8 noise.
func TestDispatchAcquireEqualsWireAllSites(t *testing.T) {
	var wireHits int64
	srv := newWireServer(t, &wireHits)
	ca := newCountingAdmitter(t)
	llmAdm := llm.Admission{Admitter: ca, Class: dispatch.ClassBackground}
	ecAdm := embedcache.Admission{Admitter: ca, Class: dispatch.ClassBackground}
	ctx := context.Background()

	// --- Site 1: client.go:190 (ollama non-stream) ---
	if _, _, _, err := llm.ChatChain(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOllama)},
		"chat", "s", "u", llm.Options{}, "", time.Second, nil, llmAdm); err != nil {
		t.Fatalf("ollama non-stream: %v", err)
	}
	// --- Site 2: client.go:263 (openai non-stream) ---
	if _, _, _, err := llm.ChatChain(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)},
		"chat", "s", "u", llm.Options{}, "", time.Second, nil, llmAdm); err != nil {
		t.Fatalf("openai non-stream: %v", err)
	}
	// --- Site 3: stream.go:288 ---
	e := &Engine{cfg: Config{}.withDefaults(), now: time.Now}
	so := e.runStream(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, false, 0, 1, "sess", "key", backends.SensPublic, llmAdm, noopSink{})
	if !so.served || so.err != nil {
		t.Fatalf("stream: served=%v err=%v", so.served, so.err)
	}
	// --- Site 4: embed.go:144 (ollama) ---
	if _, _, _, wired, err := embedcache.EmbedChain(ctx, nil, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOllama)},
		"embed", "ollama embed", embed.PrefixQuery, nil, ecAdm); err != nil || !wired {
		t.Fatalf("ollama embed: wired=%v err=%v", wired, err)
	}
	// --- Site 5: embed.go:185 (openai) ---
	if _, _, _, wired, err := embedcache.EmbedChain(ctx, nil, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)},
		"embed", "openai embed", embed.PrefixQuery, nil, ecAdm); err != nil || !wired {
		t.Fatalf("openai embed: wired=%v err=%v", wired, err)
	}
	// --- Site 6: rerank.go:96 ---
	rrIn := []rrf.SearchResult{
		{ID: "A", Content: "a", RRFScore: 0.008},
		{ID: "B", Content: "b", RRFScore: 0.006},
		{ID: "C", Content: "c", RRFScore: 0.004},
	}
	if _, tel, err := rrf.RerankCrossEncoder(ctx, backends.Backend{Host: srv.URL}, "m", 50, 1.0, "q", rrIn, llmAdm); err != nil || !tel.Wired {
		t.Fatalf("rerank: wired=%v err=%v", tel.Wired, err)
	}

	// --- Site 7: anthropic.go (native Messages API, non-stream) — reached
	// through the same chain dispatcher as sites 1 and 2 (chatWithFormat). ---
	if _, _, _, err := llm.ChatChain(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolAnthropic)},
		"chat", "s", "u", llm.Options{}, "", time.Second, nil, llmAdm); err != nil {
		t.Fatalf("anthropic non-stream: %v", err)
	}

	// --- Site 8: anthropic_stream.go — the ChatStreamBackend dispatch under
	// the same stream lease as site 3. ---
	so = e.runStream(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolAnthropic)},
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, false, 0, 1, "sess", "key", backends.SensPublic, llmAdm, noopSink{})
	if !so.served || so.err != nil {
		t.Fatalf("anthropic stream: served=%v err=%v", so.served, so.err)
	}

	// --- Sites 9 + 10: passthrough.go (gateway ChatRaw / ChatRawStream,
	// W02-06) — reached ONLY through llm.WalkChain, which acquires per
	// attempt via the Acquirer before the wire call. ---
	wa := walkAcquirer{ca: ca, class: dispatch.ClassBackground}
	for _, stream := range []bool{false, true} {
		if served, _, err := llm.WalkChain(ctx, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)},
			"chat", wa, nil, walkRules, passthroughAttempt(stream)); err != nil || served == nil {
			t.Fatalf("passthrough stream=%v: served=%v err=%v", stream, served, err)
		}
	}

	// Ten sites, each one physical wire contact under one lease.
	if ca.count() != 10 || atomic.LoadInt64(&wireHits) != 10 {
		t.Fatalf("after the ten sites: acquires=%d wire=%d, want 10/10", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Failover walk (design W5: "inkl. Failover-Pfade"): a down primary
	// (502 → Next-class) forces a second attempt on the up backend. Both the
	// down and the up contact are physical wire calls, each under its own
	// lease: +2 acquires, +2 wire contacts. ---
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&wireHits, 1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(down.Close)
	if _, served, attempts, err := llm.ChatChain(ctx,
		[]backends.Backend{invariantBackend(down.URL, backends.ProtocolOllama), invariantBackend(srv.URL, backends.ProtocolOllama)},
		"chat", "s", "u", llm.Options{}, "", time.Second, nil, llmAdm); err != nil || served == nil || len(attempts) != 2 {
		t.Fatalf("failover walk: served=%v attempts=%d err=%v", served, len(attempts), err)
	}
	if ca.count() != 12 || atomic.LoadInt64(&wireHits) != 12 {
		t.Fatalf("after failover: acquires=%d wire=%d, want 12/12", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Gateway failover walk: the same down primary under WalkChain —
	// attempt 2 acquires through Next (TryAcquire), still one lease per
	// physical contact: +2/+2. ---
	if served, attempts, err := llm.WalkChain(ctx,
		[]backends.Backend{invariantBackend(down.URL, backends.ProtocolOpenAI), invariantBackend(srv.URL, backends.ProtocolOpenAI)},
		"chat", wa, nil, walkRules, passthroughAttempt(false)); err != nil || served == nil || len(attempts) != 2 {
		t.Fatalf("passthrough failover walk: served=%v attempts=%d err=%v", served, len(attempts), err)
	}
	if ca.count() != 14 || atomic.LoadInt64(&wireHits) != 14 {
		t.Fatalf("after passthrough failover: acquires=%d wire=%d, want 14/14", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Site 9 through the real /v1 handler (W02-07): oai.ChatCompletions
	// walks with its own attempt — one served request is +1/+1, the down
	// primary in front of it +2/+2 (dispatch_invariant_oai_test.go). ---
	postCensus(t, oaiCensusRoute(t, ca, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)}))
	if ca.count() != 15 || atomic.LoadInt64(&wireHits) != 15 {
		t.Fatalf("after the /v1 route: acquires=%d wire=%d, want 15/15", ca.count(), atomic.LoadInt64(&wireHits))
	}
	downRow := invariantBackend(down.URL, backends.ProtocolOpenAI)
	downRow.ID, downRow.Name, downRow.Priority = "b-down", "down", 10
	postCensus(t, oaiCensusRoute(t, ca, []backends.Backend{downRow, invariantBackend(srv.URL, backends.ProtocolOpenAI)}))
	if ca.count() != 17 || atomic.LoadInt64(&wireHits) != 17 {
		t.Fatalf("after the /v1 route failover: acquires=%d wire=%d, want 17/17", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Site 11: embed.go EmbedRaw (W02-09, G3) through the real
	// POST /v1/embeddings — five inputs in the census planner's blocks
	// [2, 2, 1]: block one under the First lease, blocks two and three each
	// under its own continuation lease. One lease per sub-block: +3/+3; one
	// lease over all blocks would leave Σacquire < Σwire. ---
	postEmbedCensus(t, oaiEmbedCensusRoute(t, ca, []backends.Backend{invariantBackend(srv.URL, backends.ProtocolOpenAI)}), 5)
	if ca.count() != 20 || atomic.LoadInt64(&wireHits) != 20 {
		t.Fatalf("after the /v1 embeddings route: acquires=%d wire=%d, want 20/20", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Lease-free without wire contact (the cache-hit clause's structural
	// twin, reachable cross-package): the rerank early-out below
	// RerankMinResults never touches the admitter and never contacts a
	// backend. Neither counter moves. ---
	beforeAcq, beforeWire := ca.count(), atomic.LoadInt64(&wireHits)
	tooFew := []rrf.SearchResult{{ID: "A", Content: "a", RRFScore: 0.008}, {ID: "B", Content: "b", RRFScore: 0.006}}
	if _, tel, err := rrf.RerankCrossEncoder(ctx, backends.Backend{Host: "http://unused.invalid"}, "m", 50, 1.0, "q", tooFew, llmAdm); err != nil || tel.Wired {
		t.Fatalf("early-out: wired=%v err=%v, want lease-free no-wire", tel.Wired, err)
	}
	if ca.count() != beforeAcq || atomic.LoadInt64(&wireHits) != beforeWire {
		t.Fatalf("early-out moved a counter: acquires %d→%d wire %d→%d (must be lease-free)",
			beforeAcq, ca.count(), beforeWire, atomic.LoadInt64(&wireHits))
	}

	// --- The MW6 invariant: Σ acquires == Σ wire contacts. ---
	if got, want := ca.count(), atomic.LoadInt64(&wireHits); got != want {
		t.Fatalf("I-D1 broken: %d acquires vs %d physical wire contacts — a wire call ran without a lease (or vice versa)", got, want)
	}
}

// wirePin is the exact per-file http.NewRequestWithContext census of the three
// inference wire packages (design/01 §1.2). Line-drift-robust: a COUNT per
// file, never a line number. Update this map ONLY when a wire site is
// deliberately added/removed AND its acquire coupling is proven in
// TestDispatchAcquireEqualsWireAllSites.
var wirePin = map[string]int{
	"llm/client.go":           2, // /api/chat + /v1/chat/completions (non-stream)
	"llm/stream.go":           1, // /v1/chat/completions (stream)
	"llm/anthropic.go":        1, // /v1/messages (anthropic non-stream)
	"llm/anthropic_stream.go": 1, // /v1/messages (anthropic stream)
	"embed/embed.go":          2, // /api/embed + /v1/embeddings
	"rerank/rerank.go":        1, // /v1/rerank
	"llm/passthrough.go":      2, // /v1/chat/completions (gateway ChatRaw + ChatRawStream, W02-06)
}

// stampPin is the static half of the hop-stamp census (F-W4, design/04 §4.3
// "Zensus, zweistufig"): per wire file the number of wire sites that stamp
// their request — fedhop.Stamp directly, or a call of a stamp route
// (stampRoutes) — and hand the answer to fedhop.Observe. stampPin ≡ wirePin:
// the same keys, so a wire file without a stamp entry is red, and the same
// counts, because every wire site stamps and observes exactly once. Whoever
// adds a wire file adds it here and to hopDrivers (wire_hop_capture_test.go),
// whose capture census proves on the wire what this one counts in the source.
var stampPin = map[string]int{
	"llm/client.go":           2, // chatOllama + chatOpenAI, through setChatHeaders
	"llm/stream.go":           1, // ChatStream, through setChatHeaders
	"llm/anthropic.go":        1, // chatAnthropic, through setAnthropicWireHeaders
	"llm/anthropic_stream.go": 1, // chatStreamAnthropic, through setAnthropicWireHeaders
	"embed/embed.go":          2, // embedOllama + embedOpenAI, fedhop.Stamp directly
	"rerank/rerank.go":        1, // Score, fedhop.Stamp directly
	"llm/passthrough.go":      2, // ChatRaw + ChatRawStream, through setChatHeaders
}

// stampRoutes are the header helpers that carry the one fedhop.Stamp of the
// sites calling them — row extra_headers, then the stamp, then the core
// headers — keyed by name, with the file that declares them. A call of a
// route counts as the calling site's stamp; the route's own fedhop.Stamp
// does not count again.
var stampRoutes = map[string]string{
	"setChatHeaders":          "llm/client.go",
	"setAnthropicWireHeaders": "llm/anthropic.go",
}

// censusStampSites counts, per pinned wire file, the stamp calls of its sites
// (a stamp-route call, or a fedhop.Stamp outside a route body) and its
// fedhop.Observe calls; routes is the number of fedhop.Stamp calls inside
// each route body. Comments do not count (parseWireFile drops them).
func censusStampSites(t *testing.T) (stamps, observes, routes map[string]int) {
	t.Helper()
	stamps, observes, routes = map[string]int{}, map[string]int{}, map[string]int{}
	for rel := range wirePin {
		f := parseWireFile(t, rel)
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			route := ""
			if stampRoutes[fn.Name.Name] == rel && fn.Recv == nil {
				route = fn.Name.Name
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				if id, ok := call.Fun.(*ast.Ident); ok && stampRoutes[id.Name] != "" {
					stamps[rel]++
				}
				switch recv, name := selectorCall(call); {
				case recv == "fedhop" && name == "Stamp" && route != "":
					routes[route]++
				case recv == "fedhop" && name == "Stamp":
					stamps[rel]++
				case recv == "fedhop" && name == "Observe":
					observes[rel]++
				}
				return true
			})
		}
	}
	return stamps, observes, routes
}

// TestStampPinMatchesWirePin is the static gate of the hop stamp (F-W4 (a)):
// stampPin ≡ wirePin, every pinned wire file carries as many stamp calls and
// as many fedhop.Observe calls as it has wire sites, and every stamp route
// stamps exactly once. A site whose stamp is removed, a new wire file without
// a stamp entry, or a route that loses its fedhop.Stamp turns it red.
func TestStampPinMatchesWirePin(t *testing.T) {
	for file, n := range wirePin {
		if got, ok := stampPin[file]; !ok || got != n {
			t.Errorf("stampPin[%s] = %d (present %v), wirePin says %d — every wire file is stamped at each of its sites", file, got, ok, n)
		}
	}
	for file := range stampPin {
		if _, ok := wirePin[file]; !ok {
			t.Errorf("stampPin names %s, which wirePin does not — the two pins carry the same keys", file)
		}
	}
	stamps, observes, routes := censusStampSites(t)
	for file, want := range stampPin {
		if stamps[file] != want {
			t.Errorf("%s: %d stamped wire site(s) in the source, stampPin says %d (a site sends without fedhop.Stamp)", file, stamps[file], want)
		}
		if observes[file] != want {
			t.Errorf("%s: %d fedhop.Observe call(s) in the source, stampPin says %d (a site drops the answer headers unread)", file, observes[file], want)
		}
	}
	for name, file := range stampRoutes {
		if routes[name] != 1 {
			t.Errorf("stamp route %s (%s) carries %d fedhop.Stamp call(s), want exactly 1", name, file, routes[name])
		}
	}
}

// wirePackages are the packages the census scans. internal/oai is in it with
// pin 0 (design/02 §4.1 L4, B14): no wirePin key names an oai file, so ANY
// http.NewRequestWithContext there is an un-pinned site — the gateway handler
// package never talks to a backend itself; its wire calls are llm's.
var wirePackages = []string{"llm", "embed", "rerank", "oai"}

// TestWireSitePositiveListPinned pins the grep positive list: the ONLY
// http.NewRequestWithContext sites in internal/{llm,embed,rerank,oai}
// (wirePackages; oai with pin 0) are the ones listed above. A new un-listed wire file, or a changed count in a listed file,
// turns this gate red — the static half of the I-D1 endpoint proof (§1.2:
// admittiertheit itself is proven by the count gate, not by grep).
func TestWireSitePositiveListPinned(t *testing.T) {
	got := censusWireSites(t)
	for file, want := range wirePin {
		if got[file] != want {
			t.Errorf("wire site count for %s = %d, want %d (pin drift — a wire site was added/removed/moved)", file, got[file], want)
		}
	}
	for file, n := range got {
		if _, listed := wirePin[file]; !listed {
			t.Errorf("UN-PINNED wire site: %s carries %d http.NewRequestWithContext call(s) outside the positive list", file, n)
		}
	}
}

// internalDir locates …/internal from this file.
func internalDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the source tree")
	}
	return filepath.Dir(filepath.Dir(self)) // …/internal/chat → …/internal
}

// censusWireSites counts http.NewRequestWithContext per non-test .go file across
// internal/{llm,embed,rerank,oai}, keyed "pkg/file". Files with zero matches are
// omitted so an added wire file surfaces as an un-pinned key.
func censusWireSites(t *testing.T) map[string]int {
	t.Helper()
	internalDir := internalDir(t)
	const needle = "http.NewRequestWithContext("
	got := map[string]int{}
	for _, pkg := range wirePackages {
		dir := filepath.Join(internalDir, pkg)
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("read %s: %v", dir, err)
		}
		for _, ent := range entries {
			name := ent.Name()
			if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				t.Fatalf("read %s/%s: %v", pkg, name, err)
			}
			if n := strings.Count(string(data), needle); n > 0 {
				got[pkg+"/"+name] = n
			}
		}
	}
	return got
}

// parseWireFile parses one pinned wire file (comments dropped — a doc comment
// naming httpClients.For(b.EgressGuarded()) must not count).
func parseWireFile(t *testing.T, rel string) *ast.File {
	t.Helper()
	f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(internalDir(t), rel), nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", rel, err)
	}
	return f
}

// selectorCall reports pkg.name of a call's function expression ("" when the
// callee is not a selector on an identifier).
func selectorCall(call *ast.CallExpr) (recv, name string) {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", ""
	}
	if id, ok := sel.X.(*ast.Ident); ok {
		return id.Name, sel.Sel.Name
	}
	return "", sel.Sel.Name
}

// TestEveryWireSiteSelectsEgressClient is the AST half of B19 (design/02 §5):
// in every pinned wire file
//   - the number of client choices X.For(b.EgressGuarded()) — the predicate
//     called on a plain identifier, the attempt's row, never on a literal —
//     equals the number of wire sites;
//   - every client handed to httpx.DoRetryOnce or a one-argument .Do is such
//     a choice or a parameter of the enclosing function (embed.go hands the
//     choice down to embedOpenAI/embedOllama), never a local value;
//   - no client is built or borrowed past the pair: no
//     httpx.PooledClient/GuardedPooledClient, no http.DefaultClient, no
//     http.Client literal or new(http.Client).
//
// A tenant row whose host rebinds to loopback/LAN must never be dialed without
// ssrfguard. The behaviour half per wire family: llm/egress_guard_test.go,
// llm/passthrough_test.go.
func TestEveryWireSiteSelectsEgressClient(t *testing.T) {
	for rel, sites := range wirePin {
		f := parseWireFile(t, rel)
		choices, requests := 0, 0
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				recv, name := selectorCall(x)
				if recv == "http" && name == "NewRequestWithContext" {
					requests++
				}
				if isEgressChoice(x) {
					choices++
				}
				if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "new" && len(x.Args) == 1 && isSelector(x.Args[0], "http", "Client") {
					t.Errorf("%s builds new(http.Client) — a wire file picks its client only via httpClients.For(b.EgressGuarded())", rel)
				}
			case *ast.SelectorExpr:
				if isSelector(x, "httpx", "PooledClient") || isSelector(x, "httpx", "GuardedPooledClient") || isSelector(x, "http", "DefaultClient") {
					t.Errorf("%s names %s — a wire file picks its client only via httpClients.For(b.EgressGuarded())", rel, x.Sel.Name)
				}
			case *ast.CompositeLit:
				// A *http.Client parameter type is fine (embed.go hands the
				// chosen client down); building one is not.
				if isSelector(x.Type, "http", "Client") {
					t.Errorf("%s builds an http.Client literal — a wire file picks its client only via httpClients.For(b.EgressGuarded())", rel)
				}
			}
			return true
		})
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			params := map[string]bool{}
			for _, p := range fn.Type.Params.List {
				for _, nm := range p.Names {
					params[nm.Name] = true
				}
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				recv, name := selectorCall(call)
				var client ast.Expr
				switch {
				case recv == "httpx" && name == "DoRetryOnce" && len(call.Args) > 0:
					client = call.Args[0]
				case name == "Do" && len(call.Args) == 1:
					if _, isFunc := call.Args[0].(*ast.FuncLit); isFunc {
						return true // sync.Once.Do(func(){…}) — no client call
					}
					client = call.Fun.(*ast.SelectorExpr).X
				default:
					return true
				}
				if c, ok := client.(*ast.CallExpr); ok && isEgressChoice(c) {
					return true
				}
				if id, ok := client.(*ast.Ident); ok && params[id.Name] {
					return true
				}
				t.Errorf("%s: %s sends with a client that is neither X.For(b.EgressGuarded()) nor a parameter of the function", rel, fn.Name.Name)
				return true
			})
		}
		if requests != sites {
			t.Errorf("%s: %d http.NewRequestWithContext call(s) in the AST, pin says %d", rel, requests, sites)
		}
		if choices != sites {
			t.Errorf("%s: %d .For(b.EgressGuarded()) client choice(s) for %d wire site(s) — every site picks the egress client by the one predicate (B19)", rel, choices, sites)
		}
	}
}

// isEgressChoice matches X.For(<ident>.EgressGuarded()).
func isEgressChoice(call *ast.CallExpr) bool {
	if _, name := selectorCall(call); name != "For" || len(call.Args) != 1 {
		return false
	}
	arg, ok := call.Args[0].(*ast.CallExpr)
	if !ok || len(arg.Args) != 0 {
		return false
	}
	sel, ok := arg.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "EgressGuarded" {
		return false
	}
	_, onIdent := sel.X.(*ast.Ident)
	return onIdent
}

// isSelector matches the expression pkg.name.
func isSelector(e ast.Expr, pkg, name string) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != name {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	return ok && id.Name == pkg
}

// oaiForbiddenImports are import paths internal/oai never takes, whatever
// local name: a reverse proxy or a TLS dial would talk to a backend past
// admission, overlay, egress choice and the header rule (B14/B19; fremd-
// review 2 W02-06 F-1). depguard rule oai-isolation (.golangci.yml) bans
// the same paths at lint time; this is the test-time half.
var oaiForbiddenImports = map[string]bool{
	"net/http/httputil": true,
	"crypto/tls":        true,
}

// oaiForbiddenRefs are the package-level names internal/oai never uses, keyed
// by IMPORT PATH (resolved per file, so an import alias — h "net/http" —
// changes nothing): wire calls and every client, transport or dialer.
// Without a client value the Get/Post METHODS of one are unreachable, so
// they need no receiver-blind rule (r.Header.Get stays legal).
var oaiForbiddenRefs = map[string]map[string]bool{
	"net/http": {
		"Get": true, "Head": true, "Post": true, "PostForm": true, "NewRequest": true, "NewRequestWithContext": true,
		"Client": true, "DefaultClient": true, "Transport": true, "DefaultTransport": true,
	},
	"net":                                 {"Dialer": true},
	"github.com/GottZ/ctx/internal/httpx": {"DoRetryOnce": true, "PooledClient": true, "GuardedPooledClient": true, "NewClients": true, "Clients": true},
}

// importNames maps each file-local package name to its import path: the
// alias when one is given, else the path's last element (true for every path
// the oai rules name). Dot and blank imports come back separately.
func importNames(f *ast.File) (names map[string]string, dotted []string) {
	names = map[string]string{}
	for _, imp := range f.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		switch {
		case imp.Name == nil:
			names[path[strings.LastIndex(path, "/")+1:]] = path
		case imp.Name.Name == ".":
			dotted = append(dotted, path)
		case imp.Name.Name != "_":
			names[imp.Name.Name] = path
		}
	}
	return names, dotted
}

// TestOAIPackageHasNoWireCalls is the AST half of the oai pin 0 (design/02
// §5 B14). No non-test file of internal/oai
//   - imports net/http/httputil or crypto/tls (oaiForbiddenImports), or
//     dot-imports a package the rules below name;
//   - names a package-level wire call, client, transport or dialer of
//     net/http, net or httpx (oaiForbiddenRefs; net.Dial* by prefix) — the
//     package is resolved through the file's imports, so an alias does not
//     hide it;
//   - calls a method named Do, RoundTrip or DialContext on any receiver —
//     fail-closed, because the AST carries no types (a sync.Once.Do there
//     trips this pin: teach the pin the receiver, never drop the rule).
func TestOAIPackageHasNoWireCalls(t *testing.T) {
	dir := filepath.Join(internalDir(t), "oai")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	files := 0
	for _, ent := range entries {
		name := ent.Name()
		if ent.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		files++
		f := parseWireFile(t, filepath.Join("oai", name))
		names, dotted := importNames(f)
		for _, imp := range f.Imports {
			if path := strings.Trim(imp.Path.Value, `"`); oaiForbiddenImports[path] {
				t.Errorf("oai/%s imports %s — internal/oai never talks to a backend itself (wire pin 0)", name, path)
			}
		}
		for _, path := range dotted {
			if _, ruled := oaiForbiddenRefs[path]; ruled || oaiForbiddenImports[path] {
				t.Errorf("oai/%s dot-imports %s — the pin cannot see its names; import it by name", name, path)
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok {
					path := names[id.Name]
					if oaiForbiddenRefs[path][sel.Sel.Name] || (path == "net" && strings.HasPrefix(sel.Sel.Name, "Dial")) {
						t.Errorf("oai/%s names %s.%s (%s) — internal/oai holds no backend client and makes no wire call (wire pin 0)", name, id.Name, sel.Sel.Name, path)
					}
				}
			}
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if recv, fn := selectorCall(call); fn == "Do" || fn == "RoundTrip" || fn == "DialContext" {
				t.Errorf("oai/%s calls %s.%s — internal/oai has wire pin 0; backend calls belong to internal/llm", name, recv, fn)
			}
			return true
		})
	}
	if files == 0 {
		t.Fatal("internal/oai has no non-test files — the pin would be vacuous")
	}
}
