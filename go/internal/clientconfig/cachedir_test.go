package clientconfig

import (
	"path/filepath"
	"testing"
)

// TestCacheDir pins the resolution order and the one deliberate difference to
// BaseDir: no relative fallback. A relative XDG_CACHE_HOME or LOCALAPPDATA is
// ignored (the XDG rule), and with no home directory the answer is "" — the
// caller keeps no cache instead of writing into the working directory.
func TestCacheDir(t *testing.T) {
	abs := t.TempDir()
	local := t.TempDir()
	home := t.TempDir()

	cases := []struct {
		name, xdg, local, home, want string
	}{
		{"XDG_CACHE_HOME wins", abs, local, home, filepath.Join(abs, "ctx")},
		{"relative XDG_CACHE_HOME is ignored", "rel/cache", local, home, filepath.Join(local, "ctx")},
		{"LOCALAPPDATA before home", "", local, home, filepath.Join(local, "ctx")},
		{"relative LOCALAPPDATA is ignored", "", "rel", home, filepath.Join(home, ".cache", "ctx")},
		{"home fallback", "", "", home, filepath.Join(home, ".cache", "ctx")},
		{"no home: no cache", "", "", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("XDG_CACHE_HOME", tc.xdg)
			t.Setenv("LOCALAPPDATA", tc.local)
			t.Setenv("HOME", tc.home)
			t.Setenv("USERPROFILE", tc.home) // os.UserHomeDir on Windows
			if got := CacheDir(); got != tc.want {
				t.Errorf("CacheDir() = %q, want %q", got, tc.want)
			}
		})
	}
}
