package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
)

// cmdPrime runs the unpinned pass and writes pins, the prime stamp and — for
// G-REAL — the pooling file.
func cmdPrime(ctx context.Context, c *common) error {
	// Shadow types belong to the conditional DUMP, never to priming: the pins a
	// prime collects have to be the same for the base and the conditional run,
	// and a prime that measured a different corpus than the one it pins for
	// would make the two dumps incomparable in a way no stamp would show.
	// Refused rather than ignored — a silently dropped flag is worse.
	if len(c.shadowTypeNames()) > 0 {
		return fmt.Errorf("-shadow-types gilt nur für `dump` (der Prime-Lauf sammelt Pins für BEIDE Dumps)")
	}
	gold, err := c.goldGuard()
	if err != nil {
		return err
	}
	dumps, err := c.dumpGuard(gold)
	if err != nil {
		return err
	}
	cases, err := armsweep.LoadCases(gold, c.sliceNames())
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("keine Gold-Fälle geladen (Slices %q)", c.slices)
	}
	r, err := c.runner(gold, dumps)
	if err != nil {
		return err
	}

	pins, pools, stamp, err := r.Prime(ctx, cases)
	if err != nil {
		return err
	}

	pinFile := "pins-" + c.id() + ".jsonl"
	pinPath, err := gold.Resolve(pinFile)
	if err != nil {
		return err
	}
	if err := armsweep.WritePins(pinPath, pins); err != nil {
		return err
	}
	digest, err := goldset.FileDigest(pinPath)
	if err != nil {
		return err
	}
	stamp.PinFile, stamp.PinSHA256 = pinFile, digest

	if len(pools) > 0 {
		poolFile := "pool-" + c.id() + ".jsonl"
		poolPath, perr := gold.Resolve(poolFile)
		if perr != nil {
			return perr
		}
		if perr = armsweep.WritePool(poolPath, pools); perr != nil {
			return perr
		}
		stamp.PoolFile = poolFile
	}

	stampPath, err := gold.Resolve("prime-" + c.id() + ".json")
	if err != nil {
		return err
	}
	if err := armsweep.WriteJSONFile(stampPath, stamp); err != nil {
		return err
	}
	fmt.Printf("prime %s: %d Pins, %d ausgeschlossen, p50 %d ms, p95 %d ms → %s\n",
		c.id(), stamp.Pins, len(stamp.Excluded), stamp.Latency.P50, stamp.Latency.P95, pinFile)
	return nil
}

// cmdDump runs the pinned pass, brackets it with the drift census and writes
// the dump plus its stamp.
func cmdDump(ctx context.Context, c *common, pinFile string) error {
	gold, err := c.goldGuard()
	if err != nil {
		return err
	}
	dumps, err := c.dumpGuard(gold)
	if err != nil {
		return err
	}
	cases, err := armsweep.LoadCases(gold, c.sliceNames())
	if err != nil {
		return err
	}
	if len(cases) == 0 {
		return fmt.Errorf("keine Gold-Fälle geladen (Slices %q)", c.slices)
	}
	pins, pinFile, pinDigest, err := loadPins(gold, pinFile)
	if err != nil {
		return err
	}
	goldStamp, err := loadGoldStamp(gold)
	if err != nil {
		return err
	}

	r, err := c.runner(gold, dumps)
	if err != nil {
		return err
	}

	// Gate (l) BEFORE the first measurement request (design/05 §5 B4b): a
	// shadow dump belongs into a restored measure copy, because the shadow
	// corpus it measures spends index budget on every production query of the
	// instance it lives in. The kind the instance reported goes into the stamp
	// either way — also under the override, so the report cannot hide it.
	instanceKind, err := c.gateInstance(ctx, r)
	if err != nil {
		return err
	}

	recs, stamp, err := r.Dump(ctx, cases, pins, goldIDsOf(cases), goldStamp.CorpusMaxCreatedAt)
	if err != nil {
		return err
	}

	stamp.InstanceKind, stamp.AllowLiveInstance = instanceKind, c.allowLive
	stamp.PinFile, stamp.PinSHA256 = pinFile, pinDigest
	stamp.PinRunID = strings.TrimSuffix(strings.TrimPrefix(pinFile, "pins-"), ".jsonl")
	stamp.AllowOutsideGoldset = c.allowOutside
	if err := fillInstanceStamp(ctx, c, &stamp); err != nil {
		return err
	}
	if err := fillGoldDigests(gold, cases, &stamp); err != nil {
		return err
	}

	dumpFile := c.id() + ".jsonl"
	dumpPath, err := dumps.Resolve(dumpFile)
	if err != nil {
		return err
	}
	if err := armsweep.WriteRecords(dumpPath, recs); err != nil {
		return err
	}
	stamp.DumpFile = filepath.Join(armsweep.DumpDirName, dumpFile)

	if stamp.Aborted {
		marked, merr := armsweep.MarkAborted(dumpPath)
		if merr != nil {
			return merr
		}
		stamp.DumpFile = filepath.Join(armsweep.DumpDirName, filepath.Base(marked))
	}
	stampPath, err := dumps.Resolve(c.id() + ".stamp.json")
	if err != nil {
		return err
	}
	if err := armsweep.WriteJSONFile(stampPath, stamp); err != nil {
		return err
	}

	fmt.Printf("dump %s: %d Fälle, %d ausgeschlossen, p50 %d ms, p95 %d ms → %s\n",
		c.id(), stamp.Records, len(stamp.Excluded), stamp.Latency.P50, stamp.Latency.P95, stamp.DumpFile)
	if stamp.Aborted {
		for _, reason := range stamp.Drift.Reasons {
			fmt.Fprintln(os.Stderr, "  ABBRUCH:", reason)
		}
		return fmt.Errorf("%w: %s", errDumpAborted, stamp.DumpFile)
	}
	return nil
}

// gateInstance runs gate (l) and reports what it found. A dry run asks nothing:
// it never touches an instance, so there is no instance to make a claim about —
// and the stamp of a dry run says so by carrying no kind at all.
func (c *common) gateInstance(ctx context.Context, r *armsweep.Runner) (string, error) {
	if c.dryRun || len(r.ShadowTypes) == 0 {
		return "", nil
	}
	kind, err := armsweep.GateInstanceKind(ctx, r.Client, r.ShadowTypes, c.allowLive)
	if err != nil {
		return kind, err
	}
	if c.allowLive && kind != armsweep.InstanceKindMeasureCopy {
		fmt.Fprintf(os.Stderr,
			"WARNUNG: Schatten-Dump gegen %s=%q — nur wegen -allow-live-instance; steht im Report.\n",
			armsweep.SettingInstanceKind, kind)
	}
	return kind, nil
}

// cmdScore is the offline half: no instance, no clock beyond the header line.
func cmdScore(c *common, dumpA, dumpB, outDir, name, dampingType string) error {
	if dumpA == "" {
		return fmt.Errorf("-dump ist Pflicht")
	}
	gold, err := c.goldGuard()
	if err != nil {
		return err
	}
	dumps, err := c.dumpGuard(gold)
	if err != nil {
		return err
	}
	goldStamp, err := loadGoldStamp(gold)
	if err != nil {
		return err
	}

	recsA, stampA, err := loadDump(dumps, dumpA)
	if err != nil {
		return err
	}
	in := armsweep.ScoreInput{
		RecordsA: recsA, StampA: stampA, DampingType: dampingType,
		Seed: c.seed, GitRevision: buildRev(), GoldStamp: goldStamp,
	}
	if dumpB != "" {
		recsB, stampB, berr := loadDump(dumps, dumpB)
		if berr != nil {
			return berr
		}
		in.RecordsB, in.StampB = recsB, &stampB
	}
	body, err := armsweep.Score(in)
	if err != nil {
		return err
	}

	reports, err := c.reportGuard(outDir)
	if err != nil {
		return err
	}
	base := name
	if base == "" {
		base = "armsweep-" + stampA.RunID
	}
	jsonPath, err := reports.Resolve(base + ".json")
	if err != nil {
		return err
	}
	mdPath, err := reports.Resolve(base + ".md")
	if err != nil {
		return err
	}
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	if err := armsweep.WriteReport(jsonPath, generatedAt, body); err != nil {
		return err
	}
	if err := armsweep.WriteMarkdown(mdPath, generatedAt, body); err != nil {
		return err
	}
	fmt.Printf("score: %d Konfigurationen%s, G-NOISE %s → %s\n",
		len(body.Configs), dampingNote(body), interpretable(body.Interpretable), jsonPath)
	return nil
}

// dampingNote names the second family in the summary line. Silent when no
// damping type was swept, so the line of a plain run is the line it always was.
func dampingNote(body armsweep.ReportBody) string {
	if len(body.Damping) == 0 {
		return ""
	}
	return fmt.Sprintf(" + %d Damping-Stützstellen auf %q", len(body.Damping), body.DampingType)
}

func interpretable(v bool) string {
	if v {
		return "bestanden"
	}
	return "NICHT bestanden — keine Variante ist ein Ergebnis"
}

// ------------------------------------------------------------- loading.

// loadPins resolves the pin file, defaulting to the most recent one in the gold
// directory. The default is a convenience with a hard edge: it prints which
// file it chose, because a dump pinned against the wrong priming run is a
// different measurement wearing the same name.
func loadPins(g *goldset.Guard, name string) (map[string]armsweep.Pin, string, string, error) {
	if name == "" {
		latest, err := latestPinFile(g.Root())
		if err != nil {
			return nil, "", "", err
		}
		name = latest
		fmt.Fprintf(os.Stderr, "Pin-Datei nicht angegeben — nehme %s\n", name)
	}
	p, err := g.Resolve(name)
	if err != nil {
		return nil, "", "", err
	}
	pins, err := armsweep.ReadPins(p)
	if err != nil {
		return nil, "", "", err
	}
	digest, err := goldset.FileDigest(p)
	if err != nil {
		return nil, "", "", err
	}
	return pins, filepath.Base(name), digest, nil
}

func latestPinFile(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "pins-") && strings.HasSuffix(e.Name(), ".jsonl") {
			names = append(names, e.Name())
		}
	}
	if len(names) == 0 {
		return "", fmt.Errorf("keine pins-*.jsonl in %s — erst `ctx-armsweep prime` fahren", root)
	}
	sort.Strings(names) // run ids are lexicographically sortable UTC stamps
	return names[len(names)-1], nil
}

// loadDump reads a dump and its sibling stamp. The stamp is mandatory: it
// carries the exclusion list, the drift verdict and the instance provenance,
// and a report built without it would silently claim a clean run.
func loadDump(dumps *goldset.Guard, name string) ([]armsweep.Record, armsweep.DumpStamp, error) {
	p, err := dumps.Resolve(filepath.Base(name))
	if err != nil {
		return nil, armsweep.DumpStamp{}, err
	}
	recs, err := armsweep.ReadRecords(p)
	if err != nil {
		return nil, armsweep.DumpStamp{}, err
	}
	stampName := strings.TrimSuffix(strings.TrimSuffix(filepath.Base(p), ".aborted"), ".jsonl") + ".stamp.json"
	sp, err := dumps.Resolve(stampName)
	if err != nil {
		return nil, armsweep.DumpStamp{}, err
	}
	var stamp armsweep.DumpStamp
	if err := armsweep.ReadJSONFile(sp, &stamp); err != nil {
		return nil, armsweep.DumpStamp{}, fmt.Errorf("kein lesbarer Dump-Stempel %s: %w", stampName, err)
	}
	return recs, stamp, nil
}

func loadGoldStamp(g *goldset.Guard) (goldset.Stamp, error) {
	p, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		return goldset.Stamp{}, err
	}
	return goldset.ReadStamp(p)
}

// goldIDsOf collects the distinct labelled block ids the drift census watches.
func goldIDsOf(cases []goldset.Case) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cases {
		for _, id := range c.GoldIDs {
			if !seen[id] {
				seen[id], out = true, append(out, id)
			}
		}
	}
	sort.Strings(out)
	return out
}

// fillInstanceStamp records what the MEASURED instance was, not what the
// measuring host is: the applied schema generation and the post-fusion stage
// state, both read off the API.
func fillInstanceStamp(ctx context.Context, c *common, stamp *armsweep.DumpStamp) error {
	if c.dryRun {
		stamp.PostFusionStages = map[string]any{}
		return nil
	}
	cl, err := c.client()
	if err != nil {
		return err
	}
	if stamp.MigrationsMax, err = cl.MigrationsMax(ctx); err != nil {
		return fmt.Errorf("Schema-Generation der Instanz: %w", err)
	}
	if stamp.PostFusionStages, err = cl.PostStageState(ctx); err != nil {
		return fmt.Errorf("Post-Fusion-Stufen: %w", err)
	}
	return nil
}

// fillGoldDigests binds the stamp to the exact slice bytes measured.
func fillGoldDigests(g *goldset.Guard, cases []goldset.Case, stamp *armsweep.DumpStamp) error {
	files := map[string]string{
		goldset.SliceKI:   goldset.FileKI,
		goldset.SliceQ:    goldset.FileQ,
		goldset.SliceReal: goldset.FileReal,
	}
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Slice]++
	}
	for _, slice := range armsweep.CanonicalSlices() {
		if counts[slice] == 0 {
			continue
		}
		p, err := g.Resolve(files[slice])
		if err != nil {
			return err
		}
		digest, err := goldset.FileDigest(p)
		if err != nil {
			return err
		}
		stamp.SliceFiles = append(stamp.SliceFiles,
			armsweep.SliceDigest{Slice: slice, File: files[slice], SHA256: digest, N: counts[slice]})
	}
	sp, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		return err
	}
	stamp.GoldStamp, err = goldset.FileDigest(sp)
	return err
}
