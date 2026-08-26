package main

// The multi-gold slice generators of wave M-W5: sess, mh, glob and the floor
// check glob-konstr (design/05 §4.5).
//
// All four share one runner. That is not tidiness — it is the only way the
// on-prem assertion, the redaction sweep, the concurrency cap and the stamp
// stay identical across four slices instead of drifting apart one copy at a
// time.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

// slicesOpts are the flags every new generator shares.
type slicesOpts struct {
	n           int
	minContent  int
	backend     string
	model       string
	concurrency int
	timeoutSec  int
	dryRun      bool
	maxGold     int
}

// job is one candidate: a rendered prompt plus the case it becomes if the
// answer survives the filter.
type job struct {
	prompt string
	proto  goldset.Case
}

// runGeneration is the shared LLM phase. Order is restored after the parallel
// phase so the slice file is deterministic in candidate order, and the
// concurrency cap is applied here rather than trusted to the caller — the
// endpoint is production serving.
func runGeneration(ctx context.Context, client *goldset.ChatClient, slice string, jobs []job, conc, want int) (cases []goldset.Case, calls, shape, redaction int) {
	if conc > maxConcurrency {
		conc = maxConcurrency
	}
	if conc < 1 {
		conc = 1
	}
	type result struct {
		i       int
		c       goldset.Case
		verdict goldset.Verdict
	}
	in := make(chan int)
	out := make(chan result, len(jobs))
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range in {
				j := jobs[i]
				ans, err := client.Ask(ctx, systemFor(slice), j.prompt, 160, 0.3)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  %s case %d: %v\n", slice, i, err)
					out <- result{i: i, verdict: goldset.VerdictShape}
					continue
				}
				q, v := goldset.InspectGeneratedQuery(ans, slice)
				c := j.proto
				c.Query = q
				out <- result{i: i, c: c, verdict: v}
			}
		}()
	}
	go func() {
		for i := range jobs {
			in <- i
		}
		close(in)
		wg.Wait()
		close(out)
	}()

	byIdx := map[int]goldset.Case{}
	for r := range out {
		calls++
		switch r.verdict {
		case goldset.VerdictAccept:
			byIdx[r.i] = r.c
		case goldset.VerdictRedaction:
			redaction++
		case goldset.VerdictShape:
			shape++
		}
	}
	idx := make([]int, 0, len(byIdx))
	for i := range byIdx {
		idx = append(idx, i)
	}
	sort.Ints(idx)

	// A question another case already produced would make two cases with
	// different gold carry the same query — the pairing across dumps is keyed
	// on the query digest, so that is a collision, not a duplicate.
	seen := map[string]bool{}
	cases = make([]goldset.Case, 0, len(idx))
	for _, i := range idx {
		c := byIdx[i]
		key := normalizeQuery(c.Query)
		if seen[key] {
			shape++
			continue
		}
		seen[key] = true
		cases = append(cases, c)
		if want > 0 && len(cases) >= want {
			break
		}
	}
	return cases, calls, shape, redaction
}

// maxConcurrency mirrors the cap of the `q` subcommand: the generator endpoint
// is production serving, so this is a ceiling, not a default.
const maxConcurrency = 2

func normalizeQuery(q string) string {
	return goldset.SHA256Hex(lower(q))
}

func lower(s string) string {
	b := []rune(s)
	for i, r := range b {
		if r >= 'A' && r <= 'Z' {
			b[i] = r + ('a' - 'A')
		}
	}
	return string(b)
}

func systemFor(slice string) string {
	switch slice {
	case goldset.SliceSess:
		return goldset.SessSystem()
	case goldset.SliceMH:
		return goldset.MHSystem()
	default:
		return goldset.GlobSystem()
	}
}

// prepared is everything a generator needs before the first model call.
type prepared struct {
	g      *goldset.Guard
	db     *goldset.DB
	client *goldset.ChatClient
	be     goldset.Backend
}

// prepare opens the guard, the read-only database and — unless this is a dry
// run — the verified on-prem chat client. The on-prem check happens BEFORE any
// row is drawn, so a mislabelled backend cannot be discovered halfway through
// a slice.
func prepare(ctx context.Context, c *common, o slicesOpts) (*prepared, error) {
	g, err := c.guard()
	if err != nil {
		return nil, err
	}
	db, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	p := &prepared{g: g, db: db}
	if o.dryRun {
		return p, nil
	}
	be, err := db.LookupBackend(ctx, o.backend)
	if err != nil {
		_ = db.Close(ctx)
		return nil, err
	}
	if err := goldset.RequireOnPrem(be); err != nil {
		_ = db.Close(ctx)
		return nil, err // hard abort — never an external endpoint
	}
	client, err := goldset.NewChatClient(be, o.model, time.Duration(o.timeoutSec)*time.Second)
	if err != nil {
		_ = db.Close(ctx)
		return nil, err
	}
	p.be, p.client = be, client
	return p, nil
}

// finish writes the slice file and its profile, and refreshes the K9 population
// figures. A slice that missed its target n is an ERROR: silently shipping a
// short slice would move a gate without anybody deciding to.
func finish(p *prepared, c *common, o slicesOpts, slice, file string, cases []goldset.Case,
	candidates, construction, redaction, generator int, elapsed time.Duration, calls int, population string) error {
	ctx := context.Background()
	total, median := goldset.GoldStats(cases)
	profile, _ := goldset.ProfileFor(slice)
	profile.Population = population
	profile.Generator = &goldset.Generator{
		Backend: p.be.Name, Model: p.client.Model, Endpoint: p.client.URL,
		Locality: p.be.Locality, Trust: p.be.Trust,
		PromptSHA256: goldset.PromptSHA256For(slice),
		GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
		Calls:        calls, DurationSec: elapsed.Seconds(), Concurrency: min(o.concurrency, maxConcurrency),
	}
	if slice == goldset.SliceSess {
		profile.WindowRule = windowRule
	}
	if slice == goldset.SliceMH {
		profile.ConfidenceFloor = goldset.MinDreamConfidence
	}

	if err := writeSlice(p.g, c, slice, file, cases, func(s *goldset.SliceStamp) {
		s.Candidates = candidates
		s.DiscardedConstruction = construction
		s.DiscardedRedaction = redaction
		s.DiscardedGenerator = generator
		s.GoldIDs, s.GoldIDsMedian = total, median
		s.Profile = &profile
	}); err != nil {
		return err
	}
	retrievable, err := p.db.RetrievableCount(ctx)
	if err != nil {
		return err
	}
	active, err := p.db.ActiveCount(ctx)
	if err != nil {
		return err
	}
	maxCreated, err := p.db.CorpusMaxCreatedAt(ctx)
	if err != nil {
		return err
	}
	if err := updateStamp(p.g, func(s *goldset.Stamp) {
		s.RetrievableBlocks = retrievable
		s.CorpusMaxCreatedAt = maxCreated
		s.Population = &goldset.Population{
			Definition:  populationDefinition,
			Retrievable: retrievable,
			Active:      active,
		}
	}); err != nil {
		return err
	}
	fmt.Printf("%s: n=%d candidates=%d discarded_construction=%d discarded_redaction=%d discarded_shape=%d calls=%d elapsed=%s gold_ids=%d gold_median=%d model=%s endpoint=%s\n",
		slice, len(cases), candidates, construction, redaction, generator, calls,
		elapsed.Round(time.Second), total, median, p.client.Model, p.client.URL)
	if len(cases) < o.n {
		return fmt.Errorf("%s target n=%d not reached (%d)", slice, o.n, len(cases))
	}
	return nil
}

// populationDefinition and windowRule are stamped verbatim: K9 says a
// measurement names its ground set instead of implying one, and the session
// window rule is the declared bias of G-SESS.
const (
	populationDefinition = "non-archived blocks whose type policy in context_block_types is not 'excluded' (retrieval-visible); 'active' counts every non-archived block"
	windowRule           = "half-open [day 00:00Z, day+1 00:00Z) per calendar day carrying a daily report (title date, not created_at); span windows are disjoint runs of consecutive reported days"
)

// ------------------------------------------------------------------ sess.

// cmdSess builds G-SESS. The gold is constructive and comes from timestamps and
// daily reports only — no insight, no cluster, no summary — which is what makes
// the slice usable as the primary measurement for the insight layer.
func cmdSess(c *common, o slicesOpts, spanLen int) error {
	ctx := context.Background()
	p, err := prepare(ctx, c, o)
	if err != nil {
		return err
	}
	defer func() { _ = p.db.Close(ctx) }()

	reports, err := p.db.SessionReports(ctx)
	if err != nil {
		return err
	}
	windows := goldset.BuildSessionWindows(reports, []int{spanLen, spanLen * 2})
	var jobs []job
	construction := 0
	for _, w := range windows {
		gold, goldErr := p.db.WindowGold(ctx, w)
		if goldErr != nil {
			return goldErr
		}
		// A window whose gold set dwarfs the retrieval window measures coverage,
		// not ranking: Recall@5 is capped at 5/len(gold) before a single arm
		// runs. Such a window is dropped, never trimmed — trimming would label
		// genuinely relevant blocks as irrelevant.
		if len(gold) == 0 || len(gold) > o.maxGold {
			construction++
			continue
		}
		jobs = append(jobs, job{prompt: goldset.SessPrompt(w), proto: goldset.Case{
			Slice: goldset.SliceSess, GoldIDs: gold, Origin: "session-window",
			PoolRef: w.PoolRef(), SourceTitle: w.Label,
		}})
	}
	fmt.Printf("G-SESS candidates: windows=%d usable=%d dropped_construction=%d (max-gold=%d)\n",
		len(windows), len(jobs), construction, o.maxGold)
	if o.dryRun {
		return nil
	}
	start := time.Now()
	cases, calls, shape, redaction := runGeneration(ctx, p.client, goldset.SliceSess, jobs, o.concurrency, o.n)
	return finish(p, c, o, goldset.SliceSess, goldset.FileSess, cases,
		len(jobs), construction, redaction, shape, time.Since(start), calls,
		fmt.Sprintf("%d daily reports (audit-trail/synthesis) forming %d windows", len(reports), len(windows)))
}

// -------------------------------------------------------------------- mh.

// cmdMH builds G-MH from dream links at confidence >= goldset.MinDreamConfidence.
func cmdMH(c *common, o slicesOpts) error {
	ctx := context.Background()
	p, err := prepare(ctx, c, o)
	if err != nil {
		return err
	}
	defer func() { _ = p.db.Close(ctx) }()

	drawn, err := p.db.DreamLinkPairs(ctx, c.seed+2, o.n*2, o.minContent)
	if err != nil {
		return err
	}
	links := goldset.FilterDreamLinks(drawn)
	construction := len(drawn) - len(links)
	jobs := make([]job, 0, len(links))
	for _, l := range links {
		jobs = append(jobs, job{prompt: goldset.MHPrompt(l), proto: goldset.Case{
			Slice: goldset.SliceMH, GoldIDs: l.GoldIDs(), Origin: "dream-bridge",
			PoolRef: l.PoolRef(),
		}})
	}
	fmt.Printf("G-MH candidates: drawn=%d usable=%d dropped_construction=%d (floor=%.2f)\n",
		len(drawn), len(jobs), construction, goldset.MinDreamConfidence)
	if o.dryRun {
		return nil
	}
	start := time.Now()
	cases, calls, shape, redaction := runGeneration(ctx, p.client, goldset.SliceMH, jobs, o.concurrency, o.n)
	return finish(p, c, o, goldset.SliceMH, goldset.FileMH, cases,
		len(jobs), construction, redaction, shape, time.Since(start), calls,
		fmt.Sprintf("dream links at confidence >= %.2f between two retrievable blocks", goldset.MinDreamConfidence))
}

// ------------------------------------------------------------------ glob.

// cmdGlob builds G-GLOB from corpus TAGS. Gold stays empty: these judgements
// are the ones E-9 assigns to the judging run, and a constructive label here
// would quietly answer a question the project decided to judge.
func cmdGlob(c *common, o slicesOpts, minBlocks, sampleTitles int) error {
	ctx := context.Background()
	p, err := prepare(ctx, c, o)
	if err != nil {
		return err
	}
	defer func() { _ = p.db.Close(ctx) }()

	pools, err := p.db.TagPools(ctx, c.seed+3, o.n*2, minBlocks, sampleTitles)
	if err != nil {
		return err
	}
	jobs := make([]job, 0, len(pools))
	for _, pool := range pools {
		jobs = append(jobs, job{prompt: goldset.GlobPrompt(pool, goldset.GlobAspects[0]), proto: goldset.Case{
			Slice: goldset.SliceGlob, Origin: "tag-aggregate",
			PoolRef: pool.Ref, SourceTitle: pool.Label,
		}})
	}
	fmt.Printf("G-GLOB candidates: tag pools=%d (min-blocks=%d)\n", len(jobs), minBlocks)
	if o.dryRun {
		return nil
	}
	start := time.Now()
	cases, calls, shape, redaction := runGeneration(ctx, p.client, goldset.SliceGlob, jobs, o.concurrency, o.n)
	return finish(p, c, o, goldset.SliceGlob, goldset.FileGlob, cases,
		len(jobs), 0, redaction, shape, time.Since(start), calls,
		fmt.Sprintf("corpus tags carrying >= %d retrievable blocks", minBlocks))
}

// cmdGlobKonstr builds the floor check: the same question family, but with gold
// taken from graph_cluster_member. Circular against the graph layer by
// construction — the profile says so and ReportSlices leaves it out.
func cmdGlobKonstr(c *common, o slicesOpts, minMembers, sampleTitles int) error {
	ctx := context.Background()
	p, err := prepare(ctx, c, o)
	if err != nil {
		return err
	}
	defer func() { _ = p.db.Close(ctx) }()

	pools, err := p.db.ClusterPools(ctx, minMembers, sampleTitles)
	if err != nil {
		return err
	}
	var jobs []job
	construction := 0
	for _, pool := range pools {
		if len(pool.GoldIDs) > o.maxGold {
			construction++
			continue
		}
		// One cluster carries several cases, one per aspect: there are only so
		// many clusters, and three angles on one topic are three questions —
		// with the SAME gold, which is a formulation variation and is declared
		// as one.
		for _, aspect := range goldset.GlobAspects {
			jobs = append(jobs, job{prompt: goldset.GlobPrompt(pool, aspect), proto: goldset.Case{
				Slice: goldset.SliceGlobKonstr, GoldIDs: pool.GoldIDs, Origin: "cluster-aggregate",
				PoolRef: pool.Ref, SourceTitle: pool.Label,
			}})
		}
	}
	fmt.Printf("G-GLOB-KONSTR candidates: clusters=%d jobs=%d dropped_construction=%d (min-members=%d, aspects=%d)\n",
		len(pools), len(jobs), construction, minMembers, len(goldset.GlobAspects))
	if o.dryRun {
		return nil
	}
	start := time.Now()
	cases, calls, shape, redaction := runGeneration(ctx, p.client, goldset.SliceGlobKonstr, jobs, o.concurrency, o.n)
	return finish(p, c, o, goldset.SliceGlobKonstr, goldset.FileGlobKonstr, cases,
		len(jobs), construction, redaction, shape, time.Since(start), calls,
		fmt.Sprintf("clusters with >= %d retrievable members, %d aspects each", minMembers, len(goldset.GlobAspects)))
}
