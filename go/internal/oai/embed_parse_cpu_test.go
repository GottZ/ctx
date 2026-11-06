package oai

// The forms of the embeddings parse's CPU gate (W02-09 rebase round 2, part
// A; embed_parse_cpu_gate_test.go): FOLGE-P's chat forms (parse_cpu_test.go)
// on the one key level the embeddings request has — many keys, long keys
// sharing a prefix of 1 KiB as escapes, plain bytes and literal UTF-8 —
// plus the two shapes the embeddings parse answers that the chat forms do
// not reach: a repeated long key (the repeat is compared in full, cmpKeys)
// and two keys sharing one prefix across the whole body of each unit of
// cpuPrefixUnits, invalid UTF-8 among them (Delta-Review 3 R3-0: hardening 2
// of FOLGE-P made exactly this comparison quadratic; the embeddings parse
// compares with the same cmpKeys since the port). The bounds are FOLGE-P's
// (parseCPURatioMax, parseCPURatioMaxUTF8, cpuGrowthMax), the oracle the
// parse before the port (baseEmbedTop).

import (
	"fmt"
	"strings"
	"time"
)

// embedCPUHead is the start of every form: the two CTX keys a request needs.
const embedCPUHead = `{"model":"emb","input":"x"`

// embedCPUForms builds the gate's forms at about size bytes each.
func embedCPUForms(size int) []cpuForm {
	escPrefix := strings.Repeat(escU('a'), 1024) // 1 KiB decoded, 6 KiB sent
	plainPrefix := strings.Repeat("a", 1024)
	fill := func(key func(i int) string) []byte {
		var b strings.Builder
		b.WriteString(embedCPUHead)
		for i := 0; b.Len() < size-len(key(i))-8; i++ {
			fmt.Fprintf(&b, `,"%s":0`, key(i))
		}
		b.WriteString(`}`)
		return []byte(b.String())
	}
	short := func(i int) string { return fmt.Sprintf("k%07d", i) }
	esc := func(i int) string { return fmt.Sprintf("%s%07d", escPrefix, 9999999-i) }
	plain := func(i int) string { return fmt.Sprintf("%s%07d", plainPrefix, 9999999-i) }
	utf8Key := func(r rune, n int) func(i int) string {
		p := strings.Repeat(string(r), n)
		return func(i int) string { return fmt.Sprintf("%s%07d", p, 9999999-i) }
	}
	// repeat is one key of about half the body, sent twice: the second is
	// compared in full with the first.
	repeat := func(unit string) []byte {
		k := strings.Repeat(unit, max((size-len(embedCPUHead)-16)/2/len(unit), 1))
		return []byte(embedCPUHead + `,"` + k + `":0,"` + k + `":1}`)
	}
	var texts strings.Builder
	texts.WriteString(`{"model":"emb","input":[`)
	for i := range 8 {
		if i > 0 {
			texts.WriteString(",")
		}
		fmt.Fprintf(&texts, "%q", strings.Repeat("lorem ipsum ", 20))
	}
	texts.WriteString(`],"encoding_format":"base64","user":"u"}`)
	pre, suf := `{"model":"emb","input":"`, `"}`
	return []cpuForm{
		{"typical_2k", []byte(texts.String()), policyLimits{}, 0},
		{"text", []byte(pre + strings.Repeat("a", size-len(pre)-len(suf)) + suf), policyLimits{}, 0},
		{"top_unknown_keys", fill(short), policyLimits{}, 0},
		{"top_escaped_long_keys", fill(esc), policyLimits{}, 0},
		{"top_plain_long_keys", fill(plain), policyLimits{}, 0},
		{"top_utf8_2byte_long_keys", fill(utf8Key(0xe9, 512)), policyLimits{}, parseCPURatioMaxUTF8},    // é
		{"top_utf8_3byte_long_keys", fill(utf8Key(0x4e2d, 342)), policyLimits{}, parseCPURatioMaxUTF8},  // 中
		{"top_utf8_4byte_long_keys", fill(utf8Key(0x1f600, 256)), policyLimits{}, parseCPURatioMaxUTF8}, // 😀
		{"repeated_plain_key", repeat("a"), policyLimits{}, 0},
		{"repeated_escaped_key", repeat(escU('a')), policyLimits{}, 0},
		{"repeated_lone_ff_key", repeat("\xff"), policyLimits{}, 0},
		{"repeated_utf8_then_ff_key", repeat("é\xff"), policyLimits{}, parseCPURatioMaxUTF8},
	}
}

// embedPrefixForms are two keys sharing one prefix of a unit across the whole
// body of about size bytes (prefixForms on the embeddings top level): both
// are refused, the smaller is named, so the two are compared through the
// whole prefix.
func embedPrefixForms(size int) []cpuForm {
	var out []cpuForm
	for _, u := range cpuPrefixUnits {
		var bound float64
		if strings.ContainsRune(u.unit, 'é') {
			bound = parseCPURatioMaxUTF8
		}
		p := strings.Repeat(u.unit, max((size-len(embedCPUHead)-16)/2/len(u.unit), 1))
		body := embedCPUHead + `,"` + p + `1":0,"` + p + `2":0}`
		out = append(out, cpuForm{"top_" + u.name, []byte(body), policyLimits{}, bound})
	}
	return out
}

// embedCPURatio is the minimum time of the parse over rounds against the
// base parse's minimum, both interleaved.
func embedCPURatio(body []byte, rounds int) (ratio float64, minNew, minBase time.Duration) {
	minNew, minBase = time.Duration(1<<62), time.Duration(1<<62)
	for range rounds {
		t0 := time.Now()
		_, _ = parseEmbedTop(body, nil)
		minNew = min(minNew, time.Since(t0))
		t0 = time.Now()
		_, _ = baseEmbedTop(body)
		minBase = min(minBase, time.Since(t0))
	}
	return float64(minNew) / float64(max(minBase, 1)), minNew, minBase
}
