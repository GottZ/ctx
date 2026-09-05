package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/provenance"
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

// gateInstance reads the instance's provenance label and runs gate (l) on it.
//
// Two things that used to be one (wave X-W3a): the LABEL goes into EVERY dump
// stamp, because the campaign rule it serves — F-32, all dumps of one campaign
// come from one instance — is about every dump; the REFUSAL still belongs only
// to a run that builds a shadow corpus. Before X-W3a the label was read only
// when the refusal was, so ordinary dumps carried nothing and a mixed
// Live/measure-copy campaign compared clean (X-W2b §4.2, exit 0 measured).
//
// A dry run asks nothing: it never touches an instance, so there is no instance
// to make a claim about — and the stamp of a dry run says so by carrying no
// kind at all.
func (c *common) gateInstance(ctx context.Context, r *armsweep.Runner) (string, error) {
	if c.dryRun {
		return "", nil
	}
	kind, err := armsweep.StampInstanceKind(ctx, r.Client)
	if err != nil {
		return "", err
	}
	if err := armsweep.CheckInstanceKind(kind, r.ShadowTypes, c.allowLive); err != nil {
		return kind, err
	}
	if len(r.ShadowTypes) == 0 {
		return kind, nil
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
	split, err := loadRegimeSplit(gold, c.regimeLabels)
	if err != nil {
		return err
	}

	recsA, stampA, err := loadDump(dumps, dumpA)
	if err != nil {
		return err
	}
	in := armsweep.ScoreInput{
		RecordsA: recsA, StampA: stampA, DampingType: dampingType, RegimeSplit: split,
		Seed: c.seed, GitRevision: provenance.BuildRev(), GoldStamp: goldStamp,
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

// cmdCompare is the conditional comparison (design/05 §4.3, wave M-W3d):
// offline like `score`, but over FOUR dumps — the two conditions and the
// replicate pair that measures the noise floor they are read against.
//
// The report is written BEFORE a refusal is propagated, the same shape `dump`
// uses for an aborted run: a refused comparison is a finding about the
// instrument, and the artefact is the evidence for it.
func cmdCompare(c *common, base, cond, noisePair, outDir, name, conditionField string) error {
	if base == "" || cond == "" {
		return fmt.Errorf("-dump-base und -dump-cond sind Pflicht")
	}
	noise, err := splitNoisePair(noisePair)
	if err != nil {
		return err
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
	split, err := loadRegimeSplit(gold, c.regimeLabels)
	if err != nil {
		return err
	}

	in := armsweep.CompareInput{
		Seed: c.seed, GitRevision: provenance.BuildRev(), GoldStamp: goldStamp, RegimeSplit: split,
		ConditionField: conditionField,
	}
	if in.Base, err = loadDumpRef(dumps, armsweep.RoleBase, base); err != nil {
		return err
	}
	if in.Cond, err = loadDumpRef(dumps, armsweep.RoleCond, cond); err != nil {
		return err
	}
	for i, n := range noise {
		role := armsweep.RoleNoiseA
		if i == 1 {
			role = armsweep.RoleNoiseB
		}
		ref, rerr := loadDumpRef(dumps, role, n)
		if rerr != nil {
			return rerr
		}
		in.NoisePair = append(in.NoisePair, ref)
	}

	body, cmpErr := armsweep.Compare(in)
	// A refusal that still carries a body is a VERDICT about the measurement
	// (gate (b), a red noise floor): the artefact is the evidence for it and
	// gets written. A refusal WITHOUT one — a declaration this comparison
	// cannot honour — walked no dump at all, and writing its empty body would
	// put a report of zeros under a green "G-NOISE bestanden" line: the silent
	// success this wave exists to prevent (X-W3a).
	if cmpErr != nil && (!errors.Is(cmpErr, armsweep.ErrGateRefused) || body.Version == 0) {
		return cmpErr
	}

	reports, err := c.reportGuard(outDir)
	if err != nil {
		return err
	}
	base2 := name
	if base2 == "" {
		base2 = "compare-" + in.Cond.Stamp.RunID
	}
	jsonPath, err := reports.Resolve(base2 + ".json")
	if err != nil {
		return err
	}
	mdPath, err := reports.Resolve(base2 + ".md")
	if err != nil {
		return err
	}
	generatedAt := time.Now().UTC().Format(time.RFC3339)
	if err := armsweep.WriteCompareReport(jsonPath, generatedAt, body); err != nil {
		return err
	}
	if err := armsweep.WriteCompareMarkdown(mdPath, generatedAt, body); err != nil {
		return err
	}
	fmt.Printf("compare: %d gepaarte Fälle, %d ungepaart, %d Slices, G-NOISE %s%s → %s\n",
		body.Paired, body.UnpairedTotal, len(body.Effects), interpretable(!body.Refused),
		conditionNote(body.Condition), jsonPath)
	return cmpErr
}

// conditionNote puts the declaration into the one line an operator reads on the
// terminal. A comparison whose basis is not the fusion must never look like one
// that is.
func conditionNote(d *armsweep.ConditionDeclaration) string {
	if d == nil {
		return ""
	}
	out := fmt.Sprintf(", Bedingung `%s` auf Basis `%s`", d.Field, d.Basis)
	if !d.Applies {
		out += " (NICHT eingetreten — Basis und Bedingung tragen denselben Wert)"
	}
	return out
}

// splitNoisePair parses -noise-pair. Exactly two names: the pair IS the noise
// floor, and one dump measures nothing about repeat disagreement.
func splitNoisePair(csv string) ([]string, error) {
	var out []string
	for _, s := range strings.Split(csv, ",") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	if len(out) != 2 {
		return nil, fmt.Errorf(
			"%w: -noise-pair braucht genau zwei Dumps derselben Kampagne (V0,V0'), bekommen: %d",
			armsweep.ErrGateRefused, len(out))
	}
	return out, nil
}

// loadDumpRef resolves a dump and its stamp WITHOUT reading the records: the
// comparison streams them itself (design/05 §6.1).
func loadDumpRef(dumps *goldset.Guard, role, name string) (armsweep.DumpRef, error) {
	path, stamp, err := loadDumpStamp(dumps, name)
	if err != nil {
		return armsweep.DumpRef{}, err
	}
	return armsweep.DumpRef{Role: role, Path: path, Stamp: stamp}, nil
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
	p, stamp, err := loadDumpStamp(dumps, name)
	if err != nil {
		return nil, armsweep.DumpStamp{}, err
	}
	recs, err := armsweep.ReadRecords(p)
	if err != nil {
		return nil, armsweep.DumpStamp{}, err
	}
	return recs, stamp, nil
}

// loadDumpStamp resolves a dump path and reads its sibling stamp WITHOUT the
// records. `compare` needs exactly this: the stamps decide congruence before a
// single record is read, and the records are then streamed rather than loaded.
func loadDumpStamp(dumps *goldset.Guard, name string) (string, armsweep.DumpStamp, error) {
	p, err := dumps.Resolve(filepath.Base(name))
	if err != nil {
		return "", armsweep.DumpStamp{}, err
	}
	stem := strings.TrimSuffix(filepath.Base(p), ".aborted")
	stem = strings.TrimSuffix(strings.TrimSuffix(stem, armsweep.GzipSuffix), ".jsonl")
	stampName := stem + ".stamp.json"
	sp, err := dumps.Resolve(stampName)
	if err != nil {
		return "", armsweep.DumpStamp{}, err
	}
	var stamp armsweep.DumpStamp
	if err := armsweep.ReadJSONFile(sp, &stamp); err != nil {
		return "", armsweep.DumpStamp{}, fmt.Errorf("kein lesbarer Dump-Stempel %s: %w", stampName, err)
	}
	return p, stamp, nil
}

// loadRegimeSplit resolves the X-W0 label file, or returns the INACTIVE split.
//
// Opt-in by name rather than "use the file if it happens to be there": the file
// is untracked private data, and a report whose slice rows depended on whether a
// file existed at scoring time would not be reproducible from its own flags.
// The name is resolved through the gold guard like every other gold artefact.
func loadRegimeSplit(g *goldset.Guard, name string) (armsweep.RegimeSplit, error) {
	if name == "" {
		return armsweep.RegimeSplit{}, nil
	}
	p, err := g.Resolve(filepath.Base(name))
	if err != nil {
		return armsweep.RegimeSplit{}, err
	}
	regimes, err := goldset.ReadRegimeLabels(p)
	if err != nil {
		return armsweep.RegimeSplit{}, fmt.Errorf("Regime-Labels %s: %w", filepath.Base(name), err)
	}
	digest, err := goldset.FileDigest(p)
	if err != nil {
		return armsweep.RegimeSplit{}, err
	}
	return armsweep.RegimeSplit{File: filepath.Base(p), SHA256: digest, Regimes: regimes}, nil
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
	// The ANN window of the measured instance (M-W3d gate (h), design/05 §4.4b).
	// Recorded here for the same reason as the two above: `compare` is offline
	// and can only know what the dump wrote down while the instance was up.
	if stamp.EfSearch, err = cl.EfSearchEffective(ctx); err != nil {
		return fmt.Errorf("hnsw.ef_search der Instanz: %w", err)
	}
	return nil
}

// fillGoldDigests binds the stamp to the exact slice bytes measured.
//
// The slice→file table is NOT repeated here: armsweep.SliceFileOf is the single
// source of truth, because a second copy is what let wave M-W5 add four slices
// that the stamp then described as if they had never been measured.
func fillGoldDigests(g *goldset.Guard, cases []goldset.Case, stamp *armsweep.DumpStamp) error {
	counts := map[string]int{}
	for _, c := range cases {
		counts[c.Slice]++
	}
	for _, slice := range armsweep.CanonicalSlices() {
		if counts[slice] == 0 {
			continue
		}
		file, ok := armsweep.SliceFileOf(slice)
		if !ok {
			return fmt.Errorf("Gold-Slice %s hat keine Datei in der Registry — der Stempel könnte den Lauf nicht beschreiben", slice)
		}
		p, err := g.Resolve(file)
		if err != nil {
			return err
		}
		digest, err := goldset.FileDigest(p)
		if err != nil {
			return err
		}
		stamp.SliceFiles = append(stamp.SliceFiles,
			armsweep.SliceDigest{Slice: slice, File: file, SHA256: digest, N: counts[slice]})
	}
	sp, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		return err
	}
	stamp.GoldStamp, err = goldset.FileDigest(sp)
	return err
}
