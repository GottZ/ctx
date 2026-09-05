package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// The two transaction sentinels only work as a PAIR: a function that raises
// one must translate it back — errors.Is, right at its pgxdb.Write call —
// into the result the straight-line code returned there. Delete the
// translation and the sentinel becomes an API error: DeleteApiKey answers a
// miss with "pgxdb: rollback requested" instead of (false, nil), and every
// caller that only checks err != nil turns a 404 into a 500.
//
// No integration test catches that. The miss paths return early, so a suite
// that asserts on the happy path stays green while the sentinel leaks; the
// sibling wave measured exactly that. This test is the guard, and it is
// structural on purpose: it needs no database, runs under -short, and fails
// per FUNCTION, so the failure names the site whose translation went missing.
//
// It does not check WHICH result a translation produces — that is what the
// per-function integration tests are for. It checks the one thing those
// silently tolerate: that a translation exists at all.

// sentinelNames are the values this package raises out of a pgxdb.Write body.
// One is imported (pgxdb.ErrRollback, the generic commit-less exit of K37) and
// one is package-private (errTxCommitted, tx.go). BOTH spellings have to be
// recognised: an imported sentinel is a SelectorExpr in the AST, not an Ident,
// and a guard that only looked for Idents would pass while every
// pgxdb.ErrRollback site sat unwatched.
var sentinelNames = []string{"pgxdb.ErrRollback", "errTxCommitted"}

// txSentinelExempt lists the functions that may name a sentinel without
// translating it: tx.go's own helper, which raises errTxCommitted FOR its
// callers and is itself covered by their translations.
var txSentinelExempt = map[string]bool{"commitThenStop": true}

func TestTxSentinelsAreTranslated(t *testing.T) {
	raises, filters := scanTxSentinels(t)

	names := make([]string, 0, len(raises))
	for fn := range raises {
		names = append(names, fn)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no sentinel raise site found at all — the scan is broken, not the code")
	}

	// Both spellings must actually occur, or the scanner has quietly stopped
	// seeing one of the two AST shapes (the SelectorExpr regression).
	seen := map[string]bool{}
	for _, fn := range names {
		for s := range raises[fn] {
			seen[s] = true
		}
	}
	for _, s := range sentinelNames {
		if !seen[s] {
			t.Errorf("no raise site found for %s — either it is gone from the "+
				"package or the scanner no longer recognises its AST shape", s)
		}
	}

	for _, fn := range names {
		t.Run(fn, func(t *testing.T) {
			for _, sentinel := range sentinelNames {
				if !raises[fn][sentinel] {
					continue
				}
				if !filters[fn][sentinel] {
					t.Errorf("%s raises %s but never translates it back "+
						"(no errors.Is(..., %s) in the same function) — a miss "+
						"would reach the caller as an error instead of its "+
						"documented result", fn, sentinel, sentinel)
				}
			}
		})
	}
}

// TestTxSentinelsHaveNoStrayTranslation is the mirror: a translation whose
// raise site was removed is dead code that hides the next regression.
func TestTxSentinelsHaveNoStrayTranslation(t *testing.T) {
	raises, filters := scanTxSentinels(t)

	names := make([]string, 0, len(filters))
	for fn := range filters {
		names = append(names, fn)
	}
	sort.Strings(names)

	for _, fn := range names {
		for _, sentinel := range sentinelNames {
			if filters[fn][sentinel] && !raises[fn][sentinel] {
				t.Errorf("%s translates %s but no longer raises it", fn, sentinel)
			}
		}
	}
}

// scanTxSentinels parses every non-test file of this package and reports, per
// top-level function, which sentinels it RAISES (returns, directly or through
// commitThenStop) and which it FILTERS (names inside an errors.Is call).
func scanTxSentinels(t *testing.T) (raises, filters map[string]map[string]bool) {
	t.Helper()
	raises = map[string]map[string]bool{}
	filters = map[string]map[string]bool{}

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(".", name), nil, 0)
		if perr != nil {
			t.Fatalf("parse %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || txSentinelExempt[fn.Name.Name] {
				continue
			}
			markTxSentinels(fn, raises, filters)
		}
	}
	return raises, filters
}

// markTxSentinels walks one function (its closures included — the raise sites
// all sit inside the pgxdb.Write body) and records raises and filters under
// the enclosing top-level function's name.
func markTxSentinels(fn *ast.FuncDecl, raises, filters map[string]map[string]bool) {
	name := fn.Name.Name
	set := func(m map[string]map[string]bool, sentinel string) {
		if m[name] == nil {
			m[name] = map[string]bool{}
		}
		m[name][sentinel] = true
	}

	ast.Inspect(fn.Body, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.ReturnStmt:
			for _, res := range node.Results {
				if s := sentinelOf(res); s != "" {
					set(raises, s)
					continue
				}
				// commitThenStop(ctx, tx) raises errTxCommitted for its caller.
				if call, ok := res.(*ast.CallExpr); ok {
					if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "commitThenStop" {
						set(raises, "errTxCommitted")
					}
				}
			}
		case *ast.CallExpr:
			sel, ok := node.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Is" {
				return true
			}
			pkg, ok := sel.X.(*ast.Ident)
			if !ok || pkg.Name != "errors" {
				return true
			}
			for _, arg := range node.Args {
				if s := sentinelOf(arg); s != "" {
					set(filters, s)
				}
			}
		}
		return true
	})
}

// sentinelOf renders an expression as a sentinel name if it is one. It must
// handle BOTH AST shapes: a bare Ident for the package-private sentinel and a
// SelectorExpr for the imported pgxdb.ErrRollback.
func sentinelOf(expr ast.Expr) string {
	var text string
	switch e := expr.(type) {
	case *ast.Ident:
		text = e.Name
	case *ast.SelectorExpr:
		pkg, ok := e.X.(*ast.Ident)
		if !ok {
			return ""
		}
		text = pkg.Name + "." + e.Sel.Name
	default:
		return ""
	}
	for _, s := range sentinelNames {
		if text == s {
			return text
		}
	}
	return ""
}
