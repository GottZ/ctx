package oai_test

// The heap gate of the chat parse (FOLGE-P, gate 1) for the stream half
// (W02-08 rebase round 2, part A2): a stream request holds on the heap only
// what its reservation covers — the request share of the byte ledger (3 ×
// the body, grown by every booking) and the answer share of a stream, 2 ×
// gateway.max_stream_frame_bytes instead of the 2 × max_response_bytes of a
// non-streamed answer. The rig is FOLGE-P's (memMount, memForm, heldSum) with
// an engine that streams: a few chunks, no usage chunk — the estimate path,
// the one that counts the request's runes. One form and one concurrency per
// process, as the parse gate:
//
//	CTX_W0208_STREAM_FORM=f16_text_ffa CTX_W0208_STREAM_N=10 [CTX_W0208_STREAM_BYTES=1048576] \
//	  GOMEMLIMIT=200MiB go test -run '^TestChatStreamHeapGate$' ./internal/oai/
//
// The bound is the parse gate's with the stream's answer share: the sum of
// the reservations the requests held plus the harness load per attempt;
// for n = 1 also without the answer share of the served streams (the parse,
// the scan and the stream's own request-sized work are covered by the
// request share alone), for n ≤ 10 the budget's own peak.

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/oai"
)

// memStreamFrameCap is the production default of
// gateway.max_stream_frame_bytes.
const memStreamFrameCap = 1 << 20

// newMemStreamEngine is a fake engine that discards the request and streams
// three content chunks and [DONE] — no usage chunk, no timings.
func newMemStreamEngine(t testing.TB) *memEngine {
	u := &memEngine{}
	u.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u.hits.Add(1)
		n, _ := io.Copy(io.Discard, r.Body)
		u.total.Add(n)
		for {
			p := u.max.Load()
			if n <= p || u.max.CompareAndSwap(p, n) {
				break
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for i, d := range []string{"hello", " world", ""} {
			fin := ""
			if i == 2 {
				fin = "stop"
			}
			_, _ = io.WriteString(w, "data: "+llamaChunk(d, fin)+"\n\n")
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(u.srv.Close)
	return u
}

// newMemStreamRig is newMemRig with the streaming engine.
func newMemStreamRig(t *testing.T, n int, held *heldSum) (*chatRig, *memEngine) {
	up := newMemStreamEngine(t)
	rig := newChatRig(t, []backends.Backend{up.row("good", 10)}, 4*n, 1, withStreamSettings)
	rig.setSettings(func(s *oai.Settings) {
		s.InflightBudget, s.MaxRequestBytes, s.MaxResponseBytes, s.MaxStreamFrameBytes = memBudget, memCap, memRespCap, memStreamFrameCap
		if v := os.Getenv("CTX_FOLGEP_BODYLOG"); v != "" {
			s.BodyLogging = v
		}
	})
	rig.deps.Record = func(row llmlog.GatewayRow) { _ = llmlog.GatewayEntry(row) }
	rig.router = memMount(rig, oai.ChatCompletions(rig.deps), held)
	return rig, up
}

// streamForm is memForm's body as a stream request: "stream":true first.
func streamForm(t testing.TB, name string, lim int) []byte {
	const key = `"stream":true,`
	shape := memForm(t, name, lim-len(key))
	if shape.chunked || shape.failover > 0 || shape.model != "chat" || !bytes.HasPrefix(shape.body, []byte("{")) {
		t.Fatalf("form %s: the stream gate takes a declared-length single-row form of the alias chat", name)
	}
	return append([]byte("{"+key), shape.body[1:]...)
}

func TestChatStreamHeapGate(t *testing.T) {
	form := os.Getenv("CTX_W0208_STREAM_FORM")
	if form == "" {
		t.Skip("CTX_W0208_STREAM_FORM not set (measurement test, see the file comment)")
	}
	n, _ := strconv.Atoi(os.Getenv("CTX_W0208_STREAM_N"))
	n = max(n, 1)
	lim := memCap - 16
	if v, err := strconv.Atoi(os.Getenv("CTX_W0208_STREAM_BYTES")); err == nil && v > 0 {
		lim = v
	}
	body := streamForm(t, form, lim)
	var held heldSum
	rig, up := newMemStreamRig(t, n, &held)

	runtime.GC()
	runtime.GC()
	debug.FreeOSMemory()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	base, gc0 := ms.HeapInuse, ms.NumGC
	var peak atomic.Uint64
	var budgetPeak atomic.Int64
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		var s runtime.MemStats
		for {
			runtime.ReadMemStats(&s)
			memMax(&peak, s.HeapInuse)
			if u := rig.budget.InUse(); u > budgetPeak.Load() {
				budgetPeak.Store(u)
			}
			select {
			case <-stop:
				return
			default:
			}
			time.Sleep(200 * time.Microsecond)
		}
	}()
	statuses := make([]int, n)
	frames := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			<-start
			rig.router.ServeHTTP(w, req)
			statuses[i] = w.Code
			frames[i] = strings.Count(w.Body.String(), "data: ")
		}(i)
	}
	t0 := time.Now()
	close(start)
	wg.Wait()
	dur := time.Since(t0)
	close(stop)
	<-done
	runtime.ReadMemStats(&ms)

	cnt := map[int]int{}
	for i, s := range statuses {
		cnt[s]++
		if s == http.StatusOK && frames[i] < 4 {
			t.Errorf("stream %d: 200 with %d data frames, want the relayed stream", i, frames[i])
		}
	}
	keys := make([]int, 0, len(cnt))
	for k := range cnt {
		keys = append(keys, k)
	}
	sort.Ints(keys)
	var sb strings.Builder
	for _, k := range keys {
		fmt.Fprintf(&sb, "%d×%d ", k, cnt[k])
	}
	sum, handled := held.sum()
	growth := int64(peak.Load()) - int64(base)
	served := int64(cnt[http.StatusOK])
	bound := sum + int64(n)*heapGateBaseLoad
	boundNoAnswer := sum - served*2*memStreamFrameCap + int64(n)*heapGateBaseLoad
	boundPeak := budgetPeak.Load() + int64(n)*heapGateBaseLoad
	mib := func(v int64) float64 { return float64(v) / (1 << 20) }
	verdict := "ok"
	switch {
	case growth > bound:
		verdict = "OVER"
	case n == 1 && growth > boundNoAnswer:
		verdict = "OVER_NOANSWER"
	case n > 1 && n <= 10 && growth > boundPeak:
		verdict = "OVER_PEAK"
	}
	fmt.Printf("W0208STREAM form=%s n=%d gomemlimit=%q bodylog=%q body=%d answers=[%s] growth_MiB=%.1f held_sum_MiB=%.1f handled=%d bound_MiB=%.1f "+
		"bound_noanswer_MiB=%.1f bound_peak_MiB=%.1f budget_peak_MiB=%.1f numgc=%d dur=%v up_hits=%d up_max_body=%d verdict=%s\n",
		form, n, os.Getenv("GOMEMLIMIT"), os.Getenv("CTX_FOLGEP_BODYLOG"), len(body), strings.TrimSpace(sb.String()), mib(growth), mib(sum), handled,
		mib(bound), mib(boundNoAnswer), mib(boundPeak), mib(budgetPeak.Load()), ms.NumGC-gc0, dur.Round(time.Millisecond), up.hits.Load(), up.max.Load(), verdict)
	if served == 0 {
		t.Errorf("form %s, n=%d: no stream served (%s)", form, n, strings.TrimSpace(sb.String()))
	}
	if verdict != "ok" {
		t.Errorf("form %s, n=%d: heap growth %.1f MiB > reserved %.1f MiB + %d × base load = %.1f MiB (without the streams' answer share %.1f, the budget's peak %.1f): %s",
			form, n, mib(growth), mib(sum), n, mib(bound), mib(boundNoAnswer), mib(boundPeak), verdict)
	}
}
