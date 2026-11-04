package federation

// F-W9 bot reviews on PR #126: the depth bound of the body walk (CodeRabbit)
// and the untruncated assignment probe (Copilot).
//
//	go test -count=1 -run 'TestScanRaiseProbe|TestScanRaiseDepth' -v ./internal/federation/

import (
	"bytes"
	"runtime"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/sensitivity"
)

// TestScanRaiseProbeFullToken (Copilot, PR #126): the scanner's assignment
// rule measures entropy over the WHOLE captured token; a probe cut to a
// prefix can fall below the threshold where the whole token passes. The
// fixture is the reviewer's: 256 × "z" (entropy 0) then 32 × a 16-letter run
// (whole token ≈ 3.58 ≥ 3.0) — the isolated leaf also misses the generic
// rules (not hex, base64 entropy below its threshold), so only the
// key-value probe can raise. A second long scalar: 600 × "z" then a
// 2 000-byte run over 20 non-hex letters (whole token ≈ 4.1 bits, below the
// base64 rule's 4.5; a hex-free prefix, so a cut probe cannot turn into a
// hex blob either).
func TestScanRaiseProbeFullToken(t *testing.T) {
	copilot := strings.Repeat("z", 256) + strings.Repeat("ghijklmnopqrstuv", 32)
	long := strings.Repeat("z", 600) + strings.Repeat("ghijkmnopqrstuwxyGHJ", 100)
	for name, v := range map[string]string{"copilot fixture": copilot, "second long scalar": long} {
		t.Run(name, func(t *testing.T) {
			if _, hit := sensitivity.Scan(v); hit {
				t.Fatalf("precondition: the value alone hits the scanner — the fixture does not isolate the probe")
			}
			if _, hit := sensitivity.Scan("api_key: " + v); !hit {
				t.Fatalf("precondition: the scanner does not take the whole token as an assignment")
			}
			body := []byte(`{"model":"m","metadata":{"api_key":"` + v + `"}}`)
			if got := ScanRaise(backends.SensPersonal, body); got != backends.SensCredentials {
				t.Errorf("api_key with a %d-byte token: %q, want credentials (the probe must carry the whole token)", len(v), got)
			}
		})
	}
}

// TestScanRaiseDepthCap (CodeRabbit, PR #126): json.Decoder.Token does not
// bound the nesting, so a body of 8 MiB of "[" — inside
// gateway.max_request_bytes, from any consumer below credentials — would grow
// the walk's stack and the decoder's to ≈ 8 M entries. Beyond maxScanDepth
// open containers the walk stops and the raw scan decides, as for a body that
// does not decode. Two pins:
//   - result: a secret hidden by a JSON escape below 20 000 "[" is what the
//     RAW scan says (it misses the escape) — an unbounded walk would decode it
//     and differ;
//   - memory: 8 MiB of "[" allocate at most scanDepthAllocBound in total
//     (runtime.MemStats.TotalAlloc before/after), measured and logged.
func TestScanRaiseDepthCap(t *testing.T) {
	const depth = 20000
	escaped := strings.Repeat("[", depth) + `"\u0041` + awsKey()[1:] + `"` + strings.Repeat("]", depth)
	if !strings.Contains(escaped, `\u0041`) {
		t.Fatal("fixture carries no JSON escape")
	}
	_, raw := sensitivity.Scan(escaped)
	want := backends.SensPersonal
	if raw {
		want = backends.SensCredentials
	}
	if got := ScanRaise(backends.SensPersonal, []byte(escaped)); got != want {
		t.Errorf("%d nested arrays: ScanRaise = %q, want %q (beyond the depth cap the raw scan decides)", depth, got, want)
	}

	const scanDepthAllocBound = 64 << 20
	brackets := []byte(strings.Repeat("[", 8<<20))
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	got := ScanRaise(backends.SensPersonal, brackets)
	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc
	t.Logf("8 MiB of \"[\": ScanRaise = %s, TotalAlloc +%d bytes (%.1f MiB), bound %d MiB", got, alloc, float64(alloc)/(1<<20), scanDepthAllocBound>>20)
	if alloc > scanDepthAllocBound {
		t.Errorf("8 MiB of \"[\" allocated %.1f MiB, want ≤ %d MiB — the walk is not bounded in depth", float64(alloc)/(1<<20), scanDepthAllocBound>>20)
	}
}

// TestScanBodyDepthBound is the toolchain-independent gate of the bound:
// at a small bound the walk hands over to the raw scan as soon as a
// container would exceed it — here a JSON-escaped key (the raw scan misses
// the escape, the walk would decode it) three levels deep. Without the bound
// the walk decodes it and answers true.
func TestScanBodyDepthBound(t *testing.T) {
	body := []byte(`[[["\u0041` + awsKey()[1:] + `"]]]`)
	if !bytes.Contains(body, []byte(`\u0041`)) {
		t.Fatal("fixture carries no JSON escape")
	}
	if !scanBody(body, maxScanDepth) {
		t.Fatal("within the bound: the walk must decode the escaped key")
	}
	if scanBody(body, 2) {
		t.Error("bound 2, depth 3: the raw scan decides and misses the escape — got a hit, the bound did not hand over")
	}
}
