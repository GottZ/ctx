package fedhop

// Review-2 gates of F-W3 (reports/review/F-W3-fremdreview-2.md): the token
// MAC is pinned to HMAC-SHA256 by a known answer (FR2-2, mutant N04), and the
// zero Tier is the loop tier — a target whose tier was never set still sends
// exactly one own Ctx-Hop (FR2-1, mutant N14).

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"strings"
	"testing"
)

// FR2-2: the normative contract is hex(nonce ‖ HMAC-SHA256(key, nonce)[:8]).
// Every minted token verifies against the reference formula, and a token
// built from it by hand is own — another hash or truncation would not be.
func TestTokenKnownAnswerHMACSHA256(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	for range 1000 {
		tk := self.MintToken()
		raw, err := hex.DecodeString(tk)
		if err != nil || len(raw) != nonceLen+macLen {
			t.Fatalf("token %q: %v", tk, err)
		}
		m := hmac.New(sha256.New, testSecret)
		_, _ = m.Write(raw[:nonceLen])
		if want := m.Sum(nil)[:macLen]; !hmac.Equal(want, raw[nonceLen:]) {
			t.Fatalf("token %q: MAC %x, HMAC-SHA256(key, nonce)[:8] = %x", tk, raw[nonceLen:], want)
		}
	}
	nonce := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	m := hmac.New(sha256.New, testSecret)
	_, _ = m.Write(nonce)
	ref := hex.EncodeToString(append(nonce, m.Sum(nil)[:macLen]...))
	if !self.IsOwnToken(ref) {
		t.Errorf("reference token %s (HMAC-SHA256 by hand) is not recognised as own", ref)
	}
}

// FR2-1: the zero value of Tier is TierLoop (design/04 would read TierNone =
// iota — the deviation of this wave, fail-closed): a Target{} and an invalid
// tier value stamp exactly one own Ctx-Hop and nothing else.
func TestZeroTierStampsLoopMarker(t *testing.T) {
	self := NewIdentity(testID, testSecret)
	for _, tg := range []Target{{}, {Tier: Tier(7)}} {
		h := http.Header{}
		Stamp(selfCtx(), h, tg)
		ctxKeys := 0
		for k := range h {
			if strings.HasPrefix(strings.ToLower(k), "ctx-") || strings.HasPrefix(strings.ToLower(k), "x-ctx-") {
				ctxKeys++
			}
		}
		if !self.IsOwnToken(h.Get(HeaderHop)) || ctxKeys != 1 {
			t.Errorf("tier %d: headers %v, want exactly one own Ctx-Hop (the zero tier is the loop tier)", tg.Tier, h)
		}
	}
}
