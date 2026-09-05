package config

import (
	"fmt"
	"go/ast"
	goparser "go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// The env-name scanner. It answers ONE question over a given set of packages:
// which env-name-shaped string literals live there that the caller has not
// classified? The two parameters — package set and allowlist — are what make
// it the single scanner for two gates (masterplan K10/A-12): T05-6 calls it
// with the ctxd package closure and the union of the three classified name
// sets; the tooling fence calls it with cmd/ctx-* and its own allowlist.
//
// Why an AST walk and not a regexp over the file text: a struct tag is ONE
// string literal whose text happens to contain env names
// (`env:"CTX_EMBED_HOST" json:"host"`). Measured on the 47-package ctxd
// closure, a text regexp returns 245 distinct names against the AST scan's 21
// — 218 of its raw hits come from the struct tags of config.go alone, and the
// rest is prose in comments, this paragraph included. Skipping ast.Field.Tag
// makes the tag one literal that does not match the anchored pattern, and parsing
// without ParseComments keeps comments out of the tree entirely. The same
// correction was made once before in this tree for the same reason
// (store/resolve_sources_pin_test.go:88-93: a text probe tripped a pin from a
// COMMENT before it tripped it from code).

// ScanPackage is one package of a scan set: the import path used for
// reporting, and the directory that holds its .go files.
type ScanPackage struct {
	ImportPath string
	Dir        string
}

// EnvNameRef is one env-name-shaped string literal found by ScanEnvNames.
type EnvNameRef struct {
	Name       string
	ImportPath string
	File       string
	Line       int
}

// String renders a finding as "NAME at file:line (package)" — the form the
// gate failures print, kept here so both callers report identically.
func (r EnvNameRef) String() string {
	return fmt.Sprintf("%s at %s:%d (%s)", r.Name, r.File, r.Line, r.ImportPath)
}

// envNamePattern is anchored on purpose: it matches a literal that IS an env
// name, never a literal that merely contains one. Both prefixes are in use in
// the tree (CTX_* for the ctx surface, CONTEXT_* for the database group).
var envNamePattern = regexp.MustCompile(`^(CTX_|CONTEXT_)[A-Z0-9_]{2,}$`)

// ScanEnvNames walks the non-test .go files of every package in pkgs and
// returns each env-name-shaped string literal that is not in allow, in
// package order and then file order. A nil allow returns every literal found,
// which is what the counting gate needs. Struct tags are skipped (see the
// file comment above); comments never enter the tree.
func ScanEnvNames(pkgs []ScanPackage, allow map[string]bool) ([]EnvNameRef, error) {
	fset := token.NewFileSet()
	var out []EnvNameRef
	for _, pkg := range pkgs {
		files, err := nonTestGoFiles(pkg.Dir)
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			file, err := goparser.ParseFile(fset, path, nil, goparser.SkipObjectResolution)
			if err != nil {
				return nil, fmt.Errorf("env scan: parse %s: %w", path, err)
			}
			out = append(out, envNameRefs(fset, file, pkg.ImportPath, allow)...)
		}
	}
	return out, nil
}

// nonTestGoFiles lists the .go files of one directory, test files excluded,
// sorted so that a failure list is stable across runs.
func nonTestGoFiles(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("env scan: read %s: %w", dir, err)
	}
	out := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, name))
	}
	sort.Strings(out)
	return out, nil
}

// envNameRefs collects the matching string literals of one parsed file.
func envNameRefs(fset *token.FileSet, file *ast.File, importPath string, allow map[string]bool) []EnvNameRef {
	var out []EnvNameRef
	var visit func(ast.Node) bool
	visit = func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.Field:
			// The tag is deliberately not visited: it is the one literal that
			// carries env names without being one. The field TYPE still is —
			// an anonymous struct nested in it keeps the same treatment — and
			// the field NAMES are identifiers, which hold no literals.
			if n.Type != nil {
				ast.Inspect(n.Type, visit)
			}
			return false
		case *ast.BasicLit:
			if n.Kind != token.STRING {
				return false
			}
			value, err := strconv.Unquote(n.Value)
			if err != nil || !envNamePattern.MatchString(value) || allow[value] {
				return false
			}
			pos := fset.Position(n.Pos())
			out = append(out, EnvNameRef{
				Name:       value,
				ImportPath: importPath,
				File:       pos.Filename,
				Line:       pos.Line,
			})
			return false
		}
		return true
	}
	ast.Inspect(file, visit)
	return out
}
