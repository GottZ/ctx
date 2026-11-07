package oai

// The CPU gate of the chat parse (FOLGE-P hardening H-6, Fremd-Review 1 B6):
// on no form does the allocation-free parse cost more than parseCPURatioMax ×
// the base parse of 4839a958 (the oracle, parse_oracle_test.go) — measured
// in the same test run, both sides interleaved and the minimum of several
// rounds each, so host load moves both sides alike and the ratio stays.
// The forms are the reviewer's (many keys, long keys sharing a decoded
// prefix of 1 KiB, as escapes and as plain bytes, on the top level and on a
// message) plus every other level that holds a key window (a part, an
// image_url, a hull, logit_bias), and long keys of literal UTF-8 with two-,
// three- and four-byte runes (Delta-Review 2 F-3). The prefix forms
// (Delta-Review 3 R3-0) are two keys sharing one prefix across the whole
// body: their gate is the growth of the cost with the body, not only the
// ratio.

import (
	"fmt"
	"strings"
	"time"
)

// parseCPURatioMax bounds the parse cost against the base parse per form.
// Measured on the forms below (bau report FOLGE-P §Härtung H-6, §Härtung 3):
// at most 0.75 on keys of ASCII and escapes after H-6, at 256 KiB and at
// 8 MiB — before it up to 10 (long escaped keys: each key was compared in
// full against the sixteen of the window). Beside ten busy loops on 12 cores
// at most 1.00 (Delta-Review 4) and 0.92 (hardening 4, 1 000 measurements);
// a form over its bound is measured again (cpuPasses).
const parseCPURatioMax = 2

// parseCPURatioMaxUTF8 bounds the forms whose keys share a prefix of literal
// UTF-8: the byte decoder takes such a key rune by rune (strDecoder.next
// decodes and re-encodes each, sameTokens decodes each on both sides), the
// base parse decodes it at once. Measured 2.16–2.90 × the base parse at
// 256 KiB on a quiet host (bau report FOLGE-P §Härtung 3, 30 runs), 2.0–3.4
// in Delta-Review 2 (8 MiB: 110–238 ms against 38–180 ms). Hardening 2 had
// copied and compared runs of valid UTF-8 at once (0.63–1.00), but the run's
// backslash scan restarted at every invalid byte and made the comparison
// quadratic in a prefix of invalid bytes two keys share (Delta-Review 3
// R3-0: 11 min for one 8 MiB request); that path is withdrawn, this bound is
// the cost of the one that stands. Beside ten busy loops on 12 cores single
// measurements reached 4.91 (Delta-Review 4 R4-1) and 4.56 (hardening 4,
// 2 of 301 over 4): a form over the bound is measured again (cpuPasses)
// rather than the bound raised.
const parseCPURatioMaxUTF8 = 4

// cpuGrowthMax bounds the cost of a prefix form at 4 × the body against the
// same form at 1 × (the minimum of the rounds each): a linear cost grows 4 ×,
// a quadratic one 16 × — 8 halves either way. Measured (bau report FOLGE-P
// §Härtung 3): 2.71–4.82 on every prefix form, 1 080 measurements; 17.2–18.2
// on the forms of invalid bytes with hardening 2's run copy, 15.7–15.8 on
// those of é before 0xff with an early exit at an invalid first rune added
// to it (the reviewer's partial fix). Under load single measurements of the
// linear cost pass it: beside ten busy loops on 12 cores up to 14.89
// (Delta-Review 4 R4-1) and 12.37 (hardening 4, 10 of 1 811 over 8, the
// valid é forms among them), in the CI form at a host load of about 45 9.9
// (message_lone_ff, Delta-Review 4); against the base parse the prefix
// forms of invalid bytes reached 2.02 and 2.52 there (bound 2). A form over
// a bound is measured again (cpuPasses).
const cpuGrowthMax = 8

// cpuPasses is how many passes a CPU gate gives a form at most (Delta-Review
// 4 R4-1, gateForm): a form that exceeds a bound is measured again, up to
// two more passes with the same rounds, and fails the gate only when every
// pass exceeds — the bounds above stay, and no pass is averaged with another.
// Load inflates single passes: beside ten busy loops 3 of 150 gate runs
// were red without it, none with it, 6 runs measured a form again and 2
// forms needed the third pass (hardening 4). A cost that is not linear goes
// over in each pass: the quadratic copy (growth 12.4–23.1 under that load,
// Delta-Review 4), and a backslash scan over a 32nd of the rest at every
// invalid byte — growth 5.6–6.4 and so caught by the base bound on the
// top-level forms — stayed red in 10 of 10 runs quiet and loaded alike.
const cpuPasses = 3

// cpuPrefixBytes is the smaller body of the prefix forms; the gate parses
// each at it and at 4 ×.
const cpuPrefixBytes = 64 << 10

// escU is the JSON escape of r (built at run time: the source holds no
// escape sequence an editor could decode).
func escU(r rune) string { return fmt.Sprintf("%cu%04x", '\\', r) }

// cpuForm is one body of the gate and the limits it runs under; ratioMax
// is its bound against the base parse (0: parseCPURatioMax).
type cpuForm struct {
	name     string
	body     []byte
	l        policyLimits
	ratioMax float64
}

// bound is the form's bound against the base parse.
func (f cpuForm) bound() float64 {
	if f.ratioMax > 0 {
		return f.ratioMax
	}
	return parseCPURatioMax
}

// cpuForms builds the gate's forms at about size bytes each.
func cpuForms(size int) []cpuForm {
	msg := `{"model":"chat","messages":[{"role":"user","content":"x"}]`
	escPrefix := strings.Repeat(escU('a'), 1024) // 1 KiB decoded, 6 KiB sent
	plainPrefix := strings.Repeat("a", 1024)
	fill := func(prefix, suffix string, key func(i int) string) []byte {
		var b strings.Builder
		b.WriteString(prefix)
		for i := 0; b.Len() < size-len(suffix)-len(key(i))-8; i++ {
			fmt.Fprintf(&b, `,"%s":0`, key(i))
		}
		b.WriteString(suffix)
		return []byte(b.String())
	}
	short := func(i int) string { return fmt.Sprintf("k%07d", i) }
	esc := func(i int) string { return fmt.Sprintf("%s%07d", escPrefix, 9999999-i) }
	plain := func(i int) string { return fmt.Sprintf("%s%07d", plainPrefix, 9999999-i) }
	// Delta-Review 2 F-3: long keys sharing a literal UTF-8 prefix of about
	// 1 KiB — two-, three- and four-byte runes.
	utf8Key := func(r rune, n int) func(i int) string {
		p := strings.Repeat(string(r), n)
		return func(i int) string { return fmt.Sprintf("%s%07d", p, 9999999-i) }
	}
	lbKeys := func() []byte {
		// logit_bias at its cap (64): every key repeat is searched among
		// all of them, the keys long and alike.
		klen := max(size/64/6-8, 1)
		p := strings.Repeat(escU('1'), klen)
		var b strings.Builder
		b.WriteString(msg + `,"logit_bias":{`)
		for i := range 64 {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"%s%02d":0`, p, i)
		}
		b.WriteString(`}}`)
		return []byte(b.String())
	}
	def, vis := diffLimits[0], diffLimits[1]
	vis.visionMaxBytes = 1024
	img := `{"model":"vis","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"`
	var typical strings.Builder
	typical.WriteString(`{"model":"chat","temperature":0.7,"messages":[`)
	for i, role := range []string{"system", "user", "assistant", "user"} {
		if i > 0 {
			typical.WriteString(",")
		}
		fmt.Fprintf(&typical, `{"role":%q,"content":%q}`, role, strings.Repeat("lorem ipsum ", 40))
	}
	typical.WriteString(`]}`)
	pre, suf := `{"model":"chat","messages":[{"role":"user","content":"`, `"}]}`
	return []cpuForm{
		{"typical_2k", []byte(typical.String()), def, 0},
		{"text", []byte(pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf), def, 0},
		{"top_unknown_keys", fill(msg, `}`, short), def, 0},
		{"top_escaped_long_keys", fill(msg, `}`, esc), def, 0},
		{"top_plain_long_keys", fill(msg, `}`, plain), def, 0},
		{"top_utf8_2byte_long_keys", fill(msg, `}`, utf8Key(0xe9, 512)), def, parseCPURatioMaxUTF8},    // é
		{"top_utf8_3byte_long_keys", fill(msg, `}`, utf8Key(0x4e2d, 342)), def, parseCPURatioMaxUTF8},  // 中
		{"top_utf8_4byte_long_keys", fill(msg, `}`, utf8Key(0x1f600, 256)), def, parseCPURatioMaxUTF8}, // 😀
		{"message_escaped_long_keys", fill(`{"model":"chat","messages":[{"role":"user","content":"x"`, `}]}`, esc), def, 0},
		{"part_escaped_long_keys", fill(`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x"`, `}]}]}`, esc), def, 0},
		{"image_escaped_long_keys", fill(img, `}}]}]}`, esc), vis, 0},
		{"hull_escaped_long_keys", fill(msg+`,"tool_choice":{"type":"function","function":{"name":"f"}`, `}}`, esc), def, 0},
		{"logit_bias_escaped_long_keys", lbKeys(), def, 0},
	}
}

// cpuPrefixUnits are the units the prefix forms repeat (Delta-Review 3
// R3-0, R3-0b): invalid UTF-8 — a lone 0xff, a stray continuation byte
// after a plain one, an invalid byte every third, a valid two-byte rune
// before an invalid byte, a raw surrogate (ED A0 80, invalid as UTF-8) — and
// valid é beside them.
var cpuPrefixUnits = []struct{ name, unit string }{
	{"lone_ff", "\xff"},
	{"stray_continuation", "a\x80"},
	{"ff_every_third", "ab\xff"},
	{"utf8_then_ff", "é\xff"},
	{"raw_surrogate", "\xed\xa0\x80"},
	{"utf8_2byte", "é"},
}

// prefixForms are two keys sharing one prefix of a unit across the whole
// body of about size bytes, on the top level, on a message and on a part:
// both keys are refused and compared with each other through the whole
// prefix (cmpKeys: the smaller refused key is the one named), so a cost per
// shared byte that grows with the rest of the prefix makes the parse
// quadratic in the body. The forms with a valid rune in their unit carry
// the UTF-8 bound.
func prefixForms(size int) []cpuForm {
	levels := []struct{ name, pre, suf string }{
		{"top", `{"model":"chat","messages":[{"role":"user","content":"x"}],`, `}`},
		{"message", `{"model":"chat","messages":[{"role":"user","content":"x",`, `}]}`},
		{"part", `{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x",`, `}]}]}`},
	}
	var out []cpuForm
	for _, u := range cpuPrefixUnits {
		var bound float64
		if strings.ContainsRune(u.unit, 'é') {
			bound = parseCPURatioMaxUTF8
		}
		for _, lv := range levels {
			p := strings.Repeat(u.unit, max((size-len(lv.pre)-len(lv.suf)-12)/2/len(u.unit), 1))
			body := lv.pre + `"` + p + `1":0,"` + p + `2":0` + lv.suf
			out = append(out, cpuForm{lv.name + "_" + u.name, []byte(body), diffLimits[0], bound})
		}
	}
	return out
}

// cpuRatio is the minimum time of the parse over rounds against the base
// parse's minimum, both interleaved.
func cpuRatio(f cpuForm, rounds int) (ratio float64, minNew, minBase time.Duration) {
	minNew, minBase = time.Duration(1<<62), time.Duration(1<<62)
	for range rounds {
		t0 := time.Now()
		_ = jsonStateBytes(f.body) // the route books the nesting state before the parse
		_ = newParse(f.body, f.l)
		minNew = min(minNew, time.Since(t0))
		t0 = time.Now()
		_ = baseParse(f.body, f.l)
		minBase = min(minBase, time.Since(t0))
	}
	return float64(minNew) / float64(max(minBase, 1)), minNew, minBase
}
