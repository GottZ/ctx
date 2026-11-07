package oai

import (
	"bytes"
	"encoding/json"
	"slices"
	"strconv"
	"unicode/utf8"
)

// The writer of the stream rewrite (FOLGE-S, RC-D cluster C8): the client
// form of an upstream chunk is written by walking the upstream bytes (FOLGE-P's memberIter, cmpKeys,
// strDecoder) into one output buffer, byte for byte what encoding/json
// wrote for the map rewrite before (json.Marshal of map[string]
// json.RawMessage: members sorted by their decoded keys, a repeated key's
// last value, every value compacted and HTML-escaped, nothing else of it
// touched). The map rewrite cost 7–8 times the chunk in allocations (the
// map's copies of every value, normalizeChunkChoices' second decode, two
// json.Marshal buffers growing by a quarter); the walk allocates its output
// buffer once per attempt and nothing per byte. TestRewriteChunkTraceIsTheBase
// and FuzzRewriteChunkIntoIsTheBase hold the bytes to the base's.

// member is one member of an object level being rewritten: its key as sent
// (escapes untouched) and its raw value; synth marks a member the rewrite
// adds, gone one it drops.
type member struct {
	key, val []byte
	synth    bool
	gone     bool
}

// levelMembersMax is the size of a level's member array on the stack: a
// choice or a delta has a handful of keys; a level with more takes the heap.
const levelMembersMax = 16

// collectMembers appends the members of the JSON object raw to ms, sorted by
// their decoded keys, a repeated key once with its last value — the members
// json.Marshal writes for the map json.Unmarshal reads from raw. raw is a
// valid JSON object.
func collectMembers(ms []member, raw []byte) []member {
	it, ok := newMemberIter(raw)
	if !ok {
		return ms
	}
	start := len(ms)
	for {
		k, v, more := it.next()
		if !more {
			break
		}
		ms = append(ms, member{key: k, val: v})
	}
	return sortMembers(ms, start)
}

// sortMembers sorts ms[start:] by decoded key (stable: equal keys keep their
// order) and keeps the last of equal keys.
func sortMembers(ms []member, start int) []member {
	level := ms[start:]
	slices.SortStableFunc(level, func(a, b member) int { return cmpKeys(a.key, b.key) })
	out := level[:0]
	for i, m := range level {
		if i+1 < len(level) && cmpKeys(m.key, level[i+1].key) == 0 {
			continue
		}
		out = append(out, m)
	}
	return ms[:start+len(out)]
}

// findMember is the index of the member whose decoded key is name in the
// sorted level ms, -1 when there is none.
func findMember(ms []member, name string) int {
	for i := range ms {
		if !ms[i].gone && keyIs(ms[i].key, name) {
			return i
		}
	}
	return -1
}

// keyIs reports whether the raw key decodes to name.
func keyIs(raw []byte, name string) bool {
	if !needsDecode(raw) {
		return string(raw) == name
	}
	return cmpKeyName(raw, name) == 0
}

// insertMember adds the synthetic member name: val to the sorted level ms
// (name is not in it) at its place in the order.
func insertMember(ms []member, name string, val []byte) []member {
	at := len(ms)
	for i := range ms {
		if cmpKeyName(ms[i].key, name) > 0 {
			at = i
			break
		}
	}
	return slices.Insert(ms, at, member{key: []byte(name), val: val, synth: true})
}

// growBy makes room for n more bytes in dst: the capacity at least doubles
// when it grows, so a run of growths costs at most twice the final buffer.
func growBy(dst []byte, n int) []byte {
	if cap(dst)-len(dst) >= n {
		return dst
	}
	grown := make([]byte, len(dst), max(len(dst)+n, 2*cap(dst)))
	copy(grown, dst)
	return grown
}

// rawPlain reports whether json.Marshal writes the raw value v as it is:
// no whitespace to drop, nothing to escape for HTML (<, >, &, U+2028,
// U+2029; json.Marshal keeps every other byte of a raw value, escapes and
// invalid UTF-8 included).
func rawPlain(v []byte) bool {
	for _, c := range v {
		switch c {
		case ' ', '\t', '\n', '\r', '<', '>', '&', 0xe2:
			return false
		}
	}
	return true
}

// rawLineSep reports whether v[i:] starts with U+2028 or U+2029.
func rawLineSep(v []byte, i int) bool {
	return i+2 < len(v) && v[i] == 0xe2 && v[i+1] == 0x80 && (v[i+2] == 0xa8 || v[i+2] == 0xa9)
}

// rawLen is the length json.Marshal writes for the raw value v.
func rawLen(v []byte) int {
	n, in := 0, false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case in && c == '\\':
			n += 2
			i++
			continue
		case c == '"':
			in = !in
		case !in && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			continue
		case c == '<' || c == '>' || c == '&':
			n += 6
			continue
		case rawLineSep(v, i):
			n += 6
			i += 2
			continue
		}
		n++
	}
	return n
}

// appendRaw appends what json.Marshal writes for the json.RawMessage v (a
// valid JSON value): compacted, HTML-escaped.
func appendRaw(dst, v []byte) []byte {
	if rawPlain(v) {
		return append(growBy(dst, len(v)), v...)
	}
	dst = growBy(dst, rawLen(v))
	in := false
	for i := 0; i < len(v); i++ {
		c := v[i]
		switch {
		case in && c == '\\':
			dst = append(dst, c, v[i+1])
			i++
			continue
		case c == '"':
			in = !in
		case !in && (c == ' ' || c == '\t' || c == '\n' || c == '\r'):
			continue
		case c == '<' || c == '>' || c == '&':
			dst = appendEscape(dst, rune(c))
			continue
		case rawLineSep(v, i):
			dst = appendEscape(dst, 0x2028+rune(v[i+2]-0xa8))
			i += 2
			continue
		}
		dst = append(dst, c)
	}
	return dst
}

const lowerHex = "0123456789abcdef"

// appendEscape appends the six-byte escape of r (r < 0x10000).
func appendEscape(dst []byte, r rune) []byte {
	return append(dst, '\\', 'u', lowerHex[r>>12&0xf], lowerHex[r>>8&0xf], lowerHex[r>>4&0xf], lowerHex[r&0xf])
}

// appendRune appends r as json.Marshal writes it inside a string.
func appendRune(dst []byte, r rune) []byte {
	switch {
	case r == '"' || r == '\\':
		return append(dst, '\\', byte(r))
	case r == '\b':
		return append(dst, '\\', 'b')
	case r == '\f':
		return append(dst, '\\', 'f')
	case r == '\n':
		return append(dst, '\\', 'n')
	case r == '\r':
		return append(dst, '\\', 'r')
	case r == '\t':
		return append(dst, '\\', 't')
	case r < 0x20 || r == '<' || r == '>' || r == '&' || r == 0x2028 || r == 0x2029:
		return appendEscape(dst, r)
	case r < utf8.RuneSelf:
		return append(dst, byte(r))
	}
	return utf8.AppendRune(dst, r)
}

// appendString appends the JSON string json.Marshal writes for s.
func appendString(dst []byte, s string) []byte {
	dst = append(growBy(dst, len(s)+2), '"')
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		dst = appendRune(dst, r) // an invalid byte decodes as U+FFFD, as json.Marshal writes it
		i += size
	}
	return append(dst, '"')
}

// appendKey appends the decoded raw key (the content of a valid JSON
// string) as json.Marshal writes a map key.
func appendKey(dst, raw []byte) []byte {
	if !needsDecode(raw) && rawPlain(raw) {
		dst = append(growBy(dst, len(raw)+2), '"')
		return append(append(dst, raw...), '"')
	}
	dst = append(growBy(dst, len(raw)+2), '"')
	for i := 0; i < len(raw); {
		c := raw[i]
		var r rune
		switch {
		case c == '\\':
			d := strDecoder{raw: raw, i: i}
			r = d.escape()
			i = d.i
		case c < utf8.RuneSelf:
			r = rune(c)
			i++
		default:
			var size int
			r, size = utf8.DecodeRune(raw[i:])
			i += size
		}
		dst = appendRune(dst, r)
	}
	return append(dst, '"')
}

// appendMember appends key: v (v a raw value), with a comma before every
// member but the first of its object.
func appendMember(dst []byte, first bool, key, v []byte) []byte {
	if !first {
		dst = append(dst, ',')
	}
	dst = appendKey(dst, key)
	dst = append(dst, ':')
	return appendRaw(dst, v)
}

// appendLevel appends the members of ms that are not gone as one object.
func appendLevel(dst []byte, ms []member) []byte {
	dst = append(growBy(dst, 2), '{')
	first := true
	for _, m := range ms {
		if m.gone {
			continue
		}
		dst = appendMember(dst, first, m.key, m.val)
		first = false
	}
	return append(dst, '}')
}

// stringRunes counts the runes of the decoded JSON string value v — what
// utf8.RuneCountInString counts on the Go string json.Unmarshal gives it —
// and reports whether v decodes into a Go string (a string, or null: none).
func stringRunes(v []byte) (int, bool) {
	content, ok := stringContent(v)
	if !ok {
		return 0, isNull(v)
	}
	if !needsDecode(content) {
		return len(content), true
	}
	n := 0
	for i := 0; i < len(content); n++ {
		switch c := content[i]; {
		case c == '\\':
			d := strDecoder{raw: content, i: i}
			d.escape()
			i = d.i
		case c < utf8.RuneSelf:
			i++
		default:
			_, size := utf8.DecodeRune(content[i:])
			i += size
		}
	}
	return n, true
}

// rawInt is strconv.Atoi over a raw JSON value: a plain integer that fits
// an int, else false.
func rawInt(v []byte) (int, bool) {
	if len(v) == 0 || len(v) > 20 {
		return 0, false
	}
	var buf [20]byte
	i, err := strconv.Atoi(string(append(buf[:0], v...)))
	return i, err == nil
}

// rawStringIs reports whether the raw JSON value v is a string that decodes
// to one of names (json.Unmarshal into a Go string, compared).
func rawStringIn(v []byte, names map[string]bool) (string, bool) {
	content, ok := stringContent(v)
	if !ok {
		return "", false
	}
	var buf [16]byte
	n, fits := decodeShort(content, buf[:])
	if !fits {
		return "", false
	}
	for name := range names {
		if string(buf[:n]) == name {
			return name, true
		}
	}
	return "", false
}

// mapError is the error json.Unmarshal gives raw read as T — the base
// rewrite's error, on the refusal path only.
func mapError[T any](raw []byte) error {
	var v T
	return json.Unmarshal(raw, &v)
}

// trimJSON is raw without the whitespace around its value.
func trimJSON(raw []byte) []byte { return bytes.TrimSpace(raw) }
