package oai

// The class gate of FOLGE-P (hardening 6, Delta-Review 5): five reviews found
// the same class place by place — a client value decoded or copied outside
// the reservation (model, response_format.type, stream). This gate checks
// the class as a whole: every key the parse knows — taken from the parse's
// own tables, not from a hand list — with every hostile value form, each as
// large as the cap that holds for that key (the body, or the alias policy's
// ceiling for a capped value). Three measurements share the bodies, each
// for both kinds of alias — one at credentials (no sensitivity scan) and one
// below it (the scan runs; hardening 7: the scan side was a special run and
// held what the gate did not see): the bytes one parse allocates
// (TestClassGateBytesPerParse, in the suite; the parse does not depend on
// the alias's sensitivity), the bytes the scan allocates against what the
// route books for it (TestClassGateBytesPerScan, chat_class_gate_scan_test.go,
// in every build without -race) and the live heap over the real route at
// small bodies with both aliases (TestClassGateOverTheRoute, which runs
// TestChatParseHeapGate with the form "cg:<slot>:<value form>[@pub]" one
// process per run, chat_parse_mem_test.go, a measurement test outside the
// suite). TestClassGateCoversEveryKeyTable is the count pin: a new key
// table, a key added to one, or a key left out of the gate turns it red.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/modelalias"
)

// cgModel is the alias of every class-gate body: the policy ceilings (the
// largest value a capped key admits) and vision, so an image part reaches
// its check (the route's rig registers it as "ceil").
const cgModel = "ceil"

// cgLimits are the policy ceilings (modelalias.Ceiling*): the alias policy
// of cgModel.
var cgLimits = policyLimits{
	toolsMaxCount: modelalias.CeilingToolsMaxCount, toolsMaxBytes: modelalias.CeilingToolsMaxBytes,
	responseFormatMaxBytes: modelalias.CeilingResponseFormatMaxBytes, logitBiasMaxEntries: modelalias.CeilingLogitBiasMaxEntries,
	stopMaxEntries: modelalias.CeilingStopMaxEntries, vision: true, visionMaxBytes: modelalias.CeilingVisionMaxBytes,
}

// CGCeilPolicy is cgLimits as the policy JSON of an alias row.
var CGCeilPolicy = fmt.Sprintf(`{"vision":true,"vision_max_bytes":%d,"tools.max_count":%d,"tools.max_bytes":%d,`+
	`"response_format.max_bytes":%d,"logit_bias.max_entries":%d,"stop.max_entries":%d}`,
	modelalias.CeilingVisionMaxBytes, modelalias.CeilingToolsMaxCount, modelalias.CeilingToolsMaxBytes,
	modelalias.CeilingResponseFormatMaxBytes, modelalias.CeilingLogitBiasMaxEntries, modelalias.CeilingStopMaxEntries)

// cgSlot is one key of a parse table and the body that carries a value to
// its check: body(v) is the request, capped(v) the policy-capped top-level
// value around v (nil when only the body cap holds), depth the containers
// around v (the root object is one), countOf the policy's entry cap of the
// value (tools, stop, logit_bias: the many-values forms hold exactly that
// many entries, the most that passes the count and reaches the next check).
type cgSlot struct {
	table, key string
	depth      int
	body       func(v string) string
	capped     func(v string) string
	capOf      func(l policyLimits) int
	countOf    func(l policyLimits) int
}

func (s cgSlot) name() string { return s.table + "." + s.key }

const cgMessages = `[{"role":"user","content":"x"}]`

// cgTop is a request with v at top-level key k.
func cgTop(k, v string) string {
	switch k {
	case "model":
		return `{"model":` + v + `,"messages":` + cgMessages + `}`
	case "messages":
		return `{"model":"` + cgModel + `","messages":` + v + `}`
	}
	return `{"model":"` + cgModel + `","messages":` + cgMessages + `,"` + k + `":` + v + `}`
}

// cgMessage is a message with v at key k, in the role that admits k.
func cgMessage(k, v string) string {
	switch k {
	case "role":
		return `{"role":` + v + `,"content":"x"}`
	case "content":
		return `{"role":"user","content":` + v + `}`
	case "tool_calls":
		return `{"role":"assistant","content":"x","tool_calls":` + v + `}`
	case "tool_call_id":
		return `{"role":"tool","content":"x","tool_call_id":` + v + `}`
	}
	return `{"role":"user","content":"x","` + k + `":` + v + `}`
}

// cgPart is a content part with v at key k.
func cgPart(k, v string) string {
	switch k {
	case "type":
		return `{"type":` + v + `,"text":"x"}`
	case "text":
		return `{"type":"text","text":` + v + `}`
	case "image_url":
		return `{"type":"image_url","image_url":` + v + `}`
	}
	return `{"type":"text","text":"x","` + k + `":` + v + `}`
}

func cgInParts(part string) string {
	return cgTop("messages", `[{"role":"user","content":[`+part+`]}]`)
}

// cgHull is a hull object with v at key k; the other known keys hold
// valid values (vals), absent when k replaces them.
func cgHull(known []string, valid map[string]string, k, v string) string {
	var b strings.Builder
	b.WriteByte('{')
	for _, kn := range known {
		if kn == k || valid[kn] == "" {
			continue
		}
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + kn + `":` + valid[kn])
	}
	if b.Len() > 1 {
		b.WriteByte(',')
	}
	b.WriteString(`"` + k + `":` + v + `}`)
	return b.String()
}

// cgTable is one key table of the parse (TestClassGateCoversEveryKeyTable
// names every one of parse.go) and the slots of its keys.
type cgTable struct {
	name string
	keys []string
	slot func(k string) cgSlot
}

func cgSorted[V any](m map[string]V) []string {
	ks := make([]string, 0, len(m))
	for k := range m {
		ks = append(ks, k)
	}
	slices.Sort(ks)
	return ks
}

func cgNamesOf(index map[string]int) []string { return cgSorted(index) }

// cgTables are the parse's key tables with the bodies of their keys. A free
// member (a key no table names: inside stream_options, reasoning, a
// logit_bias entry, a stop element) is the key "*".
func cgTables() []cgTable {
	top := func(k string) cgSlot {
		s := cgSlot{depth: 1, body: func(v string) string { return cgTop(k, v) }}
		switch k {
		case "tools":
			s.capped, s.capOf = func(v string) string { return v }, func(l policyLimits) int { return l.toolsMaxBytes }
			s.countOf = func(l policyLimits) int { return l.toolsMaxCount }
		case "stop":
			s.countOf = func(l policyLimits) int { return l.stopMaxEntries }
		case "logit_bias":
			s.countOf = func(l policyLimits) int { return l.logitBiasMaxEntries }
		case "response_format":
			s.capped, s.capOf = func(v string) string { return v }, func(l policyLimits) int { return l.responseFormatMaxBytes }
		}
		return s
	}
	rf := func(v string) string { return cgTop("response_format", v) }
	rfValid := map[string]string{"type": `"json_schema"`, "json_schema": `{"name":"s"}`}
	jsValid := map[string]string{"name": `"s"`}
	toolValid := map[string]string{"type": `"function"`, "function": `{"name":"f"}`}
	capRF := func(l policyLimits) int { return l.responseFormatMaxBytes }
	capTools := func(l policyLimits) int { return l.toolsMaxBytes }
	free := func(k string, depth int, wrap func(v string) string) cgSlot {
		return cgSlot{depth: depth, body: func(v string) string { return cgTop(k, wrap(v)) }}
	}
	return []cgTable{
		{"passKeys", cgSorted(passKeys), top},
		{"ctxKeys", cgSorted(ctxKeys), top},
		{"dropKeys", cgSorted(dropKeys), top},
		{"messagePassKeys", cgSorted(messagePassKeys), nil},
		{"messageDropKeys", cgSorted(messageDropKeys), nil},
		{"partIndex", cgNamesOf(partIndex), func(k string) cgSlot {
			return cgSlot{depth: 5, body: func(v string) string { return cgInParts(cgPart(k, v)) }}
		}},
		{"imageIndex", cgNamesOf(imageIndex), func(k string) cgSlot {
			valid := map[string]string{"url": `"data:image/png;base64,AAAA"`}
			return cgSlot{depth: 6, body: func(v string) string { return cgInParts(cgPart("image_url", cgHull(imageNames(), valid, k, v))) }}
		}},
		{"responseFormatKeys", responseFormatKeys, func(k string) cgSlot {
			h := func(v string) string { return cgHull(responseFormatKeys, rfValid, k, v) }
			return cgSlot{depth: 2, body: func(v string) string { return rf(h(v)) }, capped: h, capOf: capRF}
		}},
		{"responseFormatJSONKeys", responseFormatJSONKeys, func(k string) cgSlot {
			h := func(v string) string {
				return `{"type":"json_schema","json_schema":` + cgHull(responseFormatJSONKeys, jsValid, k, v) + `}`
			}
			return cgSlot{depth: 3, body: func(v string) string { return rf(h(v)) }, capped: h, capOf: capRF}
		}},
		{"toolHullKeys/tools", toolHullKeys, func(k string) cgSlot {
			h := func(v string) string { return `[` + cgHull(toolHullKeys, toolValid, k, v) + `]` }
			return cgSlot{depth: 3, body: func(v string) string { return cgTop("tools", h(v)) }, capped: h, capOf: capTools}
		}},
		{"toolHullKeys/tool_choice", toolHullKeys, func(k string) cgSlot {
			return cgSlot{depth: 2, body: func(v string) string { return cgTop("tool_choice", cgHull(toolHullKeys, toolValid, k, v)) }}
		}},
		// W02-08 (rebase round 2, A5): stream_options.include_usage is read,
		// by a walk over the object (readStreamOptions).
		{"streamOptionsKeys", streamOptionsKeys, func(k string) cgSlot {
			return free("stream_options", 2, func(v string) string { return `{"` + k + `":` + v + `}` })
		}},
		{"free", []string{"stream_options.*", "reasoning.*", "logit_bias.*", "stop.*"}, func(k string) cgSlot {
			switch k {
			case "stop.*":
				return free("stop", 2, func(v string) string { return `[` + v + `]` })
			case "logit_bias.*":
				return free("logit_bias", 2, func(v string) string { return `{"1":` + v + `}` })
			}
			return free(strings.TrimSuffix(k, ".*"), 2, func(v string) string { return `{"x":` + v + `}` })
		}},
	}
}

// imageNames are the image_url keys in sort order.
func imageNames() []string { return cgNamesOf(imageIndex) }

// cgSlots are the slots of every key of every table.
func cgSlots() []cgSlot {
	var out []cgSlot
	msg := func(k string) cgSlot {
		return cgSlot{depth: 3, body: func(v string) string { return cgTop("messages", `[`+cgMessage(k, v)+`]`) }}
	}
	for _, t := range cgTables() {
		mk := t.slot
		if mk == nil {
			mk = msg
		}
		for _, k := range t.keys {
			s := mk(k)
			s.table, s.key = t.name, k
			out = append(out, s)
		}
	}
	return out
}

// cgForm is one hostile value form: gen writes it in at most n bytes, at
// most depth containers deep and, for count > 0, with at most count
// entries.
type cgForm struct {
	name string
	gen  func(n, depth, count int) string
}

// cgEntries is how many entries of width w (separator included) fit in n
// bytes, at most count when count > 0.
func cgEntries(n, w, count int) int {
	e := max(n-2, 0) / w
	if count > 0 {
		e = min(e, count)
	}
	return e
}

// cgForms are the value forms of the gate (Delta-Review 5 §3: invalid UTF-8
// and lone surrogate escapes made a decode hold 15 × and 12 × the value; a
// deep array held the decoder's state).
var cgForms = []cgForm{
	{"ff", func(n, _, _ int) string { return `"` + strings.Repeat("\xff", max(n-2, 0)) + `"` }},
	{"surr", func(n, _, _ int) string { return `"` + strings.Repeat(`\ud800`, max(n-2, 0)/6) + `"` }},
	{"ascii", func(n, _, _ int) string { return `"` + strings.Repeat("a", max(n-2, 0)) + `"` }},
	{"deeparr", func(n, d, _ int) string { d = min(d, n/2); return strings.Repeat("[", d) + strings.Repeat("]", d) }},
	{"deepobj", func(n, d, _ int) string {
		d = min(d, (n-1)/6)
		return strings.Repeat(`{"a":`, d) + "0" + strings.Repeat("}", d)
	}},
	{"longint", func(n, _, _ int) string { return "1" + strings.Repeat("0", max(n-1, 0)) }},
	{"manyarr", func(n, _, count int) string {
		return "[" + strings.Repeat("0,", max(cgEntries(n, 2, count)-1, 0)) + "0]"
	}},
	{"manyobj", func(n, _, count int) string {
		var b strings.Builder
		b.WriteByte('{')
		for i := range cgEntries(n, 13, count) {
			if i > 0 {
				b.WriteByte(',')
			}
			fmt.Fprintf(&b, `"k%07d":0`, i)
		}
		b.WriteByte('}')
		return b.String()
	}},
	{"null", func(int, int, int) string { return "null" }},
}

// cgBody is the request of slot s with value form f at size bytes under l:
// the value fills the body to size, and a capped value to its cap.
func cgBody(s cgSlot, f cgForm, size int, l policyLimits) []byte {
	n := size - len(s.body(""))
	if s.capped != nil {
		n = min(n, s.capOf(l)-len(s.capped("")))
	}
	count := 0
	if s.countOf != nil {
		count = s.countOf(l)
	}
	return []byte(s.body(f.gen(max(n, 4), jsonMaxDepth-s.depth, count)))
}

// cgFind is the slot and form of the gate form name "<table>.<key>:<form>".
func cgFind(name string) (cgSlot, cgForm, bool) {
	sn, fn, _ := strings.Cut(name, ":")
	for _, s := range cgSlots() {
		if s.name() != sn {
			continue
		}
		for _, f := range cgForms {
			if f.name == fn {
				return s, f, true
			}
		}
	}
	return cgSlot{}, cgForm{}, false
}

// ClassGateBody is the body of the class-gate form name at size bytes (the
// route gate's "cg:" forms; the alias is CGModel with CGCeilPolicy).
func ClassGateBody(name string, size int) ([]byte, bool) {
	s, f, ok := cgFind(name)
	if !ok {
		return nil, false
	}
	return cgBody(s, f, size, cgLimits), true
}

// CGModel is the alias name of the class-gate bodies.
const CGModel = cgModel

// ClassGateCombos are the gate's combination names "<table>.<key>:<form>",
// every slot with every value form (the scan side, chat_class_gate_scan_test.go,
// and the route side, TestClassGateOverTheRoute, run each of them).
func ClassGateCombos() []string {
	var out []string
	for _, s := range cgSlots() {
		for _, f := range cgForms {
			out = append(out, s.name()+":"+f.name)
		}
	}
	return out
}

// cgException is a combination whose allocation is fixed and small, named
// with its measured upper bound (no silent tolerance): the gate allows it
// at most that many bytes beyond the slack.
type cgException struct {
	combo string
	bound uint64
	why   string
}

// cgExceptions are the named exceptions of the gate.
var cgExceptions = []cgException{}

// cgSize is the body of the bytes-per-parse measurement (the pins' size).
const cgSize = 256 << 10

// cgNear is the share of the slack past which a combination is near the
// bound: the route gate runs it at small bodies.
const cgNear = parseAllocSlack / 2

// TestClassGateBytesPerParse (hardening 6, P-3 (a)): every combination of a
// parse key and a value form at the key's cap allocates at most
// parseAllocSlack plus what the parse booked through grow (the rewritten
// message array) plus a named exception's bound. CTX_CLASSGATE_OUT names a
// file for one line per combination (the route gate's candidates).
func TestClassGateBytesPerParse(t *testing.T) {
	exc := map[string]cgException{}
	for _, e := range cgExceptions {
		exc[e.combo] = e
	}
	var out strings.Builder
	red, near := 0, 0
	for _, s := range cgSlots() {
		for _, f := range cgForms {
			combo := s.name() + ":" + f.name
			body := cgBody(s, f, cgSize, cgLimits)
			var booked int64
			var perr *paramError
			alloc := allocatedBy(func() {
				booked = 0
				req, p := parseTopLevel(body)
				if p == nil {
					p = req.parsePolicy(cgLimits, func(n int64) bool { booked += n; return true })
				}
				perr = p
			})
			bound := uint64(parseAllocSlack) + uint64(booked) + exc[combo].bound
			verdict := "ok"
			switch {
			case alloc > bound:
				verdict, red = "RED", red+1
				t.Errorf("%s: the parse of %d bytes allocates %d bytes, want ≤ %d (slack %d + booked %d + exception %d %s)",
					combo, len(body), alloc, bound, parseAllocSlack, booked, exc[combo].bound, exc[combo].why)
			case alloc > uint64(cgNear)+uint64(booked):
				verdict, near = "NEAR", near+1
			}
			answer := "accepted"
			if perr != nil {
				answer = perr.code + "@" + perr.param
			}
			fmt.Fprintf(&out, "CG combo=%s body=%d alloc=%d booked=%d bound=%d answer=%s verdict=%s\n", combo, len(body), alloc, booked, bound, answer, verdict)
		}
	}
	fmt.Fprintf(&out, "CG summary slots=%d forms=%d combos=%d red=%d near=%d\n", len(cgSlots()), len(cgForms), len(cgSlots())*len(cgForms), red, near)
	if p := os.Getenv("CTX_CLASSGATE_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// cgKeyCount and cgComboCount pin the size of the gate: a key added to a
// table moves them (and asks for a look at its body), a key left out of the
// gate moves them too.
const (
	cgKeyCount   = 81 // 80 on FOLGE-P; + stream_options.include_usage (W02-08)
	cgComboCount = cgKeyCount * 9
)

// cgNotKeyTables are the package's string tables that are no key table of
// the request parse, each with its reason: the census below finds every
// table by name and by shape, and one it finds must be a gate table or
// named here.
var cgNotKeyTables = map[string]string{
	"messageRoles":         "value table of a message's role, compared in place (cgForms reach it through role)",
	"serviceTiers":         "value table of service_tier, compared in place",
	"responseFormatTypes":  "value table of response_format.type, compared in place (P-1)",
	"visionPrefixes":       "value table of an image_url's data URL prefix, compared in place",
	"usageKeys":            "response side (rewrite.go): the usage object of an upstream answer",
	"reasoningMessageKeys": "response side (rewrite.go): reasoning members of an upstream answer's message",
	// The stream rewrite (W02-08) reads two tables of an upstream chunk.
	"streamDeltaKeys":   "response side (rewrite.go): the generated text fields of an upstream stream chunk's delta, which the stream's estimate counts",
	"nullableDeltaKeys": "response side (rewrite.go): the delta keys the stream schema allows as null in an upstream stream chunk",
	// The embeddings parse (W02-09) has its own key tables and its own class
	// gate (FOLGE-E, embed_class_gate_test.go): outside this gate's bodies,
	// which are chat requests.
	"embedKeys":     "request-key table of the embeddings parse (embeddings_parse.go), gated by the embeddings class gate",
	"embedDropKeys": "drop-key table of the embeddings parse (embeddings_parse.go), gated by the embeddings class gate",
}

// cgCensus is the census of the package's string tables over every non-test
// Go file (Delta-Review 6 L-1: parse.go alone missed a table in another
// file): every package-level var named …Keys, every var initialised with a
// map[string]… or []string literal, and every var assigned from a
// levelNames call with a literal list among its arguments — one list or
// more.
func cgCensus(t *testing.T) map[string]string {
	t.Helper()
	fset := token.NewFileSet()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	census := map[string]string{}
	parsed := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.VAR {
				continue
			}
			for _, sp := range g.Specs {
				vs := sp.(*ast.ValueSpec)
				for i, id := range vs.Names {
					if id.Name == "_" {
						continue
					}
					if strings.HasSuffix(id.Name, "Keys") {
						census[id.Name] = path
					}
					if len(vs.Values) == 0 || (i >= len(vs.Values) && len(vs.Values) != 1) {
						continue
					}
					if cgIsTable(vs.Values[min(i, len(vs.Values)-1)]) {
						census[id.Name] = path
					}
				}
			}
		}
	}
	if parsed < 2 {
		t.Fatalf("the census parsed %d non-test files of the package", parsed)
	}
	return census
}

// cgIsTable reports a string table by shape: a map[string]… or []string
// literal, or a levelNames call with a literal list among its arguments.
func cgIsTable(v ast.Expr) bool {
	switch e := v.(type) {
	case *ast.CompositeLit:
		switch ty := e.Type.(type) {
		case *ast.MapType:
			k, ok := ty.Key.(*ast.Ident)
			return ok && k.Name == "string"
		case *ast.ArrayType:
			el, ok := ty.Elt.(*ast.Ident)
			return ok && el.Name == "string"
		}
	case *ast.CallExpr:
		if fn, ok := e.Fun.(*ast.Ident); ok && fn.Name == "levelNames" {
			for _, a := range e.Args {
				if _, lit := a.(*ast.CompositeLit); lit {
					return true
				}
			}
		}
	}
	return false
}

// TestClassGateCoversEveryKeyTable (P-3, the count pin): the gate names
// every key table of the package — every string table the census finds in
// a non-test file (cgCensus) is a gate table or named in cgNotKeyTables —
// and every key of each table (read from the table, not copied), and its
// size is pinned.
func TestClassGateCoversEveryKeyTable(t *testing.T) {
	census := cgCensus(t)
	gate := map[string]bool{}
	for _, tb := range cgTables() {
		gate[strings.Split(tb.name, "/")[0]] = true
	}
	delete(gate, "free")
	for name, path := range census {
		if !gate[name] && cgNotKeyTables[name] == "" {
			t.Errorf("%s: table %s has no class-gate entry (cgTables) and is not named in cgNotKeyTables", path, name)
		}
	}
	for name := range gate {
		if census[name] == "" {
			t.Errorf("class-gate table %s is not a table of the package", name)
		}
	}
	for name := range cgNotKeyTables {
		if census[name] == "" {
			t.Errorf("cgNotKeyTables names %s, which the census does not find", name)
		}
	}
	want := map[string][]string{
		"passKeys": cgSorted(passKeys), "ctxKeys": cgSorted(ctxKeys), "dropKeys": cgSorted(dropKeys),
		"messagePassKeys": cgSorted(messagePassKeys), "messageDropKeys": cgSorted(messageDropKeys),
		"partIndex": cgNamesOf(partIndex), "imageIndex": cgNamesOf(imageIndex),
		"responseFormatKeys": responseFormatKeys, "responseFormatJSONKeys": responseFormatJSONKeys,
		"toolHullKeys/tools": toolHullKeys, "toolHullKeys/tool_choice": toolHullKeys,
		"streamOptionsKeys": streamOptionsKeys,
	}
	got := map[string][]string{}
	for _, s := range cgSlots() {
		got[s.table] = append(got[s.table], s.key)
	}
	for tb, keys := range want {
		g := slices.Sorted(slices.Values(got[tb]))
		w := slices.Sorted(slices.Values(keys))
		if !slices.Equal(g, w) {
			t.Errorf("class gate, table %s: keys %v, the table has %v", tb, g, w)
		}
	}
	if n := len(cgSlots()); n != cgKeyCount || n*len(cgForms) != cgComboCount {
		t.Errorf("class gate: %d keys × %d forms = %d combinations, pinned %d × 9 = %d (a key was added or left out: give it a body, then move the pin)",
			n, len(cgForms), n*len(cgForms), cgKeyCount, cgComboCount)
	}
	// Every slot reaches its key: some form is accepted or refused at a path
	// that names the key (a body that misses its slot answers elsewhere).
	for _, s := range cgSlots() {
		reached := false
		leaf := strings.TrimSuffix(s.key, ".*")
		for _, f := range cgForms {
			req, perr := parseTopLevel(cgBody(s, f, 4<<10, cgLimits))
			if perr == nil {
				perr = req.parsePolicy(cgLimits, nil)
			}
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
