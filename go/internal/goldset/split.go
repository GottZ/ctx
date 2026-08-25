package goldset

import (
	"math/rand/v2"
	"sort"
	"strings"
)

// Split partitions keys 50/50 into DERIV and HOLD from a seed alone
// (design 04 §4.6): variants are derived on DERIV, the win gate is evaluated on
// HOLD. Determinism is a gate of this wave — the input order must not matter,
// so keys are sorted before the seeded shuffle.
//
// An odd count puts the extra case in DERIV; HOLD, the half that decides, is
// never the one padded by a rounding rule.
func Split(keys []string, seed int64) map[string]string {
	sorted := append([]string(nil), keys...)
	sort.Strings(sorted)

	//nolint:gosec // deterministic reproducibility is the requirement here, not unpredictability
	r := rand.New(rand.NewPCG(uint64(seed), 0x676f6c64)) // "gold"
	for i := len(sorted) - 1; i > 0; i-- {
		j := r.IntN(i + 1)
		sorted[i], sorted[j] = sorted[j], sorted[i]
	}

	out := make(map[string]string, len(sorted))
	half := (len(sorted) + 1) / 2
	for i, k := range sorted {
		if i < half {
			out[k] = SplitDeriv
		} else {
			out[k] = SplitHold
		}
	}
	return out
}

// SplitFingerprint digests a partition so the stamp can pin it and a later run
// can prove it reproduced the same halves.
func SplitFingerprint(m map[string]string) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(m[k])
		b.WriteByte('\n')
	}
	return SHA256Hex(b.String())
}

// SplitCounts returns the DERIV and HOLD sizes.
func SplitCounts(m map[string]string) (deriv, hold int) {
	for _, v := range m {
		if v == SplitDeriv {
			deriv++
		} else {
			hold++
		}
	}
	return deriv, hold
}
