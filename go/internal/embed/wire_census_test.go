package embed

// The census of the gateway body's length functions (FOLGE-E hardening 1).

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestWireLengthHasOneDefinition is the census of the length functions: the
// tree's production code defines WireStringLen and WireBodyLen (in any
// spelling) once, here — the route imports them (FOLGE-E hardening 1: the
// route's own wireStringLen moved here), a copy drifts from the writer.
func TestWireLengthHasOneDefinition(t *testing.T) {
	// The package directory comes from the working directory, not from
	// runtime.Caller: under -trimpath (go/gates.sh, FOLGE-T) the caller's file
	// is the import path, which the filesystem does not know.
	self, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	internal := filepath.Dir(self)
	defs := map[string][]string{}
	err = filepath.WalkDir(internal, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return err
		}
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		for _, decl := range f.Decls {
			if fn, ok := decl.(*ast.FuncDecl); ok {
				switch name := strings.ToLower(fn.Name.Name); name {
				case "wirestringlen", "wirebodylen":
					rel, _ := filepath.Rel(internal, path)
					defs[name] = append(defs[name], rel+":"+fn.Name.Name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string][]string{
		"wirestringlen": {"embed/wire_body.go:WireStringLen"},
		"wirebodylen":   {"embed/wire_body.go:WireBodyLen", "embed/wire_body.go:wireBodyLen"},
	}
	for name, w := range want {
		if strings.Join(defs[name], ",") != strings.Join(w, ",") {
			t.Errorf("definitions of %s: %v, want %v", name, defs[name], w)
		}
	}
}
