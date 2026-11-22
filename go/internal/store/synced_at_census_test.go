package store_test

// Census gate of issue #452: context_project_sync_map.synced_at is compared in
// SQL only. AdvancePushBase writes '-infinity' into it for a mapping whose
// block changed while the push's PATCH was on the wire, and pgx cannot scan
// '-infinity' into a time.Time ("cannot scan -Infinity into *time.Time"), so a
// Go reader that selects the column would fail on exactly those rows. Every
// mention in a non-comment line of a non-test Go file must be an assignment
// (`synced_at = …`) or the candidate filter's comparison (`> m.synced_at`). A
// new reader has to cast or filter the value in SQL — then extend the allowed
// forms here.
//
// Each mention is checked on its own (issue #486): a line that assigns the
// column and returns it (`SET synced_at = now() RETURNING synced_at`) hands
// the raw value to Go although its first mention is an assignment.

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var syncedAtMention = regexp.MustCompile(`\bsynced_at\b`)

// syncedAtReads returns the byte offset of every mention of synced_at in line
// that is neither the target of an assignment (followed by " = ") nor the
// right operand of the candidate filter (preceded by "> m.").
func syncedAtReads(line string) []int {
	var reads []int
	for _, loc := range syncedAtMention.FindAllStringIndex(line, -1) {
		if strings.HasPrefix(line[loc[1]:], " = ") || strings.HasSuffix(line[:loc[0]], "> m.") {
			continue
		}
		reads = append(reads, loc[0])
	}
	return reads
}

func TestSyncedAtReadsAreFoundPerMention(t *testing.T) {
	for _, tc := range []struct {
		line  string
		reads int
	}{
		{"SET synced_at = now() RETURNING synced_at", 1},
		{"SET synced_at = now(),", 0},
		{"synced_at = CASE WHEN base_hash = $6 AND $4 THEN $5::timestamptz ELSE '-infinity'::timestamptz END,", 0},
		{"AND (m.forge_id = 0 OR b.updated_at > m.synced_at)", 0},
		{"SELECT synced_at FROM context_project_sync_map", 1},
		{"SET synced_at = synced_at", 1},
		{"SELECT m.synced_at, b.updated_at > m.synced_at FROM x", 1},
	} {
		if got := len(syncedAtReads(tc.line)); got != tc.reads {
			t.Errorf("%q: %d reads, want %d", tc.line, got, tc.reads)
		}
	}
}

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
			if strings.HasPrefix(trimmed, "//") {
				continue
			}
			mentions := len(syncedAtMention.FindAllStringIndex(line, -1))
			reads := syncedAtReads(line)
			for _, off := range reads {
				t.Errorf("%s:%d:%d mentions synced_at outside an assignment or the candidate filter: %q — the column can hold -infinity, which a Go time.Time cannot hold; compare it in SQL",
					filepath.ToSlash(rel), i+1, off+1, trimmed)
			}
			writers += mentions - len(reads)
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
