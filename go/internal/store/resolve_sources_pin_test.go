// Wave W01-5 (design/01 §4.5.4, §4.8.1a, §4.8.2 + §7 W01-5) — the container-free
// half of the gate. Three of the wave's obligations are statements about the
// SHAPE of the code, not about a row in a table, and a container suite cannot
// make them red:
//
//	(1) store.ResolveSources is the ONLY producer of derived.SourceFacts, and
//	    therefore of FlooredMax (W01-1 review finding #6: none of V13's three
//	    clauses can tell whether ScopeFloor.Apply ran at all, because the floor
//	    only ever raises — so the single-producer property IS the guarantee).
//	(2) sources (CiteGate) and SourceFacts (Validate) come out of ONE resolve
//	    result (W01-1 review finding #7): a second, independent resolution
//	    re-opens the seam where a credentials source can be left out of the echo
//	    index while still being validated.
//	(3) MissingInScope / ForeignOrUnknown are produced by the two-query split of
//	    §4.5.4 and cannot be asserted by a caller (W01-1 review note N4: today
//	    they are caller claims, and a caller that puts every source into
//	    MissingInScope switches V6, V11 and half of V13 off).
//
// (3) is enforced by construction — every field of SourceSet is unexported, so
// no package outside store can build one that carries facts — and the probe
// here is what keeps that property from being "fixed" away in a later wave.
//
// The fourth probe is the write side: SensitivityWrite.Derived is a
// SERVER-path badge. It must stay unreachable for any client, which means it
// must not appear on a wire struct and must not be set outside internal/store
// until the arm lands (D-02/D-03) — at which point this test names the new call
// site explicitly rather than growing a wildcard.
//
// Same construction as internal/guard/guard_callsite_test.go: walk the module
// root, non-_test.go only, no DB, no build tag — runs under `go test -short`.
package store_test

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
)

// productionGoFiles returns every non-test .go file under the module root,
// as paths relative to that root.
func productionGoFiles(t *testing.T) (root string, files []string) {
	t.Helper()
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	err = filepath.Walk(root, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if info.IsDir() {
			base := info.Name()
			if base == "vendor" || base == ".git" || strings.HasPrefix(base, ".gocache") || strings.HasPrefix(base, ".gotmp") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		files = append(files, rel)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if len(files) == 0 {
		t.Fatalf("found 0 production .go files under %s — the walk is not scanning the tree", root)
	}
	return root, files
}

// There was a sitesOf helper here that counted a marker string per file. It is
// gone on purpose: both pins that used it were walked past by the W01-5 review
// (#3b textual "Derived:" missed the assignment form; #4 textual "SourceFacts{"
// missed mutation entirely), and a text search over source files also matches
// its own prose — the reviewer's probe file tripped the pin from a COMMENT
// before it tripped it from code. Everything here reads the syntax tree now.

// TestSourceFactsHaveExactlyOneProducer is obligation (1) and (2) in one probe:
// derived.NewSourceFacts — the only way to build a populated SourceFacts from
// outside the derived package — may be called at exactly ONE place in production
// code, and that place is the resolve result's own accessor. Because FlooredMax
// and the two failure sets are fields of that struct and Validate reads them
// from there, one producer of the struct is one producer of all three.
//
// The first version of this pin greped for `SourceFacts{` and `.FlooredMax =`
// and the review walked past it (#4): the struct's fields were exported, so a
// production file could MUTATE a fact set — append to MissingInScope, delete
// from the three maps — using neither spelling, and the pin stayed green. That
// is closed structurally now (the fields are unexported and the accessors copy),
// and what is left to watch is the single entry point.
func TestSourceFactsHaveExactlyOneProducer(t *testing.T) {
	const want = "internal/store/resolve_sources.go"
	sites := callSitesOf(t, "NewSourceFacts")
	if len(sites) != 1 || sites[0].file != filepath.FromSlash(want) {
		t.Fatalf("derived.NewSourceFacts call sites in production code: %v, want exactly one in %s\n"+
			"a second producer re-opens W01-1 review #6/#7 and note N4: FlooredMax, the CiteGate source map "+
			"and the two failure sets would no longer be provably the same resolve result", sites, want)
	}
}

// callSitesOf reports every production call to a function of this name, over the
// parsed syntax tree — both the qualified form (derived.NewSourceFacts(…)) and
// the bare one, so a dot-import or an alias cannot slip past.
func callSitesOf(t *testing.T, name string) []badgeSite {
	t.Helper()
	root, files := productionGoFiles(t)
	fset := token.NewFileSet()
	var sites []badgeSite

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, isCall := n.(*ast.CallExpr)
			if !isCall {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.SelectorExpr:
				if fn.Sel.Name == name {
					sites = append(sites, badgeSite{rel, fset.Position(fn.Sel.Pos()).Line, "qualified call"})
				}
			case *ast.Ident:
				if fn.Name == name {
					sites = append(sites, badgeSite{rel, fset.Position(fn.Pos()).Line, "bare call"})
				}
			}
			return true
		})
	}
	return sites
}

// TestSourceSetIsNotCallerConstructible is obligation (3): the two failure sets
// of §4.5.4 exist only as the outcome of the two queries. A caller that could
// declare them would be back to the vacuum W01-1 review note N4 describes — all
// sources reported MissingInScope, all fact maps empty, Validate returns nil.
func TestSourceSetIsNotCallerConstructible(t *testing.T) {
	rt := reflect.TypeOf(store.SourceSet{})
	if rt.NumField() == 0 {
		t.Fatal("store.SourceSet has no fields — the probe cannot say anything")
	}
	for i := range rt.NumField() {
		if f := rt.Field(i); f.IsExported() {
			t.Errorf("store.SourceSet.%s is exported — a caller can then assert %s instead of resolving it", f.Name, f.Name)
		}
	}

	// The zero value must be inert rather than permissive: an empty set claims
	// nothing, and its facts carry no floored maximum, so V10/V13 fail closed.
	var empty store.SourceSet
	facts := empty.Facts()
	if facts.FlooredMax() != "" {
		t.Errorf("zero SourceSet.Facts().FlooredMax() = %q, want \"\" (fail closed)", facts.FlooredMax())
	}
	if len(facts.MissingInScope()) != 0 || len(facts.ForeignOrUnknown()) != 0 {
		t.Errorf("zero SourceSet claims %d missing / %d foreign sources, want none",
			len(facts.MissingInScope()), len(facts.ForeignOrUnknown()))
	}
	if err := derived.Validate(derived.Provenance{}, nil, derived.Target{}, facts); err == nil {
		t.Error("Validate accepted a provenance against the zero SourceSet — the empty resolve result must not validate anything")
	}
}

// TestDerivedBadgeIsServerPathOnly pins the write half. SensitivityWrite.Derived
// does two things at once (§4.8.2 source='derived' AND the S3 server-path badge
// of design/01 §4.3.1), which is exactly why every place that sets it has to be
// named here: a future caller that only wants the first effect would silently
// acquire the second.
//
// The first version of this test got both halves wrong and the review measured
// it (#3): it demanded the ABSENCE of a json tag — which is what makes an
// exported field decodable, not what protects it — and it looked for the
// literal "Derived:", so the assignment form `sw.Derived = true` walked past it.
// Both are fixed here: the tag must be `-`, and the call sites are found over
// the parsed syntax tree.
func TestDerivedBadgeIsServerPathOnly(t *testing.T) {
	// (1) The tag. `json:"-"` is the only spelling that removes the field from
	// Unmarshal AND Marshal; a missing tag decodes under the field name,
	// case-insensitively.
	f, ok := reflect.TypeOf(store.SensitivityWrite{}).FieldByName("Derived")
	if !ok {
		t.Fatal("store.SensitivityWrite has no Derived field")
	}
	if got := f.Tag.Get("json"); got != "-" {
		t.Errorf(`SensitivityWrite.Derived carries json:%q, want "-" — without it {"derived":true} sets the badge`, got)
	}

	// (2) The property that actually carries: the badge is not decodable in
	// practice either. Probed rather than asserted — this is the statement the
	// review found load-bearing, so it gets a live check and not a comment.
	var decoded store.SensitivityWrite
	if err := json.Unmarshal([]byte(`{"derived":true,"Derived":true}`), &decoded); err != nil {
		t.Fatalf("unmarshal probe: %v", err)
	}
	if decoded.Derived {
		t.Error("a JSON body set SensitivityWrite.Derived — the badge is wire-settable")
	}
	if out, err := json.Marshal(store.SensitivityWrite{Derived: true}); err != nil {
		t.Fatalf("marshal probe: %v", err)
	} else if strings.Contains(strings.ToLower(string(out)), "derived") {
		t.Errorf("Marshal emits the badge: %s", out)
	}

	// (3) The staged, HASH-BOUND payload must have no counterpart field —
	// otherwise a client could claim the server path through the confirm flow.
	if _, ok := reflect.TypeOf(store.CanonicalWrite{}).FieldByName("Derived"); ok {
		t.Error("store.CanonicalWrite carries a Derived field — the staged payload would let a client claim the server path")
	}

	// (4) The call sites, over the AST: composite key `Derived:` AND assignment
	// `x.Derived = …`. Only internal/store may, until the arm lands (D-02/D-03)
	// and is named here.
	for _, site := range derivedBadgeSites(t) {
		if dir := filepath.ToSlash(filepath.Dir(site.file)); dir != "internal/store" {
			t.Errorf("%s:%d sets .Derived (%s) — only internal/store may, until the arm lands (D-02/D-03) and is named here",
				site.file, site.line, site.form)
		}
	}
}

// badgeSite is one syntactic place that writes a field called Derived.
type badgeSite struct {
	file string
	line int
	form string // "composite key" | "assignment"
}

// derivedBadgeSites parses every production .go file and reports each write to a
// field named Derived — in a composite literal (`SensitivityWrite{Derived: …}`)
// or through an assignment (`sw.Derived = …`, `&sw.Derived`).
//
// Deliberately NAME-based rather than type-based: go/types would need a full
// package load per package, and the stricter reading is the safer one here. If a
// second, unrelated `Derived` field ever appears in production code, this test
// names it and the decision becomes visible instead of silent.
func derivedBadgeSites(t *testing.T) []badgeSite {
	t.Helper()
	root, files := productionGoFiles(t)
	fset := token.NewFileSet()
	var sites []badgeSite

	for _, rel := range files {
		f, err := parser.ParseFile(fset, filepath.Join(root, rel), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", rel, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				for _, elt := range node.Elts {
					kv, isKV := elt.(*ast.KeyValueExpr)
					if !isKV {
						continue
					}
					if id, isID := kv.Key.(*ast.Ident); isID && id.Name == "Derived" {
						sites = append(sites, badgeSite{rel, fset.Position(id.Pos()).Line, "composite key"})
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					if sel, isSel := lhs.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Derived" {
						sites = append(sites, badgeSite{rel, fset.Position(sel.Sel.Pos()).Line, "assignment"})
					}
				}
			case *ast.UnaryExpr:
				// &sw.Derived hands the address out; whoever holds it writes it.
				if node.Op != token.AND {
					return true
				}
				if sel, isSel := node.X.(*ast.SelectorExpr); isSel && sel.Sel.Name == "Derived" {
					sites = append(sites, badgeSite{rel, fset.Position(sel.Sel.Pos()).Line, "address taken"})
				}
			}
			return true
		})
	}
	return sites
}
