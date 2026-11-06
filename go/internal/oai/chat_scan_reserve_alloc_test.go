//go:build !race

package oai_test

// The relational pins of the scan's booking (FOLGE-P hardening H-8,
// hardening 2 after Delta-Review 2 F-1), outside -race builds: under the race
// detector sync.Pool drops a quarter of what is put back, so the scanner's
// pattern matchers are allocated anew on calls that reuse them in production
// — a tiny body then allocates up to 600 KB (measured), which says nothing
// about the production heap. Every plain `go test` runs them, the CI in its
// integration job (no -race).

import (
	"encoding/json"
	"fmt"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/oai"
)

// scanPeakForms are the forms the scan reads once that cost it the most per
// body byte, and scanPeakBytes the size where their ratio peaks (h2 probe,
// TotalAlloc − base per body byte: assign_semi/assign_text 21.36 at 256 KiB,
// 22.21 at 1 MiB, 22.23 at 2 MiB, 22.06 at 4 MiB).
var scanPeakForms = []string{"assign_semi", "assign_text", "assign_upper", "tc_hugekey_null"}

const scanPeakBytes = 2 << 20

// TestScanReserveIsTheMeasuredOne is the relational pin of the booking for
// the forms the scan reads once: at 256 B … 256 KiB every form allocates at
// most factor × body + base, and so do the most expensive ones at 2 MiB,
// where their ratio peaks; the largest of those needs more than factor − 1
// per body byte. So the pin tells 22 from 23 (the booking would be untrue)
// and 24 from 23 (it would cost requests for nothing).
func TestScanReserveIsTheMeasuredOne(t *testing.T) {
	check := func(name string, body []byte) float64 {
		alloc, raised := scanAlloc(backends.SensPublic, body)
		if raised != backends.SensPublic {
			t.Fatalf("form %s: the scan raised to %s (a form without a credential)", name, raised)
		}
		if want := oai.ScanReserveFactor*int64(len(body)) + oai.ScanReserveBase; alloc > want {
			t.Errorf("form %s, %d bytes: the scan allocates %d bytes, the route books %d (%d × body + %d)",
				name, len(body), alloc, want, oai.ScanReserveFactor, oai.ScanReserveBase)
		}
		return float64(alloc-oai.ScanReserveBase) / float64(len(body))
	}
	for _, size := range []int{256, 2 << 10, 64 << 10, 256 << 10} {
		names, forms := scanForms(size)
		for _, name := range names {
			check(name, forms[name])
		}
	}
	var largest float64
	var largestForm string
	_, forms := scanForms(scanPeakBytes)
	for _, name := range scanPeakForms {
		if f := check(name, forms[name]); f > largest {
			largest, largestForm = f, name
		}
	}
	t.Logf("FOLGEP scan: largest %.3f bytes per body byte at %d B (%s), factor %d, base %d", largest, scanPeakBytes, largestForm, oai.ScanReserveFactor, oai.ScanReserveBase)
	if largest <= oai.ScanReserveFactor-1 {
		t.Errorf("the largest form needs %.3f bytes per body byte at %d B (%s), the factor %d books a whole byte more: measure again and lower it",
			largest, scanPeakBytes, largestForm, oai.ScanReserveFactor)
	}
}

// scanDepthForms are the deepest bodies the route accepts per kind of
// nesting (hardening 7, S-H6-1): a bare array, objects with a one-byte key
// and both alternating, nested at a top-level key, in a message member and
// in a tool call — at the decoder's limit and at 9 216 levels, where the
// scan's stacks have just grown and its allocation per level peaks (every
// third depth measured from 300 to 10 000, bau report FOLGE-P §Härtung 7).
// The bracket bytes are the fewest a level can take, so these forms leave
// the factor the least to book.
func scanDepthForms() ([]string, map[string][]byte) {
	wraps := map[string]func(v string) string{
		"top": func(v string) string {
			return `{"model":"pub","messages":[{"role":"user","content":"x"}],"bootstrap_host":` + v + `}`
		},
		"msg": func(v string) string { return `{"model":"pub","messages":[{"role":"user","content":"x","reasoning":` + v + `}]}` },
		"tc": func(v string) string {
			return `{"model":"pub","messages":[{"role":"user","content":"x"},{"role":"assistant","content":null,"tool_calls":[` + v + `]}]}`
		},
	}
	nests := map[string]func(d int) string{
		"arr": func(d int) string { return strings.Repeat("[", d) + strings.Repeat("]", d) },
		"obj": func(d int) string { return strings.Repeat(`{"a":`, d) + "0" + strings.Repeat("}", d) },
		"mixed": func(d int) string {
			return strings.Repeat(`[{"a":`, d/2) + "0" + strings.Repeat("}]", d/2)
		},
	}
	f := map[string][]byte{}
	for wn, w := range wraps {
		for nn, n := range nests {
			for _, d := range []int{9216, 9990} {
				f[fmt.Sprintf("%s_%s_%d", wn, nn, d)] = []byte(w(n(d)))
			}
		}
	}
	names := make([]string, 0, len(f))
	for k := range f {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, f
}

// TestScanDepthReserveIsTheMeasuredOne is the relational pin of the scan's
// booking per nesting level (hardening 7, S-H6-1): every deepest form
// allocates at most what coverScan books for it (oai.ScanBooking: factor ×
// body + base + ScanDepthStateBytes × depth), and the largest needs more
// than ScanDepthStateBytes − 8 per level beyond factor × body + base — so the
// pin tells 96 from 104 (the booking would be untrue) and 112 from 104 (it
// would cost deep requests for nothing). The floor's term
// (ScanDepthReserve) is the most the booking takes for the depth: the
// depth's count saturates one past the decoder's limit.
func TestScanDepthReserveIsTheMeasuredOne(t *testing.T) {
	var largest float64
	var largestForm string
	names, forms := scanDepthForms()
	for _, name := range names {
		body := forms[name]
		if !json.Valid(body) {
			t.Fatalf("form %s is not valid JSON (the scan runs after the parse)", name)
		}
		depth := oai.JSONDepth(body)
		alloc, raised := scanAlloc(backends.SensPublic, body)
		if raised != backends.SensPublic {
			t.Fatalf("form %s: the scan raised to %s (a form without a credential)", name, raised)
		}
		if want := oai.ScanBooking(body, depth); alloc > want {
			t.Errorf("form %s, %d bytes, %d deep: the scan allocates %d bytes, the route books %d (%d × body + %d + %d × depth)",
				name, len(body), depth, alloc, want, oai.ScanReserveFactor, oai.ScanReserveBase, oai.ScanDepthStateBytes)
		}
		if f := float64(alloc-oai.ScanReserveFactor*int64(len(body))-oai.ScanReserveBase) / float64(depth); f > largest {
			largest, largestForm = f, name
		}
	}
	t.Logf("FOLGEP scan depth: largest %.1f bytes per level beyond %d × body + %d (%s), booked %d", largest, oai.ScanReserveFactor, oai.ScanReserveBase, largestForm, oai.ScanDepthStateBytes)
	if largest <= oai.ScanDepthStateBytes-8 {
		t.Errorf("the largest form needs %.1f bytes per level (%s), the booking of %d per level is 8 more: measure again and lower it",
			largest, largestForm, oai.ScanDepthStateBytes)
	}
	nest := []byte(strings.Repeat("[", 20000) + strings.Repeat("]", 20000))
	if got := oai.ScanBooking(nil, oai.JSONDepth(nest)) - oai.ScanReserveBase; got != oai.ScanDepthReserve {
		t.Errorf("the scan's booking takes at most %d bytes for the depth, ScanDepthReserve is %d", got, oai.ScanDepthReserve)
	}
}

// TestScanTwiceHoldsOneScanAtOnce pins the one form whose scan allocates more
// than one booking in total (Delta-Review 2 F-1): one key of the whole body
// with a scalar value is scanned twice in a row — the key as a token, then
// the probe key + ": " + value. The two scans allocate 40.4 (256 KiB) …
// 42.4 (1 MiB, 2 MiB) bytes per body byte together, but the first one's
// garbage is dead before the second begins: what the scan holds at once — the
// live heap the collector marks, read at every cycle under GOGC=1 — is one
// scan's, as for the same key scanned once (tc_hugekey_null; 9.1–12.5 against
// 9.5–11.6 bytes per body byte in 30 runs — a cycle that catches the
// scanner's result slice while append copies it reads the upper mode). The
// booking, factor × body + base, covers one scan call with its garbage
// (TestScanReserveIsTheMeasuredOne); here: at most 2 × factor × body + base
// allocated (a third scan of the body fails it), at most factor × body +
// base held at once (the live heap no collector can bring under the
// booking), and at most scanHeldPerByteMax × body + base held at once, by
// the form scanned once as by the two scanned twice (Delta-Review 3 §3: a
// scanner whose matches of both calls live until the end of the body scan
// holds 16.0 or 19.4, under the factor — only this bound tells one scan
// held at once from two).
func TestScanTwiceHoldsOneScanAtOnce(t *testing.T) {
	names, forms := scanTwiceForms(1 << 20)
	for _, name := range names {
		body := forms[name]
		alloc, raised := scanAlloc(backends.SensPublic, body)
		if raised != backends.SensPublic {
			t.Fatalf("form %s: the scan raised to %s (a form without a credential)", name, raised)
		}
		t.Logf("FOLGEP scan twice: %s %d B allocates %.3f bytes per body byte", name, len(body), float64(alloc-oai.ScanReserveBase)/float64(len(body)))
		if want := 2*oai.ScanReserveFactor*int64(len(body)) + oai.ScanReserveBase; alloc > want {
			t.Errorf("form %s, %d bytes: the scan allocates %d bytes, more than two scans of the body (%d = 2 × %d × body + %d)",
				name, len(body), alloc, want, oai.ScanReserveFactor, oai.ScanReserveBase)
		}
	}

	const size = 256 << 10
	_, once := scanForms(size)
	single := once["tc_hugekey_null"]
	names, forms = scanTwiceForms(size)
	held := scanHeldAtOnce(single)
	t.Logf("FOLGEP scan once: tc_hugekey_null %d B holds %.2f bytes per body byte at once", len(single), float64(held)/float64(len(single)))
	if want := scanHeldPerByteMax*int64(len(single)) + oai.ScanReserveBase; held > want {
		t.Errorf("form tc_hugekey_null, %d bytes: one scan holds %d bytes at once, more than %d × body + %d (measured 9.5–11.6 × body: measure again)",
			len(single), held, scanHeldPerByteMax, oai.ScanReserveBase)
	}
	for _, name := range names {
		body := forms[name]
		got := scanHeldAtOnce(body)
		t.Logf("FOLGEP scan twice: %s %d B holds %.2f bytes per body byte at once", name, len(body), float64(got)/float64(len(body)))
		if want := oai.ScanReserveFactor*int64(len(body)) + oai.ScanReserveBase; got > want {
			t.Errorf("form %s, %d bytes: the scan holds %d bytes at once, the route books %d (%d × body + %d)",
				name, len(body), got, want, oai.ScanReserveFactor, oai.ScanReserveBase)
		}
		if want := scanHeldPerByteMax*int64(len(body)) + oai.ScanReserveBase; got > want {
			t.Errorf("form %s, %d bytes: the two scans hold %d bytes at once, more than one scan's %d × body + %d — both calls' heap lives at once",
				name, len(body), got, scanHeldPerByteMax, oai.ScanReserveBase)
		}
	}
}

// scanHeldPerByteMax bounds what a scan of the body holds at once per body
// byte: the hold of one scan call plus a margin, not the factor (which binds
// the call's allocation with its garbage). Measured (bau report FOLGE-P
// §Härtung 2, §Härtung 3; Delta-Review 3 §3): one scan 9.5–11.6, the form
// scanned twice 8.9–12.6 (200 runs; two modes, the upper one a cycle that
// catches the scanner's result slice while append copies it); both calls'
// matches alive at once 16.0 or 19.4 (the same two modes). 14 lies between:
// 1.4 above the largest hold measured, 2.0 under the mutant's lower mode.
const scanHeldPerByteMax = 14

// scanHeldAtOnce is the largest live heap the collector marks while
// ScanRaise runs over body, above the live heap before it (the largest of
// three runs): the collector runs at GOGC=1, and a finalizer re-armed at
// every cycle reads /gc/heap/live:bytes, the heap the last cycle marked. A
// cycle missed by the finalizer can only lower the reading.
func scanHeldAtOnce(body []byte) int64 {
	defer debug.SetGCPercent(debug.SetGCPercent(1))
	var most int64
	for range 3 {
		runtime.GC()
		runtime.GC()
		base := liveHeap()
		var peak atomic.Int64
		var stop atomic.Bool
		var arm func()
		arm = func() {
			s := &scanSentinel{}
			runtime.SetFinalizer(s, func(*scanSentinel) {
				memMaxInt(&peak, liveHeap())
				if !stop.Load() {
					arm()
				}
			})
		}
		arm()
		federation.ScanRaise(backends.SensPublic, body)
		stop.Store(true)
		most = max(most, peak.Load()-base)
	}
	return most
}

// scanSentinel carries a pointer so the allocator never folds it into a tiny
// block (a finalizer there could wait for its neighbours).
type scanSentinel struct {
	_ *int
	_ [8]int
}

// liveHeap is the heap the last collection marked.
func liveHeap() int64 {
	s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}
