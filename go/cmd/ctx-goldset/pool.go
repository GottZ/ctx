package main

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

// controlPoolLimit is the draw size for the uniform control sample. It is far
// above the retrievable corpus on purpose: RetrievableBlocks orders by a seeded
// hash and truncates, so a limit BELOW the corpus size would silently draw the
// controls from a seeded subset instead of from the whole retrievable set the
// design declares.
const controlPoolLimit = 1_000_000

// keyPrefix is the file-name prefix of the control key. It is a separate file
// from the template so a judge can read the template without reading which of
// its rows are control draws.
const keyPrefix = "pool-key-"

type poolOpts struct {
	poolFile string
	out      string
	// slice is the -slice flag: empty means G-REAL, the slice this command was
	// hard-wired to before wave C4-3b. poolInputs replaces it with the RESOLVED
	// slice id, so everything downstream names the slice the same way.
	slice   string
	control int
	excerpt int
	dryRun  bool
}

// cmdPool builds the blind judgement template of ONE judged slice (design 04
// §4.5; design/05a §C3-2-D05-8 k for G-GLOB): the union of the four solo-arm
// heads plus a uniform control sample, deduplicated, seeded-permuted and
// stripped of every trace of where a candidate came from.
//
// The judging itself is a human act and is not in this tool. What is in this
// tool is the guarantee that the human cannot see the answer key.
func cmdPool(c *common, o poolOpts) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	cases, entries, runID, err := poolInputs(g, &o)
	if err != nil {
		return err
	}

	db, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(ctx) }()
	controlPool, err := db.RetrievableBlocks(ctx, c.seed, controlPoolLimit, 0)
	if err != nil {
		return err
	}
	pooled, key, err := goldset.BuildPool(cases, entries, controlPool, o.control, c.seed)
	if err != nil {
		return err
	}
	key.PoolRunID, key.CreatedAt = runID, time.Now().UTC().Format(time.RFC3339)

	ids := allCandidateIDs(pooled)
	blocks, err := db.BlocksByIDs(ctx, ids)
	if err != nil {
		return err
	}
	return emitTemplate(g, o, pooled, blocks, key, runID, len(ids)-len(blocks))
}

// poolInputs resolves the slice named on the command line and loads everything
// the template is built from: the cases of that slice, the pooling dump and the
// run id the artefacts are named after.
//
// It writes the RESOLVED slice back into o. The flag is spelling-tolerant, the
// artefact naming is not: everything downstream has to see "G-GLOB" rather than
// whichever of its spellings reached the command line.
func poolInputs(g *goldset.Guard, o *poolOpts) (
	cases []goldset.Case, entries []goldset.PoolEntry, runID string, err error,
) {
	slice, file, err := resolvePoolSlice(o.slice)
	if err != nil {
		return nil, nil, "", err
	}
	o.slice = slice
	if cases, err = readSlice(g, file, slice); err != nil {
		return nil, nil, "", err
	}
	poolPath, runID, err := resolvePool(g, o.poolFile)
	if err != nil {
		return nil, nil, "", err
	}
	if entries, err = goldset.ReadPool(poolPath); err != nil {
		return nil, nil, "", err
	}
	// A pool file from a priming run before wave C4-3a holds G-REAL entries and
	// nothing else (Befund N1). Without this check the run dies on the FIRST
	// case — "no pool entry for case G-GLOB/0/<digest>" — which points at the
	// case instead of at the file that predates the slice.
	if n := countPoolEntries(entries, slice); n == 0 {
		return nil, nil, "", fmt.Errorf("%s trägt keinen %s-Eintrag (%d Einträge insgesamt) — "+
			"die Datei stammt aus einem Priming-Lauf vor Welle C4-3a; `ctx-armsweep prime` erneut fahren",
			filepath.Base(poolPath), slice, len(entries))
	}
	return cases, entries, runID, nil
}

// resolvePoolSlice maps the -slice value to a judged slice and its gold file.
//
// The empty value is G-REAL out of g-real.jsonl — what this command had
// hard-wired at pool.go:48 until wave C4-3b, so an invocation without the flag
// writes the same bytes it always did.
//
// Spelling is tolerated in one direction only: case is ignored and the "G-"
// prefix may be left off, so `-slice glob`, `-slice g-glob` and `-slice G-GLOB`
// are the same request. Anything else is REFUSED rather than resolved to a
// default — a slice whose gold is set by construction has no pool, and the
// alternative to an error message is a template full of candidates nobody
// pooled, or an ingest that overwrites constructive gold with judgements.
func resolvePoolSlice(name string) (slice, file string, err error) {
	if strings.TrimSpace(name) == "" {
		return goldset.SliceReal, goldset.FileReal, nil
	}
	slice = strings.ToUpper(strings.TrimSpace(name))
	if !strings.HasPrefix(slice, "G-") {
		slice = "G-" + slice
	}
	if f, ok := goldset.PoolSliceFile(slice); ok {
		return slice, f, nil
	}
	pooled := strings.Join(goldset.PooledSlices(), ", ")
	profile, known := goldset.ProfileFor(slice)
	if !known {
		return "", "", fmt.Errorf("-slice %s ist kein Slice des Gold-Sets — "+
			"eine Urteils-Vorlage gibt es für: %s", slice, pooled)
	}
	return "", "", fmt.Errorf("-slice %s wird nicht gepoolt: sein Gold ist konstruktiv gesetzt (%s), "+
		"es gibt keinen Pool und damit keine Vorlage — geurteilt werden: %s",
		slice, profile.GoldSource, pooled)
}

// countPoolEntries counts the entries of one slice in a pooling dump.
func countPoolEntries(entries []goldset.PoolEntry, slice string) int {
	n := 0
	for _, e := range entries {
		if e.Slice == slice {
			n++
		}
	}
	return n
}

// artefactID is the id the template, its markdown twin and its control key are
// named after: the pooling run for G-REAL, the pooling run PREFIXED BY THE
// SLICE for every other one.
//
// Both judged slices are pooled in the same priming run and therefore share a
// run id. Naming both templates judge-<run>.jsonl would mean the second build
// silently overwrites the first template AND its control key — both writes
// succeed, and the loss only surfaces as judgements that no longer match their
// key. G-REAL keeps the bare run id so the artefact names of the standing
// C3-4a/C3-4b strecke do not move.
//
// The name still round-trips: runIDOf strips the "judge-" prefix, so
// `judge -llm -template judge-glob-<run>.jsonl` derives pool-key-glob-<run>.json
// and writes judged-glob-<run>.jsonl beside it.
func artefactID(slice, runID string) string {
	if slice == "" || slice == goldset.SliceReal {
		return runID
	}
	return strings.ToLower(strings.TrimPrefix(slice, "G-")) + "-" + runID
}

// emitTemplate writes the two template forms plus the control key, or reports
// the counts alone when -dry-run is set.
func emitTemplate(g *goldset.Guard, o poolOpts, pooled []goldset.PooledCase,
	blocks map[string]goldset.Block, key goldset.PoolKey, runID string, missing int,
) error {
	p50, maxN, total := poolSizes(pooled)
	fmt.Printf("Pool-Vorlage: Fälle=%d Kandidaten=%d p50=%d max=%d Kontrolle=%d Seed=%d Lauf=%s fehlende_Blöcke=%d\n",
		len(pooled), total, p50, maxN, key.Controls, key.Seed, runID, missing)
	if o.dryRun {
		fmt.Println("Trockenlauf — nichts geschrieben.")
		return nil
	}
	name := artefactID(o.slice, runID)
	base := o.out
	if base == "" {
		base = "judge-" + name
	}
	jsonlPath, err := g.Resolve(base + ".jsonl")
	if err != nil {
		return err
	}
	mdPath, err := g.Resolve(base + ".md")
	if err != nil {
		return err
	}
	keyPath, err := g.Resolve(keyPrefix + name + ".json")
	if err != nil {
		return err
	}
	if err := goldset.WriteTemplate(jsonlPath, mdPath, pooled, blocks, o.excerpt); err != nil {
		return err
	}
	if err := goldset.WritePoolKey(keyPath, key); err != nil {
		return err
	}
	fmt.Printf("geschrieben: %s %s %s\n", filepath.Base(jsonlPath), filepath.Base(mdPath), filepath.Base(keyPath))
	return nil
}

// cmdIngest reads the filled-in judgements back into the slice and merges the
// G-REAL profile into the stamp.
func cmdIngest(c *common, judgedName, keyName, outName, stampName string) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	if judgedName == "" {
		return fmt.Errorf("-judged fehlt: die ausgefüllte Urteils-Vorlage")
	}
	judgedPath, err := g.Resolve(judgedName)
	if err != nil {
		return err
	}
	keyPath, err := resolveKey(g, keyName, judgedName)
	if err != nil {
		return err
	}
	judged, err := goldset.ParseJudgements(judgedPath)
	if err != nil {
		return err
	}
	key, err := goldset.ReadPoolKey(keyPath)
	if err != nil {
		return fmt.Errorf("control key %s: %w", filepath.Base(keyPath), err)
	}
	rate, hits, controls, err := goldset.ControlHitRate(judged, key)
	if err != nil {
		return err
	}

	outPath, err := g.Resolve(outName)
	if err != nil {
		return err
	}
	cases, err := goldset.ReadJSONL(outPath)
	if err != nil {
		return err
	}
	slice, err := ingestSlice(cases, outName)
	if err != nil {
		return err
	}
	labelled, st, err := goldset.ApplyLabels(cases, judged)
	if err != nil {
		return err
	}
	backup, err := goldset.BackupFile(outPath, time.Now().UTC().Format("20060102"))
	if err != nil {
		return err
	}
	if err := goldset.WriteJSONL(outPath, labelled); err != nil {
		return err
	}
	if err := stampIngest(g, slice, stampName, outName, judgedPath, outPath, key, st, rate, hits, controls); err != nil {
		return err
	}
	fmt.Printf("%s gelabelt: n=%d gelabelt=%d ohne_Relevante=%d Urteile=%d relevant=%d p50=%d max=%d\n",
		slice, st.Cases, st.Labelled, st.NoRelevant, st.Judged, st.Relevant, st.PoolP50, st.PoolMax)
	fmt.Printf("Kontroll-Trefferquote: %.4f (%d von %d) — Sicherung: %s\n",
		rate, hits, controls, filepath.Base(backup))
	return nil
}

// ingestSlice names the slice being labelled — read off the cases themselves
// rather than taken from a flag, so the profile can never land under a slice
// the file does not hold.
//
// Two refusals, both fail-closed. A file holding more than one slice is not
// ingestable: ApplyLabels demands a judgement for EVERY case, and one template
// covers one slice, so a mixed file would abort halfway with a case-level
// message. And a slice whose gold is set by CONSTRUCTION is refused outright:
// `-out g-glob-konstr.jsonl` would replace gold taken from graph_cluster_member
// with pooled judgements and turn a declared floor check into a judged slice,
// while the stamp went on describing it as constructive.
func ingestSlice(cases []goldset.Case, outName string) (string, error) {
	names := make([]string, 0, 1)
	seen := map[string]bool{}
	for _, c := range cases {
		if c.Slice != "" && !seen[c.Slice] {
			seen[c.Slice] = true
			names = append(names, c.Slice)
		}
	}
	switch {
	case len(names) == 0:
		return "", fmt.Errorf("%s nennt keinen Slice — die Datei ist keine Gold-Slice-Datei", outName)
	case len(names) > 1:
		return "", fmt.Errorf("%s trägt mehrere Slices (%s) — eine Urteils-Vorlage deckt genau einen ab",
			outName, strings.Join(names, ", "))
	}
	if _, ok := goldset.PoolSliceFile(names[0]); !ok {
		return "", fmt.Errorf("%s hält %s, dessen Gold konstruktiv gesetzt ist — "+
			"gepoolte Urteile würden es überschreiben; eingelesen werden: %s",
			outName, names[0], strings.Join(goldset.PooledSlices(), ", "))
	}
	return names[0], nil
}

// stampIngest merges the profile of the labelled slice into STAMP.json. The
// merge runs on the raw document, so a field written by another wave survives
// this rewrite.
func stampIngest(g *goldset.Guard, slice, stampName, outName, judgedPath, outPath string,
	key goldset.PoolKey, st goldset.LabelStats, rate float64, hits, controls int,
) error {
	stampPath, err := g.Resolve(stampName)
	if err != nil {
		return err
	}
	sliceDigest, err := goldset.FileDigest(outPath)
	if err != nil {
		return err
	}
	judgedDigest, err := goldset.FileDigest(judgedPath)
	if err != nil {
		return err
	}
	return goldset.MergeStampSlice(stampPath, slice, map[string]any{
		"n":                   st.Cases,
		"file":                outName,
		"sha256":              sliceDigest,
		"labelled":            st.Labelled,
		"no_relevant":         st.NoRelevant,
		"judgements":          st.Judged,
		"relevant_judgements": st.Relevant,
		"pool_p50":            st.PoolP50,
		"pool_max":            st.PoolMax,
		"control_hit_rate":    round4(rate),
		"control_hits":        hits,
		"control_n":           controls,
		"pool_run_id":         key.PoolRunID,
		"pool_seed":           key.Seed,
		"judgement_file":      filepath.Base(judgedPath),
		"judgement_sha256":    judgedDigest,
		"labelled_at":         time.Now().UTC().Format(time.RFC3339),
	})
}

// ------------------------------------------------------------- plumbing.

// readSlice loads one slice file and keeps the cases of that slice.
func readSlice(g *goldset.Guard, file, slice string) ([]goldset.Case, error) {
	p, err := g.Resolve(file)
	if err != nil {
		return nil, err
	}
	all, err := goldset.ReadJSONL(p)
	if err != nil {
		return nil, err
	}
	out := make([]goldset.Case, 0, len(all))
	for _, c := range all {
		if c.Slice == slice {
			out = append(out, c)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%s holds no %s case", file, slice)
	}
	return out, nil
}

// resolvePool locates the pooling file and derives the run id from its name.
// An absent file is the expected state before the first priming run, and the
// message says which command produces it — the alternative is a panic on a nil
// pool or, worse, an empty template that looks like a finished one.
func resolvePool(g *goldset.Guard, name string) (path, runID string, err error) {
	if name == "" {
		matches, globErr := filepath.Glob(filepath.Join(g.Root(), "pool-*.jsonl"))
		if globErr != nil {
			return "", "", globErr
		}
		switch {
		case len(matches) == 0:
			return "", "", fmt.Errorf("pool file missing — run ctx-armsweep prime first (searched %s/pool-*.jsonl)", g.Root())
		case len(matches) > 1:
			return "", "", fmt.Errorf("%d pool files in %s — name one with -pool", len(matches), g.Root())
		}
		name = filepath.Base(matches[0])
	}
	path, err = g.Resolve(name)
	if err != nil {
		return "", "", err
	}
	if _, statErr := os.Stat(path); statErr != nil {
		return "", "", fmt.Errorf("pool file missing — run ctx-armsweep prime first (%s)", name)
	}
	return path, runIDOf(name), nil
}

// resolveKey finds the control key belonging to a template. Guessing it from
// the template name is a convenience; the key is never optional, because the
// control hit rate cannot be computed without it and a stamp without that
// number would claim a bias probe that never ran.
func resolveKey(g *goldset.Guard, keyName, judgedName string) (string, error) {
	if keyName == "" {
		keyName = keyPrefix + runIDOf(judgedName) + ".json"
	}
	p, err := g.Resolve(keyName)
	if err != nil {
		return "", err
	}
	if _, statErr := os.Stat(p); statErr != nil { //nolint:gosec // G703: p came out of Guard.Resolve, which is the path check
		return "", fmt.Errorf("control key %s missing — the control hit rate is not computable without it", keyName)
	}
	return p, nil
}

// runIDOf strips the conventional prefix and extension from an artefact name.
func runIDOf(name string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	for _, prefix := range []string{"pool-", "judge-", keyPrefix} {
		base = strings.TrimPrefix(base, prefix)
	}
	return base
}

// allCandidateIDs is the deduplicated, sorted id set of every template row.
func allCandidateIDs(pooled []goldset.PooledCase) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range pooled {
		for _, id := range p.BlockIDs {
			if !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// poolSizes reports the upper median, the maximum and the total row count.
func poolSizes(pooled []goldset.PooledCase) (p50, maxN, total int) {
	sizes := make([]int, 0, len(pooled))
	for _, p := range pooled {
		sizes = append(sizes, len(p.BlockIDs))
		total += len(p.BlockIDs)
		if len(p.BlockIDs) > maxN {
			maxN = len(p.BlockIDs)
		}
	}
	sort.Ints(sizes)
	if len(sizes) > 0 {
		p50 = sizes[len(sizes)/2]
	}
	return p50, maxN, total
}

// round4 trims a rate to four decimals so the stamp carries a readable number
// instead of the full binary expansion of a ratio.
func round4(f float64) float64 { return math.Round(f*1e4) / 1e4 }
