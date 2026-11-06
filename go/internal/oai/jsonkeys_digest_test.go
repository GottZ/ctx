package oai

// Pins of the key digest of FOLGE-P hardening H-6: the word-wise scan of
// needsDecode, the bulk decoder of the digest and the digest itself against
// the byte-wise decoder and encoding/json.

import (
	"bytes"
	"encoding/json"
	"hash/maphash"
	"slices"
	"strings"
	"testing"
)

// TestNeedsDecodeIsTheByteLoop pins the word-wise scan of needsDecode
// against the byte loop it replaces: every byte value at every position of
// keys up to 24 bytes long.
func TestNeedsDecodeIsTheByteLoop(t *testing.T) {
	loop := func(k []byte) bool {
		for _, c := range k {
			if c == '\\' || c >= 0x80 {
				return true
			}
		}
		return false
	}
	for n := 1; n <= 24; n++ {
		k := []byte(strings.Repeat("a", n))
		for i := range n {
			for c := range 256 {
				k[i] = byte(c)
				if got, want := needsDecode(k), loop(k); got != want {
					t.Fatalf("needsDecode(%q) = %v, want %v", k, got, want)
				}
			}
			k[i] = 'a'
		}
	}
}

// strDecoderReadSeeds are JSON string contents for the read pins: plain
// runs, every escape form, surrogate pairs and lone halves, invalid UTF-8;
// runs of literal UTF-8 across the digest's 128-byte buffer, broken by an
// invalid byte or cut at the end (Delta-Review 2 F-3), and a backslash
// before a non-ASCII byte — outside a valid string's content, where read
// gave the raw byte and next U+0080…U+00FF (Delta-Review 2 F-6).
func strDecoderReadSeeds() []string {
	return []string{
		"", "a", strings.Repeat("plain ", 40),
		escU('a') + "b" + escU(0xe9) + escU(0x20ac) + `\"\\\/\b\f\n\r\t`,
		escU(0xd83d) + escU(0xde00) + "x" + escU(0xd800) + "y" + escU(0xdc00) + escU(0xd800) + escU(0xd800) + escU(0xdc00),
		"\xff\xfe" + "é€😀" + "\xe2\x82" + "z" + "\xc3",
		strings.Repeat(escU('a'), 300) + strings.Repeat("é", 70),
		strings.Repeat("中", 50) + "x" + strings.Repeat("😀", 40) + escU(0xe9) + strings.Repeat("é", 63),
		strings.Repeat("é", 64) + "\xff" + strings.Repeat("😀", 33) + "\xed\xa0\x80" + "€" + "\xe2\x82",
		strings.Repeat("a", 127) + "😀" + strings.Repeat("b", 126) + "中\\é",
		"\\\x80", "a\\\xc3\xa9", "\\\xff" + strings.Repeat("é", 70),
	}
}

// TestStrDecoderReadIsNext pins the bulk decoder of the key digest against
// the byte-wise one, at every buffer size the digest and a caller may use.
func TestStrDecoderReadIsNext(t *testing.T) {
	for _, s := range strDecoderReadSeeds() {
		checkStrDecoderRead(t, []byte(s))
	}
}

// FuzzStrDecoderRead is TestStrDecoderReadIsNext under the fuzzer, on every
// input: read is next byte for byte also outside a valid string's content
// (Delta-Review 2 F-6); encoding/json is the reference where it accepts the
// string.
func FuzzStrDecoderRead(f *testing.F) {
	for _, s := range strDecoderReadSeeds() {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, raw []byte) {
		checkStrDecoderRead(t, raw)
	})
}

// decodeAll is next's decoding of raw from byte i on.
func decodeAll(raw []byte, i int) []byte {
	var out []byte
	d := strDecoder{raw: raw, i: i}
	for c, ok := d.next(); ok; c, ok = d.next() {
		out = append(out, c)
	}
	return out
}

// checkSameTokens pins sameTokens and cmpKeys on the pair a, b (contents of
// valid strings): the prefix sameTokens skips decodes alike in both and is a
// start a decoder of either may take (prefix + rest from there = the whole
// decoding), cmpKeys is the order of the decoded keys, and equal decoded
// keys have equal digests.
func checkSameTokens(t *testing.T, a, b []byte) {
	t.Helper()
	da, db := decodeAll(a, 0), decodeAll(b, 0)
	if got, want := cmpKeys(a, b), bytes.Compare(da, db); (got < 0) != (want < 0) || (got > 0) != (want > 0) {
		t.Fatalf("cmpKeys(%q, %q) = %d, the decoded order %d", a, b, got, want)
	}
	i := sameTokens(a, b)
	if i < 0 || i > min(len(a), len(b)) {
		t.Fatalf("sameTokens(%q, %q) = %d, out of range", a, b, i)
	}
	pa, pb := decodeAll(a[:i], 0), decodeAll(b[:i], 0)
	if !bytes.Equal(pa, pb) {
		t.Fatalf("sameTokens(%q, %q) = %d: the prefixes decode apart, %q and %q", a, b, i, pa, pb)
	}
	if !bytes.Equal(append(pa, decodeAll(a, i)...), da) || !bytes.Equal(append(pb, decodeAll(b, i)...), db) {
		t.Fatalf("sameTokens(%q, %q) = %d is no start a decoder may take", a, b, i)
	}
	if bytes.Equal(da, db) && digestKey(a) != digestKey(b) {
		t.Fatalf("equal keys %q and %q, different digests", a, b)
	}
}

// TestSameTokensIsAStart is checkSameTokens on the read seeds, each against
// the others, its halves joined with them and itself extended by them.
func TestSameTokensIsAStart(t *testing.T) {
	seeds := strDecoderReadSeeds()
	for i, s := range seeds {
		for _, o := range seeds[i:] {
			fuzzSameTokens(t, []byte(s), []byte(o))
		}
	}
}

// FuzzSameTokensIsAStart is TestSameTokensIsAStart under the fuzzer (contents
// json.Valid accepts as strings: sameTokens' contract).
func FuzzSameTokensIsAStart(f *testing.F) {
	seeds := strDecoderReadSeeds()
	for i, s := range seeds {
		f.Add([]byte(s), []byte(seeds[(i+3)%len(seeds)]))
	}
	f.Fuzz(fuzzSameTokens)
}

func fuzzSameTokens(t *testing.T, a, other []byte) {
	valid := func(b []byte) bool { return json.Valid(append(append([]byte{'"'}, b...), '"')) }
	if len(a) > 4096 || len(other) > 4096 || !valid(a) || !valid(other) {
		return
	}
	for _, b := range [][]byte{other, append(slices.Clone(a[:len(a)/2]), other...), append(slices.Clone(a), other...)} {
		if valid(b) {
			checkSameTokens(t, a, b)
			checkSameTokens(t, b, a)
		}
	}
}

func checkStrDecoderRead(t *testing.T, raw []byte) {
	t.Helper()
	var want []byte
	d := strDecoder{raw: raw}
	for c, ok := d.next(); ok; c, ok = d.next() {
		want = append(want, c)
	}
	var s string
	if err := json.Unmarshal([]byte(`"`+string(raw)+`"`), &s); err == nil && s != string(want) {
		t.Fatalf("next of %q = %q, encoding/json %q", raw, want, s)
	}
	for _, size := range []int{1, 2, 3, 7, 64, 128} {
		var got []byte
		buf := make([]byte, size)
		d := strDecoder{raw: raw}
		for n := d.read(buf); n > 0; n = d.read(buf) {
			got = append(got, buf[:n]...)
		}
		if string(got) != string(want) {
			t.Fatalf("read(%d) of %q = %q, next %q", size, raw, got, want)
		}
	}
	if dg := digestKey(raw); dg != (keyDigest{len(want), maphash.Bytes(keySeed, want)}) {
		t.Fatalf("digest of %q = %+v, not the length and hash of its decoded form %q", raw, dg, want)
	}
}

// TestLevelKeysDigestGatesTheComparison pins the window's contract (H-6): a
// key is compared in full only with a window key of equal length and hash,
// and a full comparison decides — a digest match alone is no repeat. The
// window entries are set by hand: a seeded 64-bit hash collides too rarely
// to be met by a body.
func TestLevelKeysDigestGatesTheComparison(t *testing.T) {
	a, b := []byte("alpha"), []byte("bravo")
	slot := 0 // a variable: the test compiles for any window size (a mutant of it)
	var s levelKeys
	s.unknown[slot], s.digest[slot], s.n = a, digestKey(b), 1 // a collision: a's slot holds b's digest
	if s.add(b, -1) {
		t.Error("a digest match of two different keys is taken as a repeat")
	}
	s = levelKeys{}
	d := digestKey(a)
	s.unknown[slot], s.digest[slot], s.n = a, keyDigest{d.n + 1, d.h}, 1 // same hash, another length
	if s.add(a, -1) {
		t.Error("a hash match of another length is compared")
	}
	s = levelKeys{}
	s.add([]byte("k"+escU('e')+"y"), -1)
	if !s.add([]byte("key"), -1) {
		t.Error("a key equal in decoded form to a window key is no repeat")
	}
}

// TestCmpKeysIsTheDecodedOrder pins cmpKeys — sameTokens skipping the
// tokens both keys share undecoded — against the order of the decoded keys
// (encoding/json, then a byte comparison) over keys that share a prefix up
// to every kind of token and differ right after it: a plain byte, an escape,
// a surrogate pair's first half, a UTF-8 sequence, invalid UTF-8.
func TestCmpKeysIsTheDecodedOrder(t *testing.T) {
	parts := []string{"", "a", "b", escU('a'), escU('b'), escU(0xe9), escU(0xd83d) + escU(0xde00), escU(0xd83d) + escU(0xdc00),
		escU(0xd83d), escU(0xdc00), "é", "😀", "🐀", "\xff", "\xc3", strings.Repeat("p", 9), strings.Repeat(escU('p'), 3), `\"`}
	for _, prefix := range []string{"", strings.Repeat("q", 13), escU('q') + "q" + escU(0xd83d) + escU(0xde00)} {
		for _, x := range parts {
			for _, y := range parts {
				a, b := []byte(prefix+x), []byte(prefix+y)
				var da, db string
				if json.Unmarshal([]byte(`"`+string(a)+`"`), &da) != nil || json.Unmarshal([]byte(`"`+string(b)+`"`), &db) != nil {
					continue
				}
				if got, want := sign(cmpKeys(a, b)), sign(strings.Compare(da, db)); got != want {
					t.Errorf("cmpKeys(%q, %q) = %d, the decoded keys %q, %q compare %d", a, b, got, da, db, want)
				}
			}
		}
	}
}

func sign(c int) int {
	switch {
	case c < 0:
		return -1
	case c > 0:
		return 1
	}
	return 0
}
