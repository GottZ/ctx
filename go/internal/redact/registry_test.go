// M-W4a — the register's own enforcement: the marker strings of this package
// may not exist as literals anywhere else in go/.
//
// The property this protects is fail-CLOSED-ness of a write-time gate. The
// citation gate in internal/derived rejects a quote that carries a pipeline
// marker (G4), and it does so against Markers. As long as every writer of a
// marker names the same constant, "reader knows every marker a writer emits"
// holds by construction. The moment a writer spells a marker out as its own
// literal, the two sides drift apart silently: the writer emits it, the reader
// does not know it, and every quote carrying it passes the gate. Nothing about
// that is visible in a build, a vet run or any behavioural test — which is why
// it is checked here.
//
// AST-based, not textual, following internal/llm/exec_ban_test.go: a marker
// mentioned in a COMMENT documents the mechanism and must not produce a
// finding (internal/derived/citegate.go and internal/util/strings.go both do
// this deliberately). Only string literals count. _test.go files are out of
// scope: a test that asserts on the rendered marker bytes is exercising the
// contract, not defining a second source of truth.
//
// The needle set is DERIVED from Markers, so a marker added to the register is
// automatically also a marker the tree may no longer spell out.
//
// Mutation that turns this RED: any marker literal in a non-test .go file
// outside this package. Runs under `go test -short`, no DB, no build tag.
package redact

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// ownPackage is the one directory whose marker literals are the register
// itself. Module-relative, slash-separated.
const ownPackage = "internal/redact"

// needles returns the case-folded prefixes that identify a marker in a string
// literal. Derived from Markers so the register stays the single source: the
// trailing "]" is dropped so that an extended spelling of a marker
// ("[... truncated: 4 blocks]") is caught as well.
func needles() []string {
	out := make([]string, 0, len(Markers))
	for _, m := range Markers {
		out = append(out, strings.ToLower(strings.TrimSuffix(m, "]")))
	}
	return out
}

// moduleRoot resolves the go/ directory (this package sits at internal/redact).
func moduleRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	return root
}

// walkGoFiles calls fn for every non-test .go file below root, skipping vendor,
// dot dirs, node_modules and testdata.
func walkGoFiles(t *testing.T, root string, fn func(rel string, file *ast.File, fset *token.FileSet)) {
	t.Helper()
	fset := token.NewFileSet()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			base := info.Name()
			if base != "." && (strings.HasPrefix(base, ".") || base == "vendor" || base == "node_modules" || base == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			return perr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		fn(filepath.ToSlash(rel), f, fset)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
}

// markerLiterals returns "line: <literal>" for every string literal in the file
// whose case-folded value contains one of the needles.
func markerLiterals(file *ast.File, fset *token.FileSet, probes []string) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		val, uerr := strconv.Unquote(lit.Value)
		if uerr != nil {
			return true
		}
		lowered := strings.ToLower(val)
		for _, p := range probes {
			if strings.Contains(lowered, p) {
				found = append(found, strconv.Itoa(fset.Position(lit.Pos()).Line)+": "+p)
				return true
			}
		}
		return true
	})
	return found
}

// TestMarkerLiteralsAreRegistered is the M-W4a gate: outside internal/redact,
// the tree may not spell a redaction or truncation marker out as a literal.
func TestMarkerLiteralsAreRegistered(t *testing.T) {
	root := moduleRoot(t)
	probes := needles()
	if len(probes) == 0 {
		t.Fatal("needles() is empty — Markers carries no entry, the gate would pass vacuously")
	}

	var findings []string
	walkGoFiles(t, root, func(rel string, file *ast.File, fset *token.FileSet) {
		if rel == ownPackage || strings.HasPrefix(rel, ownPackage+"/") {
			return
		}
		for _, hit := range markerLiterals(file, fset, probes) {
			findings = append(findings, rel+":"+hit)
		}
	})
	sort.Strings(findings)

	if len(findings) != 0 {
		t.Errorf("%d unregistered marker literal(s) — every writer must name the %s constant instead:\n\t%s",
			len(findings), ownPackage, strings.Join(findings, "\n\t"))
	}
}

// TestMarkersAreNormalised guards the OTHER half of the register: readers fold
// a candidate to lower case and single spaces before comparing, so an entry
// that is not itself in that form can never match anything. A marker on the
// list that cannot fire is the same fail-open as a marker missing from it.
func TestMarkersAreNormalised(t *testing.T) {
	for i, m := range Markers {
		if m == "" {
			t.Errorf("Markers[%d] is empty — it would match every text", i)
			continue
		}
		if m != strings.ToLower(m) {
			t.Errorf("Markers[%d] = %q is not lower case — a case-folded reader can never match it", i, m)
		}
		if strings.TrimSpace(m) != m {
			t.Errorf("Markers[%d] = %q has outer whitespace — a space-collapsing reader trims it away", i, m)
		}
		if strings.Contains(m, "  ") || strings.ContainsAny(m, "\t\n\r\v\f") {
			t.Errorf("Markers[%d] = %q is not whitespace-collapsed to single ASCII spaces", i, m)
		}
	}
}

// TestRegisterIsSelfConsistent ties the exported constants to Markers: the open
// forms must actually be prefixes of what the writers emit, and every emitted
// marker must be represented on the negative list.
func TestRegisterIsSelfConsistent(t *testing.T) {
	if !strings.HasPrefix(Redacted, RedactedPrefix) {
		t.Errorf("Redacted = %q does not start with RedactedPrefix = %q — the reader would miss the writer", Redacted, RedactedPrefix)
	}
	for _, want := range []string{Redacted, Truncated} {
		covered := false
		for _, m := range Markers {
			if strings.Contains(strings.ToLower(want), m) {
				covered = true
				break
			}
		}
		if !covered {
			t.Errorf("emitted marker %q is not covered by any entry of Markers %q", want, Markers)
		}
	}
}
