//go:build integration

// Migration 159 gates (W06-3, design/06 §3.2 M-06-2, §7 W06-3):
// context_llm_usage_hourly is an hourly rollup of every context_llm_log row,
// kept by the AFTER INSERT trigger trg_llm_usage_rollup and backfilled once
// by the migration. Gate letters follow design/06 §7 W06-3:
//
//	(a) parity: 1 000 random seeds written raw and through llmlog.Record,
//	    before 159 (backfill path) and after it (trigger path) ⇒ the rollup
//	    equals a GROUP BY over the raw rows, computed here in Go
//	(b) two key-less rows of one dimension ⇒ one rollup row, calls = 2
//	(c) histogram: p95 from the buckets within one bucket width of
//	    percentile_cont(0.95); the upsert adds the arrays element by element
//	(d) totality: every metadata.gw form is accepted, hop and ttft clamped,
//	    dimension texts cut to 100 characters below the B-tree entry limit
//	(e) 159 applied twice ⇒ identical catalog and rollup rows
//	(f) EvictBodies leaves the rollup untouched
//	(g) load: p99 insert latency with the trigger within 2 ms of the
//	    baseline, HOT-update ratio >= 0.9 (opt-in, CTX_M159_LOAD_INSERTS)
//
// CTX_M159_OVERLAY=<name> applies a mutated 159 instead of the real file —
// that is how each gate is shown red: nulls-distinct (b), array-plus (c),
// naive-cast (d), numeric-18-8 (d, cost sum), fillfactor-100 (g).
// TestMigration159BackfillProbe times the backfill on a copy of production
// shape (opt-in, CTX_M159_PROBE_ROWS).
//
//	go test -tags=integration -p 1 ./internal/llmlog/ -run 'UsageRollup|Migration159' -count=1 -v
//	CTX_M159_LOAD_INSERTS=50000 go test -tags=integration -p 1 ./internal/llmlog/ \
//	  -run TestUsageRollupLoad -count=1 -v -timeout 30m
//	CTX_M159_PROBE_ROWS=1000000 go test -tags=integration -p 1 ./internal/llmlog/ \
//	  -run TestMigration159BackfillProbe -count=1 -v -timeout 40m
package llmlog_test

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"math/big"
	"math/bits"
	"math/rand/v2"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/migrations"
)

const (
	m159File    = "159_llm_usage_rollup.sql"
	m159Version = 159

	// The bucket contract of design/05 §3.5: 16 log2 buckets over a 60 ms
	// base; bucket i (0-based) holds ms <= 60·2^i for i < 15, bucket 15 is
	// the overflow.
	histBuckets = 16
	histBaseMs  = 60

	dimMaxRunes = 100                   // left(<text dimension>, 100) on all six
	hopMax      = 16                    // metadata.gw.hop clamped to 0..16
	ttftMaxMs   = 9_000_000_000_000_000 // metadata.gw.ttft_ms clamped to 0..9e15

	// loadWriters is gate (g)'s concurrency; loadP99Slack its H threshold;
	// loadMinInserts the smallest run that may stand for the gate.
	loadWriters    = 8
	loadP99Slack   = 2 * time.Millisecond
	loadHOTFloor   = 0.9
	loadMinInserts = 10_000
)

// m159Overlays are the mutated variants of 159 that prove the gates red:
// CTX_M159_OVERLAY=<name> applies the real file with these replacements. A
// replacement that no longer matches fails the test, so an overlay can never
// fall back to the real migration unnoticed.
var m159Overlays = map[string][][2]string{
	// (b) a unique index without NULLS NOT DISTINCT: key-less rows never conflict.
	"nulls-distinct": {{"    NULLS NOT DISTINCT;", ";"}},
	// (c) the upsert adds the arrays with + (bigint[] + bigint[] does not exist).
	"array-plus": {
		{"hist_add(u.latency_hist, EXCLUDED.latency_hist)", "u.latency_hist + EXCLUDED.latency_hist"},
		{"hist_add(u.queue_wait_hist, EXCLUDED.queue_wait_hist)", "u.queue_wait_hist + EXCLUDED.queue_wait_hist"},
		{"hist_add(u.ttft_hist, EXCLUDED.ttft_hist)", "u.ttft_hist + EXCLUDED.ttft_hist"},
	},
	// (d) the trigger casts metadata.gw.hop / ttft_ms without type guard or clamp.
	"naive-cast": {
		{"jsonb_typeof(v_gw->'hop') = 'number'", "v_gw ? 'hop'"},
		{"LEAST(GREATEST(floor((v_gw->>'hop')::numeric), 0), 16)::smallint", "(v_gw->>'hop')::smallint"},
		{"jsonb_typeof(v_gw->'ttft_ms') = 'number'", "v_gw ? 'ttft_ms'"},
		{"LEAST(GREATEST(floor((v_gw->>'ttft_ms')::numeric), 0), 9e15)::bigint", "(v_gw->>'ttft_ms')::bigint"},
	},
	// (d) the cost column of the design draft: a bounded NUMERIC(18,8).
	"numeric-18-8": {{"cost_usd          NUMERIC NOT NULL", "cost_usd          NUMERIC(18,8) NOT NULL"}},
	// (g) the rollup without room for HOT updates.
	"fillfactor-100": {{"fillfactor = 50", "fillfactor = 100"}},
}

// m159Body returns the body of 159 to apply and the overlay applied to it
// ("" for the real file). Without the file — the tree before this wave — it
// returns "", so every gate meets the missing table (42P01) rather than a
// read error.
func m159Body(t *testing.T) (body, overlay string) {
	t.Helper()
	raw, err := migrations.FS.ReadFile(m159File)
	if errors.Is(err, fs.ErrNotExist) {
		t.Logf("%s is not in the tree — nothing to apply", m159File)
		return "", ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", m159File, err)
	}
	overlay = os.Getenv("CTX_M159_OVERLAY")
	if overlay == "" {
		return string(raw), ""
	}
	reps, ok := m159Overlays[overlay]
	if !ok {
		names := make([]string, 0, len(m159Overlays))
		for n := range m159Overlays {
			names = append(names, n)
		}
		sort.Strings(names)
		t.Fatalf("CTX_M159_OVERLAY=%q: unknown overlay, have %v", overlay, names)
	}
	body = string(raw)
	for _, r := range reps {
		if !strings.Contains(body, r[0]) {
			t.Fatalf("overlay %s: %q is not in %s any more", overlay, r[0], m159File)
		}
		body = strings.ReplaceAll(body, r[0], r[1])
	}
	t.Logf("CTX_M159_OVERLAY=%s: 159 applied with %d replacement(s)", overlay, len(reps))
	return body, overlay
}

// applyM159 brings a database at 158 to 159 — through the boot runner, or,
// under an overlay, by executing the mutated body in one transaction as the
// runner would.
func applyM159(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	body, overlay := m159Body(t)
	switch {
	case body == "":
	case overlay == "":
		if err := store.RunMigrationsUpTo(ctx, pool, m159Version); err != nil {
			t.Fatalf("apply 159: %v", err)
		}
	default:
		execInTx(ctx, t, pool, body)
	}
}

func execInTx(ctx context.Context, t *testing.T, pool *pgxpool.Pool, sql string) {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, sql); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("exec in one transaction: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// setupRollupDB returns a database at 159. seed runs at 158, before the
// migration, so its rows reach the rollup through the backfill.
func setupRollupDB(t *testing.T, seed func(context.Context, *pgxpool.Pool)) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDBUpTo(t, m159Version-1)
	ctx := context.Background()
	if seed != nil {
		seed(ctx, pool)
	}
	applyM159(ctx, t, pool)
	return pool
}

// genRow is one synthetic context_llm_log row. Zero values mean NULL, as on
// the Go write path (Record stores 0 tokens and "" strings as NULL).
type genRow struct {
	at                 time.Time // raw path only; Record rows get now()
	key, consumer      string
	pipeline           string
	backend, host      string
	model              string
	dclass, loc        string
	errText            string
	prompt, completion int
	durMs, qwMs        *int64
	cost               string // decimal text
	metadata           string // JSON text
}

const rawInsertSQL = `
	INSERT INTO context_llm_log
	    (created_at, pipeline, model, host, duration_ms, error, prompt_tokens, completion_tokens,
	     metadata, backend_name, backend_locality, cost_usd, api_key_id, queue_wait_ms,
	     dispatch_class, consumer_id)
	VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::text::jsonb, $10, $11, $12::text::numeric,
	        $13::text::uuid, $14, $15, $16::text::uuid)`

func nz(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nzInt(v int) *int {
	if v == 0 {
		return nil
	}
	return &v
}

func (r genRow) args() []any {
	return []any{r.at, r.pipeline, r.model, r.host, r.durMs, nz(r.errText), nzInt(r.prompt),
		nzInt(r.completion), r.metadata, nz(r.backend), nz(r.loc), nz(r.cost), nz(r.key),
		r.qwMs, nz(r.dclass), nz(r.consumer)}
}

// insertRaw writes rows in one batch (one implicit transaction).
func insertRaw(ctx context.Context, t *testing.T, pool *pgxpool.Pool, rows []genRow) {
	t.Helper()
	if err := insertRawErr(ctx, pool, rows); err != nil {
		t.Fatalf("insert %d raw row(s) (%s …): %v", len(rows), rows[0].pipeline, err)
	}
}

func insertRawErr(ctx context.Context, pool *pgxpool.Pool, rows []genRow) error {
	b := &pgx.Batch{}
	for _, r := range rows {
		b.Queue(rawInsertSQL, r.args()...)
	}
	return pool.SendBatch(ctx, b).Close()
}

// entry is the row as the daemon writes it. metadata must be a JSON object;
// its values travel as json.RawMessage, so forms like 1e400 arrive verbatim.
func (r genRow) entry(t *testing.T) llmlog.Entry {
	t.Helper()
	e := llmlog.Entry{
		Pipeline: r.pipeline, Model: r.model, Host: r.host,
		PromptTokens: r.prompt, CompletionTokens: r.completion,
		BackendName: r.backend, BackendLocality: r.loc, DispatchClass: r.dclass,
		APIKeyID: r.key, ConsumerID: r.consumer, QueueWaitMs: r.qwMs,
	}
	if r.durMs == nil {
		e.NoWireCall = true
	} else {
		e.Duration = time.Duration(*r.durMs) * time.Millisecond
	}
	if r.errText != "" {
		e.Err = errors.New(r.errText)
	}
	if r.cost != "" {
		f, err := strconv.ParseFloat(r.cost, 64)
		if err != nil {
			t.Fatalf("cost %q: %v", r.cost, err)
		}
		e.CostUSD = &f
	}
	var md map[string]json.RawMessage
	if err := json.Unmarshal([]byte(r.metadata), &md); err != nil {
		t.Fatalf("Record metadata %s: %v", r.metadata, err)
	}
	e.Metadata = make(map[string]any, len(md))
	for k, v := range md {
		e.Metadata[k] = v
	}
	return e
}

var (
	rollKeys = []string{
		"0195a6f0-0000-7000-8000-00000000a159",
		"0195a6f0-0000-7000-8000-00000000a160",
		"0195a6f0-0000-7000-8000-00000000a161",
	}
	rollConsumers = []string{
		"0195a6f0-0000-7000-8000-00000000c159",
		"0195a6f0-0000-7000-8000-00000000c160",
	}

	// Valid and invalid forms of the metadata.gw fields the rollup reads.
	gwAliasForms = []string{`"chat"`, `"embed-large"`, `"` + strings.Repeat("a", 200) + `"`,
		`"` + strings.Repeat("ü", 150) + `"`, `7`, `null`, `["chat"]`, `{"n": "chat"}`, `true`}
	gwHopForms = []string{`0`, `1`, `2`, `3.7`, `-5`, `16`, `17`, `1e400`, `-1e400`,
		`"x"`, `"7"`, `null`, `[]`, `{}`, `true`}
	gwUsageForms = []string{`"estimated"`, `"reported"`, `null`, `1`, `["estimated"]`}
	gwTTFTForms  = []string{`0`, `59`, `60`, `61`, `120`, `450.5`, `983040`, `983041`,
		`1e400`, `-5`, `"x"`, `"7"`, `null`, `[]`, `{}`}
	gwNotObjects = []string{`5`, `"s"`, `[1]`, `null`}

	// Dimension texts past the 100-character cut: two models that differ
	// only after it (they share one rollup row) and a dispatch class of
	// four-byte characters.
	longModelA   = strings.Repeat("µ", dimMaxRunes) + "-a"
	longModelB   = strings.Repeat("µ", dimMaxRunes) + "-b"
	longDClass   = strings.Repeat("😀", dimMaxRunes+20)
	mdNotObjects = []string{`null`, `[]`, `"s"`, `5`}
)

func pick[T any](rng *rand.Rand, xs ...T) T { return xs[rng.IntN(len(xs))] }

// genScenario draws one seed's rows: 1–30 rows over three hours before base,
// two pipelines named after the seed, three knowledge keys + two consumers +
// nobody, errors, external costs, NULL durations and waits, and metadata.gw
// in valid and invalid forms. recordable keeps metadata a JSON object (the
// only form llmlog.Record can write); stream varies the draw for one seed.
func genScenario(seed, stream uint64, base time.Time, recordable bool) []genRow {
	rng := rand.New(rand.NewPCG(seed, stream))
	rows := make([]genRow, 1+rng.IntN(30))
	for i := range rows {
		r := genRow{
			at:       base.Add(-time.Duration(rng.IntN(3*3600)) * time.Second),
			pipeline: fmt.Sprintf("m159-%04d-%d", seed, rng.IntN(2)),
			backend:  pick(rng, "", "b1", "b2"),
			host:     pick(rng, "h1", "h2"),
			model:    pick(rng, "m1", "m2", longModelA, longModelB),
			dclass:   pick(rng, "", "interactive", "background", longDClass),
			loc:      pick(rng, "", "local", "external"),
		}
		switch k := rng.IntN(6); {
		case k < 3:
			r.key = rollKeys[k]
		case k < 5:
			r.consumer = rollConsumers[k-3]
		}
		if rng.IntN(5) == 0 {
			r.errText = "upstream 502"
		}
		if rng.IntN(4) > 0 {
			r.prompt = rng.IntN(4000)
		}
		if rng.IntN(4) > 0 {
			r.completion = rng.IntN(1000)
		}
		if rng.IntN(7) > 0 {
			v := int64(math.Exp(14 * rng.Float64()))
			r.durMs = &v
		}
		if rng.IntN(3) > 0 {
			v := int64(rng.IntN(120_000))
			r.qwMs = &v
		}
		if r.loc == "external" && rng.IntN(2) == 0 {
			r.cost = fmt.Sprintf("%d.%08d", rng.IntN(3), rng.IntN(100_000_000))
		}
		r.metadata = genMetadata(rng, recordable)
		rows[i] = r
	}
	return rows
}

func genMetadata(rng *rand.Rand, recordable bool) string {
	switch rng.IntN(10) {
	case 0:
		return `{}`
	case 1:
		if recordable {
			return `{"note": "no gw"}`
		}
		return pick(rng, mdNotObjects...)
	case 2:
		return `{"gw": ` + pick(rng, gwNotObjects...) + `}`
	}
	var f []string
	if rng.IntN(5) < 3 {
		f = append(f, `"alias": `+pick(rng, gwAliasForms...))
	}
	if rng.IntN(5) < 3 {
		f = append(f, `"hop": `+pick(rng, gwHopForms...))
	}
	if rng.IntN(5) < 2 {
		f = append(f, `"usage": `+pick(rng, gwUsageForms...))
	}
	if rng.IntN(5) < 3 {
		f = append(f, `"ttft_ms": `+pick(rng, gwTTFTForms...))
	}
	return `{"note": "x", "gw": {` + strings.Join(f, ", ") + `}}`
}

// histIdx is the oracle's bucket (0-based), written independently of 159's
// log formula: for ms > 60, ceil(log2(ms/60)) is the bit length of
// floor((ms-1)/60), because 60·2^k is a whole number of milliseconds.
func histIdx(ms int64) int {
	if ms < 1 {
		ms = 1
	}
	return min(bits.Len64(uint64((ms-1)/histBaseMs)), histBuckets-1)
}

// bucketWidth is the width of bucket i in ms; the overflow bucket has none.
func bucketWidth(i int) float64 {
	switch {
	case i == 0:
		return histBaseMs
	case i < histBuckets-1:
		return float64(int64(histBaseMs) << (i - 1))
	}
	return math.Inf(1)
}

// histQuantile is the reader-side estimate design/05 §3.5 describes (W06-4
// builds the production one): find the bucket the rank falls into and
// interpolate inside it, linearly in bucket 0 and geometrically above.
func histQuantile(h []int64, q float64) float64 {
	var n int64
	for _, c := range h {
		n += c
	}
	if n == 0 {
		return math.NaN()
	}
	rank, cum := q*float64(n), 0.0
	for i, c := range h {
		if c == 0 {
			continue
		}
		if cum+float64(c) >= rank {
			f := (rank - cum) / float64(c)
			if i == 0 {
				return f * histBaseMs
			}
			lo := float64(int64(histBaseMs) << (i - 1))
			if i == histBuckets-1 {
				return lo
			}
			return lo * math.Exp2(f)
		}
		cum += float64(c)
	}
	return float64(int64(histBaseMs) << (histBuckets - 2))
}

// jsonType mirrors jsonb_typeof for one JSON value.
func jsonType(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	switch s[0] {
	case '{':
		return "object"
	case '[':
		return "array"
	case '"':
		return "string"
	case 't', 'f':
		return "boolean"
	case 'n':
		return "null"
	}
	return "number"
}

// gwObject returns metadata.gw when metadata is an object and gw is one.
func gwObject(metadata string) map[string]json.RawMessage {
	var top map[string]json.RawMessage
	if json.Unmarshal([]byte(metadata), &top) != nil {
		return nil
	}
	raw, ok := top["gw"]
	if !ok || jsonType(raw) != "object" {
		return nil
	}
	var gw map[string]json.RawMessage
	if json.Unmarshal(raw, &gw) != nil {
		return nil
	}
	return gw
}

func gwAlias(gw map[string]json.RawMessage) string {
	raw, ok := gw["alias"]
	if !ok || jsonType(raw) != "string" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return capDim(s)
}

// capDim is left(s, 100): the cut 159 applies to every text dimension, so
// that no unique-index entry outgrows the B-tree limit.
func capDim(s string) string {
	if r := []rune(s); len(r) > dimMaxRunes {
		return string(r[:dimMaxRunes])
	}
	return s
}

// gwFloorClamp is floor(value) clamped to [lo, hi] for a JSON number; false
// for every other form. big.Rat reads any JSON number exactly (1e400 too).
func gwFloorClamp(gw map[string]json.RawMessage, field string, lo, hi int64) (int64, bool) {
	raw, ok := gw[field]
	if !ok || jsonType(raw) != "number" {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(strings.TrimSpace(string(raw)))
	if !ok {
		return 0, false
	}
	f := new(big.Int).Div(r.Num(), r.Denom()) // Euclidean, denominator > 0 ⇒ floor
	switch {
	case f.Cmp(big.NewInt(lo)) < 0:
		return lo, true
	case f.Cmp(big.NewInt(hi)) > 0:
		return hi, true
	}
	return f.Int64(), true
}

func gwHop(gw map[string]json.RawMessage) int16 {
	h, _ := gwFloorClamp(gw, "hop", 0, hopMax)
	return int16(h)
}

func gwEstimated(gw map[string]json.RawMessage) bool {
	raw, ok := gw["usage"]
	if !ok || jsonType(raw) != "string" {
		return false
	}
	var s string
	return json.Unmarshal(raw, &s) == nil && s == "estimated"
}

// rollKey is one rollup dimension; "" stands for NULL in key and consumer.
type rollKey struct {
	bucket                   int64
	key, consumer            string
	pipeline, backend, model string
	alias, dclass, loc       string
	hop                      int16
}

type rollVal struct {
	calls, errors, unattributed, estimated int64
	prompt, completion                     int64
	durSum, durN, qwSum, qwN               int64
	cost                                   *big.Rat
	lat, qw, ttft                          [histBuckets]int64
	dup                                    int // extra rollup rows with this key
}

func (v *rollVal) String() string {
	return fmt.Sprintf("calls=%d errors=%d unattributed=%d estimated=%d prompt=%d completion=%d "+
		"dur=%d/%d qw=%d/%d cost=%s lat=%v qw=%v ttft=%v dup=%d",
		v.calls, v.errors, v.unattributed, v.estimated, v.prompt, v.completion,
		v.durSum, v.durN, v.qwSum, v.qwN, v.cost.RatString(), v.lat, v.qw, v.ttft, v.dup)
}

func valOr[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// expectedRollup is the GROUP BY over the raw rows, in Go: the oracle the
// trigger and the backfill are held against. It reads the rows back from the
// table, so it sees exactly what both writers stored.
func expectedRollup(ctx context.Context, t *testing.T, q rowQuerier) (map[rollKey]*rollVal, int) {
	t.Helper()
	rows, err := q.Query(ctx, `
		SELECT created_at, api_key_id::text, consumer_id::text, pipeline, backend_name, host,
		       model, dispatch_class, backend_locality, metadata::text, error IS NOT NULL,
		       prompt_tokens, completion_tokens, duration_ms, queue_wait_ms, cost_usd::text
		  FROM context_llm_log`)
	if err != nil {
		t.Fatalf("read raw rows: %v", err)
	}
	defer rows.Close()
	want := map[rollKey]*rollVal{}
	n := 0
	for rows.Next() {
		var at time.Time
		var key, consumer, backend, dclass, loc, cost *string
		var pipeline, host, model, md string
		var isErr bool
		var prompt, completion, dur, qw *int64
		if err := rows.Scan(&at, &key, &consumer, &pipeline, &backend, &host, &model, &dclass,
			&loc, &md, &isErr, &prompt, &completion, &dur, &qw, &cost); err != nil {
			t.Fatalf("scan raw row: %v", err)
		}
		n++
		gw := gwObject(md)
		k := rollKey{
			bucket: at.UTC().Truncate(time.Hour).Unix(), consumer: valOr(consumer),
			pipeline: capDim(pipeline), backend: capDim(host), model: capDim(model), alias: gwAlias(gw),
			dclass: capDim(valOr(dclass)), loc: capDim(valOr(loc)), hop: gwHop(gw),
		}
		if consumer == nil { // the consumer wins (01 B17)
			k.key = valOr(key)
		}
		if backend != nil {
			k.backend = capDim(*backend)
		}
		v := want[k]
		if v == nil {
			v = &rollVal{cost: new(big.Rat)}
			want[k] = v
		}
		v.calls++
		switch {
		case isErr:
			v.errors++
		case backend == nil:
			v.unattributed++
		}
		if gwEstimated(gw) {
			v.estimated++
		}
		v.prompt += valOr(prompt)
		v.completion += valOr(completion)
		if dur != nil {
			v.durSum += *dur
			v.durN++
			v.lat[histIdx(*dur)]++
		}
		if qw != nil {
			v.qwSum += *qw
			v.qwN++
			v.qw[histIdx(*qw)]++
		}
		if ttft, ok := gwFloorClamp(gw, "ttft_ms", 0, ttftMaxMs); ok {
			v.ttft[histIdx(ttft)]++
		}
		if cost != nil {
			c, ok := new(big.Rat).SetString(*cost)
			if !ok {
				t.Fatalf("cost %q", *cost)
			}
			v.cost.Add(v.cost, c)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("raw rows: %v", err)
	}
	return want, n
}

// readRollup reads context_llm_usage_hourly into the oracle's shape. Two
// rows with one key (NULL-distinct index) are counted in dup.
func readRollup(ctx context.Context, t *testing.T, q rowQuerier) map[rollKey]*rollVal {
	t.Helper()
	rows, err := q.Query(ctx, `
		SELECT bucket_start, api_key_id::text, consumer_id::text, source, pipeline, backend_name,
		       model, model_alias, dispatch_class, backend_locality, hop,
		       calls, errors, unattributed_ok, estimated_calls, prompt_tokens, completion_tokens,
		       duration_ms_sum, duration_n, queue_wait_ms_sum, queue_wait_n, cost_usd::text,
		       latency_hist, queue_wait_hist, ttft_hist
		  FROM context_llm_usage_hourly`)
	if err != nil {
		t.Fatalf("read rollup: %v", err)
	}
	defer rows.Close()
	got := map[rollKey]*rollVal{}
	for rows.Next() {
		var k rollKey
		var bucket time.Time
		var key, consumer *string
		var source, cost string
		v := &rollVal{}
		var lat, qw, ttft []int64
		if err := rows.Scan(&bucket, &key, &consumer, &source, &k.pipeline, &k.backend, &k.model,
			&k.alias, &k.dclass, &k.loc, &k.hop, &v.calls, &v.errors, &v.unattributed,
			&v.estimated, &v.prompt, &v.completion, &v.durSum, &v.durN, &v.qwSum, &v.qwN,
			&cost, &lat, &qw, &ttft); err != nil {
			t.Fatalf("scan rollup row: %v", err)
		}
		k.bucket, k.key, k.consumer = bucket.Unix(), valOr(key), valOr(consumer)
		if wantSource := map[bool]string{true: "internal", false: "gateway"}[consumer == nil]; source != wantSource {
			t.Errorf("rollup row %+v: source %q, want %q", k, source, wantSource)
		}
		var ok bool
		if v.cost, ok = new(big.Rat).SetString(cost); !ok {
			t.Fatalf("rollup cost %q", cost)
		}
		for _, h := range []struct {
			dst *[histBuckets]int64
			src []int64
		}{{&v.lat, lat}, {&v.qw, qw}, {&v.ttft, ttft}} {
			if len(h.src) != histBuckets {
				t.Fatalf("rollup row %+v: histogram with %d buckets, want %d", k, len(h.src), histBuckets)
			}
			copy(h.dst[:], h.src)
		}
		if prev := got[k]; prev != nil {
			prev.dup++
			continue
		}
		got[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup rows: %v", err)
	}
	return got
}

// assertParity holds the rollup against the GROUP BY over the raw rows.
func assertParity(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	want, raw := expectedRollup(ctx, t, pool)
	got := readRollup(ctx, t, pool)
	var diffs []string
	for k, w := range want {
		g := got[k]
		switch {
		case g == nil:
			diffs = append(diffs, fmt.Sprintf("missing %+v: want %s", k, w))
		case g.String() != w.String():
			diffs = append(diffs, fmt.Sprintf("%+v:\n      got  %s\n      want %s", k, g, w))
		}
	}
	for k, g := range got {
		if want[k] == nil {
			diffs = append(diffs, fmt.Sprintf("extra %+v: %s", k, g))
		}
	}
	if len(diffs) > 0 {
		sort.Strings(diffs)
		t.Fatalf("%d raw rows, %d dimensions, rollup %d keys: %d differ, first:\n  %s",
			raw, len(want), len(got), len(diffs), strings.Join(diffs[:min(5, len(diffs))], "\n  "))
	}
	t.Logf("parity: %d raw rows ⇒ %d dimensions, rollup equal", raw, len(want))
}

func countRows(ctx context.Context, t *testing.T, q rowQuerier, sql string, args ...any) int64 {
	t.Helper()
	var n int64
	if err := q.QueryRow(ctx, sql, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", strings.TrimSpace(sql), err)
	}
	return n
}

// TestUsageRollupParity is gate (a). Seeds 0–499 are written raw before 159
// (the backfill counts them), 500–749 raw and 750–999 through llmlog.Record
// after it (the trigger counts them), and seeds 0–99 get a second draw after
// 159, so the trigger adds onto rows the backfill wrote. The rollup must
// equal the GROUP BY over the raw rows; then the rollup is emptied and the
// migration body runs again — the pure backfill path must equal it too.
func TestUsageRollupParity(t *testing.T) {
	const seeds, backfilled, recordFrom, revisited = 1000, 500, 750, 100
	base := time.Now().UTC()
	written := 0
	pool := setupRollupDB(t, func(ctx context.Context, pool *pgxpool.Pool) {
		for s := range uint64(backfilled) {
			rows := genScenario(s, 1, base, false)
			insertRaw(ctx, t, pool, rows)
			written += len(rows)
		}
	})
	ctx := context.Background()

	failures, dropped := llmlog.InsertFailures(), llmlog.InsertDropped()
	for s := uint64(backfilled); s < seeds; s++ {
		rows := genScenario(s, 1, base, s >= recordFrom)
		written += len(rows)
		if s < recordFrom {
			insertRaw(ctx, t, pool, rows)
			continue
		}
		for _, r := range rows {
			llmlog.Record(pool, r.entry(t))
		}
		if s%50 == 0 {
			if err := llmlog.Settle(ctx, pool); err != nil {
				t.Fatalf("settle: %v", err)
			}
		}
	}
	for s := range uint64(revisited) {
		rows := genScenario(s, 2, base, false)
		insertRaw(ctx, t, pool, rows)
		written += len(rows)
	}
	if err := llmlog.Settle(ctx, pool); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if f, d := llmlog.InsertFailures()-failures, llmlog.InsertDropped()-dropped; f != 0 || d != 0 {
		t.Fatalf("Record lost rows: %d failed, %d dropped — a trigger that throws takes the row with it", f, d)
	}
	if n := countRows(ctx, t, pool, `SELECT count(*) FROM context_llm_log`); n != int64(written) {
		t.Fatalf("context_llm_log holds %d rows, %d written", n, written)
	}

	t.Run("trigger on top of the backfill", func(t *testing.T) {
		assertParity(ctx, t, pool)
	})
	t.Run("backfill over every row", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `TRUNCATE context_llm_usage_hourly`); err != nil {
			t.Fatalf("truncate rollup: %v", err)
		}
		body, _ := m159Body(t)
		t0 := time.Now()
		execInTx(ctx, t, pool, body)
		t.Logf("159 body (trigger re-created, backfill of %d rows): %s", written, time.Since(t0).Round(time.Millisecond))
		assertParity(ctx, t, pool)
	})
}

// TestUsageRollupNullConflict is gate (b): key-less rows of one dimension
// share one rollup row, through the trigger and across backfill and trigger.
func TestUsageRollupNullConflict(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Hour).Add(-90 * time.Minute)
	row := func(pipeline, consumer string) genRow {
		return genRow{at: at, pipeline: pipeline, host: "h", model: "m", consumer: consumer, metadata: `{}`}
	}
	pool := setupRollupDB(t, func(ctx context.Context, pool *pgxpool.Pool) {
		insertRaw(ctx, t, pool, []genRow{row("m159-null-backfill", ""), row("m159-null-backfill", "")})
	})
	ctx := context.Background()
	for _, r := range []genRow{
		row("m159-null-trigger", ""), row("m159-null-trigger", ""),
		row("m159-null-backfill", ""),
		row("m159-null-consumer", rollConsumers[0]), row("m159-null-consumer", rollConsumers[0]),
	} {
		insertRaw(ctx, t, pool, []genRow{r})
	}
	for _, c := range []struct {
		pipeline string
		calls    int64
	}{{"m159-null-trigger", 2}, {"m159-null-backfill", 3}, {"m159-null-consumer", 2}} {
		var rows, calls int64
		if err := pool.QueryRow(ctx, `
			SELECT count(*), coalesce(sum(calls), 0) FROM context_llm_usage_hourly WHERE pipeline = $1`,
			c.pipeline).Scan(&rows, &calls); err != nil {
			t.Fatalf("%s: %v", c.pipeline, err)
		}
		if rows != 1 || calls != c.calls {
			t.Errorf("%s: %d rollup row(s) with %d calls, want 1 row with calls = %d", c.pipeline, rows, calls, c.calls)
		}
	}
}

func readHists(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pipeline string) (lat, qw, ttft []int64) {
	t.Helper()
	if err := pool.QueryRow(ctx, `
		SELECT latency_hist, queue_wait_hist, ttft_hist FROM context_llm_usage_hourly WHERE pipeline = $1`,
		pipeline).Scan(&lat, &qw, &ttft); err != nil {
		t.Fatalf("histograms of %s: %v", pipeline, err)
	}
	return lat, qw, ttft
}

func goHist(values []int64) []int64 {
	h := make([]int64, histBuckets)
	for _, v := range values {
		h[histIdx(v)]++
	}
	return h
}

// TestUsageRollupHistogram is gate (c): 1 000 rows of one dimension with
// known distributions. Every insert after the first goes through the upsert,
// so the arrays are added element by element (bigint[] + bigint[] is 42883);
// the p95 the buckets give lies within one bucket width of
// percentile_cont(0.95) over the raw rows; every bucket boundary of
// design/05 §3.5 falls where the contract says.
func TestUsageRollupHistogram(t *testing.T) {
	pool := setupRollupDB(t, nil)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)

	rng := rand.New(rand.NewPCG(159, 3))
	durs, qws, ttfts := make([]int64, 1000), make([]int64, 1000), make([]int64, 1000)
	rows := make([]genRow, 1000)
	for i := range rows {
		durs[i] = int64(histBaseMs * math.Exp2(12*rng.Float64()))         // log-uniform 60 ms … 4 min
		qws[i] = int64(rng.IntN(2000))                                    // uniform 0 … 2 s
		ttfts[i] = int64(math.Exp(math.Log(800) + 0.8*rng.NormFloat64())) // log-normal around 0.8 s
		rows[i] = genRow{at: at, pipeline: "m159-hist", host: "h", model: "m",
			durMs: &durs[i], qwMs: &qws[i], metadata: fmt.Sprintf(`{"gw": {"ttft_ms": %d}}`, ttfts[i])}
	}
	for i := range rows {
		if err := insertRawErr(ctx, pool, rows[i:i+1]); err != nil {
			t.Fatalf("insert %d of 1000: %v", i+1, err)
		}
	}

	lat, qw, ttft := readHists(ctx, t, pool, "m159-hist")
	for _, m := range []struct {
		name, expr string
		hist, vals []int64
	}{
		{"latency", "duration_ms", lat, durs},
		{"queue_wait", "queue_wait_ms", qw, qws},
		{"ttft", "(metadata->'gw'->>'ttft_ms')::bigint", ttft, ttfts},
	} {
		if want := goHist(m.vals); !slices.Equal(m.hist, want) {
			t.Errorf("%s buckets %v, want %v", m.name, m.hist, want)
		}
		var truth float64
		if err := pool.QueryRow(ctx, `SELECT percentile_cont(0.95) WITHIN GROUP (ORDER BY `+m.expr+`)
			  FROM context_llm_log WHERE pipeline = 'm159-hist'`).Scan(&truth); err != nil {
			t.Fatalf("%s percentile_cont: %v", m.name, err)
		}
		est := histQuantile(m.hist, 0.95)
		width := bucketWidth(histIdx(int64(math.Ceil(truth))))
		t.Logf("%-10s p95 percentile_cont %9.1f ms, from buckets %9.1f ms, |Δ| %8.1f ms, bucket width %8.0f ms",
			m.name, truth, est, math.Abs(est-truth), width)
		if math.Abs(est-truth) > width {
			t.Errorf("%s: p95 from the buckets %.1f ms is %.1f ms off percentile_cont %.1f ms, more than one bucket width (%.0f ms)",
				m.name, est, math.Abs(est-truth), truth, width)
		}
	}

	t.Run("bucket boundaries", func(t *testing.T) {
		vals := []int64{-5, 0, 1, 59, math.MaxInt32}
		for i := range histBuckets - 1 {
			vals = append(vals, int64(histBaseMs)<<i, int64(histBaseMs)<<i+1)
		}
		bounds := make([]genRow, len(vals))
		for i := range vals {
			bounds[i] = genRow{at: at, pipeline: "m159-bounds", host: "h", model: "m",
				durMs: &vals[i], qwMs: &vals[i], metadata: fmt.Sprintf(`{"gw": {"ttft_ms": %d}}`, vals[i])}
		}
		insertRaw(ctx, t, pool, bounds)
		want := goHist(vals)
		lat, qw, ttft := readHists(ctx, t, pool, "m159-bounds")
		for name, h := range map[string][]int64{"latency": lat, "queue_wait": qw, "ttft": ttft} {
			if !slices.Equal(h, want) {
				t.Errorf("%s over %v: buckets %v, want %v", name, vals, h, want)
			}
		}
	})
}

// totalityForms are the metadata.gw.hop / ttft_ms forms of gate (d) with the
// values the rollup has to make of them: the hop (0..16) and the 0-based
// ttft bucket (-1 = no ttft).
var totalityForms = []struct {
	raw     string
	hop     int16
	ttftIdx int
}{
	{`"x"`, 0, -1}, {`1e400`, 16, histBuckets - 1}, {`-5`, 0, 0}, {`null`, 0, -1},
	{`[]`, 0, -1}, {`{}`, 0, -1}, {`3.7`, 3, 0}, {`"7"`, 0, -1},
}

func wantTTFT(idx int) []int64 {
	h := make([]int64, histBuckets)
	if idx >= 0 {
		h[idx] = 1
	}
	return h
}

func checkTotalityRow(ctx context.Context, t *testing.T, pool *pgxpool.Pool, pipeline string, hop int16, ttftIdx int) {
	t.Helper()
	var gotHop int16
	var calls int64
	var ttft []int64
	if err := pool.QueryRow(ctx, `
		SELECT hop, calls, ttft_hist FROM context_llm_usage_hourly WHERE pipeline = $1`,
		pipeline).Scan(&gotHop, &calls, &ttft); err != nil {
		t.Errorf("%s: %v", pipeline, err)
		return
	}
	if gotHop != hop || calls != 1 || !slices.Equal(ttft, wantTTFT(ttftIdx)) {
		t.Errorf("%s: hop %d calls %d ttft %v, want hop %d calls 1 ttft %v",
			pipeline, gotHop, calls, ttft, hop, wantTTFT(ttftIdx))
	}
}

// TestUsageRollupTotality is gate (d) — B-06-4: the trigger runs inside the
// llmlog INSERT, so a trigger that throws loses the row with its telemetry.
// Every hop × ttft_ms form, gw and metadata that are not objects, dimension
// texts too long for a B-tree entry, a row with both identities and a cost
// sum past NUMERIC(18,8) must be accepted.
func TestUsageRollupTotality(t *testing.T) {
	pool := setupRollupDB(t, nil)
	ctx := context.Background()
	at := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Minute)

	t.Run("hop × ttft_ms forms, raw", func(t *testing.T) {
		var refused []string
		for i, h := range totalityForms {
			for j, f := range totalityForms {
				r := genRow{at: at, pipeline: fmt.Sprintf("m159-tot-%d-%d", i, j), host: "h", model: "m",
					metadata: fmt.Sprintf(`{"gw": {"hop": %s, "ttft_ms": %s}}`, h.raw, f.raw)}
				if err := insertRawErr(ctx, pool, []genRow{r}); err != nil {
					refused = append(refused, fmt.Sprintf("hop %s ttft_ms %s: %v", h.raw, f.raw, err))
					continue
				}
				checkTotalityRow(ctx, t, pool, r.pipeline, h.hop, f.ttftIdx)
			}
		}
		if len(refused) > 0 {
			t.Fatalf("%d of %d inserts refused, first: %s", len(refused),
				len(totalityForms)*len(totalityForms), strings.Join(refused[:min(3, len(refused))], "; "))
		}
	})

	t.Run("hop × ttft_ms forms, through Record", func(t *testing.T) {
		failures := llmlog.InsertFailures()
		for i, h := range totalityForms {
			for j, f := range totalityForms {
				r := genRow{pipeline: fmt.Sprintf("m159-totrec-%d-%d", i, j), host: "h", model: "m",
					metadata: fmt.Sprintf(`{"gw": {"hop": %s, "ttft_ms": %s}}`, h.raw, f.raw)}
				llmlog.Record(pool, r.entry(t))
			}
		}
		if err := llmlog.Settle(ctx, pool); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if d := llmlog.InsertFailures() - failures; d != 0 {
			t.Fatalf("%d of %d Record inserts failed", d, len(totalityForms)*len(totalityForms))
		}
		for i, h := range totalityForms {
			for j, f := range totalityForms {
				checkTotalityRow(ctx, t, pool, fmt.Sprintf("m159-totrec-%d-%d", i, j), h.hop, f.ttftIdx)
			}
		}
	})

	t.Run("gw and metadata that are not objects", func(t *testing.T) {
		var mds []string
		for _, g := range gwNotObjects {
			mds = append(mds, `{"gw": `+g+`}`)
		}
		mds = append(mds, mdNotObjects...)
		for i, md := range mds {
			r := genRow{at: at, pipeline: fmt.Sprintf("m159-totmd-%d", i), host: "h", model: "m", metadata: md}
			if err := insertRawErr(ctx, pool, []genRow{r}); err != nil {
				t.Errorf("metadata %s: %v", md, err)
				continue
			}
			var alias string
			var est int64
			if err := pool.QueryRow(ctx, `SELECT model_alias, estimated_calls FROM context_llm_usage_hourly
				  WHERE pipeline = $1`, r.pipeline).Scan(&alias, &est); err != nil || alias != "" || est != 0 {
				t.Errorf("metadata %s: alias %q estimated %d (%v), want '' and 0", md, alias, est, err)
			}
			checkTotalityRow(ctx, t, pool, r.pipeline, 0, -1)
		}
	})

	t.Run("cost sum beyond NUMERIC(18,8)", func(t *testing.T) {
		// 10 001 rows at the column maximum of context_llm_log.cost_usd
		// (NUMERIC(14,8)) in one dimension sum to more than 10^10.
		if _, err := pool.Exec(ctx, `
			INSERT INTO context_llm_log (created_at, pipeline, model, host, cost_usd)
			SELECT $1, 'm159-totcost', 'm', 'h', 999999.99999999 FROM generate_series(1, 10001)`, at); err != nil {
			t.Fatalf("10 001 rows at cost 999999.99999999: %v", err)
		}
		var sum string
		if err := pool.QueryRow(ctx, `SELECT cost_usd::text FROM context_llm_usage_hourly
			  WHERE pipeline = 'm159-totcost'`).Scan(&sum); err != nil {
			t.Fatalf("cost sum: %v", err)
		}
		if got, want := sum, "10000999999.99989999"; got != want {
			t.Errorf("cost_usd = %s, want %s", got, want)
		}
	})

	t.Run("dimension texts past the B-tree entry limit", func(t *testing.T) {
		// A unique-index entry holds at most 2 704 bytes (B-tree version 4).
		// context_llm_log stores these rows; uncut, the rollup upsert would
		// refuse them with 54000 and take the log row along.
		rng := rand.New(rand.NewPCG(159, 4))
		wide := func(n int) string { // n random four-byte characters
			var b strings.Builder
			for range n {
				b.WriteRune(rune(0x1F300 + rng.IntN(0x600)))
			}
			return b.String()
		}
		var hexModel strings.Builder
		for i := 0; hexModel.Len() < 3000; i++ {
			fmt.Fprintf(&hexModel, "%x", sha256.Sum256([]byte(strconv.Itoa(i))))
		}
		cases := []struct {
			name string
			row  genRow
			gw   string
		}{
			{"model of 3 000 bytes", genRow{at: at, pipeline: "m159-totlong-model", host: "h",
				model: hexModel.String()[:3000], metadata: `{}`}, ""},
			{"six dimensions of 128 four-byte characters", genRow{at: at, pipeline: wide(128),
				backend: wide(128), host: "h", model: wide(128), dclass: wide(128), loc: wide(128)}, wide(128)},
		}
		for _, c := range cases {
			c.row.metadata = `{}`
			if c.gw != "" {
				c.row.metadata = `{"gw": {"alias": "` + c.gw + `"}}`
			}
			if err := insertRawErr(ctx, pool, []genRow{c.row}); err != nil {
				t.Errorf("%s: insert refused: %v", c.name, err)
				continue
			}
			if n := countRows(ctx, t, pool, `SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, c.row.pipeline); n != 1 {
				t.Errorf("%s: %d log rows, want 1", c.name, n)
			}
			var calls int64
			var backend, model, alias, dclass, loc string
			if err := pool.QueryRow(ctx, `
				SELECT calls, backend_name, model, model_alias, dispatch_class, backend_locality
				  FROM context_llm_usage_hourly WHERE pipeline = $1`, capDim(c.row.pipeline)).Scan(
				&calls, &backend, &model, &alias, &dclass, &loc); err != nil {
				t.Errorf("%s: rollup row: %v", c.name, err)
				continue
			}
			wantBackend := c.row.backend
			if wantBackend == "" {
				wantBackend = c.row.host
			}
			got := []string{backend, model, alias, dclass, loc}
			want := []string{capDim(wantBackend), capDim(c.row.model), capDim(c.gw), capDim(c.row.dclass), capDim(c.row.loc)}
			if calls != 1 || !slices.Equal(got, want) {
				t.Errorf("%s: calls %d, dimensions not cut to %d characters: got %q, want %q", c.name, calls, dimMaxRunes, got, want)
			}
		}
	})

	// Last: it drops the llmlog CHECK of 158, the only way a row with both
	// identities can exist (a database where the CHECK is missing).
	t.Run("both identities: the consumer wins", func(t *testing.T) {
		if _, err := pool.Exec(ctx, `ALTER TABLE context_llm_log DROP CONSTRAINT ck_llm_log_one_identity`); err != nil {
			t.Fatalf("drop llmlog CHECK: %v", err)
		}
		r := genRow{at: at, pipeline: "m159-totboth", host: "h", model: "m", metadata: `{}`,
			key: rollKeys[0], consumer: rollConsumers[0]}
		if err := insertRawErr(ctx, pool, []genRow{r}); err != nil {
			t.Fatalf("row with both identities refused: %v", err)
		}
		var key, consumer *string
		var source string
		if err := pool.QueryRow(ctx, `SELECT api_key_id::text, consumer_id::text, source
			  FROM context_llm_usage_hourly WHERE pipeline = 'm159-totboth'`).Scan(&key, &consumer, &source); err != nil {
			t.Fatalf("rollup row: %v", err)
		}
		if key != nil || valOr(consumer) != rollConsumers[0] || source != "gateway" {
			t.Errorf("api_key_id %v consumer_id %v source %s, want NULL / %s / gateway", valOr(key), valOr(consumer), source, rollConsumers[0])
		}
	})
}

// rollupCatalog renders the objects 159 creates — the table with defaults,
// generated column and storage options, its constraints and indexes, the
// four functions and the trigger on the hypertable and on every chunk — one
// line each, in a stable order.
func rollupCatalog(ctx context.Context, t *testing.T, q rowQuerier) string {
	t.Helper()
	parts := []struct{ label, sql string }{
		{"column", `
			SELECT a.attname || ' ' || format_type(a.atttypid, a.atttypmod)
			       || CASE WHEN a.attnotnull THEN ' not null' ELSE '' END
			       || coalesce(' default ' || pg_get_expr(d.adbin, d.adrelid), '')
			       || CASE WHEN a.attgenerated = 's' THEN ' stored' ELSE '' END
			  FROM pg_attribute a
			  LEFT JOIN pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
			 WHERE a.attrelid = 'context_llm_usage_hourly'::regclass AND a.attnum > 0 AND NOT a.attisdropped
			 ORDER BY a.attnum`},
		{"reloptions", `
			SELECT array_to_string(reloptions, ',') FROM pg_class
			 WHERE oid = 'context_llm_usage_hourly'::regclass`},
		{"constraint", `
			SELECT conname || ' ' || pg_get_constraintdef(oid) FROM pg_constraint
			 WHERE conrelid = 'context_llm_usage_hourly'::regclass ORDER BY conname`},
		{"index", `
			SELECT indexdef FROM pg_indexes
			 WHERE schemaname = 'public' AND tablename = 'context_llm_usage_hourly' ORDER BY indexname`},
		{"function", `
			SELECT pg_get_functiondef(oid) FROM pg_proc
			 WHERE proname IN ('llm_usage_hist_idx', 'llm_usage_hist_one', 'hist_add', 'llm_usage_rollup_ins')
			 ORDER BY proname`},
		{"trigger", `
			SELECT c.relname || ' ' || pg_get_triggerdef(tg.oid) || ' enabled=' || tg.tgenabled::text
			  FROM pg_trigger tg JOIN pg_class c ON c.oid = tg.tgrelid
			 WHERE tg.tgname = 'trg_llm_usage_rollup' ORDER BY c.relname`},
	}
	var b strings.Builder
	for _, p := range parts {
		rows, err := q.Query(ctx, p.sql)
		if err != nil {
			t.Fatalf("catalog %s: %v", p.label, err)
		}
		for rows.Next() {
			var line string
			if err := rows.Scan(&line); err != nil {
				rows.Close()
				t.Fatalf("catalog %s scan: %v", p.label, err)
			}
			b.WriteString(p.label + ": " + line + "\n")
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			t.Fatalf("catalog %s rows: %v", p.label, err)
		}
	}
	return b.String()
}

// rollupRows renders every rollup row with its xmin: equal output means no
// row was rewritten, not merely that the values match.
func rollupRows(ctx context.Context, t *testing.T, q rowQuerier) string {
	t.Helper()
	rows, err := q.Query(ctx, `SELECT xmin::text || ' ' || u::text FROM context_llm_usage_hourly u ORDER BY 1`)
	if err != nil {
		t.Fatalf("rollup rows: %v", err)
	}
	defer rows.Close()
	var b strings.Builder
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatalf("rollup row: %v", err)
		}
		b.WriteString(line + "\n")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rollup rows: %v", err)
	}
	return b.String()
}

func gwSeed(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	seedLLMLog(ctx, t, pool, 5000, 30, 25, 16)
	base := time.Now().UTC()
	for s := range uint64(20) {
		insertRaw(ctx, t, pool, genScenario(5000+s, 1, base, false))
	}
}

// TestMigration159Idempotent is gate (e): 159 on a populated database through
// the boot runner, then its body a second time — catalog and rollup rows stay
// byte-identical, and the backfill rewrites no row whose counts already hold.
// It also pins the exactly-once argument of design/06 §3.2: CREATE TRIGGER
// blocks a concurrent INSERT until the migration commits, so the row is
// counted by the trigger and not also by the backfill.
func TestMigration159Idempotent(t *testing.T) {
	ctx := context.Background()
	pool := setupRollupDB(t, func(ctx context.Context, pool *pgxpool.Pool) { gwSeed(ctx, t, pool) })

	t.Run("second apply is a no-op", func(t *testing.T) {
		catalog, rows := rollupCatalog(ctx, t, pool), rollupRows(ctx, t, pool)
		body, _ := m159Body(t)
		execInTx(ctx, t, pool, body)
		if again := rollupCatalog(ctx, t, pool); again != catalog {
			t.Errorf("catalog changed on the second apply:\nbefore:\n%s\nafter:\n%s", catalog, again)
		}
		if again := rollupRows(ctx, t, pool); again != rows {
			t.Errorf("rollup rows changed on the second apply (%d → %d bytes)", len(rows), len(again))
		}
		t.Logf("catalog (%d lines) and %d rollup rows unchanged", strings.Count(catalog, "\n"), strings.Count(rows, "\n"))
		if s, n := countRows(ctx, t, pool, `SELECT coalesce(sum(calls), 0) FROM context_llm_usage_hourly`),
			countRows(ctx, t, pool, `SELECT count(*) FROM context_llm_log`); s != n {
			t.Errorf("sum(calls) = %d, count(*) = %d", s, n)
		}
	})

	t.Run("an insert racing the migration is counted once", func(t *testing.T) {
		pool := testdb.SetupTestDBUpTo(t, m159Version-1)
		seedLLMLog(ctx, t, pool, 200, 14, 10, 8)
		body, _ := m159Body(t)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		if _, err := tx.Exec(ctx, body); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("159 body: %v", err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := pool.Exec(ctx, `INSERT INTO context_llm_log (pipeline, model, host) VALUES ('m159-race', 'm', 'h')`)
			done <- err
		}()
		select {
		case err := <-done:
			_ = tx.Rollback(ctx)
			t.Fatalf("the insert finished while the migration was still open (err %v): nothing blocks writers between CREATE TRIGGER and the backfill", err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
		if err := <-done; err != nil {
			t.Fatalf("racing insert: %v", err)
		}
		if s := countRows(ctx, t, pool, `SELECT coalesce(sum(calls), 0) FROM context_llm_usage_hourly`); s != 201 {
			t.Errorf("sum(calls) = %d, want 201 (200 backfilled + 1 racing row)", s)
		}
		if c := countRows(ctx, t, pool, `SELECT coalesce(sum(calls), 0) FROM context_llm_usage_hourly
			  WHERE pipeline = 'm159-race'`); c != 1 {
			t.Errorf("racing row counted %d times, want 1", c)
		}
	})
}

// TestUsageRollupSurvivesEvictBodies is gate (f): the body janitor is an
// UPDATE; the trigger fires on INSERT only, so the rollup does not change.
func TestUsageRollupSurvivesEvictBodies(t *testing.T) {
	pool := setupRollupDB(t, nil)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log
		    (created_at, pipeline, model, host, duration_ms, request_system, request_user,
		     response_content, metadata)
		SELECT now() - make_interval(days => CASE WHEN g <= 50 THEN 40 + g % 5 ELSE 0 END),
		       'm159-evict', 'm', 'h', 100 + g, 'sys', 'user', 'resp', '{"gw": {"ttft_ms": 300}}'
		  FROM generate_series(1, 60) g`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	before := rollupRows(ctx, t, pool)
	n, err := llmlog.EvictBodies(ctx, pool, 30)
	if err != nil {
		t.Fatalf("EvictBodies: %v", err)
	}
	if n != 50 {
		t.Fatalf("EvictBodies NULLed %d rows, want 50", n)
	}
	if after := rollupRows(ctx, t, pool); after != before {
		t.Errorf("rollup changed by EvictBodies:\nbefore:\n%s\nafter:\n%s", before, after)
	}
	if s := countRows(ctx, t, pool, `SELECT coalesce(sum(calls), 0) FROM context_llm_usage_hourly`); s != 60 {
		t.Errorf("sum(calls) = %d after EvictBodies, want 60", s)
	}
}

// loadRun writes warm+n rows with loadWriters concurrent connections, the
// pipeline cycling through dims names of one hour, and returns the latency
// of each of the n measured inserts (the warm-up ones are not measured).
func loadRun(ctx context.Context, t *testing.T, dsn, tag string, n, warm, dims int) []time.Duration {
	t.Helper()
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("pool config: %v", err)
	}
	cfg.MaxConns, cfg.MinConns = loadWriters, loadWriters
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()

	lat := make([]time.Duration, n)
	var next atomic.Int64
	var failed atomic.Bool
	var errOnce sync.Once
	var loadErr error
	var wg sync.WaitGroup
	for range loadWriters {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1) - 1)
				if i >= warm+n || failed.Load() {
					return
				}
				t0 := time.Now()
				_, err := pool.Exec(ctx, `
					INSERT INTO context_llm_log
					    (pipeline, model, host, duration_ms, queue_wait_ms, prompt_tokens,
					     completion_tokens, backend_name, backend_locality, dispatch_class)
					VALUES ($1, 'qwen', 'h', $2, 0, 10, 20, 'b1', 'local', 'background')`,
					fmt.Sprintf("%s-%02d", tag, i%dims), 50+i%5000)
				if err != nil {
					errOnce.Do(func() { loadErr = err })
					failed.Store(true)
					return
				}
				if i >= warm {
					lat[i-warm] = time.Since(t0)
				}
			}
		}()
	}
	wg.Wait()
	if loadErr != nil {
		t.Fatalf("load insert: %v", loadErr)
	}
	return lat
}

type latStats struct{ p50, p95, p99, p999, max time.Duration }

func percentiles(lat []time.Duration) latStats {
	s := slices.Clone(lat)
	slices.Sort(s)
	at := func(q float64) time.Duration { return s[min(len(s)-1, int(q*float64(len(s))))] }
	return latStats{at(0.50), at(0.95), at(0.99), at(0.999), s[len(s)-1]}
}

func (s latStats) String() string {
	return fmt.Sprintf("p50 %v p95 %v p99 %v p99.9 %v max %v", s.p50, s.p95, s.p99, s.p999, s.max)
}

// tableUpdates reads the rollup's update counters once the writers' stats
// have reached the shared counters (a backend flushes them when it exits).
func tableUpdates(ctx context.Context, t *testing.T, pool *pgxpool.Pool, atLeast int64) (upd, hot int64) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := pool.QueryRow(ctx, `SELECT n_tup_upd, n_tup_hot_upd FROM pg_stat_user_tables
			  WHERE relid = 'context_llm_usage_hourly'::regclass`).Scan(&upd, &hot); err != nil {
			t.Fatalf("pg_stat_user_tables: %v", err)
		}
		if upd >= atLeast || time.Now().After(deadline) {
			return upd, hot
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestUsageRollupLoad is gate (g), opt-in (CTX_M159_LOAD_INSERTS = inserts
// per side, 50000 in the gate): loadWriters concurrent writers on a database
// at 158 (no trigger, the baseline) and one at 159, in the order base, 159,
// 159, base with half the inserts each, so drift over the run falls on both
// sides. p99 with the trigger may exceed the baseline's by loadP99Slack, and
// the HOT share of the rollup's updates must reach loadHOTFloor. Two
// profiles: every insert on one dimension — the gate as specified and the
// worst case, every writer queues for the same row lock — and 64 dimensions
// of one hour, the shape of a busy hour. Measured in W06-3, the fillfactor
// moves the HOT share only in the first profile (0.917–0.919 at 50, 0.894 at
// 100: a row that has to leave its page lands on one with room); the second
// stays above 0.99 at either setting.
func TestUsageRollupLoad(t *testing.T) {
	raw := os.Getenv("CTX_M159_LOAD_INSERTS")
	if raw == "" {
		t.Skip("CTX_M159_LOAD_INSERTS not set — the load gate (g) is opt-in")
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < loadMinInserts {
		t.Fatalf("CTX_M159_LOAD_INSERTS=%q: want at least %d inserts per side — a smaller run shows no p99 and no HOT share worth the name; the gate runs with 50000",
			raw, loadMinInserts)
	}
	ctx := context.Background()
	_, baseDSN := testdb.SetupTestDBUpToWithDSN(t, m159Version-1)
	trigPool, trigDSN := testdb.SetupTestDBUpToWithDSN(t, m159Version-1)
	applyM159(ctx, t, trigPool)
	var reloptions string
	if err := trigPool.QueryRow(ctx, `SELECT array_to_string(reloptions, ',') FROM pg_class
		  WHERE oid = 'context_llm_usage_hourly'::regclass`).Scan(&reloptions); err != nil {
		t.Fatalf("reloptions: %v", err)
	}
	t.Logf("rollup reloptions: %s", reloptions)

	for _, p := range []struct {
		name string
		dims int
	}{{"one dimension", 1}, {"64 dimensions of one hour", 64}} {
		t.Run(p.name, func(t *testing.T) {
			tag := fmt.Sprintf("m159-load-%d", p.dims)
			half, warm := n/2, 1000
			start := time.Now()
			updBefore, hotBefore := tableUpdates(ctx, t, trigPool, 0)
			rowsQ := `SELECT count(*) FROM context_llm_usage_hourly WHERE pipeline LIKE $1`
			rowsBefore := countRows(ctx, t, trigPool, rowsQ, tag+"-%")
			var base, trig []time.Duration
			base = append(base, loadRun(ctx, t, baseDSN, tag, half, warm, p.dims)...)
			trig = append(trig, loadRun(ctx, t, trigDSN, tag, half, warm, p.dims)...)
			trig = append(trig, loadRun(ctx, t, trigDSN, tag, n-half, warm, p.dims)...)
			base = append(base, loadRun(ctx, t, baseDSN, tag, n-half, warm, p.dims)...)
			// Every trigger-side insert that did not create a rollup row
			// updated one. Counted rather than assumed to be p.dims: a run
			// that crosses an hour boundary creates a second row per
			// dimension, and the inserts keep created_at to its default as
			// every writer of the daemon does.
			wantUpd := int64(n+2*warm) - (countRows(ctx, t, trigPool, rowsQ, tag+"-%") - rowsBefore)
			upd, hot := tableUpdates(ctx, t, trigPool, updBefore+wantUpd)
			upd, hot = upd-updBefore, hot-hotBefore
			bs, ts := percentiles(base), percentiles(trig)
			ratio := float64(hot) / float64(max(upd, 1))
			var pages int64
			if err := trigPool.QueryRow(ctx, `SELECT pg_relation_size('context_llm_usage_hourly') / 8192`).Scan(&pages); err != nil {
				t.Fatalf("rollup size: %v", err)
			}
			t.Logf("%d inserts per side, %d writers, %s", n, loadWriters, time.Since(start).Round(time.Millisecond))
			t.Logf("baseline (158, no trigger): %s", bs)
			t.Logf("159 (trigger):              %s", ts)
			t.Logf("p99 Δ %v (threshold %v); rollup updates %d (want %d), HOT %d = %.4f; rollup %d pages",
				ts.p99-bs.p99, loadP99Slack, upd, wantUpd, hot, ratio, pages)
			if ts.p99 > bs.p99+loadP99Slack {
				t.Errorf("p99 with the trigger %v exceeds the baseline %v by more than %v", ts.p99, bs.p99, loadP99Slack)
			}
			if upd < wantUpd {
				t.Errorf("rollup saw %d updates, want %d — stats not flushed or rows not counted", upd, wantUpd)
			}
			if ratio < loadHOTFloor {
				t.Errorf("HOT share %.4f (%d of %d updates), want >= %.2f", ratio, hot, upd, loadHOTFloor)
			}
		})
	}
}

// TestMigration159BackfillProbe times 159 on a copy of production shape: the
// database stops at 158 and gets CTX_M159_PROBE_ROWS rows over 196 days (29
// chunks, the live count), about 1 kB of heap per row as in the 158 probe,
// six pipelines with their models and backends, a knowledge key on one row
// in 60, a metadata.gw on one row in five, errors and external costs. Each
// top-level statement of 159 is timed in one transaction that is rolled
// back, then the boot runner applies the file for real.
func TestMigration159BackfillProbe(t *testing.T) {
	raw := os.Getenv("CTX_M159_PROBE_ROWS")
	if raw == "" {
		t.Skip("CTX_M159_PROBE_ROWS not set — the backfill probe is opt-in")
	}
	rows, err := strconv.Atoi(raw)
	if err != nil || rows < 1 {
		t.Fatalf("CTX_M159_PROBE_ROWS=%q: want a positive row count", raw)
	}
	body, _ := m159Body(t)
	stmts := topLevelStatements(body)
	pool := testdb.SetupTestDBUpTo(t, m159Version-1)
	ctx := context.Background()

	start := time.Now()
	if _, err := pool.Exec(ctx, `
		INSERT INTO context_llm_log
		    (created_at, pipeline, model, host, duration_ms, queue_wait_ms, prompt_tokens,
		     completion_tokens, request_user, metadata, backend_name, backend_locality,
		     dispatch_class, api_key_id, error, cost_usd)
		SELECT now() - make_interval(secs => g * $2::float8),
		       'p' || (g % 6), 'm' || (g % 6 % 2), 'h', (g::bigint * 7919) % 200000, (g::bigint * 104729) % 30000,
		       100 + g % 900, 10 + g % 300,
		       repeat('u', 400),
		       CASE WHEN g % 5 = 0
		            THEN jsonb_build_object('note', repeat('m', 400), 'gw', jsonb_build_object(
		                     'alias', 'a' || (g % 3), 'hop', g % 3, 'ttft_ms', (g * 31) % 5000,
		                     'usage', CASE WHEN g % 10 = 0 THEN 'estimated' ELSE 'reported' END))
		            ELSE jsonb_build_object('note', repeat('m', 400)) END,
		       CASE WHEN g % 50 <> 0 THEN 'b' || (g % 6 % 3) END,
		       CASE WHEN g % 6 = 5 THEN 'external' ELSE 'local' END,
		       CASE WHEN g % 2 = 0 THEN 'background' ELSE 'interactive' END,
		       CASE WHEN g % 60 = 0 THEN ('0195a6f0-0000-7000-8000-00000000a' || lpad((g % 3)::text, 3, '0'))::uuid END,
		       CASE WHEN g % 97 = 0 THEN 'upstream 502' END,
		       CASE WHEN g % 6 = 5 THEN (g % 1000) / 1e6 END
		  FROM generate_series(1, $1::int) g`, rows, 196*86400/float64(rows)); err != nil {
		t.Fatalf("seed %d rows: %v", rows, err)
	}
	t.Logf("seed: %d rows in %s", rows, time.Since(start).Round(time.Millisecond))
	start = time.Now()
	if _, err := pool.Exec(ctx, `VACUUM (ANALYZE) context_llm_log`); err != nil {
		t.Fatalf("vacuum analyze: %v", err)
	}
	t.Logf("vacuum analyze: %s", time.Since(start).Round(time.Millisecond))
	var heap int64
	var tsVersion, sharedBuffers string
	if err := pool.QueryRow(ctx, `
		SELECT sum(pg_relation_size(format('%I.%I', chunk_schema, chunk_name)))::bigint,
		       (SELECT extversion FROM pg_extension WHERE extname = 'timescaledb'),
		       current_setting('shared_buffers')
		  FROM timescaledb_information.chunks WHERE hypertable_name = 'context_llm_log'`,
	).Scan(&heap, &tsVersion, &sharedBuffers); err != nil {
		t.Fatalf("sizes: %v", err)
	}
	t.Logf("table: %d rows, %d chunks, heap %d MB (%d B/row); timescaledb %s, shared_buffers %s",
		rows, llmLogChunks(ctx, t, pool), heap>>20, heap/int64(rows), tsVersion, sharedBuffers)

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for i, s := range stmts {
		t0 := time.Now()
		if _, err := tx.Exec(ctx, s); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("statement %d: %v\n%s", i+1, err, s)
		}
		if d := time.Since(t0); d >= time.Millisecond {
			t.Logf("statement %2d: %10s  %s", i+1, d.Round(time.Millisecond), strings.SplitN(strings.TrimSpace(s), "\n", 2)[0])
		}
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	t.Logf("%d top-level statements; those under 1 ms not listed", len(stmts))

	start = time.Now()
	applyM159(ctx, t, pool)
	t.Logf("runner (whole file, one transaction, as at boot): %s", time.Since(start).Round(time.Millisecond))
	var groups, size, calls int64
	if err := pool.QueryRow(ctx, `SELECT count(*), coalesce(sum(calls), 0), pg_total_relation_size('context_llm_usage_hourly')
		  FROM context_llm_usage_hourly`).Scan(&groups, &calls, &size); err != nil {
		t.Fatalf("rollup: %v", err)
	}
	t.Logf("rollup: %d rows (one per %.1f llmlog rows), %d MB with indexes; sum(calls) %d", groups,
		float64(rows)/float64(max(groups, 1)), size>>20, calls)
	if calls != int64(rows) {
		t.Errorf("sum(calls) = %d, want %d", calls, rows)
	}
}
