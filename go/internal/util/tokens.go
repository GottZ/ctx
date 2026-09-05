// tokens.go — the scoring tokeniser: a string to its set of lower-cased
// [a-z0-9äöüß] runs.
//
// It came from internal/evalscore unchanged (T01-4). It lives here because it
// is not an eval primitive: derived.Adequacy computes claim novelty with it and
// the distill novelty floor gates live writes on it, so the eval harnesses and
// the production path have to share ONE tokeniser — two would give the gate and
// the instrument two different orderings of the same claims. util is the only
// package in this tree that imports nothing but the standard library, which is
// what lets the production path take it without dragging tooling along.
//
// Source: https://github.com/GottZ/ctx
package util

import (
	"regexp"
	"strings"
)

// titleTokenRe zerlegt Titel in Score-Tokens: lowercase [a-z0-9äöüß]+.
var titleTokenRe = regexp.MustCompile(`[a-z0-9äöüß]+`)

// TokenSet liefert die Token-Menge eines Strings (lowercase, [a-z0-9äöüß]+).
func TokenSet(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range titleTokenRe.FindAllString(strings.ToLower(s), -1) {
		out[t] = true
	}
	return out
}
