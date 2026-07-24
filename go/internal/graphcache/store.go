package graphcache

import "sync/atomic"

// Store is the lock-free double buffer that publishes snapshots (§4.1 / §3.2).
// Readers call Current() and run on whatever pointer they loaded; the rebuild
// job (W05.2) builds a fresh Snapshot beside the live one and installs it with
// Swap — a single atomic store. This is the Go single-process equivalent of
// pgContext's two-phase metapage publication (§1): an immutable structure
// behind an atomic pointer needs no reader-writer lock and no generation
// machinery.
//
// The Cache-Zustandsautomat (§4.6: Empty/Fresh/Degraded/Failed) is W05.2 — this
// type is ONLY the raw buffer. Current() == nil is the "not ready" signal every
// consumer treats as "fall back to SQL".
type Store struct {
	ptr atomic.Pointer[Snapshot]
	seq atomic.Uint64
}

// Current returns the live snapshot, or nil if none has been published yet
// (boot before the first build — consumers use their SQL path).
func (st *Store) Current() *Snapshot {
	return st.ptr.Load()
}

// Swap stamps the snapshot with the next monotone Seq and publishes it,
// returning the previous snapshot (nil on the first publish). The Seq stamp
// mutates the not-yet-published snapshot before the store, so it is race-free:
// no reader can observe the pointer until the store completes.
func (st *Store) Swap(next *Snapshot) *Snapshot {
	if next != nil {
		next.Seq = st.seq.Add(1)
	}
	return st.ptr.Swap(next)
}
