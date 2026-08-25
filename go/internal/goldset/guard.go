package goldset

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	abs = filepath.Clean(abs)
	if filepath.Base(abs) != DirName && !allowOutside {
		return nil, fmt.Errorf("%w: root %q is not a %q directory", ErrOutsideGoldset, abs, DirName)
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
	real, err := resolveExistingPrefix(p)
	if err != nil {
		return "", err
	}
	root, err := resolveExistingPrefix(g.root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, real)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("%w: %q is not under %q", ErrOutsideGoldset, p, g.root)
	}
	return p, nil
}

// resolveExistingPrefix expands symlinks over the longest existing prefix of p
// and re-appends the missing tail. filepath.EvalSymlinks fails outright on a
// path that does not exist yet, which every fresh output file is.
func resolveExistingPrefix(p string) (string, error) {
	tail := ""
	cur := p
	for {
		if resolved, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(resolved, tail), nil
		} else if !os.IsNotExist(err) {
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
