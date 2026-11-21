package handler

// Issue #417: a handler's error line about a failure of a call on the request
// context goes through logRequestError (or logResponseError, logEncodeError),
// so a client that gave up does not land on the ERROR level. This census pins
// the slog.Error calls that remain in the package, per file. Each reports a
// failure that does not come from the request context: a random source, a
// marshal or parse of server-side data, a recovered panic, a wiring or
// configuration defect, a goroutine on a detached context, and the helper's
// own ERROR branch (client_abort.go).
//
// A new slog.Error here fails this test. Route it through the helper when the
// failing call ran on the request context; when it does not, raise its file's
// count in the same diff. A removed one lowers the count.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
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
	"ingest.go":               2, // detached write-log and temporal goroutines
	"llmlog.go":               2, // query build and row scan
	"manage_table.go":         1, // dispatch-table wiring defect
	"mcp_archive.go":          1, // payload canonicalization
	"mcp_blob.go":             3, // detached attribution, payload canonicalization
	"mcp_confirm.go":          1, // payload canonicalization
	"mcp_issues.go":           1, // MCP tool error without a request context in view
	"mcp_recover.go":          2, // recovered panics
	"mcp_store_batch.go":      1, // payload canonicalization
	"mcp_update.go":           2, // content edits and payload canonicalization
	"middleware.go":           5, // credential conflicts, request id random source, recovered panic, door wiring
	"oauth_consent.go":        1, // template execution
	"project_issues_write.go": 1, // detached write-log goroutine
	"query.go":                1, // embed chain resolution (no I/O)
	"session_keys.go":         2, // row scan, csrf random source
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
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	fset := token.NewFileSet()
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		src, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		file, err := parser.ParseFile(fset, f, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", f, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Error" {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "slog" {
				got[f]++
			}
			return true
		})
	}
	for _, f := range slices.Sorted(maps.Keys(got)) {
		if want := rawErrorLines[f]; got[f] > want {
			t.Errorf("%s: %d slog.Error calls, census allows %d — a failure of a call on the request context goes through logRequestError (issue #417); a line that reports no such failure raises the census in the same diff",
				f, got[f], want)
		}
	}
	for _, f := range slices.Sorted(maps.Keys(rawErrorLines)) {
		if got[f] < rawErrorLines[f] {
			t.Errorf("%s: %d slog.Error calls, census says %d — lower the census", f, got[f], rawErrorLines[f])
		}
	}
}
