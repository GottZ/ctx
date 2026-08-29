package main

// The `-draw` and `-calibrate` runs of wave C3-4a (design/05a §C3-2-D05-7).
//
// They are two invocations for the same reason `-llm` and `-kappa` are: a judge
// fills the sheet in between. `-draw` writes the answer key and the blind sheet
// and never looks at a verdict again; `-calibrate` reads the filled sheet back
// and computes everything the gates rest on. A single command that drew and
// calibrated in one breath would report a number nobody could have contradicted
// in between.
//
// Part of ctx by GottZ — The memory your LLM pretends to have.
// Source: https://github.com/GottZ/ctx

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

// drawKeyPrefix and sheetPrefix are the file-name prefixes of the two draw
// artefacts. Separate files, like PoolKey and the pooling template: the sheet
// must be readable without the key being readable with it.
const (
	drawKeyPrefix = "draw-key-"
	sheetPrefix   = "fable-sheet-"
)

// drawRunIDOf strips the C3-4a artefact prefixes on top of the ones runIDOf
// knows. It is a second function rather than a widened runIDOf: that one is on
// the `-llm`, `-kappa` and `ingest` paths, and adding a prefix to it would
// change which control key those three resolve by default.
func drawRunIDOf(name string) string {
	base := filepath.Base(name)
	base = strings.TrimSuffix(base, filepath.Ext(base))
	for _, p := range []string{"judged-", sheetPrefix, drawKeyPrefix, "pool-", "judge-", keyPrefix} {
		base = strings.TrimPrefix(base, p)
	}
	return base
}

// judgeDraw builds the answer key and the blind sheet.
func judgeDraw(c *common, o judgeOpts) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	if o.judged == "" {
		return fmt.Errorf("-judged fehlt: der geurteilte Bestand aus `ctx-goldset judge -llm` " +
			"(C3-4a verwendet den 6 475er-Lauf wieder, es gibt keinen zweiten LLM-Lauf)")
	}
	// The seed has NO default, for the same reason -kappa-min has none: it
	// fixes which 20 queries become the metric anchor, and a tool that picked
	// it would be the tool choosing the sample it is later measured on.
	if o.drawSeed == 0 {
		return fmt.Errorf("-draw-seed fehlt: der Ziehungs-Seed ist eine sichtbare Lead-Entscheidung " +
			"und wird im Ziehungs-Schlüssel festgeschrieben (§C3-2-D05-3)")
	}
	spec, err := drawSpecOf(o)
	if err != nil {
		return err
	}
	in, err := readDrawInput(g, c, o, spec)
	if err != nil {
		return err
	}
	key, err := goldset.Draw(in)
	if err != nil {
		return err
	}
	cells, err := goldset.ReadTemplateCells(mustResolve(g, o.judged))
	if err != nil {
		return err
	}
	sheet, err := goldset.RenderFableSheetJSONL(key, cells)
	if err != nil {
		return err
	}
	base := o.out
	if base == "" {
		base = drawRunIDOf(o.judged)
	}
	keyPath, err := g.Resolve(drawKeyPrefix + base + ".json")
	if err != nil {
		return err
	}
	sheetPath, err := g.Resolve(sheetPrefix + base + ".jsonl")
	if err != nil {
		return err
	}
	if werr := goldset.WriteDrawKey(keyPath, key); werr != nil {
		return werr
	}
	if werr := goldset.WriteOwnerOnly(sheetPath, sheet); werr != nil {
		return werr
	}
	reportDraw(key, keyPath, sheetPath)
	return nil
}

// readDrawInput assembles the four inputs of a draw.
func readDrawInput(g *goldset.Guard, c *common, o judgeOpts, spec goldset.DrawSpec) (goldset.DrawInput, error) {
	var in goldset.DrawInput
	judgedPath, err := g.Resolve(o.judged)
	if err != nil {
		return in, err
	}
	cells, err := goldset.ReadTemplateCells(judgedPath)
	if err != nil {
		return in, err
	}
	judged, err := goldset.ParseJudgements(judgedPath)
	if err != nil {
		return in, err
	}
	poolName := o.pool
	if poolName == "" {
		poolName = "pool-" + drawRunIDOf(o.judged) + ".jsonl"
	}
	poolPath, err := g.Resolve(poolName)
	if err != nil {
		return in, err
	}
	entries, err := goldset.ReadPool(poolPath)
	if err != nil {
		return in, err
	}
	keyPath, err := resolveKey(g, o.key, drawRunIDOf(o.judged)+".jsonl")
	if err != nil {
		return in, err
	}
	poolKey, err := goldset.ReadPoolKey(keyPath)
	if err != nil {
		return in, fmt.Errorf("control key %s: %w", filepath.Base(keyPath), err)
	}
	labelName := o.labels
	if labelName == "" {
		labelName = goldset.FileRegimeLabels
	}
	labelPath, err := g.Resolve(labelName)
	if err != nil {
		return in, err
	}
	regimes, err := goldset.ReadRegimeLabels(labelPath)
	if err != nil {
		return in, err
	}
	_ = c
	return goldset.DrawInput{
		SourceRun: drawRunIDOf(o.judged), Cells: cells, Judged: judged,
		Pool: entries, Key: poolKey, Regimes: regimes, Spec: spec,
	}, nil
}

// drawSpecOf parses the allocation flags.
func drawSpecOf(o judgeOpts) (goldset.DrawSpec, error) {
	spec := goldset.DefaultDrawSpec(o.drawSeed)
	if o.coreQueries != "" {
		// Lower bound 0: the two numbers name POPULATIONS, and a single-regime
		// slice has none in one of them (goldset.DrawSpec.ValidateCore). Which
		// combinations of 0 are admissible is decided there, once, for every
		// caller of Draw — this is only where the flag text becomes numbers.
		v, err := intList(o.coreQueries, 2, 0, "-core-queries", "local,global")
		if err != nil {
			return spec, err
		}
		spec.CoreLocal, spec.CoreGlobal = v[0], v[1]
		if verr := spec.ValidateCore(); verr != nil {
			return spec, fmt.Errorf("-core-queries %q: %w", o.coreQueries, verr)
		}
	}
	if o.strata != "" {
		v, err := intList(o.strata, 5, 1, "-strata", "S1,S2,S3,S4,S0")
		if err != nil {
			return spec, err
		}
		spec.S1, spec.S2, spec.S3, spec.S4, spec.S0 = v[0], v[1], v[2], v[3], v[4]
	}
	return spec, nil
}

// intList parses one comma-separated allocation flag. `low` is the smallest
// admissible value, and the two flags differ in it on purpose: `-strata` names
// five SAMPLE SIZES inside strata that are drawn from by construction, where a 0
// would be a stratum weight of N/0 and takeByHash refuses it anyway;
// `-core-queries` names two populations of which a slice may have only one.
func intList(s string, want, low int, flag, shape string) ([]int, error) {
	parts := strings.Split(s, ",")
	if len(parts) != want {
		return nil, fmt.Errorf("%s erwartet %d Zahlen (%s), bekam %q", flag, want, shape, s)
	}
	out := make([]int, 0, want)
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil || n < low {
			if low > 0 {
				return nil, fmt.Errorf("%s: %q ist keine positive Zahl", flag, p)
			}
			return nil, fmt.Errorf("%s: %q ist keine Zahl ≥ %d", flag, p, low)
		}
		out = append(out, n)
	}
	return out, nil
}

func reportDraw(key goldset.DrawKey, keyPath, sheetPath string) {
	fmt.Printf("Ziehung %s · Seed %d · Kern %d Queries (%d local / %d global)\n",
		key.SourceRun, key.Spec.Seed, len(key.CoreQueries), key.Spec.CoreLocal, key.Spec.CoreGlobal)
	fmt.Println("| Schicht | N (Population) | n (gezogen) | Gewicht N/n |")
	for _, s := range []string{goldset.StratumCore, goldset.StratumS1, goldset.StratumS2,
		goldset.StratumS3, goldset.StratumS4, goldset.StratumS0} {
		n, drawn := key.Population[s], key.Sampled[s]
		if drawn == 0 {
			continue
		}
		fmt.Printf("| %-5s | %14d | %11d | %11.4f |\n", s, n, drawn, float64(n)/float64(drawn))
	}
	fmt.Printf("Bogen: %d Zellen · geschrieben: %s %s (0600)\n",
		len(key.Cells), filepath.Base(keyPath), filepath.Base(sheetPath))
	fmt.Printf("Urteiler: %s\n", goldset.FableJudge)
}

// ----------------------------------------------------------------- -gold.

// The two gold sources of §C3-2-D05-7 step 4. They live side by side rather
// than one replacing the other, because the flip test needs BOTH over the same
// queries — that is the whole construction.
const (
	GoldSourceFableCore   = "fable-kern"
	GoldSourceJudgeCarry  = "judge-uebertragen"
	goldCoreSuffix        = "-kern.jsonl"
	goldCarrySuffix       = "-uebertragen.jsonl"
	goldDefaultBasePrefix = "gold-"
)

// judgeGold writes the two named gold variants.
//
// Neither of them touches the slice file: `ingest` rewrites g-real.jsonl in
// place and backs it up, which is right for the one canonical labelling and
// wrong for two variants that exist to be compared against each other. These
// are new files, and the slice file stays what it was.
func judgeGold(c *common, o judgeOpts) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	if o.sheet == "" || o.judged == "" {
		return fmt.Errorf("-sheet und -judged werden beide gebraucht: der ausgefüllte blinde Bogen " +
			"stellt das Fable-Gold des Kerns, der Maschinen-Bestand das übertragene Gold")
	}
	key, answers, err := readFilledSheet(g, o)
	if err != nil {
		return err
	}
	sliceName := o.slice
	if sliceName == "" {
		sliceName = goldset.FileReal
	}
	cases, err := readSlice(g, sliceName, goldset.SliceReal)
	if err != nil {
		return err
	}
	base := o.out
	if base == "" {
		base = goldDefaultBasePrefix + drawRunIDOf(o.sheet)
	}

	coreJudged, err := goldset.FableJudgements(key, answers, goldset.StratumCore)
	if err != nil {
		return err
	}
	if werr := writeGoldVariant(g, base+goldCoreSuffix, cases, coreJudged, GoldSourceFableCore, true); werr != nil {
		return werr
	}
	judgedPath, err := g.Resolve(o.judged)
	if err != nil {
		return err
	}
	machine, err := goldset.ParseJudgements(judgedPath)
	if err != nil {
		return err
	}
	return writeGoldVariant(g, base+goldCarrySuffix, cases, machine, GoldSourceJudgeCarry, false)
}

// writeGoldVariant labels one variant and reports its profile.
func writeGoldVariant(g *goldset.Guard, name string, cases []goldset.Case,
	judged map[string][]goldset.Judgement, source string, restrict bool,
) error {
	labelled, st, err := goldset.ApplyLabelsNamed(cases, judged, source, restrict)
	if err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	p, err := g.Resolve(name)
	if err != nil {
		return err
	}
	if werr := goldset.WriteJSONLKeepIndex(p, labelled); werr != nil {
		return werr
	}
	digest, err := goldset.FileDigest(p)
	if err != nil {
		return err
	}
	fmt.Printf("%s: n=%d gelabelt=%d ohne_Relevante=%d Urteile=%d relevant=%d p50=%d max=%d → %s (%s)\n",
		source, st.Cases, st.Labelled, st.NoRelevant, st.Judged, st.Relevant,
		st.PoolP50, st.PoolMax, filepath.Base(p), digest[:12])
	return nil
}

// readFilledSheet loads the draw key and the filled sheet that belongs to it.
func readFilledSheet(g *goldset.Guard, o judgeOpts) (goldset.DrawKey, []goldset.FableJudgement, error) {
	keyName := o.drawKey
	if keyName == "" {
		keyName = drawKeyPrefix + drawRunIDOf(o.sheet) + ".json"
	}
	keyPath, err := g.Resolve(keyName)
	if err != nil {
		return goldset.DrawKey{}, nil, err
	}
	key, err := goldset.ReadDrawKey(keyPath)
	if err != nil {
		return goldset.DrawKey{}, nil, err
	}
	sheetPath, err := g.Resolve(o.sheet)
	if err != nil {
		return goldset.DrawKey{}, nil, err
	}
	answers, err := goldset.ParseFableSheet(sheetPath)
	if err != nil {
		return goldset.DrawKey{}, nil, err
	}
	return key, answers, nil
}

// ------------------------------------------------------------- -calibrate.

// judgeCalibrate joins the filled sheet back to the key and writes the
// C3-4a Kipp-Report.
func judgeCalibrate(c *common, o judgeOpts) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	th, err := thresholdsOf(o)
	if err != nil {
		return err
	}
	if o.sheet == "" {
		return fmt.Errorf("-sheet fehlt: der ausgefüllte blinde Bogen aus `ctx-goldset judge -draw`")
	}
	sheetPath, err := g.Resolve(o.sheet)
	if err != nil {
		return err
	}
	keyName := o.drawKey
	if keyName == "" {
		keyName = drawKeyPrefix + drawRunIDOf(o.sheet) + ".json"
	}
	keyPath, err := g.Resolve(keyName)
	if err != nil {
		return err
	}
	key, err := goldset.ReadDrawKey(keyPath)
	if err != nil {
		return err
	}
	answers, err := goldset.ParseFableSheet(sheetPath)
	if err != nil {
		return err
	}
	pairs, err := goldset.JoinCalibration(key, answers)
	if err != nil {
		return err
	}
	res := goldset.Calibrate(pairs)
	slice := res.Slice
	if slice == "" {
		slice = goldset.SliceReal
	}
	flips, err := readFlips(g, o.flip)
	if err != nil {
		return err
	}
	bySlice := map[string]goldset.CalibrationResult{slice: res}
	gates := goldset.CalibratedGateReport(bySlice, flips, th)
	return emitCalibration(g, o, bySlice, flips, gates, th, key)
}

// thresholdsOf validates the stated rules of the run.
func thresholdsOf(o judgeOpts) (goldset.CalibrationThresholds, error) {
	th := goldset.DefaultCalibrationThresholds()
	if isNaN(o.kappaMin) {
		return th, fmt.Errorf("-kappa-min fehlt: die κ-Schranke wird VORAB genannt (D-05 §4.5 (3)) — " +
			"sie entscheidet unter §C3-2-D05-6 die Reichweite der Judge-Labels, nicht mehr das Gate")
	}
	if o.kappaMin < -1 || o.kappaMin > 1 {
		return th, fmt.Errorf("-kappa-min %.4f liegt außerhalb von [-1, 1]", o.kappaMin)
	}
	th.KappaMin = o.kappaMin
	if o.rhoMin >= 0 {
		th.RhoMin = o.rhoMin
	}
	if o.piMin >= 0 {
		th.PiMin = o.piMin
	}
	if o.unsureMax >= 0 {
		th.UnsureMax = o.unsureMax
	}
	return th, nil
}

// readFlips loads the metric flip results `ctx-armsweep compare` produced on
// the core. Absent is not "no flip" — the gate reads it as a missing primary
// authority (goldset.flipReasons).
func readFlips(g *goldset.Guard, name string) (map[string]goldset.MetricFlip, error) {
	out := map[string]goldset.MetricFlip{}
	if name == "" {
		return out, nil
	}
	p, err := g.Resolve(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p) //nolint:gosec // G703: p came out of Guard.Resolve
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	return out, nil
}

// emitCalibration writes the report in both forms and merges the provenance.
func emitCalibration(g *goldset.Guard, o judgeOpts, res map[string]goldset.CalibrationResult,
	flips map[string]goldset.MetricFlip, gates []goldset.GateVerdict,
	th goldset.CalibrationThresholds, key goldset.DrawKey,
) error {
	base := o.out
	if base == "" {
		base = "kappa-" + drawRunIDOf(o.sheet)
	}
	report := goldset.RenderCalibrationReport(th, res, flips, gates)
	mdPath, err := g.Resolve(base + ".md")
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(mdPath, []byte(report)); werr != nil {
		return werr
	}
	doc := map[string]any{
		"version": 1, "created_at": time.Now().UTC().Format(time.RFC3339),
		"source_run": key.SourceRun, "draw_seed": key.Spec.Seed, "spec": key.Spec,
		"thresholds": map[string]any{
			"kappa_min": th.KappaMin, "rho_min": th.RhoMin, "rho_ci_lo_min": th.RhoCILoMin,
			"pi_min": th.PiMin, "pi_ci_lo_min": th.PiCILoMin, "unsure_max": th.UnsureMax,
		},
		"calibration": res, "metric_flip": flips, "gates": gates,
	}
	jb, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	jsonPath, err := g.Resolve(base + ".json")
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(jsonPath, append(jb, '\n')); werr != nil {
		return werr
	}
	fmt.Print(report)
	undecided := 0
	for _, gate := range gates {
		if gate.Verdict == goldset.GateUndecided {
			undecided++
		}
	}
	fmt.Printf("\nGates: %d von %d %q · Berichte: %s %s\n",
		undecided, len(gates), goldset.GateUndecided, filepath.Base(mdPath), filepath.Base(jsonPath))
	return stampCalibration(g, o.stampName, res, mdPath, jsonPath, key)
}

// stampCalibration merges the calibration provenance into the slice stamp.
func stampCalibration(g *goldset.Guard, stampName string, res map[string]goldset.CalibrationResult,
	mdPath, jsonPath string, key goldset.DrawKey,
) error {
	stampPath, err := g.Resolve(stampName)
	if err != nil {
		return err
	}
	mdDigest, err := goldset.FileDigest(mdPath)
	if err != nil {
		return err
	}
	jsonDigest, err := goldset.FileDigest(jsonPath)
	if err != nil {
		return err
	}
	for slice, r := range res {
		if err := goldset.MergeStampSlice(stampPath, slice, map[string]any{
			"calibration_judge":         goldset.FableJudge,
			"calibration_draw_seed":     key.Spec.Seed,
			"calibration_source_run":    key.SourceRun,
			"calibration_pairs":         r.Pairs,
			"calibration_kappa":         round4(r.Unweighted.Kappa),
			"calibration_kappa_w":       round4(r.Weighted.Kappa),
			"calibration_rho":           round4(r.Rho.Value),
			"calibration_pi":            round4(r.Pi.Value),
			"calibration_control_rate":  round4(r.ControlRate),
			"calibration_control_n":     r.ControlN,
			"calibration_report":        filepath.Base(mdPath),
			"calibration_report_sha256": mdDigest,
			"calibration_json":          filepath.Base(jsonPath),
			"calibration_json_sha256":   jsonDigest,
			"calibrated_at":             time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
	}
	return nil
}
