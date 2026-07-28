// H13d — negative probe for the claimed capability boundary "the prompt path
// NEVER calls os/exec" (design/04 §4.8 table row 4, claimed in §2.5-a as a
// grep finding with no test behind it).
//
// The Hermes-C8 transport half rests on this: hostile block content travels
// as a JSON body (buildStreamBody → json.Marshal → http.NewRequestWithContext),
// so it crosses neither execve-argv nor a shell. A single exec.Command on a
// prompt-building package would end that property silently — grep findings do
// not go red.
//
// Two assertions, deliberately of different shape:
//
//	TestPromptPathDoesNotUseOsExec — the boundary itself: no package that
//	builds or dispatches a prompt may reach for os/exec. The package list is
//	explicit (reviewable) and guarded against staleness by deriving the
//	pipeline-owning packages from the `Pipeline:` literals in the tree.
//
//	TestProductionExecCallSitesAreEnumerated — the module-wide budget: exactly
//	the two legitimate call sites (internal/cli/project.go, git detection reads;
//	internal/events/overview_worker.go, boot-wired re-exec of the own binary).
//	A THIRD one anywhere goes red and has to be argued for explicitly.
//
// AST-based, not textual: a comment or a string mentioning exec.Command must
// not produce a finding (§2.5-a counted 7 grep lines for 2 real call sites).
// No DB, no build tag: runs under `go test -short`.
//
// Mutation that turns this RED: any exec.Command in internal/llm (or any other
// prompt-path package).
package llm

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

// promptPathPackages are the module-relative package dirs that assemble an LLM
// prompt or hand it to a backend. internal/rerank is included although it owns
// no `Pipeline:` literal: it ships document text to the cross-encoder over its
// own JSON body (§2.5-a).
var promptPathPackages = []string{
	"internal/chat",
	"internal/dream",
	"internal/handler",
	"internal/llm",
	"internal/rerank",
	"internal/rrf",
}

// knownExecCallSites are the two argued exec call sites of the whole module,
// with the number of calls each is allowed to carry. Files, not line numbers:
// a line anchor would go red on every unrelated edit above the call.
var knownExecCallSites = map[string]int{
	"internal/cli/project.go":            1,
	"internal/events/overview_worker.go": 1,
}

// moduleRoot resolves the go/ directory (this package sits at internal/llm).
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

// execCalls returns the positions of every exec.Command / exec.CommandContext /
// syscall.Exec call expression in the file.
func execCalls(file *ast.File, fset *token.FileSet) []string {
	var found []string
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		pkg, ok := sel.X.(*ast.Ident)
		if !ok {
			return true
		}
		switch {
		case pkg.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext"),
			pkg.Name == "syscall" && sel.Sel.Name == "Exec":
			found = append(found, fset.Position(call.Pos()).String())
		}
		return true
	})
	return found
}

func importsOsExec(file *ast.File) bool {
	for _, imp := range file.Imports {
		if p, err := strconv.Unquote(imp.Path.Value); err == nil && p == "os/exec" {
			return true
		}
	}
	return false
}

// pipelineLiteralCount counts `Pipeline: "…"` composite-literal fields — the
// same anchor design/04 §7-H11 uses to enumerate the prompt pipelines.
func pipelineLiteralCount(file *ast.File) int {
	n := 0
	ast.Inspect(file, func(node ast.Node) bool {
		kv, ok := node.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Pipeline" {
			return true
		}
		if lit, ok := kv.Value.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			n++
		}
		return true
	})
	return n
}

func TestPromptPathDoesNotUseOsExec(t *testing.T) {
	root := moduleRoot(t)
	allowed := make(map[string]bool, len(promptPathPackages))
	for _, p := range promptPathPackages {
		allowed[p] = true
	}

	pipelineSites := 0
	pipelineDirs := map[string]bool{}
	scanned := map[string]bool{}

	walkGoFiles(t, root, func(rel string, file *ast.File, fset *token.FileSet) {
		dir := filepath.ToSlash(filepath.Dir(rel))
		if n := pipelineLiteralCount(file); n > 0 {
			pipelineSites += n
			pipelineDirs[dir] = true
		}
		if !allowed[dir] {
			return
		}
		scanned[dir] = true
		if importsOsExec(file) {
			t.Errorf("%s imports os/exec — the prompt path must not reach a subprocess (Hermes-C8 transport half, §2.5-a)", rel)
		}
		for _, pos := range execCalls(file, fset) {
			t.Errorf("%s: exec/syscall call on the prompt path at %s — hostile block content must never cross execve-argv or a shell", rel, pos)
		}
	})

	// Staleness guard 1: every package that owns a prompt pipeline must be on
	// the list. A new prompt-building package is caught the moment it logs one.
	for dir := range pipelineDirs {
		if !allowed[dir] {
			t.Errorf("package %s owns a Pipeline: literal but is missing from promptPathPackages — extend the list or argue why it is not a prompt path", dir)
		}
	}
	// Staleness guard 2: the scan must have found the pipelines at all — a
	// refactor that renames the field would otherwise empty this test silently.
	if pipelineSites < 12 {
		t.Errorf("found %d Pipeline: literals, want >= 12 — the enumeration anchor moved (design/04 §7-H11 counts 12)", pipelineSites)
	}
	// Staleness guard 3: every listed package must actually exist and be read.
	for _, p := range promptPathPackages {
		if !scanned[p] {
			t.Errorf("promptPathPackages lists %s, but no non-test .go file was scanned there — the path is stale", p)
		}
	}
}

func TestProductionExecCallSitesAreEnumerated(t *testing.T) {
	root := moduleRoot(t)
	got := map[string][]string{}

	walkGoFiles(t, root, func(rel string, file *ast.File, fset *token.FileSet) {
		if calls := execCalls(file, fset); len(calls) > 0 {
			got[rel] = append(got[rel], calls...)
		}
	})

	for rel, calls := range got {
		want, known := knownExecCallSites[rel]
		if !known {
			t.Errorf("NEW exec call site %s (%v) — subprocess spawning is an argued exception, not a default. Add it to knownExecCallSites with its reason, or remove the call", rel, calls)
			continue
		}
		if len(calls) != want {
			t.Errorf("%s carries %d exec calls (%v), want %d", rel, len(calls), calls, want)
		}
	}
	var missing []string
	for rel := range knownExecCallSites {
		if _, ok := got[rel]; !ok {
			missing = append(missing, rel)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("known exec call sites vanished: %v — the enumeration no longer scans them (stale paths make this gate blind)", missing)
	}
}
