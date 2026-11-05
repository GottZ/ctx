package auth

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

// authImportPath is this package's import path; a reference through it is
// what the census counts outside the package.
const authImportPath = "github.com/GottZ/ctx/internal/auth"

// TestAuthenticateCallSitesGuarded is the census of design/01 §4.3 (W01-4).
// Authenticate hashes whatever string it is handed, so a caller that feeds it
// a user-supplied credential directly skips the model-key refusal of
// AuthenticateRaw and sends a ctxm_ key to ctx_auth as a query parameter. Every
// non-test .go file of the module is parsed — the parser ignores build
// constraints, so integration-only and OS-specific files count too. Findings:
// every reference to Authenticate (a call or a function value) other than the
// one direct call in AuthenticateRaw, a //go:linkname pull of it, and a string
// literal naming ctx_auth( outside this package (a second way to the SQL
// function). AuthenticateByID is a different identifier and stays out: it
// takes a key id the server resolved itself, never user input.
func TestAuthenticateCallSitesGuarded(t *testing.T) {
	root := moduleRoot(t)
	fset := token.NewFileSet()
	var findings []string
	files := 0
	parsed := map[string]bool{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Not part of the build: dependency trees (web/node_modules
			// ships Go ports of npm packages), fixtures, hidden dirs.
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
		files++
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		parsed[filepath.ToSlash(rel)] = true
		inAuth := filepath.ToSlash(filepath.Dir(rel)) == "internal/auth"
		for _, pos := range authenticateRefs(f, inAuth) {
			findings = append(findings, fmt.Sprintf("%s:%d", filepath.ToSlash(rel), fset.Position(pos).Line))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("census walk %s: %v", root, err)
	}
	// A census over a wrong root finds nothing and passes — so it must have
	// read the files that hold the knowledge-auth entry points.
	for _, must := range []string{"internal/auth/auth.go", "internal/handler/middleware.go", "internal/handler/auth_session.go", "internal/handler/oauth.go", "cmd/ctxd/server.go"} {
		if !parsed[must] {
			t.Fatalf("census did not parse %s under %s (%d files) — wrong module root?", must, root, files)
		}
	}
	t.Logf("census: %d non-test files parsed, %d direct Authenticate reference(s) outside AuthenticateRaw", files, len(findings))
	for _, f := range findings {
		t.Logf("  %s", f)
	}
	if len(findings) > 0 {
		t.Errorf("route these through auth.AuthenticateRaw (model-key refusal before SanitizeKey):\n  %s",
			strings.Join(findings, "\n  "))
	}
}

// TestAuthenticateCensusDetects probes the census itself: a census that
// cannot see a reference passes forever. Each source is one shape a direct
// reference can take.
func TestAuthenticateCensusDetects(t *testing.T) {
	for _, tc := range []struct {
		name   string
		inAuth bool
		src    string
		want   int
	}{
		{"qualified call", false, `package x
import "github.com/GottZ/ctx/internal/auth"
func f() { _, _ = auth.Authenticate(nil, nil, "") }`, 1},
		{"aliased import, function value", false, `package x
import a "github.com/GottZ/ctx/internal/auth"
var g = a.Authenticate`, 1},
		{"dot import", false, `package x
import . "github.com/GottZ/ctx/internal/auth"
func f() { _, _ = Authenticate(nil, nil, "") }`, 1},
		{"a wrapper named AuthenticateRaw outside the package is no exemption", false, `package x
import "github.com/GottZ/ctx/internal/auth"
func AuthenticateRaw() { _, _ = auth.Authenticate(nil, nil, "") }`, 1},
		{"sibling identifiers do not count", false, `package x
import "github.com/GottZ/ctx/internal/auth"
func f() { _, _ = auth.AuthenticateByID(nil, nil, ""); _, _ = auth.AuthenticateRaw(nil, nil, "") }`, 0},
		{"another package's Authenticate does not count", false, `package x
import other "example.com/other"
func f() { other.Authenticate() }`, 0},
		{"inside the package: only AuthenticateRaw may call it", true, `package auth
func Authenticate(a, b any, c string) (any, error) { return nil, nil }
func AuthenticateRaw(a, b any, c string) (any, error) { return Authenticate(a, b, c) }
func other() { _, _ = Authenticate(nil, nil, "") }
var h = Authenticate`, 2},
		{"AuthenticateRaw may call it once, directly — nothing else", true, `package auth
func AuthenticateRaw(a, b any, c string) (any, error) {
	leak := Authenticate
	_, _ = leak(a, b, c)
	_, _ = Authenticate(a, b, c)
	return Authenticate(a, b, c)
}`, 2},
		{"go:linkname pull", false, `package x
import _ "unsafe"
//go:linkname leak github.com/GottZ/ctx/internal/auth.Authenticate
func leak(a, b any, c string) (any, error)`, 1},
		{"go:linkname to a sibling does not count", false, `package x
import _ "unsafe"
//go:linkname byID github.com/GottZ/ctx/internal/auth.AuthenticateByID
func byID(a, b any, c string) (any, error)`, 0},
		{"the SQL function named outside the package", false, `package x
const q = "SELECT is_valid FROM ctx_auth($1)"`, 1},
		{"the by-id SQL sibling does not count", false, `package x
const q = "SELECT is_valid FROM ctx_auth_by_id($1::uuid)"`, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, err := parser.ParseFile(token.NewFileSet(), "probe.go", tc.src, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(authenticateRefs(f, tc.inAuth)); got != tc.want {
				t.Errorf("census found %d reference(s), want %d", got, tc.want)
			}
		})
	}
}

// authenticateRefs returns the position of every way f reaches
// auth.Authenticate or its SQL function other than through AuthenticateRaw.
// Outside the package: a selector on any local name of the auth import
// (default, alias), a bare identifier under a dot import, a //go:linkname pull,
// a string literal naming ctx_auth(. Inside the package: the bare identifier
// anywhere except the declaration itself and ONE direct call in this
// package's AuthenticateRaw.
func authenticateRefs(f *ast.File, inAuth bool) []token.Pos {
	var refs []token.Pos
	for _, cg := range f.Comments {
		for _, c := range cg.List {
			if fields := strings.Fields(c.Text); len(fields) == 3 && fields[0] == "//go:linkname" && fields[2] == authImportPath+".Authenticate" {
				refs = append(refs, c.Pos())
			}
		}
	}
	if !inAuth {
		ast.Inspect(f, func(n ast.Node) bool {
			if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING && strings.Contains(lit.Value, "ctx_auth(") {
				refs = append(refs, lit.Pos())
			}
			return true
		})
	}

	qualifiers := map[string]bool{}
	bare := inAuth
	for _, imp := range f.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err != nil || p != authImportPath {
			continue
		}
		switch {
		case imp.Name == nil:
			qualifiers["auth"] = true
		case imp.Name.Name == ".":
			bare = true
		case imp.Name.Name != "_":
			qualifiers[imp.Name.Name] = true
		}
	}
	if len(qualifiers) == 0 && !bare {
		return refs
	}
	skip := map[*ast.Ident]bool{} // Sel idents, declarations, the one sanctioned call
	for _, decl := range f.Decls {
		sanctioned := false
		if fd, ok := decl.(*ast.FuncDecl); ok {
			skip[fd.Name] = true // a declaration, not a reference
			sanctioned = inAuth && fd.Recv == nil && fd.Name.Name == "AuthenticateRaw"
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CallExpr:
				if fn, ok := n.Fun.(*ast.Ident); ok && sanctioned && fn.Name == "Authenticate" {
					skip[fn] = true
					sanctioned = false // exactly one
				}
			case *ast.SelectorExpr:
				skip[n.Sel] = true
				if x, ok := n.X.(*ast.Ident); ok && qualifiers[x.Name] && n.Sel.Name == "Authenticate" {
					refs = append(refs, n.Pos())
				}
			case *ast.Ident:
				if bare && !skip[n] && n.Name == "Authenticate" {
					refs = append(refs, n.Pos())
				}
			}
			return true
		})
	}
	return refs
}

// moduleRoot walks up from the test's working directory to go.mod.
func moduleRoot(t *testing.T) string {
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
			t.Fatal("go.mod not found above the test directory")
		}
		dir = parent
	}
}
