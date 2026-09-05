// Package safepath holds the two path decisions every private-corpus write in
// this tree has to make: how far to follow symlinks on a path that does not
// exist yet, and which permission bits the artefact is created with.
//
// Both used to live as copies. The resolver existed twice — goldset's Guard
// refused a write whose prefix left the gold directory, and the distill dry-run
// sink walked for a .git ancestor — with the same loop and two different
// answers to the same question: what to do when EvalSymlinks fails for a reason
// other than "does not exist". The loop is the mechanism and lives here once;
// the two answers are the policy and keep their own names, so a caller picks
// one deliberately instead of inheriting whichever copy it was reading.
//
// # Boundary rule
//
// safepath is a stdlib-only leaf: path policy and mode constants, nothing that
// touches a corpus, a database or a config. It is deliberately NOT part of
// internal/util, which is the text-primitive leaf (runes, tokens, word tables);
// a path resolver is not a text primitive, and widening that leaf would blur a
// boundary another wave is drawing.
//
// It is also not the perimeter itself: Guard.Resolve, the dump's name check and
// the distill target validation stay where they are. This package answers what
// a path IS, never whether a caller may write to it.
package safepath

import (
	"os"
	"path/filepath"
)

// FileMode keeps a written artefact owner-readable only. Everything written
// under it — gold slices, arm dumps, judgement journals, distill dry-run prose
// — carries verbatim text and block ids of a private corpus, and the three
// packages that produce those files used to declare the same 0o600 each.
const FileMode os.FileMode = 0o600

// DirMode is the directory counterpart of FileMode.
//
// MkdirAll and OpenFile both take the process umask off the mode they are
// given, so a caller that has to guarantee the bits re-applies them with Chmod
// after creating the directory; a 022 umask would otherwise leave 0755 on a
// directory of raw session prose.
const DirMode os.FileMode = 0o700

// ResolvePrefix expands every symlink on p as far as p exists and re-appends
// the part that does not exist yet. filepath.EvalSymlinks fails outright on a
// path whose leaf is missing, which every fresh output file is; the missing
// tail cannot itself be a symlink precisely because it does not exist.
//
// This is the strict policy: a failure that is not "does not exist" — a
// non-directory component, a symlink loop, a permission wall — aborts with that
// error. Use it where the resolved path decides whether a write is allowed at
// all, so an unreadable prefix refuses the write instead of silently answering
// a path nobody checked.
//
// A path with no resolvable ancestor at all is answered unchanged, without an
// error.
func ResolvePrefix(p string) (string, error) {
	return resolvePrefix(p, true)
}

// ResolvePrefixLenient answers the same question under the opposite policy:
// every EvalSymlinks failure counts as "does not exist" and the walk keeps
// climbing, so the answer is the deepest ancestor that could be resolved with
// the remainder appended. A path with no resolvable ancestor is answered
// unchanged.
//
// Use it where the resolved path only improves a later decision and the caller
// has its own gate: the distill dry-run sink walks for a .git ancestor, and
// MkdirAll — not this function — is what decides whether the target is usable.
// A walk over the lexical form is still strictly better than no walk.
func ResolvePrefixLenient(p string) string {
	resolved, _ := resolvePrefix(p, false)
	return resolved
}

// resolvePrefix is the one loop both policies run. strict decides the single
// question they answer differently: whether an EvalSymlinks error that is not
// os.IsNotExist aborts the walk or is climbed past.
func resolvePrefix(p string, strict bool) (string, error) {
	tail := ""
	cur := p
	for {
		resolved, err := filepath.EvalSymlinks(cur)
		if err == nil {
			return filepath.Join(resolved, tail), nil
		}
		if strict && !os.IsNotExist(err) {
			return "", err
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p, nil
		}
		tail = filepath.Join(filepath.Base(cur), tail)
		cur = parent
	}
}
