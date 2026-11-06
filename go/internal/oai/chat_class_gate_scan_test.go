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
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/oai"
)

// cgScanSizes are the body sizes of the route gate (TestClassGateOverTheRoute):
// a deep body leaves the factor the least to book at the smallest, which
// -short runs alone (the scan of the 720 bodies at all three sizes takes
// about two minutes; a run without -short, as the CI's integration job,
// takes all three).
var cgScanSizes = []int{20100, 65600, 262144}

// TestClassGateBytesPerScan (hardening 7): every combination of the class
// gate at every route-gate size, scanned as the route scans it for an alias
// below credentials, allocates at most what the route books for that scan
// before it runs (oai.ScanBooking over the depth the route counts,
// oai.JSONDepth); at credentials the scan allocates nothing (the route books
// nothing there). No named exceptions.
func TestClassGateBytesPerScan(t *testing.T) {
	sizes := cgScanSizes
	if testing.Short() {
		sizes = sizes[:1]
	}
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
