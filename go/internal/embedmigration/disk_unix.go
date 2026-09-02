//go:build unix

package embedmigration

import "golang.org/x/sys/unix"

// statfsAvailableBytes wraps statfs(2). Bavail (blocks available to an
// unprivileged caller) rather than Bfree — matches `df`'s "Avail" column,
// excluding the root-reserved slice a fail-closed disk gate should not
// count as usable headroom. The container image is Linux-only, but the
// tree must still cross-compile for the release matrix and for developers
// on Windows (issue #40, bug 4) — hence the build-tag split with
// disk_windows.go.
func statfsAvailableBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
