package overview

import "testing"

func TestLockKeyForScopes(t *testing.T) {
	t.Run("empty and nil return the base key byte-identically", func(t *testing.T) {
		// Rollback/mixed-deploy invariant (B1-M1): the global run's key must
		// stay EXACTLY the pre-B-W4 constant so old binaries keep excluding
		// the new ones. 0x6f76727677 is pinned here on purpose — mutating the
		// base constant turns this red.
		if got := lockKeyForScopes(nil); got != 0x6f76727677 {
			t.Fatalf("nil filter: lock key = %#x, want the pre-B-W4 base key 0x6f76727677", got)
		}
		if got := lockKeyForScopes([]string{}); got != 0x6f76727677 {
			t.Fatalf("empty filter: lock key = %#x, want the pre-B-W4 base key 0x6f76727677", got)
		}
	})

	t.Run("order and duplicates never change the key", func(t *testing.T) {
		a := lockKeyForScopes([]string{"private", "shared", "work"})
		b := lockKeyForScopes([]string{"work", "private", "shared"})
		c := lockKeyForScopes([]string{"work", "private", "shared", "private", "work"})
		if a != b || a != c {
			t.Fatalf("scope-set key not order/dup-stable: %#x / %#x / %#x", a, b, c)
		}
	})

	t.Run("distinct sets get distinct keys and never the base key", func(t *testing.T) {
		keys := map[int64]string{overviewLockKey: "base"}
		for _, set := range [][]string{
			{"private", "shared", "work"},
			{"tenant-b"},
			{"tenant-b", "tenant-b-shared"},
			{"private"},
		} {
			k := lockKeyForScopes(set)
			if prev, clash := keys[k]; clash {
				t.Fatalf("lock key collision between %v and %s: %#x", set, prev, k)
			}
			keys[k] = set[0]
		}
	})

	t.Run("framing distinguishes concatenation-ambiguous sets", func(t *testing.T) {
		// Without the 0x00 separator {"ab","c"} and {"a","bc"} would hash the
		// same bytes.
		if lockKeyForScopes([]string{"ab", "c"}) == lockKeyForScopes([]string{"a", "bc"}) {
			t.Fatal("lock key framing broken: concatenation-ambiguous sets collide")
		}
	})
}

// TestLockKeyGoldenValue pins the key for a fixed sample set. Process
// stability proof: `go test -count=1` in two SEPARATE processes must print
// the identical value (a per-process-seeded hash cannot pass twice).
func TestLockKeyGoldenValue(t *testing.T) {
	// Pinned 2026-07-05 (B-W4) from the first FNV-64a run. A seedless hash
	// reproduces it in every process and on every platform — hash/maphash
	// (per-process seed, B1-M1) could not pass this twice. Recompute
	// deliberately if the framing ever changes; never adjust to green a red.
	got := lockKeyForScopes([]string{"private", "shared", "work"})
	const want int64 = -4335225535149169795
	if got != want {
		t.Fatalf("golden lock key drifted: got %d, want %d — process-stable hashing broken (maphash? framing change?)", got, want)
	}
}
