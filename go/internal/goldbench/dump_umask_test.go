//go:build linux

package goldbench

import "syscall"

func setUmask(m int) int { return syscall.Umask(m) }
