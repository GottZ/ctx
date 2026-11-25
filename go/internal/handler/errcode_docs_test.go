package handler

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestRejectClassesAreDocumented (issue #547, review round 1): every code of
// the rejection classes (errcode.go) is in the closed vocabulary docs/api.md
// and docs/mcp.md list, and they list no other — a new class (write_conflict
// was one) reaches the clients' documentation with it.
func TestRejectClassesAreDocumented(t *testing.T) {
	src, err := os.ReadFile("errcode.go")
	if err != nil {
		t.Fatal(err)
	}
	var classes []string
	for _, m := range regexp.MustCompile(`rejectClass\{http\.\w+, "([a-z_]+)"\}`).FindAllStringSubmatch(string(src), -1) {
		classes = append(classes, m[1])
	}
	slices.Sort(classes)
	if len(classes) < 20 {
		t.Fatalf("read %d classes from errcode.go: %v", len(classes), classes)
	}
	for _, doc := range []struct{ file, from, to string }{
		{"../../../docs/api.md", "the closed vocabulary is ", " — one code per rejection class"},
		{"../../../docs/mcp.md", "The codes are the ones the HTTP API uses for the same class:", "Other errors"},
	} {
		raw, err := os.ReadFile(doc.file)
		if err != nil {
			t.Fatal(err)
		}
		text := string(raw)
		i := strings.Index(text, doc.from)
		j := strings.Index(text[max(i, 0):], doc.to)
		if i < 0 || j < 0 {
			t.Fatalf("%s: the vocabulary between %q and %q is gone", doc.file, doc.from, doc.to)
		}
		var listed []string
		for _, m := range regexp.MustCompile("`([a-z_]+)`").FindAllStringSubmatch(text[i:i+j], -1) {
			listed = append(listed, m[1])
		}
		slices.Sort(listed)
		if !slices.Equal(listed, classes) {
			t.Errorf("%s lists %v, errcode.go has %v", doc.file, listed, classes)
		}
	}
}
