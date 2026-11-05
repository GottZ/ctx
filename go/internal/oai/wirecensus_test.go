package oai_test

// X13 census (W02-06 review N-5, W02-07 G16): internal/oai reaches a backend
// only through llm.ChatRaw / llm.ChatRawStream, and only inside an attempt
// of llm.WalkChain — the walker's lease is the admission of every wire
// contact (I-D1). A ChatRaw* call outside an attempt would be a wire call
// without a lease, invisible to the dispatcher census (Σacquire == Σwire).
//
// The rule, per production file: every function that contains a ChatRaw*
// call takes a *dispatch.Lease (it runs under a lease the walker holds), and
// it is referenced only inside the function literal passed as the attempt
// argument of an llm.WalkChain call (or it IS that literal).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const llmImport = "github.com/GottZ/ctx/internal/llm"

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
// violations and the number of ChatRaw* call sites seen.
func wireCensus(fset *token.FileSet, files []*ast.File) ([]wireFinding, int) {
	var findings []wireFinding
	sites := 0
	// wireFuncs: named functions/methods holding a ChatRaw* call.
	wireFuncs := map[string]token.Pos{}
	// attemptLits: the attempt literals of WalkChain calls.
	var attemptLits []*ast.FuncLit
	for _, f := range files {
		llmName := importName(f, llmImport, "llm")
		if llmName == "." {
			findings = append(findings, wireFinding{fset.Position(f.Package), "internal/llm dot-imported: its wire sites would be unqualified and invisible"})
			continue
		}
		if llmName == "" {
			continue
		}
		findings = append(findings, wireValueRefs(fset, f, llmName)...)
		ast.Inspect(f, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && isPkgCall(call, llmName, "WalkChain") && len(call.Args) > 0 {
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
				if !ok || !isPkgCall(call, llmName, "ChatRaw", "ChatRawStream") {
					return true
				}
				sites++
				// the innermost enclosing function; a go statement between it
				// and the call runs the wire call outside the walker's lease
				var encl ast.Node = fd
				for i := len(stack) - 2; i >= 0; i-- {
					if _, ok := stack[i].(*ast.GoStmt); ok {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), "ChatRaw* in a goroutine: it outlives the walker's lease"})
					}
					if lit, ok := stack[i].(*ast.FuncLit); ok {
						encl = lit
						break
					}
				}
				switch e := encl.(type) {
				case *ast.FuncDecl:
					if !takesLease(e.Type) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), "ChatRaw* in " + e.Name.Name + ", which runs under no lease (no *dispatch.Lease parameter)"})
					}
					wireFuncs[e.Name.Name] = e.Pos()
				case *ast.FuncLit:
					if !takesLease(e.Type) || !containsLit(attemptLits, e) {
						findings = append(findings, wireFinding{fset.Position(call.Pos()), "ChatRaw* in a function literal that is no WalkChain attempt"})
					}
				}
				return true
			})
		}
	}
	// Every reference to a wire function lies inside an attempt literal, and
	// none inside a go statement.
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
			declPos, ok := wireFuncs[name]
			if !ok || insideDeclName(files, n.Pos(), declPos) {
				return true
			}
			if !insideAny(attemptLits, n.Pos()) {
				findings = append(findings, wireFinding{fset.Position(n.Pos()), name + " (a wire function) referenced outside a WalkChain attempt"})
			}
			return true
		})
	}
	return dedupe(findings), sites
}

// wireValueRefs reports every llm.ChatRaw* selector that is not the callee
// of a call — a method value, a package variable, an argument: a wire site
// that the call census would not count (Fremd-Review 1 F-6).
func wireValueRefs(fset *token.FileSet, f *ast.File, llmName string) []wireFinding {
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
		if id, ok := sel.X.(*ast.Ident); ok && id.Name == llmName && (sel.Sel.Name == "ChatRaw" || sel.Sel.Name == "ChatRawStream") {
			out = append(out, wireFinding{fset.Position(sel.Pos()), "llm." + sel.Sel.Name + " used as a value: a wire site the call census cannot follow"})
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

// insideDeclName reports whether p is the declaring name of the FuncDecl at
// declPos (its own declaration is no reference).
func insideDeclName(files []*ast.File, p, declPos token.Pos) bool {
	for _, f := range files {
		for _, d := range f.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Pos() == declPos {
				return p == fd.Name.Pos()
			}
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
// is the same hole in the harness) — hold exactly two ChatRaw* call sites
// (the non-stream attempt, W02-07, and the stream attempt, W02-08), and
// both obey the X13 rule.
func TestOAIWireCallsOnlyInsideWalkChainAttempts(t *testing.T) {
	fset := token.NewFileSet()
	files := parseDir(t, fset, ".", func(string) bool { return true })
	findings, sites := wireCensus(fset, files)
	for _, f := range findings {
		t.Errorf("%s: %s", f.pos, f.what)
	}
	if sites != 2 {
		t.Errorf("ChatRaw* call sites in internal/oai = %d, want 2 (the non-stream attempt W02-07, the stream attempt W02-08)", sites)
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
