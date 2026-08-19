//go:build linux

package llmlog

import "syscall"

// setUmask setzt die Prozess-umask und liefert die alte — der 0600-Beweis
// muss gegen eine permissive umask laufen, sonst prüft er nichts.
func setUmask(m int) int { return syscall.Umask(m) }
