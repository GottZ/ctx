package oai_test

// Hardening 2 after Fremd-Review 3 (W02-09): the decode share before the
// acquire where it is certain (C-2, F3-2) and its growth with a dimension
// the first block taught (C-4, F3-8). The factors are the gate's own
// constants (the route's are unexported and a mutated one must not move the
// expectation with it).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// The factors of an embedding block's read cap and decode share (H-3).
const (
	h2FloatBytes    = 4        // one float32 component held for the answer
	h2EncodeBuffer  = 64 << 10 // the answer's encoder buffer
	h2WireComponent = 25       // the longest float64 literal and its separator
	h2WireItem      = 256      // the frame of one data entry
	h2WireFrame     = 4 << 10  // the frame of the answer
	h2DecodeFactor  = 3        // the decoder buffer's doubling
	h2TransportCap  = 16 << 20 // httpx.MaxResponseBytes
)

// h2ReadCap is the read cap of a block of inputs whose answer has floats
// components in all.
func h2ReadCap(inputs int, floats int64) int64 {
	return min(floats*h2WireComponent+int64(inputs)*h2WireItem+h2WireFrame, h2TransportCap)
}

// h2SlowWriter holds the first Write until release: a client that reads its
// answer slowly keeps the request's whole budget share.
type h2SlowWriter struct {
	h       http.Header
	status  int
	release chan struct{}
	once    sync.Once
	started chan struct{}
}

func (w *h2SlowWriter) Header() http.Header { return w.h }
func (w *h2SlowWriter) WriteHeader(c int)   { w.status = c }
func (w *h2SlowWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}
func (w *h2SlowWriter) Flush() {}

// TestEmbedDecodeRefusalBeforeTheQueue (C-2, Fremd-Review 3 F3-2): an alias
// without native_dims reads every block up to the floats ceiling, so its
// whole decode share is certain before the acquire. E = 1; request A holds
// the unit in its first block and then its budget share while it writes to
// a slow client; request B finds room for its body and answer but not for
// the decode share — 503 server_overloaded at once: no queue wait, no
// limiter, no acquire. Red (review probe): 503 after the queue wait (1.5 s).
func TestEmbedDecodeRefusalBeforeTheQueue(t *testing.T) {
	const hold = 2 * time.Second
	e := newEmbedEngine(t, 8)
	gate := make(chan struct{})
	var gateOnce sync.Once
	openGate := func() { gateOnce.Do(func() { close(gate) }) }
	defer openGate()
	e.setRespond(func(_ http.ResponseWriter, _ *http.Request, n int64, _ []string) bool {
		if n == 1 {
			<-gate
		}
		return false
	})
	rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1, withoutCensus)
	const maxFloats = 1 << 20 // newEmbedRig's gateway.embedding_max_floats
	body := embedBody("emb", embedInputs(2))
	share := 3*int64(len(body)) + maxFloats*h2FloatBytes + h2EncodeBuffer
	decode := h2DecodeFactor * h2ReadCap(2, maxFloats) // 3 × 16 MiB: the cap is clamped
	rig.setSettings(func(s *oai.Settings) { s.InflightBudget = 2*share + decode + 1<<20 })

	a := &h2SlowWriter{h: http.Header{}, release: make(chan struct{}), started: make(chan struct{})}
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		rig.router.ServeHTTP(a, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)))
	}()
	for e.hits.Load() == 0 {
		time.Sleep(time.Millisecond)
	}
	start := time.Now()
	var b chatAnswer
	var took time.Duration
	doneB := make(chan struct{})
	go func() { defer close(doneB); b = rig.postEmbed(t, body); took = time.Since(start) }()
	select {
	case <-doneB:
	case <-time.After(hold):
	}
	openGate() // A's block ends, its lease goes back; A then writes slowly and keeps its budget share
	<-a.started
	<-doneB
	admits := rig.limiter.admits.Load()
	close(a.release)
	<-doneA
	code, _, _ := b.envelope(t)
	t.Logf("B: %d %s after %v (A held the unit up to %v); limiter admits %d; wire hits %d", b.status, code, took.Round(time.Millisecond), hold, admits, e.hits.Load())
	if b.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
		t.Fatalf("B: %d %s, want 503 server_overloaded", b.status, b.body)
	}
	if took > 500*time.Millisecond || admits != 1 {
		t.Errorf("B refused after %v with %d limiter admission(s), want at once (≤ 500 ms) before the limiter and the queue (admits 1: A only)", took, admits)
	}
	if h := e.hits.Load(); h != 2 {
		t.Errorf("wire hits %d, want 2 (A's two blocks, none of B)", h)
	}
}

// TestEmbedDecodeShareBeforeAndAfterTheAcquire (C-2) pins both halves for a
// known native dimension (emb4k, 4096, sub_batch 32, E = 32, 32 inputs):
// the share of a one-input block is certain before the acquire — a budget
// one byte short of it is 503 server_overloaded with no limiter admission
// and no acquire; the rest of the 32-input block (its size is the row's
// capacity, known at the acquire) is booked after it — a budget that holds
// the one-input share but not the block is 503 after one acquire, with no
// wire contact (the named limit, docs/api.md: "after the queue wait").
func TestEmbedDecodeShareBeforeAndAfterTheAcquire(t *testing.T) {
	const dims, n = 4096, 32
	body := embedBody("emb4k", embedInputs(n))
	share := 3*int64(len(body)) + n*dims*h2FloatBytes + h2EncodeBuffer
	floor := h2DecodeFactor * h2ReadCap(1, dims)
	full := h2DecodeFactor * h2ReadCap(n, n*dims)
	for _, tc := range []struct {
		name     string
		budget   int64
		admits   int64
		acquires int
	}{
		{"one byte short of the one-input share", share + floor - 1, 0, 0},
		{"the one-input share, not the block", share + floor + (full-floor)/2, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, dims)
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, n+1, 1)
			rig.setSettings(func(s *oai.Settings) { s.InflightBudget = tc.budget })
			_ = rig.recorder.Take()
			a := rig.postEmbed(t, body)
			code, _, _ := a.envelope(t)
			acq := acquires(rig.recorder.Take())
			t.Logf("%s (budget %d B, one-input share %d B, block %d B): %d %s; limiter admits %d aborts %d; acquires %d; wire hits %d",
				tc.name, tc.budget, floor, full, a.status, code, rig.limiter.admits.Load(), rig.limiter.aborts.Load(), acq, e.hits.Load())
			if a.status != http.StatusServiceUnavailable || code != oai.CodeServerOverloaded {
				t.Fatalf("%d %.200s, want 503 server_overloaded", a.status, a.body)
			}
			if rig.limiter.admits.Load() != tc.admits || acq != tc.acquires || e.hits.Load() != 0 {
				t.Errorf("limiter admits %d, acquires %d, wire hits %d — want %d, %d, 0", rig.limiter.admits.Load(), acq, e.hits.Load(), tc.admits, tc.acquires)
			}
			if rig.budget.InUse() != 0 {
				t.Errorf("budget in use %d B after the answer, want 0", rig.budget.InUse())
			}
		})
	}
}

// TestEmbedLearnedDimensionGrowsTheDecodeShare (C-4, Fremd-Review 3 F3-8):
// an alias without native_dims under gateway.embedding_max_floats below
// 16 MiB / 25 B — the first block's read cap is the floats ceiling (here
// 100 000 floats ⇒ 2.5 MB), and a row that writes short literals ("1,", 2 B
// a component) teaches a dimension of 1.2 million in it. Every later block
// is read up to the cap of that dimension (16 MiB), so its decode share
// grows with it before the next block is asked for: ample budget ⇒ the
// budget holds 3 × 16 MiB while block two is in the engine and the request
// is served with a 14.4-MB block two; a budget that holds the learned
// answer but not the grown share ⇒ 503 server_overloaded after block one,
// block two is never sent.
func TestEmbedLearnedDimensionGrowsTheDecodeShare(t *testing.T) {
	skipUnderRace(t, "the decode share grown to a learned dimension of 1.2 million components (two blocks read, decoded and encoded up to 16 MiB)")
	const (
		maxFloats = 100_000
		learned   = 1_200_000
		n         = 2
	)
	short := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` + strings.TrimSuffix(strings.Repeat("1,", learned), ",") + `]}],"usage":{"prompt_tokens":1}}`
	long := `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[` + strings.TrimSuffix(strings.Repeat("0.123456789,", learned), ",") + `]}],"usage":{"prompt_tokens":1}}`
	body := embedBody("emb", embedInputs(n))
	learnedAnswer := int64(n*learned*h2FloatBytes + h2EncodeBuffer)
	first := h2DecodeFactor * h2ReadCap(n, maxFloats) // the floats ceiling, booked before the acquire
	grown := h2DecodeFactor * h2ReadCap(1, learned)   // a one-input block at the learned dimension
	if int64(len(short)) > h2ReadCap(1, maxFloats) || int64(len(long)) > h2ReadCap(1, learned) || int64(len(long)) <= first/h2DecodeFactor {
		t.Fatalf("fixture: short %d B (cap %d), long %d B (cap %d, first share's cap %d)", len(short), h2ReadCap(1, maxFloats), len(long), h2ReadCap(1, learned), first/h2DecodeFactor)
	}
	for _, tc := range []struct {
		name   string
		budget int64
		status int
		hits   int64
	}{
		{"ample budget", 256 << 20, http.StatusOK, 2},
		{"the learned answer, not the grown share", 3*int64(len(body)) + learnedAnswer + first + 1<<20, http.StatusServiceUnavailable, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := newEmbedEngine(t, 8)
			var atBlockTwo int64
			rig := newEmbedRig(t, []backends.Backend{e.row("spark-embed", backends.ProtocolOpenAI, 10)}, 2, 1)
			e.setRespond(func(w http.ResponseWriter, _ *http.Request, call int64, _ []string) bool {
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					_, _ = w.Write([]byte(short))
					return true
				}
				atBlockTwo = rig.budget.InUse()
				_, _ = w.Write([]byte(long))
				return true
			})
			rig.setSettings(func(s *oai.Settings) { s.EmbeddingMaxFloats = maxFloats; s.InflightBudget = tc.budget })
			a := &discardWriter{h: http.Header{}}
			rig.router.ServeHTTP(a, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)))
			t.Logf("%s: %d, %d B answer; wire hits %d; budget at block two %d B (body %d + learned answer %d + grown decode share %d = %d); block two %d B",
				tc.name, a.status, a.n, e.hits.Load(), atBlockTwo, 3*len(body), learnedAnswer, grown, 3*int64(len(body))+learnedAnswer+grown, len(long))
			if a.status != tc.status || e.hits.Load() != tc.hits {
				t.Fatalf("%d after %d wire hit(s), want %d after %d", a.status, e.hits.Load(), tc.status, tc.hits)
			}
			if tc.hits == 2 && atBlockTwo < 3*int64(len(body))+learnedAnswer+grown {
				t.Errorf("the budget holds %d B while block two (read up to %d B) is in the engine, want ≥ %d B: its decode share at the learned dimension is reserved",
					atBlockTwo, h2ReadCap(1, learned), 3*int64(len(body))+learnedAnswer+grown)
			}
			if rig.budget.InUse() != 0 {
				t.Errorf("budget in use %d B after the answer, want 0", rig.budget.InUse())
			}
		})
	}
}
