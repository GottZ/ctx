package store_test

// Census gate of issue #452: context_project_sync_map.synced_at is compared in
// SQL only. AdvancePushBase writes '-infinity' into it for a mapping whose
// block changed while the push's PATCH was on the wire, and pgx cannot scan
// '-infinity' into a time.Time ("cannot scan -Infinity into *time.Time"), so a
// Go reader that selects the column would fail on exactly those rows. Every
// non-comment mention in a non-test Go file must be an assignment
// (`synced_at = …`) or the candidate filter's comparison (`> m.synced_at`). A
// new reader has to cast or filter the value in SQL — then extend the allowed
// forms here.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	syncedAtMention = regexp.MustCompile(`\bsynced_at\b`)
	syncedAtAllowed = regexp.MustCompile(`\bsynced_at = |> m\.synced_at\b`)
)

func TestSyncedAtIsComparedInSQLOnly(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("module root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("module root %s has no go.mod: %v", root, err)
	}
	writers := 0
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
		b, err := os.ReadFile(abs) //nolint:gosec // G304: a path the walk of the module itself produced
		if err != nil {
			return err
		}
		for i, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || !syncedAtMention.MatchString(line) {
				continue
			}
			if !syncedAtAllowed.MatchString(line) {
				t.Errorf("%s:%d mentions synced_at outside an assignment or the candidate filter: %q — the column can hold -infinity, which a Go time.Time cannot hold; compare it in SQL",
					filepath.ToSlash(rel), i+1, trimmed)
				continue
			}
			writers++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if writers == 0 {
		t.Fatalf("no mention of synced_at found under %s: the census reads the wrong tree", root)
	}
}
