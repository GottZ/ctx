package safepath

import (
	"os"
	"path/filepath"
	"testing"
)

// fixture lays down the tree both policies are measured on and answers its
// resolved root. EvalSymlinks runs on the temp root itself so the expectations
// below stay literal: on a host whose temp directory is reached through a link,
// every resolved answer would otherwise carry the resolved form while the
// expectation carried the lexical one.
//
//	<root>/real/sub          a directory
//	<root>/real/datei2       a regular file, reachable through the link
//	<root>/datei             a regular file
//	<root>/lnk       -> <root>/real
//	<root>/rellnk    -> real   (relative target)
//	<root>/dangling  -> <root>/nothing
func fixture(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve temp root: %v", err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(os.MkdirAll(filepath.Join(root, "real", "sub"), 0o755))
	must(os.WriteFile(filepath.Join(root, "real", "datei2"), []byte("x"), FileMode))
	must(os.WriteFile(filepath.Join(root, "datei"), []byte("x"), FileMode))
	must(os.Symlink(filepath.Join(root, "real"), filepath.Join(root, "lnk")))
	must(os.Symlink("real", filepath.Join(root, "rellnk")))
	must(os.Symlink(filepath.Join(root, "nothing"), filepath.Join(root, "dangling")))
	return root
}

// TestModesAreTheOwnerOnlyPair pins the two constants: every caller that used
// to declare its own 0o600 now reads them from here, so a drift would be
// silent everywhere at once.
func TestModesAreTheOwnerOnlyPair(t *testing.T) {
	if FileMode != 0o600 {
		t.Fatalf("FileMode = %v, want %v", FileMode, os.FileMode(0o600))
	}
	if DirMode != 0o700 {
		t.Fatalf("DirMode = %v, want %v", DirMode, os.FileMode(0o700))
	}
}

// TestBothPoliciesAgreeWhereTheyMay walks the cases in which the two policies
// must not differ: a path that resolves, a missing tail under a link, a
// dangling link, a path with no existing prefix at all.
func TestBothPoliciesAgreeWhereTheyMay(t *testing.T) {
	root := fixture(t)
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"the root itself", root, root},
		{"a real directory", filepath.Join(root, "real", "sub"), filepath.Join(root, "real", "sub")},
		{"a link to a directory", filepath.Join(root, "lnk"), filepath.Join(root, "real")},
		{"through a link", filepath.Join(root, "lnk", "sub"), filepath.Join(root, "real", "sub")},
		{"a missing leaf under a link", filepath.Join(root, "lnk", "neu"), filepath.Join(root, "real", "neu")},
		{"a missing tail under a link", filepath.Join(root, "lnk", "a", "b", "c"), filepath.Join(root, "real", "a", "b", "c")},
		{"a relative link target", filepath.Join(root, "rellnk", "neu"), filepath.Join(root, "real", "neu")},
		{"a dangling link", filepath.Join(root, "dangling"), filepath.Join(root, "dangling")},
		{"below a dangling link", filepath.Join(root, "dangling", "tail"), filepath.Join(root, "dangling", "tail")},
		{"a missing tree", filepath.Join(root, "fehlt", "tief", "x"), filepath.Join(root, "fehlt", "tief", "x")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := ResolvePrefix(c.in)
			if err != nil {
				t.Fatalf("ResolvePrefix(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Fatalf("ResolvePrefix(%q) = %q, want %q", c.in, got, c.want)
			}
			if got := ResolvePrefixLenient(c.in); got != c.want {
				t.Fatalf("ResolvePrefixLenient(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestResolvePrefixAbortsOnANonExistenceError is the strict half of the split.
// A regular file in the middle of a path makes EvalSymlinks answer ENOTDIR, an
// error that is NOT os.IsNotExist — the class the goldset Guard has to refuse
// rather than walk past, because a prefix it could not read is a prefix it
// cannot vouch for. ENOTDIR rather than EACCES: the suite runs as uid 0, which
// walks through any permission wall.
func TestResolvePrefixAbortsOnANonExistenceError(t *testing.T) {
	root := fixture(t)
	probe := filepath.Join(root, "datei", "child")

	if _, err := filepath.EvalSymlinks(probe); err == nil || os.IsNotExist(err) {
		t.Fatalf("fixture no longer produces a non-existence error: %v", err)
	}
	got, err := ResolvePrefix(probe)
	if err == nil {
		t.Fatalf("ResolvePrefix(%q) = %q, want an error", probe, got)
	}
	if got != "" {
		t.Fatalf("ResolvePrefix(%q) returned %q alongside the error, want an empty path", probe, got)
	}
}

// TestResolvePrefixLenientClimbsPastANonExistenceError is the lenient half. The
// same ENOTDIR is climbed past, and the answer is the RESOLVED ancestor with
// the remainder appended — not the input. The link above the file is what makes
// the difference visible: without it, "resolved ancestor" and "input unchanged"
// would be the same string and the test would prove nothing.
func TestResolvePrefixLenientClimbsPastANonExistenceError(t *testing.T) {
	root := fixture(t)
	probe := filepath.Join(root, "lnk", "datei2", "child")
	want := filepath.Join(root, "real", "datei2", "child")

	if _, err := filepath.EvalSymlinks(probe); err == nil || os.IsNotExist(err) {
		t.Fatalf("fixture no longer produces a non-existence error: %v", err)
	}
	if _, err := ResolvePrefix(probe); err == nil {
		t.Fatalf("ResolvePrefix(%q) succeeded, want the strict policy to abort", probe)
	}
	got := ResolvePrefixLenient(probe)
	if got == probe {
		t.Fatalf("ResolvePrefixLenient(%q) answered the input unchanged, want the resolved ancestor", probe)
	}
	if got != want {
		t.Fatalf("ResolvePrefixLenient(%q) = %q, want %q", probe, got, want)
	}
}

// TestASymlinkLoopSplitsTheTwoPolicies is the second error class of the same
// shape and the reason the split is named rather than incidental: ELOOP is not
// os.IsNotExist either.
func TestASymlinkLoopSplitsTheTwoPolicies(t *testing.T) {
	root := fixture(t)
	loop := filepath.Join(root, "loop")
	if err := os.Symlink(loop, loop); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolvePrefix(loop); err == nil {
		t.Fatalf("ResolvePrefix(%q) succeeded, want the strict policy to abort", loop)
	}
	if got := ResolvePrefixLenient(loop); got != loop {
		t.Fatalf("ResolvePrefixLenient(%q) = %q, want the ancestor walk to rebuild the input", loop, got)
	}
}

// TestARelativePathWithoutAnExistingPrefixIsAnsweredUnchanged pins the end of
// the walk for a relative input: it climbs to ".", which resolves, so the
// answer is the input rebuilt. Both policies do it, and the strict one does it
// without an error.
func TestARelativePathWithoutAnExistingPrefixIsAnsweredUnchanged(t *testing.T) {
	const probe = "relativ/nicht/da"
	got, err := ResolvePrefix(probe)
	if err != nil {
		t.Fatalf("ResolvePrefix(%q): %v", probe, err)
	}
	if got != probe {
		t.Fatalf("ResolvePrefix(%q) = %q, want the input unchanged", probe, got)
	}
	if got := ResolvePrefixLenient(probe); got != probe {
		t.Fatalf("ResolvePrefixLenient(%q) = %q, want the input unchanged", probe, got)
	}
}
