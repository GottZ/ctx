package store_test

// Census gates of W01-3 that need no database.
//
//   - TestModelRowDeleteCensus (design/01 §9.5 no. 5, I-K6): no non-test Go
//     file outside internal/store/tenant.go deletes rows of
//     context_model_consumers or context_model_keys — consumers are
//     deactivated, keys revoked; only the tenant prune (step 7b) deletes.
//   - TestModelReadShapesCarryNoSecret: the store's read types have no field
//     for a hash or a plaintext, and a consumer patch has none for an expiry,
//     the kind or the peer binding (V-01-d: no path extends a key).

import (
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

var modelRowDelete = regexp.MustCompile(`(?i)DELETE\s+FROM\s+(ONLY\s+)?context_model_(consumers|keys)\b`)

func TestModelRowDeleteCensus(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	files := 0
	var hits []string
	err = filepath.WalkDir(root, func(abs string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(abs, ".go") || strings.HasSuffix(abs, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(root, abs)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		files++
		if rel == "internal/store/tenant.go" {
			return nil
		}
		b, err := os.ReadFile(abs) //nolint:gosec // G304: a path the walk of the module itself produced
		if err != nil {
			return err
		}
		// Over the whole file, not line by line: a DELETE split across lines
		// of a raw string is the same statement.
		for _, m := range modelRowDelete.FindAllIndex(b, -1) {
			line := 1 + strings.Count(string(b[:m[0]]), "\n")
			hits = append(hits, rel+":"+strconv.Itoa(line)+": "+strings.Join(strings.Fields(string(b[m[0]:m[1]])), " "))
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if files < 100 {
		t.Fatalf("walked %d Go files under %s — the census would see nothing", files, root)
	}
	if len(hits) > 0 {
		t.Errorf("model rows deleted outside the tenant prune (I-K6):\n%s", strings.Join(hits, "\n"))
	}
}

func TestModelReadShapesCarryNoSecret(t *testing.T) {
	forbidden := regexp.MustCompile(`(?i)hash|secret|token|^plaintext$|^key$|^apikey$`)
	for _, typ := range []reflect.Type{reflect.TypeFor[store.ModelConsumer](), reflect.TypeFor[store.ModelKeyInfo]()} {
		for i := range typ.NumField() {
			f := typ.Field(i)
			if forbidden.MatchString(f.Name) {
				t.Errorf("%s.%s: a read type of the model store carries a secret-shaped field", typ.Name(), f.Name)
			}
		}
	}
	patch := reflect.TypeFor[store.ModelConsumerPatch]()
	reach := regexp.MustCompile(`(?i)expir|kind|peer|tenantid`)
	for i := range patch.NumField() {
		f := patch.Field(i)
		if reach.MatchString(f.Name) {
			t.Errorf("ModelConsumerPatch.%s: a patch must not reach a key expiry, the kind, the peer binding or the tenant", f.Name)
		}
	}
	if patch.NumField() == 0 {
		t.Fatal("ModelConsumerPatch has no field — the census sees nothing")
	}
}
