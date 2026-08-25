//go:build integration

// B-W2 gates for the Go-side measurement seam (design/04 §4.4): rrf.SearchTx /
// rrf.ArmRanksTx over a caller-supplied rrf.Querier, so ctx_rrf and
// ctx_rrf_arms can be made to run on ONE transaction — and therefore on one
// snapshot and one SET LOCAL hnsw.* state.
//
// The fixture is B-W1's (bw1SeedCorpus, arms_parity_integration_test.go): 220
// blocks over three scopes, four types, archived rows, NULL embeddings and
// content_times. Reusing it keeps the two waves measuring the same corpus.
//
//	go test -tags=integration ./internal/rrf/ -run TestBW2 -count=1 -v
package rrf_test

import (
	"context"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/rrf"
	"github.com/GottZ/ctx/internal/testdb"
)

// bw2Counter wraps a rrf.Querier and records every statement that travels
// through it. hook fires ONCE, at the start of the nth Query — which is how
// gate (g) lands a concurrent write exactly between the two statements of the
// measurement transaction.
type bw2Counter struct {
	inner rrf.Querier
	stmts []string
	hookN int
	hook  func()
}

func (c *bw2Counter) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.stmts = append(c.stmts, sql)
	if c.hook != nil && len(c.stmts) == c.hookN {
		c.hook()
		c.hook = nil
	}
	return c.inner.Query(ctx, sql, args...)
}

func (c *bw2Counter) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	c.stmts = append(c.stmts, sql)
	return c.inner.QueryRow(ctx, sql, args...)
}

// calls counts the recorded statements containing needle.
func (c *bw2Counter) calls(needle string) int {
	n := 0
	for _, s := range c.stmts {
		if strings.Contains(s, needle) {
			n++
		}
	}
	return n
}

// bw2Search runs SearchTx with the B-W1 fixture's policy surface.
func bw2Search(ctx context.Context, q rrf.Querier, pool *pgxpool.Pool, emb []float32, policy rrf.SelectorPolicy) ([]rrf.SearchResult, rrf.SelectorDecision, error) {
	return rrf.SearchTx(ctx, pool, q, emb, bw1FixedQuery, bw1FixedQuery,
		[]string{bw1ScopeA, bw1ScopeB}, nil, nil, bw1Limit, "", "",
		bw1VisibleTypes, bw1DampedTypes, bw1DampedFactors, nil, nil, nil, policy)
}

// bw2Arms runs ArmRanksTx with the same argument surface.
func bw2Arms(ctx context.Context, q rrf.Querier, dec rrf.SelectorDecision, policy rrf.SelectorPolicy, emb []float32) ([]rrf.ArmRow, error) {
	return rrf.ArmRanksTx(ctx, q, dec, policy, emb, bw1FixedQuery, bw1FixedQuery,
		[]string{bw1ScopeA, bw1ScopeB}, nil, nil, bw1Limit, "", "",
		bw1VisibleTypes, bw1DampedTypes, bw1DampedFactors, nil, nil, nil)
}

// ---------------------------------------------------------------------------
// Gate (f), statement half: what runs, and how often
// ---------------------------------------------------------------------------

// TestBW2StatementCounts pins the statement budget of the seam. With the
// selector OFF (the production default) a measurement is exactly two
// statements — one ctx_rrf, one ctx_rrf_arms — and nothing else; no probe, no
// repeat, no accidental second fusion call.
//
// RED before B-W2: rrf.SearchTx and rrf.ArmRanksTx did not exist, so this file
// did not compile:
//
//	internal/rrf/arms_seam_bw2_integration_test.go:75:9: undefined: rrf.SearchTx
//	internal/rrf/arms_seam_bw2_integration_test.go:82:9: undefined: rrf.ArmRanksTx
//	internal/rrf/arms_seam_bw2_integration_test.go:36:8: undefined: rrf.Querier
func TestBW2StatementCounts(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)
	emb := bw1Embedding(0)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	c := &bw2Counter{inner: tx}
	res, dec, err := bw2Search(ctx, c, pool, emb, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("SearchTx: %v", err)
	}
	if len(res) == 0 {
		t.Fatal("SearchTx returned no rows — the counts below would be vacuous")
	}
	if got := len(c.stmts); got != 1 {
		t.Errorf("SearchTx with selector off: %d statements through the querier, want 1: %q", got, c.stmts)
	}
	if got := c.calls("ctx_rrf_arms"); got != 0 {
		t.Errorf("SearchTx ran ctx_rrf_arms %d times, want 0", got)
	}

	rows, err := bw2Arms(ctx, c, dec, rrf.SelectorPolicy{}, emb)
	if err != nil {
		t.Fatalf("ArmRanksTx: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("ArmRanksTx returned no rows")
	}
	if got := len(c.stmts); got != 2 {
		t.Errorf("seam total: %d statements, want 2: %q", got, c.stmts)
	}
	if got := c.calls("ctx_rrf_arms"); got != 1 {
		t.Errorf("ctx_rrf_arms ran %d times, want exactly 1", got)
	}
	// ctx_rrf_arms contains the substring "ctx_rrf", so the fusion call is
	// counted by difference rather than by a substring that both match.
	if got := c.calls("FROM ctx_rrf("); got != 1 {
		t.Errorf("ctx_rrf ran %d times, want exactly 1", got)
	}
	t.Logf("statements: %d (%s)", len(c.stmts), strings.Join(bw2Heads(c.stmts), " | "))
}

// TestBW2ProbeRidesTheQuerier pins the boundedProbe decision (SearchTx doc
// comment): with the selector ARMED, the cardinality probe travels through the
// same Querier as the fusion statement — not through the pool. Inside a
// measurement transaction that is what keeps the strategy decision and the
// statement it decides on one snapshot.
func TestBW2ProbeRidesTheQuerier(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)

	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	c := &bw2Counter{inner: tx}
	policy := rrf.SelectorPolicy{Enabled: true, ExactMax: 64, GreyMax: 65536, GreyScanTuples: 60000}
	if _, dec, err := bw2Search(ctx, c, pool, bw1Embedding(0), policy); err != nil {
		t.Fatalf("SearchTx: %v", err)
	} else if dec.Reason == "" {
		t.Fatal("armed selector produced no reason")
	}
	if got := c.calls("FROM context_blocks"); got != 1 {
		t.Errorf("bounded probe through the querier: %d, want 1: %q", got, bw2Heads(c.stmts))
	}
	if got := len(c.stmts); got != 2 {
		t.Errorf("armed selector: %d statements, want 2 (probe + ctx_rrf): %q", got, bw2Heads(c.stmts))
	}
}

// ---------------------------------------------------------------------------
// Gate (g): snapshot identity
// ---------------------------------------------------------------------------

// bw2Intruder inserts one block from a SEPARATE connection — a write the
// measurement transaction must not be able to see once its snapshot is taken.
// The block is a maximal match: its embedding IS the query vector and its text
// carries both query words, so the semantic and both FTS arms would rank it
// first if it were visible at all.
func bw2Intruder(t *testing.T, pool *pgxpool.Pool, emb []float32) string {
	t.Helper()
	const id = "019fa402-0000-7000-9000-000000099999"
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
		 VALUES ($1::uuid, 'alpha', 'Datenbank function intruder',
		         'Datenbank function intruder block', $2, $3, 'knowledge')`,
		id, bw1ScopeA, pgvec.NewVector(emb),
	); err != nil {
		t.Fatalf("intruder insert: %v", err)
	}
	return id
}

func bw2Contains(rows []rrf.ArmRow, id string) bool {
	for _, r := range rows {
		if r.ID == id {
			return true
		}
	}
	return false
}

// TestBW2SnapshotIdentity is the gate that justifies the transaction. A row
// inserted from a second connection BETWEEN the two statements must not appear
// in the ctx_rrf_arms result — otherwise the arm ranks describe a candidate
// set the fusion never saw.
//
// The autocommit half is the RED control and runs in the same test: over the
// pool (which is what the seam would be without a transaction) the very same
// intruder DOES show up. That is the failure this gate is written against, and
// it is asserted rather than described, so the gate cannot quietly become
// vacuous.
func TestBW2SnapshotIdentity(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)
	emb := bw1Embedding(0)

	// --- RED control: two autocommit statements, no transaction.
	{
		var intruder string
		c := &bw2Counter{inner: pool, hookN: 2, hook: func() { intruder = bw2Intruder(t, pool, emb) }}
		_, dec, err := bw2Search(ctx, c, pool, emb, rrf.SelectorPolicy{})
		if err != nil {
			t.Fatalf("autocommit SearchTx: %v", err)
		}
		rows, err := bw2Arms(ctx, c, dec, rrf.SelectorPolicy{}, emb)
		if err != nil {
			t.Fatalf("autocommit ArmRanksTx: %v", err)
		}
		if intruder == "" {
			t.Fatal("hook never fired — the control proves nothing")
		}
		if !bw2Contains(rows, intruder) {
			t.Fatalf("autocommit control: intruder %s NOT in the arms result — the gate below would be vacuous", intruder)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM context_blocks WHERE id = $1::uuid`, intruder); err != nil {
			t.Fatalf("cleanup intruder: %v", err)
		}
	}

	// --- The seam: one RepeatableRead/ReadOnly transaction.
	tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // read-only

	var intruder string
	c := &bw2Counter{inner: tx, hookN: 2, hook: func() { intruder = bw2Intruder(t, pool, emb) }}
	_, dec, err := bw2Search(ctx, c, pool, emb, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("SearchTx: %v", err)
	}
	rows, err := bw2Arms(ctx, c, dec, rrf.SelectorPolicy{}, emb)
	if err != nil {
		t.Fatalf("ArmRanksTx: %v", err)
	}
	if intruder == "" {
		t.Fatal("hook never fired")
	}
	if bw2Contains(rows, intruder) {
		t.Errorf("snapshot leak: intruder %s appeared in the arms result of the measurement tx", intruder)
	}
	t.Logf("arms rows: %d, intruder %s correctly invisible", len(rows), intruder)
}

// ---------------------------------------------------------------------------
// Delegation
// ---------------------------------------------------------------------------

// TestBW2SearchDelegatesToSearchTx pins that the untouched Search entry point
// and the new SearchTx produce the same retrieval on the same corpus — the
// non-regression argument for the ~20 existing Search call sites, asserted on
// data rather than on the one-line body.
func TestBW2SearchDelegatesToSearchTx(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	bw1SeedCorpus(t, pool)
	emb := bw1Embedding(3)

	a, decA, err := rrf.Search(ctx, pool, emb, bw1FixedQuery, bw1FixedQuery,
		[]string{bw1ScopeA, bw1ScopeB}, nil, nil, bw1Limit, "", "",
		bw1VisibleTypes, bw1DampedTypes, bw1DampedFactors, nil, nil, nil, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	b, decB, err := bw2Search(ctx, pool, pool, emb, rrf.SelectorPolicy{})
	if err != nil {
		t.Fatalf("SearchTx: %v", err)
	}
	if len(a) == 0 || len(a) != len(b) {
		t.Fatalf("row counts differ: Search %d, SearchTx %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].RRFScore != b[i].RRFScore {
			t.Fatalf("row %d differs: Search %s/%v, SearchTx %s/%v", i, a[i].ID, a[i].RRFScore, b[i].ID, b[i].RRFScore)
		}
	}
	if decA != decB {
		t.Errorf("decisions differ: %+v vs %+v", decA, decB)
	}
}

// bw2Heads shortens statements for a readable failure message.
func bw2Heads(stmts []string) []string {
	out := make([]string, len(stmts))
	for i, s := range stmts {
		s = strings.Join(strings.Fields(s), " ")
		if len(s) > 60 {
			s = s[:60]
		}
		out[i] = s
	}
	return out
}
