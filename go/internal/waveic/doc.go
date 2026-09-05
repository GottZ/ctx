// Package waveic hosts the cross-pipeline integration gates for Achse-02 Welle
// I-C (issue/comment block-type seeds, design/02 §7-I-C). It carries NO
// production code, and nothing imports it — the dependency runs the other way:
// ic_gates_integration_test.go imports blocktype, dream, guard, overview and
// testdb (:27-31) and drives those consumers end-to-end against a real PG18
// testcontainer to prove the seeded issue/comment policies take effect. The
// digest consumer is the exception: it is NOT imported, its filter is mirrored
// through blocktype.Set.DigestTypes instead. The gates live here — not in
// package blocktype — because importing the consumer packages (each of which
// imports blocktype) into a blocktype test would form an import cycle.
package waveic
