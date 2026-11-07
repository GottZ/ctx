//go:build !race

package oai

// G6 of FOLGE-E (briefing FOLGE-E (c)): the parse sizes the inputs before
// it decodes them (decodedLen over strDecoder, booked through cover) and
// then decodes them itself into one allocation (writeDecoded) instead of a
// json.Unmarshal per input. Both runs are linear in the bytes of the input,
// and the parse with both costs at most parseCPURatioMax × the parse before
// FOLGE-E (baseEmbedTop: json.Unmarshal of the whole input, the oracle in
// embed_parse_oracle_test.go) — on the input forms the two runs take apart:
// ASCII (no decode, the raw bytes), runs of invalid bytes (U+FFFD each),
// valid and invalid bytes mixed, escapes of an ASCII letter, lone surrogate
// escapes (U+FFFD each), surrogate pairs, control escapes and literal
// two-byte UTF-8 (the decoder takes a non-ASCII rune through next, rune by
// rune), each as one string and as 255 array elements. The chat parse's CPU
// gate (parse_cpu_gate_test.go) and the embeddings parse's
// (embed_parse_cpu_gate_test.go) cover the keys and one ASCII text at
// 256 KiB; no form of theirs reaches the size run.
//
// Outside -race and coverage builds and with the repeat rule of those gates
// (gateForm, cpuPasses: a form over a bound is measured again, it fails only
// when every pass exceeds, the bounds stay — the reasons are in
// parse_cpu_gate_test.go and at the constants in parse_cpu_test.go). -short
// runs the smaller sizes alone (256 KiB → 1 MiB, 1 MiB against the base);
// a run without it, as the CI's integration job, also 2 MiB → 8 MiB and
// 8 MiB against the base.

import (
	"bytes"
	"fmt"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// embedInputUnit is one input form of the gate: unit is the raw content of a
// JSON string, repeated through every input; decoded is how many bytes
// json.Unmarshal gives one unit.
type embedInputUnit struct {
	name    string
	unit    string
	decoded int
}

// embedInputUnits are the forms the size run and the decode take apart.
func embedInputUnits() []embedInputUnit {
	return []embedInputUnit{
		{"ascii", "a", 1},
		{"ff", "\xff", 3},                                  // an invalid byte: U+FFFD
		{"a_ff", "a\xff", 4},                               // valid and invalid bytes mixed
		{"esc_ascii", escU('A'), 1},                        // read's direct path
		{"lone_surrogate", escU(0xd800), 3},                // unpaired: U+FFFD, through next
		{"surrogate_pair", escU(0xd83d) + escU(0xde00), 4}, // 😀
		{"esc_control", escU(0x01), 1},                     // re-encoded upstream as six bytes
		{"utf8_2byte", "ä", 2},                             // literal UTF-8, rune by rune
	}
}

// embedInputShapes are how many inputs a body of the gate carries: one string
// or an array of 255 (one below the code ceiling of embedding.max_inputs).
var embedInputShapes = []struct {
	name string
	n    int
}{{"string", 1}, {"array255", 255}}

// embedInputRounds is how many rounds a pass of the gate measures (the
// minimum of them counts), as the chat parse's gate.
const embedInputRounds = 7

// embedInputForm is one body of the gate: raws are the raw contents of its
// inputs, slices of body itself (the size run reads the bytes the parse
// reads, not one hot copy of them), decoded is their decoded length in all.
type embedInputForm struct {
	name    string
	body    []byte
	raws    [][]byte
	decoded int
}

// embedInputForms builds every unit in every shape at about size bytes of
// body each.
func embedInputForms(size int) []embedInputForm {
	const head = `{"model":"emb","input":`
	var out []embedInputForm
	for _, sh := range embedInputShapes {
		for _, u := range embedInputUnits() {
			per := (size - len(head) - 3) / sh.n // the brackets and the brace
			content := strings.Repeat(u.unit, max((per-3)/len(u.unit), 1))
			var b bytes.Buffer
			b.Grow(size)
			b.WriteString(head)
			if sh.n > 1 {
				b.WriteByte('[')
			}
			offs := make([]int, 0, sh.n)
			for i := range sh.n {
				if i > 0 {
					b.WriteByte(',')
				}
				b.WriteByte('"')
				offs = append(offs, b.Len())
				b.WriteString(content)
				b.WriteByte('"')
			}
			if sh.n > 1 {
				b.WriteByte(']')
			}
			b.WriteByte('}')
			body := b.Bytes()
			raws := make([][]byte, len(offs))
			for i, o := range offs {
				raws[i] = body[o : o+len(content)]
			}
			out = append(out, embedInputForm{
				name:    sh.name + "_" + u.name,
				body:    body,
				raws:    raws,
				decoded: sh.n * len(content) / len(u.unit) * u.decoded,
			})
		}
	}
	return out
}

// embedInputSink keeps the size run's result live.
var embedInputSink int

// embedInputSizeRun is the parse's size run over the form's inputs.
func embedInputSizeRun(raws [][]byte) int {
	total := 0
	for _, r := range raws {
		total += decodedLen(r)
	}
	return total
}

// embedInputPin holds the form to what it claims before it is timed: the
// parse accepts it with every input, and the size run, the parse's decoded
// bytes and the form's own count agree.
func embedInputPin(t *testing.T, f embedInputForm) {
	t.Helper()
	req, perr := parseEmbedTop(f.body, nil)
	if perr != nil {
		t.Fatalf("form %s (%d B): the parse refuses it: %s", f.name, len(f.body), embedOutcomeOf(req, perr))
	}
	if run := embedInputSizeRun(f.raws); len(req.inputs) != len(f.raws) || req.bytesSum != f.decoded || run != f.decoded {
		t.Fatalf("form %s (%d B): %d inputs of %d decoded bytes, size run %d — the form has %d inputs of %d", f.name, len(f.body), len(req.inputs), req.bytesSum, run, len(f.raws), f.decoded)
	}
}

// embedInputGrowthSizes are the smaller bodies of the linearity gate; each
// form is measured at it and at 4 ×.
func embedInputGrowthSizes() []int {
	if testing.Short() {
		return []int{256 << 10}
	}
	return []int{256 << 10, 2 << 20}
}

// embedInputBaseSizes are the bodies of the gate against the base parse.
func embedInputBaseSizes() []int {
	if testing.Short() {
		return []int{1 << 20}
	}
	return []int{1 << 20, 8 << 20}
}

// TestEmbedInputCPUIsLinear: the size run (decodedLen over every input) and
// the parse that books and decodes after it (parseEmbedTop) take at most
// cpuGrowthMax × as long at 4 × the body as at 1 × — linear 4, quadratic 16.
func TestEmbedInputCPUIsLinear(t *testing.T) {
	skipUnderCoverage(t)
	for _, size := range embedInputGrowthSizes() {
		small, large := embedInputForms(size), embedInputForms(4*size)
		for i, s := range small {
			l := large[i]
			embedInputPin(t, s)
			embedInputPin(t, l)
			gateForm(t, fmt.Sprintf("%s@%d", l.name, len(l.body)), func(pass int) []string {
				runS, runL, parseS, parseL := time.Duration(1<<62), time.Duration(1<<62), time.Duration(1<<62), time.Duration(1<<62)
				for range embedInputRounds {
					runtime.GC() // the parses' strings of the round before are not marked during this one
					t0 := time.Now()
					embedInputSink += embedInputSizeRun(s.raws)
					runS = min(runS, time.Since(t0))
					t0 = time.Now()
					embedInputSink += embedInputSizeRun(l.raws)
					runL = min(runL, time.Since(t0))
					t0 = time.Now()
					_, _ = parseEmbedTop(s.body, nil)
					parseS = min(parseS, time.Since(t0))
					t0 = time.Now()
					_, _ = parseEmbedTop(l.body, nil)
					parseL = min(parseL, time.Since(t0))
				}
				growthRun, growthParse := float64(runL)/float64(max(runS, 1)), float64(parseL)/float64(max(parseS, 1))
				t.Logf("FOLGEE cpu linear form=%s bytes=%d/%d sizerun=%v/%v growth=%.2f parse=%v/%v growth=%.2f pass=%d", l.name, len(s.body), len(l.body), runS, runL, growthRun, parseS, parseL, growthParse, pass)
				var over []string
				if growthRun > cpuGrowthMax {
					over = append(over, fmt.Sprintf("the size run takes %v at %d B and %v at %d B, %.2f × for 4 × the body (at most %d ×: the cost is not linear)", runS, len(s.body), runL, len(l.body), growthRun, cpuGrowthMax))
				}
				if growthParse > cpuGrowthMax {
					over = append(over, fmt.Sprintf("the parse takes %v at %d B and %v at %d B, %.2f × for 4 × the body (at most %d ×: the cost is not linear)", parseS, len(s.body), parseL, len(l.body), growthParse, cpuGrowthMax))
				}
				return over
			})
		}
	}
}

// TestEmbedInputCPUAgainstTheBase: the parse with the size run and its own
// decode takes at most parseCPURatioMax × the parse before FOLGE-E on every
// input form, and both give the same inputs. Each timed parse starts after a
// collection: at 8 MiB the base parse leaves up to some ten times the
// decoded text behind (json.Unmarshal's transients), which would otherwise
// be marked while the next parse is timed.
func TestEmbedInputCPUAgainstTheBase(t *testing.T) {
	skipUnderCoverage(t)
	for _, size := range embedInputBaseSizes() {
		for _, f := range embedInputForms(size) {
			embedInputPin(t, f)
			if d := embedOutcomeDiff(f.body); d != "" {
				t.Fatalf("form %s (%d B): the parses answer differently: %s", f.name, len(f.body), d)
			}
			nr, _ := parseEmbedTop(f.body, nil)
			or, _ := baseEmbedTop(f.body)
			if !slices.Equal(nr.inputs, or.inputs) {
				t.Fatalf("form %s (%d B): the parses decode the inputs differently", f.name, len(f.body))
			}
			gateForm(t, fmt.Sprintf("%s@%d", f.name, len(f.body)), func(pass int) []string {
				mn, mb := time.Duration(1<<62), time.Duration(1<<62)
				for range embedInputRounds {
					runtime.GC()
					t0 := time.Now()
					_, _ = parseEmbedTop(f.body, nil)
					mn = min(mn, time.Since(t0))
					runtime.GC()
					t0 = time.Now()
					_, _ = baseEmbedTop(f.body)
					mb = min(mb, time.Since(t0))
				}
				ratio := float64(mn) / float64(max(mb, 1))
				t.Logf("FOLGEE cpu base form=%s bytes=%d new=%v base=%v ratio=%.2f pass=%d", f.name, len(f.body), mn, mb, ratio, pass)
				if ratio > parseCPURatioMax {
					return []string{fmt.Sprintf("the parse takes %v, %.2f × the base parse's %v (at most %d ×)", mn, ratio, mb, parseCPURatioMax)}
				}
				return nil
			})
		}
	}
}
