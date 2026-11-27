package recblocks

import (
	"bytes"
	"encoding/json"
	"strconv"
)

// The decomposition walks a body that json.Valid accepted with the small
// scanner below instead of decoding it into a generic tree: a member's or
// an element's value is a sub-slice of the decomposition's own copy of the
// body (zero copies, its raw bytes exactly as written), and nothing recurses
// — a container's end is found with a depth count, so no nesting reaches the
// stack. The scanner trusts the validation (it is never handed anything
// else) and is bounds-safe regardless: on malformed bytes it stops at the
// end instead of panicking.

// member is one member of a JSON object.
type member struct {
	key    string // the decoded key
	rawKey []byte // the key as written, quotes included
	val    []byte // the value as written
}

// object is a JSON object's members in document order.
type object []member

// get returns the value of key, the last occurrence when the object names it
// twice (as encoding/json reads it).
func (o object) get(key string) ([]byte, bool) {
	for i := len(o) - 1; i >= 0; i-- {
		if o[i].key == key {
			return o[i].val, true
		}
	}
	return nil, false
}

// str returns the decoded string value of key; ok=false when the key is
// absent or its value is not a string.
func (o object) str(key string) ([]byte, bool) {
	v, ok := o.get(key)
	if !ok {
		return nil, false
	}
	return decodeString(v)
}

// text is str as a Go string, "" when absent or not a string.
func (o object) text(key string) string {
	s, _ := o.str(key)
	return string(s)
}

// obj returns the object value of key; ok=false when the key is absent or
// its value is not an object.
func (o object) obj(key string) (object, bool) {
	v, ok := o.get(key)
	if !ok {
		return nil, false
	}
	return parseObject(v)
}

func isWS(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

func skipWS(b []byte, i int) int {
	for i < len(b) && isWS(b[i]) {
		i++
	}
	return i
}

// trimWS is b without leading and trailing JSON whitespace.
func trimWS(b []byte) []byte {
	i, j := skipWS(b, 0), len(b)
	for j > i && isWS(b[j-1]) {
		j--
	}
	return b[i:j:j]
}

// stringEnd is the index after the closing quote of the string that starts
// at b[i].
func stringEnd(b []byte, i int) int {
	for j := i + 1; j < len(b); j++ {
		switch b[j] {
		case '\\':
			j++
		case '"':
			return j + 1
		}
	}
	return len(b)
}

// containerEnd is the index after the object or array that starts at b[i].
func containerEnd(b []byte, i int) int {
	depth := 0
	for j := i; j < len(b); j++ {
		switch b[j] {
		case '"':
			j = stringEnd(b, j) - 1
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				return j + 1
			}
		}
	}
	return len(b)
}

// valueEnd is the index after the value that starts at b[i].
func valueEnd(b []byte, i int) int {
	if i >= len(b) {
		return len(b)
	}
	switch b[i] {
	case '"':
		return stringEnd(b, i)
	case '{', '[':
		return containerEnd(b, i)
	}
	j := i
	for j < len(b) && b[j] != ',' && b[j] != '}' && b[j] != ']' && b[j] != ':' && !isWS(b[j]) {
		j++
	}
	return j
}

// parseObject returns the members of v; ok=false when v is not an object.
func parseObject(v []byte) (object, bool) {
	if len(v) == 0 || v[0] != '{' {
		return nil, false
	}
	var o object
	i := skipWS(v, 1)
	for i < len(v) && v[i] == '"' {
		ke := stringEnd(v, i)
		m := member{rawKey: v[i:ke:ke]}
		if k, ok := decodeString(m.rawKey); ok {
			m.key = string(k)
		}
		i = skipWS(v, ke)
		if i < len(v) && v[i] == ':' {
			i = skipWS(v, i+1)
		}
		ve := valueEnd(v, i)
		m.val = v[i:ve:ve]
		o = append(o, m)
		i = skipWS(v, ve)
		if i < len(v) && v[i] == ',' {
			i = skipWS(v, i+1)
		}
	}
	return o, true
}

// parseArray returns the elements of v; ok=false when v is not an array.
func parseArray(v []byte) ([][]byte, bool) {
	if len(v) == 0 || v[0] != '[' {
		return nil, false
	}
	var out [][]byte
	i := skipWS(v, 1)
	for i < len(v) && v[i] != ']' {
		ve := valueEnd(v, i)
		if ve <= i {
			break
		}
		out = append(out, v[i:ve:ve])
		i = skipWS(v, ve)
		if i < len(v) && v[i] == ',' {
			i = skipWS(v, i+1)
		}
	}
	return out, true
}

// decodeString returns the string v encodes; ok=false when v is not a
// string. Without an escape it is the bytes between the quotes (a sub-slice
// of v: the body is valid UTF-8 and json.Valid rejects control bytes in a
// string, so encoding/json would yield exactly these bytes); with one, what
// encoding/json decodes.
func decodeString(v []byte) ([]byte, bool) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return nil, false
	}
	inner := v[1 : len(v)-1 : len(v)-1]
	if bytes.IndexByte(inner, '\\') < 0 {
		return inner, true
	}
	var s string
	if err := json.Unmarshal(v, &s); err != nil {
		return nil, false
	}
	return []byte(s), true
}

func isNull(v []byte) bool { return string(v) == "null" }

func isNumber(v []byte) bool { return len(v) > 0 && (v[0] == '-' || v[0] >= '0' && v[0] <= '9') }

// allNumbers reports whether every element is a number (true for none).
func allNumbers(elems [][]byte) bool {
	for _, e := range elems {
		if !isNumber(e) {
			return false
		}
	}
	return true
}

// plainIndex reads v as a choice's or a tool call's "index": a JSON number
// written as a non-negative integer that fits 31 bits; ok=false for anything
// else (a fraction, an exponent, a sign, a string).
func plainIndex(v []byte) (int, bool) {
	n, err := strconv.ParseUint(string(v), 10, 31)
	if err != nil {
		return 0, false
	}
	return int(n), true
}
