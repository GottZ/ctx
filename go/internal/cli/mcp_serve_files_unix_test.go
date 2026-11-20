//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// fileFixture is an allowed directory with an outside directory beside it.
type fileFixture struct {
	roots   *mcpFileRoots
	allowed string // resolved
	outside string // resolved
}

func newFileFixture(t *testing.T) fileFixture {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	allowed := filepath.Join(base, "allowed")
	outside := filepath.Join(base, "outside")
	for _, d := range []string{allowed, outside, filepath.Join(allowed, "sub")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	write := func(path, content string) {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(allowed, "note.md"), "inside text")
	write(filepath.Join(allowed, "sub", "deep.md"), "deep text")
	write(filepath.Join(allowed, "empty.md"), "")
	write(filepath.Join(allowed, "big.md"), strings.Repeat("a", mcpContentFileMaxBytes+1))
	write(filepath.Join(allowed, "exact.md"), strings.Repeat("a", mcpContentFileMaxBytes))
	write(filepath.Join(allowed, "latin1.md"), "caf\xe9")
	write(filepath.Join(outside, "secret.md"), "outside text")
	symlink := func(target, link string) {
		if err := os.Symlink(target, link); err != nil {
			t.Fatal(err)
		}
	}
	symlink(filepath.Join(outside, "secret.md"), filepath.Join(allowed, "leak.md"))      // file link out
	symlink(outside, filepath.Join(allowed, "outdir"))                                   // directory link out
	symlink("note.md", filepath.Join(allowed, "alias.md"))                               // relative link in
	symlink(filepath.Join(allowed, "note.md"), filepath.Join(outside, "into.md"))        // link from outside in
	symlink(filepath.Join(allowed, "nothing.md"), filepath.Join(allowed, "dangling.md")) // leads nowhere
	if err := syscall.Mkfifo(filepath.Join(allowed, "fifo"), 0o600); err != nil {
		t.Fatal(err)
	}
	roots, err := newMCPFileRoots([]string{allowed})
	if err != nil {
		t.Fatal(err)
	}
	roots.cwd = filepath.Join(allowed, "sub")
	return fileFixture{roots: roots, allowed: allowed, outside: outside}
}

func TestMCPServeContentFileConfinement(t *testing.T) {
	f := newFileFixture(t)
	cases := []struct {
		name, path, want, errPart string
	}{
		{"inside", filepath.Join(f.allowed, "note.md"), "inside text", ""},
		{"relative to the working directory", "deep.md", "deep text", ""},
		{"relative climbing inside", "../note.md", "inside text", ""},
		{"symlink inside the directory", filepath.Join(f.allowed, "alias.md"), "inside text", ""},
		{"symlink from outside into the directory", filepath.Join(f.outside, "into.md"), "inside text", ""},
		{"exactly the limit", filepath.Join(f.allowed, "exact.md"), strings.Repeat("a", mcpContentFileMaxBytes), ""},
		{"outside", filepath.Join(f.outside, "secret.md"), "", "outside the directories"},
		{"relative climbing out", "../../outside/secret.md", "", "outside the directories"},
		{"file symlink leaving", filepath.Join(f.allowed, "leak.md"), "", "outside the directories"},
		{"directory symlink leaving", filepath.Join(f.allowed, "outdir", "secret.md"), "", "outside the directories"},
		{"missing", filepath.Join(f.allowed, "missing.md"), "", "does not exist"},
		// os.Root refuses to follow the absolute link (its target is not even
		// there); the refusal names the link.
		{"dangling symlink", filepath.Join(f.allowed, "dangling.md"), "", "dangling.md"},
		{"empty", filepath.Join(f.allowed, "empty.md"), "", "is empty"},
		{"over the limit", filepath.Join(f.allowed, "big.md"), "", "more than the"},
		{"not UTF-8", filepath.Join(f.allowed, "latin1.md"), "", "not UTF-8"},
		{"directory", filepath.Join(f.allowed, "sub"), "", "not a regular file"},
		{"fifo", filepath.Join(f.allowed, "fifo"), "", "not a regular file"},
		{"no path", "", "", "is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := f.roots.readContentFile(tc.path)
			if tc.errPart == "" {
				if err != nil || got != tc.want {
					t.Fatalf("read %q = %.40q, %v; want %.40q", tc.path, got, err, tc.want)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.errPart) {
				t.Fatalf("read %q = %.40q, %v; want an error containing %q", tc.path, got, err, tc.errPart)
			}
		})
	}
}

func TestMCPServeOutputFileConfinement(t *testing.T) {
	f := newFileFixture(t)
	cases := []struct {
		name, path, errPart string
	}{
		{"new file inside", filepath.Join(f.allowed, "new.md"), ""},
		{"relative new file", "fresh.md", ""},
		{"existing file inside", filepath.Join(f.allowed, "note.md"), ""},
		{"through a symlink inside", filepath.Join(f.allowed, "alias.md"), ""},
		{"outside", filepath.Join(f.outside, "x.md"), "outside the directories"},
		{"file symlink leaving", filepath.Join(f.allowed, "leak.md"), "outside the directories"},
		{"new file under a directory symlink leaving", filepath.Join(f.allowed, "outdir", "new.md"), "outside the directories"},
		{"missing directory", filepath.Join(f.allowed, "nodir", "x.md"), "directory that does not exist"},
		{"dangling symlink", filepath.Join(f.allowed, "dangling.md"), "symbolic link"},
		{"directory", filepath.Join(f.allowed, "sub"), "not a regular file"},
		{"fifo", filepath.Join(f.allowed, "fifo"), "not a regular file"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			target, err := f.roots.outputTarget(tc.path)
			if tc.errPart != "" {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("target %q = %v, %v; want an error containing %q", tc.path, target, err, tc.errPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("target %q: %v", tc.path, err)
			}
			if n, err := target.write("written"); err != nil || n != len("written") {
				t.Fatalf("write %q = %d, %v", tc.path, n, err)
			}
		})
	}
	// The outside file the leaving symlink points at is untouched.
	if b, _ := os.ReadFile(filepath.Join(f.outside, "secret.md")); string(b) != "outside text" {
		t.Errorf("outside file changed: %q", b)
	}
	// A new file is created 0600; an overwritten one keeps its mode.
	if fi, err := os.Stat(filepath.Join(f.allowed, "new.md")); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("new file mode = %v, %v; want 0600", fi.Mode().Perm(), err)
	}
	if b, _ := os.ReadFile(filepath.Join(f.allowed, "sub", "fresh.md")); string(b) != "written" {
		t.Errorf("relative output_file did not land under the working directory: %q", b)
	}
	keep := filepath.Join(f.allowed, "shared.md")
	if err := os.WriteFile(keep, []byte("old old old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(keep, 0o644); err != nil {
		t.Fatal(err)
	}
	target, err := f.roots.outputTarget(keep)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := target.write("new"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Stat(keep); fi.Mode().Perm() != 0o644 {
		t.Errorf("overwritten file mode = %v, want 0644 kept", fi.Mode().Perm())
	}
	if b, _ := os.ReadFile(keep); string(b) != "new" {
		t.Errorf("overwritten file = %q, want %q (truncated)", b, "new")
	}
}
