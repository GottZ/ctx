package oai_test

// The gates of FOLGE-S (RC-D S1, cluster C8; LOOP §Queue 62, M-1 of W02-08):
// the work of one relayed stream frame and of a non-streamed answer at its
// cap, against the sollwerte of W02-08 external review 5 point 5:
//
//	S1 TotalAlloc per relayed frame ≤ 2 × F (Ist 17–19 ×),
//	S2 GC-marked live heap of n concurrent streams ≤ the reservations they
//	   held + n × heapGateBaseLoad (Ist n = 8: 50.6–82.1 MiB against 18),
//	S3 the house metric (HeapInuse growth, FOLGE-P's verdict rule) with
//	   1-MiB and 4-MiB frames ≤ held + heapGateBaseLoad (Ist 49–58 MiB
//	   against 2.25),
//	S4 the non-streamed answer at max_response_bytes: S1's and S3's
//	   sollwerte (Ist 9.40 × and 28–32 MiB against 8).
//
// Red against ab6b2415, green after FOLGE-S S2 (the reused line buffer of
// the relay reader, []byte to the callback, streamChunk without Content, the
// token-walk rewrite into a reused, pre-framed output buffer). Measurement
// forms are the W02-08 merge round 3 probes (m3-probe/zz_m1_probe_test.go),
// in the tree now. Outside -short and outside -race (skipUnderRace); the
// heap gates run one case per child process under GOMEMLIMIT=200MiB, as the
// FOLGE-P heap gates do.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// folgeSFrameMax is S1's sollwert: allocation per relayed frame byte.
const folgeSFrameMax = 2.0

// folgeSOverflowText is the relay overflow's message (relay.go
// relayOverflowMessage) as the client reads it in the 503 envelope, and
// folgeSOverflowRetries how often a case is run again when a stream was cut
// by it (folgeSChildren).
const (
	folgeSOverflowText    = "The client read the stream slower than the model wrote it"
	folgeSOverflowRetries = 2
)

// folgeSChildEnv names the case a child process runs (empty: the parent).
const folgeSChildEnv = "CTX_FOLGE_S_CHILD"

type folgeSDiscard struct {
	h    http.Header
	n    atomic.Int64
	code int
	head []byte // the first bytes of the body (an error envelope)
}

func (d *folgeSDiscard) Header() http.Header { return d.h }
func (d *folgeSDiscard) Write(p []byte) (int, error) {
	if room := 512 - len(d.head); room > 0 {
		d.head = append(d.head, p[:min(room, len(p))]...)
	}
	d.n.Add(int64(len(p)))
	return len(p), nil
}
func (d *folgeSDiscard) WriteHeader(c int) { d.code = c }
func (d *folgeSDiscard) Flush()            {}

// folgeSPayload is an SGLang content chunk of exactly size bytes.
func folgeSPayload(size int) string {
	empty := sglangChunk("", "")
	return sglangChunk(strings.Repeat("a", size-len(empty)), "")
}

// folgeSEngine streams k frames of payload and the end; gate, when set,
// holds every stream until it is closed (S2: all streams live at once).
func folgeSEngine(t testing.TB, payload string, k int, arrived func(), gate <-chan struct{}) *memEngine {
	frame := []byte("data: " + payload + "\n\n")
	last := []byte("data: " + sglangChunk("", "stop") + "\n\ndata: [DONE]\n\n")
	u := &memEngine{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if arrived != nil {
			arrived()
		}
		if gate != nil {
			select {
			case <-gate:
			case <-r.Context().Done():
				return
			}
		}
		for range k {
			_, _ = w.Write(frame)
		}
		_, _ = w.Write(last)
	}))
	t.Cleanup(u.srv.Close)
	return u
}

type folgeSResult struct {
	alloc, mallocs uint64
	growth         int64 // HeapInuse growth (the house metric), when sampled
	held           int64
	wrote          int64
	code           int
}

// folgeSRig is the stream route over FOLGE-P's mount with one engine row.
func folgeSRig(t *testing.T, up *memEngine, slots int, frameCap int64, held *heldSum) *chatRig {
	rig := newChatRig(t, []backends.Backend{up.row("good", 10)}, slots, 1, withStreamSettings)
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes, s.MaxStreamFrameBytes = memBudget, memCap, memRespCap, frameCap
	})
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), held)
	return rig
}

func folgeSPost(rig *chatRig, body string) *folgeSDiscard {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := &folgeSDiscard{h: http.Header{}}
	rig.router.ServeHTTP(w, req)
	return w
}

// folgeSSample samples HeapInuse every 100 µs until stop; peak is its max.
func folgeSSample(peak *atomic.Uint64, stop <-chan struct{}, done chan<- struct{}) {
	defer close(done)
	var s runtime.MemStats
	for {
		runtime.ReadMemStats(&s)
		memMax(peak, s.HeapInuse)
		select {
		case <-stop:
			return
		default:
		}
		time.Sleep(100 * time.Microsecond)
	}
}

// folgeSRoute runs one stream of k frames of payload through the route
// (after one unmeasured warm-up request) and measures it.
func folgeSRoute(t *testing.T, frameCap int64, payload string, k int, sample bool) folgeSResult {
	var held heldSum
	rig := folgeSRig(t, folgeSEngine(t, payload, k, nil, nil), 4, frameCap, &held)
	body := streamBody("chat")
	folgeSPost(rig, body)
	held = heldSum{}
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	a0, m0, base := ms.TotalAlloc, ms.Mallocs, ms.HeapInuse
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	if sample {
		go folgeSSample(&peak, stop, done)
	} else {
		close(done)
	}
	w := folgeSPost(rig, body)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)
	s, _ := held.sum()
	r := folgeSResult{alloc: ms.TotalAlloc - a0, mallocs: ms.Mallocs - m0, held: s, wrote: w.n.Load(), code: w.code}
	if sample {
		r.growth = int64(peak.Load()) - int64(base)
	}
	if w.code != http.StatusOK && w.code != 0 {
		t.Errorf("route answered %d", w.code)
	}
	return r
}

func folgeSMedian(v []float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

func folgeSSkip(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("FOLGE-S measurement gate: outside -short (megabytes per frame, child processes)")
	}
	skipUnderRace(t, "the heap bytes one relayed frame allocates")
}

// TestStreamFrameWorkPerFrame is S1: TotalAlloc per relayed frame over the
// route — (k frames of F) − (k frames of a minimal chunk), per frame byte —
// at most folgeSFrameMax for F of 1 KiB, 64 KiB, the 1-MiB default cap − 64
// and the 4-MiB cap − 64. Median of three. The stages are pinned where a
// wire call is allowed: the relay reader in internal/llm
// (TestReadSSELineIntoReusesItsBuffer, TestDecodeChunkMetaSkipsTheText) —
// internal/oai's X13 census admits no llm.ChatRawStream outside a lease.
func TestStreamFrameWorkPerFrame(t *testing.T) {
	folgeSSkip(t)
	tiny := folgeSPayload(len(sglangChunk("", "")) + 1)
	for _, c := range []struct {
		frameCap int64
		size, k  int
	}{
		{1 << 20, 1 << 10, 64}, {1 << 20, 64 << 10, 16}, {1 << 20, 1<<20 - 64, 8}, {4 << 20, 4<<20 - 64, 4},
	} {
		big := folgeSPayload(c.size)
		var route []float64
		for range 3 {
			rb := folgeSRoute(t, c.frameCap, big, c.k, false)
			rt := folgeSRoute(t, c.frameCap, tiny, c.k, false)
			route = append(route, (float64(rb.alloc)-float64(rt.alloc))/float64(c.k*(len(big)-len(tiny))))
		}
		f := folgeSMedian(route)
		t.Logf("FOLGE-S S1: cap=%d frame=%d k=%d route %.2f × (runs %.2f)", c.frameCap, len(big), c.k, f, route)
		if f > folgeSFrameMax {
			t.Errorf("cap %d, frames of %d B: a relayed frame allocates %.2f × its bytes, want at most %.1f × (FOLGE-S S1)", c.frameCap, len(big), f, folgeSFrameMax)
		}
	}
}

// folgeSAnswer is a non-streamed completion of exactly size bytes.
func folgeSAnswer(size int) string {
	mk := func(c string) string {
		return fmt.Sprintf(`{"id":"ID-CANARY","object":"chat.completion","created":1700000000,"model":"MODEL-CANARY","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, c)
	}
	return mk(strings.Repeat("a", size-len(mk(""))))
}

// folgeSNonStream runs one non-streamed request answered with answer.
func folgeSNonStream(t *testing.T, answer string, sample bool) folgeSResult {
	return folgeSNonStreamCL(t, answer, sample, false)
}

// folgeSNonStreamCL is folgeSNonStream with the engine's answer header:
// contentLength sends Content-Length (what llama.cpp, vLLM and SGLang send
// for a non-streamed answer), else the answer goes out chunked (httptest
// with more than its buffer written: no length).
func folgeSNonStreamCL(t *testing.T, answer string, sample, contentLength bool) folgeSResult {
	u := &memEngine{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		if contentLength {
			w.Header().Set("Content-Length", strconv.Itoa(len(answer)))
		}
		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(u.srv.Close)
	var held heldSum
	rig := folgeSRig(t, u, 4, 1<<20, &held)
	body := chatBody("chat")
	folgeSPost(rig, body)
	held = heldSum{}
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	a0, base := ms.TotalAlloc, ms.HeapInuse
	var peak atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	if sample {
		go folgeSSample(&peak, stop, done)
	} else {
		close(done)
	}
	w := folgeSPost(rig, body)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)
	s, _ := held.sum()
	r := folgeSResult{alloc: ms.TotalAlloc - a0, held: s, wrote: w.n.Load(), code: w.code}
	if sample {
		r.growth = int64(peak.Load()) - int64(base)
	}
	if w.code != http.StatusOK {
		t.Errorf("non-stream answered %d", w.code)
	}
	return r
}

// folgeSAnswerSlack is the named slack of S4: the allocation rounding of
// the answer's two buffers (the read and the rewrite), each at most one
// 8-KiB page above its length (embed.WireBodySlack's derivation) — the
// plan's standard "upper bound plus a named slack" (LOOP §Queue 62).
const folgeSAnswerSlack = 2 * (8 << 10)

// folgeSChunkedSlack is S4's named slack for a chunked answer: the read
// (llm readAnswer) doubles from 512 B to the answer, and every buffer from
// 32 KiB on rounds up by at most one 8-KiB page — eight of them up to the
// 4-MiB cap — plus the rewrite's buffer: nine pages.
const folgeSChunkedSlack = 9 * (8 << 10)

// TestNonStreamWorkAtTheCap is S4's allocation half, per the main context's
// decision of 2026-11-08 (bau report RCD-c8-hardening-S2 §FOLGE-S; it
// replaces the S1 cell "≤ 2.0 × with a chunked engine", which no read of an
// answer of unknown length meets: the read doubles to the answer, ≈ 2 × L,
// before the rewrite's 1 × L). A non-streamed answer of 1 MiB and at
// max_response_bytes (4 MiB − 4 KiB), beyond a 300-byte answer, median of
// three:
//
//   - S4, from an engine that sends Content-Length (the production case):
//     at most 2 × its bytes + folgeSAnswerSlack (the read in one buffer of
//     the answer's length, the rewrite in one more);
//   - S4', chunked: at most 3 × its bytes + folgeSAnswerSlack, and the
//     reservation the request held grows with what the read leaves behind
//     — the growth's discarded buffers are booked (the ledger stays true):
//     the size-dependent allocation is at most what the request held plus
//     folgeSChunkedSlack (the pages' rounding, which no booking names).
func TestNonStreamWorkAtTheCap(t *testing.T) {
	folgeSSkip(t)
	tiny := folgeSAnswer(300)
	for _, cell := range []struct {
		name          string
		contentLength bool
		copies        int
		slack         int64
	}{{"S4 content-length", true, 2, folgeSAnswerSlack}, {"S4' chunked", false, 3, folgeSChunkedSlack}} {
		for _, size := range []int{1 << 20, memRespCap - 4096} {
			big := folgeSAnswer(size)
			var fs []float64
			var deltas []int64
			var held int64
			for range 3 {
				rb := folgeSNonStreamCL(t, big, false, cell.contentLength)
				rt := folgeSNonStreamCL(t, tiny, false, cell.contentLength)
				d := int64(rb.alloc) - int64(rt.alloc)
				deltas = append(deltas, d)
				fs = append(fs, float64(d)/float64(len(big)-len(tiny)))
				held = max(held, rb.held)
			}
			f := folgeSMedian(fs)
			slices.Sort(deltas)
			d := deltas[len(deltas)/2]
			bound := int64(cell.copies)*int64(len(big)-len(tiny)) + cell.slack
			t.Logf("FOLGE-S %s: answer=%d B resp_cap=%d ⇒ %.3f × (runs %.3f), Δ %d B against %d B; held %d B", cell.name, len(big), memRespCap, f, fs, d, bound, held)
			if d > bound {
				t.Errorf("%s: a non-streamed answer of %d B allocates %d B beyond a small one (%.3f ×), want at most %d × its bytes + %d B (FOLGE-S S4)", cell.name, len(big), d, f, cell.copies, cell.slack)
			}
			if !cell.contentLength && d > held+cell.slack {
				t.Errorf("%s: the answer of %d B allocates %d B beyond a small one, the request held %d B — the read's discarded buffers are not booked", cell.name, len(big), d, held)
			}
		}
	}
}

// folgeSVerdict is one child's measurement, printed as one JSON line.
type folgeSVerdict struct {
	Case       string  `json:"case"`
	GrowthMiB  float64 `json:"growth_mib"`
	BoundMiB   float64 `json:"bound_mib"`
	HeldMiB    float64 `json:"held_mib"`
	HeapInMiB  float64 `json:"heap_inuse_growth_mib,omitempty"`
	GCCycles   uint64  `json:"gc_cycles,omitempty"`
	Codes      string  `json:"codes,omitempty"`
	Overflow   int     `json:"relay_overflow,omitempty"`
	GOMEMLIMIT string  `json:"gomemlimit"`
	Over       bool    `json:"over"`
}

func mibOf(v int64) float64 { return float64(v) / (1 << 20) }

// folgeSChildren runs every case of test in a child process of its own
// under GOMEMLIMIT=200MiB and fails for every child that failed; a child
// prints its verdict as a FOLGES line.
func folgeSChildren(t *testing.T, test string, cases []string) {
	t.Helper()
	for _, c := range cases {
		var out []byte
		var err error
		for attempt := 1; ; attempt++ {
			cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1", "-test.v")
			cmd.Env = append(os.Environ(), folgeSChildEnv+"="+c, "GOMEMLIMIT=200MiB")
			out, err = cmd.CombinedOutput()
			var line string
			for _, l := range strings.Split(string(out), "\n") {
				if i := strings.Index(l, "FOLGES "); i >= 0 {
					line = l[i:]
				}
			}
			t.Logf("%s %s (attempt %d): %s", test, c, attempt, line)
			// A stream the relay cut because the harness's client fell
			// behind (A03-W10's overflow, a drain-timing outcome under host
			// load, not a heap outcome) is run again, at most
			// folgeSOverflowRetries times; the heap verdict holds in every
			// attempt — a run over its bound fails at once.
			if err == nil || !strings.Contains(line, `"relay_overflow":`) || strings.Contains(line, `"over":true`) || attempt > folgeSOverflowRetries {
				break
			}
		}
		if err != nil {
			tail := string(out)
			if len(tail) > 1500 {
				tail = tail[len(tail)-1500:]
			}
			t.Errorf("%s case %s failed: %v\n%s", test, c, err, tail)
		}
	}
}

// TestStreamHeapGateLargeFrames is S3 and S4's heap half in the house
// metric: one request — a stream of frames at the cap (8 × (1 MiB − 64) at
// the 1-MiB default cap, 4 × (4 MiB − 64) at a 4-MiB cap) or a non-streamed
// answer at max_response_bytes — grows HeapInuse by at most the reservation
// it held plus heapGateBaseLoad. One case per child process.
func TestStreamHeapGateLargeFrames(t *testing.T) {
	folgeSSkip(t)
	spec := os.Getenv(folgeSChildEnv)
	if spec == "" {
		folgeSChildren(t, "TestStreamHeapGateLargeFrames", []string{"stream:1048576:1048512:8", "stream:4194304:4194240:4", "nonstream:4190208"})
		return
	}
	p := strings.Split(spec, ":")
	var r folgeSResult
	if p[0] == "nonstream" {
		size, _ := strconv.Atoi(p[1])
		r = folgeSNonStream(t, folgeSAnswer(size), true)
	} else {
		frameCap, _ := strconv.ParseInt(p[1], 10, 64)
		size, _ := strconv.Atoi(p[2])
		k, _ := strconv.Atoi(p[3])
		r = folgeSRoute(t, frameCap, folgeSPayload(size), k, true)
	}
	bound := r.held + heapGateBaseLoad
	v := folgeSVerdict{Case: spec, GrowthMiB: mibOf(r.growth), BoundMiB: mibOf(bound), HeldMiB: mibOf(r.held), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"), Over: r.growth > bound}
	j, _ := json.Marshal(v)
	fmt.Printf("FOLGES %s\n", j)
	if r.growth > bound {
		t.Errorf("%s: HeapInuse grew %.1f MiB, the request held %.1f MiB (bound %.1f MiB with the harness load) — FOLGE-S S3/S4", spec, mibOf(r.growth), mibOf(r.held), mibOf(bound))
	}
}

// folgeSLive reads the GC-marked live heap.
func folgeSLive(s []metrics.Sample) uint64 {
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// TestStreamLiveHeapConcurrent is S2: n streams relay 8 frames of
// (cap − 64) at once (the engine starts when all n arrived), and the peak
// of the GC-marked live heap grows by at most the reservations they held
// plus n × heapGateBaseLoad — n = 8 and 32 at the 1-MiB default cap, n = 8
// at a 4-MiB cap (32 × 4 MiB streams do not fit the default budget: each
// holds 2 × the cap and a relay buffer of one cap). One case per child.
func TestStreamLiveHeapConcurrent(t *testing.T) {
	folgeSSkip(t)
	spec := os.Getenv(folgeSChildEnv)
	if spec == "" {
		folgeSChildren(t, "TestStreamLiveHeapConcurrent", []string{"1048576:8", "1048576:32", "4194304:8"})
		return
	}
	p := strings.Split(spec, ":")
	frameCap, _ := strconv.ParseInt(p[0], 10, 64)
	n, _ := strconv.Atoi(p[1])
	const k = 8
	gate := make(chan struct{})
	var arrivals atomic.Int64
	var once sync.Once
	arrived := func() {
		if arrivals.Add(1) == int64(n) {
			once.Do(func() { close(gate) })
		}
	}
	up := folgeSEngine(t, folgeSPayload(int(frameCap)-64), k, arrived, gate)
	var held heldSum
	rig := folgeSRig(t, up, 4*n, frameCap, &held)
	body := streamBody("chat")
	// Warm-up: one stream through an open gate of its own engine is not
	// possible here (the gate is the case's); the first measured streams
	// carry the first-use allocations — n × heapGateBaseLoad covers them.
	go func() {
		time.Sleep(10 * time.Second)
		once.Do(func() { close(gate) })
	}()
	samples := []metrics.Sample{{Name: "/gc/heap/live:bytes"}, {Name: "/gc/cycles/total:gc-cycles"}}
	runtime.GC()
	runtime.GC()
	base := folgeSLive(samples)
	gc0 := samples[1].Value.Uint64()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	inuse0 := ms.HeapInuse
	var peak, peakInuse atomic.Uint64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
		var m runtime.MemStats
		for i := 0; ; i++ {
			memMax(&peak, folgeSLive(s))
			if i%16 == 0 {
				runtime.ReadMemStats(&m)
				memMax(&peakInuse, m.HeapInuse)
			}
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	codes := make([]int, n)
	heads := make([][]byte, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			w := folgeSPost(rig, body)
			codes[i], heads[i] = w.code, w.head
		}()
	}
	wg.Wait()
	close(stop)
	<-done
	folgeSLive(samples)
	gc := samples[1].Value.Uint64() - gc0
	s, _ := held.sum()
	growth := int64(peak.Load()) - int64(base)
	bound := s + int64(n)*heapGateBaseLoad
	var cs bytes.Buffer
	for _, c := range codes {
		fmt.Fprintf(&cs, "%d ", c)
	}
	overflow := 0
	for i, c := range codes {
		if c == http.StatusServiceUnavailable && bytes.Contains(heads[i], []byte(folgeSOverflowText)) {
			overflow++
		}
	}
	v := folgeSVerdict{Case: spec, GrowthMiB: mibOf(growth), BoundMiB: mibOf(bound), HeldMiB: mibOf(s), HeapInMiB: mibOf(int64(peakInuse.Load()) - int64(inuse0)), GCCycles: gc, Codes: strings.TrimSpace(cs.String()), Overflow: overflow, GOMEMLIMIT: os.Getenv("GOMEMLIMIT"), Over: growth > bound}
	j, _ := json.Marshal(v)
	fmt.Printf("FOLGES %s\n", j)
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("stream %d answered %d: %.200s", i, c, heads[i])
		}
	}
	if growth > bound {
		t.Errorf("%d streams at cap %d: the live heap grew %.1f MiB, they held %.1f MiB (bound %.1f MiB with n × the harness load) — FOLGE-S S2", n, frameCap, mibOf(growth), mibOf(s), mibOf(bound))
	}
}
