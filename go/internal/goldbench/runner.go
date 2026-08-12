package goldbench

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Report ist das Gesamt-Ergebnis eines Benchmark-Laufs.
type Report struct {
	Env             EnvStamp              `json:"env"`
	Axes            map[string]AxisResult `json:"axes"`
	Composite       float64               `json:"composite"`        // ungewichtetes Mittel aller primary_scores
	CompositeGold   *float64              `json:"composite_gold"`   // Mittel der gold-Achsen (≤50 % silver-Cases)
	CompositeSilver *float64              `json:"composite_silver"` // Mittel der silver-Achsen (>50 % silver-Cases)
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
			runs[i] = caseRun{c: c, outputs: make([]string, len(reqs))}
			jobs = append(jobs, job{axis: axis, idx: i, reqs: reqs})
		}
		axisRuns[axis] = runs
	}

	if !cfg.DryRun {
		if err := executeJobs(ctx, cfg, jobs, axisRuns); err != nil {
			return nil, err
		}
	}

	// Scoren + aggregieren.
	report := &Report{
		Env: EnvStamp{
			Model:         cfg.Model,
			Endpoint:      cfg.Endpoint,
			DatasetSHA256: hash,
			GitRev:        cfg.GitRev,
			Timestamp:     time.Now().UTC().Format(time.RFC3339),
			Seed:          cfg.Seed,
			DryRun:        cfg.DryRun,
			NPerAxis:      nPerAxis,
		},
		Axes: map[string]AxisResult{},
	}
	for _, axis := range axes {
		res := scoreAxisRuns(registry[axis], axisRuns[axis], cfg.Verbose)
		report.Axes[axis] = res
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
	conc := cfg.Concurrency
	if conc < 1 {
		conc = 1
	}

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
					out, err := client.Chat(ctx, req)
					if err != nil {
						if run.callErr == nil {
							run.callErr = err
						}
						continue
					}
					run.outputs[i] = out
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
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("goldbench: run abgebrochen: %w", err)
	}
	return nil
}

// scoreAxisRuns ruft den Achsen-Scorer und ergänzt die generischen Felder
// (silver_share, label_quality, transport_errors, per_case bei verbose).
func scoreAxisRuns(def axisDef, runs []caseRun, verbose bool) AxisResult {
	res, perCase := def.score(runs)
	res.Prospective = def.prospective

	silver, transportErrs := 0, 0
	for _, r := range runs {
		if r.c.LabelQuality == "silver" {
			silver++
		}
		if r.callErr != nil {
			transportErrs++
		}
	}
	res.SilverShare = ratioOrZero(silver, len(runs))
	res.LabelQuality = "gold"
	if res.SilverShare > 0.5 {
		res.LabelQuality = "silver"
	}
	res.TransportErrors = transportErrs
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
