package handler

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"testing"
)

// M-W2 slice pinning, wiring half (design/05 §4.2, non-regression gate (j)).
//
// The rule is two-part and so is its gate. The ALLOCATION half — the measure
// slice is a clone, so the visible slice's backing array is never written — is
// a runtime probe (TestMW2MeasureSliceIsACopy). The WIRING half is this test:
// which of the two slices reaches which consumer. It has to be static, because
// the two slices are function locals: no test can observe the argument another
// stage received, and an integration probe cannot tell "the post-stage never
// saw the shadow type" from "the post-stage saw it and produced nothing".
//
// What it refuses is precisely the Revision-1 shape §4.2 overturned: ONE slice,
// mutated at the single source, handed to every consumer. In that shape the
// identifier measureVisibleTypes does not exist and the two SQL statements take
// visibleTypes — both halves of the table below go red.
//
// Deliberately argument-POSITION-independent: the rule is "this consumer sees
// this slice and not the other one", and a signature that gains a parameter is
// not a violation of it.
func TestShadowSlicePinning(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "query.go", nil, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse query.go: %v", err)
	}

	// want[call] = the slice expression this call MUST carry; the other one is
	// then forbidden in the same argument list.
	const (
		visible = "visibleTypes"
		measure = "measureVisibleTypes"
	)
	want := map[string]string{
		// The production fusion and the three post-fusion stages: the narrow
		// slice, always.
		"rrf.Search":            visible,
		"applyClusterBoost":     visible,
		"rrf.GraphExpandCached": visible,
		"foldAggregates":        visible,
		// The two measurement statements — and ONLY they — take the widened one.
		"rrf.SearchTx":   "p." + measure,
		"rrf.ArmRanksTx": "p." + measure,
	}
	forbidden := map[string]string{
		"rrf.Search":            measure,
		"applyClusterBoost":     measure,
		"rrf.GraphExpandCached": measure,
		"foldAggregates":        measure,
		"rrf.SearchTx":          "p." + visible,
		"rrf.ArmRanksTx":        "p." + visible,
	}

	seen := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := calleeName(call.Fun)
		w, tracked := want[name]
		if !tracked {
			return true
		}
		seen[name] = true
		args := make([]string, 0, len(call.Args))
		for _, a := range call.Args {
			args = append(args, types.ExprString(a))
		}
		if !containsArg(args, w) {
			t.Errorf("%s does not receive %s (args: %v)", name, w, args)
		}
		if bad := forbidden[name]; containsArg(args, bad) {
			t.Errorf("%s receives %s — the two slices were merged (args: %v)", name, bad, args)
		}
		return true
	})

	for name := range want {
		if !seen[name] {
			t.Errorf("call site %s not found in query.go — the wiring pin lost its subject", name)
		}
	}

	// The measurement struct is the hand-over point between the two halves: its
	// field must carry the widened slice, under a name that cannot be confused
	// with the narrow one.
	if !armSweepParamsCarries(file, measure) {
		t.Errorf("the armSweepParams literal does not set %s — the widened slice never reaches the seam", measure)
	}
}

// calleeName renders a call target as "pkg.Func" or the bare method name.
func calleeName(fun ast.Expr) string {
	switch f := fun.(type) {
	case *ast.Ident:
		return f.Name
	case *ast.SelectorExpr:
		if pkg, ok := f.X.(*ast.Ident); ok && pkg.Name != "h" {
			return pkg.Name + "." + f.Sel.Name
		}
		return f.Sel.Name
	}
	return ""
}

func containsArg(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// armSweepParamsCarries reports whether the armSweepParams composite literal
// assigns the named identifier to its visible-types field.
func armSweepParamsCarries(file *ast.File, name string) bool {
	found := false
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		id, ok := lit.Type.(*ast.Ident)
		if !ok || id.Name != "armSweepParams" {
			return true
		}
		for _, e := range lit.Elts {
			kv, ok := e.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			if types.ExprString(kv.Value) == name {
				found = true
			}
		}
		return true
	})
	return found
}
