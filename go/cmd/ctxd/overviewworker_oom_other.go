//go:build !linux

package main

// preferSelfForOOMKill ist ausserhalb von Linux ein no-op: oom_score_adj ist
// eine Linux-Schnittstelle, und die Produktion läuft im Container. Die
// Signatur bleibt gleich, damit der Aufrufer keine Build-Tags kennt.
func preferSelfForOOMKill() error { return nil }
