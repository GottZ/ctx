//go:build linux

package main

import (
	"fmt"
	"os"
)

// preferSelfForOOMKill macht den Worker zum bevorzugten Opfer des
// OOM-Killers (Achse 04 / S7b, SP-8).
//
// WARUM DAS NÖTIG IST: das Speicherlimit gilt der CGROUP, nicht dem Prozess —
// Kind und Daemon teilen es sich. Reisst es, wählt der Kernel das Opfer nach
// oom_score, und ohne Zutun ist das häufig der Prozess mit dem grössten
// Speicheranteil. Das kann der Daemon sein, der die graphcache-CSR über den
// ganzen Korpus hält (UD-09-04) — dann stirbt der Server, weil ein
// Hintergrund-Rebuild zu gross war. Genau diese Verwechslung schliesst
// oom_score_adj: der Rebuild ist die wegwerfbare Arbeit, der Daemon nicht.
//
// Der Datei-Modus ist bedeutungslos — /proc/self/oom_score_adj existiert
// bereits, WriteFile legt nichts an. 0600 statt 0644, weil der Linter zu
// Recht keinen Unterschied zwischen einer echten Datei und einer procfs-
// Schnittstelle sehen kann und die engere Zahl hier nichts kostet.
//
// 1000 ist das Maximum ("kill mich zuerst"). Der Wert darf nach OBEN immer
// gesetzt werden, ohne Privilegien — nur das Senken ist gated. Ein Fehler ist
// deshalb kein Rechte-Problem, sondern eine exotische Sandbox ohne
// beschreibbares /proc; er WARNt und bricht nichts ab, dieselbe
// Best-effort-Zusage wie bei deprioritizeSelf.
func preferSelfForOOMKill() error {
	const maxAdj = "1000"
	if err := os.WriteFile("/proc/self/oom_score_adj", []byte(maxAdj), 0o600); err != nil {
		return fmt.Errorf("writing oom_score_adj: %w", err)
	}
	return nil
}
