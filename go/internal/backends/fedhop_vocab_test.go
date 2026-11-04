package backends

// Vocabulary pin of F-W3 (design/04 §4.1): internal/fedhop is a stdlib-only
// leaf and cannot import this package, so it carries its own copy of the
// sensitivity doctrine for Ctx-Sensitivity (MergeSensitivity, the grammar's
// "unknown ⇒ credentials"). This test holds the copy to the original from
// outside the leaf: the same values (every Sensitivity constant declared in
// this package, read from the source, so a new level cannot be forgotten
// here), the same rank per value (fedhop's order index = sensRank), the same
// fold (MergeSensitivity = MaxSensitivity for every pair, unknown and empty
// included) and the same fail-closed default.
//
//	go test -count=1 -run TestFedhopSensitivityVocabularyMatchesSensRank ./internal/backends/

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/fedhop"
)

// declaredSensitivities returns the values of every const of type
// Sensitivity in the non-test sources of this package.
func declaredSensitivities(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	fset := token.NewFileSet()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, e.Name(), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, spec := range g.Specs {
				vs := spec.(*ast.ValueSpec)
				if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "Sensitivity" {
					continue
				}
				for _, v := range vs.Values {
					lit, ok := v.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: Sensitivity constant that is not a string literal", fset.Position(v.Pos()))
					}
					s, _ := strconv.Unquote(lit.Value)
					out = append(out, s)
				}
			}
		}
	}
	return out
}

func TestFedhopSensitivityVocabularyMatchesSensRank(t *testing.T) {
	declared := declaredSensitivities(t)
	if len(declared) == 0 {
		t.Fatal("no Sensitivity constant found in the package sources")
	}
	vocab := fedhop.SensitivityVocabulary()
	if !slices.Equal(slices.Sorted(slices.Values(declared)), slices.Sorted(slices.Values(vocab))) {
		t.Fatalf("fedhop sensitivity vocabulary %v ≠ backends Sensitivity constants %v", vocab, declared)
	}
	for i, v := range vocab {
		if got := sensRank(Sensitivity(v)); got != i {
			t.Errorf("%q: fedhop rank %d, sensRank %d", v, i, got)
		}
	}
	probe := append(slices.Clone(declared), "", "bogus", "PUBLIC", " public")
	for _, a := range probe {
		for _, b := range probe {
			if b == "" {
				continue // "" is "header absent" in fedhop — the base alone, below
			}
			want := string(MaxSensitivity(Sensitivity(a), Sensitivity(b)))
			if got := fedhop.MergeSensitivity(a, b); got != want {
				t.Errorf("MergeSensitivity(%q, %q) = %q, MaxSensitivity = %q", a, b, got, want)
			}
		}
		if got, want := fedhop.MergeSensitivity(a, ""), string(MaxSensitivity(Sensitivity(a))); got != want {
			t.Errorf("MergeSensitivity(%q, \"\") = %q, MaxSensitivity(%q) = %q", a, got, a, want)
		}
	}
}
