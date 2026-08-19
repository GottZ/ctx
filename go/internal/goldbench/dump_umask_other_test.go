//go:build !linux

package goldbench

// setUmask ist auf Nicht-Linux ein No-op; die Modus-Assertion läuft dort
// gegen die Default-umask und ist entsprechend schwächer.
func setUmask(m int) int { return 0 }
