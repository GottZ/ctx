package goldbench

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Report ist das Gesamt-Ergebnis eines Benchmark-Laufs.
// Throughput dokumentiert den gemessenen Token-Durchsatz des Laufs.
type Throughput struct {
	WallSeconds         float64 `json:"wall_seconds"`
	PromptTokens        int64   `json:"prompt_tokens"`
	CompletionTokens    int64   `json:"completion_tokens"`
	ReasoningTokens     int64   `json:"reasoning_tokens,omitempty"` // Denk-Anteil der Completion (wo der Server es ausweist)
	PromptTokPerSec     float64 `json:"prompt_tok_per_sec"`
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
	// Concurrency stempelt das Lauf-Regime (τ ist regime-abhängig: c1-
	// Diagnose ≠ c4-Promote-Report); omitempty hält Bestands-Reports stabil.
	Concurrency int `json:"concurrency,omitempty"`
	// Spec ist die strukturierte Spec-Provenienz (-spec-config); nil bei
	// Läufen ohne Flag (byte-stabil zum v4-Protokoll).
	Spec *SpecConfig `json:"spec,omitempty"`
	// ResumedCases/ExecutedCases (KW3): Fälle aus dem Append-Dump übernommen
	// vs. in diesem Prozess gecallt. Throughput misst NUR die gecallten —
	// ein Resume-Report ist daran als solcher erkennbar (omitempty: Läufe ohne
	// -dump-append bleiben byte-stabil).
	ResumedCases  int `json:"resumed_cases,omitempty"`
	ExecutedCases int `json:"executed_cases,omitempty"`
}

// stampConcurrency liefert das Lauf-Regime für den EnvStamp: 0 im Dry-Run
// (kein Regime, Feld bleibt weg — Bestands-Dry-Run-Reports byte-stabil),
// sonst die effektive Worker-Zahl wie in executeJobs.
func stampConcurrency(cfg Config) int {
	if cfg.DryRun {
		return 0
	}
	if cfg.Concurrency <= 0 {
		return 1
	}
	return cfg.Concurrency
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

	// KW3: bestehenden Append-Dump lesen — Done-Set (axis,id) + Stamp-Gate.
	var done *dumpDone
	if cfg.DumpAppend {
		if cfg.DumpOutputs == "" {
			return nil, errors.New("goldbench: dump-append braucht dump-outputs")
		}
		if cfg.GenStamp == nil {
			return nil, errors.New("goldbench: dump-append braucht einen gen-Stempel (Stamp-Resume-Gate)")
		}
		if done, err = loadDumpDone(cfg.DumpOutputs, cfg.GenStamp); err != nil {
			return nil, err
		}
	}

	axisRuns, jobs, nPerAxis, resumed, err := buildRuns(cfg, axes, registry, done)
	if err != nil {
		return nil, err
	}

	if cfg.DumpOutputs != "" {
		// Preflight VOR dem Serving-Lauf (Review F7): Symlink am Pfad, fremder
		// Eigentümer (Chmod EPERM) oder fehlendes Verzeichnis sollen sofort
		// scheitern — nicht erst nach Stunden GPU-Zeit, wenn dumpOutputs die
		// Datei öffnet (und per O_TRUNC den Altbestand bereits geleert hätte).
		if err := preflightDumpPath(cfg.DumpOutputs); err != nil {
			return nil, err
		}
	}

	// KW3: Append-Writer VOR dem Lauf öffnen (jeder fertige Fall wird sofort
	// geschrieben); geschlossen auf JEDEM Rückweg, auch nach Abbruch.
	var sink *dumpAppender
	if cfg.DumpAppend && !cfg.DryRun {
		var err error
		if sink, err = openDumpAppender(cfg.DumpOutputs, done.validLen); err != nil {
			return nil, err
		}
		defer func() { _ = sink.Close() }()
		outside := done.total - resumed
		fmt.Fprintf(os.Stderr, "[resume] %s: %d Fälle übernommen, %d offen, %d Datei-Zeilen außerhalb der Stichprobe/Achsen\n",
			cfg.DumpOutputs, resumed, len(jobs), outside)
	}

	if !cfg.DryRun {
		if err := executeJobs(ctx, cfg, jobs, axisRuns, sink); err != nil {
			return nil, err
		}
	}

	switch {
	case sink != nil:
		if err := sink.Close(); err != nil {
			return nil, err
		}
	case cfg.DumpAppend:
		// Dry-Run mit -dump-append: NUR lesen (Stamp-Gate, Done-Set) — der
		// End-of-Run-Writer würde die Append-Datei per O_TRUNC vernichten.
	case cfg.DumpOutputs != "":
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
			Concurrency:   stampConcurrency(cfg),
			Spec:          cfg.SpecConfig,
			ResumedCases:  resumed,
			ExecutedCases: executedCases(cfg, resumed, len(jobs)),
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

// buildRuns lädt, sampelt und baut die Fälle aller Achsen (Build validiert die
// Daten auch im Dry-Run — ein Fall, dessen Prompt nicht baubar ist, ist ein
// Datenfehler). Fälle aus dem Done-Set (KW3-Resume) werden übernommen statt
// gecallt; Rückgabe: Runs je Achse, Job-Liste, n je Achse, Anzahl übernommen.
func buildRuns(cfg Config, axes []string, registry map[string]axisDef, done *dumpDone) (
	map[string][]caseRun, []job, map[string]int, int, error,
) {
	axisRuns := map[string][]caseRun{}
	jobs := make([]job, 0, 1024)
	nPerAxis := map[string]int{}
	resumed := 0
	for _, axis := range axes {
		cases, err := LoadCases(cfg.DataDir, axis)
		if err != nil {
			return nil, nil, nil, 0, err
		}
		cases = SampleCases(cases, cfg.N, cfg.Seed)
		nPerAxis[axis] = len(cases)
		runs := make([]caseRun, len(cases))
		for i, c := range cases {
			reqs, err := registry[axis].build(c)
			if err != nil {
				return nil, nil, nil, 0, err
			}
			runs[i] = caseRun{c: c, reqs: reqs, outputs: make([]string, len(reqs))}
			if rec, ok := done.lookup(axis, c.ID); ok && !cfg.DryRun {
				// Resume: Fall liegt im Dump — Output/usage übernehmen, kein
				// Call, kein erneutes Schreiben (Skip VOR dem Job-Bau).
				if len(rec.Outputs) != len(reqs) {
					return nil, nil, nil, 0, fmt.Errorf("goldbench: dump-append: %s/%s: %d Output-Slots in der Datei, %d Requests gebaut — Dump passt nicht zur Achsen-Definition", axis, c.ID, len(rec.Outputs), len(reqs))
				}
				runs[i].adopt(rec)
				resumed++
				continue
			}
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
	return axisRuns, jobs, nPerAxis, resumed, nil
}

// executeJobs fährt die LLM-Calls nebenläufig über einen Worker-Pool.
// Transport-Fehler landen im caseRun (der Fall scored 0), sie brechen den
// Lauf nicht ab — abgesehen von Context-Cancel.
func executeJobs(parent context.Context, cfg Config, jobs []job, axisRuns map[string][]caseRun, sink *dumpAppender) error {
	// Eigener Cancel-Scope: ein Schreibfehler des Append-Sinks (ENOSPC, EIO)
	// stoppt den Lauf sofort — weiter gegen die GPU zu fahren, ohne zu
	// persistieren, wäre verbrannte Zeit (Review KW3 F5).
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	var sinkErr error
	var sinkErrOnce sync.Once
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
				// KW3: fertigen Fall sofort persistieren — nur VOLLSTÄNDIGE
				// Fälle (kein Abbruch, kein Transport-Fehler): so holt der
				// Resume Fehlschläge nach, statt sie als erledigt zu überspringen.
				// Der Fall selbst ist vollständig (kein Transport-Fehler) — ein
				// Persistenz-Fehler ist eine eigene Klasse: Lauf abbrechen, der
				// Fall wird beim Resume nachgeholt (er steht nicht in der Datei).
				if sink != nil && run.callErr == nil {
					if err := sink.write(newDumpRecord(j.axis, *run, cfg.GenStamp)); err != nil {
						sinkErrOnce.Do(func() {
							sinkErr = fmt.Errorf("%w: %s/%s: %w", ErrDumpWrite, j.axis, run.c.ID, err)
							cancel()
						})
					}
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
	if sinkErr != nil {
		return sinkErr
	}
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("goldbench: run abgebrochen: %w", err)
	}
	return nil
}

// executedCases liefert die in diesem Prozess gecallten Fälle (0 im Dry-Run).
func executedCases(cfg Config, resumed, jobs int) int {
	if cfg.DryRun || !cfg.DumpAppend {
		return 0
	}
	_ = resumed
	return jobs
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

// ErrDumpStamp: der gen-Stempel der bestehenden Append-Datei passt nicht zum
// Live-Stempel — ein resumed Dump darf nie zwei Engine-/Quant-/Template-
// Verteilungen stumm mischen (design/02 §4.3 Stamp-Resume-Gate).
var ErrDumpStamp = errors.New("goldbench: dump-append: gen-Stempel der Datei weicht vom Live-Stempel ab — Abbruch statt Append")

// ErrDumpWrite: der Append-Sink konnte einen fertigen Fall nicht persistieren
// (ENOSPC/EIO) — der Lauf bricht ab, der Fall wird beim Resume nachgeholt.
var ErrDumpWrite = errors.New("goldbench: dump-append: Schreibfehler — Lauf abgebrochen")

// ErrDumpLocked: die Append-Datei ist von einem anderen Prozess gesperrt
// (flock) — zwei Treiber auf einer Datei schrieben Duplikate.
var ErrDumpLocked = errors.New("goldbench: dump-append: Datei von einem anderen Prozess gesperrt (flock)")

// doneRec ist der übernommene Anteil eines gedumpten Falls (nur was adopt
// braucht — die Volltext-Prompts bleiben auf der Platte, Review KW3 F9).
type doneRec struct {
	Outputs []string
	Usage   []CallUsage
}

// dumpDone ist das Done-Set einer Append-Datei.
type dumpDone struct {
	recs     map[string]map[string]doneRec // axis → id → letzter VOLLSTÄNDIGER Record
	total    int                           // vollständige Records in der Datei
	validLen int64                         // Byte-Länge bis zur letzten vollständigen Zeile (Torn-Tail-Toleranz)
}

func (d *dumpDone) lookup(axis, id string) (doneRec, bool) {
	if d == nil {
		return doneRec{}, false
	}
	r, ok := d.recs[axis][id]
	return r, ok
}

// recComplete: ein Record zählt nur als erledigt, wenn jeder Slot einen Output
// trägt und keinen Fehler — Legacy-/Fremd-Dumps (End-of-Run-Writer) enthalten
// auch gescheiterte Fälle; die dürfen beim Resume nicht als erledigt einfrieren
// (Review KW3 F2). Ein zweiter, vollständiger Record derselben (axis,id) nach
// einem gescheiterten ist deshalb legitim und gewinnt; zwei VOLLSTÄNDIGE sind ein
// Duplikat (Datenfehler, fail-closed).
func recComplete(rec dumpRecord) bool {
	if len(rec.Outputs) == 0 {
		return false
	}
	for _, o := range rec.Outputs {
		if o == "" {
			return false
		}
	}
	for _, u := range rec.Usage {
		if u.Err != "" {
			return false
		}
	}
	return true
}

// loadDumpDone liest eine bestehende Append-Datei und liefert das Done-Set.
// Stamp-Gate: JEDE Zeile muss denselben gen-Stempel tragen wie der Live-Lauf.
// Fehlende Datei = leeres Set. Eine Zeile ohne axis/id ist ein Datenfehler.
// Torn-Tail: genau EINE unvollständige letzte Zeile ohne abschließendes
// Newline (Abbruch mitten im Write) wird toleriert — validLen zeigt auf das
// Ende der letzten vollständigen Zeile, der Appender schneidet dort ab
// (Review KW3 F4); jeder andere Parse-Fehler bleibt fail-closed.
func loadDumpDone(path string, gen *GenStamp) (*dumpDone, error) {
	d := &dumpDone{recs: map[string]map[string]doneRec{}}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return d, nil
	}
	if err != nil {
		return nil, fmt.Errorf("goldbench: dump-append: %w", err)
	}
	defer func() { _ = f.Close() }()
	rd := bufio.NewReaderSize(f, 1<<20)
	var off int64
	n := 0
	for {
		line, rerr := rd.ReadBytes('\n')
		if len(line) == 0 && rerr != nil {
			break
		}
		n++
		complete := len(line) > 0 && line[len(line)-1] == '\n'
		trimmed := strings.TrimSpace(string(line))
		if trimmed == "" {
			off += int64(len(line))
			if rerr != nil {
				break
			}
			continue
		}
		var rec dumpRecord
		if uerr := json.Unmarshal([]byte(trimmed), &rec); uerr != nil {
			if !complete && rerr != nil {
				// abgerissene Schlusszeile: tolerieren, Länge NICHT vorrücken
				fmt.Fprintf(os.Stderr, "[dump-append] %s: abgerissene letzte Zeile %d (%d Bytes) wird verworfen und überschrieben\n", path, n, len(line))
				break
			}
			return nil, fmt.Errorf("goldbench: dump-append: Zeile %d: %w", n, uerr)
		}
		if rec.Axis == "" || rec.ID == "" {
			return nil, fmt.Errorf("goldbench: dump-append: Zeile %d ohne axis/id", n)
		}
		if !sameGenStamp(rec.Gen, gen) {
			return nil, fmt.Errorf("%w (Zeile %d: Datei %s, Lauf %s)", ErrDumpStamp, n, genString(rec.Gen), genString(gen))
		}
		if recComplete(rec) {
			if d.recs[rec.Axis] == nil {
				d.recs[rec.Axis] = map[string]doneRec{}
			}
			if _, dup := d.recs[rec.Axis][rec.ID]; dup {
				return nil, fmt.Errorf("goldbench: dump-append: Zeile %d: Duplikat (%s,%s) — zwei vollständige Records", n, rec.Axis, rec.ID)
			}
			d.recs[rec.Axis][rec.ID] = doneRec{Outputs: rec.Outputs, Usage: rec.Usage}
			d.total++
		}
		off += int64(len(line))
		if rerr != nil {
			break
		}
	}
	d.validLen = off
	return d, nil
}

func sameGenStamp(a, b *GenStamp) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func genString(g *GenStamp) string {
	if g == nil {
		return "<kein Stempel>"
	}
	b, _ := json.Marshal(g)
	return string(b)
}

// adopt übernimmt einen gedumpten Fall in den caseRun (Resume): Outputs und
// usage-Slots wie persistiert, Zähler aus den Slots rekonstruiert.
func (r *caseRun) adopt(rec doneRec) {
	copy(r.outputs, rec.Outputs)
	r.usages = append([]CallUsage(nil), rec.Usage...)
	for _, u := range r.usages {
		if u.Finish == "length" {
			r.truncated++
		}
		if u.ThinkStripped {
			r.thinkStrip++
		}
	}
}

// dumpAppender ist der inkrementelle Dump-Writer (KW3): eine JSONL-Zeile pro
// fertigem Fall, mutex-serialisiert (alle Worker dieses Prozesses; genau ein
// Prozess je Datei — die Treiber skippen auf Datei-Ebene), O_APPEND.
type dumpAppender struct {
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
	buf  *bufio.Writer
	n    int
	path string
}

// openDumpAppender öffnet die Append-Datei exklusiv (flock LOCK_EX|LOCK_NB —
// ein zweiter Treiber auf derselben Datei bekommt ErrDumpLocked statt
// Duplikate zu schreiben, Review KW3 F7) und schneidet eine abgerissene
// Schlusszeile ab (validLen aus loadDumpDone; 0 = neue/leere Datei).
func openDumpAppender(path string, validLen int64) (*dumpAppender, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, fmt.Errorf("goldbench: dump-append: %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("goldbench: dump-append: chmod: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w: %s: %w", ErrDumpLocked, path, err)
	}
	if st, err := f.Stat(); err == nil && validLen >= 0 && st.Size() > validLen {
		if err := f.Truncate(validLen); err != nil {
			_ = f.Close()
			return nil, fmt.Errorf("goldbench: dump-append: truncate torn tail: %w", err)
		}
	}
	a := &dumpAppender{f: f, path: path}
	a.buf = bufio.NewWriterSize(f, 1<<20)
	a.enc = json.NewEncoder(a.buf)
	return a, nil
}

// write persistiert einen Record sofort (Encode + Flush unter Mutex): nach
// der Rückkehr steht die Zeile im Page-Cache — kill -9 verliert sie nicht,
// nur ein Stromausfall (fsync erst in Close; das ist der bewusste Preis).
func (a *dumpAppender) write(rec dumpRecord) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.enc.Encode(rec); err != nil {
		return err
	}
	if err := a.buf.Flush(); err != nil {
		return err
	}
	a.n++
	return nil
}

// Close flusht, fsynct und schließt; idempotent (zweiter Aufruf = nil).
func (a *dumpAppender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.f == nil {
		return nil
	}
	f := a.f
	a.f = nil
	if err := a.buf.Flush(); err != nil {
		_ = f.Close()
		return fmt.Errorf("goldbench: dump-append: %w", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return fmt.Errorf("goldbench: dump-append: fsync: %w", err)
	}
	fmt.Fprintf(os.Stderr, "[dump-append] %s: %d Fälle geschrieben, fsync ok\n", a.path, a.n)
	return f.Close() // gibt auch das flock frei
}

// preflightDumpPath öffnet den Dump-Pfad wie dumpOutputs (O_NOFOLLOW, 0600,
// Chmod) — aber OHNE O_TRUNC: ein vorhandener Dump bleibt unangetastet, die
// Fehlerklassen (Symlink, EPERM, ENOENT des Verzeichnisses) zeigen sich vor
// dem Lauf. Eine neu angelegte leere Datei ist der dokumentierte Zustand
// „Lauf begonnen, kein Dump" (design/02 §4.3 ROT-Probe b).
func preflightDumpPath(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return fmt.Errorf("goldbench: dump-outputs (preflight): %w", err)
	}
	if err := f.Chmod(0o600); err != nil {
		_ = f.Close()
		return fmt.Errorf("goldbench: dump-outputs (preflight): chmod: %w", err)
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
