package handler

// Issue #453: the failure line of a block or blob write names what the write
// left behind (write_outcome: writeOutcome for a labeled bracket,
// statementOutcome for a write without a commit label), so an operator can
// tell a client that never saw its answer whether the write landed. This
// census walks every call of a store function that writes a block, a blob or
// a staged card, and requires its failure branch — the statements after the
// call up to the `if err != nil` that handles it — to carry the
// "write_outcome" key, directly or in a function of this package it calls
// there. Every direct write call names it (issue #483); one that does not
// fails this test. Name the outcome on its line.
//
// It sees the call as `x, err := store.F(…)`, `err = store.F(…)`,
// `if x, err := store.F(…); err != nil {…}` and `return store.F(…)`, in any
// block or switch/select case, under whatever name the file imports the
// store package.
//
// Limits: a call whose error is returned to the caller (`return store.F(…)`,
// a core both transports share) is counted, not followed — its callers name
// the outcome (the issue writes: writeIssueWriteError and mcpIssueError); the
// census sees the key, not which helper fills it — which classifier a path
// takes is held by the cut tests (write_outcome_cut_integration_test.go); and
// it knows only the store functions in storeWrites.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// storeWrites are the store functions that write a block, a blob or a staged
// card.
var storeWrites = []string{
	"UpsertBlock", "UpsertBlockSupersedes", "UpsertBlocks", "UpdateBlock", "DeleteBlock",
	"GuardResolve", "GuardResolveBatch", "DreamLinkResolve", "PurgeScope", "InsertChunk",
	"InsertIssueBlock", "UpdateIssueBlock", "InsertCommentBlock",
	"StagePendingWrite", "UpsertBlob", "UpdateBlobBlockRef", "DeleteBlob",
}

// writesWithoutOutcome counts, per file, the write calls whose failure line
// names no outcome. Empty since issue #483; an entry needs a reason.
var writesWithoutOutcome = map[string]int{}

func TestWriteFailureLinesNameTheOutcome(t *testing.T) {
	// The list must name store functions that exist.
	storeFuncs := map[string]bool{}
	storeFiles, err := filepath.Glob("../store/*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	for _, f := range storeFiles {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Recv == nil {
				storeFuncs[fd.Name.Name] = true
			}
		}
	}
	for _, name := range storeWrites {
		if !storeFuncs[name] {
			t.Errorf("store.%s does not exist — update storeWrites", name)
		}
	}
	writes := map[string]bool{}
	for _, n := range storeWrites {
		writes[n] = true
	}

	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	namers := map[string]bool{} // package functions whose body passes "write_outcome"
	for _, f := range names {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		files = append(files, file)
		for _, d := range file.Decls {
			if fd, ok := d.(*ast.FuncDecl); ok && fd.Body != nil && carriesOutcomeKey(fd.Body) {
				namers[fd.Name.Name] = true
			}
		}
	}

	got := map[string]int{}
	calls, returned := 0, 0
	for _, file := range files {
		c := censusFile(fset, file, writes, namers)
		calls += c.calls
		returned += c.returned
		for _, pos := range c.unnamed {
			got[filepath.Base(pos.Filename)]++
			t.Logf("%s — failure line without write_outcome", pos)
		}
	}
	if calls == 0 {
		t.Fatal("found no store write call — did the names change?")
	}
	t.Logf("%d store write calls, %d hand their error to the caller", calls, returned)
	for _, f := range slices.Sorted(maps.Keys(got)) {
		if got[f] > writesWithoutOutcome[f] {
			t.Errorf("%s: %d write calls whose failure line names no write_outcome, census allows %d — name the outcome (writeOutcome for a labeled bracket, statementOutcome otherwise)",
				f, got[f], writesWithoutOutcome[f])
		}
	}
	for _, f := range slices.Sorted(maps.Keys(writesWithoutOutcome)) {
		if got[f] < writesWithoutOutcome[f] {
			t.Errorf("%s: %d write calls without write_outcome, census says %d — lower the census", f, got[f], writesWithoutOutcome[f])
		}
	}
}

// census is what censusFile counts in one file: the store write calls, those
// whose error goes back to the caller, and the positions of those whose
// failure line names no outcome.
type census struct {
	calls, returned int
	unnamed         []token.Position
}

// censusFile walks every statement list of file for store write calls.
func censusFile(fset *token.FileSet, file *ast.File, writes map[string]bool, namers map[string]bool) census {
	var c census
	pkg := storeImportName(file)
	if pkg == "" {
		return c
	}
	ast.Inspect(file, func(n ast.Node) bool {
		// Every statement list: blocks, and the bodies of switch and select
		// cases (not blocks in go/ast).
		var list []ast.Stmt
		switch b := n.(type) {
		case *ast.BlockStmt:
			list = b.List
		case *ast.CaseClause:
			list = b.Body
		case *ast.CommClause:
			list = b.Body
		default:
			return true
		}
		for i, stmt := range list {
			name, errVar, isReturn := storeWriteCall(stmt, writes, pkg)
			if name == "" {
				continue
			}
			c.calls++
			if isReturn {
				c.returned++
				continue
			}
			rest := list[i+1:]
			if is, ok := stmt.(*ast.IfStmt); ok {
				// The call sits in the if's init: its body is the failure
				// branch when the condition is `err != nil`.
				rest = nil
				if testsNotNil(is.Cond, errVar) {
					rest = is.Body.List
				}
			}
			switch failureBranch(rest, errVar, namers) {
			case branchNamed:
			case branchReturned:
				c.returned++
			default:
				c.unnamed = append(c.unnamed, fset.Position(stmt.Pos()))
			}
		}
		return true
	})
	return c
}

// storeImportPath is the store package the census looks for.
const storeImportPath = "github.com/GottZ/ctx/internal/store"

// storeImportName is the name file calls the store package by — its alias,
// else "store" — and "" when the file does not import it.
func storeImportName(file *ast.File) string {
	for _, imp := range file.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != storeImportPath {
			continue
		}
		if imp.Name != nil {
			return imp.Name.Name
		}
		return "store"
	}
	return ""
}

// storeWriteCall reports the store write stmt makes — `x, err := pkg.F(…)`,
// `err = pkg.F(…)`, `if x, err := pkg.F(…); …` or `return pkg.F(…)` — with
// the variable its error lands in.
func storeWriteCall(stmt ast.Stmt, writes map[string]bool, pkg string) (name, errVar string, isReturn bool) {
	var call *ast.CallExpr
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if s.Init == nil {
			return "", "", false
		}
		return storeWriteCall(s.Init, writes, pkg)
	case *ast.AssignStmt:
		if len(s.Rhs) != 1 {
			return "", "", false
		}
		call, _ = s.Rhs[0].(*ast.CallExpr)
		if id, ok := s.Lhs[len(s.Lhs)-1].(*ast.Ident); ok {
			errVar = id.Name
		}
	case *ast.ReturnStmt:
		if len(s.Results) != 1 {
			return "", "", false
		}
		call, _ = s.Results[0].(*ast.CallExpr)
		isReturn = true
	}
	if call == nil {
		return "", "", false
	}
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return "", "", false
	}
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != pkg || !writes[sel.Sel.Name] {
		return "", "", false
	}
	return sel.Sel.Name, errVar, isReturn
}

type branch int

const (
	branchUnnamed branch = iota
	branchNamed
	branchReturned
)

// failureBranch reads the statements after a write call up to the `if errVar
// != nil` that handles its error: named when one of them carries the
// "write_outcome" key or calls a function that does, returned when the error
// goes back to the caller as it is.
func failureBranch(rest []ast.Stmt, errVar string, namers map[string]bool) branch {
	for _, stmt := range rest {
		if carriesOutcomeKey(stmt) || callsNamer(stmt, namers) {
			return branchNamed
		}
		if r, ok := stmt.(*ast.ReturnStmt); ok {
			for _, res := range r.Results {
				if id, ok := res.(*ast.Ident); ok && id.Name == errVar {
					return branchReturned
				}
			}
			return branchUnnamed
		}
		if is, ok := stmt.(*ast.IfStmt); ok && is.Init == nil && testsNotNil(is.Cond, errVar) {
			return branchUnnamed
		}
	}
	return branchUnnamed
}

func testsNotNil(cond ast.Expr, errVar string) bool {
	b, ok := cond.(*ast.BinaryExpr)
	if !ok || b.Op != token.NEQ {
		return false
	}
	x, ok1 := b.X.(*ast.Ident)
	y, ok2 := b.Y.(*ast.Ident)
	return ok1 && ok2 && x.Name == errVar && y.Name == "nil"
}

// carriesOutcomeKey reports whether n contains the string literal
// "write_outcome".
func carriesOutcomeKey(n ast.Node) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil && s == "write_outcome" {
				found = true
			}
		}
		return !found
	})
	return found
}

// callsNamer reports whether n calls a package function that carries the
// key.
func callsNamer(n ast.Node, namers map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok {
			if id, ok := call.Fun.(*ast.Ident); ok && namers[id.Name] {
				found = true
			}
		}
		return !found
	})
	return found
}

// TestWriteOutcomeCensusSeesEveryForm — the census finds a write in the init
// of an if and under an aliased import of the store package (issue #483).
func TestWriteOutcomeCensusSeesEveryForm(t *testing.T) {
	const src = `package handler

import st "github.com/GottZ/ctx/internal/store"

func unnamedInInit(ctx context.Context) {
	if _, err := st.UpsertBlock(ctx); err != nil {
		logRequestError(ctx, "x", "error", err)
		return
	}
}

func namedInInit(ctx context.Context) {
	if _, err := st.DeleteBlob(ctx); err != nil {
		logRequestError(ctx, "x", "error", err, "write_outcome", statementOutcome(err))
	}
}

func unnamedAliased(ctx context.Context) {
	_, err := st.UpdateBlock(ctx)
	if err != nil {
		return
	}
}

func unnamedInCase(ctx context.Context, op string) {
	switch op {
	case "x":
		_, err := st.UpsertBlock(ctx)
		if err != nil {
			return
		}
	}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string]bool{"UpsertBlock": true, "DeleteBlob": true, "UpdateBlock": true}
	c := censusFile(fset, file, writes, map[string]bool{})
	if c.calls != 4 || len(c.unnamed) != 3 {
		t.Fatalf("census = %d calls, unnamed at %v; want 4 calls, 3 unnamed (the if-init write without the key, the aliased one, the one in a case)", c.calls, c.unnamed)
	}
	if c.unnamed[0].Line != 6 || c.unnamed[1].Line != 19 || c.unnamed[2].Line != 28 {
		t.Errorf("unnamed at %v, want lines 6, 19 and 28", c.unnamed)
	}
}
