package oai_test

// G15, the load gate of the stream route (W02-08; design/02 §4.2.1, W02-02
// review FR2-4): a measurement, not a red gate. It runs only with
// CTX_W0208_LOADGATE=1 (minutes of CPU, never in CI):
//
//	CTX_W0208_LOADGATE=1 go test -count=1 -run TestStreamLoadGate -v ./internal/oai/
//
// It logs the heap peak of 64 concurrent streams of 8 consumers against the
// in-flight budget, how many streams the default budget admits at once
// (the open user decision N-3: the reserve is taken before the queue), and
// how long 11 connections that declare an 8-MiB body and never send it
// lock the budget.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
)

// loadConsumer serves each request as the consumer its X-Load-Consumer
// header names (the rig's own consumer is cleared).
func loadConsumer(r *chatRig, h http.Handler) http.Handler {
	r.ca.Store(nil)
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		ca := urgentApp()
		n := req.Header.Get("X-Load-Consumer")
		ca.ConsumerID = fmt.Sprintf("00000000-0000-4000-8000-0000000c%04s", n)
		h.ServeHTTP(w, req.WithContext(context.WithValue(req.Context(), consumerKey{}, ca)))
	})
}

// heapPeak samples the heap every 5 ms until stop is closed.
func heapPeak(stop <-chan struct{}) <-chan uint64 {
	out := make(chan uint64, 1)
	go func() {
		var peak uint64
		var ms runtime.MemStats
		t := time.NewTicker(5 * time.Millisecond)
		defer t.Stop()
		for {
			runtime.ReadMemStats(&ms)
			peak = max(peak, ms.HeapAlloc)
			select {
			case <-stop:
				out <- peak
				return
			case <-t.C:
			}
		}
	}()
	return out
}

func TestStreamLoadGate(t *testing.T) {
	if os.Getenv("CTX_W0208_LOADGATE") != "1" {
		t.Skip("measurement gate G15: set CTX_W0208_LOADGATE=1")
	}
	const budget = 256 << 20 // the production default of gateway.inflight_budget
	settings := func(r *chatRig) {
		s := *r.settings.Load()
		s.InflightBudget, s.MaxRequestBytes, s.MaxStreamFrameBytes = budget, 8<<20, 1<<20
		r.settings.Store(&s)
	}
	chunk := strings.Repeat("y", 1024)
	steps := make([]sseStep, 0, 210)
	for i := 0; i < 200; i++ {
		steps = append(steps, sseStep{wait: 5 * time.Millisecond, data: sglangChunk(chunk, "")})
	}
	steps = append(steps, sseStep{data: sglangChunk("", "stop")}, sseStep{data: sglangUsage}, sseStep{data: "[DONE]"})

	t.Run("64 streams of 8 consumers", func(t *testing.T) {
		u := newUpstream(t, "")
		u.setRespond(sseRespond(steps, nil, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 129, 1, withStreamSettings, settings)
		srv := serveHTTP(t, loadConsumer(rig, rig.router))
		runtime.GC()
		var ms runtime.MemStats
		runtime.ReadMemStats(&ms)
		base := ms.HeapAlloc
		stop := make(chan struct{})
		peak := heapPeak(stop)
		var ok, other atomic.Int64
		var wg sync.WaitGroup
		start := time.Now()
		for i := 0; i < 64; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				a := postStream(context.Background(), t, srv, streamBody("chat"), "X-Load-Consumer", fmt.Sprint(i%8))
				if a.status == http.StatusOK && a.dones() == 1 {
					ok.Add(1)
				} else {
					other.Add(1)
				}
			}(i)
		}
		wg.Wait()
		close(stop)
		p := <-peak
		t.Logf("64 streams / 8 consumers: %d served to [DONE], %d other, %v; heap base %.1f MiB, peak %.1f MiB, peak-base %.1f MiB (budget %d MiB)",
			ok.Load(), other.Load(), time.Since(start), float64(base)/(1<<20), float64(p)/(1<<20), float64(p-base)/(1<<20), budget>>20)
		if p-base >= budget {
			t.Errorf("heap rose by %d bytes, at least the in-flight budget %d", p-base, budget)
		}
	})

	t.Run("streams admitted at once by the default budget (N-3)", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 301, 1, withStreamSettings, settings)
		srv := serveHTTP(t, loadConsumer(rig, rig.router))
		var open []*liveStream
		defer func() {
			for _, ls := range open {
				_ = ls.Close()
			}
		}()
		n := 0
		for ; n < 300; n++ {
			ls, each := openAndRead(t, context.Background(), srv, streamBody("chat"))
			resp := ls.response(5 * time.Second)
			if resp == nil {
				t.Fatalf("stream %d: no response", n)
			}
			open = append(open, ls)
			if resp.StatusCode != http.StatusOK {
				t.Logf("stream %d: %d (budget in use %.1f MiB of %d MiB)", n+1, resp.StatusCode, float64(rig.budget.InUse())/(1<<20), budget>>20)
				break
			}
			if _, ok := nextData(each, 5*time.Second); !ok {
				t.Fatalf("stream %d: no frame", n)
			}
		}
		t.Logf("the default budget (%d MiB) holds %d concurrent streams (each reserves 2 × max_stream_frame_bytes + 3 × its body)", budget>>20, n)
	})

	t.Run("11 declared 8-MiB bodies that never come", func(t *testing.T) {
		hold := make(chan struct{})
		defer close(hold)
		u := newUpstream(t, "")
		u.setRespond(sseRespond([]sseStep{{data: sglangChunk("hello", "")}, {hold: hold}}, nil, nil))
		rig := newChatRig(t, []backends.Backend{u.row("spark", backends.TrustFull, 10)}, 129, 1, withStreamSettings, settings)
		const readTimeout = 2 * time.Second // ctxd's ReadTimeout is 30 s; scaled
		srv := httptest.NewUnstartedServer(loadConsumer(rig, rig.router))
		srv.Config.ReadTimeout = readTimeout
		srv.Start()
		t.Cleanup(srv.Close)
		var conns []net.Conn
		defer func() {
			for _, c := range conns {
				_ = c.Close()
			}
		}()
		start := time.Now()
		for i := 0; i < 11; i++ {
			c, err := net.Dial("tcp", srv.Listener.Addr().String())
			if err != nil {
				t.Fatal(err)
			}
			conns = append(conns, c)
			_, _ = fmt.Fprintf(c, "POST /v1/chat/completions HTTP/1.1\r\nHost: ctx\r\nContent-Type: application/json\r\nX-Load-Consumer: %d\r\nContent-Length: %d\r\n\r\n{\"model\":", i%8, 8<<20)
		}
		waitFor(2*time.Second, func() bool { return rig.budget.InUse() >= int64(10*3*(8<<20)) })
		held := rig.budget.InUse()
		// How many streams still fit while the declared bodies hold the budget.
		var open []*liveStream
		defer func() {
			for _, ls := range open {
				_ = ls.Close()
			}
		}()
		fit := 0
		for ; fit < 64; fit++ {
			ls, _ := openAndRead(t, context.Background(), srv, streamBody("chat"))
			open = append(open, ls)
			resp := ls.response(5 * time.Second)
			if resp == nil || resp.StatusCode != http.StatusOK {
				break
			}
		}
		for _, ls := range open {
			_ = ls.Close()
		}
		open = nil
		// The lock ends when the server's read timeout drops the bodies.
		freed := waitFor(10*time.Second, func() bool { return rig.budget.InUse() < 1<<20 })
		t.Logf("11 declared 8-MiB bodies: budget %.1f MiB of %d MiB held (10 × 3 × 8 MiB; the 11th refused at once); %d stream(s) still admitted during the lock; budget free again after %v (freed %v; server ReadTimeout %v — ctxd 30 s)",
			float64(held)/(1<<20), budget>>20, fit, time.Since(start), freed, readTimeout)
	})
}
