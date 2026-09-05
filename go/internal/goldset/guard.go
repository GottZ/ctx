package goldset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/GottZ/ctx/internal/safepath"
)

// ErrOutsideGoldset is returned when a write target escapes the gold directory
// and --allow-outside-goldset was not set (design 04 §3.3, §4.8).
var ErrOutsideGoldset = errors.New("write target outside the gold-set directory")

// Guard confines every write of this tool to one directory. The gold slices
// carry real query texts and block ids of a private corpus, and the project
// doctrine forbids such data under /tmp; the guard is what makes that rule
// mechanical instead of a habit.
type Guard struct {
	root         string
	allowOutside bool
}

// NewGuard resolves root to an absolute path and creates it at mode 0700.
// A root whose basename is not DirName is accepted only with allowOutside —
// otherwise a typo would silently open a second, unprotected gold directory.
func NewGuard(root string, allowOutside bool) (*Guard, error) {
	return NewNamedGuard(root, DirName, allowOutside)
}

// NewNamedGuard is NewGuard with an explicit expected basename. The B-W5 sweep
// driver writes into two directories that are NOT the gold directory itself —
// its dump sink (`dumps/` beneath the gold root) and the plan's `reports/` —
// and both need the same confinement, the same 0700 creation and the same
// symlink-resistant Resolve. Duplicating the guard for them would mean two
// implementations of one rule, and the second one is always the weaker.
//
// wantBase is the basename the caller expects; an empty string skips the name
// check for a caller that has no naming convention to defend. allowOutside
// waives it either way and is recorded in the report.
func NewNamedGuard(root, wantBase string, allowOutside bool) (*Guard, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if wantBase != "" && filepath.Base(abs) != wantBase && !allowOutside {
		return nil, fmt.Errorf("%w: root %q is not a %q directory", ErrOutsideGoldset, abs, wantBase)
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Guard{root: abs, allowOutside: allowOutside}, nil
}

// Root is the absolute gold directory.
func (g *Guard) Root() string { return g.root }

// AllowOutside reports whether the override was set — the stamp and the report
// must declare it.
func (g *Guard) AllowOutside() bool { return g.allowOutside }

// Resolve maps a file name or path to an absolute path inside the gold
// directory. A relative name is joined to the root; anything that lands outside
// the root is refused unless the override is set. Symlinks are resolved on the
// existing prefix so a link out of the directory cannot smuggle a write.
func (g *Guard) Resolve(name string) (string, error) {
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(g.root, p)
	}
	p = filepath.Clean(p)
	if g.allowOutside {
		return p, nil
	}
	real, err := safepath.ResolvePrefix(p)
	if err != nil {
		return "", err
	}
	root, err := safepath.ResolvePrefix(g.root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q is not under %q", ErrOutsideGoldset, p, g.root)
	}
	return p, nil
}
