package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// TestAmendedChecksums_Wellformed pins the shape of every amended entry: the
// version still ships as a file, each listed checksum is a sha256 hex digest,
// and none of them is the current file's checksum — an entry equal to the
// file it amends would mean the file was never changed, and the entry would
// silently widen what the schema contract accepts.
func TestAmendedChecksums_Wellformed(t *testing.T) {
	if len(amended) == 0 {
		t.Skip("no amended migrations")
	}
	for version, sums := range amended {
		current := currentChecksum(t, version)
		if len(sums) == 0 {
			t.Errorf("version %d: empty amended list", version)
		}
		for _, s := range sums {
			if !sha256Hex.MatchString(s) {
				t.Errorf("version %d: %q is not a lowercase sha256 hex digest", version, s)
			}
			if s == current {
				t.Errorf("version %d: amended checksum %s equals the current file's checksum", version, s)
			}
			if !IsAmendedChecksum(version, s) {
				t.Errorf("IsAmendedChecksum(%d, %s) = false for a listed checksum", version, s)
			}
		}
		if IsAmendedChecksum(version, current) {
			t.Errorf("IsAmendedChecksum(%d, current) = true; the current checksum must not be listed", version)
		}
	}
	if IsAmendedChecksum(1, strings.Repeat("d", 64)) {
		t.Error("IsAmendedChecksum accepted an unlisted version")
	}
}

// TestAmendedChecksums_Provenance ties every amended checksum to the bytes it
// stands for. Each earlier revision is kept byte for byte as it shipped, as
// testdata/<file name without .sql>.<first 8 hex digits>.sql.txt; its sha256
// must be the listed checksum, and every kept revision must be listed. Without
// the file an entry is a number copied by hand, and only another copy of the
// same number would ever disagree with it. The .txt suffix keeps the file out
// of every go/migrations/*.sql path: the embed pattern, and the git pathspec
// of the pre-commit manifest gate, which matches across directories.
func TestAmendedChecksums_Provenance(t *testing.T) {
	kept := map[string]bool{}
	for version, sums := range amended {
		for _, s := range sums {
			if !sha256Hex.MatchString(s) {
				continue // TestAmendedChecksums_Wellformed reports it
			}
			pattern := filepath.Join("testdata", fmt.Sprintf("%03d_*.%s.sql.txt", version, s[:8]))
			matches, err := filepath.Glob(pattern)
			if err != nil {
				t.Fatalf("glob %s: %v", pattern, err)
			}
			if len(matches) != 1 {
				t.Errorf("version %d, checksum %s: want exactly one shipped revision %s, got %v", version, s, pattern, matches)
				continue
			}
			content, err := os.ReadFile(matches[0])
			if err != nil {
				t.Fatalf("read %s: %v", matches[0], err)
			}
			sum := sha256.Sum256(content)
			if got := hex.EncodeToString(sum[:]); got != s {
				t.Errorf("%s: sha256 %s, but amended lists %s", matches[0], got, s)
			}
			name := strings.TrimSuffix(filepath.Base(matches[0]), "."+s[:8]+".sql.txt") + ".sql"
			if _, err := fs.Stat(FS, name); err != nil {
				t.Errorf("%s is no revision of an embedded migration: %s: %v", matches[0], name, err)
			}
			kept[matches[0]] = true
		}
	}
	all, err := filepath.Glob(filepath.Join("testdata", "*.sql.txt"))
	if err != nil {
		t.Fatalf("glob testdata: %v", err)
	}
	for _, f := range all {
		if !kept[f] {
			t.Errorf("%s: shipped revision without an amended entry", f)
		}
	}
}

func currentChecksum(t *testing.T, version int) string {
	t.Helper()
	matches, err := fs.Glob(FS, fmt.Sprintf("%03d_*.sql", version))
	if err != nil {
		t.Fatalf("glob version %d: %v", version, err)
	}
	if len(matches) != 1 {
		t.Fatalf("version %d: want exactly one embedded file, got %v", version, matches)
	}
	content, err := FS.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read %s: %v", matches[0], err)
	}
	sum := sha256.Sum256(content)
	return hex.EncodeToString(sum[:])
}
