package dispatch

// A03-W10 differential trace (gate (a), byte-identical default): 6 seeds × 300
// operations — TryAcquire of all four classes with weights, releases with a
// live or an ended run, double releases and policy swaps — on two
// dispatchers whose policies differ only in abort_settle_ms: missing, and
// 0/0. Every step's books must match, byte for byte; the trace's digest per
// seed goes to CTX_A03W10_TRACE_OUT when set, so the same file run against
// the base (the tree before the wave, which ignores the key) yields the
// digests the head must match (the third side of the comparison). The file
// names nothing the wave added — it compiles against the base.

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"os"
	"strings"
	"testing"
)

// traceRows: settleMs < 0 = no abort_settle_ms key, else {settleMs, settleMs}.
func traceRows(settleMs, slots, reserved int, preempt bool) []BackendRow {
	limits := map[string]any{"slots": float64(slots), "internal_reserved_slots": float64(reserved), "preempt_background": preempt}
	if settleMs >= 0 {
		limits["abort_settle_ms"] = map[string]any{"stream": float64(settleMs), "nonstream": float64(settleMs)}
	}
	return []BackendRow{{Name: "gpu", Scope: GlobalScope, BaseURL: testOrigin, ProviderClass: "llamacpp", Limits: limits}}
}

type settleTraceLease struct {
	l      *Lease
	cancel context.CancelFunc
}

// runSettleTrace runs one seed's trace and returns its step log.
func runSettleTrace(t *testing.T, seed uint64, settleMs int) []string {
	t.Helper()
	d := New(slog.New(slog.DiscardHandler), DefaultSettings())
	defer d.Close()
	shapes := [][3]int{{8, 3, 0}, {6, 2, 1}, {12, 4, 0}}
	pol := func(i int) Policy {
		s := shapes[i%len(shapes)]
		return DerivePolicy(traceRows(settleMs, s[0], s[1], s[2] == 1), slog.New(slog.DiscardHandler))
	}
	d.UpdatePolicy(pol(0))
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	var held []settleTraceLease
	steps := make([]string, 0, 300)
	for op := range 300 {
		var what string
		switch k := rng.IntN(10); {
		case k < 5: // acquire
			ctx, cancel := context.WithCancel(context.Background())
			var req Request
			switch rng.IntN(4) {
			case 0:
				ctx, req = withPrincipal(ctx, principal(fmt.Sprint(rng.IntN(3)))), interactiveReq()
			case 1:
				req = backgroundReq()
			case 2:
				ctx, req = xctx(ctx, fmt.Sprint("c", rng.IntN(3)), "t1"), externalReq()
				req.Weight = 1 + rng.IntN(2)
			default:
				ctx, req = xctx(ctx, fmt.Sprint("c", rng.IntN(3)), "t2"), Request{Target: Target{Origin: testOrigin}, Class: ClassExternalBulk, Role: "test"}
			}
			l, _, err := d.TryAcquire(ctx, req)
			if err != nil {
				cancel()
				what = fmt.Sprintf("acquire %s: %v", req.Class, err)
				break
			}
			held = append(held, settleTraceLease{l, cancel})
			what = fmt.Sprintf("acquire %s w%d: ok", req.Class, req.Weight)
		case k < 8 && len(held) > 0: // release, the run ended or alive
			i := rng.IntN(len(held))
			tl := held[i]
			held = append(held[:i], held[i+1:]...)
			ended := rng.IntN(2) == 0
			if ended {
				tl.cancel()
			}
			tl.l.Release()
			if rng.IntN(4) == 0 {
				tl.l.Release()
			}
			tl.cancel()
			what = fmt.Sprintf("release %s ended=%v", tl.l.Class(), ended)
		case k == 8:
			i := rng.IntN(len(shapes))
			d.UpdatePolicy(pol(i))
			what = fmt.Sprintf("policy %d", i)
		default:
			what = "noop"
		}
		ts := target(t, d)
		steps = append(steps, fmt.Sprintf("%03d %s | held=%d w=%d wx=%d hi=%d hx=%d in=%d ii=%d qi=%d qb=%d qx=%d qxb=%d",
			op, what, ts.Held, ts.HeldWeight, ts.HeldExternalWeight, ts.HeldInternal, ts.HeldExternal, ts.Inflight,
			ts.InflightInternal, ts.Interactive.Waiting, ts.Background.Waiting, ts.External.Waiting, ts.ExternalBulk.Waiting))
	}
	for _, tl := range held {
		tl.l.Release()
		tl.cancel()
	}
	return steps
}

func TestSettleZeroTraceIsByteIdentical(t *testing.T) {
	var out strings.Builder
	for seed := uint64(1); seed <= 6; seed++ {
		missing := runSettleTrace(t, seed, -1)
		zero := runSettleTrace(t, seed, 0)
		if len(missing) != 300 || len(zero) != 300 {
			t.Fatalf("seed %d: %d / %d steps, want 300", seed, len(missing), len(zero))
		}
		for i := range missing {
			if missing[i] != zero[i] {
				t.Fatalf("seed %d step %d differs:\n missing key: %s\n 0/0:         %s", seed, i, missing[i], zero[i])
			}
		}
		sum := sha256.Sum256([]byte(strings.Join(missing, "\n")))
		fmt.Fprintf(&out, "seed %d %x\n", seed, sum[:12])
	}
	if p := os.Getenv("CTX_A03W10_TRACE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Log("\n" + out.String())
}
