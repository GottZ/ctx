// Package waveic hosts the cross-pipeline integration gates for Achse-02 Welle
// I-C (issue/comment block-type seeds, design/02 §7-I-C). It carries NO
// production code: the gates drive the guard, dream, digest and overview
// consumers end-to-end against a real PG18 testcontainer to prove the seeded
// issue/comment policies take effect. They live here — not in package blocktype
// — because importing those four consumer packages (each of which imports
// blocktype) into a blocktype test would form an import cycle.
package waveic
