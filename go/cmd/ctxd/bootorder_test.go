package main

// F-W2 (design/04 §4.2, Fremd-Review F-2/F-4): where main() loads the
// instance identity. The boot step must come right after the migrations that
// create the row — before any other boot step writes to the database, so a
// boot without identity aborts with nothing done — and in any case before
// the scheduler spawns its detached contexts and before NewRouter freezes the
// identity into its first middleware. A source pin: the order of the
// statements in main's body, which no runtime test of a single boot step can
// see. Red when the step moves after `go scheduler.Run` or `NewRouter`, or
// away from the migrations.

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

func TestMainLoadsIdentityRightAfterMigrations(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body.List
		}
	}
	if body == nil {
		t.Fatal("main.go has no func main")
	}
	// calls reports whether n contains a call of pkg.name (pkg "" = a plain
	// identifier or a method selector of any receiver).
	calls := func(n ast.Node, pkg, name string) bool {
		found := false
		ast.Inspect(n, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := c.Fun.(type) {
			case *ast.Ident:
				found = found || (pkg == "" && fun.Name == name)
			case *ast.SelectorExpr:
				x, _ := fun.X.(*ast.Ident)
				found = found || (fun.Sel.Name == name && (pkg == "" || (x != nil && x.Name == pkg)))
			}
			return !found
		})
		return found
	}
	at := map[string]int{}
	for i, st := range body {
		switch {
		case calls(st, "store", "RunMigrations"):
			at["migrations"] = i
		case calls(st, "fedhop", "SetDefaultIdentity") && calls(st, "", "bootInstanceIdentity"):
			at["identity"] = i
		case calls(st, "", "NewRouter") || calls(st, "", "newRouter"):
			at["router"] = i
		}
		if g, ok := st.(*ast.GoStmt); ok && calls(g, "", "Run") {
			if sel, ok := g.Call.Fun.(*ast.SelectorExpr); ok {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "scheduler" {
					at["scheduler"] = i
				}
			}
		}
	}
	for _, k := range []string{"migrations", "identity", "scheduler", "router"} {
		if _, ok := at[k]; !ok {
			t.Fatalf("main(): no top-level statement for %s found (have %v)", k, at)
		}
	}
	if at["identity"] != at["migrations"]+1 {
		t.Errorf("main(): fedhop.SetDefaultIdentity(bootInstanceIdentity(…)) is statement %d, the migrations %d — it must follow them directly (%s)",
			at["identity"], at["migrations"], fset.Position(body[at["identity"]].Pos()))
	}
	if at["identity"] > at["scheduler"] || at["identity"] > at["router"] {
		t.Errorf("main(): identity at statement %d, go scheduler.Run at %d, NewRouter at %d — the identity must come first",
			at["identity"], at["scheduler"], at["router"])
	}
}

// TestMainInstallsPoolObserverBeforeScheduler — F-W4 (design/04 §4.2, §4.4):
// the backend pool is the boot's fedhop observer, installed by the statement
// right after the pool is built and before the scheduler spawns the detached
// contexts whose wire calls report to it, and before NewRouter. Red when the
// call is missing or moves after `go scheduler.Run` or `NewRouter`.
func TestMainInstallsPoolObserverBeforeScheduler(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body.List
		}
	}
	at := map[string]int{}
	for i, st := range body {
		ast.Inspect(st, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := c.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			x, _ := sel.X.(*ast.Ident)
			switch {
			case x != nil && x.Name == "backends" && sel.Sel.Name == "NewPool":
				at["pool"] = i
			case x != nil && x.Name == "fedhop" && sel.Sel.Name == "SetDefaultObserver" && len(c.Args) == 1:
				if a, ok := c.Args[0].(*ast.Ident); ok && a.Name == "backendPool" {
					at["observer"] = i
				}
			case x != nil && x.Name == "scheduler" && sel.Sel.Name == "Run":
				if _, isGo := st.(*ast.GoStmt); isGo {
					at["scheduler"] = i
				}
			}
			return true
		})
		if es, ok := st.(*ast.AssignStmt); ok {
			for _, r := range es.Rhs {
				if c, ok := r.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && (id.Name == "NewRouter" || id.Name == "newRouter") {
						at["router"] = i
					}
				}
			}
		}
	}
	for _, k := range []string{"pool", "observer", "scheduler"} {
		if _, ok := at[k]; !ok {
			t.Fatalf("main(): no top-level statement for %s found (have %v)", k, at)
		}
	}
	if at["observer"] != at["pool"]+1 {
		t.Errorf("main(): fedhop.SetDefaultObserver(backendPool) is statement %d, backends.NewPool %d — it must follow the pool directly", at["observer"], at["pool"])
	}
	if at["observer"] > at["scheduler"] {
		t.Errorf("main(): observer at statement %d after go scheduler.Run at %d", at["observer"], at["scheduler"])
	}
	if r, ok := at["router"]; ok && at["observer"] > r {
		t.Errorf("main(): observer at statement %d after NewRouter at %d", at["observer"], r)
	}
}

// TestMainAssertsGatewayIngressRightAfterRouter — F-W8 (design/04 §4.6,
// §7.2 no. 1): the boot assert "no /v1 without its ingress protection" reads
// the ingress registration of the router main serves, so it must follow the
// router construction directly and come before the HTTP server exists. Red
// when the assert is missing, moves before the router (nothing registered
// yet) or after newHTTPServer (the server would already be built).
func TestMainAssertsGatewayIngressRightAfterRouter(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	var body []ast.Stmt
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "main" && fn.Recv == nil {
			body = fn.Body.List
		}
	}
	at := map[string]int{}
	for i, st := range body {
		ast.Inspect(st, func(n ast.Node) bool {
			c, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if id, ok := c.Fun.(*ast.Ident); ok {
				switch id.Name {
				case "newRouter":
					at["router"] = i
				case "gatewayIngressAssert":
					at["assert"] = i
				case "newHTTPServer":
					at["server"] = i
				}
			}
			return true
		})
	}
	for _, k := range []string{"router", "assert", "server"} {
		if _, ok := at[k]; !ok {
			t.Fatalf("main(): no top-level statement calling %s found (have %v)", k, at)
		}
	}
	if at["assert"] != at["router"]+1 || at["assert"] >= at["server"] {
		t.Errorf("main(): newRouter at statement %d, gatewayIngressAssert at %d, newHTTPServer at %d — the assert must follow the router directly and precede the server",
			at["router"], at["assert"], at["server"])
	}
}
