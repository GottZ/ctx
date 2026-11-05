package oai

// The CPU gate of the chat parse (FOLGE-P hardening H-6, Fremd-Review 1 B6):
// on no form does the allocation-free parse cost more than parseCPURatioMax ×
// the base parse of 4839a958 (the oracle, parse_oracle_test.go) — measured
// in the same test run, both sides interleaved and the minimum of several
// rounds each, so host load moves both sides alike and the ratio stays.
// The forms are the reviewer's (many keys, long keys sharing a decoded
// prefix of 1 KiB, as escapes and as plain bytes, on the top level and on a
// message) plus every other level that holds a key window (a part, an
// image_url, a hull, logit_bias).

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// parseCPURatioMax bounds the parse cost against the base parse per form.
// Measured on the forms below (bau report FOLGE-P §Härtung H-6): 0.1–1.2
// after H-6 at 256 KiB and at 8 MiB; before it, up to 10 (long escaped
// keys: each key was compared in full against the sixteen of the window).
const parseCPURatioMax = 2

// escU is the JSON escape of r (built at run time: the source holds no
// escape sequence an editor could decode).
func escU(r rune) string { return fmt.Sprintf("%cu%04x", '\\', r) }

// cpuForm is one body of the gate and the limits it runs under.
type cpuForm struct {
	name string
	body []byte
	l    policyLimits
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
		{"typical_2k", []byte(typical.String()), def},
		{"text", []byte(pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf), def},
		{"top_unknown_keys", fill(msg, `}`, short), def},
		{"top_escaped_long_keys", fill(msg, `}`, esc), def},
		{"top_plain_long_keys", fill(msg, `}`, plain), def},
		{"message_escaped_long_keys", fill(`{"model":"chat","messages":[{"role":"user","content":"x"`, `}]}`, esc), def},
		{"part_escaped_long_keys", fill(`{"model":"chat","messages":[{"role":"user","content":[{"type":"text","text":"x"`, `}]}]}`, esc), def},
		{"image_escaped_long_keys", fill(img, `}}]}]}`, esc), vis},
		{"hull_escaped_long_keys", fill(msg+`,"tool_choice":{"type":"function","function":{"name":"f"}`, `}}`, esc), def},
		{"logit_bias_escaped_long_keys", lbKeys(), def},
	}
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

// TestParseCPUAgainstTheBase is the gate at 256 KiB per form (the effect is
// per key, so it shows at this size as at 8 MiB; the 8 MiB numbers are in
// the bau report).
func TestParseCPUAgainstTheBase(t *testing.T) {
	size := 256 << 10
	if v, err := strconv.Atoi(os.Getenv("CTX_FOLGEP_CPU_BYTES")); err == nil && v > 0 {
		size = v // the report's 8 MiB run
	}
	for _, f := range cpuForms(size) {
		// Both parses answer alike (the answer itself is gate 2's: on a part
		// or an image_url the base named one of the refused keys at random).
		n, o := newParse(f.body, f.l), baseParse(f.body, f.l)
		if n.stage != o.stage || n.code != o.code {
			t.Fatalf("form %s: new %s %q, base %s %q", f.name, n.stage, n.code, o.stage, o.code)
		}
		ratio, mn, mb := cpuRatio(f, 7)
		t.Logf("FOLGEP cpu form=%s bytes=%d answer=%s/%s new=%v base=%v ratio=%.2f", f.name, len(f.body), n.stage, n.code, mn, mb, ratio)
		if ratio > parseCPURatioMax {
			t.Errorf("form %s: the parse takes %v, %.1f × the base parse's %v (at most %d ×)", f.name, mn, ratio, mb, parseCPURatioMax)
		}
	}
}
