package main

// `ctx-armsweep goldflip` — the metric flip test of design/05a §C3-2-D05-6.
//
// It is a separate subcommand from `compare` and not a flag on it, because it
// asks a different question. `compare` asks whether a variant beats a baseline
// and needs a measured noise floor to answer it. `goldflip` asks whether the
// GOLD SOURCE changes that answer, over one dump, one fusion and one case set —
// there is no second measurement in it, so a noise pair would be a requirement
// it cannot use.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

type goldflipOpts struct {
	dump    string
	goldA   string
	goldB   string
	base    string
	variant string
	slice   string
	out     string
	seed    int64
}

// cmdGoldFlip scores one comparison twice over the same records and writes the
// result in the shape `ctx-goldset judge -calibrate -flip` reads.
func cmdGoldFlip(c *common, o goldflipOpts) error {
	g, err := c.goldGuard()
	if err != nil {
		return err
	}
	dumps, err := c.dumpGuard(g)
	if err != nil {
		return err
	}
	if o.dump == "" || o.goldA == "" || o.goldB == "" {
		return fmt.Errorf("-dump, -gold-a und -gold-b sind Pflicht: der Kipp-Test braucht dieselben " +
			"Records unter beiden Gold-Quellen (§C3-2-D05-6)")
	}
	base, ok := armsweep.ConfigByName(o.base)
	if !ok {
		return fmt.Errorf("keine statische Konfiguration %q für -base", o.base)
	}
	variant, ok := armsweep.ConfigByName(o.variant)
	if !ok {
		return fmt.Errorf("keine statische Konfiguration %q für -variant", o.variant)
	}
	dumpPath, err := dumps.Resolve(o.dump)
	if err != nil {
		return err
	}
	recs, err := armsweep.ReadRecords(dumpPath)
	if err != nil {
		return err
	}
	goldA, err := readGoldVariant(g, o.goldA)
	if err != nil {
		return err
	}
	goldB, err := readGoldVariant(g, o.goldB)
	if err != nil {
		return err
	}
	// The intersection IS the core: gold A only covers the fully judged
	// queries, so GoldFlip's own skip rule selects them. Filtering here as well
	// would be a second, silent selection rule over the same set.
	flip := armsweep.GoldFlip(recs, base, variant, goldA, goldB, armsweep.PrimaryLevel, o.seed)
	if !flip.Available {
		return fmt.Errorf("kein Fall trägt beide Gold-Quellen — %d Records, %d/%d Gold-Fälle",
			len(recs), len(goldA), len(goldB))
	}
	slice := o.slice
	if slice == "" {
		slice = goldset.SliceReal
	}
	doc := map[string]goldset.MetricFlip{slice: flip}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	outPath, err := g.Resolve(o.out)
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(outPath, append(b, '\n')); werr != nil {
		return werr
	}
	fmt.Printf("Metrik-Kipp %s · Base=%s Variant=%s · n=%d\n", slice, base.Name, variant.Name, flip.N)
	fmt.Printf("ΔnDCG@10 Gold-A (%s): %+.5f · Gold-B (%s): %+.5f\n",
		filepath.Base(o.goldA), flip.DeltaFable, filepath.Base(o.goldB), flip.DeltaJudge)
	fmt.Printf("gepaartes %.0f-%%-CI der Differenz: [%+.5f, %+.5f] · Vorzeichenwechsel=%v · Kipp=%v\n",
		armsweep.PrimaryLevel*100, flip.DiffCILo, flip.DiffCIHi, flip.SignFlip(), flip.Flipped())
	fmt.Printf("geschrieben: %s (0600)\n", filepath.Base(outPath))
	return nil
}

// readGoldVariant loads one labelled slice file into the gold map GoldFlip
// reads. A case without gold ids is kept: an empty gold set is a declared
// state of this slice (§4.5), and dropping it here would select the cases the
// retrieval is worst at out of the comparison.
func readGoldVariant(g *goldset.Guard, name string) (map[string][]string, error) {
	p, err := g.Resolve(name)
	if err != nil {
		return nil, err
	}
	cases, err := goldset.ReadJSONL(p)
	if err != nil {
		return nil, err
	}
	if len(cases) == 0 {
		return nil, fmt.Errorf("%s: keine Fälle", name)
	}
	return armsweep.GoldOf(cases), nil
}
