// Package modelkey is the credential format of the model API (/v1): the
// "ctxm_" key an external consumer presents as a Bearer token. It is a
// stdlib-only leaf (depguard rule modelkey-leaf): the store that mints keys,
// the knowledge paths that refuse them and the /v1 middleware that checks
// them all import this package, so it imports none of them.
//
// A key is 54 bytes without separators — the prefix "ctxm_", 43 Base62
// characters of body and 6 Base62 characters of checksum:
//
//	ctxm_0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefg0KG44D
//
// The body carries 256 bits from crypto/rand, drawn character by character
// with rejection sampling so every digit is uniform. The checksum is
// CRC32-IEEE over prefix and body, written as six big-endian Base62 digits
// padded with '0'. It turns typos and garbage away before any database
// round-trip; it is NOT a forgery barrier — anyone can compute it, so a
// well-formed key proves nothing about its validity (design/01 §5 B18).
//
// Only the SHA-256 of a key is stored (Hash). Plaintext is the one method that
// returns the secret; fmt, slog and encoding/json show the Hint — the public
// checksum tail — or an address or {}, never the body. Reflection-based
// dumpers (test-diff libraries, go-spew, reflect itself) follow the pointer
// inside a Key and do print it, so a key is never handed to them. Compare
// keys by Hash (== on a Key compares pointers), and do not embed a Key in a
// struct: its String and LogValue would take over the outer type's output.
package modelkey

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"strconv"
	"strings"
)

const (
	// Prefix starts every model key. It extends the family of the opaque
	// OAuth tokens (store.AccessTokenPrefix "ctxt_", RefreshTokenPrefix
	// "ctxr_"), which knowledge keys never carry — so the credential class
	// is decidable from the first five bytes. "_" is not a Base62 digit.
	Prefix = "ctxm_"

	// bodyLen Base62 digits carry 43 · log2(62) ≈ 256.03 bits — the smallest
	// length that reaches 256.
	bodyLen = 43

	// sumLen Base62 digits hold every CRC32 value: 62^6 ≈ 5.7·10^10 > 2^32.
	sumLen = 6

	// Len is the exact length of a model key in bytes: 54.
	Len = len(Prefix) + bodyLen + sumLen

	// alphabet is Base62 in the order [0-9A-Za-z]; a digit's value is its index.
	alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

	// rejectFrom is the first random byte generate discards. 248 = 4 · 62 is
	// the largest multiple of 62 below 256, so b % 62 over [0, 248) hits each
	// digit exactly four times; keeping 248…255 would draw the digits 0…7 with
	// probability 5/256 instead of 4/256.
	rejectFrom = 256 - 256%len(alphabet)

	// hintTail is how many trailing characters a Hint shows. All of them lie
	// in the checksum: a hint discloses about 24 bits of a CRC32 of the key,
	// no body character.
	hintTail = 4
)

// ErrShape is the one error Parse returns, whatever is wrong with the input —
// length, prefix, a character outside Base62, a checksum mismatch. A distinct
// error per cause would tell a prober which part of a guess it got right.
var ErrShape = errors.New("modelkey: malformed key")

// Key is a well-formed model key. The zero Key is no key: Plaintext, Hash and
// Hint return "" and HashPrefix the zero array, so a zero Key that slips
// through fails closed at the key_hash CHECK and at every lookup.
//
// The plaintext sits behind a pointer on purpose. fmt formats a value that it
// reaches through an unexported struct field — struct{ key modelkey.Key } —
// by reflection, without calling any of its methods, and the same happens for
// a verb a Stringer does not serve (%d, %t): a string field would print
// verbatim there, a pointer prints as an address.
type Key struct {
	raw *string
}

func newKey(raw string) Key { return Key{raw: &raw} }

// Generate draws a fresh key from crypto/rand. Since Go 1.24 crypto/rand does
// not fail (it aborts the process instead), so the error is nil in practice;
// it stays in the signature because the draw runs through an io.Reader.
func Generate() (Key, error) {
	return generate(rand.Reader)
}

// generate fills the body by rejection sampling: a byte below rejectFrom
// becomes alphabet[b % 62], any other byte is discarded and replaced by the
// next one. Each round reads exactly as many bytes as digits are missing, so
// a fixed reader yields a fixed key — the seam the sampling gate tests.
func generate(r io.Reader) (Key, error) {
	var raw [Len]byte
	copy(raw[:], Prefix)
	body := raw[len(Prefix) : len(Prefix)+bodyLen]
	var buf [bodyLen]byte
	for n := 0; n < bodyLen; {
		chunk := buf[:bodyLen-n]
		if _, err := io.ReadFull(r, chunk); err != nil {
			return Key{}, fmt.Errorf("modelkey: read random bytes: %w", err)
		}
		for _, b := range chunk {
			if int(b) >= rejectFrom {
				continue
			}
			body[n] = alphabet[int(b)%len(alphabet)]
			n++
		}
	}
	sum := checksum(raw[:len(Prefix)+bodyLen])
	copy(raw[len(Prefix)+bodyLen:], sum[:])
	return newKey(string(raw[:])), nil
}

// Parse accepts exactly the canonical form: Len bytes, Prefix, Base62 after
// it, and a checksum equal — as a string — to the one recomputed from prefix
// and body. Comparing the spelling instead of a decoded number leaves no
// second spelling of the same checksum (CRC+2^32 fits in six digits too).
// Nothing is trimmed or case-folded; the caller passes the token as sent.
//
// The checks return early and the comparison is not constant-time: each one
// looks only at the input and at a public function of it, so their timing
// tells a prober nothing it could not compute itself.
func Parse(raw string) (Key, error) {
	if len(raw) != Len || raw[:len(Prefix)] != Prefix {
		return Key{}, ErrShape
	}
	for i := len(Prefix); i < Len; i++ {
		if !isBase62(raw[i]) {
			return Key{}, ErrShape
		}
	}
	sum := checksum([]byte(raw[:len(Prefix)+bodyLen]))
	if string(sum[:]) != raw[len(Prefix)+bodyLen:] {
		return Key{}, ErrShape
	}
	return newKey(raw), nil
}

// HasPrefix reports whether raw claims to be a model key: the prefix after
// trimming surrounding whitespace, nothing more. It is the knowledge paths'
// switch (design/01 §4.3) — a credential of this class is refused there
// without a database round-trip, well-formed or not.
func HasPrefix(raw string) bool {
	return strings.HasPrefix(strings.TrimSpace(raw), Prefix)
}

// checksum is CRC32-IEEE of b as sumLen big-endian Base62 digits; leading
// zeros stay as '0', so the width is fixed.
func checksum(b []byte) [sumLen]byte {
	v := crc32.ChecksumIEEE(b)
	var out [sumLen]byte
	for i := sumLen - 1; i >= 0; i-- {
		out[i] = alphabet[v%uint32(len(alphabet))]
		v /= uint32(len(alphabet))
	}
	return out
}

func isBase62(c byte) bool {
	return '0' <= c && c <= '9' || 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z'
}

// Plaintext is the key itself — the one way to the secret. Its callers are
// the reveal-once answers of the admin API; whatever only names a key uses
// Hint, whatever stores or looks one up uses Hash.
func (k Key) Plaintext() string {
	if k.raw == nil {
		return ""
	}
	return *k.raw
}

// Hash is the stored identifier: lowercase hex of SHA-256 over the whole key,
// prefix and checksum included — the procedure knowledge keys use
// (store/api_keys.go), 64 characters for the key_hash CHECK
// '^[0-9a-f]{64}$'. With 256 random bits a fast hash is the right one; NIST
// SP 800-63B asks for password hashing only below 112 bits.
func (k Key) Hash() string {
	if k.raw == nil {
		return ""
	}
	sum := sha256.Sum256([]byte(*k.raw))
	return hex.EncodeToString(sum[:])
}

// HashPrefix is the first 8 bytes of that SHA-256: the per-key half of the
// /v1 failure limiter's key (design/01 §4.2 stage 5). It is a prefix of the
// stored hash, not a secret.
func (k Key) HashPrefix() [8]byte {
	var p [8]byte
	if k.raw != nil {
		sum := sha256.Sum256([]byte(*k.raw))
		copy(p[:], sum[:])
	}
	return p
}

// Hint names a key without revealing it: Prefix, "…" and the last four
// characters — 10 characters (12 bytes), within key_hint VARCHAR(16).
func (k Key) Hint() string {
	if k.raw == nil {
		return ""
	}
	return Prefix + "…" + (*k.raw)[Len-hintTail:]
}

// String returns the Hint, so %v, %s, %q, %x and every Stringer consumer
// print a name instead of the secret.
func (k Key) String() string { return k.Hint() }

// GoString returns the Hint for %#v, as modelkey.Key("ctxm_…abcd").
func (k Key) GoString() string { return "modelkey.Key(" + strconv.Quote(k.Hint()) + ")" }

// LogValue makes slog record the Hint, with the text and the JSON handler
// alike.
func (k Key) LogValue() slog.Value { return slog.StringValue(k.Hint()) }
