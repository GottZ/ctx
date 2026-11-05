package fedhop

// The hop token (design/04 §4.1 "Hop-Token (normativ)"): hex(nonce ‖ mac)
// with an 8-byte nonce from crypto/rand and mac = HMAC-SHA256(key, nonce)[:8],
// 32 lower-case hex characters. Only the minting instance recognises its
// token again (it alone holds the key); two requests of one instance carry
// unlinkable tokens (fresh nonce, PRF output); a token twice in one chain is
// two identical strings. A foreign nonce with a matching 64-bit MAC happens
// with probability 2⁻⁶⁴ per entry. The MAC proves nothing about foreign
// entries — the loop protection needs no signature over the chain, a forged
// entry only refuses or narrows its own request (design/04 §1.2).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
)

const (
	nonceLen = 8
	macLen   = 8
)

// MintToken returns a fresh hop token of this instance, "" for an identity
// without key (the zero Identity: Stamp refuses before it gets here).
func (i Identity) MintToken() string {
	key := i.secret()
	if len(key) == 0 {
		return ""
	}
	var raw [nonceLen + macLen]byte
	// Since Go 1.24 crypto/rand.Read never returns an error: it fills the
	// buffer or crashes the program irrecoverably.
	_, _ = rand.Read(raw[:nonceLen])
	copy(raw[nonceLen:], tokenMAC(key, raw[:nonceLen]))
	return hex.EncodeToString(raw[:])
}

// IsOwnToken reports whether tok is a hop token this instance minted: the
// grammar (32 lower-case hex digits — an upper-case spelling is no token),
// then the MAC of its nonce recomputed and compared in constant time.
func (i Identity) IsOwnToken(tok string) bool {
	key := i.secret()
	if len(key) == 0 || !validToken(tok) {
		return false
	}
	var raw [nonceLen + macLen]byte
	if _, err := hex.Decode(raw[:], []byte(tok)); err != nil {
		return false
	}
	return hmac.Equal(tokenMAC(key, raw[:nonceLen]), raw[nonceLen:])
}

// tokenMAC is HMAC-SHA256(key, nonce) truncated to macLen bytes.
func tokenMAC(key, nonce []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write(nonce) // a hash.Hash never returns an error
	return m.Sum(nil)[:macLen]
}

// secret returns the key, nil for the zero Identity.
func (i Identity) secret() []byte {
	if i.key == nil {
		return nil
	}
	return i.key()
}
