package oai

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
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
// Cost (bau report W02-07 §Härtung nach Review 2, H-2): the scans below run
// over bytes json.Unmarshal already validated and allocate nothing; only an
// object whose keys need decoding (an escape, a non-ASCII byte) or a hull
// with more than hullPairwiseMax keys takes the decoding path.

// memberIter walks the members of one JSON object without decoding or
// allocating. raw must be valid JSON (json.Unmarshal accepted it); a shape
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

// repeatedKey names the first member name of the object raw that occurs
// twice after decoding — the error path only, so it may allocate.
func repeatedKey(raw []byte) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return ""
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return ""
		}
		k, _ := tok.(string)
		if seen[k] {
			return k
		}
		seen[k] = true
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return ""
		}
	}
	return ""
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

// uniqueMembers refuses an object whose map form m (decoded from raw) lost a
// member to a repeated key.
func uniqueMembers(at string, raw []byte, m map[string]json.RawMessage) *paramError {
	if objectMembers(raw) == len(m) {
		return nil
	}
	return repeated(at, repeatedKey(raw))
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
				return true, repeated(levelPath(at, idx), string(k))
			}
		}
		seen[n] = k
		n++
		for i, kn := range known {
			switch {
			case string(k) == kn:
				vals[i] = v
			case asciiEqualFold(k, kn):
				return true, unsupportedParameter(keyPath(levelPath(at, idx), string(k)))
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

// checkHullDecoded is checkHull over the decoded map: keys with escapes or
// non-ASCII bytes (Unicode folding: "ſ" folds to "s"), or many keys.
func checkHullDecoded(at string, raw []byte, known []string, vals [][]byte) *paramError {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil
	}
	if perr := uniqueMembers(at, raw, m); perr != nil {
		return perr
	}
	for k := range m {
		for i, kn := range known {
			switch {
			case k == kn:
				vals[i] = m[k]
			case strings.EqualFold(k, kn):
				return unsupportedParameter(keyPath(at, k))
			}
		}
	}
	return nil
}

// needsDecode reports a key the byte comparison cannot judge: an escape or
// a non-ASCII byte.
func needsDecode(k []byte) bool {
	for _, c := range k {
		if c == '\\' || c >= 0x80 {
			return true
		}
	}
	return false
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
