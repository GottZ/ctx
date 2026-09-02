//go:build windows

package embedmigration

import "golang.org/x/sys/windows"

// statfsAvailableBytes is the Windows counterpart of the statfs(2) wrapper in
// disk_unix.go. GetDiskFreeSpaceEx's first out-parameter is the number of
// bytes available to the calling user (quota-aware), which is the same
// "usable headroom" notion Bavail*Bsize expresses on unix.
func statfsAvailableBytes(path string) (uint64, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	var availableToCaller, total, free uint64
	if err := windows.GetDiskFreeSpaceEx(p, &availableToCaller, &total, &free); err != nil {
		return 0, err
	}
	return availableToCaller, nil
}
