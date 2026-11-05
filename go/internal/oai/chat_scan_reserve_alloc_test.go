//go:build !race

package oai_test

// The relational pin of the scan's booking (FOLGE-P hardening H-8), outside
// -race builds: under the race detector sync.Pool drops a quarter of what is
// put back, so the scanner's pattern matchers are allocated anew on calls
// that reuse them in production — a tiny body then allocates up to 600 KB
// (measured), which says nothing about the production heap. Every plain
// `go test` runs it, the CI in its integration job (no -race).

import (
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// TestScanReserveIsTheMeasuredOne is the relational pin of the booking: at
// 256 B … 256 KiB every form allocates at most factor × body + base, and the
// largest per body byte at 256 KiB needs more than factor − 2 (the ratio
// grows with the body: assign_text 21.3 at 256 KiB, 22.2 at 1 MiB, 21.8 at
// 8 MiB — the larger bodies are the bau report's measurement).
func TestScanReserveIsTheMeasuredOne(t *testing.T) {
	var largest float64
	var largestForm string
	for _, size := range []int{256, 2 << 10, 64 << 10, 256 << 10} {
		names, forms := scanForms(size)
		for _, name := range names {
			body := forms[name]
			alloc, raised := scanAlloc(backends.SensPublic, body)
			if raised != backends.SensPublic {
				t.Fatalf("form %s: the scan raised to %s (a form without a credential)", name, raised)
			}
			want := oai.ScanReserveFactor*int64(len(body)) + oai.ScanReserveBase
			if alloc > want {
				t.Errorf("form %s, %d bytes: the scan allocates %d bytes, the route books %d (%d × body + %d)",
					name, len(body), alloc, want, oai.ScanReserveFactor, oai.ScanReserveBase)
			}
			if f := float64(alloc-oai.ScanReserveBase) / float64(len(body)); size == 256<<10 && f > largest {
				largest, largestForm = f, name
			}
		}
	}
	t.Logf("FOLGEP scan: largest %.2f bytes per body byte at 256 KiB (%s), factor %d, base %d", largest, largestForm, oai.ScanReserveFactor, oai.ScanReserveBase)
	if largest <= oai.ScanReserveFactor-2 {
		t.Errorf("the largest form needs %.2f bytes per body byte (%s), the factor %d books two more: measure again and lower it", largest, largestForm, oai.ScanReserveFactor)
	}
}
