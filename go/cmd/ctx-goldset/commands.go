package main

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/GottZ/ctx/internal/goldset"
)

// fileQRaw holds the generated questions before the hand check; fileQCheck is
// the human-readable sample. Both stay inside the gold directory.
const (
	fileQRaw   = "g-q-raw.jsonl"
	fileQCheck = "g-q-handcheck.txt"
)

// cmdKI draws known-item cases: query = lightly paraphrased title, gold = that
// very block. Declared bias: strongly trigram- and title-FTS-friendly, usable
// as a floor only (design 04 §4.5).
func cmdKI(c *common, n, minContent int) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	db, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(ctx) }()

	blocks, err := db.RetrievableBlocks(ctx, c.seed, n*2, minContent)
	if err != nil {
		return err
	}
	cases := make([]goldset.Case, 0, n)
	seen := map[string]bool{}
	skipped := 0
	for _, b := range blocks {
		if len(cases) >= n {
			break
		}
		q := goldset.ParaphraseTitle(b.Title, c.seed)
		// A paraphrase that collapses to a text another block already produced
		// makes the "gold = exactly this block" label false. Drop, do not
		// dedupe silently into a multi-label case.
		if q == "" || seen[strings.ToLower(q)] {
			skipped++
			continue
		}
		seen[strings.ToLower(q)] = true
		cases = append(cases, goldset.Case{
			Slice: goldset.SliceKI, Query: q, GoldIDs: []string{b.ID},
			Origin: "title-paraphrase", SourceTitle: b.Title,
		})
	}
	cand := len(blocks)
	if err := writeSlice(g, c, goldset.SliceKI, goldset.FileKI, cases, func(s *goldset.SliceStamp) {
		s.Candidates = cand
	}); err != nil {
		return err
	}
	fmt.Printf("G-KI: n=%d candidates=%d skipped_ambiguous=%d\n", len(cases), cand, skipped)
	if len(cases) < n {
		return fmt.Errorf("G-KI target n=%d not reached (%d)", n, len(cases))
	}
	return nil
}

type qOpts struct {
	n, minContent, concurrency int
	backend, model             string
	timeout                    time.Duration
	checkFrac                  float64
}

// cmdQ generates the content-derived questions. This is the ONE point in the
// axis where block content reaches a model, so the endpoint is verified on-prem
// before the first call and its identity goes into the stamp (§4.5).
func cmdQ(c *common, o qOpts) error {
	ctx := context.Background()
	g, err := c.guard()
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
		return err // hard abort — never an external endpoint
	}
	if o.concurrency > 2 {
		o.concurrency = 2
	}
	client, err := goldset.NewChatClient(be, o.model, o.timeout)
	if err != nil {
		return err
	}
	blocks, err := db.RetrievableBlocks(ctx, c.seed+1, o.n, o.minContent)
	if err != nil {
		return err
	}

	start := time.Now()
	cases, calls, rejected := generateQuestions(ctx, client, blocks, o.concurrency)
	elapsed := time.Since(start)

	if err := writeSlice(g, c, goldset.SliceQ, fileQRaw, cases, func(s *goldset.SliceStamp) {
		s.Candidates = len(blocks)
		s.DiscardedGenerator = rejected
	}); err != nil {
		return err
	}
	if err := updateStamp(g, func(s *goldset.Stamp) {
		s.Generator = &goldset.Generator{
			Backend: be.Name, Model: client.Model, Endpoint: client.URL,
			Locality: be.Locality, Trust: be.Trust,
			PromptSHA256: goldset.PromptSHA256(),
			GeneratedAt:  time.Now().UTC().Format(time.RFC3339),
			Calls:        calls, DurationSec: elapsed.Seconds(), Concurrency: o.concurrency,
		}
	}); err != nil {
		return err
	}
	if err := writeHandcheck(g, cases, c.seed, o.checkFrac); err != nil {
		return err
	}
	fmt.Printf("G-Q raw: n=%d attempted=%d rejected_shape=%d calls=%d elapsed=%s model=%s endpoint=%s\n",
		len(cases), len(blocks), rejected, calls, elapsed.Round(time.Second), client.Model, client.URL)
	return nil
}

// generateQuestions runs the frozen prompt over the blocks. Order is restored
// after the parallel phase so the raw file is deterministic in block order.
func generateQuestions(ctx context.Context, client *goldset.ChatClient, blocks []goldset.Block, conc int) ([]goldset.Case, int, int) {
	type result struct {
		i    int
		c    goldset.Case
		ok   bool
		call bool
	}
	jobs := make(chan int)
	out := make(chan result, len(blocks))
	var wg sync.WaitGroup
	for w := 0; w < conc; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				b := blocks[i]
				ans, err := client.Ask(ctx, goldset.GQSystem(), goldset.GQPrompt(b), 160, 0.3)
				if err != nil {
					fmt.Fprintf(os.Stderr, "  block %s: %v\n", b.ID[:8], err)
					out <- result{i: i, call: true}
					continue
				}
				q, ok := goldset.AcceptQuestion(ans, b.Title)
				out <- result{i: i, ok: ok, call: true, c: goldset.Case{
					Slice: goldset.SliceQ, Query: q, GoldIDs: []string{b.ID}, Origin: "llm-question",
				}}
			}
		}()
	}
	go func() {
		for i := range blocks {
			jobs <- i
		}
		close(jobs)
		wg.Wait()
		close(out)
	}()

	byIdx := map[int]goldset.Case{}
	calls, rejected := 0, 0
	for r := range out {
		if r.call {
			calls++
		}
		if r.ok {
			byIdx[r.i] = r.c
		} else {
			rejected++
		}
	}
	idx := make([]int, 0, len(byIdx))
	for i := range byIdx {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	cases := make([]goldset.Case, 0, len(idx))
	for _, i := range idx {
		cases = append(cases, byIdx[i])
	}
	return cases, calls, rejected
}

// writeHandcheck lists a seeded sample for human reading. The sample carries
// the question and the raw index only — the reader judges answerability from
// the question plus the source block, not from a pre-cooked verdict.
func writeHandcheck(g *goldset.Guard, cases []goldset.Case, seed int64, frac float64) error {
	p, err := g.Resolve(fileQCheck)
	if err != nil {
		return err
	}
	step := len(cases)
	want := int(float64(len(cases))*frac + 0.5)
	if want < 1 {
		want = 1
	}
	if want > 0 {
		step = len(cases) / want
	}
	if step < 1 {
		step = 1
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# G-Q hand check — %d of %d raw cases, systematic sample (seed %d, step %d)\n",
		want, len(cases), seed, step)
	fmt.Fprintf(&b, "# verdict per line: keep | drop. Feed dropped indices to `ctx-goldset qfinal -drop`.\n\n")
	offset := int(seed) % step
	if offset < 0 {
		offset += step
	}
	for i := offset; i < len(cases); i += step {
		fmt.Fprintf(&b, "idx=%d block=%s\n  Q: %s\n\n", i, cases[i].GoldIDs[0], cases[i].Query)
	}
	return os.WriteFile(p, []byte(b.String()), 0o600)
}

// cmdQFinal removes the hand-check rejects, trims to the target n and applies
// the seeded 50/50 DERIV/HOLD partition (§4.6).
func cmdQFinal(c *common, n int, dropSpec string) error {
	g, err := c.guard()
	if err != nil {
		return err
	}
	rawPath, err := g.Resolve(fileQRaw)
	if err != nil {
		return err
	}
	raw, err := goldset.ReadJSONL(rawPath)
	if err != nil {
		return err
	}
	drop := map[int]bool{}
	for _, s := range strings.Split(dropSpec, ",") {
		if s = strings.TrimSpace(s); s == "" {
			continue
		}
		i, convErr := strconv.Atoi(s)
		if convErr != nil {
			return fmt.Errorf("-drop %q: %w", s, convErr)
		}
		drop[i] = true
	}
	kept := make([]goldset.Case, 0, len(raw))
	for i, cs := range raw {
		if drop[i] {
			continue
		}
		kept = append(kept, cs)
	}
	if len(kept) < n {
		return fmt.Errorf("G-Q target n=%d not reachable: %d kept after %d hand-check drops", n, len(kept), len(drop))
	}
	kept = kept[:n]

	keys := make([]string, len(kept))
	for i := range kept {
		keys[i] = kept[i].GoldIDs[0]
	}
	split := goldset.Split(keys, c.splitSeed)
	for i := range kept {
		kept[i].Split = split[kept[i].GoldIDs[0]]
	}
	deriv, hold := goldset.SplitCounts(split)
	fp := goldset.SplitFingerprint(split)

	if err := writeSlice(g, c, goldset.SliceQ, goldset.FileQ, kept, func(s *goldset.SliceStamp) {
		s.File = goldset.FileQ
		s.DiscardedHandcheck = len(drop)
		s.SplitDeriv, s.SplitHold, s.SplitFingerprint = deriv, hold, fp
	}); err != nil {
		return err
	}
	fmt.Printf("G-Q final: n=%d dropped_handcheck=%d DERIV=%d HOLD=%d split_seed=%d fingerprint=%s\n",
		len(kept), len(drop), deriv, hold, c.splitSeed, fp[:16])
	return nil
}

// cmdReal draws real access-log queries. Two rules are load-bearing:
// the source != 'armsweep' filter (§5.3d) and the redaction sweep, which
// DISCARDS a hit instead of carrying it on redacted (§4.5).
func cmdReal(c *common, n, days, minLen int) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	db, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(ctx) }()

	pool, err := db.AccessLogCandidateCount(ctx, days, minLen)
	if err != nil {
		return err
	}
	texts, err := db.AccessLogQueries(ctx, days, minLen, n*3, c.seed)
	if err != nil {
		return err
	}
	cases := make([]goldset.Case, 0, n)
	discarded := map[string]int{}
	for _, t := range texts {
		if len(cases) >= n {
			break
		}
		if m, hit := goldset.ScanQuery(t); hit {
			discarded[m.Kind]++
			continue
		}
		// No labels here: the G-REAL relevance judgements need the pooling dump
		// from the driver and land in wave B-W6.
		cases = append(cases, goldset.Case{Slice: goldset.SliceReal, Query: t, Origin: "access-log"})
	}
	total := 0
	kinds := make([]string, 0, len(discarded))
	for k, v := range discarded {
		total += v
		kinds = append(kinds, fmt.Sprintf("%s=%d", k, v))
	}
	sort.Strings(kinds)
	if err := writeSlice(g, c, goldset.SliceReal, goldset.FileReal, cases, func(s *goldset.SliceStamp) {
		s.Candidates = pool
		s.DiscardedRedaction = total
	}); err != nil {
		return err
	}
	fmt.Printf("G-REAL: n=%d pool=%d drawn=%d discarded_redaction=%d [%s]\n",
		len(cases), pool, len(texts), total, strings.Join(kinds, " "))
	if len(cases) < n {
		return fmt.Errorf("G-REAL target n=%d not reached (%d)", n, len(cases))
	}
	return nil
}

// cmdStamp refreshes the file digests and the corpus contamination stamp.
func cmdStamp(c *common) error {
	ctx := context.Background()
	g, err := c.guard()
	if err != nil {
		return err
	}
	db, err := c.open(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = db.Close(ctx) }()

	maxCreated, err := db.CorpusMaxCreatedAt(ctx)
	if err != nil {
		return err
	}
	retrievable, err := db.RetrievableCount(ctx)
	if err != nil {
		return err
	}
	if err := updateStamp(g, func(s *goldset.Stamp) {
		s.CorpusMaxCreatedAt = maxCreated
		s.RetrievableBlocks = retrievable
		for name, st := range s.Slices {
			if p, resErr := g.Resolve(st.File); resErr == nil {
				if d, digErr := goldset.FileDigest(p); digErr == nil {
					st.SHA256 = d
					if cases, readErr := goldset.ReadJSONL(p); readErr == nil {
						st.N = len(cases)
					}
					s.Slices[name] = st
				}
			}
		}
	}); err != nil {
		return err
	}
	p, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		return err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return err
	}
	fmt.Print(string(b))
	return nil
}
