package oai

// The class gate of the rerank routes (W02-10, after FOLGE-E's
// embed_class_gate_test.go and FOLGE-P's chat_class_gate_test.go): every
// key the parse knows — taken from the parse's own tables (rerankKeys,
// rerankDocKeys), not from a hand list — and the element level of documents
// (an array element), each with every value form of the embeddings gate
// (ecgForms: the chat gate's nine and FOLGE-E's seven). The bytes one parse
// allocates are held against what the route books before and in it
// (TestRerankClassGateBytesPerParse: the JSON check's nesting state, then
// the decoded model, query and documents through cover);
// TestRerankClassGateCoversEveryKeyTable is the count pin. The live heap
// over the route with both kinds of alias runs in rerank_gates_test.go
// (TestRerankClassGateOverTheRoute).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelalias"
)

// rcgTop is a request to model with v at top-level key k.
func rcgTop(model, k, v string) string {
	switch k {
	case "model":
		return `{"model":` + v + `,"query":"q","documents":["x"]}`
	case "query":
		return `{"model":"` + model + `","query":` + v + `,"documents":["x"]}`
	case "documents":
		return `{"model":"` + model + `","query":"q","documents":` + v + `}`
	}
	return `{"model":"` + model + `","query":"q","documents":["x"],"` + k + `":` + v + `}`
}

// rcgTables are the rerank parse's key tables with the bodies of their keys;
// "element" is the level below documents (an array element).
func rcgTables() []ecgTable {
	top := func(k string) ecgSlot {
		s := ecgSlot{depth: 1, body: func(m, v string) string { return rcgTop(m, k, v) }}
		if k == "documents" {
			s.count = modelalias.CeilingRerankMaxDocuments
		}
		return s
	}
	doc := func(k string) ecgSlot {
		return ecgSlot{depth: 3, body: func(m, v string) string { return rcgTop(m, "documents", `[{"`+k+`":`+v+`}]`) }}
	}
	return []ecgTable{
		{"rerankKeys", cgSorted(rerankKeys), top},
		{"rerankDocKeys", cgSorted(rerankDocKeys), doc},
		{"element", []string{"documents.*"}, func(string) ecgSlot {
			return ecgSlot{depth: 2, body: func(m, v string) string { return rcgTop(m, "documents", `[`+v+`]`) }}
		}},
	}
}

// rcgSlots are the slots of every key of every table.
func rcgSlots() []ecgSlot {
	var out []ecgSlot
	for _, t := range rcgTables() {
		for _, k := range t.keys {
			s := t.slot(k)
			s.table, s.key = t.name, k
			out = append(out, s)
		}
	}
	return out
}

// rcgFind is the slot and form of the gate form name "<table>.<key>:<form>".
func rcgFind(name string) (ecgSlot, cgForm, bool) {
	sn, fn, _ := strings.Cut(name, ":")
	for _, s := range rcgSlots() {
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

// RerankClassGateBody is the body of the rerank class-gate form name to
// model at size bytes (the route gate's forms).
func RerankClassGateBody(name, model string, size int) ([]byte, bool) {
	s, f, ok := rcgFind(name)
	if !ok {
		return nil, false
	}
	return ecgBody(s, f, model, size), true
}

// RerankClassGateCombos are the gate's combination names
// "<table>.<key>:<form>", every slot with every value form.
func RerankClassGateCombos() []string {
	var out []string
	for _, s := range rcgSlots() {
		for _, f := range ecgForms {
			out = append(out, s.name()+":"+f.name)
		}
	}
	return out
}

// rcgModel is the alias name of the parse-side bodies (the parse does not
// resolve it).
const rcgModel = "rrpub"

// TestRerankClassGateBytesPerParse: every combination of a parse key and a
// value form at every gate size allocates at most parseAllocSlack plus what
// the route books for it — the JSON check's nesting state before the parse
// (jsonDepth × jsonDepthStateBytes) and the decoded texts inside it (cover).
// -short measures the three small sizes; the cap runs without it.
func TestRerankClassGateBytesPerParse(t *testing.T) {
	red := 0
	sizes := EmbedClassGateSizes
	if testing.Short() {
		sizes = sizes[:len(sizes)-1]
	}
	answers := map[string]int{}
	for _, size := range sizes {
		for _, s := range rcgSlots() {
			for _, f := range ecgForms {
				combo := s.name() + ":" + f.name
				body := ecgBody(s, f, rcgModel, size)
				var booked int64
				var perr *paramError
				alloc := allocatedBy(func() {
					booked = jsonDepth(body) * jsonDepthStateBytes
					_, perr = parseRerankTop(body, func(n int64) bool { booked += n; return true })
				})
				bound := uint64(parseAllocSlack) + uint64(booked)
				if alloc > bound {
					red++
					t.Errorf("%s at %d B: the parse allocates %d bytes, want ≤ %d (slack %d + booked %d)", combo, len(body), alloc, bound, parseAllocSlack, booked)
				}
				answer := "accepted"
				if perr != nil {
					answer = perr.code
				}
				answers[answer]++
			}
		}
	}
	t.Logf("W0210 class gate parse: slots=%d forms=%d sizes=%d runs=%d red=%d answers=%v",
		len(rcgSlots()), len(ecgForms), len(sizes), len(rcgSlots())*len(ecgForms)*len(sizes), red, answers)
}

// rcgKeyCount and rcgComboCount pin the size of the gate: a key added to a
// table moves them, a table or key left out of the gate too.
const (
	rcgKeyCount   = 8
	rcgComboCount = rcgKeyCount * 16
)

// TestRerankClassGateCoversEveryKeyTable (the count pin): the gate names
// every key table of rerank_parse.go — every package-level var named …Keys
// — and every key of each table (read from the table, not copied), and its
// size is pinned.
func TestRerankClassGateCoversEveryKeyTable(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "rerank_parse.go", nil, 0)
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
	for _, tb := range rcgTables() {
		gate[tb.name] = true
	}
	delete(gate, "element")
	for name := range census {
		if !gate[name] {
			t.Errorf("rerank_parse.go table %s has no class-gate entry (rcgTables)", name)
		}
	}
	for name := range gate {
		if !census[name] {
			t.Errorf("class-gate table %s is not a key table of rerank_parse.go", name)
		}
	}
	want := map[string][]string{"rerankKeys": cgSorted(rerankKeys), "rerankDocKeys": cgSorted(rerankDocKeys)}
	got := map[string][]string{}
	for _, s := range rcgSlots() {
		got[s.table] = append(got[s.table], s.key)
	}
	for tb, keys := range want {
		if g := slices.Sorted(slices.Values(got[tb])); !slices.Equal(g, keys) {
			t.Errorf("class gate, table %s: keys %v, the table has %v", tb, g, keys)
		}
	}
	if n := len(rcgSlots()); n != rcgKeyCount || n*len(ecgForms) != rcgComboCount {
		t.Errorf("class gate: %d keys × %d forms = %d combinations, pinned %d × 16 = %d (a key was added or left out: give it a body, then move the pin)",
			n, len(ecgForms), n*len(ecgForms), rcgKeyCount, rcgComboCount)
	}
	// Every slot reaches its key: some form is accepted or refused at a path
	// that names the key.
	for _, s := range rcgSlots() {
		reached := false
		leaf := strings.TrimSuffix(s.key, ".*")
		for _, f := range ecgForms {
			_, perr := parseRerankTop(ecgBody(s, f, rcgModel, 4<<10), nil)
			if perr == nil || strings.Contains(perr.param, leaf) {
				reached = true
				break
			}
		}
		if !reached {
			t.Errorf("class gate: no form of %s reaches the key's check", s.name())
		}
	}
	t.Logf("W0210 class gate census: tables=%d slots=%d forms=%d combos=%d", len(census), len(rcgSlots()), len(ecgForms), len(rcgSlots())*len(ecgForms))
}
