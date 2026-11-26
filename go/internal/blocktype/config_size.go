package blocktype

import (
	"bytes"
	"encoding/json"
	"io"
	"strconv"
	"strings"
)

// configSize is the size maxConfigBytes caps: the length of the config as the
// database stores it, written compact — the jsonb text without the blank its
// output puts after each ',' and ':'. ONE measure for every path that checks
// the cap (PR #567 review, R2-4): the write paths see the request or a merged
// config, Reload and the tenant-set build read config::text back. Measured by
// length on both, the write measured the compact form and the read the jsonb
// text, so a config of 60 title patterns was written at 16 384 bytes and
// stored as 16 489, and from then on every Reload failed on it; a create could
// do the same at the cap.
//
// The measure is what the database will render, token by token: structure
// and separators compact; a string as jsonb writes it (an escape the request
// used for a printable character counts as the character, a control
// character as its escape); a number as numeric writes it (1e-5 is 0.00001,
// 1.50 stays 1.50). For the jsonb text that Reload reads this is exactly its
// compact length, so no row that loads today fails to load, and a row stored
// between the two measures loads again. Input that is not JSON counts its
// bytes — the decoder refuses it anyway.
func configSize(raw []byte) int {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	type frame struct {
		object bool
		tokens int
	}
	var stack []frame
	size := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			return size
		}
		if err != nil {
			return len(raw)
		}
		if d, ok := tok.(json.Delim); ok && (d == '}' || d == ']') {
			stack = stack[:len(stack)-1]
			size++
			continue
		}
		if n := len(stack); n > 0 {
			f := &stack[n-1]
			switch {
			case f.object && f.tokens%2 == 0: // a key: its comma and colon
				if f.tokens > 0 {
					size++
				}
				size++
			case !f.object && f.tokens > 0: // an array element's comma
				size++
			}
			f.tokens++
		}
		switch v := tok.(type) {
		case json.Delim:
			size++
			stack = append(stack, frame{object: v == '{'})
		case string:
			size += jsonbStringSize(v)
		case json.Number:
			size += numericSize(string(v))
		case bool:
			if v {
				size += len("true")
			} else {
				size += len("false")
			}
		case nil:
			size += len("null")
		}
	}
}

// jsonbStringSize is the length of s as jsonb writes it, quotes included: the
// quote, the backslash and \b \f \n \r \t escaped by two bytes, any other
// control character as \u00XX, everything else as its UTF-8 bytes.
func jsonbStringSize(s string) int {
	size := 2
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c == '"' || c == '\\' || c == '\b' || c == '\f' || c == '\n' || c == '\r' || c == '\t':
			size += 2
		case c < 0x20:
			size += 6
		default:
			size++
		}
	}
	return size
}

// numericSize is the length of the JSON number lit as numeric writes it:
// plain decimal notation, the integer part without leading zeros (at least
// "0"), and as many fractional digits as lit has after its point minus its
// exponent (none below zero); a minus sign only for a value that is not zero.
// An exponent too large to place counts as more than any config may carry.
func numericSize(lit string) int {
	neg := strings.HasPrefix(lit, "-")
	mant, exp := strings.TrimPrefix(lit, "-"), 0
	if i := strings.IndexAny(mant, "eE"); i >= 0 {
		e, err := strconv.Atoi(mant[i+1:])
		if err != nil || e > maxConfigBytes || e < -maxConfigBytes {
			return maxConfigBytes + 1
		}
		mant, exp = mant[:i], e
	}
	intPart, frac, _ := strings.Cut(mant, ".")
	digits := intPart + frac
	point := len(intPart) + exp // digits[:point] is the integer part
	intDigits := 0
	if point > 0 {
		lead := digits[:min(point, len(digits))]
		if trimmed := strings.TrimLeft(lead, "0"); trimmed != "" {
			intDigits = len(trimmed) + max(0, point-len(digits))
		}
	}
	size := max(intDigits, 1)
	if scale := len(frac) - exp; scale > 0 {
		size += 1 + scale
	}
	if neg && strings.Trim(digits, "0") != "" {
		size++
	}
	return size
}
