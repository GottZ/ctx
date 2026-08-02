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
		case "/api/embed": // embed.go:144 (ollama)
			fmt.Fprintf(w, `{"embeddings":[[%s]],"prompt_eval_count":7}`, vec)
		case "/v1/embeddings": // embed.go:185 (openai)
			fmt.Fprintf(w, `{"data":[{"embedding":[%s]}]}`, vec)
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
	}))
	t.Cleanup(srv.Close)
	return srv
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
		[]llm.ChatMsg{{Role: "user", Content: "hi"}}, false, 0, 1, "sess", "key", backends.SensPublic, llmAdm, noopSink{})
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
	if _, tel, err := rrf.RerankCrossEncoder(ctx, srv.URL, "", "m", "", 50, 1.0, "q", rrIn, llmAdm); err != nil || !tel.Wired {
		t.Fatalf("rerank: wired=%v err=%v", tel.Wired, err)
	}

	// Six sites, each one physical wire contact under one lease.
	if ca.count() != 6 || atomic.LoadInt64(&wireHits) != 6 {
		t.Fatalf("after the six sites: acquires=%d wire=%d, want 6/6", ca.count(), atomic.LoadInt64(&wireHits))
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
	if ca.count() != 8 || atomic.LoadInt64(&wireHits) != 8 {
		t.Fatalf("after failover: acquires=%d wire=%d, want 8/8", ca.count(), atomic.LoadInt64(&wireHits))
	}

	// --- Lease-free without wire contact (the cache-hit clause's structural
	// twin, reachable cross-package): the rerank early-out below
	// RerankMinResults never touches the admitter and never contacts a
	// backend. Neither counter moves. ---
	beforeAcq, beforeWire := ca.count(), atomic.LoadInt64(&wireHits)
	tooFew := []rrf.SearchResult{{ID: "A", Content: "a", RRFScore: 0.008}, {ID: "B", Content: "b", RRFScore: 0.006}}
	if _, tel, err := rrf.RerankCrossEncoder(ctx, "http://unused.invalid", "", "m", "", 50, 1.0, "q", tooFew, llmAdm); err != nil || tel.Wired {
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
	"llm/client.go":    2, // /api/chat + /v1/chat/completions (non-stream)
	"llm/stream.go":    1, // /v1/chat/completions (stream)
	"embed/embed.go":   2, // /api/embed + /v1/embeddings
	"rerank/rerank.go": 1, // /v1/rerank
}

// TestWireSitePositiveListPinned pins the grep positive list: the ONLY
// http.NewRequestWithContext sites in internal/{llm,embed,rerank} are the six
// listed above. A new un-listed wire file, or a changed count in a listed file,
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

// censusWireSites counts http.NewRequestWithContext per non-test .go file across
// internal/{llm,embed,rerank}, keyed "pkg/file". Files with zero matches are
// omitted so an added wire file surfaces as an un-pinned key.
func censusWireSites(t *testing.T) map[string]int {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed — cannot locate the source tree")
	}
	internalDir := filepath.Dir(filepath.Dir(self)) // …/internal/chat → …/internal
	const needle = "http.NewRequestWithContext("
	got := map[string]int{}
	for _, pkg := range []string{"llm", "embed", "rerank"} {
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
