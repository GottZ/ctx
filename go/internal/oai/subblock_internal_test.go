package oai

// Internal pins of the sub-block walk and the streamed answer (W02-09):
// every block's usage reaches ITS lease while the lease is still held (Z3,
// the per-lease half — the dispatcher keeps an external lease's usage
// unexported, so the report hook is the observation point), the weight plan
// checks, and the encoder never holds the whole answer (G9).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"runtime"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
)

// blockAcquirer leases every block from a real dispatcher (pass-through
// policy, background class: the lease mechanics without a principal).
type blockAcquirer struct {
	d      *dispatch.Dispatcher
	blocks []int
	held   func() int
}

func (a *blockAcquirer) Class() dispatch.Class { return dispatch.ClassBackground }
func (a *blockAcquirer) Blocks() []int         { return a.blocks }
func (a *blockAcquirer) acquire(ctx context.Context, b *backends.Backend) (*dispatch.Lease, context.Context, error) {
	return a.d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: b.Host}, Class: dispatch.ClassBackground, Role: "embed"})
}
func (a *blockAcquirer) First(ctx context.Context, b *backends.Backend, _ string, _ time.Duration) (*dispatch.Lease, context.Context, error) {
	return a.acquire(ctx, b)
}
func (a *blockAcquirer) Next(ctx context.Context, b *backends.Backend, _ string, _ time.Duration) (*dispatch.Lease, context.Context, error) {
	return a.acquire(ctx, b)
}
func (a *blockAcquirer) Continue(ctx context.Context, b *backends.Backend, _ string, _ time.Duration) (*dispatch.Lease, context.Context, error) {
	if a.held() != 0 {
		return nil, nil, errors.New("continuation while a block lease is held")
	}
	return a.acquire(ctx, b)
}

var _ RequestAcquirer = (*blockAcquirer)(nil)

// TestSubBlockUsagePerLease (Z3): blocks [3, 3, 2]: each block's usage is
// reported to its own lease, while that lease is held (a report after the
// Release would be dropped), and every lease is released before the next
// block asks (rule 7). Red: the usage of block one only.
func TestSubBlockUsagePerLease(t *testing.T) {
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	const host = "http://subblock.invalid:9000"
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{host: {Slots: 4}}})
	held := func() int {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == host {
				return ts.Held
			}
		}
		return 0
	}
	acq := &blockAcquirer{d: d, blocks: []int{3, 3, 2}, held: held}
	type report struct {
		lease *dispatch.Lease
		usage dispatch.Usage
		held  int
	}
	var reports []report
	run := subBlockRun{
		ctx: context.Background(), acq: acq, role: "embed",
		timeout: func(*backends.Backend) time.Duration { return time.Second },
		alive:   func() error { return nil },
		report: func(l *dispatch.Lease, u dispatch.Usage) {
			reports = append(reports, report{l, u, held()})
		},
	}
	b := &backends.Backend{Name: "row", Host: host}
	var spans [][2]int
	var leases []*dispatch.Lease
	walk := func(runCtx context.Context, lease *dispatch.Lease) (dispatch.Usage, bool, error) {
		return run.run(runCtx, b, lease, time.Second, acq.blocks, func(i, lo, hi int, l *dispatch.Lease, _ context.Context, _ time.Duration) (dispatch.Usage, error) {
			spans = append(spans, [2]int{lo, hi})
			leases = append(leases, l)
			return dispatch.Usage{PromptTokens: 10 * (i + 1)}, nil
		})
	}
	first, runCtx, err := acq.First(context.Background(), b, "embed", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	u, committed, err := walk(runCtx, first)
	first.ReportUsage(u) // the walker's report after the attempt: a no-op once run released it
	first.Release()
	if err != nil || !committed {
		t.Fatalf("run: committed=%v err=%v", committed, err)
	}
	if u.PromptTokens != 10 {
		t.Errorf("usage returned for the walker's lease %d, want block one's 10", u.PromptTokens)
	}
	if want := [][2]int{{0, 3}, {3, 6}, {6, 8}}; len(spans) != 3 || spans[0] != want[0] || spans[1] != want[1] || spans[2] != want[2] {
		t.Fatalf("spans %v, want %v", spans, want)
	}
	if leases[0] != first || leases[1] == first || leases[2] == leases[1] {
		t.Fatalf("block leases not one per block: %p %p %p (first %p)", leases[0], leases[1], leases[2], first)
	}
	if len(reports) != 3 {
		t.Fatalf("%d usage reports, want one per block lease", len(reports))
	}
	sum := 0
	for i, r := range reports {
		if r.lease != leases[i] || r.usage.PromptTokens != 10*(i+1) || r.held != 1 {
			t.Errorf("report %d: lease %p (block lease %p) usage %d held %d — want the block's own lease, %d tokens, reported while held",
				i, r.lease, leases[i], r.usage.PromptTokens, r.held, 10*(i+1))
		}
		sum += r.usage.PromptTokens
	}
	if sum != 60 {
		t.Errorf("Σ reported %d, want 60", sum)
	}
	if h := held(); h != 0 {
		t.Errorf("held after the walk %d, want 0", h)
	}
}

// TestSubBlockContinuationFailureInTheHull: a continuation error comes back
// inside the acquire hull (terminal for the walker, the writer's domain);
// a lease in another class than the plan's is handed back, ErrWalkClass in
// the hull.
func TestSubBlockContinuationFailureInTheHull(t *testing.T) {
	b := &backends.Backend{Name: "row", Host: "http://x.invalid"}
	run := subBlockRun{ctx: context.Background(), acq: failingContinue{err: dispatch.ErrQueueFull}, role: "embed",
		timeout: func(*backends.Backend) time.Duration { return time.Second }, alive: func() error { return nil }}
	l, _, err := run.continueBlock(b, time.Second)
	if l != nil || !llm.IsAdmissionError(err) || !errors.Is(err, dispatch.ErrQueueFull) {
		t.Fatalf("lease %v err %v, want no lease and ErrQueueFull in the acquire hull", l, err)
	}
	run.acq = failingContinue{}
	if _, _, err := run.continueBlock(b, time.Second); !llm.IsAdmissionError(err) || !errors.Is(err, errNoLease) {
		t.Fatalf("no lease and no error: %v, want errNoLease in the hull", err)
	}
}

type failingContinue struct {
	RequestAcquirer
	err error
}

func (failingContinue) Class() dispatch.Class { return dispatch.ClassExternal }
func (f failingContinue) Continue(context.Context, *backends.Backend, string, time.Duration) (*dispatch.Lease, context.Context, error) {
	return nil, nil, f.err
}

// TestCheckBlocks: a weight plan the request cannot walk is refused.
func TestCheckBlocks(t *testing.T) {
	for _, c := range []struct {
		blocks []int
		units  int
		ok     bool
	}{
		{[]int{1}, 1, true}, {[]int{4, 4, 2}, 10, true}, {nil, 1, false}, {[]int{4, 0}, 4, false}, {[]int{4, 4}, 9, false},
		// a plan covering more units than the request (S03): refused, never
		// a slice past the inputs
		{[]int{5, 5}, 9, false}, {[]int{4, 4, 2}, 9, false}, {[]int{2}, 1, false},
	} {
		if err := checkBlocks(c.blocks, c.units); (err == nil) != c.ok {
			t.Errorf("checkBlocks(%v, %d) = %v, want ok=%v", c.blocks, c.units, err, c.ok)
		}
	}
	if continuationAllowed([]int{2, 2}, false) || !continuationAllowed([]int{4}, false) || !continuationAllowed([]int{2, 2}, true) {
		t.Error("continuationAllowed: the k = 1 rule")
	}
}

// TestStreamedEncodingNeverHoldsTheAnswer (G9): an answer of 32 × 4096
// floats is encoded item by item — the bytes the encoder allocates per
// answer stay below a quarter of the answer and within the encoder buffer
// plus 4 KiB per item (the whole JSON never exists in memory), and the
// stream is one valid JSON object. Red: building the
// whole answer with json.Marshal before the write.
func TestStreamedEncodingNeverHoldsTheAnswer(t *testing.T) {
	vecs := make([][]float32, 32)
	for i := range vecs {
		vecs[i] = make([]float32, 4096)
		for j := range vecs[i] {
			vecs[i][j] = float32(math.Sin(float64(i*4096+j))) * 0.123456789
		}
	}
	for _, b64 := range []bool{false, true} {
		a := embedAnswer{vecs: vecs, dims: 4096, base64: b64, model: "emb", promptTokens: 64}
		var size countWriter
		if err := writeEmbeddings(&size, a); err != nil {
			t.Fatal(err)
		}
		var ms0, ms1 runtime.MemStats
		const runs = 5
		runtime.GC()
		runtime.ReadMemStats(&ms0)
		for range runs {
			if err := writeEmbeddings(io.Discard, a); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&ms1)
		perRun := (ms1.TotalAlloc - ms0.TotalAlloc) / runs
		t.Logf("base64=%v: answer %d bytes, allocated %d bytes per answer (%.1f %%)", b64, size.n, perRun, 100*float64(perRun)/float64(size.n))
		// The whole JSON would be ≥ 100 % of the answer; the stream holds the
		// encoder buffer and one item's encoding at a time.
		if bound := uint64(EmbedEncodeBuffer + len(vecs)*4<<10); perRun*4 > uint64(size.n) || perRun > bound {
			t.Errorf("base64=%v: %d bytes allocated per answer of %d — want < a quarter of it and ≤ the buffer + 4 KiB per item (%d)",
				b64, perRun, size.n, bound)
		}
		var buf bytes.Buffer
		if err := writeEmbeddings(&buf, a); err != nil {
			t.Fatal(err)
		}
		var out struct {
			Object string `json:"object"`
			Data   []struct {
				Index int `json:"index"`
			} `json:"data"`
			Model string `json:"model"`
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(buf.Bytes(), &out); err != nil || out.Object != "list" || len(out.Data) != 32 || out.Model != "emb" ||
			out.Usage.PromptTokens != 64 || out.Usage.TotalTokens != 64 {
			t.Fatalf("base64=%v: the stream is no valid answer: %v (%d items)", b64, err, len(out.Data))
		}
	}
}

type countWriter struct{ n int }

func (c *countWriter) Write(p []byte) (int, error) { c.n += len(p); return len(p), nil }

// TestFloat32FormIsEncodingJSONs (F-1 of Fremd-Review 1): the hand-encoded
// float32 is byte for byte what encoding/json writes for it — over the
// boundaries of its two formats (1e-6, 1e21), subnormals, the extremes, ±0
// and 200 000 random bit patterns; a non-finite value is refused.
func TestFloat32FormIsEncodingJSONs(t *testing.T) {
	vals := []float32{0, float32(math.Copysign(0, -1)), 1, -1, 0.1, 1e-6, 9.999999e-7, 1.0000001e-6, 1e21, 9.999999e20, 1e-7, 1e-45,
		math.SmallestNonzeroFloat32, math.MaxFloat32, -math.MaxFloat32, 3.4028235e38, 1.17549435e-38, 123456.789, 2.9209967}
	seed := uint32(0x9e3779b9)
	for range 200000 {
		seed ^= seed << 13
		seed ^= seed >> 17
		seed ^= seed << 5
		f := math.Float32frombits(seed)
		if !math.IsNaN(float64(f)) && !math.IsInf(float64(f), 0) {
			vals = append(vals, f)
		}
	}
	for _, x := range vals {
		want, err := json.Marshal(x)
		if err != nil {
			t.Fatal(err)
		}
		got, err := appendFloat32(nil, x)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("%v (bits %08x): %q, encoding/json %q (err %v)", x, math.Float32bits(x), got, want, err)
		}
	}
	for _, x := range []float32{float32(math.Inf(1)), float32(math.Inf(-1)), float32(math.NaN())} {
		if _, err := appendFloat32(nil, x); err == nil {
			t.Errorf("%v accepted", x)
		}
	}
}

// breachAcquirer answers Continue with a real lease of the dispatcher and a
// breach of the door's contract: a lease of another class than the plan's
// (S01), or no run context (S04).
type breachAcquirer struct {
	RequestAcquirer
	d       *dispatch.Dispatcher
	class   dispatch.Class
	noRun   bool
	granted *dispatch.Lease
}

func (a *breachAcquirer) Class() dispatch.Class { return dispatch.ClassExternal }
func (a *breachAcquirer) Continue(ctx context.Context, b *backends.Backend, _ string, _ time.Duration) (*dispatch.Lease, context.Context, error) {
	l, rc, err := a.d.Acquire(ctx, dispatch.Request{Target: dispatch.Target{Origin: b.Host}, Class: a.class, Role: "embed"})
	a.granted = l
	if a.noRun {
		rc = nil
	}
	return l, rc, err
}

// TestSubBlockContinuationBreachesGiveTheLeaseBack (hardening H-5, S01,
// S04, S05): a continuation lease in another class than the plan's, and one
// without a run context, are refused inside the acquire hull (ErrWalkClass
// resp. errNoLease) — and the lease goes back: the target holds nothing
// afterwards (a kept lease would hold its units for good).
func TestSubBlockContinuationBreachesGiveTheLeaseBack(t *testing.T) {
	d := dispatch.New(slog.New(slog.DiscardHandler), dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	const host = "http://breach.invalid:9000"
	d.UpdatePolicy(dispatch.Policy{Targets: map[string]dispatch.TargetPolicy{host: {Slots: 4}}})
	held := func() int {
		for _, ts := range d.Snapshot().Targets {
			if ts.Origin == host {
				return ts.Held
			}
		}
		return 0
	}
	b := &backends.Backend{Name: "row", Host: host}
	for _, tc := range []struct {
		name  string
		acq   *breachAcquirer
		cause error
	}{
		{"lease in another class (S01)", &breachAcquirer{d: d, class: dispatch.ClassBackground}, llm.ErrWalkClass},
		{"no run context (S04)", &breachAcquirer{d: d, class: dispatch.ClassBackground, noRun: true}, errNoLease},
	} {
		run := subBlockRun{ctx: context.Background(), acq: tc.acq, role: "embed",
			timeout: func(*backends.Backend) time.Duration { return time.Second }, alive: func() error { return nil }}
		l, rc, err := run.continueBlock(b, time.Second)
		if l != nil || rc != nil || !llm.IsAdmissionError(err) || !errors.Is(err, tc.cause) {
			t.Errorf("%s: lease %v err %v, want no lease and %v in the acquire hull", tc.name, l, err, tc.cause)
		}
		if tc.acq.granted == nil {
			t.Fatalf("%s: the fake granted no lease", tc.name)
		}
		if h := held(); h != 0 {
			t.Errorf("%s: the target holds %d lease(s) after the refusal (S05: the breach must give the lease back)", tc.name, h)
		}
	}
}
