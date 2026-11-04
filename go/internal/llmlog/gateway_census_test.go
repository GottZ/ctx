package llmlog

// W06-9 census pins (gate (e), design/06 §4.6, B17, R-06-1): two syntactic
// walks over every non-test .go file of the module.
//
//  1. Entry.ConsumerID is written only by Entry.StampGateway — the one
//     setter of the /v1 identity. A write is a composite literal of
//     llmlog.Entry naming the field (including the elided element literals
//     of a []llmlog.Entry / map[…]llmlog.Entry), an assignment to a
//     .ConsumerID selector, or taking its address — the latter two in the
//     llmlog package itself and in every file that imports it.
//  2. Under internal/oai: no reference to llmlog.Record (the /v1 write path
//     is RecordGateway) and no write of Entry.APIKeyID.
//
// Limits, stated rather than assumed: the walk has no type information. A
// .ConsumerID assignment in a file that imports llmlog counts even when the
// receiver is another struct with a field of that name (today: none —
// fail-closed, an exemption would be a visible diff here). Not seen: an
// assignment in a file that does NOT import llmlog — to an Entry value
// obtained by type inference (e := newChainEntry(…); e.ConsumerID = …) or
// to the promoted field of a struct embedding llmlog.Entry declared
// elsewhere — and writes through encoding/json or reflect. Today no
// production file assigns .ConsumerID outside StampGateway at all.

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

const llmlogImportPath = "github.com/GottZ/ctx/internal/llmlog"

// gatewayCensusModuleRoot walks up from the package directory to go.mod.
func gatewayCensusModuleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test directory")
		}
		dir = parent
	}
}

// censusFile is one parsed production file.
type censusFile struct {
	rel  string
	file *ast.File
}

// productionFiles parses every non-test .go file under root (build
// constraints ignored: integration-only files count), skipping testdata,
// vendor, node_modules and hidden directories.
func productionFiles(t *testing.T, fset *token.FileSet, root string) []censusFile {
	t.Helper()
	var out []censusFile
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if name := d.Name(); path != root && (strings.HasPrefix(name, ".") || name == "node_modules" || name == "testdata" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.ParseComments|parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		out = append(out, censusFile{rel: filepath.ToSlash(rel), file: f})
		return nil
	})
	if err != nil {
		t.Fatalf("census walk %s: %v", root, err)
	}
	return out
}

// llmlogNames is how f names the llmlog package: its qualifiers (default
// name or alias) and whether Entry is reachable bare (inside the package or
// through a dot import). imported reports any import of it.
func llmlogNames(f *ast.File) (qualifiers map[string]bool, bare, imported bool) {
	qualifiers = map[string]bool{}
	bare = f.Name.Name == "llmlog"
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != llmlogImportPath {
			continue
		}
		imported = true
		switch {
		case imp.Name == nil:
			qualifiers["llmlog"] = true
		case imp.Name.Name == ".":
			bare = true
		case imp.Name.Name != "_":
			qualifiers[imp.Name.Name] = true
		}
	}
	return qualifiers, bare, imported
}

// isLLMLogName reports whether x names llmlog.<name> in f.
func isLLMLogName(x ast.Expr, name string, qualifiers map[string]bool, bare bool) bool {
	switch v := x.(type) {
	case *ast.SelectorExpr:
		id, ok := v.X.(*ast.Ident)
		return ok && qualifiers[id.Name] && v.Sel.Name == name
	case *ast.Ident:
		return bare && v.Name == name
	case *ast.StarExpr:
		return isLLMLogName(v.X, name, qualifiers, bare)
	}
	return false
}

// entryFieldWrites returns the positions where f writes field of
// llmlog.Entry, except inside the method exempt of Entry (package llmlog
// only; "" = no exemption). Entry is llmlog.Entry under any import name,
// plus every type the file declares as it (type X = llmlog.Entry, type X
// llmlog.Entry). Literals count with their elided element literals at any
// depth ([][]llmlog.Entry{{{…}}}, map values, pointer elements);
// assignments count as =, op= and the key/value of a range with =.
func entryFieldWrites(f *ast.File, field, exempt string) []token.Pos {
	qualifiers, bare, imported := llmlogNames(f)
	inLLMLog := f.Name.Name == "llmlog"
	local := map[string]bool{}
	ast.Inspect(f, func(n ast.Node) bool {
		if ts, ok := n.(*ast.TypeSpec); ok && isLLMLogName(ts.Type, "Entry", qualifiers, bare) {
			local[ts.Name.Name] = true
		}
		return true
	})
	isEntry := func(x ast.Expr) bool {
		if st, ok := x.(*ast.StarExpr); ok {
			x = st.X
		}
		if id, ok := x.(*ast.Ident); ok && local[id.Name] {
			return true
		}
		return isLLMLogName(x, "Entry", qualifiers, bare)
	}
	var hits []token.Pos
	literalHits := func(lit *ast.CompositeLit) {
		for _, el := range lit.Elts {
			if kv, ok := el.(*ast.KeyValueExpr); ok {
				if id, ok := kv.Key.(*ast.Ident); ok && id.Name == field {
					hits = append(hits, kv.Pos())
				}
			}
		}
	}
	// typeOf holds the type of a literal whose own Type is elided — the
	// element type of the enclosing container literal.
	typeOf := map[*ast.CompositeLit]ast.Expr{}
	var exemptBody *ast.BlockStmt
	if inLLMLog && exempt != "" {
		for _, decl := range f.Decls {
			if fd, ok := decl.(*ast.FuncDecl); ok && fd.Recv != nil && fd.Name.Name == exempt && len(fd.Recv.List) == 1 && isEntry(fd.Recv.List[0].Type) {
				exemptBody = fd.Body
			}
		}
	}
	inExempt := func(p token.Pos) bool {
		return exemptBody != nil && p >= exemptBody.Pos() && p < exemptBody.End()
	}
	selectorWrite := func(x ast.Expr) {
		if sel, ok := x.(*ast.SelectorExpr); ok && sel.Sel.Name == field && (inLLMLog || imported) && !inExempt(sel.Pos()) {
			hits = append(hits, sel.Pos())
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch v := n.(type) {
		case *ast.CompositeLit:
			typ := v.Type
			if typ == nil {
				typ = typeOf[v]
			}
			if st, ok := typ.(*ast.StarExpr); ok {
				typ = st.X
			}
			var elt ast.Expr
			switch t := typ.(type) {
			case *ast.ArrayType:
				elt = t.Elt
			case *ast.MapType:
				elt = t.Value
			}
			if elt != nil {
				for _, el := range v.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						el = kv.Value
					}
					if u, ok := el.(*ast.UnaryExpr); ok && u.Op == token.AND {
						el = u.X
					}
					if inner, ok := el.(*ast.CompositeLit); ok && inner.Type == nil {
						typeOf[inner] = elt
					}
				}
			}
			if typ != nil && isEntry(typ) {
				literalHits(v)
			}
		case *ast.AssignStmt:
			for _, lhs := range v.Lhs {
				selectorWrite(lhs)
			}
		case *ast.RangeStmt:
			if v.Tok == token.ASSIGN {
				selectorWrite(v.Key)
				selectorWrite(v.Value)
			}
		case *ast.UnaryExpr:
			if v.Op == token.AND {
				selectorWrite(v.X)
			}
		}
		return true
	})
	return hits
}

// llmlogRecordRefs returns every reference to llmlog.Record in f (a call
// or a function value).
func llmlogRecordRefs(f *ast.File) []token.Pos {
	qualifiers, bare, _ := llmlogNames(f)
	var hits []token.Pos
	ast.Inspect(f, func(n ast.Node) bool {
		if x, ok := n.(ast.Expr); ok && isLLMLogName(x, "Record", qualifiers, bare && f.Name.Name != "llmlog") {
			hits = append(hits, x.Pos())
			return false
		}
		return true
	})
	return hits
}

// TestGatewayConsumerIDCensus is census pin 1 over the module.
func TestGatewayConsumerIDCensus(t *testing.T) {
	root := gatewayCensusModuleRoot(t)
	fset := token.NewFileSet()
	files := productionFiles(t, fset, root)
	parsed := map[string]bool{}
	var findings []string
	stamps := 0
	for _, cf := range files {
		parsed[cf.rel] = true
		for _, p := range entryFieldWrites(cf.file, "ConsumerID", "StampGateway") {
			findings = append(findings, fmt.Sprintf("%s:%d", cf.rel, fset.Position(p).Line))
		}
		if cf.rel == "internal/llmlog/gateway.go" {
			stamps = len(entryFieldWrites(cf.file, "ConsumerID", ""))
		}
	}
	for _, must := range []string{"internal/llmlog/llmlog.go", "internal/llmlog/gateway.go", "internal/llm/chain.go", "internal/handler/model_auth.go", "cmd/ctxd/server.go"} {
		if !parsed[must] {
			t.Fatalf("census did not parse %s under %s (%d files) — wrong module root?", must, root, len(files))
		}
	}
	// The exemption must cover a real write, or the census passes over a
	// renamed setter forever.
	if stamps != 1 {
		t.Fatalf("StampGateway writes ConsumerID %d time(s) by the census' own count, want exactly 1", stamps)
	}
	t.Logf("census: %d non-test files, %d ConsumerID write(s) outside StampGateway", len(files), len(findings))
	if len(findings) > 0 {
		t.Errorf("Entry.ConsumerID is set only by Entry.StampGateway (W06-9, B17):\n  %s", strings.Join(findings, "\n  "))
	}
}

// TestGatewayOAICensus is census pin 2: internal/oai writes no llmlog row
// except through RecordGateway and never names a knowledge key.
func TestGatewayOAICensus(t *testing.T) {
	root := gatewayCensusModuleRoot(t)
	fset := token.NewFileSet()
	var findings []string
	seen := 0
	for _, cf := range productionFiles(t, fset, root) {
		if !strings.HasPrefix(cf.rel, "internal/oai/") {
			continue
		}
		seen++
		for _, p := range llmlogRecordRefs(cf.file) {
			findings = append(findings, fmt.Sprintf("%s:%d llmlog.Record", cf.rel, fset.Position(p).Line))
		}
		for _, p := range entryFieldWrites(cf.file, "APIKeyID", "") {
			findings = append(findings, fmt.Sprintf("%s:%d Entry.APIKeyID", cf.rel, fset.Position(p).Line))
		}
	}
	if seen == 0 {
		t.Fatal("census saw no file under internal/oai")
	}
	t.Logf("census: %d internal/oai files, %d finding(s)", seen, len(findings))
	if len(findings) > 0 {
		t.Errorf("internal/oai writes /v1 rows only through llmlog.RecordGateway and never sets Entry.APIKeyID:\n  %s", strings.Join(findings, "\n  "))
	}
}

// TestGatewayCensusDetects probes both walks against testdata/census: each
// file states how many findings it must produce ("// census-want: N"). A
// census that cannot see a write passes forever.
func TestGatewayCensusDetects(t *testing.T) {
	for dir, count := range map[string]func(*ast.File) int{
		"testdata/census/consumer": func(f *ast.File) int { return len(entryFieldWrites(f, "ConsumerID", "StampGateway")) },
		"testdata/census/oai": func(f *ast.File) int {
			return len(llmlogRecordRefs(f)) + len(entryFieldWrites(f, "APIKeyID", ""))
		},
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		probes := 0
		for _, de := range entries {
			if !strings.HasSuffix(de.Name(), ".go") {
				continue
			}
			path := filepath.Join(dir, de.Name())
			src, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			line, _, _ := strings.Cut(string(src), "\n")
			want, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "// census-want:")))
			if err != nil {
				t.Fatalf("%s: first line %q is no census-want directive", path, line)
			}
			f, err := parser.ParseFile(token.NewFileSet(), path, src, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			probes++
			if got := count(f); got != want {
				t.Errorf("%s: census found %d, want %d", path, got, want)
			}
		}
		if probes < 3 {
			t.Fatalf("%s: %d probe file(s), want the full set", dir, probes)
		}
	}
}
