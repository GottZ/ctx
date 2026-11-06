//go:build !race

package oai

// The CPU gate of the embeddings parse (W02-09 rebase round 2, part A),
// outside -race and coverage builds like FOLGE-P's gate of the chat parse
// (parse_cpu_gate_test.go, the reasons there): since the port the embeddings
// parse classifies, repeats and orders its keys with FOLGE-P's levelKeys,
// keyIndex and cmpKeys, and inherits their cost — Delta-Review 3 R3-0 (a
// prefix of invalid UTF-8 two keys share, quadratic under FOLGE-P's
// hardening 2) would have reached this parse through the same cmpKeys
// (FOLGE-P Fremd-Review 3 §10). Every form stays within its bound against the
// parse before the port, and the prefix forms grow linearly with the body.
// A form over a bound is measured again like the chat parse's (gateForm,
// cpuPasses; FOLGE-P Delta-Review 4 R4-1): the gate fails only when every
// pass exceeds, each repeat is logged, the bounds stay.

import (
	"fmt"
	"runtime"
	"testing"
	"time"
)

func TestEmbedParseCPUAgainstTheBase(t *testing.T) {
	skipUnderCoverage(t)
	for _, f := range embedCPUForms(256 << 10) {
		if d := embedOutcomeDiff(f.body); d != "" {
			t.Fatalf("form %s: the parses answer differently: %s", f.name, d)
		}
		gateForm(t, f.name, func(pass int) []string {
			ratio, mn, mb := embedCPURatio(f.body, 7)
			t.Logf("W0209 cpu form=%s bytes=%d answer=%s new=%v base=%v ratio=%.2f pass=%d", f.name, len(f.body), embedOutcomeOf(parseEmbedTop(f.body, nil)), mn, mb, ratio, pass)
			if ratio > f.bound() {
				return []string{fmt.Sprintf("the parse takes %v, %.2f × the base parse's %v (at most %g ×)", mn, ratio, mb, f.bound())}
			}
			return nil
		})
	}
}

func TestEmbedParseCPUIsLinearInASharedPrefix(t *testing.T) {
	skipUnderCoverage(t)
	small, large := embedPrefixForms(cpuPrefixBytes), embedPrefixForms(4*cpuPrefixBytes)
	for i, s := range small {
		l := large[i]
		if d := embedOutcomeDiff(l.body); d != "" {
			t.Fatalf("form %s: the parses answer differently: %s", l.name, d)
		}
		gateForm(t, l.name, func(pass int) []string {
			minS, minL, minB := time.Duration(1<<62), time.Duration(1<<62), time.Duration(1<<62)
			for range 7 {
				runtime.GC()
				t0 := time.Now()
				_, _ = parseEmbedTop(s.body, nil)
				minS = min(minS, time.Since(t0))
				t0 = time.Now()
				_, _ = parseEmbedTop(l.body, nil)
				minL = min(minL, time.Since(t0))
				t0 = time.Now()
				_, _ = baseEmbedTop(l.body)
				minB = min(minB, time.Since(t0))
			}
			growth, ratio := float64(minL)/float64(max(minS, 1)), float64(minL)/float64(max(minB, 1))
			t.Logf("W0209 cpu prefix form=%s bytes=%d/%d answer=%s new=%v/%v growth=%.2f base=%v ratio=%.2f pass=%d", l.name, len(s.body), len(l.body), embedOutcomeOf(parseEmbedTop(l.body, nil)), minS, minL, growth, minB, ratio, pass)
			var over []string
			if growth > cpuGrowthMax {
				over = append(over, fmt.Sprintf("the parse takes %v at %d B and %v at %d B, %.2f × for 4 × the body (at most %d ×: the cost is not linear)", minS, len(s.body), minL, len(l.body), growth, cpuGrowthMax))
			}
			if ratio > l.bound() {
				over = append(over, fmt.Sprintf("the parse takes %v, %.2f × the base parse's %v (at most %g ×)", minL, ratio, minB, l.bound()))
			}
			return over
		})
	}
}
