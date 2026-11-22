package handler

// Issue #453: the failure line of a block or blob write names what the write
// left behind (write_outcome: writeOutcome for a labeled bracket,
// statementOutcome for a write without a commit label), so an operator can
// tell a client that never saw its answer whether the write landed. This
// census walks every call of a store function that writes a block, a blob, a
// staged card or a structural link, and requires its failure branch — the
// statements after the call up to the `if err != nil` that handles it — to
// carry the "write_outcome" key, directly or in a function of this package it
// calls there, on every failure line it writes about that error. Every direct
// write call names it (issue #483); one that does not fails this test. Name
// the outcome on its line.
//
// It sees the call as `x, err := store.F(…)`, `err = store.F(…)`,
// `var x, err = store.F(…)`, `if x, err := store.F(…); err != nil {…}` and
// `return store.F(…)` (one result among several too), also nested in
// another call (`x, err := wrap(store.F(…))`), in any block or switch/select
// case, under whatever name the file imports the store package. A call whose
// error is dropped — `go store.F(…)`, `defer store.F(…)`, a bare
// `store.F(…)` statement — has no failure line and is unnamed. A call inside
// a function literal whose error the literal returns (a transaction helper's
// body: `err := inTx(ctx, func(tx pgx.Tx) error { return store.F(…) })`) is
// followed to the statement that runs the literal — it passes the literal to
// a call and assigns, declares or returns that call's result — and that
// statement's failure branch decides; a literal stored in a variable, or run
// by a call that drops the error, is unnamed. A failure branch whose key sits in one sub-branch
// is named only when no other line about the same error — a call of slog, of
// a package function that writes a line, with the error variable among its
// arguments — goes without it.
//
// Limits (issue #483, the review of #490): a call through a function value
// (`f := store.F; f(…)`) is not seen — following values needs type
// information and no handler stores a write in a variable; a statement before
// the `if err != nil` that carries the key (a success line naming
// write_outcome) counts as naming it — telling success from failure there
// needs control flow; a call whose error a named function — or a function
// literal at package level — returns to its callers (`return store.F(…)`, a core both transports share) is counted, not
// followed — its callers name the outcome (the issue writes:
// writeIssueWriteError and mcpIssueError), and following them needs a call
// graph across functions; a method is neither a namer nor a line writer to
// the census (it knows functions by name, and method names repeat across
// types), so a method that logs the error without the key in one sub-branch
// beside a named one goes unseen; the census sees the key, not which helper fills it
// — which classifier a path takes is held by the cut tests
// (write_outcome_cut_integration_test.go); and it knows only the store
// functions in storeWrites.

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

// storeWrites are the store functions that write a block, a blob, a staged
// card or a structural link.
var storeWrites = []string{
	"UpsertBlock", "UpsertBlockSupersedes", "UpsertBlocks", "UpdateBlock", "DeleteBlock",
	"GuardResolve", "GuardResolveBatch", "DreamLinkResolve", "PurgeScope", "InsertChunk",
	"InsertIssueBlock", "UpdateIssueBlock", "InsertCommentBlock",
	"StagePendingWrite", "UpsertBlob", "UpdateBlobBlockRef", "DeleteBlob",
	"PutStructuralLink", "DeleteStructuralLink",
}

// writesWithoutOutcome counts, per file, the write calls whose failure line
// names no outcome. Empty since issue #483; an entry needs a reason.
var writesWithoutOutcome = map[string]int{}

// parsePackage parses the package's non-test files.
func parsePackage(t *testing.T, fset *token.FileSet) []*ast.File {
	t.Helper()
	names, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	for _, f := range names {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		files = append(files, file)
	}
	return files
}

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

	files := parsePackage(t, fset)
	pkgFuncs := packageFuncs(files)

	got := map[string]int{}
	calls, returned := 0, 0
	for _, file := range files {
		c := censusFile(fset, file, writes, pkgFuncs)
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

// pkgFuncs is what the census knows about the package's own functions (not
// methods: a method is known by its name alone here, and names repeat across
// types), by name: namers pass the "write_outcome" key in their body; loggers
// write a line (slog, or another logger) — a call of a logger that is no
// namer and passes the write's error without the key is a failure line that
// names no outcome.
type pkgFuncs struct {
	namers, loggers map[string]bool
}

// notAboutTheError are loggers whose line is about something else than the
// arguments they are handed: logEncodeError writes the failure to write a
// response body, so writeJSON — which every answer goes through — is no line
// about the write's error.
var notAboutTheError = map[string]bool{"logEncodeError": true}

// packageFuncs collects the namers and, to a fixpoint, the loggers of files.
func packageFuncs(files []*ast.File) pkgFuncs {
	pf := pkgFuncs{namers: map[string]bool{}, loggers: map[string]bool{}}
	type body struct {
		block   *ast.BlockStmt
		imports map[string]bool
	}
	bodies := map[string][]body{}
	for _, file := range files {
		imp := importNames(file)
		for _, d := range file.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil || fd.Recv != nil {
				continue
			}
			if carriesOutcomeKey(fd.Body) {
				pf.namers[fd.Name.Name] = true
			}
			bodies[fd.Name.Name] = append(bodies[fd.Name.Name], body{fd.Body, imp})
		}
	}
	for changed := true; changed; {
		changed = false
		for name, bs := range bodies {
			if pf.loggers[name] || notAboutTheError[name] {
				continue
			}
			for _, b := range bs {
				logs := false
				ast.Inspect(b.block, func(n ast.Node) bool {
					if call, ok := n.(*ast.CallExpr); ok && pf.writesLine(call, b.imports) {
						logs = true
					}
					return !logs
				})
				if logs {
					pf.loggers[name] = true
					changed = true
					break
				}
			}
		}
	}
	return pf
}

// importNames is the set of names file calls its imports by.
func importNames(file *ast.File) map[string]bool {
	out := map[string]bool{}
	for _, imp := range file.Imports {
		p, err := strconv.Unquote(imp.Path.Value)
		if err != nil {
			continue
		}
		name := p[strings.LastIndex(p, "/")+1:]
		if imp.Name != nil {
			name = imp.Name.Name
		}
		out[name] = true
	}
	return out
}

// censusCallee is the package function call calls ("" for a method, a call
// of another package or of a function value).
func censusCallee(call *ast.CallExpr) string {
	if f, ok := call.Fun.(*ast.Ident); ok {
		return f.Name
	}
	return ""
}

// slogWriters are slog's top-level functions that write a line. Its other
// functions build values (slog.Any, slog.String, slog.Group, …): a call of
// slog.Any("error", err) inside a line is that line's attribute, not a line
// of its own (issue #505).
var slogWriters = map[string]bool{
	"Debug": true, "Info": true, "Warn": true, "Error": true, "Log": true, "LogAttrs": true,
	"DebugContext": true, "InfoContext": true, "WarnContext": true, "ErrorContext": true,
}

// writesLine reports whether call writes a log line: slog's top-level
// functions that write one, or a logger of the package.
func (pf pkgFuncs) writesLine(call *ast.CallExpr, imports map[string]bool) bool {
	if sel, ok := call.Fun.(*ast.SelectorExpr); ok {
		if x, ok := sel.X.(*ast.Ident); ok && x.Name == "slog" && imports["slog"] {
			return slogWriters[sel.Sel.Name]
		}
	}
	return pf.loggers[censusCallee(call)]
}

// census is what censusFile counts in one file: the store write calls, those
// whose error goes back to the caller, the positions of those whose failure
// line names no outcome, and the names of the calls it followed out of a
// function literal to a failure line that names it.
type census struct {
	calls, returned int
	unnamed         []token.Position
	followed        []string
}

// stmtList is a statement list and the function literal whose body holds it
// (nil: a function declaration's).
type stmtList struct {
	list []ast.Stmt
	lit  *ast.FuncLit
}

// litSite is where a function literal runs: the statement of in that holds
// it, at index i.
type litSite struct {
	in stmtList
	i  int
}

// collectLists returns every statement list of file — blocks and the bodies
// of switch and select cases (not blocks in go/ast) — with its function
// literal, and where each literal runs.
func collectLists(file *ast.File) ([]stmtList, map[*ast.FuncLit]litSite) {
	var (
		lists []stmtList
		stack []ast.Node
	)
	sites := map[*ast.FuncLit]litSite{}
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		stack = append(stack, n)
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
		sl := stmtList{list: list}
		for i := len(stack) - 2; i >= 0; i-- {
			if fl, ok := stack[i].(*ast.FuncLit); ok {
				sl.lit = fl
				break
			}
			if _, ok := stack[i].(*ast.FuncDecl); ok {
				break
			}
		}
		lists = append(lists, sl)
		for i, stmt := range list {
			ast.Inspect(stmt, func(m ast.Node) bool {
				if fl, ok := m.(*ast.FuncLit); ok {
					sites[fl] = litSite{in: sl, i: i}
					return false
				}
				return true
			})
		}
		return true
	})
	return lists, sites
}

// censusFile walks every statement list of file for store write calls.
func censusFile(fset *token.FileSet, file *ast.File, writes map[string]bool, pf pkgFuncs) census {
	var c census
	pkg := storeImportName(file)
	if pkg == "" {
		return c
	}
	imports := importNames(file)
	lists, sites := collectLists(file)
	for _, sl := range lists {
		for i, stmt := range sl.list {
			name, errVar, kind := storeWriteCall(stmt, writes, pkg)
			if name == "" {
				continue
			}
			c.calls++
			if kind == callDropped {
				c.unnamed = append(c.unnamed, fset.Position(stmt.Pos()))
				continue
			}
			verdict := branchReturned
			if kind == callAssigned {
				verdict = failureBranch(statementBranch(stmt, sl.list[i+1:], errVar), errVar, pf, imports)
			}
			// A function literal that hands the error back: the statement
			// that runs it decides, up through every enclosing literal.
			lit, followed := sl.lit, false
			for verdict == branchReturned && lit != nil {
				site, ok := sites[lit]
				if !ok {
					break
				}
				outer := site.in.list[site.i]
				if !runsLiteral(outer, lit) {
					// The literal is stored (fn := func…) or its call drops
					// the error: the census cannot follow it to a failure line.
					verdict = branchUnnamed
					break
				}
				lit = site.in.lit
				if _, isReturn := outer.(*ast.ReturnStmt); isReturn {
					continue // its result goes back further
				}
				oerr := assignedErr(outer)
				verdict = failureBranch(statementBranch(outer, site.in.list[site.i+1:], oerr), oerr, pf, imports)
				followed = true
			}
			switch verdict {
			case branchNamed:
				if followed {
					c.followed = append(c.followed, name)
				}
			case branchReturned:
				c.returned++
			default:
				c.unnamed = append(c.unnamed, fset.Position(stmt.Pos()))
			}
		}
	}
	return c
}

// runsLiteral reports whether stmt runs lit and keeps the error: lit is an
// argument of a call whose result the statement assigns, declares (also in an
// if's init) or returns. A literal stored in a variable, or passed to a call
// whose result go, defer or a bare statement drops, is not followed.
func runsLiteral(stmt ast.Stmt, lit *ast.FuncLit) bool {
	var exprs []ast.Expr
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if s.Init == nil {
			return false
		}
		return runsLiteral(s.Init, lit)
	case *ast.AssignStmt:
		exprs = s.Rhs
	case *ast.DeclStmt:
		if gd, ok := s.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) == 1 {
			if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok {
				exprs = vs.Values
			}
		}
	case *ast.ReturnStmt:
		exprs = s.Results
	}
	found := false
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok {
				for _, a := range call.Args {
					if a == ast.Expr(lit) {
						found = true
					}
				}
			}
			return !found
		})
	}
	return found
}

// statementBranch is what follows stmt as its failure branch: the body of an
// `if …; errVar != nil` whose init it is, else the statements after it.
func statementBranch(stmt ast.Stmt, rest []ast.Stmt, errVar string) []ast.Stmt {
	if is, ok := stmt.(*ast.IfStmt); ok && is.Init != nil {
		if testsNotNil(is.Cond, errVar) {
			return is.Body.List
		}
		return nil
	}
	return rest
}

// assignedErr is the variable the last value stmt assigns or declares lands
// in ("" when it assigns none).
func assignedErr(stmt ast.Stmt) string {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if s.Init != nil {
			return assignedErr(s.Init)
		}
	case *ast.AssignStmt:
		if id, ok := s.Lhs[len(s.Lhs)-1].(*ast.Ident); ok {
			return id.Name
		}
	case *ast.DeclStmt:
		if gd, ok := s.Decl.(*ast.GenDecl); ok && gd.Tok == token.VAR && len(gd.Specs) == 1 {
			if vs, ok := gd.Specs[0].(*ast.ValueSpec); ok && len(vs.Names) > 0 {
				return vs.Names[len(vs.Names)-1].Name
			}
		}
	}
	return ""
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

// callKind is how a statement treats the error of the store write it makes.
type callKind int

const (
	callAssigned callKind = iota // assigned or declared: its failure branch follows
	callReturned                 // returned to the caller
	callDropped                  // discarded: go, defer, a bare call
)

// storeWriteCall reports the store write stmt makes — assigned
// (`x, err := …`, `err = …`, `var x, err = …`, an if's init), returned or
// dropped (go, defer, a bare call statement), the call itself or nested in
// another call's arguments — with the variable its error lands in.
func storeWriteCall(stmt ast.Stmt, writes map[string]bool, pkg string) (name, errVar string, kind callKind) {
	switch s := stmt.(type) {
	case *ast.IfStmt:
		if s.Init == nil {
			return "", "", 0
		}
		return storeWriteCall(s.Init, writes, pkg)
	case *ast.AssignStmt:
		if len(s.Rhs) != 1 {
			return "", "", 0
		}
		return findStoreWrite(s.Rhs[0], writes, pkg), assignedErr(s), callAssigned
	case *ast.DeclStmt:
		gd, ok := s.Decl.(*ast.GenDecl)
		if !ok || gd.Tok != token.VAR || len(gd.Specs) != 1 {
			return "", "", 0
		}
		vs, ok := gd.Specs[0].(*ast.ValueSpec)
		if !ok || len(vs.Values) != 1 {
			return "", "", 0
		}
		return findStoreWrite(vs.Values[0], writes, pkg), assignedErr(s), callAssigned
	case *ast.ReturnStmt:
		for _, res := range s.Results {
			if n := findStoreWrite(res, writes, pkg); n != "" {
				return n, "", callReturned
			}
		}
	case *ast.GoStmt:
		return findStoreWrite(s.Call, writes, pkg), "", callDropped
	case *ast.DeferStmt:
		return findStoreWrite(s.Call, writes, pkg), "", callDropped
	case *ast.ExprStmt:
		return findStoreWrite(s.X, writes, pkg), "", callDropped
	}
	return "", "", 0
}

// findStoreWrite returns the store write function e calls — itself or in a
// call's arguments, not inside a function literal (its own statements) — or
// "".
func findStoreWrite(e ast.Expr, writes map[string]bool, pkg string) string {
	found := ""
	ast.Inspect(e, func(n ast.Node) bool {
		if found != "" {
			return false
		}
		switch x := n.(type) {
		case *ast.FuncLit:
			return false
		case *ast.CallExpr:
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == pkg && writes[sel.Sel.Name] {
					found = sel.Sel.Name
					return false
				}
			}
		}
		return true
	})
	return found
}

type branch int

const (
	branchUnnamed branch = iota
	branchNamed
	branchReturned
)

// failureBranch reads the statements after a write call up to the `if errVar
// != nil` that handles its error: named when one of them carries the
// "write_outcome" key or calls a function that does and no line about the
// error goes without it, returned when the error goes back to the caller as
// it is.
func failureBranch(rest []ast.Stmt, errVar string, pf pkgFuncs, imports map[string]bool) branch {
	for _, stmt := range rest {
		if unnamedLine(stmt, errVar, pf, imports) {
			return branchUnnamed
		}
		if carriesOutcomeKey(stmt) || callsNamer(stmt, pf, imports) {
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

// unnamedLine reports whether n writes a line about errVar — a line writer
// called with it among its arguments — that neither carries the key nor is a
// namer's.
func unnamedLine(n ast.Node, errVar string, pf pkgFuncs, imports map[string]bool) bool {
	if errVar == "" || errVar == "_" {
		return false
	}
	found := false
	ast.Inspect(n, func(m ast.Node) bool {
		call, ok := m.(*ast.CallExpr)
		if !ok || found {
			return !found
		}
		if pf.writesLine(call, imports) && passesVar(call, errVar) && !carriesOutcomeKey(call) && !pf.namers[censusCallee(call)] {
			found = true
		}
		return !found
	})
	return found
}

// passesVar reports whether one of call's arguments refers to v.
func passesVar(call *ast.CallExpr, v string) bool {
	found := false
	for _, a := range call.Args {
		ast.Inspect(a, func(n ast.Node) bool {
			if id, ok := n.(*ast.Ident); ok && id.Name == v {
				found = true
			}
			return !found
		})
	}
	return found
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
func callsNamer(n ast.Node, pf pkgFuncs, imports map[string]bool) bool {
	found := false
	ast.Inspect(n, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && pf.namers[censusCallee(call)] {
			found = true
		}
		return !found
	})
	return found
}

// TestWriteOutcomeCensusSeesEveryForm — the census finds a write in the init
// of an if and under an aliased import of the store package (issue #483), in
// a var declaration, nested in another call, discarded by go, defer or a bare
// call, and inside a function literal whose error the statement that runs it
// handles (not one stored in a variable or run by a call that drops the
// error); a failure line without the key in one sub-branch of the error
// branch is unnamed even when another sub-branch names it. The error goes on
// the line as "error", err or as slog.Any("error", err) (issue #505): an
// attribute constructor of slog writes no line of its own.
func TestWriteOutcomeCensusSeesEveryForm(t *testing.T) {
	const src = `package handler

import st "github.com/GottZ/ctx/internal/store"
import "log/slog"

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

func unnamedVar(ctx context.Context) {
	var _, err = st.UpsertBlock(ctx)
	if err != nil {
		logRequestError(ctx, "x", "error", err)
	}
}

func unnamedNested(ctx context.Context) {
	b, err := wrap(st.UpdateBlock(ctx))
	if err != nil {
		logRequestError(ctx, "x", "error", err)
	}
	_ = b
}

func unnamedGo(ctx context.Context) {
	go st.DeleteBlob(ctx)
}

func unnamedDefer(ctx context.Context) {
	defer st.DeleteBlob(ctx)
}

func unnamedBare(ctx context.Context) {
	st.DeleteBlob(ctx)
}

func unnamedInSubBranch(ctx context.Context) {
	_, err := st.UpsertBlock(ctx)
	if err != nil {
		if errors.Is(err, errX) {
			logRequestError(ctx, "x", "error", err, "write_outcome", statementOutcome(err))
			return
		}
		logRequestError(ctx, "y", "error", err)
	}
}

func namedWithClientBranch(ctx context.Context) {
	_, err := st.UpsertBlock(ctx)
	if err != nil {
		if errors.Is(err, errX) {
			writeJSON(w, 404, nil)
			return
		}
		logRequestError(ctx, "y", "error", err, "write_outcome", statementOutcome(err))
	}
}

func unnamedInClosure(ctx context.Context) {
	_, err := inTx(ctx, func(tx pgx.Tx) (int, error) {
		return 0, st.DeleteBlob(ctx, tx)
	})
	if err != nil {
		logRequestError(ctx, "x", "error", err)
	}
}

func unnamedStoredLiteral(ctx context.Context) {
	fn := func(tx pgx.Tx) error {
		return st.DeleteBlob(ctx, tx)
	}
	err := inTx(ctx, fn)
	if err != nil {
		logRequestError(ctx, "x", "error", err)
	}
	slog.Info("later", "write_outcome", "committed")
}

func unnamedDroppedLiteral(ctx context.Context) {
	inTx(ctx, func(tx pgx.Tx) error {
		return st.DeleteBlob(ctx, tx)
	})
	slog.Info("later", "write_outcome", "committed")
}

func namedInClosure(ctx context.Context) {
	err := inTx(ctx, func(tx pgx.Tx) error {
		_, err := st.UpdateBlock(ctx, tx)
		return err
	})
	if err != nil {
		logRequestError(ctx, "x", "error", err, "write_outcome", writeOutcome(err))
	}
}

func namedSlogAny(ctx context.Context) {
	_, err := st.UpsertBlock(ctx)
	if err != nil {
		slog.Warn("x", slog.Any("error", err), "write_outcome", statementOutcome(err))
	}
}

func unnamedSlogAny(ctx context.Context) {
	_, err := st.UpsertBlock(ctx)
	if err != nil {
		slog.Error("x", slog.Any("error", err))
	}
}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "probe.go", src, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	writes := map[string]bool{"UpsertBlock": true, "DeleteBlob": true, "UpdateBlock": true}
	// The probe calls the package's own line writers (logRequestError).
	c := censusFile(fset, file, writes, packageFuncs(append(parsePackage(t, fset), file)))
	var lines []int
	for _, pos := range c.unnamed {
		lines = append(lines, pos.Line)
	}
	want := []int{7, 20, 29, 37, 44, 52, 56, 60, 64, 87, 96, 107, 130}
	if c.calls != 17 || !slices.Equal(lines, want) {
		t.Fatalf("census = %d calls, unnamed at lines %v; want 17 calls, unnamed at %v", c.calls, lines, want)
	}
}

// TestWriteOutcomeCensusFollowsTheIssueLinkRoutes — the structural-link
// writes of manage issue-link-create and issue-link-delete run in a function
// literal (inIssueTx); the census follows each to the route's failure line and
// finds write_outcome there (issue #483).
func TestWriteOutcomeCensusFollowsTheIssueLinkRoutes(t *testing.T) {
	fset := token.NewFileSet()
	files := parsePackage(t, fset)
	writes := map[string]bool{}
	for _, n := range storeWrites {
		writes[n] = true
	}
	pkgFuncs := packageFuncs(files)
	named := map[string]int{}
	for _, f := range files {
		if filepath.Base(fset.Position(f.Pos()).Filename) != "context_manage_issues.go" {
			continue
		}
		for _, call := range censusFile(fset, f, writes, pkgFuncs).followed {
			named[call]++
		}
	}
	for _, fn := range []string{"PutStructuralLink", "DeleteStructuralLink"} {
		if named[fn] != 1 {
			t.Errorf("census followed store.%s to a named failure line %d times in context_manage_issues.go, want 1 (followed: %v)", fn, named[fn], named)
		}
	}
}
