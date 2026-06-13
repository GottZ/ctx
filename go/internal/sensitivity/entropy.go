// Package sensitivity — deterministic credentials detector (G40, F3-P8).
//
// A pattern/entropy scanner that only ever UPGRADES a block/query to
// sensitivity='credentials' (never downgrades). It is the deterministic VETO
// against the G41 LLM audit: a pattern hit stamps sensitivity_source='pattern',
// which the audit's pick set (source='default' only) can never re-touch.
//
// Precision over recall by design: a false positive permanently blocks the
// external-failover net for an operation touching that block, so the rule set
// favours structured, high-precision signals (AWS keys, PEM private keys, JWTs,
// known token prefixes, entropy-gated secret assignments) over generic
// hex/base64 blobs that collide with this corpus's git SHAs and content hashes.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx
package sensitivity

import "math"

// shannonEntropy returns the Shannon entropy of s in bits per character (0 for
// the empty string). Uniform random base64 approaches ~6.0, hex ~4.0, natural
// language ~3.5-4.2 — the threshold that separates a high-entropy secret blob
// from prose sits at ~4.5 (design 03 §2.3c).
func shannonEntropy(s string) float64 {
	if s == "" {
		return 0
	}
	var freq [256]int
	n := 0
	for i := 0; i < len(s); i++ {
		freq[s[i]]++
		n++
	}
	entropy := 0.0
	for _, c := range freq {
		if c == 0 {
			continue
		}
		p := float64(c) / float64(n)
		entropy -= p * math.Log2(p)
	}
	return entropy
}
