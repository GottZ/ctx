package armsweep

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
	"github.com/GottZ/ctx/internal/rrf"
)

// DefaultRetries is the per-query retry budget of §4.7: two retries, then the
// case is EXCLUDED and listed. Never replaced by a substitute — a substituted
// case silently changes the population a report is computed over.
const DefaultRetries = 2

// Runner carries everything both stages share. It owns no state beyond its
// configuration; a stage returns its artefacts and the caller writes them,
// which is what makes both stages testable against an httptest server without
// a filesystem.
type Runner struct {
	Client *Client
	// GoldDir is the guard over the gold directory (slices, pins, prime
	// stamps); DumpDir is the guard over its dumps/ subdirectory. Two guards
	// rather than one so a dump can never be written next to the slice files.
	GoldDir *goldset.Guard
	DumpDir *goldset.Guard

	RunID       string
	Concurrency int
	Retries     int
	Limit       *int
	DryRun      bool
	// Logf is the progress sink. It NEVER receives a query text: a sweep log on
	// a shared host would otherwise carry the private corpus' queries.
	Logf func(format string, args ...any)
}

func (r *Runner) logf(format string, args ...any) {
	if r.Logf != nil {
		r.Logf(format, args...)
	}
}

func (r *Runner) retries() int {
	if r.Retries < 0 {
		return 0
	}
	return r.Retries
}

func (r *Runner) concurrency() int {
	if r.Concurrency < 1 {
		return 1
	}
	return r.Concurrency
}

// outcome is one worker's result for one case.
type outcome struct {
	rec      Record
	pin      Pin
	pool     PoolEntry
	excluded *ExcludedCase
	fatal    error
}

// NewRunID builds a run identifier from a UTC instant. Sortable, filename-safe
// and readable — the three properties a directory of dumps needs.
func NewRunID(now time.Time) string { return now.UTC().Format("20060102T150405Z") }

// LoadCases reads the gold slices named in names ("G-KI", "G-Q", "G-REAL") in
// the canonical slice order, so a run over all three always visits them in the
// same sequence.
func LoadCases(g *goldset.Guard, names []string) ([]goldset.Case, error) {
	files := map[string]string{
		goldset.SliceKI:   goldset.FileKI,
		goldset.SliceQ:    goldset.FileQ,
		goldset.SliceReal: goldset.FileReal,
	}
	var out []goldset.Case
	for _, n := range CanonicalSlices() {
		if !contains(names, n) {
			continue
		}
		p, err := g.Resolve(files[n])
		if err != nil {
			return nil, err
		}
		cases, err := goldset.ReadJSONL(p)
		if err != nil {
			return nil, fmt.Errorf("load slice %s: %w", n, err)
		}
		out = append(out, cases...)
	}
	return out, nil
}

// CanonicalSlices is the fixed slice order of every artefact and report.
func CanonicalSlices() []string {
	return []string{goldset.SliceKI, goldset.SliceQ, goldset.SliceReal}
}

func contains(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// Prime runs every case ONCE without pins (§4.6): it captures the translation
// and temporal results as pins, warms the embed cache, and — for the unlabelled
// G-REAL slice — collects the per-arm pooling candidates wave B-W6 judges.
//
// Nothing is scored here. The run exists so the two dumps that ARE scored can
// be identical in everything except the corpus underneath them.
func (r *Runner) Prime(ctx context.Context, cases []goldset.Case) ([]Pin, []PoolEntry, PrimeStamp, error) {
	res, err := r.sweep(ctx, cases, nil)
	if err != nil {
		return nil, nil, PrimeStamp{}, err
	}
	stamp := PrimeStamp{
		RunID:     r.RunID,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
		BaseURL:   r.Client.BaseURL,
		Slices:    sliceNamesOf(cases),
		Pins:      len(res.pins),
		Excluded:  res.excluded,
		Latency:   SummariseLatency(res.latencies),
	}
	for _, p := range res.pins {
		if p.EmbedModel != "" {
			stamp.EmbedModel = p.EmbedModel
			break
		}
	}
	for _, rec := range res.records {
		if !rec.EmbedCacheHit {
			stamp.EmbedWarmed++
		}
		if rec.EffectiveTemporal != "" {
			stamp.TemporalHits++
		}
	}
	return res.pins, res.pools, stamp, nil
}

// DumpStamp is the provenance of one measurement run.
type DumpStamp struct {
	RunID      string        `json:"run_id"`
	CreatedAt  string        `json:"created_at"`
	BaseURL    string        `json:"base_url"`
	Slices     []string      `json:"slices"`
	Records    int           `json:"records"`
	DumpFile   string        `json:"dump_file"`
	PinFile    string        `json:"pin_file"`
	PinRunID   string        `json:"pin_run_id"`
	PinSHA256  string        `json:"pin_sha256"`
	GoldStamp  string        `json:"gold_stamp_sha256"`
	SliceFiles []SliceDigest `json:"slice_files"`

	// MigrationsMax and PostFusionStages are properties of the INSTANCE at
	// measurement time, captured here rather than at score time. The score step
	// is offline by construction (gate (c): two runs over one dump must produce
	// the same bytes), so anything it needs about the instance has to have been
	// written down while the instance was being measured.
	MigrationsMax    int            `json:"migrations_max"`
	PostFusionStages map[string]any `json:"post_fusion_stages"`
	// AllowOutsideGoldset records the path-guard override for the report.
	AllowOutsideGoldset bool `json:"allow_outside_goldset"`

	Before DriftStamp   `json:"drift_before"`
	After  DriftStamp   `json:"drift_after"`
	Drift  DriftVerdict `json:"drift_verdict"`
	// Aborted mirrors Drift.Abort, denormalised so a reader of the stamp does
	// not have to know which nested field carries the verdict.
	Aborted  bool           `json:"aborted"`
	Excluded []ExcludedCase `json:"excluded"`
	Latency  Latency        `json:"latency"`
	// TemporalPerSlice is the count of cases that received a temporal FTS
	// expansion, per slice (§4.8).
	TemporalPerSlice map[string]int `json:"temporal_per_slice"`
	CasesPerSlice    map[string]int `json:"cases_per_slice"`
}

// SliceDigest binds a stamp to the exact slice bytes it was measured over.
type SliceDigest struct {
	Slice  string `json:"slice"`
	File   string `json:"file"`
	SHA256 string `json:"sha256"`
	N      int    `json:"n"`
}

// Dump runs every case WITH its pin and returns the records plus the stamp.
//
// A missing pin is a hard error, never a silent unpinned fallback: a run where
// some cases are pinned and others are not is neither a pinned nor an unpinned
// measurement, and the difference would be invisible in the artefact.
func (r *Runner) Dump(ctx context.Context, cases []goldset.Case, pins map[string]Pin, goldIDs []string, corpusMaxCreatedAt string) ([]Record, DumpStamp, error) {
	for _, c := range cases {
		if _, ok := pins[CaseKey(c.Slice, c.Index, c.QuerySHA256)]; !ok {
			return nil, DumpStamp{}, fmt.Errorf("no pin for case %s/%d/%s — run `prime` first (a partly pinned dump is not a measurement)",
				c.Slice, c.Index, ShortSHA(c.QuerySHA256))
		}
	}

	before, err := r.census(ctx, goldIDs)
	if err != nil {
		return nil, DumpStamp{}, fmt.Errorf("drift census (before): %w", err)
	}
	res, err := r.sweep(ctx, cases, pins)
	if err != nil {
		return nil, DumpStamp{}, err
	}
	after, err := r.census(ctx, goldIDs)
	if err != nil {
		return nil, DumpStamp{}, fmt.Errorf("drift census (after): %w", err)
	}

	stamp := DumpStamp{
		RunID:            r.RunID,
		CreatedAt:        time.Now().UTC().Format(time.RFC3339),
		BaseURL:          r.Client.BaseURL,
		Slices:           sliceNamesOf(cases),
		Records:          len(res.records),
		Before:           before,
		After:            after,
		Excluded:         res.excluded,
		Latency:          SummariseLatency(res.latencies),
		TemporalPerSlice: map[string]int{},
		CasesPerSlice:    map[string]int{},
	}
	// A dry run never touched the instance, so there is no census to evaluate.
	// Running the rules over two empty stamps would abort every dry run on
	// "retrievable block count was 0" — a true statement about a measurement
	// that did not happen, and useless as a verdict.
	if r.DryRun {
		stamp.Drift = DriftVerdict{Notes: []string{"dry run — no drift census was taken"}}
	} else {
		stamp.Drift = EvaluateDrift(before, after, corpusMaxCreatedAt)
	}
	stamp.Aborted = stamp.Drift.Abort
	for _, rec := range res.records {
		stamp.CasesPerSlice[rec.Slice]++
		if rec.EffectiveTemporal != "" {
			stamp.TemporalPerSlice[rec.Slice]++
		}
	}
	return res.records, stamp, nil
}

// census takes one drift stamp; a dry run reports an empty census rather than
// touching the instance.
func (r *Runner) census(ctx context.Context, goldIDs []string) (DriftStamp, error) {
	if r.DryRun {
		return DriftStamp{At: time.Now().UTC().Format(time.RFC3339)}, nil
	}
	return r.Client.Drift(ctx, goldIDs)
}

// sweepResult aggregates one pass over the case set.
type sweepResult struct {
	records   []Record
	pins      []Pin
	pools     []PoolEntry
	excluded  []ExcludedCase
	latencies []int64
}

// sweep runs the case set with the configured concurrency and retry budget.
//
// Concurrency defaults to 1 and should stay there for a real campaign: the
// measurement transaction is RepeatableRead/ReadOnly and holds a snapshot, so
// parallel measurement requests pile snapshots onto a production instance
// during the very window the run is trying not to disturb.
//
// Results are collected POSITIONALLY and assembled in case order, so the output
// does not depend on which worker finished first.
func (r *Runner) sweep(ctx context.Context, cases []goldset.Case, pins map[string]Pin) (sweepResult, error) {
	out := make([]outcome, len(cases))
	sem := make(chan struct{}, r.concurrency())
	var wg sync.WaitGroup

	for i := range cases {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer wg.Done()
			defer func() { <-sem }()
			out[i] = r.one(ctx, cases[i], pins)
		}(i)
	}
	wg.Wait()

	var res sweepResult
	for i := range out {
		switch {
		case out[i].fatal != nil:
			return sweepResult{}, out[i].fatal
		case out[i].excluded != nil:
			res.excluded = append(res.excluded, *out[i].excluded)
			continue
		}
		res.records = append(res.records, out[i].rec)
		res.pins = append(res.pins, out[i].pin)
		if out[i].pool.Slice != "" {
			res.pools = append(res.pools, out[i].pool)
		}
		res.latencies = append(res.latencies, out[i].rec.LatencyMS)
	}
	if len(res.excluded) > 0 {
		r.logf("excluded %d of %d cases after %d retries each", len(res.excluded), len(cases), r.retries())
	}
	return res, nil
}

// one measures a single case, spending the retry budget on retryable failures.
func (r *Runner) one(ctx context.Context, c goldset.Case, pins map[string]Pin) outcome {
	req := QueryRequest{Query: c.Query, Synthesize: false, ArmRanks: true, Limit: r.Limit}
	if pins != nil {
		p := pins[CaseKey(c.Slice, c.Index, c.QuerySHA256)]
		tr, tmp := p.Translation, p.Temporal
		req.PinnedTranslation, req.PinnedTemporal = &tr, &tmp
	}

	var lastErr error
	for attempt := 1; attempt <= r.retries()+1; attempt++ {
		start := time.Now()
		resp, err := r.measure(ctx, req)
		elapsed := time.Since(start).Milliseconds()
		switch {
		case err == nil:
			return outcome{
				rec:  buildRecord(c, resp, attempt, elapsed),
				pin:  buildPin(c, resp),
				pool: buildPool(c, resp),
			}
		case errors.Is(err, ErrGateRefused), errors.Is(ctx.Err(), context.Canceled):
			return outcome{fatal: err}
		case !errors.Is(err, ErrRetryable):
			return outcome{fatal: fmt.Errorf("case %s/%d: %w", c.Slice, c.Index, err)}
		}
		lastErr = err
		r.logf("case %s/%d attempt %d failed (%v)", c.Slice, c.Index, attempt, err)
	}
	return outcome{excluded: &ExcludedCase{
		Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
		Attempts: r.retries() + 1, Reason: lastErr.Error(),
	}}
}

// measure is the one place a dry run diverges: it fabricates an empty but
// well-formed response so the whole pipeline — pins, records, stamps, path
// guard, report — can be exercised without an instance.
func (r *Runner) measure(ctx context.Context, req QueryRequest) (*QueryResponse, error) {
	if r.DryRun {
		return &QueryResponse{Success: true, ArmRanks: &ArmRanksBlock{
			EffectiveQuery: req.Query, EmbedModel: "dry-run",
			Selector: Selector{Mode: "ann", Reason: "dry-run"},
		}}, nil
	}
	return r.Client.Measure(ctx, req)
}

func buildRecord(c goldset.Case, resp *QueryResponse, attempt int, elapsed int64) Record {
	inArms := make(map[string]bool, len(resp.ArmRanks.Rows))
	for _, row := range resp.ArmRanks.Rows {
		inArms[row.ID] = true
	}
	delivered := make([]Delivered, 0, len(resp.Sources))
	for _, s := range resp.Sources {
		delivered = append(delivered, Delivered{ID: s.ID, ViaPostStage: !inArms[s.ID]})
	}
	return Record{
		Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
		Split: c.Split, GoldIDs: c.GoldIDs,
		Rows: resp.ArmRanks.Rows, FusionOrder: resp.ArmRanks.FusionOrder, Delivered: delivered,
		EffectiveQuery:       resp.ArmRanks.EffectiveQuery,
		EffectiveQuerySpaced: resp.ArmRanks.EffectiveQuerySpaced,
		EffectiveTemporal:    resp.ArmRanks.EffectiveTemporal,
		EmbedModel:           resp.ArmRanks.EmbedModel,
		EmbedCacheHit:        resp.ArmRanks.EmbedCacheHit,
		Selector:             resp.ArmRanks.Selector,
		Attempts:             attempt, LatencyMS: elapsed,
	}
}

func buildPin(c goldset.Case, resp *QueryResponse) Pin {
	return Pin{
		Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
		Translation: resp.ArmRanks.EffectiveQuery,
		Temporal:    resp.ArmRanks.EffectiveTemporal,
		QuerySpaced: resp.ArmRanks.EffectiveQuerySpaced,
		EmbedModel:  resp.ArmRanks.EmbedModel,
	}
}

// buildPool collects the per-arm heads for the unlabelled slice only. Building
// them for a labelled slice would be wasted bytes: G-KI and G-Q carry
// constructive labels and need no pooling.
func buildPool(c goldset.Case, resp *QueryResponse) PoolEntry {
	if c.Slice != goldset.SliceReal {
		return PoolEntry{}
	}
	rows := resp.ArmRanks.Rows
	return PoolEntry{
		Slice: c.Slice, Index: c.Index, QuerySHA256: c.QuerySHA256,
		Semantic: topByArm(rows, func(r rrf.ArmRow) *int { return r.RankSemantic }),
		FTSDe:    topByArm(rows, func(r rrf.ArmRow) *int { return r.RankFTSDe }),
		FTSEn:    topByArm(rows, func(r rrf.ArmRow) *int { return r.RankFTSEn }),
		Trigram:  topByArm(rows, func(r rrf.ArmRow) *int { return r.RankTrigram }),
	}
}

// topByArm returns the PoolDepth best ids of one arm, by that arm's own rank —
// not by the fused score. A pool drawn from the fusion would inherit the very
// weighting the sweep is trying to judge.
func topByArm(rows []rrf.ArmRow, rank func(rrf.ArmRow) *int) []string {
	type entry struct {
		id string
		r  int
	}
	var in []entry
	for _, row := range rows {
		if v := rank(row); v != nil {
			in = append(in, entry{row.ID, *v})
		}
	}
	sort.Slice(in, func(i, j int) bool {
		if in[i].r != in[j].r {
			return in[i].r < in[j].r
		}
		return in[i].id < in[j].id
	})
	if len(in) > PoolDepth {
		in = in[:PoolDepth]
	}
	out := make([]string, 0, len(in))
	for _, e := range in {
		out = append(out, e.id)
	}
	return out
}

func sliceNamesOf(cases []goldset.Case) []string {
	seen := map[string]bool{}
	var out []string
	for _, n := range CanonicalSlices() {
		for _, c := range cases {
			if c.Slice == n && !seen[n] {
				seen[n], out = true, append(out, n)
			}
		}
	}
	return out
}

// MarkAborted renames a written dump so nothing can score it by accident. The
// bytes are KEPT: an aborted run is evidence about what the corpus did, and
// deleting it would destroy the only record of the drift that caused the abort.
func MarkAborted(path string) (string, error) {
	target := path + ".aborted"
	if err := os.Rename(path, target); err != nil {
		return "", err
	}
	return target, nil
}
