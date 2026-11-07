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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"runtime"
	"runtime/metrics"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/oai"
)

// folgeSFrameMax is S1's sollwert: allocation per relayed frame byte.
const folgeSFrameMax = 2.0

// folgeSChildEnv names the case a child process runs (empty: the parent).
const folgeSChildEnv = "CTX_FOLGE_S_CHILD"

type folgeSDiscard struct {
	h    http.Header
	n    atomic.Int64
	code int
}

func (d *folgeSDiscard) Header() http.Header         { return d.h }
func (d *folgeSDiscard) Write(p []byte) (int, error) { d.n.Add(int64(len(p))); return len(p), nil }
func (d *folgeSDiscard) WriteHeader(c int)           { d.code = c }
func (d *folgeSDiscard) Flush()                      {}

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

// folgeSRelay measures llm.ChatRawStream alone (the relay reader) with a
// no-op callback — the llm share of S1, logged beside the route's.
func folgeSRelay(t *testing.T, frameCap int64, payload string, k int) uint64 {
	row := folgeSEngine(t, payload, k, nil, nil).row("good", 10)
	body := []byte(`{"model":"MODEL-CANARY","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	cb := func([]byte) error { return nil }
	if _, err := llm.ChatRawStream(context.Background(), row, body, 30*time.Second, frameCap, cb); err != nil {
		t.Fatal(err)
	}
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	a0 := ms.TotalAlloc
	if _, err := llm.ChatRawStream(context.Background(), row, body, 30*time.Second, frameCap, cb); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&ms)
	return ms.TotalAlloc - a0
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
// and the 4-MiB cap − 64. Median of three. The llm share (the relay reader
// alone) is logged beside it.
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
		var route, relay []float64
		for range 3 {
			rb := folgeSRoute(t, c.frameCap, big, c.k, false)
			rt := folgeSRoute(t, c.frameCap, tiny, c.k, false)
			per := float64(c.k * (len(big) - len(tiny)))
			route = append(route, (float64(rb.alloc)-float64(rt.alloc))/per)
			relay = append(relay, (float64(folgeSRelay(t, c.frameCap, big, c.k))-float64(folgeSRelay(t, c.frameCap, tiny, c.k)))/per)
		}
		f := folgeSMedian(route)
		t.Logf("FOLGE-S S1: cap=%d frame=%d k=%d route %.2f × (runs %.2f) — llm share %.2f ×, oai share %.2f ×", c.frameCap, len(big), c.k, f, route, folgeSMedian(relay), f-folgeSMedian(relay))
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
	u := &memEngine{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
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

// TestNonStreamWorkAtTheCap is S4's allocation half: a non-streamed answer
// of 1 MiB and at max_response_bytes (4 MiB − 4 KiB) allocates at most
// folgeSFrameMax × its bytes beyond a 300-byte answer. Median of three.
func TestNonStreamWorkAtTheCap(t *testing.T) {
	folgeSSkip(t)
	tiny := folgeSAnswer(300)
	for _, size := range []int{1 << 20, memRespCap - 4096} {
		big := folgeSAnswer(size)
		var fs []float64
		for range 3 {
			rb := folgeSNonStream(t, big, false)
			rt := folgeSNonStream(t, tiny, false)
			fs = append(fs, (float64(rb.alloc)-float64(rt.alloc))/float64(len(big)-len(tiny)))
		}
		f := folgeSMedian(fs)
		t.Logf("FOLGE-S S4: answer=%d B resp_cap=%d ⇒ %.2f × (runs %.2f)", len(big), memRespCap, f, fs)
		if f > folgeSFrameMax {
			t.Errorf("a non-streamed answer of %d B allocates %.2f × its bytes, want at most %.1f × (FOLGE-S S4)", len(big), f, folgeSFrameMax)
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
		cmd := exec.Command(os.Args[0], "-test.run=^"+test+"$", "-test.count=1", "-test.v")
		cmd.Env = append(os.Environ(), folgeSChildEnv+"="+c, "GOMEMLIMIT=200MiB")
		out, err := cmd.CombinedOutput()
		var line string
		for _, l := range strings.Split(string(out), "\n") {
			if i := strings.Index(l, "FOLGES "); i >= 0 {
				line = l[i:]
			}
		}
		t.Logf("%s %s: %s", test, c, line)
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
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			codes[i] = folgeSPost(rig, body).code
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
	v := folgeSVerdict{Case: spec, GrowthMiB: mibOf(growth), BoundMiB: mibOf(bound), HeldMiB: mibOf(s), HeapInMiB: mibOf(int64(peakInuse.Load()) - int64(inuse0)), GCCycles: gc, Codes: strings.TrimSpace(cs.String()), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"), Over: growth > bound}
	j, _ := json.Marshal(v)
	fmt.Printf("FOLGES %s\n", j)
	for i, c := range codes {
		if c != http.StatusOK {
			t.Errorf("stream %d answered %d", i, c)
		}
	}
	if growth > bound {
		t.Errorf("%d streams at cap %d: the live heap grew %.1f MiB, they held %.1f MiB (bound %.1f MiB with n × the harness load) — FOLGE-S S2", n, frameCap, mibOf(growth), mibOf(s), mibOf(bound))
	}
}
