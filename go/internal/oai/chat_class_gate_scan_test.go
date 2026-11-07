//go:build !race

package oai_test

// The scan side of the class gate (FOLGE-P hardening 7, stop finding S-H6-1
// of hardening 6): with an alias below credentials the route runs the
// sensitivity scan over the body after the parse and books it before
// (coverScan). The gate's route runs with the public alias found deep bodies
// whose scan held up to 0.8 MB more than that booking; the gate now checks
// the scan of every combination as part of the suite, for both kinds of
// alias. Outside -race builds, as the scan's other pins
// (chat_scan_reserve_alloc_test.go): under the race detector sync.Pool drops
// a quarter of what is put back and the scanner's matchers are allocated
// anew, which says nothing about the production heap.

import (
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/oai"
)

// cgScanSizes are the body sizes of the route gate (TestClassGateOverTheRoute):
// a deep body leaves the factor the least to book at the smallest, which
// -short runs alone (the scan of the 720 bodies at all three sizes takes
// about two minutes; a run without -short, as the CI's integration job,
// takes all three).
var cgScanSizes = []int{20100, 65600, 262144}

// cgScanBoundaryShapes are the bodies whose scan comes closest to its
// booking (Delta-Review 6 M-1, hardening 8): one string value of invalid
// UTF-8 whose length crosses 64 KiB or 128 KiB — in a message's content, in
// a member of metadata (a pass key), in the drop key user and in the ctx
// key n (the class gate's ctxKeys.n:ff at 65 600 B, which the review saw
// over the old booking in 2 of 2 full runs). Measured per byte of the
// value's length within ±320 B of both lengths and per KiB to ±8 KiB (bau
// report FOLGE-P §Härtung 8): at 64 KiB a spike at 65 534 B, at 128 KiB
// every length from 1 B under the boundary to about 1 KiB past it over
// 23 × the body + 64 KiB + the depth term — 6–14 KB in most scan calls,
// 6–36 KB more in others, at most 50 229 B in 4 000 calls at the largest
// points; 1 KiB before and 2 KiB past either length the scan is under it.
var cgScanBoundaryShapes = []struct{ name, pre, suf string }{
	{"content", `{"model":"pub","messages":[{"role":"user","content":"`, `"}]}`},
	{"metadata.x", `{"model":"pub","messages":[{"role":"user","content":"x"}],"metadata":{"x":"`, `"}}`},
	{"user", `{"model":"pub","messages":[{"role":"user","content":"x"}],"user":"`, `"}`},
	{"n", `{"model":"` + oai.CGModel + `","messages":[{"role":"user","content":"x"}],"n":"`, `"}`},
}

// cgScanBoundaryLengths are the boundary lengths, cgScanBoundaryOffsets the
// value lengths the gate scans around each: either side of it, the spike at
// 64 KiB (−2: 65 534 B in every shape, the n body of 65 600 B the review
// found), the largest measured at 128 KiB (+4: the content body of
// 131 133 B, up to 50 229 B, the most of 4 000 calls; +31: the user body of
// 131 171 B, Delta-Review 6 +43 451 B, here up to 49 275 B; +209: the n
// body of 131 347 B, up to 45 451 B) and 1 KiB past it.
var (
	cgScanBoundaryLengths = []int{64 << 10, 128 << 10}
	cgScanBoundaryOffsets = []int{-32, -2, -1, 0, 1, 4, 31, 32, 209, 1024}
)

// cgScanBoundaryCount pins the boundary part's size: 4 shapes × 2 lengths ×
// 10 value lengths.
const cgScanBoundaryCount = 80

// cgScanBoundaryBodies are the boundary part's bodies by name.
func cgScanBoundaryBodies() ([]string, map[string][]byte) {
	var names []string
	bodies := map[string][]byte{}
	for _, sh := range cgScanBoundaryShapes {
		for _, l := range cgScanBoundaryLengths {
			for _, off := range cgScanBoundaryOffsets {
				name := fmt.Sprintf("%s:ff@%d%+d", sh.name, l, off)
				names = append(names, name)
				bodies[name] = []byte(sh.pre + strings.Repeat("\xff", l+off) + sh.suf)
			}
		}
	}
	return names, bodies
}

// scanAllocMost is the larger TotalAlloc of two ScanRaise calls over body
// (cold pattern pools, as scanAlloc): the boundary part reads the larger of
// two calls, not the smaller — what lies past the boundary is not noise
// but a call's allocation, and the base is sized for the largest call.
func scanAllocMost(body []byte) int64 {
	var most int64
	for range 2 {
		runtime.GC()
		runtime.GC()
		var a, b runtime.MemStats
		runtime.ReadMemStats(&a)
		federation.ScanRaise(backends.SensPublic, body)
		runtime.ReadMemStats(&b)
		most = max(most, int64(b.TotalAlloc-a.TotalAlloc))
	}
	return most
}

// TestClassGateBytesPerScan (hardening 7): every combination of the class
// gate at every route-gate size, scanned as the route scans it for an alias
// below credentials, allocates at most what the route books for that scan
// before it runs (oai.ScanBooking over the depth the route counts,
// oai.JSONDepth); at credentials the scan allocates nothing (the route books
// nothing there). No named exceptions. Hardening 8 (Delta-Review 6 M-1):
// the boundary bodies (cgScanBoundaryShapes) are a fixed part of every run,
// -short included, each read as the larger of two scan calls.
func TestClassGateBytesPerScan(t *testing.T) {
	sizes := cgScanSizes
	if testing.Short() {
		sizes = sizes[:1]
	}
	names, bodies := cgScanBoundaryBodies()
	if len(names) != cgScanBoundaryCount || !slices.Contains(cgScanBoundaryOffsets, 4) || !slices.Contains(cgScanBoundaryOffsets, 31) || !slices.Contains(cgScanBoundaryOffsets, 209) ||
		!slices.Contains(cgScanBoundaryOffsets, -2) || !slices.Equal(cgScanBoundaryLengths, []int{64 << 10, 128 << 10}) {
		t.Fatalf("the boundary part scans %d bodies at lengths %v + %v, pinned %d with the measured largest (+4, +31, +209) and the spike (−2)",
			len(names), cgScanBoundaryLengths, cgScanBoundaryOffsets, cgScanBoundaryCount)
	}
	worst, worstAt, scannedBoundaries := int64(-1<<62), "", 0
	for _, name := range names {
		body := bodies[name]
		depth := oai.JSONDepth(body)
		alloc := scanAllocMost(body)
		scannedBoundaries++
		booked := oai.ScanBooking(body, depth)
		if d := alloc - booked; d > worst {
			worst, worstAt = d, name
		}
		if alloc > booked {
			t.Errorf("boundary %s (body %d, %d deep), public alias: the scan allocates %d bytes, the route books %d",
				name, len(body), depth, alloc, booked)
		}
	}
	if scannedBoundaries != cgScanBoundaryCount {
		t.Errorf("the boundary part scanned %d bodies, pinned %d", scannedBoundaries, cgScanBoundaryCount)
	}
	t.Logf("FOLGEP class gate scan boundaries: bodies=%d closest=%s alloc-booked=%d", scannedBoundaries, worstAt, worst)
	red, raisedTo := 0, 0
	var scanned int64
	for _, combo := range oai.ClassGateCombos() {
		for _, size := range sizes {
			body, ok := oai.ClassGateBody(combo, size)
			if !ok {
				t.Fatalf("no class-gate body for %s", combo)
			}
			// A body the scanner takes for a credential (a long run of one
			// letter, say) ends the scan at the hit; the booking holds either way.
			alloc, raised := scanAlloc(backends.SensPublic, body)
			if raised != backends.SensPublic {
				raisedTo++
			}
			scanned += alloc
			if booked := oai.ScanBooking(body, oai.JSONDepth(body)); alloc > booked {
				red++
				t.Errorf("%s at %d B (body %d, %d deep), public alias: the scan allocates %d bytes, the route books %d",
					combo, size, len(body), oai.JSONDepth(body), alloc, booked)
			}
			if size != sizes[0] {
				continue
			}
			if alloc, raised := scanAlloc(backends.SensCredentials, body); alloc != 0 || raised != backends.SensCredentials {
				t.Errorf("%s at %d B, credentials alias: the scan allocates %d bytes and answers %s, want 0 and credentials", combo, size, alloc, raised)
			}
		}
	}
	if scanned == 0 {
		t.Error("the scans below credentials allocated nothing: the gate did not scan")
	}
	t.Logf("FOLGEP class gate scan: combos=%d sizes=%v red=%d raised=%d", len(oai.ClassGateCombos()), sizes, red, raisedTo)
}
