package handler

// Issue #417: a handler's error line about a failure of a call on the request
// context goes through logRequestError (or logResponseError, logEncodeError),
// so a client that gave up does not land on the ERROR level. This census pins
// the error lines that remain in the package, per file — every form log/slog
// writes ERROR with (issue #453 point 3): slog.Error, slog.ErrorContext,
// slog.Log and LogAttrs at LevelError, the same methods of a *slog.Logger
// (slog.Default().Error, a logger field or parameter). Each reports a
// failure that does not come from the request context: a random source, a
// marshal or parse of server-side data, a recovered panic, a wiring or
// configuration defect, a goroutine on a detached context, and the helper's
// own ERROR branch (client_abort.go).
//
// A new error line here fails this test. Route it through the helper when the
// failing call ran on the request context; when it does not, raise its file's
// count in the same diff. A removed one lowers the count.

import (
	"fmt"
	"go/ast"
	"go/build"
	"go/constant"
	"go/importer"
	"go/parser"
	"go/token"
	"go/types"
	"log/slog"
	"maps"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
)

var rawErrorLines = map[string]int{
	"auth_session.go":         1, // csrf random source
	"auth_sso.go":             7, // provider config, redirect base, PKCE, sealbox, csrf random source
	"blob.go":                 1, // dispatch-table wiring defect
	"blob_core.go":            1, // detached attribution goroutine
	"blocks_run.go":           1, // run start without a request context
	"chat_stage.go":           2, // payload canonicalization
	"client_abort.go":         1, // logFailure's ERROR branch
	"confirm_core.go":         2, // staged payload unmarshal and parse
	"context_manage.go":       1, // answer projection of a read block
	"context_store.go":        2, // detached write-log and temporal goroutines
	"health_notice.go":        1, // the database ping of a probe round, on the round's own context (issue #519)
	"ingest.go":               2, // detached write-log and temporal goroutines
	"llmlog.go":               2, // query build and row scan
	"loop_guard.go":           1, // federation hop chain without instance identity (wiring defect)
	"manage_table.go":         1, // dispatch-table wiring defect
	"mcp_archive.go":          1, // payload canonicalization
	"mcp_blob.go":             3, // detached attribution, payload canonicalization
	"mcp_confirm.go":          1, // payload canonicalization
	"mcp_issues.go":           1, // MCP tool error without a request context in view
	"mcp_recover.go":          2, // recovered panics
	"mcp_store_batch.go":      2, // payload canonicalization; an update item's edits failing at stage for a cause other than a refusal
	"mcp_update.go":           2, // content edits and payload canonicalization
	"middleware.go":           5, // credential conflicts, request id random source, recovered panic, door wiring
	"model_auth.go":           3, // trusted-proxy configuration (twice), door stacking (wiring defect)
	"oauth_consent.go":        1, // template execution
	"project_issues_write.go": 1, // detached write-log goroutine
	"query.go":                1, // embed chain resolution (no I/O)
	"session_keys.go":         2, // row scan, csrf random source
	"store_batch.go":          1, // a staged batch's update base unparseable at confirm
	"v1_admission.go":         3, // admission on a foreign context, planner or wiring defect, error outside the writer
	"webhook_github.go":       2, // sealbox, secret resolve verdict
}

// errorLineHelpers are the helpers and the number of arguments before their
// slog-style arguments (context or writer, then the message).
var errorLineHelpers = map[string]int{"logRequestError": 2, "logResponseError": 2, "logEncodeError": 2}

// TestErrorLineHelperArguments stands in for go vet's slog check, which knows
// only log/slog's own functions: the helpers' arguments after the message
// are key-value pairs with a string-literal key, or slog attributes.
func TestErrorLineHelperArguments(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	calls := 0
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, f, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			skip, ok := errorLineHelpers[id.Name]
			if !ok || call.Ellipsis.IsValid() {
				return true
			}
			calls++
			args := call.Args[skip:]
			for i := 0; i < len(args); i++ {
				switch a := args[i].(type) {
				case *ast.BasicLit:
					if a.Kind != token.STRING {
						t.Errorf("%s: key %s is no string", fset.Position(a.Pos()), a.Value)
					}
					if i+1 >= len(args) {
						t.Errorf("%s: key %s has no value", fset.Position(a.Pos()), a.Value)
					}
					i++
				case *ast.CallExpr:
					if sel, ok := a.Fun.(*ast.SelectorExpr); ok {
						if x, ok := sel.X.(*ast.Ident); ok && x.Name == "slog" {
							continue
						}
					}
					t.Errorf("%s: argument is neither a string-literal key nor a slog attribute", fset.Position(a.Pos()))
				default:
					t.Errorf("%s: argument is neither a string-literal key nor a slog attribute", fset.Position(args[i].Pos()))
				}
			}
			return true
		})
	}
	if calls == 0 {
		t.Error("checked no helper call — did the helper names change?")
	}
	t.Logf("checked %d helper calls", calls)
}

func TestHandlerErrorLinesGoThroughTheHelper(t *testing.T) {
	tp := typedHandlerPackage(t)
	got := map[string]int{}
	for _, file := range tp.files {
		ast.Inspect(file, func(n ast.Node) bool {
			if call, ok := n.(*ast.CallExpr); ok && tp.isErrorLine(call) {
				got[filepath.Base(tp.fset.Position(call.Pos()).Filename)]++
			}
			return true
		})
	}
	for _, f := range slices.Sorted(maps.Keys(got)) {
		if want := rawErrorLines[f]; got[f] > want {
			t.Errorf("%s: %d error lines, census allows %d — a failure of a call on the request context goes through logRequestError (issue #417); a line that reports no such failure raises the census in the same diff",
				f, got[f], want)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(rawErrorLines)) {
		if got[f] < rawErrorLines[f] {
			t.Errorf("%s: %d error lines, census says %d — lower the census", f, got[f], rawErrorLines[f])
		}
	}
}

// typedPackage is the package's non-test files, type-checked with log/slog
// read from its source and every other import an empty stand-in: calls of
// this package's functions and of log/slog resolve, the rest stays untyped.
type typedPackage struct {
	fset  *token.FileSet
	files []*ast.File
	info  *types.Info
	pkg   *types.Package
}

var (
	typedOnce sync.Once
	typedPkg  *typedPackage
	typedErr  error
)

func typedHandlerPackage(t *testing.T) *typedPackage {
	t.Helper()
	typedOnce.Do(func() { typedPkg, typedErr = typeCheckHandlerPackage() })
	if typedErr != nil {
		t.Fatal(typedErr)
	}
	return typedPkg
}

func typeCheckHandlerPackage() (*typedPackage, error) {
	names, err := filepath.Glob("*.go")
	if err != nil {
		return nil, err
	}
	tp := &typedPackage{fset: token.NewFileSet(), info: &types.Info{
		Defs:       map[*ast.Ident]types.Object{},
		Uses:       map[*ast.Ident]types.Object{},
		Selections: map[*ast.SelectorExpr]*types.Selection{},
		Types:      map[ast.Expr]types.TypeAndValue{},
	}}
	for _, f := range names {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(tp.fset, f, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", f, err)
		}
		tp.files = append(tp.files, file)
	}
	// Built with -trimpath (gates.sh, CI) the test binary cannot tell go/build
	// where GOROOT is; the go command that runs the test can.
	if build.Default.GOROOT == "" {
		out, err := exec.Command("go", "env", "GOROOT").Output()
		if err != nil {
			return nil, fmt.Errorf("go env GOROOT: %w", err)
		}
		build.Default.GOROOT = strings.TrimSpace(string(out))
	}
	imp := censusImporter{slog: importer.ForCompiler(tp.fset, "source", nil)}
	conf := types.Config{Importer: imp, Error: func(error) {}}
	tp.pkg, _ = conf.Check("handler", tp.fset, tp.files, tp.info)
	if s, err := imp.Import("log/slog"); err != nil || s.Scope().Lookup("ErrorContext") == nil {
		return nil, fmt.Errorf("log/slog did not type-check from source: %w", err)
	}
	return tp, nil
}

// callee resolves the function or method a call names: this package's own,
// or log/slog's; nil for everything else.
func (tp *typedPackage) callee(call *ast.CallExpr) *types.Func {
	var fn *types.Func
	switch f := ast.Unparen(call.Fun).(type) {
	case *ast.Ident:
		fn, _ = tp.info.Uses[f].(*types.Func)
	case *ast.SelectorExpr:
		if sel := tp.info.Selections[f]; sel != nil {
			fn, _ = sel.Obj().(*types.Func)
		} else {
			fn, _ = tp.info.Uses[f.Sel].(*types.Func)
		}
	}
	if fn == nil {
		return nil
	}
	return fn.Origin()
}

// isErrorLine reports whether call writes a log/slog record at ERROR: the
// package functions slog.Error and slog.ErrorContext, the same methods of a
// *slog.Logger (slog.Default().Error, a logger field or parameter), and Log
// or LogAttrs with a level of ERROR or above — or a level that is no
// constant, which may be one.
func (tp *typedPackage) isErrorLine(call *ast.CallExpr) bool {
	fn := tp.callee(call)
	if fn == nil || fn.Pkg() == nil || fn.Pkg().Path() != "log/slog" {
		return false
	}
	switch fn.Name() {
	case "Error", "ErrorContext":
		return true
	case "Log", "LogAttrs":
		if len(call.Args) < 2 {
			return false
		}
		tv := tp.info.Types[call.Args[1]]
		if tv.Value == nil {
			return true
		}
		lvl, ok := constant.Int64Val(tv.Value)
		return !ok || lvl >= int64(slog.LevelError)
	}
	return false
}

// censusImporter reads log/slog from its source and stands in for every
// other import with an empty, complete package.
type censusImporter struct{ slog types.Importer }

func (c censusImporter) Import(path string) (*types.Package, error) {
	if path == "log/slog" {
		return c.slog.Import(path)
	}
	parts := strings.Split(path, "/")
	name := parts[len(parts)-1]
	if len(parts) > 1 && len(name) > 1 && name[0] == 'v' && strings.Trim(name[1:], "0123456789") == "" {
		name = parts[len(parts)-2]
	}
	p := types.NewPackage(path, strings.ReplaceAll(name, "-", "_"))
	p.MarkComplete()
	return p, nil
}
