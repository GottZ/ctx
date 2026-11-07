package oai_test

// X13 census (W02-06 review N-5, W02-07 G16, W02-09 Z4): internal/oai
// reaches a backend only through llm.ChatRaw / llm.ChatRawStream and
// embed.EmbedRaw (embed.Embed counts as a wire too: the internal path must
// never run from here without a lease), and only inside an attempt of
// llm.WalkChain — the walker's lease, or a continuation lease the attempt
// asked for, is the admission of every wire contact (I-D1). A wire call
// outside an attempt would be a wire call without a lease, invisible to the
// dispatcher census (Σacquire == Σwire).
//
// The rule, per production file: every function that contains a wire call
// — or, transitively, a reference to such a function (the sub-block walk:
// attempt → block function, W02-09) — takes a *dispatch.Lease (it runs
// under a lease), and it is referenced only inside the function literal
// passed as the attempt argument of an llm.WalkChain call (or it IS that
// literal) or inside another such function, never in a go statement.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	llmImport   = "github.com/GottZ/ctx/internal/llm"
	embedImport = "github.com/GottZ/ctx/internal/embed"
)

// wireSelectors are the wire functions per package (by import path).
var wireSelectors = map[string][]string{
	llmImport:   {"ChatRaw", "ChatRawStream"},
	embedImport: {"EmbedRaw", "Embed"},
}

// wireNames binds each wire package to the name file f imports it as; a
// dot import of one is a finding (its wire sites would be unqualified).
func wireNames(fset *token.FileSet, f *ast.File) (map[string]string, []wireFinding) {
	names := map[string]string{}
	var findings []wireFinding
	for path, def := range map[string]string{llmImport: "llm", embedImport: "embed"} {
		switch n := importName(f, path, def); n {
		case "":
		case ".":
			findings = append(findings, wireFinding{fset.Position(f.Package), path + " dot-imported: its wire sites would be unqualified and invisible"})
		default:
			names[path] = n
		}
	}
	return names, findings
}

// wireCall reports whether call is a wire call under names, and which.
func wireCall(call *ast.CallExpr, names map[string]string) (string, bool) {
	for path, pkg := range names {
		if isPkgCall(call, pkg, wireSelectors[path]...) {
			return pkg + "." + call.Fun.(*ast.SelectorExpr).Sel.Name, true
		}
	}
	return "", false
}

// wireFinding is one violation of the X13 rule.
type wireFinding struct {
	pos  token.Position
	what string
}

// importName is the name file f binds path to ("" = not imported).
func importName(f *ast.File, path, def string) string {
	for _, im := range f.Imports {
		if p, _ := strconv.Unquote(im.Path.Value); p == path {
			if im.Name != nil {
				return im.Name.Name
			}
			return def
		}
	}
	return ""
}

func isPkgCall(call *ast.CallExpr, pkg string, names ...string) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	id, ok := sel.X.(*ast.Ident)
	if !ok || id.Name != pkg {
		return false
	}
	for _, n := range names {
		if sel.Sel.Name == n {
			return true
		}
	}
	return false
}

// takesLease reports a parameter of type *dispatch.Lease.
func takesLease(ft *ast.FuncType) bool {
	if ft == nil || ft.Params == nil {
		return false
	}
	for _, p := range ft.Params.List {
		star, ok := p.Type.(*ast.StarExpr)
		if !ok {
			continue
		}
		if sel, ok := star.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "Lease" {
			return true
		}
	}
	return false
}

// wireCensus applies the X13 rule to the files (one package) and returns the
// violations and the number of wire call sites seen per wire function
// ("llm.ChatRaw", "embed.EmbedRaw", …).
func wireCensus(fset *token.FileSet, files []*ast.File) ([]wireFinding, map[string]int) {
	var findings []wireFinding
	sites := map[string]int{}
	// wireFuncs: the names of the functions/methods holding a wire call
	// (closed below over the functions referencing one); wireDecls the
	// declarations themselves — two methods of one name (chatCall.attempt,
	// embedCall.attempt) are two declarations.
	wireFuncs := map[string]token.Pos{}
	wireDecls := map[token.Pos]bool{}
	// attemptLits: the attempt literals of WalkChain calls.
	var attemptLits []*ast.FuncLit
	for _, f := range files {
		names, dot := wireNames(fset, f)
		findings = append(findings, dot...)
		if len(names) == 0 {
			continue
		}
		findings = append(findings, wireValueRefs(fset, f, names)...)
		llmName := names[llmImport]
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && llmName != "" && isPkgCall(call, llmName, "WalkChain") && len(call.Args) > 0 {
				if lit, ok := call.Args[len(call.Args)-1].(*ast.FuncLit); ok {
					attemptLits = append(attemptLits, lit)
				}
			}
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			var stack []ast.Node
			ast.Inspect(fd, func(n ast.Node) bool {
				if n == nil {
					stack = stack[:len(stack)-1]
					return true
				}
				stack = append(stack, n)
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				wire, ok := wireCall(call, names)
				if !ok {
					return true
				}
				sites[wire]++
				// the innermost enclosing function; a go statement between it
				// and the call runs the wire call outside the walker's lease
				var encl ast.Node = fd
				for i := len(stack) - 2; i >= 0; i-- {
					if _, ok := stack[i].(*ast.GoStmt); ok {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), wire + " in a goroutine: it outlives the walker's lease"})
					}
					if lit, ok := stack[i].(*ast.FuncLit); ok {
						encl = lit
						break
					}
				}
				switch e := encl.(type) {
				case *ast.FuncDecl:
					if !takesLease(e.Type) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), wire + " in " + e.Name.Name + ", which runs under no lease (no *dispatch.Lease parameter)"})
					}
					wireFuncs[e.Name.Name] = e.Pos()
					wireDecls[e.Pos()] = true
				case *ast.FuncLit:
					if !takesLease(e.Type) || !containsLit(attemptLits, e) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), wire + " in a function literal that is no WalkChain attempt"})
					}
				}
				return true
			})
		}
	}
	findings = append(findings, closeWireFuncs(fset, files, wireFuncs, wireDecls, attemptLits)...)
	bf, bfSites := blockFuncCalls(fset, files)
	findings = append(findings, bf...)
	findings = append(findings, blockFuncValues(fset, files, wireFuncs, attemptLits)...)
	if bfSites > 0 {
		sites["blockFunc"] = bfSites
	}
	callees := calleeIdents(files)
	// Every reference to a wire function lies inside an attempt literal or
	// inside another wire function, and none inside a go statement.
	for _, f := range files {
		var goDepth []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				goDepth = goDepth[:len(goDepth)-1]
				return true
			}
			goDepth = append(goDepth, n)
			id, ok := n.(*ast.Ident) // a selector's Sel is an Ident too
			if !ok {
				return true
			}
			if _, isWire := wireFuncs[id.Name]; isWire {
				for _, a := range goDepth {
					if _, ok := a.(*ast.GoStmt); ok {
						findings = append(findings, wireFinding{fset.Position(n.Pos()), id.Name + " (a wire function) started as a goroutine"})
						break
					}
				}
			}
			name := id.Name
			if _, ok := wireFuncs[name]; !ok || isDeclName(files, n.Pos()) {
				return true
			}
			if !callees[n.Pos()] {
				// A method value or a function value of a wire function: its
				// later call (a go statement, after the lease went back) is
				// invisible to the rule (Fremd-Review 1 F-2).
				findings = append(findings, wireFinding{fset.Position(n.Pos()), name + " (a wire function) used as a value"})
			}
			if !insideAny(attemptLits, n.Pos()) && !insideWireDecl(files, wireDecls, n.Pos()) {
				findings = append(findings, wireFinding{fset.Position(n.Pos()), name + " (a wire function) referenced outside a WalkChain attempt"})
			}
			return true
		})
	}
	return dedupe(findings), sites
}

// calleeIdents are the identifiers in call position: the callee of a call
// (f(…)) or the selected name of one (x.f(…)).
func calleeIdents(files []*ast.File) map[token.Pos]bool {
	out := map[token.Pos]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				out[fun.Pos()] = true
			case *ast.SelectorExpr:
				out[fun.Sel.Pos()] = true
			}
			return true
		})
	}
	return out
}

// blockFuncType is the type of the sub-block wire call the sub-block walk
// receives as a value (subblock.go): a call of a parameter of this type is a
// wire site the name-based rule cannot follow (W02-09, Fremd-Review 1 F-2).
const blockFuncType = "blockFunc"

// blockFuncCalls finds every call of a parameter of type blockFunc: the
// innermost enclosing function must take a lease and no go statement may lie
// between it and the call (the call would outlive the block's lease).
func blockFuncCalls(fset *token.FileSet, files []*ast.File) ([]wireFinding, int) {
	var findings []wireFinding
	sites := 0
	fields := blockFuncFields(files)
	for _, f := range files {
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				if !blockFuncInScope(stack, fun.Name) {
					return true
				}
			case *ast.SelectorExpr:
				// a field of type blockFunc (Review 2 probe A)
				if !fields[fun.Sel.Name] {
					return true
				}
			default:
				return true
			}
			sites++
			for i := len(stack) - 2; i >= 0; i-- {
				switch e := stack[i].(type) {
				case *ast.GoStmt:
					findings = append(findings, wireFinding{fset.Position(call.Pos()), "a blockFunc call in a goroutine: it outlives the block's lease"})
				case *ast.FuncDecl:
					if !takesLease(e.Type) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), "a blockFunc call in " + e.Name.Name + ", which runs under no lease"})
					}
				case *ast.FuncLit:
					if !takesLease(e.Type) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), "a blockFunc call in a function literal that runs under no lease"})
					}
				default:
					continue
				}
				break
			}
			return true
		})
	}
	return findings, sites
}

// blockFuncInScope reports whether name is a parameter of type blockFunc of
// one of the functions enclosing the call (lexically: the stack).
func blockFuncInScope(stack []ast.Node, name string) bool {
	for _, n := range stack {
		var ft *ast.FuncType
		switch fn := n.(type) {
		case *ast.FuncDecl:
			ft = fn.Type
		case *ast.FuncLit:
			ft = fn.Type
		}
		if ft == nil || ft.Params == nil {
			continue
		}
		for _, p := range ft.Params.List {
			if id, ok := p.Type.(*ast.Ident); ok && id.Name == blockFuncType {
				for _, nm := range p.Names {
					if nm.Name == name {
						return true
					}
				}
			}
		}
	}
	return false
}

// blockFuncFields names the struct fields of type blockFunc.
func blockFuncFields(files []*ast.File) map[string]bool {
	out := map[string]bool{}
	for _, f := range files {
		ast.Inspect(f, func(n ast.Node) bool {
			if st, ok := n.(*ast.StructType); ok {
				for _, fl := range st.Fields.List {
					if id, ok := fl.Type.(*ast.Ident); ok && id.Name == blockFuncType {
						for _, nm := range fl.Names {
							out[nm.Name] = true
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// blockFuncParam reports whether argument idx of a call of name goes to a
// parameter of type blockFunc of a function or method of that name in the
// files (AST only: every declaration of the name is asked).
func blockFuncParam(files []*ast.File, name string, idx int) bool {
	for _, f := range files {
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Name.Name != name || fd.Type.Params == nil {
				continue
			}
			i := 0
			for _, p := range fd.Type.Params.List {
				names := max(len(p.Names), 1)
				if idx < i+names {
					if id, ok := p.Type.(*ast.Ident); ok && id.Name == blockFuncType {
						return true
					}
					break
				}
				i += names
			}
		}
	}
	return false
}

// calleeName is the name a call goes to (f(…) or x.f(…)), "" otherwise.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		return fun.Sel.Name
	}
	return ""
}

// blockFuncValues finds every use of a blockFunc value that is no call of
// it (Review 2 R2-2, hardening H-8): a parameter or field of type blockFunc
// copied into a variable, stored, returned — anything but a call or an
// argument to another parameter of type blockFunc — and a function literal
// holding a wire function that is neither a WalkChain attempt literal nor
// handed to a parameter of type blockFunc (probe C: a helper with the
// unnamed func type). Either way the later call is out of the census's
// sight, and with it the lease it runs under.
func blockFuncValues(fset *token.FileSet, files []*ast.File, wireFuncs map[string]token.Pos, attemptLits []*ast.FuncLit) []wireFinding {
	var findings []wireFinding
	fields := blockFuncFields(files)
	for _, f := range files {
		allowed := map[ast.Node]bool{} // arguments handed to a blockFunc parameter
		decl := map[*ast.Ident]bool{}  // declaring names
		ast.Inspect(f, func(n ast.Node) bool {
			switch x := n.(type) {
			case *ast.CallExpr:
				name := calleeName(x)
				for i, a := range x.Args {
					if name != "" && blockFuncParam(files, name, i) {
						allowed[a] = true
					}
				}
				switch fun := x.Fun.(type) {
				case *ast.Ident:
					decl[fun] = true // a call of the name, not a value
				case *ast.SelectorExpr:
					decl[fun.Sel] = true
				}
			case *ast.Field:
				for _, nm := range x.Names {
					decl[nm] = true
				}
			case *ast.ValueSpec:
				for _, nm := range x.Names {
					decl[nm] = true
				}
			}
			return true
		})
		var stack []ast.Node
		ast.Inspect(f, func(n ast.Node) bool {
			if n == nil {
				stack = stack[:len(stack)-1]
				return true
			}
			stack = append(stack, n)
			switch x := n.(type) {
			case *ast.Ident:
				if decl[x] || allowed[x] {
					return true
				}
				if blockFuncInScope(stack, x.Name) {
					findings = append(findings, wireFinding{fset.Position(x.Pos()), "the blockFunc " + x.Name + " used as a value: its call is out of the census's sight"})
				}
			case *ast.SelectorExpr:
				if fields[x.Sel.Name] && !decl[x.Sel] && !allowed[x] {
					findings = append(findings, wireFinding{fset.Position(x.Pos()), "the blockFunc field " + x.Sel.Name + " used as a value"})
				}
			case *ast.FuncLit:
				if containsLit(attemptLits, x) || allowed[x] || !refersTo(x, wireFuncs) {
					return true
				}
				findings = append(findings, wireFinding{fset.Position(x.Pos()), "a function literal holding a wire function, handed to no blockFunc parameter and no WalkChain attempt"})
			}
			return true
		})
	}
	return findings
}

// refersTo reports whether the literal's body names a wire function.
func refersTo(lit *ast.FuncLit, wireFuncs map[string]token.Pos) bool {
	found := false
	ast.Inspect(lit.Body, func(n ast.Node) bool {
		if id, ok := n.(*ast.Ident); ok {
			if _, w := wireFuncs[id.Name]; w {
				found = true
			}
		}
		return !found
	})
	return found
}

// closeWireFuncs adds every named function that references a wire function
// outside a WalkChain attempt literal to wireFuncs, to the fixpoint — the
// sub-block walk calls its block function from the attempt (W02-09) — and
// reports each such function that takes no lease: a wire call reached
// through it runs under none. A reference inside an attempt literal is the
// entry the rule allows; it makes the enclosing function no wire function.
func closeWireFuncs(fset *token.FileSet, files []*ast.File, wireFuncs map[string]token.Pos, wireDecls map[token.Pos]bool,
	attemptLits []*ast.FuncLit) []wireFinding {
	var findings []wireFinding
	for grew := true; grew; {
		grew = false
		for _, f := range files {
			for _, d := range f.Decls {
				fd, ok := d.(*ast.FuncDecl)
				if !ok || fd.Body == nil {
					continue
				}
				if wireDecls[fd.Pos()] {
					continue
				}
				refs := false
				ast.Inspect(fd.Body, func(n ast.Node) bool {
					if lit, ok := n.(*ast.FuncLit); ok && containsLit(attemptLits, lit) {
						return false
					}
					if id, ok := n.(*ast.Ident); ok {
						if _, w := wireFuncs[id.Name]; w {
							refs = true
						}
					}
					return !refs
				})
				if !refs {
					continue
				}
				wireFuncs[fd.Name.Name] = fd.Pos()
				wireDecls[fd.Pos()] = true
				grew = true
				if !takesLease(fd.Type) {
					findings = append(findings, wireFinding{fset.Position(fd.Pos()), fd.Name.Name + " reaches a wire function but runs under no lease (no *dispatch.Lease parameter)"})
				}
			}
		}
	}
	return findings
}

// insideWireDecl reports whether p lies in the body of a wire function's
// declaration.
func insideWireDecl(files []*ast.File, wireDecls map[token.Pos]bool, p token.Pos) bool {
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && wireDecls[fd.Pos()] && p >= fd.Body.Pos() && p <= fd.Body.End() {
				return true
			}
		}
	}
	return false
}

// isDeclName reports whether p is the declaring name of a function (its own
// declaration is no reference).
func isDeclName(files []*ast.File, p token.Pos) bool {
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Name.Pos() == p {
				return true
			}
		}
	}
	return false
}

// wireValueRefs reports every wire selector that is not the callee of a call
// — a method value, a package variable, an argument: a wire site that the
// call census would not count (Fremd-Review 1 F-6).
func wireValueRefs(fset *token.FileSet, f *ast.File, names map[string]string) []wireFinding {
	callee := map[*ast.SelectorExpr]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
				callee[sel] = true
			}
		}
		return true
	})
	var out []wireFinding
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok || callee[sel] {
			return true
		}
		id, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		for path, pkg := range names {
			if id.Name == pkg && slices.Contains(wireSelectors[path], sel.Sel.Name) {
				out = append(out, wireFinding{fset.Position(sel.Pos()), pkg + "." + sel.Sel.Name + " used as a value: a wire site the call census cannot follow"})
			}
		}
		return true
	})
	return out
}

func containsLit(lits []*ast.FuncLit, l *ast.FuncLit) bool {
	for _, x := range lits {
		if x == l {
			return true
		}
	}
	return false
}

func insideAny(lits []*ast.FuncLit, p token.Pos) bool {
	for _, l := range lits {
		if p >= l.Pos() && p <= l.End() {
			return true
		}
	}
	return false
}

func dedupe(fs []wireFinding) []wireFinding {
	seen := map[string]bool{}
	var out []wireFinding
	for _, f := range fs {
		k := f.pos.String() + f.what
		if !seen[k] {
			seen[k] = true
			out = append(out, f)
		}
	}
	return out
}

func parseDir(t *testing.T, fset *token.FileSet, dir string, keep func(name string) bool) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || !keep(e.Name()) {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, e.Name()), nil, parser.ParseComments)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestOAIWireCallsOnlyInsideWalkChainAttempts: the files of internal/oai —
// production and tests alike (a test that dials a backend past the walker
// is the same hole in the harness) — hold exactly one llm.ChatRaw call site
// (the non-stream chat attempt; W02-08 adds the stream one) and one
// embed.EmbedRaw call site (the block function of the embeddings attempt,
// W02-09), and no other wire call; every one obeys the X13 rule.
func TestOAIWireCallsOnlyInsideWalkChainAttempts(t *testing.T) {
	fset := token.NewFileSet()
	files := parseDir(t, fset, ".", func(string) bool { return true })
	findings, sites := wireCensus(fset, files)
	for _, f := range findings {
		t.Errorf("%s: %s", f.pos, f.what)
	}
	if want := map[string]int{"llm.ChatRaw": 1, "embed.EmbedRaw": 1, "blockFunc": 2}; !maps.Equal(sites, want) {
		t.Errorf("wire call sites in internal/oai = %v, want %v (the chat attempt W02-07; the embeddings block and the two block calls of the sub-block walk, W02-09)", sites, want)
	}
}

// TestOAIWireCensusDetects: the census against its probes under
// testdata/x13 — each file says how many findings it must produce
// ("// census-want: N"). A census that cannot see a direct call passes
// forever.
func TestOAIWireCensusDetects(t *testing.T) {
	dir := filepath.Join("testdata", "x13")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	probes := 0
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go.txt") {
			continue
		}
		probes++
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		want := -1
		for _, line := range strings.Split(string(raw), "\n") {
			if v, ok := strings.CutPrefix(strings.TrimSpace(line), "// census-want: "); ok {
				want, _ = strconv.Atoi(v)
			}
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, e.Name(), raw, parser.ParseComments)
		if err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		got, _ := wireCensus(fset, []*ast.File{f})
		if len(got) != want {
			t.Errorf("%s: %d finding(s), want %d: %v", e.Name(), len(got), want, got)
		}
	}
	if probes < 3 {
		t.Fatalf("%d probes under %s, want ≥ 3", probes, dir)
	}
}
