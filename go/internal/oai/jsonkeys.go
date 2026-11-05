package oai

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"hash/maphash"
	"slices"
	"sort"
	"strings"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Duplicate and case-variant keys on the levels the parse checks (Fremd-Review
// 2 F2-2, hardening H-2): encoding/json keeps the LAST of two equal keys in a
// map and matches struct fields case-insensitively, while an unchanged
// message, part or PASS value goes upstream byte for byte — an engine whose
// parser keeps the first key, or reads "Type" where ctx read "type", would
// act on a value ctx never checked (an http:// image URL behind a data: one,
// an engine-specific response_format type). The checked value must be the
// sent value, so such a key is refused, never silently normalised: a
// repeated key ⇒ 400 invalid_json (RFC 8259 §4: the behaviour of a receiver
// is unpredictable), a key that differs from a known key of its level only
// in letter case ⇒ 400 unsupported_parameter. Free payload objects — the
// contents of a JSON schema, a tool's parameters — keep their keys as sent.
//
// Cost (bau report W02-07 §Härtung nach Review 2, H-2; FOLGE-P): the scans
// below run over bytes json.Valid already validated and allocate nothing — a
// key that needs decoding (an escape, a non-ASCII byte) is compared in its
// decoded form byte by byte (strDecoder), never decoded into a string.

// memberIter walks the members of one JSON object without decoding or
// allocating. raw must be valid JSON (json.Valid accepted it); a shape
// the walk does not expect sets bad.
type memberIter struct {
	raw     []byte
	i       int
	started bool
	bad     bool
}

// newMemberIter starts a walk over raw; false when raw is no JSON object.
func newMemberIter(raw []byte) (memberIter, bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '{' {
		return memberIter{}, false
	}
	return memberIter{raw: raw, i: i + 1}, true
}

// next returns the next member: its key as sent (between the quotes, escapes
// untouched) and its raw value; ok false at the end (or on a bad shape).
func (it *memberIter) next() (key, val []byte, ok bool) {
	raw := it.raw
	i := skipSpace(raw, it.i)
	if i >= len(raw) {
		it.bad = true
		return nil, nil, false
	}
	switch {
	case raw[i] == '}':
		return nil, nil, false
	case it.started && raw[i] == ',':
		i = skipSpace(raw, i+1)
	case it.started:
		it.bad = true
		return nil, nil, false
	}
	it.started = true
	if i >= len(raw) || raw[i] != '"' {
		it.bad = true
		return nil, nil, false
	}
	end := skipString(raw, i)
	if end > len(raw) || end-1 <= i {
		it.bad = true
		return nil, nil, false
	}
	key = raw[i+1 : end-1]
	j := skipSpace(raw, end)
	if j >= len(raw) || raw[j] != ':' {
		it.bad = true
		return nil, nil, false
	}
	j = skipSpace(raw, j+1)
	vend := skipValue(raw, j)
	it.i = vend
	return key, raw[j:vend], true
}

// objectMembers counts the members of the JSON object raw; -1 when raw is
// no object (never equal to a map's size). Two members whose names decode to
// the same string (an escape such as "url", invalid UTF-8 replaced by
// U+FFFD) count twice: a map of the same object is then smaller — exactly
// the duplicate.
func objectMembers(raw []byte) int {
	it, ok := newMemberIter(raw)
	if !ok {
		return -1
	}
	n := 0
	for {
		if _, _, more := it.next(); !more {
			break
		}
		n++
	}
	if it.bad {
		return -1
	}
	return n
}

func skipSpace(raw []byte, i int) int {
	for i < len(raw) && (raw[i] == ' ' || raw[i] == '\t' || raw[i] == '\n' || raw[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index after the string that starts at raw[i] ('"').
// The closing quote is the first quote not preceded by an odd run of
// backslashes; bytes.IndexByte jumps over the content (a data-URI image is
// megabytes of one string).
func skipString(raw []byte, i int) int {
	j := i + 1
	for {
		k := bytes.IndexByte(raw[j:], '"')
		if k < 0 {
			return len(raw) + 1
		}
		q := j + k
		bs := 0
		for p := q - 1; p > i && raw[p] == '\\'; p-- {
			bs++
		}
		if bs%2 == 0 {
			return q + 1
		}
		j = q + 1
	}
}

// skipValue returns the index after the value that starts at raw[i]: a
// string, a container (strings inside skipped), or a literal.
func skipValue(raw []byte, i int) int {
	if i >= len(raw) {
		return i
	}
	switch raw[i] {
	case '"':
		return min(skipString(raw, i), len(raw))
	case '{', '[':
		depth := 0
		for i < len(raw) {
			switch raw[i] {
			case '"':
				i = min(skipString(raw, i), len(raw))
				continue
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
			i++
		}
		return i
	}
	for ; i < len(raw); i++ {
		switch raw[i] {
		case ',', '}', ']', ' ', '\t', '\r', '\n':
			return i
		}
	}
	return i
}

// keyPath is the param of key on the level at ("" = the top level).
func keyPath(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

func repeated(at, key string) *paramError {
	param := at
	if key != "" {
		param = keyPath(at, key)
	}
	return &paramError{CodeInvalidJSON, param, fmt.Sprintf("The request repeats the key %s.", param)}
}

// hullPairwiseMax bounds the allocation-free duplicate check of a hull
// level (pairwise over its keys): a real hull has two to four keys; a
// larger one takes the decoding path, so the check stays linear.
const hullPairwiseMax = 16

// checkHull checks one hull level whose unknown keys pass verbatim
// (response_format, its json_schema object, tool_choice, a tool): a repeated
// key and a key that differs from one of known only in letter case are
// refused. vals[i] receives the raw value of known[i] (nil when absent).
// isObject is false when raw is no JSON object (the caller's type check
// decides). The level's path is at, or at[idx] for idx ≥ 0 — built only for
// a refusal.
func checkHull(at string, idx int, raw []byte, known []string, vals [][]byte) (isObject bool, perr *paramError) {
	it, ok := newMemberIter(raw)
	if !ok {
		return false, nil
	}
	var seen [hullPairwiseMax][]byte
	n := 0
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		if n == hullPairwiseMax || needsDecode(k) {
			return true, checkHullDecoded(levelPath(at, idx), raw, known, vals)
		}
		for _, p := range seen[:n] {
			if bytes.Equal(p, k) {
				return true, repeated(levelPath(at, idx), decodeKey(k))
			}
		}
		seen[n] = k
		n++
		for i, kn := range known {
			switch {
			case string(k) == kn:
				vals[i] = v
			case asciiEqualFold(k, kn):
				return true, unsupportedParameter(keyPath(levelPath(at, idx), decodeKey(k)))
			}
		}
	}
	if it.bad {
		return false, nil
	}
	return true, nil
}

// levelPath is at, or at[idx] for an element of an array level.
func levelPath(at string, idx int) string {
	if idx < 0 {
		return at
	}
	return fmt.Sprintf("%s[%d]", at, idx)
}

// checkHullDecoded is checkHull over the decoded keys: keys with escapes or
// non-ASCII bytes (Unicode folding: "ſ" folds to "s"), or many keys. A repeat
// of a known key is always found; a repeat among the hull's other keys —
// which pass verbatim, unchecked — when the first of the two is among the
// first pairwiseKeys of them (FOLGE-P: no map of the hull, levelKeys). The
// first repeat in body order wins over a case variant, as the decoded map's
// check did; of several case variants the first in body order is named.
func checkHullDecoded(at string, raw []byte, known []string, vals [][]byte) *paramError {
	it, ok := newMemberIter(raw)
	if !ok {
		return nil
	}
	var keys levelKeys
	var variant []byte
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		idx, fold := hullKey(k, known)
		if keys.add(k, idx) {
			return repeated(at, decodeKey(k))
		}
		switch {
		case idx >= 0:
			vals[idx] = v
		case fold && variant == nil:
			variant = k
		}
	}
	if variant != nil {
		return unsupportedParameter(keyPath(at, decodeKey(variant)))
	}
	return nil
}

// hullKeyDecodeMax bounds the decoded hull key the case-variant check reads:
// a known hull key has at most 11 bytes, and a key that folds onto one has at
// most two more (only k and s have longer fold forms, "K" and "ſ"); a longer
// key is neither known nor a variant.
const hullKeyDecodeMax = 32

// hullKey matches the hull key k against known: its index (-1: none), or
// whether it differs from a known key only in letter case.
func hullKey(k []byte, known []string) (idx int, fold bool) {
	var buf [hullKeyDecodeMax]byte
	n, ok := decodeShort(k, buf[:])
	if !ok {
		return -1, false
	}
	s := buf[:n]
	for i, kn := range known {
		switch {
		case string(s) == kn:
			return i, false
		case strings.EqualFold(string(s), kn):
			fold = true
		}
	}
	return -1, fold
}

// --- FOLGE-P: the allocation-free walks of the request parse ---
//
// The parse of POST /v1/chat/completions holds on the heap only what the
// request's reservation in the in-flight budget covers (FOLGE-P): every
// value is a slice of the body, an array is walked element by element, a key
// is classified before its value is looked at, and a JSON string is compared
// in its decoded form without being decoded into a Go string.

// arrayIter walks the elements of one JSON array without decoding or
// allocating; raw must be valid JSON (json.Valid accepted the body).
type arrayIter struct {
	raw     []byte
	i       int
	started bool
}

// newArrayIter starts a walk over raw; false when raw is no JSON array.
func newArrayIter(raw []byte) (arrayIter, bool) {
	i := skipSpace(raw, 0)
	if i >= len(raw) || raw[i] != '[' {
		return arrayIter{}, false
	}
	return arrayIter{raw: raw, i: i + 1}, true
}

// next returns the next element as sent; false at the end.
func (it *arrayIter) next() ([]byte, bool) {
	raw := it.raw
	i := skipSpace(raw, it.i)
	if i >= len(raw) || raw[i] == ']' {
		return nil, false
	}
	if it.started {
		if raw[i] != ',' {
			return nil, false
		}
		i = skipSpace(raw, i+1)
	}
	it.started = true
	end := skipValue(raw, i)
	it.i = end
	return raw[i:end], true
}

// jsonMaxDepth is the nesting limit of encoding/json's decoder (a deeper
// body is invalid JSON, json.Valid stops there).
const jsonMaxDepth = 10000

// jsonDepthStateBytes is the heap json.Valid keeps per open container
// (measured on go1.27.3, jsonv2: 190 144 bytes at the depth limit, ≈ 19 per
// level, allocated anew on every call that fails), rounded up.
const jsonDepthStateBytes = 24

// jsonStateBytes is the heap the JSON check of raw keeps for its nesting at
// the deepest point (FOLGE-P: a 20 KB body nested to the limit costs
// json.Valid 190 KB, nine times its share of the budget) — counted without
// allocating, so the route books it before the check runs.
func jsonStateBytes(raw []byte) int64 {
	depth, deepest := 0, 0
	for i := 0; i < len(raw) && deepest <= jsonMaxDepth; i++ {
		switch raw[i] {
		case '"':
			i = skipString(raw, i) - 1
		case '[', '{':
			depth++
			deepest = max(deepest, depth)
		case ']', '}':
			depth--
		}
	}
	return int64(deepest) * jsonDepthStateBytes
}

// stringContent is the raw content between the quotes of the JSON string v;
// false when v is no string.
func stringContent(v []byte) ([]byte, bool) {
	t := bytes.TrimSpace(v)
	if len(t) < 2 || t[0] != '"' {
		return nil, false
	}
	return t[1 : len(t)-1], true
}

// strDecoder yields the decoded form of one JSON string — the one
// encoding/json gives it: escapes resolved, an unpaired surrogate escape and
// every invalid UTF-8 byte replaced by U+FFFD — one byte at a time, without
// allocating. raw is the content between the quotes of a valid string.
type strDecoder struct {
	raw    []byte
	i      int
	buf    [utf8.UTFMax]byte
	bi, bn int
}

func (d *strDecoder) next() (byte, bool) {
	if d.bi < d.bn {
		c := d.buf[d.bi]
		d.bi++
		return c, true
	}
	if d.i >= len(d.raw) {
		return 0, false
	}
	c := d.raw[d.i]
	if c != '\\' && c < utf8.RuneSelf {
		d.i++
		return c, true
	}
	var r rune
	if c == '\\' {
		r = d.escape()
	} else {
		var size int
		r, size = utf8.DecodeRune(d.raw[d.i:])
		d.i += size
	}
	d.bn, d.bi = utf8.EncodeRune(d.buf[:], r), 1
	return d.buf[0], true
}

// read decodes the next bytes of the string into dst — the bytes next would
// yield — and returns how many, 0 at the end: a run of plain bytes is copied
// at once, an escape of an ASCII byte written directly (locals, not fields,
// in the loop: it is the digest's whole cost on a key of escapes).
func (d *strDecoder) read(dst []byte) int {
	n := 0
	for n < len(dst) && d.bi < d.bn {
		dst[n] = d.buf[d.bi]
		d.bi++
		n++
	}
	raw, i := d.raw, d.i
	for n < len(dst) && i < len(raw) {
		c := raw[i]
		switch {
		case c != '\\' && c < utf8.RuneSelf:
			dst[n] = c
			n++
			i++
			continue
		case c == '\\' && i+5 < len(raw) && raw[i+1] == 'u':
			if r := hex4(raw[i+2:]); 0 <= r && r < utf8.RuneSelf {
				dst[n] = byte(r)
				n++
				i += 6
				continue
			}
		case c == '\\' && i+1 < len(raw) && raw[i+1] != 'u':
			dst[n] = unescapeByte(raw[i+1])
			n++
			i += 2
			continue
		}
		// a non-ASCII rune: through next, which buffers its UTF-8 bytes
		d.i = i
		b, _ := d.next()
		dst[n] = b
		n++
		i = d.i
		for n < len(dst) && d.bi < d.bn {
			dst[n] = d.buf[d.bi]
			d.bi++
			n++
		}
	}
	d.i = i
	return n
}

// unescapeByte is the byte of the two-byte escape \c (c is not 'u').
func unescapeByte(c byte) byte {
	switch c {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	}
	return c // '"', '\\', '/'
}

// escape resolves the escape sequence at d.i (encoding/json's unquote: a
// surrogate escape pairs with a following low one, else it is U+FFFD).
func (d *strDecoder) escape() rune {
	raw := d.raw
	if d.i+1 >= len(raw) {
		d.i = len(raw)
		return utf8.RuneError
	}
	c := raw[d.i+1]
	d.i += 2
	switch c {
	case 'b':
		return '\b'
	case 'f':
		return '\f'
	case 'n':
		return '\n'
	case 'r':
		return '\r'
	case 't':
		return '\t'
	case 'u':
		r := hex4(raw[d.i:])
		if r < 0 {
			return utf8.RuneError
		}
		d.i += 4
		if !utf16.IsSurrogate(r) {
			return r
		}
		if len(raw) >= d.i+2 && raw[d.i] == '\\' && raw[d.i+1] == 'u' {
			if dec := utf16.DecodeRune(r, hex4(raw[d.i+2:])); dec != unicode.ReplacementChar {
				d.i += 6
				return dec
			}
		}
		return unicode.ReplacementChar
	}
	return rune(c) // '"', '\\', '/'
}

// hex4 reads four hex digits; -1 when there are none.
func hex4(s []byte) rune {
	if len(s) < 4 {
		return -1
	}
	a, b, c, d := hexDigit[s[0]], hexDigit[s[1]], hexDigit[s[2]], hexDigit[s[3]]
	if a|b|c|d > 0xf {
		return -1
	}
	return rune(a)<<12 | rune(b)<<8 | rune(c)<<4 | rune(d)
}

// hexDigit is the value of a hex digit, 0xff for any other byte.
var hexDigit = func() (t [256]byte) {
	for i := range t {
		t[i] = 0xff
	}
	for i := range 10 {
		t['0'+i] = byte(i)
	}
	for i := range 6 {
		t['a'+i], t['A'+i] = byte(10+i), byte(10+i)
	}
	return t
}()

// cmpKeys compares the decoded forms of the raw JSON string contents a and b
// byte by byte — the order sort.Strings gave the decoded keys — without
// allocating. The tokens both begin with alike are skipped undecoded
// (sameTokens: a key compared with the smallest refused one, FOLGE-P H-6).
func cmpKeys(a, b []byte) int {
	if !needsDecode(a) && !needsDecode(b) {
		return bytes.Compare(a, b)
	}
	i := sameTokens(a, b)
	da, db := strDecoder{raw: a, i: i}, strDecoder{raw: b, i: i}
	for {
		ca, oka := da.next()
		cb, okb := db.next()
		switch {
		case !oka || !okb:
			return boolCmp(oka, okb)
		case ca != cb:
			return int(ca) - int(cb)
		}
	}
}

// sameTokens is the length of the longest prefix a and b share token by
// token — a plain byte, an escape that is not a surrogate (one that is may
// pair with the next), a UTF-8 sequence — each token sent alike in both:
// up to there both decode alike, and a decoder of either may start there.
// Eight plain bytes alike are skipped at once.
func sameTokens(a, b []byte) int {
	n := min(len(a), len(b))
	i := 0
	for i < n {
		if i+8 <= n {
			w := binary.LittleEndian.Uint64(a[i:])
			if w == binary.LittleEndian.Uint64(b[i:]) && !wordNeedsDecode(w) {
				i += 8
				continue
			}
		}
		c := a[i]
		switch {
		case c != b[i]:
			return i
		case c != '\\' && c < utf8.RuneSelf:
			i++
		case c >= utf8.RuneSelf:
			_, na := utf8.DecodeRune(a[i:])
			if _, nb := utf8.DecodeRune(b[i:]); na != nb || string(a[i:i+na]) != string(b[i:i+na]) {
				return i
			}
			i += na
		case i+1 >= n || a[i+1] != b[i+1]:
			return i
		case a[i+1] != 'u':
			i += 2
		case i+6 > n || string(a[i:i+6]) != string(b[i:i+6]) || a[i+2]|0x20 == 'd' && a[i+3] >= '8':
			return i // a surrogate escape (D800–DFFF) may pair with the one after it
		default:
			i += 6
		}
	}
	return i
}

// cmpKeyName compares the decoded form of the raw JSON string content raw
// with name.
func cmpKeyName(raw []byte, name string) int {
	d := strDecoder{raw: raw}
	for i := 0; ; i++ {
		c, ok := d.next()
		switch {
		case !ok || i == len(name):
			return boolCmp(ok, i < len(name))
		case c != name[i]:
			return int(c) - int(name[i])
		}
	}
}

// boolCmp orders a shorter string (false: no byte left) first.
func boolCmp(a, b bool) int {
	switch {
	case a == b:
		return 0
	case a:
		return 1
	}
	return -1
}

// decodeShort decodes the raw JSON string content raw into dst; false when
// it does not fit.
func decodeShort(raw, dst []byte) (int, bool) {
	if len(raw) > 6*len(dst) { // a decoded byte takes at most six sent ones (an escape)
		return 0, false
	}
	if !needsDecode(raw) {
		return copy(dst, raw), len(raw) <= len(dst)
	}
	d := strDecoder{raw: raw}
	n := 0
	for c, ok := d.next(); ok; c, ok = d.next() {
		if n == len(dst) {
			return n, false
		}
		dst[n] = c
		n++
	}
	return n, true
}

// echoNameMax bounds a client's key name in an error answer, in its param
// and its message (FOLGE-P hardening H-9): longer than every name a level
// knows (max_completion_tokens, 21 bytes) and than the vendor keys real
// clients send, short enough that a refusal stays a few hundred bytes. Uncut,
// an 8 MiB key named twice and HTML-escaped by the answer's writer was an
// answer of 100 MB, twelve times the body, outside every reservation. A
// longer name is cut at a rune boundary and marked with "…".
const echoNameMax = 64

// decodeKey is the decoded form of the raw JSON string content raw as a Go
// string, cut to echoNameMax bytes — the refusal path only (a param, a
// message): it decodes no more than it returns.
func decodeKey(raw []byte) string {
	var buf [echoNameMax + utf8.UTFMax]byte
	d := strDecoder{raw: raw}
	n := d.read(buf[:])
	if n <= echoNameMax {
		return string(buf[:n])
	}
	cut := echoNameMax
	for cut > 0 && buf[cut]&0xc0 == 0x80 { // buf[cut] continues a rune: the rune does not fit
		cut--
	}
	return string(buf[:cut]) + "…"
}

// levelKeyDecodeMax bounds the decoded key the level lookups read (the
// longest named key of any level is 21 bytes); a longer key is unknown.
const levelKeyDecodeMax = 32

// keyIndex is the index of the decoded key raw in a level's index, -1 for a
// key the level does not name.
func keyIndex(raw []byte, index map[string]int) int {
	var buf [levelKeyDecodeMax]byte
	n, ok := decodeShort(raw, buf[:])
	if !ok {
		return -1
	}
	if i, found := index[string(buf[:n])]; found {
		return i
	}
	return -1
}

// maxLevelKeys bounds the named keys of one level (levelKeys.seen is one
// word).
const maxLevelKeys = 64

// levelNames is the sorted list and the index of a level's named keys.
func levelNames(sets ...[]string) ([]string, map[string]int) {
	var names []string
	for _, s := range sets {
		names = append(names, s...)
	}
	sort.Strings(names)
	names = slices.Compact(names)
	if len(names) > maxLevelKeys {
		panic(fmt.Sprintf("oai: %d named keys on one level, at most %d", len(names), maxLevelKeys))
	}
	index := make(map[string]int, len(names))
	for i, k := range names {
		index[k] = i
	}
	return names, index
}

// pairwiseKeys bounds the unknown keys of one object level a repeat is
// searched among pairwise (FOLGE-P). Finding every repeat among n keys needs
// memory in n — a set of them, which is the heap the probe of 2026-11-05
// measured (645 271 unknown top-level keys: 1.36 GB for ten requests). A
// repeat of a key the level names is found always (one bit per name); a
// repeat among the other keys when the first of the two is among the first
// pairwiseKeys of them — every object with at most that many unknown keys is
// checked exactly as before. A mechanism bound of the check, not a cap on
// the request: no object is refused for its size.
const pairwiseKeys = 16

// levelKeys is the key check of one object level.
type levelKeys struct {
	seen    uint64 // bit i: the level's named key i was seen
	unknown [pairwiseKeys][]byte
	digest  [pairwiseKeys]keyDigest // digest[i] belongs to unknown[i]
	n       int
	reject  []byte // the smallest unknown key so far (raw), nil none
}

// add notes member key k of named index idx (-1: a key the level does not
// name) and reports a repeat (see pairwiseKeys). An unknown key is decoded
// once, into its digest; it is compared in full only with a window key of
// the same digest (FOLGE-P hardening H-6: a full comparison against every
// window key cost a key up to pairwiseKeys decodes of itself — 8 MiB of long
// escaped keys took ten times the base parse). Equal keys have equal
// digests, so the answer is the full comparison's.
func (s *levelKeys) add(k []byte, idx int) bool {
	if idx >= 0 {
		bit := uint64(1) << uint(idx)
		if s.seen&bit != 0 {
			return true
		}
		s.seen |= bit
		return false
	}
	d := digestKey(k)
	for i, u := range s.unknown[:s.n] {
		if s.digest[i] == d && cmpKeys(u, k) == 0 {
			return true
		}
	}
	if s.n < pairwiseKeys {
		s.unknown[s.n], s.digest[s.n] = k, d
		s.n++
	}
	return false
}

// keyDigest is the length and a seeded hash of a key's decoded form.
type keyDigest struct {
	n int
	h uint64
}

// keySeed seeds the key digests of the process: a client cannot aim its keys
// at one hash value. A collision costs a full comparison, never an answer.
var keySeed = maphash.MakeSeed()

// digestKey is the digest of the decoded form of the raw JSON string content
// raw, decoded once and without allocating (maphash hashes the same bytes
// to the same value however they are split across writes).
func digestKey(raw []byte) keyDigest {
	if !needsDecode(raw) {
		return keyDigest{len(raw), maphash.Bytes(keySeed, raw)}
	}
	var h maphash.Hash
	h.SetSeed(keySeed)
	var buf [128]byte
	total := 0
	d := strDecoder{raw: raw}
	for {
		n := d.read(buf[:])
		if n == 0 {
			return keyDigest{total, h.Sum64()}
		}
		_, _ = h.Write(buf[:n])
		total += n
	}
}

// refuse notes the unknown key k: the level refuses the smallest in sort
// order (the base parse sorted the decoded keys and named the first).
func (s *levelKeys) refuse(k []byte) {
	if s.reject == nil || cmpKeys(k, s.reject) < 0 {
		s.reject = k
	}
}

// needsDecode reports a key the byte comparison cannot judge: an escape or
// a non-ASCII byte.
func needsDecode(k []byte) bool {
	i := 0
	for ; i+8 <= len(k); i += 8 { // eight bytes a step: a key can be megabytes long
		if wordNeedsDecode(binary.LittleEndian.Uint64(k[i:])) {
			return true
		}
	}
	for _, c := range k[i:] {
		if c == '\\' || c >= 0x80 {
			return true
		}
	}
	return false
}

// wordNeedsDecode is needsDecode over the eight bytes of w.
func wordNeedsDecode(w uint64) bool {
	const ones, highs, backslashes = 0x0101010101010101, 0x8080808080808080, 0x5c5c5c5c5c5c5c5c
	x := w ^ backslashes // a zero byte where w holds a backslash
	return (w|(x-ones)&^x)&highs != 0
}

// asciiEqualFold is strings.EqualFold for an ASCII key against a known key.
func asciiEqualFold(k []byte, kn string) bool {
	if len(k) != len(kn) {
		return false
	}
	for i := range k {
		a, b := k[i], kn[i]
		if 'A' <= a && a <= 'Z' {
			a += 'a' - 'A'
		}
		if 'A' <= b && b <= 'Z' {
			b += 'a' - 'A'
		}
		if a != b {
			return false
		}
	}
	return true
}
