package main

// Shared fixture of wave C4-3b (`ctx-goldset pool` becomes slice-aware).
//
// The fixture is SYNTHETIC on purpose: ids c01.., b01.., invented queries and
// invented block text. The gold directory of the project carries real query
// texts of a private corpus (design 04 §3.3) and is never read by a test.
//
// Everything here compiles against the pre-C4-3b tool as well. That is what
// makes the byte-compatibility golden in pool_bytecompat_c43b_test.go a real
// before/after comparison: the golden files were produced by running that test
// against the UNCHANGED command, and the same test then has to reproduce them
// byte for byte after the change.

import (
	"path/filepath"
	"testing"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// c43bRunID is the pooling run the fixture pretends to come from. Fixed, so
// every artefact name in the goldens is fixed too.
const c43bRunID = "20260829T000000Z"

// c43bSeed and c43bCreatedAt pin the two inputs that would otherwise make the
// artefacts vary between runs: the sampling seed and the key's clock.
const (
	c43bSeed      = int64(20260829)
	c43bCreatedAt = "2026-08-29T00:00:00Z"
)

// c43bCase builds one gold case of the named slice. The query text carries an
// umlaut because the excerpt truncation is byte-based and rune-aware, and a
// golden that never saw a multi-byte rune would not pin that.
func c43bCase(slice string, index int) goldset.Case {
	q := "Wofür steht " + slice + "-Frage " + string(rune('a'+index)) + "?"
	c := goldset.Case{Slice: slice, Index: index, Query: q, QuerySHA256: goldset.SHA256Hex(q)}
	if slice == goldset.SliceGlob {
		// G-GLOB carries no constructive gold, only the pool reference it was
		// generated with (E-9) — the very property that makes it poolable.
		c.Origin, c.PoolRef = "tag-aggregate", "tag:c43b"
	} else {
		c.Origin = "access-log"
	}
	return c
}

// c43bID is a candidate block id.
func c43bID(n int) string { return "b" + string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// c43bControlID is a control-corpus block id, disjoint from the candidates.
func c43bControlID(n int) string { return "c" + string(rune('0'+n/10)) + string(rune('0'+n%10)) }

// c43bEntry is one pooling entry: four arm heads that overlap, so the union is
// smaller than their sum and the deduplication is exercised.
func c43bEntry(c goldset.Case, base int) goldset.PoolEntry {
	id := func(n int) string { return c43bID(base + n) }
	return goldset.PoolEntry{
		Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
		Semantic: []string{id(1), id(2), id(3), id(4)},
		FTSDe:    []string{id(2), id(5)},
		FTSEn:    []string{id(6), id(1)},
		Trigram:  []string{id(7), id(3)},
	}
}

// c43bBlock renders one block. The title carries a pipe and the content a
// newline, so the markdown form has to escape the first and fold the second —
// two rules a golden pins better than a comment does.
func c43bBlock(id string) goldset.Block {
	return goldset.Block{
		ID:       id,
		Title:    "Block " + id + " | Größenordnung",
		Content:  "Inhalt von " + id + ".\nZweite Zeile mit Überlänge und Straße.",
		TypeName: "knowledge",
		Language: "de",
	}
}

// c43bControlPool is the retrievable corpus the control sample is drawn from,
// in the deterministic order the database would have produced for the seed.
func c43bControlPool() []goldset.Block {
	out := make([]goldset.Block, 0, 40)
	for i := 1; i <= 40; i++ {
		out = append(out, c43bBlock(c43bControlID(i)))
	}
	return out
}

// c43bBlocksOf answers what the database lookup would answer for these ids.
func c43bBlocksOf(ids []string) map[string]goldset.Block {
	out := make(map[string]goldset.Block, len(ids))
	for _, id := range ids {
		out[id] = c43bBlock(id)
	}
	return out
}

// c43bGold writes a complete gold directory: both judged slices and one pooling
// file that holds the entries of BOTH of them, keyed by slice/index/digest —
// exactly what a priming run writes since wave C4-3a.
func c43bGold(t *testing.T) *goldset.Guard {
	t.Helper()
	root := filepath.Join(t.TempDir(), goldset.DirName)
	g, err := goldset.NewGuard(root, false)
	if err != nil {
		t.Fatalf("NewGuard: %v", err)
	}

	realCases := []goldset.Case{
		c43bCase(goldset.SliceReal, 0), c43bCase(goldset.SliceReal, 1), c43bCase(goldset.SliceReal, 2),
	}
	globCases := []goldset.Case{
		c43bCase(goldset.SliceGlob, 0), c43bCase(goldset.SliceGlob, 1),
	}
	konstrCases := []goldset.Case{c43bCase(goldset.SliceGlobKonstr, 0)}

	c43bWriteSlice(t, g, goldset.FileReal, realCases)
	c43bWriteSlice(t, g, goldset.FileGlob, globCases)
	c43bWriteSlice(t, g, goldset.FileGlobKonstr, konstrCases)

	var entries []goldset.PoolEntry
	for i, c := range realCases {
		entries = append(entries, c43bEntry(c, i*10))
	}
	for i, c := range globCases {
		entries = append(entries, c43bEntry(c, 40+i*10))
	}
	c43bWritePool(t, g, "pool-"+c43bRunID+".jsonl", entries)
	return g
}

// c43bWriteSlice persists one slice file through the production writer.
func c43bWriteSlice(t *testing.T, g *goldset.Guard, name string, cases []goldset.Case) {
	t.Helper()
	p, err := g.Resolve(name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if err := goldset.WriteJSONL(p, cases); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}

// c43bWritePool persists the pooling file through the sweep driver's own
// writer — armsweep.PoolEntry is an alias of the goldset type, so the fixture
// carries the production wire form rather than a second rendering of it.
func c43bWritePool(t *testing.T, g *goldset.Guard, name string, entries []goldset.PoolEntry) {
	t.Helper()
	p, err := g.Resolve(name)
	if err != nil {
		t.Fatalf("resolve %s: %v", name, err)
	}
	if err := armsweep.WritePool(p, entries); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
}
