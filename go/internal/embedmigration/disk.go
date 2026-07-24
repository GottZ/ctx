package embedmigration

import "golang.org/x/sys/unix"

// statfsAvailableBytes wraps statfs(2). Bavail (blocks available to an
// unprivileged caller) rather than Bfree — matches `df`'s "Avail" column,
// excluding the root-reserved slice a fail-closed disk gate should not
// count as usable headroom. ctx only ships a Linux container image (CI is
// ubuntu-latest throughout, docker-compose.yml), so this has no build-tag
// split for other kernels.
func statfsAvailableBytes(path string) (uint64, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return 0, err
	}
	return uint64(st.Bavail) * uint64(st.Bsize), nil
}
