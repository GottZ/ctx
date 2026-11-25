// Issue #555, point 8: the NUL guard no longer joins a body into one more
// copy to learn that the escape text it carries is only text
// (chunksMayEscapeNUL). The guard's decision must stay the one the walk over
// the whole body makes: every body the walk finds a NUL in is one the chunk
// scan sends to the walk, at every edge between two chunks and read one byte
// per chunk.
package handler

import (
	"math/rand"
	"strings"
	"testing"
)

// nulEscapeBodies builds JSON bodies (valid and broken) whose strings mix
// backslash runs, the escape's text and its parts, and raw NUL bytes.
func nulEscapeBodies(n int) []string {
	r := rand.New(rand.NewSource(555))
	parts := []string{`\`, `\\`, "u", "0", "0000", "u0000", "x", `\"`, `\u00`, `\u0000`, `\\u0000`, `\\\u0000`, "\x00"}
	str := func() string {
		var b strings.Builder
		for range r.Intn(8) {
			b.WriteString(parts[r.Intn(len(parts))])
		}
		return b.String()
	}
	var out []string
	for range n {
		switch r.Intn(4) {
		case 0:
			out = append(out, `{"a":"`+str()+`","b":["`+str()+`",{"c":"`+str()+`"}]}`)
		case 1:
			out = append(out, `{"`+str()+`":"`+str()+`"}`)
		case 2:
			out = append(out, `["`+str()+`",`+str()+`,"`+str()+`"]`)
		default:
			out = append(out, str()+`"`+str()+`"`+str())
		}
	}
	return out
}

func TestChunksMayEscapeNULCoversTheWalk(t *testing.T) {
	bodies := nulEscapeBodies(4000)
	var found, spared, cases int
	for _, body := range bodies {
		raw := []byte(body)
		_, walk := jsonNULPath(raw, nil)
		splits := [][][]byte{{raw}}
		for i := 1; i < len(raw); i++ {
			splits = append(splits, [][]byte{raw[:i], raw[i:]})
		}
		one := make([][]byte, len(raw))
		for i := range raw {
			one[i] = raw[i : i+1]
		}
		splits = append(splits, one)
		for _, chunks := range splits {
			cases++
			may := chunksMayEscapeNUL(chunks)
			if walk && !may {
				t.Fatalf("the walk finds a NUL in %q, the chunk scan (%d chunks) does not send it to the walk", body, len(chunks))
			}
			if !may {
				spared++
			}
		}
		if walk {
			found++
		}
	}
	t.Logf("%d bodies (%d with a NUL the walk finds), %d chunkings: the chunk scan spared the join in %d", len(bodies), found, cases, spared)
	if found == 0 || spared == 0 {
		t.Fatalf("the generated bodies prove nothing: %d with a NUL, %d spared", found, spared)
	}
}

// TestChunksMayEscapeNULQuotedEscape: the text of the escape after an even
// run of backslashes — written into a string as text — is no NUL, across a
// chunk edge too; after an odd run it is.
func TestChunksMayEscapeNULQuotedEscape(t *testing.T) {
	for _, c := range []struct {
		chunks []string
		want   bool
	}{
		{[]string{`{"c":"\\u0000"}`}, false},
		{[]string{`{"c":"\`, `\u0000"}`}, false},
		{[]string{`{"c":"\\u00`, `00"}`}, false},
		{[]string{`{"c":"\u0000"}`}, true},
		{[]string{`{"c":"\`, `u0000"}`}, true},
		{[]string{`{"c":"\u0`, `000"}`}, true},
		{[]string{`{"c":"\\\`, `u0000"}`}, true},
		{[]string{`{"c":"x`, "\x00", `"}`}, true},
	} {
		chunks := make([][]byte, len(c.chunks))
		for i, s := range c.chunks {
			chunks[i] = []byte(s)
		}
		if got := chunksMayEscapeNUL(chunks); got != c.want {
			t.Errorf("chunksMayEscapeNUL(%q) = %v, want %v", c.chunks, got, c.want)
		}
	}
}
