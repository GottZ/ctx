package oai

// The differential of the embeddings parse (W02-09 rebase round 2, part A):
// the parse after the port onto FOLGE-P's key helpers answers every body as
// the parse before it (baseEmbedTop, embed_parse_oracle_test.go) — the same
// request, or the same refusal (code, param, message) — with one enumerated
// deviation:
//
//   - echoClipped: a refusal that names a client's key names it cut to
//     echoNameMax bytes and "…" (FOLGE-P H-9, decodeKey); the parse before
//     the port named the whole decoded key, of up to the body cap.
//   - emptyKeyRepeatInWindow: a body of more than five members whose first
//     repeat among its first six is the empty key "" is refused as that
//     repeat (invalid_json, param ""). The parse before the port kept the
//     window's first repeat in a string whose empty value meant "none", so
//     it missed exactly this repeat and named the smallest unknown key or a
//     later repeat — against its own order (a repeat first) and against its
//     path of at most five members, which named the repeated "" already.
//
// The corpus is the chat parse's (every body the chat route tests post, the
// generated probe forms, the smuggle seeds — as sent, and with messages
// renamed to input and the model to an embeddings alias's name) plus an
// enumeration of the embeddings top level: one to seven members drawn from
// the CTX, DROP, case-variant, escaped, invalid-UTF-8, long and unknown keys
// with values of every kind.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
)

// embedOutcomeOf names an answer of the parse for a log line.
func embedOutcomeOf(req *embedRequest, perr *paramError) string {
	if perr != nil {
		p := perr.param
		if len(p) > 40 {
			p = fmt.Sprintf("%.40s…(%d B)", p, len(p))
		}
		return perr.code + "/" + p
	}
	return "ok"
}

// embedOutcomeDiff is "" when both parses answer body with the same code
// (both refuse with one code, or both accept) — the CPU gate's check that it
// times like answers.
func embedOutcomeDiff(body []byte) string {
	nr, np := parseEmbedTop(body)
	or, op := baseEmbedTop(body)
	nc, oc := "ok", "ok"
	if np != nil {
		nc = np.code
	}
	if op != nil {
		oc = op.code
	}
	if nc != oc {
		return fmt.Sprintf("new %s, base %s", embedOutcomeOf(nr, np), embedOutcomeOf(or, op))
	}
	return ""
}

// clipEchoed is the name rule of decodeKey on a decoded name: up to
// echoNameMax bytes as they are, else cut at a rune boundary and "…".
func clipEchoed(s string) string {
	if len(s) <= echoNameMax {
		return s
	}
	cut := echoNameMax
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// embedDeviation compares the answers of the two parses: "" alike, the name
// of an allowed deviation, or "unallowed: …".
func embedDeviation(body []byte) string {
	nr, np := parseEmbedTop(body)
	or, op := baseEmbedTop(body)
	switch {
	case np == nil && op == nil:
		if reflect.DeepEqual(nr, or) {
			return ""
		}
		return fmt.Sprintf("unallowed: both accept, new %+v, base %+v", *nr, *or)
	case np == nil || op == nil:
		return fmt.Sprintf("unallowed: new %s, base %s", embedOutcomeOf(nr, np), embedOutcomeOf(or, op))
	case *np == *op:
		return ""
	}
	var clipped *paramError
	switch op.code {
	case CodeUnsupportedParameter:
		clipped = unsupportedParameter(clipEchoed(op.param))
	case CodeInvalidJSON:
		if op.param != "" {
			clipped = repeated("", clipEchoed(op.param))
		}
	}
	if clipped != nil && *clipped == *np && len(op.param) > echoNameMax {
		return "echoClipped"
	}
	if *np == *repeated("", "") && baseObjectMembers(body) > len(embedNames) {
		if k, ok := windowFirstRepeat(body); ok && k == "" {
			return "emptyKeyRepeatInWindow"
		}
	}
	return fmt.Sprintf("unallowed: new %s %q %.200q, base %s %.200q %.200q", np.code, np.param, np.message, op.code, op.param, op.message)
}

// windowFirstRepeat is the first key among the first six members of body
// (decoded as encoding/json decodes it) that repeats an earlier one.
func windowFirstRepeat(body []byte) (string, bool) {
	it, _ := baseNewMemberIter(body)
	seen := map[string]bool{}
	for range len(embedNames) + 1 {
		key, _, more := it.next()
		if !more {
			break
		}
		var k string
		if err := json.Unmarshal(append(append([]byte{'"'}, key...), '"'), &k); err != nil {
			return "", false
		}
		if seen[k] {
			return k, true
		}
		seen[k] = true
	}
	return "", false
}

// embedDiffCorpus is the corpus of the differential (see the file comment).
func embedDiffCorpus(t testing.TB) [][]byte {
	t.Helper()
	var out [][]byte
	chat := diffCorpus(t)
	for _, s := range rvSmuggleSeeds() {
		chat = append(chat, []byte(s))
	}
	for _, b := range chat {
		out = append(out, b)
		r := bytes.ReplaceAll(b, []byte(`"messages"`), []byte(`"input"`))
		r = bytes.ReplaceAll(r, []byte(`"model":"chat"`), []byte(`"model":"emb"`))
		out = append(out, r)
	}
	return append(out, generatedEmbedCorpus()...)
}

// generatedEmbedCorpus enumerates the embeddings top level (seeded, so the
// corpus is the same in every run).
func generatedEmbedCorpus() [][]byte {
	esc := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			b.WriteString(escU(r))
		}
		return b.String()
	}
	long := strings.Repeat("k", 200)
	keys := []string{
		"model", "input", "encoding_format", "dimensions", "user",
		"Model", "INPUT", "User", "encoding_Format",
		esc("model"), "mod" + esc("e") + "l", "in" + esc("p") + "ut", "us" + esc("e") + "r",
		"\xff", "\xfe", "é", "\xc3", "a\x80", "\xed\xa0\x80", escU(0xd800),
		"zz", "a", "", "user ", long, long + "x", esc(long), strings.Repeat("é", 100), strings.Repeat("\xff", 100), strings.Repeat("\xff", 101),
	}
	vals := []string{
		`"emb"`, `"x"`, `["a","b"]`, `[1,2]`, `[[1]]`, `3`, `null`, `"float"`, `"base64"`, `""`, `[]`, `[""]`, `{"a":1}`, `true`, `-1`, `0`, `1.5`, `"FLOAT"`, `[" ","\u0000"]`,
	}
	rng := rand.New(rand.NewPCG(20261106, 9))
	var out [][]byte
	for range 4000 {
		n := 1 + rng.IntN(7)
		var b strings.Builder
		b.WriteString("{")
		for j := range n {
			if j > 0 {
				b.WriteString(",")
			}
			k := keys[rng.IntN(len(keys))]
			if j < 2 && rng.IntN(3) > 0 { // most bodies carry the two keys a request needs
				k = []string{"model", "input"}[j]
			}
			fmt.Fprintf(&b, `"%s":%s`, k, vals[rng.IntN(len(vals))])
		}
		b.WriteString("}")
		out = append(out, []byte(b.String()))
	}
	// The window of more than five members: a repeat or an unknown key
	// inside and past the first six.
	for _, s := range []string{
		`{"model":"emb","input":"x","a":1,"b":2,"c":3,"d":4,"model":"e"}`,
		`{"model":"emb","input":"x","encoding_format":"float","dimensions":4,"user":"u","model":"e"}`,
		`{"model":"emb","input":"x","encoding_format":"float","dimensions":4,"user":"u","zz":1,"a":2}`,
		`{"zz":1,"model":"emb","input":"x","encoding_format":"float","dimensions":4,"user":"u","a":2}`,
		`{"model":"emb","model":"x","a":1,"b":2,"c":3,"d":4,"e":5}`,
		`{"model":"emb","input":"x","` + strings.Repeat("<", 300) + `":1,"b":2,"c":3,"d":4,"e":5}`,
		`{"model":"emb","input":"x","` + strings.Repeat("<", 300) + `":1,"` + strings.Repeat("<", 300) + `":2}`,
		`{"model":"emb","input":"x","` + strings.Repeat("é", 40) + `é":1,"` + strings.Repeat("é", 41) + `":2}`,
	} {
		out = append(out, []byte(s))
	}
	return out
}

func TestEmbedParseDifferential(t *testing.T) {
	devs := map[string]int{}
	corpus := embedDiffCorpus(t)
	for _, body := range corpus {
		d := embedDeviation(body)
		if strings.HasPrefix(d, "unallowed") {
			t.Errorf("body %.300q: %s", body, d)
			continue
		}
		if d == "" {
			d = "equal"
		}
		devs[d]++
	}
	t.Logf("W0209 differential: %d bodies, %v", len(corpus), devs)
}

func FuzzEmbedParseDifferential(f *testing.F) {
	for _, b := range embedDiffCorpus(f) {
		if len(b) < 4096 {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if !json.Valid(body) {
			return
		}
		if d := embedDeviation(body); strings.HasPrefix(d, "unallowed") {
			t.Errorf("body %.300q: %s", body, d)
		}
	})
}
