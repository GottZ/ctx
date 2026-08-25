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
	control  int
	excerpt  int
	dryRun   bool
}

// cmdPool builds the blind judgement template for G-REAL (design 04 §4.5): the
// union of the four solo-arm heads plus a uniform control sample, deduplicated,
// seeded-permuted and stripped of every trace of where a candidate came from.
//
// The judging itself is a human act and is not in this tool. What is in this
// tool is the guarantee that the human cannot see the answer key.
func cmdPool(c *common, o poolOpts) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	cases, err := readSlice(g, goldset.FileReal, goldset.SliceReal)
	if err != nil {
		return err
	}
	poolPath, runID, err := resolvePool(g, o.poolFile)
	if err != nil {
		return err
	}
	entries, err := goldset.ReadPool(poolPath)
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
	base := o.out
	if base == "" {
		base = "judge-" + runID
	}
	jsonlPath, err := g.Resolve(base + ".jsonl")
	if err != nil {
		return err
	}
	mdPath, err := g.Resolve(base + ".md")
	if err != nil {
		return err
	}
	keyPath, err := g.Resolve(keyPrefix + runID + ".json")
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
	if err := stampIngest(g, stampName, outName, judgedPath, outPath, key, st, rate, hits, controls); err != nil {
		return err
	}
	fmt.Printf("G-REAL gelabelt: n=%d gelabelt=%d ohne_Relevante=%d Urteile=%d relevant=%d p50=%d max=%d\n",
		st.Cases, st.Labelled, st.NoRelevant, st.Judged, st.Relevant, st.PoolP50, st.PoolMax)
	fmt.Printf("Kontroll-Trefferquote: %.4f (%d von %d) — Sicherung: %s\n",
		rate, hits, controls, filepath.Base(backup))
	return nil
}

// stampIngest merges the G-REAL profile into STAMP.json. The merge runs on the
// raw document, so a field written by another wave survives this rewrite.
func stampIngest(g *goldset.Guard, stampName, outName, judgedPath, outPath string,
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
	return goldset.MergeStampSlice(stampPath, goldset.SliceReal, map[string]any{
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
