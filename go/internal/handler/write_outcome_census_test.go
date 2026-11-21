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
		ast.Inspect(file, func(n ast.Node) bool {
			block, ok := n.(*ast.BlockStmt)
			if !ok {
				return true
			}
			for i, stmt := range block.List {
				name, errVar, isReturn := storeWriteCall(stmt, writes)
				if name == "" {
					continue
				}
				calls++
				if isReturn {
					returned++
					continue
				}
				switch failureBranch(block.List[i+1:], errVar, namers) {
				case branchNamed:
				case branchReturned:
					returned++
				default:
					got[filepath.Base(fset.Position(stmt.Pos()).Filename)]++
					t.Logf("%s: store.%s — failure line without write_outcome", fset.Position(stmt.Pos()), name)
				}
			}
			return true
		})
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

// storeWriteCall reports the store write stmt makes — `x, err := store.F(…)`,
// `err = store.F(…)` or `return store.F(…)` — with the variable its error
// lands in.
func storeWriteCall(stmt ast.Stmt, writes map[string]bool) (name, errVar string, isReturn bool) {
	var call *ast.CallExpr
	switch s := stmt.(type) {
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
	if x, ok := sel.X.(*ast.Ident); !ok || x.Name != "store" || !writes[sel.Sel.Name] {
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
