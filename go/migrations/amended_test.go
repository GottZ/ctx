package migrations

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io/fs"
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
