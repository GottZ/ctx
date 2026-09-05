package config

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"strings"
	"testing"
	"time"
)

// The pin under contract.mode's documented precedence exception
// (design/05 §4.9).
//
// ContractConfig.Mode is registry-complete but behaviorally inert: the
// EFFECTIVE mode is resolved by internal/schemacontract, env-dominant —
// the opposite of this registry's normal DB>env>default order. Four places
// say so today and all four are true (ContractConfig's type doc,
// schemacontract's EnvContractMode doc, schemaContractBoot's doc in
// cmd/ctxd/contract.go, descriptions.go's operator text). What was missing
// is the halt: nothing stopped a future reader of cfg.Contract.Mode from
// silently reinstating DB>env precedence — and with it the bug design/03
// §4.4 documents, where a DB writer (the very actor migration_integrity
// distrusts) overrides an operator's CTX_CONTRACT_MODE=off break-glass.
//
// Why an AST walk and not a grep. `grep -rn '\.Contract\.Mode' internal cmd`
// over non-test files returns 2 hits today and BOTH are comments: the type
// doc's own "Consuming cfg.Contract.Mode anywhere …" sentence and
// cmd/ctxd/contract.go's "deliberately NOT read from
// cfgStore.Snapshot().Contract.Mode". A text pin would be red at birth,
// while the one declaration it means to allow does not contain the string at
// all — the field's tag reads `key:"contract.mode"`. Parsing without
// ParseComments keeps comments out of the tree entirely, and a struct field
// is an *ast.Field, never a selector. Same correction envscan.go and
// store/resolve_sources_pin_test.go already carry, for the same reason.
//
// Scan area: the ctxd package closure (design/05 §4.6), via
// serverRuntimePackages from envonly_test.go — the server runtime is exactly
// where reading this field would decide something. internal/cli is not in the
// closure, and nonTestGoFiles keeps test files out: store_test.go reads
// c.Contract.Mode legitimately (a branded field in the snapshot-race
// fixture), and an assertion about the registry value is not an enforcement
// decision.
//
// Known limit, stated rather than hidden: this is a SYNTAX pin. It sees every
// receiver shape (cfg.Contract.Mode, cfgStore.Snapshot().Contract.Mode,
// x.Config.Contract.Mode) and both directions (a write to the field trips it
// too), but a detour through a local copy (d := cfg.Contract; d.Mode) leaves
// no Contract selector to match. That is the same trade the neighbouring
// budget pins make deliberately (design/05 §4.6: pin the set, not a call
// syntax); a go/types walk over the whole closure would cost a type-check on
// every run to close a hole nobody reaches by accident.

// contractModeRef is one syntactic access to a Contract.Mode field.
type contractModeRef struct {
	ImportPath string
	File       string
	Line       int
}

// String renders a finding as "file:line (package)", the form envscan.go's
// EnvNameRef uses, so both gates of this package report identically.
func (r contractModeRef) String() string {
	return fmt.Sprintf("%s:%d (%s)", r.File, r.Line, r.ImportPath)
}

// contractModeRefsIn collects the Contract.Mode selector chains of one parsed
// file. Split out from the walk below so the guard test can run the very same
// matcher over a source it owns end to end.
func contractModeRefsIn(fset *token.FileSet, file *ast.File, importPath string) []contractModeRef {
	var out []contractModeRef
	ast.Inspect(file, func(node ast.Node) bool {
		sel, ok := node.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Mode" {
			return true
		}
		inner, ok := sel.X.(*ast.SelectorExpr)
		if !ok || inner.Sel.Name != "Contract" {
			return true
		}
		pos := fset.Position(sel.Sel.Pos())
		out = append(out, contractModeRef{ImportPath: importPath, File: pos.Filename, Line: pos.Line})
		return true
	})
	return out
}

// contractModeReaders walks the non-test .go files of every package in pkgs.
func contractModeReaders(pkgs []ScanPackage) ([]contractModeRef, error) {
	fset := token.NewFileSet()
	var out []contractModeRef
	for _, pkg := range pkgs {
		files, err := nonTestGoFiles(pkg.Dir)
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			file, err := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("contract.mode pin: parse %s: %w", path, err)
			}
			out = append(out, contractModeRefsIn(fset, file, pkg.ImportPath)...)
		}
	}
	return out, nil
}

// contractModeReason locates ContractConfig's type doc AT RUN TIME and names
// it as "config.go:<first>-<last>". Measured, never written down: a literal
// range in this message would be wrong the first time anything above the type
// moves, and stale line numbers in prose are the one defect this tree keeps
// paying for. Failure to find it is not a test failure — the reason text
// degrades to the symbol name, the finding itself stands on its own.
func contractModeReason() string {
	const fallback = "see ContractConfig's type doc in internal/config/config.go"
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "config.go", nil, goparser.ParseComments|goparser.SkipObjectResolution)
	if err != nil {
		return fallback
	}
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.TYPE {
			continue
		}
		for _, spec := range gen.Specs {
			ts, ok := spec.(*ast.TypeSpec)
			if !ok || ts.Name.Name != "ContractConfig" {
				continue
			}
			doc := gen.Doc
			if doc == nil {
				doc = ts.Doc
			}
			if doc == nil {
				return fallback
			}
			return fmt.Sprintf("config.go:%d-%d", fset.Position(doc.Pos()).Line, fset.Position(doc.End()).Line)
		}
	}
	return fallback
}

// TestContractModeHasNoEnforcementReader is the halt: the server runtime holds
// no reader of the registry-merged contract.mode value. A new one is a
// decision that has to be argued here, not a line that quietly flips the
// precedence of a break-glass switch.
func TestContractModeHasNoEnforcementReader(t *testing.T) {
	pkgs := serverRuntimePackages(t)
	start := time.Now()
	refs, err := contractModeReaders(pkgs)
	if err != nil {
		t.Fatalf("scan server runtime for Contract.Mode: %v", err)
	}
	t.Logf("scanned %d packages for Contract.Mode in %s", len(pkgs), time.Since(start).Round(time.Millisecond))
	reason := contractModeReason()
	for _, ref := range refs {
		t.Errorf("ContractConfig.Mode is read at %s — that field is registry-complete but behaviorally "+
			"inert (%s). Resolve the effective mode through schemacontract.ResolveMode, which is "+
			"env-dominant; the registry-merged value carries DB>env precedence and would let a DB "+
			"writer override the CTX_CONTRACT_MODE break-glass", ref, reason)
	}
}

// TestContractModePinMatchesEveryReceiverShape guards the matcher itself: a
// pin that cannot fire is not a pin, and the walk above is green on an empty
// result either way. The expectation is not a count but the set of lines
// marked MATCH in the source below, so the two cannot drift apart.
func TestContractModePinMatchesEveryReceiverShape(t *testing.T) {
	const src = `package probe

type contractLike struct {
	Mode            string
	RecheckInterval int
}

type cfgLike struct {
	Contract contractLike
	Digest   contractLike
}

func direct(c cfgLike) string               { return c.Contract.Mode }         // MATCH
func viaCall(f func() cfgLike) string       { return f().Contract.Mode }       // MATCH
func nested(c struct{ Cfg cfgLike }) string { return c.Cfg.Contract.Mode }     // MATCH
func assign(c *cfgLike)                     { c.Contract.Mode = "off" }        // MATCH
func neighbour(c cfgLike) int               { return c.Contract.RecheckInterval } // SKIP: the hot key has no exception
func sibling(c cfgLike) string              { return c.Digest.Mode }           // SKIP: another group's Mode
func viaLocalCopy(c cfgLike) string         { d := c.Contract; return d.Mode } // SKIP: the documented syntax limit
`
	var want []int
	for i, line := range strings.Split(src, "\n") {
		if strings.Contains(line, "// MATCH") {
			want = append(want, i+1)
		}
	}
	if len(want) < 4 {
		t.Fatalf("probe source lost its markers: %d MATCH lines", len(want))
	}
	fset := token.NewFileSet()
	file, err := goparser.ParseFile(fset, "probe.go", src, goparser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("parse probe source: %v", err)
	}
	var got []int
	for _, ref := range contractModeRefsIn(fset, file, "probe") {
		got = append(got, ref.Line)
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("matcher hit lines %v, marked are %v — every receiver shape must trip it and nothing else", got, want)
	}
}
