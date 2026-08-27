package main

// The `judge` subcommand of wave M-W9 (design/05 §4.5 (3), §5 B6, E2-4).
//
// Two runs, never one: `-llm` produces machine verdicts over the open cells of
// a pooling template and a calibration sheet beside them; `-kappa` reads that
// sheet back once the calibration judge has filled its second column and says,
// per gate, whether the machine verdicts carry the decision at all.
//
// Splitting them is the point. A single command that judged and then declared
// its own calibration in the same breath would report a number nobody could
// have contradicted in between.

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"sort"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

type judgeOpts struct {
	llm        bool
	kappa      bool
	template   string
	key        string
	controls   string
	out        string
	backend    string
	model      string
	timeoutSec int
	kappaMin   float64
	stampName  string
}

// cmdJudge dispatches the two runs and refuses everything else. Doing both in
// one invocation is refused rather than sequenced: the calibration column is
// filled by a judge between them.
func cmdJudge(c *common, o judgeOpts) error {
	switch {
	case o.llm && o.kappa:
		return fmt.Errorf("-llm und -kappa sind zwei Läufe: erst urteilen, dann — nach dem Befüllen " +
			"der Kontrolleur-Spalte — kalibrieren")
	case o.llm:
		return judgeLLM(c, o)
	case o.kappa:
		return judgeKappa(c, o)
	default:
		return fmt.Errorf("weder -llm noch -kappa genannt: judge urteilt (-llm) oder kalibriert (-kappa)")
	}
}

// ------------------------------------------------------------------- -llm.

// judgeLLM runs the machine judgements over the open cells of a template.
//
// Order matters and is the gate: template and control key first (so a run that
// cannot mark its calibration sample never starts), then the backend lookup and
// the on-prem assertion, and only then the first cell.
func judgeLLM(c *common, o judgeOpts) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	if o.template == "" {
		return fmt.Errorf("-template fehlt: die Urteils-Vorlage aus `ctx-goldset pool`")
	}
	tplPath, err := g.Resolve(o.template)
	if err != nil {
		return err
	}
	cells, err := goldset.ReadTemplateCells(tplPath)
	if err != nil {
		return err
	}
	keyPath, err := resolveKey(g, o.key, o.template)
	if err != nil {
		return err
	}
	key, err := goldset.ReadPoolKey(keyPath)
	if err != nil {
		return fmt.Errorf("control key %s: %w", filepath.Base(keyPath), err)
	}
	controls, err := goldset.MarkControls(cells, key)
	if err != nil {
		return err
	}

	db, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(ctx) }()
	be, err := db.LookupBackend(ctx, o.backend)
	if err != nil {
		return err
	}
	if err := goldset.RequireOnPrem(be); err != nil {
		return err // hard abort — block excerpts never leave the perimeter
	}
	judge, err := goldset.NewChatJudge(be, o.model, time.Duration(o.timeoutSec)*time.Second)
	if err != nil {
		return err
	}

	base := o.out
	if base == "" {
		base = "judged-" + runIDOf(o.template)
	}
	journalPath, err := g.Resolve(base + "-journal.jsonl")
	if err != nil {
		return err
	}
	fmt.Printf("Urteilslauf: Zellen=%d Kontrollen=%d Vorlage=%s Backend=%s Modell=%s Endpunkt=%s\n",
		len(cells), controls, filepath.Base(tplPath), be.Name, judge.Provenance().Model, judge.Provenance().Endpoint)

	st, runErr := goldset.RunJudge(ctx, judge, cells, journalPath)
	fmt.Printf("Urteile: neu=%d fortgesetzt=%d relevant=%d von %d Zellen (Journal: %s)\n",
		st.Judged, st.Resumed, st.Relevant, st.Cells, filepath.Base(journalPath))
	if runErr != nil {
		return fmt.Errorf("%w — das Journal hält die bereits gefällten Urteile; derselbe Aufruf setzt fort", runErr)
	}
	return emitJudged(g, c, o, base, cells, judge.Provenance())
}

// emitJudged writes the filled template, the calibration sheet and the judge's
// provenance into the stamp (§5 B6: model, endpoint and prompt hash are part of
// the record, not just of the call).
func emitJudged(g *goldset.Guard, c *common, o judgeOpts, base string,
	cells []goldset.JudgeCell, prov goldset.Generator,
) error {
	done, err := goldset.ReadJudgeJournal(mustResolve(g, base+"-journal.jsonl"))
	if err != nil {
		return err
	}
	filled, err := goldset.RenderJudgedJSONL(cells, done)
	if err != nil {
		return err
	}
	judgedPath, err := g.Resolve(base + ".jsonl")
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(judgedPath, filled); werr != nil {
		return werr
	}
	sheet, err := goldset.RenderControlSheetJSONL(cells, done, prov)
	if err != nil {
		return err
	}
	sheetPath, err := g.Resolve(base + "-controls.jsonl")
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(sheetPath, sheet); werr != nil {
		return werr
	}
	fmt.Printf("geschrieben: %s %s\n", filepath.Base(judgedPath), filepath.Base(sheetPath))
	fmt.Printf("Kontrolleur: %s — die Spalte control_judgement ist leer und wird von Hand gefüllt.\n",
		goldset.ControllerRole)
	return stampJudge(g, o.stampName, cells, prov, judgedPath, sheetPath, done)
}

// stampJudge merges the judging provenance into every slice the run touched.
func stampJudge(g *goldset.Guard, stampName string, cells []goldset.JudgeCell,
	prov goldset.Generator, judgedPath, sheetPath string, done map[string]goldset.JudgeDecision,
) error {
	stampPath, err := g.Resolve(stampName)
	if err != nil {
		return err
	}
	judgedDigest, err := goldset.FileDigest(judgedPath)
	if err != nil {
		return err
	}
	sheetDigest, err := goldset.FileDigest(sheetPath)
	if err != nil {
		return err
	}
	type counts struct{ cells, controls, relevant int }
	per := map[string]*counts{}
	for _, c := range cells {
		e, ok := per[c.Slice]
		if !ok {
			e = &counts{}
			per[c.Slice] = e
		}
		e.cells++
		if c.Control {
			e.controls++
		}
		if d, seen := done[goldset.CaseKey(c.Slice, c.Index, c.QuerySHA256)+"/"+c.BlockID]; seen && d.Relevant {
			e.relevant++
		}
	}
	names := make([]string, 0, len(per))
	for s := range per {
		names = append(names, s)
	}
	sort.Strings(names)
	for _, s := range names {
		e := per[s]
		if err := goldset.MergeStampSlice(stampPath, s, map[string]any{
			"judge_backend":         prov.Backend,
			"judge_model":           prov.Model,
			"judge_endpoint":        prov.Endpoint,
			"judge_locality":        prov.Locality,
			"judge_prompt_sha256":   prov.PromptSHA256,
			"judge_cells":           e.cells,
			"judge_controls":        e.controls,
			"judge_relevant":        e.relevant,
			"judge_file":            filepath.Base(judgedPath),
			"judge_sha256":          judgedDigest,
			"judge_controls_file":   filepath.Base(sheetPath),
			"judge_controls_sha256": sheetDigest,
			"judge_controller":      goldset.ControllerRole,
			"judged_at":             time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			return err
		}
	}
	return nil
}

// mustResolve is the journal path, already validated by the caller that created
// it in the same invocation.
func mustResolve(g *goldset.Guard, name string) string {
	p, err := g.Resolve(name)
	if err != nil {
		return name
	}
	return p
}

// ----------------------------------------------------------------- -kappa.

// judgeKappa computes Cohen's kappa over the filled calibration sheet and
// writes the Kipp-Report.
//
// The threshold has NO default and is not derivable from the data: naming it
// afterwards would make it a description of the result instead of a rule the
// result is measured against (D-05 §4.5 (3): "Vorab-Regel, nicht nachträglich").
func judgeKappa(c *common, o judgeOpts) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	if math.IsNaN(o.kappaMin) {
		return fmt.Errorf("-kappa-min fehlt: die κ-Schranke wird VORAB genannt (D-05 §4.5 (3)) — " +
			"es gibt bewusst keinen Vorgabewert, die Setzung ist eine Lead-Entscheidung")
	}
	if o.kappaMin < -1 || o.kappaMin > 1 {
		return fmt.Errorf("-kappa-min %.4f liegt außerhalb von [-1, 1]", o.kappaMin)
	}
	if o.controls == "" {
		return fmt.Errorf("-controls fehlt: der ausgefüllte Kontrollbogen aus `ctx-goldset judge -llm`")
	}
	sheetPath, err := g.Resolve(o.controls)
	if err != nil {
		return err
	}
	pairs, err := goldset.ParseControlSheet(sheetPath)
	if err != nil {
		return err
	}
	bySlice, overall := goldset.KappaBySlice(pairs)
	gates := goldset.JudgeGateReport(bySlice, o.kappaMin)
	report := goldset.RenderKappaReport(o.kappaMin, bySlice, overall, gates)

	base := o.out
	if base == "" {
		base = "kappa-" + runIDOf(o.controls)
	}
	reportPath, err := g.Resolve(base + ".md")
	if err != nil {
		return err
	}
	if werr := goldset.WriteOwnerOnly(reportPath, []byte(report)); werr != nil {
		return werr
	}
	fmt.Print(report)
	undecided := 0
	for _, gate := range gates {
		if gate.Verdict == goldset.GateUndecided {
			undecided++
		}
	}
	fmt.Printf("\nκ gesamt: %.4f (n=%d) · Gates: %d von %d %q · Bericht: %s\n",
		overall.Kappa, overall.N, undecided, len(gates), goldset.GateUndecided, filepath.Base(reportPath))
	return nil
}
