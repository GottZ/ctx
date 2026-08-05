package overview

// SuperProbesSeen exposes the resolution-probe counter to the external
// integration test package (standard export_test idiom — test binary only,
// never shipped). It carries ONE gate: the proof that not a single Louvain probe
// runs between BEGIN and COMMIT (design/02 §7 W-F gate 7). A wall-clock
// measurement of the lock hold time could not tell a fast machine from a correct
// implementation; a counter can.
func SuperProbesSeen() int64 { return superProbes.Load() }
