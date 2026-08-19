//go:build !linux

package llmlog

// setUmask ist auf Nicht-Linux ein No-op; die Modus-Assertion läuft dort
// gegen die Default-umask und ist entsprechend schwächer (Review F9 — ohne
// den Stub kompiliert das Test-Paket auf darwin nicht).
func setUmask(m int) int { return 0 }
