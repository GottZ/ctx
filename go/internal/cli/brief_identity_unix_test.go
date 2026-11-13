//go:build unix

package cli

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// Unix-only layouts of the #146 resolver: FIFOs and symlinked paths. Windows
// has neither mkfifo nor unprivileged symlinks.

// resolveDeadline bounds one resolveProject call in these tests. A real call
// takes microseconds; the bound only turns a blocking read into a failure
// instead of a hung test binary.
const resolveDeadline = 10 * time.Second

// resolveWithin runs resolveProject(dir) and fails the test when it does not
// return within resolveDeadline.
func resolveWithin(t *testing.T, dir string) string {
	t.Helper()
	done := make(chan string, 1)
	go func() { done <- resolveProject(dir) }()
	select {
	case got := <-done:
		return got
	case <-time.After(resolveDeadline):
		t.Fatalf("resolveProject(%s) did not return within %v", dir, resolveDeadline)
		return ""
	}
}

// TestResolveProjectSkipsNonRegularGitMetadata pins that only regular files
// are read: a FIFO as .git or as commondir is skipped like any unfollowable
// entry (git itself refuses a non-regular .git), instead of blocking the hook.
func TestResolveProjectSkipsNonRegularGitMetadata(t *testing.T) {
	base := identityBase(t)
	host := filepath.Join(base, "host")
	identityRepo(t, host)

	fifoDir := filepath.Join(host, "fifo")
	identityMkdir(t, fifoDir)
	if err := syscall.Mkfifo(filepath.Join(fifoDir, ".git"), 0o600); err != nil {
		t.Fatalf("mkfifo .git: %v", err)
	}

	main := filepath.Join(base, "main")
	identityRepo(t, main)
	identityGit(t, main, "worktree", "add", "-q", "../host/wt", "-b", "wt")
	commondir := filepath.Join(main, ".git", "worktrees", "wt", "commondir")
	if err := os.Remove(commondir); err != nil {
		t.Fatalf("remove commondir: %v", err)
	}
	if err := syscall.Mkfifo(commondir, 0o600); err != nil {
		t.Fatalf("mkfifo commondir: %v", err)
	}

	for _, d := range []string{fifoDir, filepath.Join(host, "wt")} {
		if got := resolveWithin(t, d); got != host {
			t.Errorf("resolveProject(%s) = %s, want %s (entry skipped, walk continued)", d, got, host)
		}
	}
}

// TestResolveProjectSymlinkedPathKeepsWalkedSpelling: git records resolved
// paths in gitdir files. A nested linked worktree reached through a symlink
// keeps the walked spelling of its enclosing main checkout — the tag it had
// before #146 — while a sibling worktree, which has no walked ancestor to
// match, gets git's resolved path (documented).
func TestResolveProjectSymlinkedPathKeepsWalkedSpelling(t *testing.T) {
	base := identityBase(t)
	realDir := filepath.Join(base, "real")
	main := filepath.Join(realDir, "main")
	identityRepo(t, main)
	identityGit(t, main, "worktree", "add", "-q", ".wt/inner", "-b", "inner")
	identityGit(t, main, "worktree", "add", "-q", "../main-feat", "-b", "feat")
	link := filepath.Join(base, "link")
	if err := os.Symlink(realDir, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	nested := filepath.Join(link, "main", ".wt", "inner")
	if got, legacy := resolveProject(nested), legacyResolveProject(nested); got != filepath.Join(link, "main") || got != legacy {
		t.Errorf("resolveProject(%s) = %s, want %s (pre-#146 resolver: %s)", nested, got, filepath.Join(link, "main"), legacy)
	}
	expectIdentity(t, filepath.Join(link, "main"), filepath.Join(link, "main"))
	expectIdentity(t, main, filepath.Join(link, "main-feat"))
}

// TestResolveProjectRelativeGitdirThroughSymlink: with
// worktree.useRelativePaths (git 2.48+) the gitdir line is relative, and git
// resolves it against the physical directory of the .git file. A worktree
// reached through a symlink must still find its main checkout.
func TestResolveProjectRelativeGitdirThroughSymlink(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	identityGit(t, main, "-c", "worktree.useRelativePaths=true", "worktree", "add", "-q", "../main-rel", "-b", "rel")
	b, err := os.ReadFile(filepath.Join(base, "main-rel", ".git"))
	if err != nil {
		t.Fatalf("read .git file: %v", err)
	}
	if !strings.HasPrefix(string(b), "gitdir: ../") {
		t.Skipf("this git writes an absolute gitdir (worktree.useRelativePaths needs git 2.48+): %q", b)
	}
	other := filepath.Join(base, "other")
	identityMkdir(t, other)
	relLink := filepath.Join(other, "rel-link")
	if err := os.Symlink(filepath.Join(base, "main-rel"), relLink); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	expectIdentity(t, main, filepath.Join(base, "main-rel"), relLink)
}

// TestResolveProjectSeparateGitDirWithTrailingBlank: git writes and follows a
// gitdir path that ends in a blank ("store "). The main checkout and its
// linked worktree must share that directory as identity.
func TestResolveProjectSeparateGitDirWithTrailingBlank(t *testing.T) {
	base := identityBase(t)
	storeDir := filepath.Join(base, "st", "store ")
	identityMkdir(t, filepath.Join(base, "st"))
	identityGit(t, base, "init", "-q", "--separate-git-dir", storeDir, "sep")
	sep := filepath.Join(base, "sep")
	identityGit(t, sep, "commit", "-q", "--allow-empty", "-m", "root")
	identityGit(t, sep, "worktree", "add", "-q", "../sep-wt", "-b", "wt")
	expectIdentity(t, storeDir, sep, filepath.Join(base, "sep-wt"))
}

// TestResolveProjectRelativePathsResolvePhysically: git appends a relative
// gitdir, commondir or core.worktree to its base and lets the file system
// resolve it, so a symlink before ".." goes where the link points, not where
// a lexical clean would cut it. Each layout below is followed by git 2.56.0
// to the main checkout (gitdir, commondir) or the submodule checkout
// (core.worktree); lexically they all land elsewhere.
func TestResolveProjectRelativePathsResolvePhysically(t *testing.T) {
	base := identityBase(t)
	main := filepath.Join(base, "main")
	identityRepo(t, main)
	mainGit := filepath.Join(main, ".git")

	// gitdir: link/../worktrees/wt with link -> <main>/.git/objects.
	identityGit(t, main, "worktree", "add", "-q", "../wt", "-b", "wt")
	wt := filepath.Join(base, "wt")
	if err := os.Symlink(filepath.Join(mainGit, "objects"), filepath.Join(wt, "link")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: link/../worktrees/wt\n"), 0o644); err != nil {
		t.Fatalf("write .git: %v", err)
	}

	// commondir: lnk/../.. with lnk -> <main>/.git/refs/heads.
	identityGit(t, main, "worktree", "add", "-q", "../wt2", "-b", "wt2")
	admin := filepath.Join(mainGit, "worktrees", "wt2")
	if err := os.Symlink(filepath.Join(mainGit, "refs", "heads"), filepath.Join(admin, "lnk")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	if err := os.WriteFile(filepath.Join(admin, "commondir"), []byte("lnk/../..\n"), 0o644); err != nil {
		t.Fatalf("write commondir: %v", err)
	}
	expectIdentity(t, main, wt, filepath.Join(base, "wt2"))

	// core.worktree: lnk/../../../../../sub with lnk -> <modules/sub>/refs/heads.
	subSrc := filepath.Join(base, "subsrc")
	identityRepo(t, subSrc)
	super := filepath.Join(base, "super")
	identityRepo(t, super)
	identityGit(t, super, "submodule", "-q", "add", subSrc, "sub")
	modules := filepath.Join(super, ".git", "modules", "sub")
	if err := os.Symlink(filepath.Join(modules, "refs", "heads"), filepath.Join(modules, "lnk")); err != nil {
		t.Fatalf("symlink: %v", err)
	}
	identityGit(t, base, "config", "-f", filepath.Join(modules, "config"), "core.worktree", "lnk/../../../../../sub")
	sub := filepath.Join(super, "sub")
	expectIdentity(t, sub, sub)
}
