// Package provenance — the two provenance revisions the ctx tooling stamps into
// its artefacts, kept apart by name: the revision of the BUILD and the revision
// of the WORKTREE that was read.
//
// Both are resolved from files, deliberately NOT from a `git rev-parse`
// subprocess: spawning one is an argued exception in this module
// (internal/llm/exec_ban_test.go) and a provenance field does not argue it.
// BuildRev reads the VCS stamp Go embeds at build time; WorktreeRev walks up
// from a directory and resolves .git/HEAD by hand.
//
// The two values are not interchangeable, which is why they carry two names:
// Go resolves the repository by walking up from the package directory, so in a
// linked git worktree the BUILD stamp can name the enclosing checkout, while
// WorktreeRev names the HEAD of the worktree it was pointed at. BuildRev is the
// full hash and carries "-dirty"; WorktreeRev is seven characters and carries
// no dirty flag.
//
// Stdlib-only on purpose: a provenance field must not drag a dependency into
// the three tooling binaries that only want to know which revision they came
// from.
package provenance

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// BuildRev reads the VCS stamp Go embedded at build time, with the dirty flag
// appended ("" when the binary carries no stamp, as in a test binary).
//
// Caveat the stamp must not hide: Go resolves the repository by walking up from
// the package directory, and in a linked git worktree that walk can land on the
// enclosing checkout instead of the worktree. The value therefore identifies
// the BUILD, not necessarily the commit the artefact was drawn under — which is
// why the field is named for the build and carries "-dirty" verbatim.
func BuildRev() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	rev, dirty := "", false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev != "" && dirty {
		return rev + "-dirty"
	}
	return rev
}

// WorktreeRev liefert die kurze HEAD-Revision des git-Arbeitsverzeichnisses,
// das dir enthält ("" wenn nicht ermittelbar) — der Aufstieg beginnt bei dir
// und endet am Dateisystem-Wurzelverzeichnis. .git/HEAD wird gelesen,
// symbolische Refs über die Ref-Datei bzw. packed-refs aufgelöst. Worktrees
// (".git"-DATEI mit gitdir:-Zeile) werden verfolgt.
//
// Relative Pfade werden zuerst absolut gemacht, sonst endete der Aufstieg für
// "." schon nach dem ersten Schritt. Ein LEERES dir liefert dagegen "" und wird
// bewusst nicht zum Arbeitsverzeichnis ergänzt: der Aufrufer, der sein cwd
// nicht ermitteln konnte, bekommt keinen erfundenen Stempel.
func WorktreeRev(dir string) string {
	if dir == "" {
		return ""
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	for {
		if rev := revFromGitDir(filepath.Join(dir, ".git")); rev != "" {
			return rev
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}

// revFromGitDir löst HEAD eines .git-Pfads (Verzeichnis oder Worktree-Datei) auf.
func revFromGitDir(gitPath string) string {
	st, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	if !st.IsDir() {
		// Worktree/Submodule: Datei mit "gitdir: <pfad>".
		b, err := os.ReadFile(gitPath)
		if err != nil {
			return ""
		}
		line := strings.TrimSpace(string(b))
		if !strings.HasPrefix(line, "gitdir:") {
			return ""
		}
		target := strings.TrimSpace(strings.TrimPrefix(line, "gitdir:"))
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(gitPath), target)
		}
		gitPath = target
	}
	head, err := os.ReadFile(filepath.Join(gitPath, "HEAD")) //nolint:gosec // G703: repo-lokale git-Metadaten für den Env-Stamp, Pfad aus dem Verzeichnis-Aufstieg
	if err != nil {
		return ""
	}
	ref := strings.TrimSpace(string(head))
	if !strings.HasPrefix(ref, "ref:") {
		return shortRev(ref)
	}
	refName := strings.TrimSpace(strings.TrimPrefix(ref, "ref:"))
	// Direkte Ref-Datei (auch commondir-Fälle für Worktrees prüfen).
	for _, base := range []string{gitPath, commonGitDir(gitPath)} {
		if base == "" {
			continue
		}
		if b, err := os.ReadFile(filepath.Join(base, filepath.FromSlash(refName))); err == nil { //nolint:gosec // G703: repo-lokale Ref-Datei, refName aus HEAD des eigenen Repos
			return shortRev(strings.TrimSpace(string(b)))
		}
		if rev := revFromPackedRefs(filepath.Join(base, "packed-refs"), refName); rev != "" {
			return rev
		}
	}
	return ""
}

// commonGitDir liest die commondir-Datei eines Worktree-gitdirs ("" wenn keine).
func commonGitDir(gitPath string) string {
	b, err := os.ReadFile(filepath.Join(gitPath, "commondir")) //nolint:gosec // G703: repo-lokale git-Metadaten, Pfad aus dem Verzeichnis-Aufstieg
	if err != nil {
		return ""
	}
	common := strings.TrimSpace(string(b))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitPath, common)
	}
	return common
}

// revFromPackedRefs sucht refName in einer packed-refs-Datei.
func revFromPackedRefs(path, refName string) string {
	b, err := os.ReadFile(path) //nolint:gosec // G703: repo-lokale packed-refs, Pfad aus dem Verzeichnis-Aufstieg
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName {
			return shortRev(fields[0])
		}
	}
	return ""
}

// shortRev kürzt eine Commit-Hash auf die üblichen 7 Zeichen.
func shortRev(rev string) string {
	if len(rev) < 7 || strings.ContainsAny(rev, " \t") {
		return ""
	}
	return rev[:7]
}
