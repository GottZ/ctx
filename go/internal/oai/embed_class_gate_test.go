package oai

// The class gate of the embeddings route (FOLGE-E (g), after FOLGE-P's
// chat_class_gate_test.go): every key the parse knows — taken from the
// parse's own tables (embedKeys, embedDropKeys), not from a hand list — and
// the element level of input (an array element), each with every value form
// of the chat gate (cgForms) and the forms of this wave: runs of invalid
// UTF-8 bytes mixed with valid ones, \u escapes (letters, surrogate pairs,
// control characters, which the upstream body writes back as six bytes) and
// many equal keys, and — for input, whose policy caps the count — many texts
// of invalid bytes and of control escapes. Two measurements share the bodies:
// the bytes one parse allocates against what the route books before and in
// it (TestEmbedClassGateBytesPerParse, in the suite — the parse does not
// depend on the alias; the route books the JSON check's nesting state before
// it, so the gate does too), and the live heap over the real route with both
// kinds of alias and body logging off and full
// (TestEmbedClassGateOverTheRoute, which runs TestEmbedInvalidBytesHeapGate
// with the form "ecg:<slot>:<value form>[@pub]", one process per run,
// embed_invalid_utf8_mem_test.go). TestEmbedClassGateCoversEveryKeyTable is
// the count pin.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelalias"
)

// ecgSlot is one key of the embeddings parse and the body that carries a
// value to its check: body(model, v) is the request to model, depth the
// containers around v (the root object is one), count the policy's entry
// cap of the value (0: none).
type ecgSlot struct {
	table, key string
	depth      int
	count      int
	body       func(model, v string) string
}

func (s ecgSlot) name() string { return s.table + "." + s.key }

// ecgTop is a request to model with v at top-level key k.
func ecgTop(model, k, v string) string {
	switch k {
	case "model":
		return `{"model":` + v + `,"input":"x"}`
	case "input":
		return `{"model":"` + model + `","input":` + v + `}`
	}
	return `{"model":"` + model + `","input":"x","` + k + `":` + v + `}`
}

// ecgTable is one key table of the parse and its keys.
type ecgTable struct {
	name string
	keys []string
	slot func(k string) ecgSlot
}

// ecgTables are the embeddings parse's key tables with the bodies of their
// keys; "element" is the level below input (an array element).
func ecgTables() []ecgTable {
	top := func(k string) ecgSlot {
		s := ecgSlot{depth: 1, body: func(m, v string) string { return ecgTop(m, k, v) }}
		if k == "input" {
			s.count = modelalias.CeilingEmbeddingMaxInputs
		}
		return s
	}
	return []ecgTable{
		{"embedKeys", cgSorted(embedKeys), top},
		{"embedDropKeys", cgSorted(embedDropKeys), top},
		{"element", []string{"input.*"}, func(string) ecgSlot {
			return ecgSlot{depth: 2, body: func(m, v string) string { return ecgTop(m, "input", `[`+v+`]`) }}
		}},
	}
}

// ecgSlots are the slots of every key of every table.
func ecgSlots() []ecgSlot {
	var out []ecgSlot
	for _, t := range ecgTables() {
		for _, k := range t.keys {
			s := t.slot(k)
			s.table, s.key = t.name, k
			out = append(out, s)
		}
	}
	return out
}

// ecgStrings is a JSON array of count strings of unit that fills n bytes.
func ecgStrings(n, count int, unit string) string {
	count = max(count, 1)
	per := max((n-2)/count-3, len(unit)) / len(unit)
	el := `"` + strings.Repeat(unit, per) + `"`
	k := max(min(count, (n-2)/(len(el)+1)), 1)
	return "[" + strings.TrimSuffix(strings.Repeat(el+",", k), ",") + "]"
}

// ecgForms are the value forms of the gate: the chat gate's nine and the
// forms of this wave.
var ecgForms = append(slices.Clone(cgForms),
	cgForm{"ffmix", func(n, _, _ int) string { return `"` + strings.Repeat("a\xff", max(n-2, 0)/2) + `"` }},
	cgForm{"esc", func(n, _, _ int) string { return `"` + strings.Repeat(`\u0041`, max(n-2, 0)/6) + `"` }},
	cgForm{"pair", func(n, _, _ int) string { return `"` + strings.Repeat(`\ud83d\ude00`, max(n-2, 0)/12) + `"` }},
	cgForm{"ctl", func(n, _, _ int) string { return `"` + strings.Repeat(`\u0001`, max(n-2, 0)/6) + `"` }},
	cgForm{"samekeys", func(n, _, _ int) string {
		return "{" + strings.TrimSuffix(strings.Repeat(`"a":0,`, max(n-2, 6)/6), ",") + "}"
	}},
	cgForm{"manyff", func(n, _, count int) string { return ecgStrings(n, count, "\xff") }},
	cgForm{"manyctl", func(n, _, count int) string { return ecgStrings(n, count, `\u0001`) }},
)

// ecgBody is the request of slot s with value form f at size bytes to model.
func ecgBody(s ecgSlot, f cgForm, model string, size int) []byte {
	n := size - len(s.body(model, ""))
	return []byte(s.body(model, f.gen(max(n, 4), jsonMaxDepth-s.depth, s.count)))
}

// ecgFind is the slot and form of the gate form name "<table>.<key>:<form>".
func ecgFind(name string) (ecgSlot, cgForm, bool) {
	sn, fn, _ := strings.Cut(name, ":")
	for _, s := range ecgSlots() {
		if s.name() != sn {
			continue
		}
		for _, f := range ecgForms {
			if f.name == fn {
				return s, f, true
			}
		}
	}
	return ecgSlot{}, cgForm{}, false
}

// EmbedClassGateBody is the body of the embeddings class-gate form name to
// model at size bytes (the route gate's "ecg:" forms).
func EmbedClassGateBody(name, model string, size int) ([]byte, bool) {
	s, f, ok := ecgFind(name)
	if !ok {
		return nil, false
	}
	return ecgBody(s, f, model, size), true
}

// EmbedClassGateCombos are the gate's combination names
// "<table>.<key>:<form>", every slot with every value form.
func EmbedClassGateCombos() []string {
	var out []string
	for _, s := range ecgSlots() {
		for _, f := range ecgForms {
			out = append(out, s.name()+":"+f.name)
		}
	}
	return out
}

// EmbedClassGateSizes are the body sizes of both measurements: the chat
// gate's three and the cap (the production body cap less the harness's 16
// bytes, as the heap gates send it).
var EmbedClassGateSizes = []int{20100, 65600, 262144, 8<<20 - 16}

// ecgModel is the alias name of the parse-side bodies (the parse does not
// resolve it).
const ecgModel = "embsec"

// TestEmbedClassGateBytesPerParse: every combination of a parse key and a
// value form at every gate size allocates at most parseAllocSlack plus what
// the route books for it — the JSON check's nesting state before the parse
// (jsonDepth × jsonDepthStateBytes) and the decoded inputs inside it (cover).
// -short measures the three small sizes; the cap (96 parses of 8 MiB, three
// times each — past the suite's timeout under -race on a loaded host) runs
// without it, as in the CI's integration job. CTX_EMBED_CLASSGATE_OUT names
// a file for one line per combination.
func TestEmbedClassGateBytesPerParse(t *testing.T) {
	var out strings.Builder
	red := 0
	sizes := EmbedClassGateSizes
	if testing.Short() {
		sizes = sizes[:len(sizes)-1]
	}
	for _, size := range sizes {
		for _, s := range ecgSlots() {
			for _, f := range ecgForms {
				combo := s.name() + ":" + f.name
				body := ecgBody(s, f, ecgModel, size)
				var booked int64
				var perr *paramError
				alloc := allocatedBy(func() {
					booked = jsonDepth(body) * jsonDepthStateBytes
					_, perr = parseEmbedTop(body, func(n int64) bool { booked += n; return true })
				})
				bound := uint64(parseAllocSlack) + uint64(booked)
				verdict := "ok"
				if alloc > bound {
					verdict, red = "RED", red+1
					t.Errorf("%s at %d B: the parse allocates %d bytes, want ≤ %d (slack %d + booked %d)", combo, len(body), alloc, bound, parseAllocSlack, booked)
				}
				answer := "accepted"
				if perr != nil {
					answer = perr.code + "@" + perr.param
				}
				fmt.Fprintf(&out, "ECG size=%d combo=%s body=%d alloc=%d booked=%d bound=%d answer=%s verdict=%s\n", size, combo, len(body), alloc, booked, bound, answer, verdict)
			}
		}
	}
	fmt.Fprintf(&out, "ECG summary slots=%d forms=%d sizes=%d runs=%d red=%d\n", len(ecgSlots()), len(ecgForms), len(sizes), len(ecgSlots())*len(ecgForms)*len(sizes), red)
	t.Logf("FOLGEE class gate parse: slots=%d forms=%d sizes=%d red=%d", len(ecgSlots()), len(ecgForms), len(sizes), red)
	if p := os.Getenv("CTX_EMBED_CLASSGATE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// ecgKeyCount and ecgComboCount pin the size of the gate: a key added to a
// table moves them, a table or key left out of the gate too.
const (
	ecgKeyCount   = 6
	ecgComboCount = ecgKeyCount * 16
)

// TestEmbedClassGateCoversEveryKeyTable (the count pin): the gate names
// every key table of embeddings_parse.go — every package-level var named
// …Keys — and every key of each table (read from the table, not copied),
// and its size is pinned.
func TestEmbedClassGateCoversEveryKeyTable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "embeddings_parse.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	census := map[string]bool{}
	for _, d := range f.Decls {
		g, ok := d.(*ast.GenDecl)
		if !ok || g.Tok != token.VAR {
			continue
		}
		for _, sp := range g.Specs {
			for _, id := range sp.(*ast.ValueSpec).Names {
				if strings.HasSuffix(id.Name, "Keys") {
					census[id.Name] = true
				}
			}
		}
	}
	gate := map[string]bool{}
	for _, tb := range ecgTables() {
		gate[tb.name] = true
	}
	delete(gate, "element")
	for name := range census {
		if !gate[name] {
			t.Errorf("embeddings_parse.go table %s has no class-gate entry (ecgTables)", name)
		}
	}
	for name := range gate {
		if !census[name] {
			t.Errorf("class-gate table %s is not a key table of embeddings_parse.go", name)
		}
	}
	want := map[string][]string{"embedKeys": cgSorted(embedKeys), "embedDropKeys": cgSorted(embedDropKeys)}
	got := map[string][]string{}
	for _, s := range ecgSlots() {
		got[s.table] = append(got[s.table], s.key)
	}
	for tb, keys := range want {
		if g := slices.Sorted(slices.Values(got[tb])); !slices.Equal(g, keys) {
			t.Errorf("class gate, table %s: keys %v, the table has %v", tb, g, keys)
		}
	}
	if n := len(ecgSlots()); n != ecgKeyCount || n*len(ecgForms) != ecgComboCount {
		t.Errorf("class gate: %d keys × %d forms = %d combinations, pinned %d × 16 = %d (a key was added or left out: give it a body, then move the pin)",
			n, len(ecgForms), n*len(ecgForms), ecgKeyCount, ecgComboCount)
	}
	// Every slot reaches its key: some form is accepted or refused at a path
	// that names the key.
	for _, s := range ecgSlots() {
		reached := false
		leaf := strings.TrimSuffix(s.key, ".*")
		for _, f := range ecgForms {
			_, perr := parseEmbedTop(ecgBody(s, f, ecgModel, 4<<10), nil)
			if perr == nil || strings.Contains(perr.param, leaf) {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("class gate: no form of %s reaches the key's check", s.name())
		}
	}
}
