package goldbench

import (
	"context"
	"slices"
	"syscall"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

// Report ist das Gesamt-Ergebnis eines Benchmark-Laufs.
// Throughput dokumentiert den gemessenen Token-Durchsatz des Laufs.
type Throughput struct {
	WallSeconds        float64 `json:"wall_seconds"`
	PromptTokens       int64   `json:"prompt_tokens"`
	CompletionTokens   int64   `json:"completion_tokens"`
	ReasoningTokens    int64   `json:"reasoning_tokens,omitempty"` // Denk-Anteil der Completion (wo der Server es ausweist)
	PromptTokPerSec    float64 `json:"prompt_tok_per_sec"`
	CompletionTokPerSec float64 `json:"completion_tok_per_sec"`
}

// FailStats summiert die gerissenen Fälle über alle Achsen — getrennt nach
// Ursache: Server-Ablehnung an der Context-Grenze, Output am max_tokens-Budget
// gerissen (finish_reason "length"), echte Transport-Fehler. Context- und
// Transport-Fälle scoren 0; Truncation kann je nach Parser trotzdem scoren.
type FailStats struct {
	ContextErrors    int `json:"context_errors"`
	TruncatedOutputs int `json:"truncated_outputs"`
	TransportErrors  int `json:"transport_errors"`
	ThinkStripped    int `json:"think_stripped,omitempty"` // Fälle mit client-seitig entferntem <think>-Block
}

type Report struct {
	Env             EnvStamp              `json:"env"`
	Axes            map[string]AxisResult `json:"axes"`
	Composite       float64               `json:"composite"`        // ungewichtetes Mittel aller primary_scores
	CompositeGold   *float64              `json:"composite_gold"`   // Mittel der gold-Achsen (≤50 % silver-Cases)
	CompositeSilver *float64              `json:"composite_silver"` // Mittel der silver-Achsen (>50 % silver-Cases)
	Throughput      Throughput            `json:"throughput"`
	FailStats       FailStats             `json:"fail_stats"`
}

// EnvStamp dokumentiert die Lauf-Umgebung (ohne API-Key).
type EnvStamp struct {
	Model         string         `json:"model"`
	Endpoint      string         `json:"endpoint"`
	DatasetSHA256 string         `json:"dataset_sha256"`
	GitRev        string         `json:"git_rev,omitempty"`
	Timestamp     string         `json:"timestamp"`
	Seed          int64          `json:"seed"`
	DryRun        bool           `json:"dry_run,omitempty"`
	MetricVersion int            `json:"metric_version"`
	MaxTokensMult float64        `json:"max_tokens_mult,omitempty"` // >1 = Budget-Abweichung von der Pipeline-Treue
	ExtraBody     string         `json:"extra_body,omitempty"`      // Request-Merge (chat_template_kwargs etc.)
	TempOverride  *float64       `json:"temp_override,omitempty"`   // fixe Temperatur statt Pipeline-Temps (Mock-Treue-Abweichung)
	ServerNote    string         `json:"server_note,omitempty"`
	NPerAxis      map[string]int `json:"n_per_axis"`
}

// job ist eine Arbeitseinheit des Worker-Pools: alle Calls EINES Falls
// (sensitivity: 2 Calls sequenziell — sie teilen den Block, nicht das Budget).
type job struct {
	axis string
	idx  int
	reqs []ChatRequest
}

// Run führt den Benchmark über cfg.Axes aus und liefert den Report.
func Run(ctx context.Context, cfg Config) (*Report, error) {
	registry := axisRegistry()
	axes, err := resolveAxes(cfg.Axes, registry)
	if err != nil {
		return nil, err
	}

	hash, err := DatasetHash(cfg.DataDir, axes)
	if err != nil {
		return nil, err
	}

	// Fälle laden, samplen, Prompts bauen (Build validiert die Daten auch im
	// Dry-Run — ein Fall, dessen Prompt nicht baubar ist, ist ein Datenfehler).
	axisRuns := map[string][]caseRun{}
	jobs := make([]job, 0, 1024)
	nPerAxis := map[string]int{}
	for _, axis := range axes {
		cases, err := LoadCases(cfg.DataDir, axis)
		if err != nil {
			return nil, err
		}
		cases = SampleCases(cases, cfg.N, cfg.Seed)
		nPerAxis[axis] = len(cases)
		runs := make([]caseRun, len(cases))
		for i, c := range cases {
			reqs, err := registry[axis].build(c)
			if err != nil {
				return nil, err
			}
			runs[i] = caseRun{c: c, reqs: reqs, outputs: make([]string, len(reqs))}
			if !cfg.DryRun {
				// Dry-Run schreibt KEINE usage-Slots (omitempty greift nur auf
				// nil-Slice) — ein Leser unterscheidet so Dry-Run von Nullwerten.
				runs[i].usages = make([]CallUsage, len(reqs))
			}
			// Eigene Kopie für den Worker: er schreibt die effektiven Sampling-
			// Werte nach run.reqs zurück; ein zweiter Durchlauf (Retry/Resample)
			// darf nicht auf bereits multiplizierten Budgets aufsetzen.
			jobs = append(jobs, job{axis: axis, idx: i, reqs: slices.Clone(reqs)})
		}
		axisRuns[axis] = runs
	}

	if !cfg.DryRun {
		if err := executeJobs(ctx, cfg, jobs, axisRuns); err != nil {
			return nil, err
		}
	}

	if cfg.DumpOutputs != "" {
		if err := dumpOutputs(cfg.DumpOutputs, axes, axisRuns, cfg.GenStamp); err != nil {
			return nil, err
		}
	}

	// Scoren + aggregieren.
	st := lastRunStats
	tp := Throughput{WallSeconds: st.WallSeconds, PromptTokens: st.PromptTokens, CompletionTokens: st.CompletionTokens, ReasoningTokens: st.ReasoningTokens}
	if st.WallSeconds > 0 {
		tp.PromptTokPerSec = float64(st.PromptTokens) / st.WallSeconds
		tp.CompletionTokPerSec = float64(st.CompletionTokens) / st.WallSeconds
	}
	mult := cfg.MaxTokensMult
	if mult == 1 {
		mult = 0 // omitempty: pipeline-treue Läufe tragen kein Feld
	}
	var tempOv *float64
	if cfg.TempOverride >= 0 {
		tempOv = &cfg.TempOverride
	}
	report := &Report{
		Throughput: tp,
		Env: EnvStamp{
			Model:         cfg.Model,
			Endpoint:      cfg.Endpoint,
			DatasetSHA256: hash,
			GitRev:        cfg.GitRev,
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			Seed:          cfg.Seed,
			DryRun:        cfg.DryRun,
			MetricVersion: 2,
			MaxTokensMult: mult,
			ExtraBody:     cfg.ExtraBody,
			TempOverride:  tempOv,
			ServerNote:    cfg.ServerNote,
			NPerAxis:      nPerAxis,
		},
		Axes: map[string]AxisResult{},
	}
	for _, axis := range axes {
		res := scoreAxisRuns(registry[axis], axisRuns[axis], cfg.Verbose)
		report.Axes[axis] = res
		report.FailStats.ContextErrors += res.ContextErrors
		report.FailStats.TruncatedOutputs += res.TruncatedOutputs
		report.FailStats.TransportErrors += res.TransportErrors
		report.FailStats.ThinkStripped += res.ThinkStripped
	}
	applyComposites(report, axes)
	return report, nil
}

// resolveAxes validiert die Achsen-Auswahl ("all" expandiert der Aufrufer).
func resolveAxes(requested []string, registry map[string]axisDef) ([]string, error) {
	if len(requested) == 0 {
		requested = Axes
	}
	out := make([]string, 0, len(requested))
	for _, a := range requested {
		if _, ok := registry[a]; !ok {
			return nil, fmt.Errorf("goldbench: unbekannte Achse %q (gültig: %v)", a, Axes)
		}
		out = append(out, a)
	}
	sort.Strings(out)
	return out, nil
}

// executeJobs fährt die LLM-Calls nebenläufig über einen Worker-Pool.
// Transport-Fehler landen im caseRun (der Fall scored 0), sie brechen den
// Lauf nicht ab — abgesehen von Context-Cancel.
func executeJobs(ctx context.Context, cfg Config, jobs []job, axisRuns map[string][]caseRun) error {
	client := NewClient(cfg.Endpoint, cfg.Model, cfg.APIKey, cfg.Seed,
		time.Duration(cfg.TimeoutSec)*time.Second)
	if err := client.SetExtraBody(cfg.ExtraBody); err != nil {
		return err
	}
	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}

	total := 0
	for _, j := range jobs {
		total += len(j.reqs)
	}
	var doneReqs, promptToks, complToks, reasonToks atomic.Int64
	start := time.Now()

	jobCh := make(chan job)
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobCh {
				run := &axisRuns[j.axis][j.idx]
				for i, req := range j.reqs {
					if ctx.Err() != nil {
						run.callErr = ctx.Err()
						break
					}
					req.Opts.MaxTokens = applyBudgetMult(req.Opts.MaxTokens, cfg.MaxTokensMult)
					if cfg.TempOverride >= 0 {
						req.Opts.Temperature = cfg.TempOverride
					}
					// Effektive Sampling-Parameter zurückschreiben — der Dump
					// (params) soll das tatsächlich Gesendete tragen.
					run.reqs[i].Opts = req.Opts
					res, err := client.ChatWithUsage(ctx, req)
					n := doneReqs.Add(1)
					promptToks.Add(int64(res.PromptTokens))
					complToks.Add(int64(res.CompletionTokens))
					reasonToks.Add(int64(res.ReasoningTokens))
					// „Test x von y" + laufender Durchsatz auf stderr — sichtbar
					// im Run-Log, ohne den Report zu verschmutzen. Jede Zeile:
					// ~100 KB pro Lauf, dafür sekundenaktuelle Dashboard-Kachel
					// auch in prompt-lastigen Achsen langsamer Modelle.
					el := time.Since(start).Seconds()
					fmt.Fprintf(os.Stderr, "[fortschritt] Test %d von %d (%.1f%%) · in %.0f tok/s · out %.0f tok/s\n",
						n, total, 100*float64(n)/float64(total),
						float64(promptToks.Load())/el, float64(complToks.Load())/el)
					run.usages[i] = CallUsage{
						Prompt: res.PromptTokens, Completion: res.CompletionTokens, Reasoning: res.ReasoningTokens,
						Finish: res.FinishReason, ThinkStripped: res.ThinkStripped,
					}
					if err != nil {
						run.usages[i].Err = err.Error()
						if errors.Is(err, ErrContextOverflow) {
							run.contextErr = true
						}
						if run.callErr == nil {
							run.callErr = err
						}
						continue
					}
					if res.FinishReason == "length" {
						run.truncated++
					}
					if res.ThinkStripped {
						run.thinkStrip++
					}
					run.outputs[i] = res.Content
				}
			}
		}()
	}
	for _, j := range jobs {
		if ctx.Err() != nil {
			break
		}
		jobCh <- j
	}
	close(jobCh)
	wg.Wait()
	// Aggregat für den Report (Paket-Variablen, vom Run gelesen).
	lastRunStats = runStats{
		WallSeconds:      time.Since(start).Seconds(),
		PromptTokens:     promptToks.Load(),
		CompletionTokens: complToks.Load(),
		ReasoningTokens:  reasonToks.Load(),
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("goldbench: run abgebrochen: %w", err)
	}
	return nil
}

// applyBudgetMult skaliert ein per-Achse-max_tokens-Budget mit dem
// konfigurierten Multiplikator (0/1 = unverändert; MaxTokens 0 = Server-
// Default bleibt unangetastet). Aufgerundet, damit ×1.5 auf 32 nicht
// abrundet und kleine Budgets real wachsen.
func applyBudgetMult(maxTokens int, mult float64) int {
	if mult <= 0 || mult == 1 || maxTokens <= 0 {
		return maxTokens
	}
	scaled := int(float64(maxTokens)*mult + 0.999999)
	if scaled < maxTokens {
		return maxTokens
	}
	return scaled
}

// runStats trägt die Durchsatz-Aggregation des letzten executeJobs-Laufs.
// Bewusst paketweit (ein Run pro Prozess — der Harness ist ein CLI-Tool).
type runStats struct {
	WallSeconds      float64
	PromptTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
}

var lastRunStats runStats

// scoreAxisRuns ruft den Achsen-Scorer und ergänzt die generischen Felder
// (silver_share, label_quality, transport_errors, per_case bei verbose).
func scoreAxisRuns(def axisDef, runs []caseRun, verbose bool) AxisResult {
	res, perCase := def.score(runs)
	res.Prospective = def.prospective

	// Metrik v2: 95%-Bootstrap-CI über die per-Case-Scores. Bei Achsen mit
	// micro-aggregierter Primärmetrik (temporal-*) ist das CI eine
	// Unsicherheits-Näherung über Fälle, kein exaktes Metrik-CI — dokumentiert.
	scores := make([]float64, 0, len(perCase))
	for _, pc := range perCase {
		scores = append(scores, pc.Score)
	}
	var ciSeed int64
	for _, ch := range def.name {
		ciSeed = ciSeed*31 + int64(ch)
	}
	res.CI95Low, res.CI95High = bootstrapCI(scores, 20260812+ciSeed)

	silver, transportErrs, contextErrs, truncated, thinkStripped := 0, 0, 0, 0, 0
	for _, r := range runs {
		if r.c.LabelQuality == "silver" {
			silver++
		}
		if r.thinkStrip > 0 {
			thinkStripped++
		}
		// Fail-Metrik: Context-Ablehnungen getrennt von echten Transport-Fehlern
		// zählen — beide reißen den Fall (Score 0), aber nur einer ist ein
		// Serving-Limit. truncated = Output am max_tokens-Budget gerissen.
		switch {
		case r.contextErr:
			contextErrs++
		case r.callErr != nil:
			transportErrs++
		}
		if r.truncated > 0 {
			truncated++
		}
	}
	res.SilverShare = ratioOrZero(silver, len(runs))
	res.LabelQuality = "gold"
	if res.SilverShare > 0.5 {
		res.LabelQuality = "silver"
	}
	res.TransportErrors = transportErrs
	res.ContextErrors = contextErrs
	res.TruncatedOutputs = truncated
	res.ThinkStripped = thinkStripped
	if verbose {
		res.PerCase = perCase
	}
	return res
}

// applyComposites rechnet das ungewichtete Mittel der primary_scores —
// gesamt sowie getrennt nach gold-/silver-Achsen.
func applyComposites(report *Report, axes []string) {
	var all, gold, silver []float64
	for _, a := range axes {
		res := report.Axes[a]
		all = append(all, res.PrimaryScore)
		if res.LabelQuality == "silver" {
			silver = append(silver, res.PrimaryScore)
		} else {
			gold = append(gold, res.PrimaryScore)
		}
	}
	report.Composite = meanOrZero(all)
	if len(gold) > 0 {
		v := meanOrZero(gold)
		report.CompositeGold = &v
	}
	if len(silver) > 0 {
		v := meanOrZero(silver)
		report.CompositeSilver = &v
	}
}

// dumpRecord ist die Dump-Zeile (v2, design/02 §3.3): Bestandsleser lesen
// weiterhin nur axis/id/outputs (outputs bleibt flach, Sample 0 je Request);
// system/user/params/usage sind parallel zu outputs indiziert (ein Slot pro
// ChatRequest — sensitivity hat zwei), gen ist der Engine-Stempel. Alle neuen
// Felder omitempty — ohne Requests/Stempel ist die Zeile byte-gleich zu v1.
type dumpRecord struct {
	Axis    string         `json:"axis"`
	ID      string         `json:"id"`
	System  []string       `json:"system,omitempty"`
	User    []string       `json:"user,omitempty"`
	Params  []SamplingOpts `json:"params,omitempty"`
	Outputs []string       `json:"outputs"`
	Usage   []CallUsage    `json:"usage,omitempty"`
	Gen     *GenStamp      `json:"gen,omitempty"`
}

// dumpOutputs persistiert die rohen Modell-Antworten aller gefahrenen Achsen
// als JSONL — eine Zeile pro Case (dumpRecord). Outputs sind der Content
// NACH dem client-seitigen <think>-Strip (client.go stripThink) und VOR jedem
// Scorer-Parse: das Dump ist die Primärquelle für offline-Re-Scoring (Judge,
// Retrieval); ob gestrippt wurde, steht je Slot in usage.think_stripped.
// Seit v2 trägt die Datei die Volltext-Prompts (privater Block-Content) —
// deshalb O_CREATE|0600 statt umask-Default.
func dumpOutputs(path string, axes []string, axisRuns map[string][]caseRun, gen *GenStamp) error {
	// O_NOFOLLOW: nie einem Symlink am Dump-Pfad folgen; Chmod nach dem Open:
	// der Modus im OpenFile gilt nur bei Neuanlage — eine vorhandene 0644-Datei
	// (Bestands-Dumps aus regen.sh) bliebe sonst welt-lesbar mit Volltext-Prompts.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("goldbench: dump-outputs: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("goldbench: dump-outputs: chmod: %w", err)
	}
	enc := json.NewEncoder(f)
	for _, axis := range axes {
		for _, r := range axisRuns[axis] {
			if err := enc.Encode(newDumpRecord(axis, r, gen)); err != nil {
				_ = f.Close()
				return fmt.Errorf("goldbench: dump-outputs: %w", err)
			}
		}
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("goldbench: dump-outputs: %w", err)
	}
	return f.Close()
}

func newDumpRecord(axis string, r caseRun, gen *GenStamp) dumpRecord {
	rec := dumpRecord{Axis: axis, ID: r.c.ID, Outputs: r.outputs, Gen: gen}
	if len(r.reqs) > 0 {
		rec.System = make([]string, len(r.reqs))
		rec.User = make([]string, len(r.reqs))
		rec.Params = make([]SamplingOpts, len(r.reqs))
		for i, q := range r.reqs {
			rec.System[i], rec.User[i], rec.Params[i] = q.System, q.User, q.Opts
		}
	}
	if len(r.usages) > 0 {
		rec.Usage = r.usages
	}
	return rec
}
