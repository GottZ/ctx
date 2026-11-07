package oai_test

// The race guard of W02-09's allocation and size pins (hardening 7): the
// CI unit job runs `go test -short -race -timeout=5m` on two cores, and
// the embeddings route's heap, answer-size and booking pins over 8-MiB
// bodies and 16-MiB answers took 362 s of internal/oai's 629 s there
// (GOMAXPROCS=2, run 42336599705 timed the package out at 5 minutes).
// Under the race detector those pins measure its instrumentation — every
// memory access and allocation is shadowed, sync.Pool drops items at
// random, the parse and the decode run 5–8 × slower — not the reserve
// they bound. A pin that measures only that is skipped under -race
// (skipUnderRace). Every pin runs
// in full in each build without -race: the plain `go test` and the CI's
// integration job (TestEmbedPinsWithoutRaceRunInFull pins it).

import (
	"runtime/debug"
	"testing"
)

// skipUnderRace skips an allocation or size pin under the race detector:
// measures says what the pin bounds, which the detector's instrumentation
// would weigh instead of the route.
func skipUnderRace(t *testing.T, measures string) {
	t.Helper()
	if raceEnabled {
		t.Skipf("race detector: this pin measures %s — the detector's instrumentation multiplies the time and the allocations of exactly that (shadowed memory accesses, sync.Pool dropping items at random): under -race it would measure the detector and spend the CI unit job's package timeout; it runs in full in every build without -race (the plain go test, the CI integration job)", measures)
	}
}

// TestEmbedPinsWithoutRaceRunInFull pins the race guard: raceEnabled tells
// the truth (the build's -race setting), skipUnderRace skips only in a
// race build — a guard active in every build would leave the pins behind
// it pinned nowhere.
func TestEmbedPinsWithoutRaceRunInFull(t *testing.T) {
	race := false
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "-race" {
				race = s.Value == "true"
			}
		}
	}
	if race != raceEnabled {
		t.Fatalf("raceEnabled is %v, the build's -race setting says %v", raceEnabled, race)
	}
	ran := false
	t.Run("a pin behind skipUnderRace", func(t *testing.T) {
		skipUnderRace(t, "nothing (the guard's own probe)")
		ran = true
	})
	if ran == raceEnabled {
		t.Errorf("race build %v: a pin behind skipUnderRace ran %v, want %v", raceEnabled, ran, !raceEnabled)
	}
}
