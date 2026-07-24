package graphcache

import (
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"hash"
)

// Fingerprint is a stable digest over the DETERMINISTIC content of a snapshot —
// everything except BuiltAt and Seq (which are wall-clock / publication
// metadata, not data). Two Builds over an identical DB must produce equal
// Fingerprints (the Determinismus-Invariante, §7 W05.1); the integration gate
// compares this instead of reflect.DeepEqual to stay cheap at the 1M+ target.
func (s *Snapshot) Fingerprint() [32]byte {
	h := sha256.New()
	var scratch [8]byte
	putUint := func(v uint64) {
		binary.LittleEndian.PutUint64(scratch[:], v)
		h.Write(scratch[:])
	}

	putUint(uint64(len(s.UUIDs)))
	for i := range s.UUIDs {
		h.Write(s.UUIDs[i][:])
	}
	for _, v := range s.ScopeID {
		putUint(uint64(v))
	}
	for _, v := range s.TypeID {
		putUint(uint64(v))
	}
	for _, w := range s.Archived.words {
		putUint(w)
	}
	hashStrings(h, s.scopeNames)
	hashStrings(h, s.typeNames)
	hashStrings(h, s.classNames)
	hashStrings(h, s.originNames)
	hashCSRPair(h, putUint, s.Dream)
	hashCSRPair(h, putUint, s.Struct)

	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

func hashStrings(h hash.Hash, xs []string) {
	var scratch [8]byte
	binary.LittleEndian.PutUint64(scratch[:], uint64(len(xs)))
	h.Write(scratch[:])
	for _, x := range xs {
		binary.LittleEndian.PutUint64(scratch[:], uint64(len(x)))
		h.Write(scratch[:])
		h.Write([]byte(x))
	}
}

func hashCSRPair(h hash.Hash, putUint func(uint64), p CSRPair) {
	for _, c := range []CSR{p.Fwd, p.Rev} {
		for _, v := range c.Offsets {
			putUint(uint64(v))
		}
		for _, v := range c.Targets {
			putUint(uint64(v))
		}
		for _, v := range c.Rel {
			putUint(uint64(v))
		}
		for _, v := range c.Conf {
			putUint(uint64(v))
		}
		for _, v := range c.RawConf {
			putUint(uint64(v))
		}
		for _, v := range c.ClassID {
			putUint(uint64(v))
		}
		for _, v := range c.OriginID {
			putUint(uint64(v))
		}
		for _, v := range c.Created {
			putUint(uint64(v))
		}
	}
}

// checkOrdering verifies the per-node adjacency ordering invariants (§3.2 Nr. 1)
// across every CSR: dream by RawConf DESC (dst ASC tiebreak), struct by Created
// DESC (dst ASC, class ASC tiebreak). It returns a descriptive error on the
// FIRST violation. Exposed to tests via export_test — the same checker runs
// against the sorted builder (expects nil) AND the deliberately unsorted seam
// (expects an error, proving the check is non-vacuous: the Fixture-Gate).
func checkOrdering(s *Snapshot) error {
	for name, c := range map[string]CSR{"dream.fwd": s.Dream.Fwd, "dream.rev": s.Dream.Rev} {
		if err := checkRangeOrder(c, dreamLess, name); err != nil {
			return err
		}
	}
	for name, c := range map[string]CSR{"struct.fwd": s.Struct.Fwd, "struct.rev": s.Struct.Rev} {
		if err := checkRangeOrder(c, structLess, name); err != nil {
			return err
		}
	}
	return nil
}

// checkRangeOrder asserts that within every node's adjacency range, each element
// is <= its predecessor under less (i.e. less(prev, cur) never violated). less
// is a strict total order, so a valid list has less(i, i-1) == false for all
// adjacent pairs.
func checkRangeOrder(c CSR, less func(CSR, uint32, uint32) bool, name string) error {
	for n := 0; n+1 < len(c.Offsets); n++ {
		lo, hi := c.Offsets[n], c.Offsets[n+1]
		for i := lo + 1; i < hi; i++ {
			if less(c, i, i-1) {
				return fmt.Errorf("graphcache: %s adjacency of node %d out of order at edge %d", name, n, i)
			}
		}
	}
	return nil
}
