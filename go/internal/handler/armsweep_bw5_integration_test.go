//go:build integration

// B-W5 gates for the ctx-armsweep driver (design 04 §7 B-W5), driven against a
// REAL PG18 testcontainer through a REAL httptest server that serves the
// production /api/query and /api/manage handlers.
//
// The test lives in package handler and not in package armsweep for one
// mechanical reason: the request path needs an injected admin AuthResult, and
// authResultKey is unexported. armsweep imports no handler code, so a handler
// test may import armsweep without a cycle — verified by `go list -deps`.
//
//	go test -tags=integration ./internal/handler/ -run TestBW5 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/armsweep"
	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/goldset"
	"github.com/jackc/pgx/v5/pgxpool"
)

// bw5Cases is the harness case count. Twenty is the size gate (a) is stated
// over ("20 Gold-Queries, ≥ 10 deutsch"), so the latency probe below measures
// the same shape the live budget will be measured on.
const bw5Cases = 20

// bw5Rig is one wired instance: pool, HTTP server, and the counters the gates
// read their evidence off.
type bw5Rig struct {
	pool    *pgxpool.Pool
	srv     *httptest.Server
	goldDir string
	// failNext makes the NEXT n query requests answer 500 — the transport
	// fault gate (g) spends its retry budget on.
	failNext atomic.Int32
	// onQuery runs before each forwarded query, so a gate can mutate the corpus
	// mid-dump instead of before or after it.
	onQuery atomic.Pointer[func(int32)]
	queries atomic.Int32
}

// bw5Topics are the vocabulary the harness corpus and its queries are built
// from. Fifteen topics over sixty blocks means four blocks compete per query,
// which is what gives the lexical arms something to rank differently from the
// semantic one.
var bw5Topics = []string{
	"retention", "maintenance", "classification", "storage", "audit",
	"backup", "migration", "embedding", "cluster", "quota",
	"session", "gateway", "schedule", "digest", "tenant",
}

const bw5Blocks = 60

func bw5BlockID(i int) string { return fmt.Sprintf("019fa510-0000-7000-9000-0000000%05d", i) }

// bw5Corpus adds sixty topical blocks on top of the bw2 fixture.
//
// The bw2 fixture alone is NOT enough for a parity gate: five blocks over a
// single dominant semantic arm rank the same under every weight vector, so a
// parity assertion on it would hold for a fusion that ignored its weights
// entirely (measured — see the negative control in TestBW5DumpParity).
//
// Two properties make this corpus discriminating. Titles and contents draw on
// a shared topic vocabulary, so the FTS and trigram arms produce a DIFFERENT
// order per query. And the embedding index is a permutation of the block index
// (i·23 mod 60), so the semantic order — which is identical for every query,
// the fake embed backend returning one fixed vector — is not a relabelling of
// the lexical order. Reweighting therefore reorders.
func bw5Corpus(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for i := 0; i < bw5Blocks; i++ {
		topic := bw5Topics[i%len(bw5Topics)]
		other := bw5Topics[(i*7+3)%len(bw5Topics)]
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', $2, $3, $4, $5, 'knowledge')`,
			bw5BlockID(i),
			fmt.Sprintf("%s notes %02d for %s", topic, i, other),
			fmt.Sprintf("this entry documents the %s procedure and its %s implications, revision %02d", topic, other, i),
			bw2Scope, pgvec.NewVector(bw2Embedding(((i*23)%bw5Blocks)*2)),
		); err != nil {
			t.Fatalf("insert bw5 block %d: %v", i, err)
		}
	}
}

func bw5NewRig(t *testing.T) *bw5Rig {
	t.Helper()
	pool := bw2Setup(t)
	bw5Corpus(t, pool)
	rig := &bw5Rig{pool: pool}

	qh := bw2Handler(t, pool, bw2NewBackend(t))
	mh := NewManageHandler(pool, config.NewStore(bw2Config()), nil, nil, nil, nil, nil, blocktype.NewRegistry())

	ar := &auth.AuthResult{
		ApiKeyID: bw2AdminKeyID, HomeScope: bw2Scope, ReadScopes: []string{bw2Scope},
		IsValid: true, IsAdmin: true,
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/query", func(w http.ResponseWriter, r *http.Request) {
		n := rig.queries.Add(1)
		if fn := rig.onQuery.Load(); fn != nil {
			(*fn)(n)
		}
		if rig.failNext.Load() > 0 {
			rig.failNext.Add(-1)
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"success":false,"error":"embed chain exhausted"}`))
			return
		}
		qh.HandleQuery(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	})
	mux.HandleFunc("/api/manage", func(w http.ResponseWriter, r *http.Request) {
		mh.HandleManage(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	})
	rig.srv = httptest.NewServer(mux)
	t.Cleanup(rig.srv.Close)

	rig.goldDir = bw5WriteGold(t)
	return rig
}

// bw5WriteGold materialises a SYNTHETIC gold directory. The real one is private
// and root-only; a gate that needed it could not run in CI, and a gate that
// copied it would put private query texts into a temp directory.
func bw5WriteGold(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), goldset.DirName)
	g, err := goldset.NewGuard(dir, false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}

	// Ten of twenty queries are German, the minimum gate (a) is stated over.
	// Caveat of the harness, not of the tool: the fake chat backend answers
	// every translation with the same string, so all ten arrive at ctx_rrf
	// identical. They exercise the translation STAGE and the pins, not lexical
	// variety — the ten English ones carry that.
	german := []string{
		"wie ist die aufbewahrung geregelt", "welche wartung ist dokumentiert",
		"wie wird eingeordnet", "wie ist die ablage aufgebaut",
		"wie wird geprueft", "wie laeuft die sicherung",
		"wie laeuft die umstellung", "wie entstehen die vektoren",
		"wie werden gruppen gebildet", "welche grenzen sind gesetzt",
	}
	english := []string{
		"retention notes and their storage implications",
		"maintenance procedure revision",
		"classification notes for audit",
		"storage procedure and quota implications",
		"audit notes and session implications",
		"backup procedure documentation",
		"migration notes for gateway",
		"embedding procedure and schedule",
		"cluster notes and digest implications",
		"quota procedure for tenant",
	}

	var ki, q []goldset.Case
	all := append(append([]string{}, german...), english...)
	for i, text := range all {
		c := goldset.Case{
			Slice: goldset.SliceKI, Query: text, Origin: "harness",
			GoldIDs: []string{bw5BlockID((i * 3) % bw5Blocks)},
		}
		ki = append(ki, c)
		qc := c
		qc.Slice, qc.Origin = goldset.SliceQ, "harness"
		qc.Split = goldset.SplitDeriv
		if i%2 == 1 {
			qc.Split = goldset.SplitHold
		}
		q = append(q, qc)
	}

	for _, w := range []struct {
		file  string
		cases []goldset.Case
		slice string
	}{
		{goldset.FileKI, ki, goldset.SliceKI},
		{goldset.FileQ, q, goldset.SliceQ},
	} {
		p, rerr := g.Resolve(w.file)
		if rerr != nil {
			t.Fatalf("resolve %s: %v", w.file, rerr)
		}
		if rerr = goldset.WriteJSONL(p, w.cases); rerr != nil {
			t.Fatalf("write %s: %v", w.file, rerr)
		}
	}

	stampPath, err := g.Resolve(goldset.FileStamp)
	if err != nil {
		t.Fatalf("resolve stamp: %v", err)
	}
	if err := goldset.WriteStamp(stampPath, goldset.Stamp{
		Version: 1, SampleSeed: 20260812, SplitSeed: 20260825,
		CorpusMaxCreatedAt: time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano),
		Slices:             map[string]goldset.SliceStamp{},
	}); err != nil {
		t.Fatalf("write stamp: %v", err)
	}
	return dir
}

// runner wires the driver against the rig.
func (rig *bw5Rig) runner(t *testing.T, runID string) (*armsweep.Runner, *goldset.Guard, *goldset.Guard) {
	t.Helper()
	gold, err := goldset.NewGuard(rig.goldDir, false)
	if err != nil {
		t.Fatalf("gold guard: %v", err)
	}
	dumps, err := goldset.NewNamedGuard(filepath.Join(gold.Root(), armsweep.DumpDirName), armsweep.DumpDirName, false)
	if err != nil {
		t.Fatalf("dump guard: %v", err)
	}
	return &armsweep.Runner{
		Client:  armsweep.NewClient(rig.srv.URL, "harness", 60*time.Second),
		GoldDir: gold, DumpDir: dumps, RunID: runID, Concurrency: 1,
		Retries: armsweep.DefaultRetries,
		Logf:    func(format string, args ...any) { t.Logf(format, args...) },
	}, gold, dumps
}

func (rig *bw5Rig) cases(t *testing.T, gold *goldset.Guard, slices ...string) []goldset.Case {
	t.Helper()
	if len(slices) == 0 {
		slices = []string{goldset.SliceKI}
	}
	cases, err := armsweep.LoadCases(gold, slices)
	if err != nil {
		t.Fatalf("load cases: %v", err)
	}
	if len(cases) != bw5Cases*len(slices) {
		t.Fatalf("loaded %d cases, want %d", len(cases), bw5Cases*len(slices))
	}
	return cases
}

func bw5GoldIDs(cases []goldset.Case) []string {
	seen := map[string]bool{}
	var out []string
	for _, c := range cases {
		for _, id := range c.GoldIDs {
			if !seen[id] {
				seen[id], out = true, append(out, id)
			}
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Gate (b): P2 dump parity
// ---------------------------------------------------------------------------

// TestBW5DumpParity is gate (b): re-fusing a dump record under V0 must
// reproduce the fusion order the LIVE ctx_rrf produced from the same arm ranks.
//
// This is the claim that makes every other number in a report mean anything: if
// the offline fusion is not the live fusion at the live weights, then a variant
// weight is being compared against a baseline that never ran.
//
// The gate carries its own NEGATIVE CONTROL, because a parity assertion is
// worth nothing unless the fixture can distinguish configurations at all. The
// bw2 fixture alone cannot: on five blocks over a dominant semantic arm even a
// fully reversed weight vector reproduced the live order on 20 of 20 records
// (measured), so bw5Corpus adds sixty topical blocks. With those, the reversed
// vector diverges on 10 of 20 and k=60→10 fails at position 3 of the first
// record — the two mutations this gate is meant to catch.
func TestBW5DumpParity(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, _ := rig.runner(t, "parity")
	cases := rig.cases(t, gold)

	pins, _, _, err := r.Prime(context.Background(), cases)
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	pinMap := map[string]armsweep.Pin{}
	for _, p := range pins {
		pinMap[p.Key()] = p
	}
	recs, stamp, err := r.Dump(context.Background(), cases, pinMap, bw5GoldIDs(cases), "")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if stamp.Records != len(cases) {
		t.Fatalf("dump holds %d records, want %d", stamp.Records, len(cases))
	}

	checked := 0
	for _, rec := range recs {
		want := rec.FusionOrder
		got := armsweep.FusedIDs(armsweep.Fuse(rec.Rows, armsweep.ConfigV0()))
		if len(want) == 0 {
			continue
		}
		if len(got) < len(want) {
			t.Fatalf("%s: offline fusion has %d rows, live order has %d — ctx_rrf_arms lost candidates",
				rec.Key(), len(got), len(want))
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("%s: position %d = %s, live fusion had %s\noffline %v\nlive    %v",
					rec.Key(), i, got[i], want[i], got[:min(len(got), 8)], want[:min(len(want), 8)])
			}
		}
		checked++
	}
	if checked == 0 {
		t.Fatal("no record carried a fusion order — the gate was vacuous")
	}
	t.Logf("gate (b): %d records, fusion order reproduced position for position", checked)

	// Negative control: a wrong configuration must NOT reproduce the live order.
	wrong := armsweep.ConfigV0()
	wrong.Weights = armsweep.Weights{
		Semantic: armsweep.LiveWeights.Trigram, FTSDe: armsweep.LiveWeights.FTSEn,
		FTSEn: armsweep.LiveWeights.FTSDe, Trigram: armsweep.LiveWeights.Semantic,
	}
	diverged := 0
	for _, rec := range recs {
		got := armsweep.FusedIDs(armsweep.Fuse(rec.Rows, wrong))
		for i := range rec.FusionOrder {
			if got[i] != rec.FusionOrder[i] {
				diverged++
				break
			}
		}
	}
	if diverged == 0 {
		t.Error("the reversed weight vector reproduced the live order on every record — " +
			"this fixture cannot distinguish configurations, so the parity assertion above proves nothing")
	}
	t.Logf("gate (b) control: reversed weights diverge on %d of %d records", diverged, len(recs))

	// Latency probe for gate (a). The number here is a testcontainer figure and
	// is NOT the live budget — that measurement needs the deployed seam.
	t.Logf("gate (a) probe (harness, NOT live): prime+dump p50 %d ms, p95 %d ms, max %d ms over %d queries",
		stamp.Latency.P50, stamp.Latency.P95, stamp.Latency.Max, stamp.Latency.N)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestBW5PinsAreRequired pins the no-silent-fallback rule: a dump with a missing
// pin must fail, not quietly run that one case unpinned.
func TestBW5PinsAreRequired(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, _ := rig.runner(t, "nopins")
	cases := rig.cases(t, gold)

	_, _, err := r.Dump(context.Background(), cases, map[string]armsweep.Pin{}, nil, "")
	if err == nil {
		t.Fatal("a dump without pins succeeded")
	}
	if !strings.Contains(err.Error(), "no pin for case") {
		t.Errorf("error = %v, want a missing-pin refusal", err)
	}
}

// TestBW5PinsSuppressTheLLMStages is the pins' reason for existing: a pinned
// dump must not pay a chat roundtrip, so two dumps of the same query set differ
// by corpus drift alone and not by a re-rolled translation.
func TestBW5PinsSuppressTheLLMStages(t *testing.T) {
	pool := bw2Setup(t)
	backend := bw2NewBackend(t)
	qh := bw2Handler(t, pool, backend)
	ar := &auth.AuthResult{ApiKeyID: bw2AdminKeyID, HomeScope: bw2Scope,
		ReadScopes: []string{bw2Scope}, IsValid: true, IsAdmin: true}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qh.HandleQuery(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}))
	defer srv.Close()

	cl := armsweep.NewClient(srv.URL, "harness", 30*time.Second)
	const german = "wie ist der status der datenbank"

	unpinned, err := cl.Measure(context.Background(),
		armsweep.QueryRequest{Query: german, Synthesize: false, ArmRanks: true})
	if err != nil {
		t.Fatalf("unpinned: %v", err)
	}
	if backend.chatHits.Load() == 0 {
		t.Fatal("control: the unpinned run paid no chat call — the assertion below would be vacuous")
	}
	backend.chatHits.Store(0)

	tr, tmp := unpinned.ArmRanks.EffectiveQuery, unpinned.ArmRanks.EffectiveTemporal
	if _, err := cl.Measure(context.Background(), armsweep.QueryRequest{
		Query: german, Synthesize: false, ArmRanks: true,
		PinnedTranslation: &tr, PinnedTemporal: &tmp,
	}); err != nil {
		t.Fatalf("pinned: %v", err)
	}
	if got := backend.chatHits.Load(); got != 0 {
		t.Errorf("pinned run made %d chat calls, want 0", got)
	}
}

// ---------------------------------------------------------------------------
// Gate (d): the drift negative probe
// ---------------------------------------------------------------------------

// TestBW5DriftSeesAnInsertMidDump is the first half of gate (d): a block
// inserted WHILE the dump runs must show up in the after-census.
//
// RED without the census (or with a census taken only once): the counts are
// identical and no note is emitted.
func TestBW5DriftSeesAnInsertMidDump(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, _ := rig.runner(t, "insert")
	cases := rig.cases(t, gold)
	pins := bw5Prime(t, r, cases)

	hook := func(n int32) {
		if n != 5 {
			return
		}
		if _, err := rig.pool.Exec(context.Background(),
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ('019fa500-0000-7000-9000-0000000009ff'::uuid, 'knowledge', 'mid dump insert',
			         'inserted while the sweep was running', $1, $2, 'knowledge')`,
			bw2Scope, pgvec.NewVector(bw2Embedding(4))); err != nil {
			t.Errorf("mid-dump insert: %v", err)
		}
	}
	// The counter is reset because priming already spent one query per case;
	// without this the hook's trigger point would lie in the past.
	rig.queries.Store(0)
	rig.onQuery.Store(&hook)

	_, stamp, err := r.Dump(context.Background(), cases, pins, bw5GoldIDs(cases), "")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	delta := stamp.After.RetrievableBlocks - stamp.Before.RetrievableBlocks
	if delta < 1 {
		t.Errorf("retrievable blocks %d → %d (delta %d), want at least +1",
			stamp.Before.RetrievableBlocks, stamp.After.RetrievableBlocks, delta)
	}
	t.Logf("gate (d) insert: retrievable %d → %d, verdict abort=%v reasons=%v notes=%v",
		stamp.Before.RetrievableBlocks, stamp.After.RetrievableBlocks,
		stamp.Drift.Abort, stamp.Drift.Reasons, stamp.Drift.Notes)
}

// TestBW5DriftAbortsOnAGoldMutation is the second half of gate (d): an UPDATE
// on a LABELLED block during the run discards the dump, and the file on disk
// is marked so nothing can score it by accident.
//
// RED without the gold-id section of the census: the run finishes clean and
// writes an ordinary .jsonl.
func TestBW5DriftAbortsOnAGoldMutation(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, dumps := rig.runner(t, "goldmut")
	cases := rig.cases(t, gold)
	pins := bw5Prime(t, r, cases)
	goldIDs := bw5GoldIDs(cases)

	hook := func(n int32) {
		if n != 5 {
			return
		}
		if _, err := rig.pool.Exec(context.Background(),
			`UPDATE context_blocks SET title = title || ' (touched)', updated_at = now() WHERE id = $1::uuid`,
			goldIDs[0]); err != nil {
			t.Errorf("mid-dump gold update: %v", err)
		}
	}
	rig.queries.Store(0)
	rig.onQuery.Store(&hook)

	recs, stamp, err := r.Dump(context.Background(), cases, pins, goldIDs, "")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if !stamp.Aborted {
		t.Fatalf("a gold-block update did not abort the dump: %+v", stamp.Drift)
	}
	if !strings.Contains(strings.Join(stamp.Drift.Reasons, " | "), goldIDs[0]) {
		t.Errorf("abort reasons do not name the mutated block: %v", stamp.Drift.Reasons)
	}
	t.Logf("gate (d) gold mutation: %v", stamp.Drift.Reasons)

	// The artefact must be marked, not deleted: an aborted run is the only
	// record of the drift that caused it.
	p, err := dumps.Resolve("goldmut.jsonl")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if err := armsweep.WriteRecords(p, recs); err != nil {
		t.Fatalf("write: %v", err)
	}
	marked, err := armsweep.MarkAborted(p)
	if err != nil {
		t.Fatalf("mark aborted: %v", err)
	}
	if !strings.HasSuffix(marked, ".aborted") {
		t.Errorf("marked path %q lacks the .aborted suffix", marked)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("the unmarked dump is still in place: %v", err)
	}
	if _, err := os.Stat(marked); err != nil {
		t.Errorf("the aborted dump was destroyed instead of marked: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Gate (f): the contamination probe
// ---------------------------------------------------------------------------

// TestBW5ContaminationProbeAborts is gate (f): a LABELLED block created after
// the gold stamp's corpus_max_created_at means the gold set and the corpus have
// diverged, and no run over them is a measurement of the set that was drawn.
//
// RED without the created_at column in the census: the run is clean.
func TestBW5ContaminationProbeAborts(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, _ := rig.runner(t, "contam")
	cases := rig.cases(t, gold)
	pins := bw5Prime(t, r, cases)
	goldIDs := bw5GoldIDs(cases)

	// The stamp is BEFORE the fixture rows were inserted, so every gold block
	// is younger than the draw — exactly the divergence the probe names.
	drawnAt := time.Now().UTC().Add(-24 * time.Hour).Format(time.RFC3339Nano)

	_, stamp, err := r.Dump(context.Background(), cases, pins, goldIDs, drawnAt)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if !stamp.Aborted {
		t.Fatalf("contamination not caught: %+v", stamp.Drift)
	}
	joined := strings.Join(stamp.Drift.Reasons, " | ")
	if !strings.Contains(joined, "contamination") {
		t.Errorf("abort reasons do not name the contamination rule: %v", stamp.Drift.Reasons)
	}
	t.Logf("gate (f): %s", joined)

	// Control: with a stamp AFTER the fixtures, the same run is clean — proof
	// the probe compares against the stamp and does not simply always fire.
	clean := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	_, ok, err := r.Dump(context.Background(), cases, pins, goldIDs, clean)
	if err != nil {
		t.Fatalf("control dump: %v", err)
	}
	if ok.Aborted {
		t.Errorf("control run aborted although the stamp postdates the corpus: %v", ok.Drift.Reasons)
	}
}

// ---------------------------------------------------------------------------
// Gate (g): the retry budget
// ---------------------------------------------------------------------------

// TestBW5RetryBudget is gate (g): two retries are spent, a third failure
// excludes the case — listed, never replaced.
//
// RED without a budget: the first 500 aborts the whole run.
func TestBW5RetryBudget(t *testing.T) {
	rig := bw5NewRig(t)
	r, gold, _ := rig.runner(t, "retry")
	cases := rig.cases(t, gold)[:4]
	pins := bw5Prime(t, r, cases)

	// Two failures, then success: the case must survive, with attempts == 3.
	rig.failNext.Store(2)
	recs, stamp, err := r.Dump(context.Background(), cases, pins, nil, "")
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	if len(stamp.Excluded) != 0 {
		t.Errorf("a case recoverable within budget was excluded: %+v", stamp.Excluded)
	}
	attempts := 0
	for _, rec := range recs {
		if rec.Attempts > attempts {
			attempts = rec.Attempts
		}
	}
	if attempts != 3 {
		t.Errorf("max attempts = %d, want 3 (1 try + 2 retries)", attempts)
	}
	t.Logf("gate (g) within budget: %d records, max attempts %d, 0 exclusions", len(recs), attempts)

	// Three failures: the budget is exhausted and the case leaves the dump.
	rig.failNext.Store(3)
	recs2, stamp2, err := r.Dump(context.Background(), cases, pins, nil, "")
	if err != nil {
		t.Fatalf("dump 2: %v", err)
	}
	if len(stamp2.Excluded) != 1 {
		t.Fatalf("%d exclusions, want exactly 1: %+v", len(stamp2.Excluded), stamp2.Excluded)
	}
	if stamp2.Excluded[0].Attempts != 3 {
		t.Errorf("exclusion records %d attempts, want 3", stamp2.Excluded[0].Attempts)
	}
	if len(recs2) != len(cases)-1 {
		t.Errorf("dump holds %d records, want %d — an excluded case must not be replaced", len(recs2), len(cases)-1)
	}
	t.Logf("gate (g) budget exhausted: excluded %s/%d after %d attempts (%s)",
		stamp2.Excluded[0].Slice, stamp2.Excluded[0].Index, stamp2.Excluded[0].Attempts, stamp2.Excluded[0].Reason)

	// The exclusion union over the dump pair: one clean dump, one with the
	// exclusion ⇒ the report drops the case from BOTH.
	body, serr := armsweep.Score(armsweep.ScoreInput{
		RecordsA: recs, StampA: stamp, RecordsB: recs2, StampB: &stamp2, Seed: 20260812,
	})
	if serr != nil {
		t.Fatalf("score: %v", serr)
	}
	if len(body.Excluded) != 1 {
		t.Errorf("report lists %d exclusions, want the union of 1: %+v", len(body.Excluded), body.Excluded)
	}
	for _, s := range body.Slices {
		if s.N != len(cases)-1 {
			t.Errorf("slice %s kept %d cases, want %d after the union exclusion", s.Slice, s.N, len(cases)-1)
		}
	}
}

// TestBW5GateRefusalIsFatal pins the other half of the retry classification: a
// 4xx from the seam is a configuration error and must stop the run at once,
// not burn the budget three times per query on 650 queries.
func TestBW5GateRefusalIsFatal(t *testing.T) {
	pool := bw2Setup(t)
	qh := bw2Handler(t, pool, bw2NewBackend(t))
	// A NON-admin principal: the seam answers 403.
	ar := &auth.AuthResult{ApiKeyID: bw2UserKeyID, HomeScope: bw2Scope,
		ReadScopes: []string{bw2Scope}, IsValid: true, IsAdmin: false}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		qh.HandleQuery(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}))
	defer srv.Close()

	cl := armsweep.NewClient(srv.URL, "harness", 30*time.Second)
	_, err := cl.Measure(context.Background(),
		armsweep.QueryRequest{Query: bw2GoldenQuery, Synthesize: false, ArmRanks: true})
	if err == nil {
		t.Fatal("a non-admin measurement request succeeded")
	}
	if !strings.Contains(err.Error(), "admin") {
		t.Errorf("error = %v, want the admin refusal", err)
	}
}

// ---------------------------------------------------------------------------
// The additive drift section of /api/manage stats
// ---------------------------------------------------------------------------

// TestBW5DriftSectionIsAdditiveAndAdminOnly pins the three properties the
// server-side change was allowed under: opt-in, admin-only, and byte-identical
// for everyone who does not ask.
func TestBW5DriftSectionIsAdditiveAndAdminOnly(t *testing.T) {
	pool := bw2Setup(t)
	mh := NewManageHandler(pool, config.NewStore(bw2Config()), nil, nil, nil, nil, nil, blocktype.NewRegistry())

	do := func(admin bool, body string) (*httptest.ResponseRecorder, map[string]any) {
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		ar := &auth.AuthResult{ApiKeyID: bw2AdminKeyID, HomeScope: bw2Scope,
			ReadScopes: []string{bw2Scope}, IsValid: true, IsAdmin: admin}
		rec := httptest.NewRecorder()
		mh.HandleManage(rec, req.WithContext(context.WithValue(req.Context(), authResultKey, ar)))
		m := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &m)
		return rec, m
	}

	plainAdmin, m1 := do(true, `{"action":"stats"}`)
	if plainAdmin.Code != http.StatusOK {
		t.Fatalf("plain stats: %d %s", plainAdmin.Code, plainAdmin.Body.String())
	}
	if _, ok := m1["drift"]; ok {
		t.Error("a stats request that did not ask carries a drift section")
	}
	plainUser, _ := do(false, `{"action":"stats"}`)
	if plainUser.Body.String() != plainAdmin.Body.String() {
		t.Error("plain stats is not identical for admin and non-admin")
	}

	refused, _ := do(false, `{"action":"stats","data":{"drift":true}}`)
	if refused.Code != http.StatusForbidden {
		t.Errorf("non-admin drift request: %d, want 403", refused.Code)
	}

	ok, m2 := do(true, `{"action":"stats","data":{"drift":true,"gold_ids":["019fa500-0000-7000-9000-000000000100"]}}`)
	if ok.Code != http.StatusOK {
		t.Fatalf("admin drift request: %d %s", ok.Code, ok.Body.String())
	}
	drift, present := m2["drift"].(map[string]any)
	if !present {
		t.Fatalf("no drift section in an admin request: %s", ok.Body.String())
	}
	for _, k := range []string{"at", "retrievable_blocks", "types", "gold_ids"} {
		if _, has := drift[k]; !has {
			t.Errorf("drift section lacks %q: %v", k, drift)
		}
	}
	types, _ := drift["types"].([]any)
	if len(types) == 0 {
		t.Fatal("drift section reports no types")
	}
	first, _ := types[0].(map[string]any)
	for _, k := range []string{"type_name", "retrievable", "count", "max_created_at", "max_updated_at", "null_embedding"} {
		if _, has := first[k]; !has {
			t.Errorf("type census lacks %q: %v", k, first)
		}
	}
	golds, _ := drift["gold_ids"].([]any)
	if len(golds) != 1 {
		t.Errorf("gold census returned %d rows, want 1: %v", len(golds), golds)
	}
}

// bw5Prime runs the priming pass and returns the pins keyed for a dump.
func bw5Prime(t *testing.T, r *armsweep.Runner, cases []goldset.Case) map[string]armsweep.Pin {
	t.Helper()
	pins, _, stamp, err := r.Prime(context.Background(), cases)
	if err != nil {
		t.Fatalf("prime: %v", err)
	}
	if len(pins) != len(cases) {
		t.Fatalf("prime produced %d pins for %d cases (excluded %+v)", len(pins), len(cases), stamp.Excluded)
	}
	out := map[string]armsweep.Pin{}
	for _, p := range pins {
		out[p.Key()] = p
	}
	return out
}
