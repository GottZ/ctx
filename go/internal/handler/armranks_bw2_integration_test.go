//go:build integration

// B-W2 gates for the admin-gated arm_ranks debug seam on POST /api/query
// (design/04 §4.4), driven through the REAL request path against a PG18
// testcontainer: config snapshot → translate/temporal → embed → rrf →
// logAccess → response.
//
// The seam exists because an external sweep driver cannot reproduce what the
// handler does to a query before it reaches ctx_rrf (translation, temporal
// normalisation, the query-prefixed embedding, the selector decision). So the
// measurement rides the production path — and every gate below is about the
// price of that decision being paid honestly: admin only, retrieval only, one
// read-only snapshot, and access-log rows that say what they are.
//
//	go test -tags=integration ./internal/handler/ -run TestBW2 -count=1 -v
package handler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pgvec "github.com/pgvector/pgvector-go"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	bw2Scope      = "bw2scope"
	bw2AdminKeyID = "019fa500-1111-7000-9000-0000000000a1"
	bw2UserKeyID  = "019fa500-1111-7000-9000-0000000000a2"
	bw2Blocks     = 5

	// bw2GoldenQuery deliberately matches NO lexical arm: no shared trigram
	// with any fixture title (similarity well under the 0.05 threshold) and no
	// shared lexeme with any content. Only the semantic arm fires, over five
	// strictly ordered embeddings — which is what makes the response body
	// reproducible enough to hash. A query that fed the FTS arms would tie on
	// ts_rank and leave the order unspecified (ctx_rrf has no tiebreak).
	bw2GoldenQuery = "zzqqvv"
)

// bw2Embedding is the w023 fixture vector shape: base 0.1, the first k+1
// components raised to 0.9. Callers use even k only, so the dot product
// against the fake embed server's alternating vector is strictly ordered.
func bw2Embedding(k int) []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	for i := 0; i <= k && i < len(e); i++ {
		e[i] = 0.9
	}
	return e
}

func bw2Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for i := 0; i < bw2Blocks; i++ {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', $2, $3, $4, $5, 'knowledge')`,
			fmt.Sprintf("019fa500-0000-7000-9000-0000000001%02d", i),
			fmt.Sprintf("fixture entry number %02d", i),
			fmt.Sprintf("lorem ipsum dolor sit amet consectetur number %02d", i),
			bw2Scope, pgvec.NewVector(bw2Embedding(i*2)),
		); err != nil {
			t.Fatalf("insert block %d: %v", i, err)
		}
	}

	for _, id := range []string{bw2AdminKeyID, bw2UserKeyID} {
		if _, err := pool.Exec(ctx,
			`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('bw2') RETURNING id)
			 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
			 SELECT $1::uuid, 'bw2-hash-'||$1::text, 'bw2', $2, $3, p.id FROM p`,
			id, bw2Scope, []string{bw2Scope},
		); err != nil {
			t.Fatalf("insert api key %s: %v", id, err)
		}
	}
	return pool
}

// bw2Config is the request-path config: selector OFF (the production default,
// and the state the statement counts below assume), rerank/graph/cluster off.
func bw2Config() *config.Config {
	return &config.Config{
		Server:         config.ServerConfig{DBPass: "test-password"},
		Graph:          config.GraphConfig{HopDepth: 1},
		Query:          config.QueryConfig{Timezone: time.UTC},
		EmbedBackfill:  config.EmbedBackfillConfig{BackoffBase: 60 * time.Second, BackoffCap: 24 * time.Hour},
		EmbedMigration: config.EmbedMigrationConfig{BackoffBase: 60 * time.Second, BackoffCap: 24 * time.Hour},
		Distill:        config.Defaults().Distill,
	}
}

// bw2Backend serves BOTH wire shapes the query path can reach: /api/embed for
// the embedding, /api/chat for translate and the temporal LLM fallback. The
// chat counter is the instrument of gate (h): a pin must make that counter
// stay at zero, which is a stronger claim than "the response looks right".
type bw2Backend struct {
	srv       *httptest.Server
	embedHits atomic.Int32
	chatHits  atomic.Int32
}

func bw2NewBackend(t *testing.T) *bw2Backend {
	t.Helper()
	b := &bw2Backend{}
	vec := make([]float64, 1024)
	for i := range vec {
		vec[i] = float64((i % 2) * 2)
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/embed":
			b.embedHits.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"embeddings": [][]float64{vec}})
		case "/api/chat":
			b.chatHits.Add(1)
			// Answer shape valid for BOTH consumers: a plain translation
			// string, and (for the temporal fallback) parseable-as-not-JSON,
			// which that path treats as "no dates".
			_ = json.NewEncoder(w).Encode(map[string]any{
				"message":    map[string]any{"role": "assistant", "content": "database status"},
				"eval_count": 1, "prompt_eval_count": 1,
			})
		default:
			t.Errorf("unexpected backend path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(b.srv.Close)
	return b
}

// bw2Pool carries the embed AND translate roles (the temporal fallback folds
// into the translate role since F3 §2.2), so both LLM stages are reachable and
// therefore countable.
func bw2Pool(host string) *backends.Pool {
	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{
		{ID: "e", Name: "bw2-embed", Host: host, Protocol: backends.ProtocolOllama,
			Model: "test-embed", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleEmbed}},
		{ID: "t", Name: "bw2-chat", Host: host, Protocol: backends.ProtocolOllama,
			Model: "test-chat", Trust: backends.TrustFull, Enabled: true, Priority: 1,
			Roles: []string{backends.RoleTranslate}},
	})
	return bpool
}

func bw2Handler(t *testing.T, pool *pgxpool.Pool, b *bw2Backend) *QueryHandler {
	t.Helper()
	return NewQueryHandler(pool, config.NewStore(bw2Config()), bw2Pool(b.srv.URL), nil,
		blocktype.NewRegistry(), snapshotTestAdmitter(t))
}

// bw2Do drives one POST /api/query as the given principal.
func bw2Do(t *testing.T, h *QueryHandler, keyID string, admin bool, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{
		ApiKeyID:   keyID,
		HomeScope:  bw2Scope,
		ReadScopes: []string{bw2Scope},
		IsValid:    true,
		IsAdmin:    admin,
	}
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	return rec
}

// bw2Block extracts the arm_ranks object, failing the test instead of
// panicking when it is absent — which is exactly the RED state, and a panic
// there would take the rest of the suite down with it.
func bw2Block(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	blockAny, ok := bw2DecodeBody(t, rec)["arm_ranks"]
	if !ok {
		t.Fatalf("arm_ranks block missing from a %d response: %s", rec.Code, rec.Body.String())
	}
	block, ok := blockAny.(map[string]any)
	if !ok {
		t.Fatalf("arm_ranks is %T, want an object", blockAny)
	}
	return block
}

func bw2DecodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	m := map[string]any{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode body %q: %v", rec.Body.String(), err)
	}
	return m
}

// ---------------------------------------------------------------------------
// Gates (a)-(c): the fail-closed gate block
// ---------------------------------------------------------------------------

// TestBW2GateDiscipline pins the three refusals in their order of precedence.
//
// RED against HEAD 532643d (the flag did not exist, so the fields were parsed
// into nothing and every request took the ordinary retrieval path):
//
//	gate (a) non-admin arm_ranks:      status 200, want 403
//	gate (b) admin without synthesize: status 200, want 400
//	gate (c) pinned_* without flag:    status 200, want 400
func TestBW2GateDiscipline(t *testing.T) {
	pool := bw2Setup(t)
	h := bw2Handler(t, pool, bw2NewBackend(t))

	cases := []struct {
		name  string
		admin bool
		body  string
		want  int
	}{
		{"(a) non-admin is refused before anything else", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"arm_ranks":true}`, http.StatusForbidden},
		// Same body as (a) but admin: proves (a) is about the principal, not
		// about the body being malformed.
		{"(b) admin without synthesize:false", true,
			`{"query":"` + bw2GoldenQuery + `","arm_ranks":true}`, http.StatusBadRequest},
		{"(b) admin with synthesize:true is equally refused", true,
			`{"query":"` + bw2GoldenQuery + `","synthesize":true,"arm_ranks":true}`, http.StatusBadRequest},
		{"(c) pinned_translation without the flag", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"pinned_translation":"x"}`, http.StatusBadRequest},
		{"(c) pinned_temporal without the flag", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"pinned_temporal":""}`, http.StatusBadRequest},
		// Precedence: a non-admin sending everything at once still gets 403,
		// never a 400 that would tell them the rest of the body was fine.
		{"(a) beats (b) and (c) for a non-admin", false,
			`{"query":"` + bw2GoldenQuery + `","arm_ranks":true,"pinned_temporal":""}`, http.StatusForbidden},
		// The flag absent entirely is the production path and stays 200.
		{"no flag at all is the ordinary path", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false}`, http.StatusOK},
		{"arm_ranks:false is not a measurement", false,
			`{"query":"` + bw2GoldenQuery + `","synthesize":false,"arm_ranks":false}`, http.StatusOK},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := bw2Do(t, h, bw2UserKeyID, tc.admin, tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
			if tc.want == http.StatusOK {
				if _, ok := bw2DecodeBody(t, rec)["arm_ranks"]; ok {
					t.Error("arm_ranks block present on a request that did not ask for it")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Gate (d): the measurement response
// ---------------------------------------------------------------------------

// TestBW2MeasurementResponse is the happy path: admin + synthesize:false +
// arm_ranks:true returns the extra block, complete.
//
// RED against HEAD: `arm_ranks block missing from a 200 response`.
func TestBW2MeasurementResponse(t *testing.T) {
	pool := bw2Setup(t)
	h := bw2Handler(t, pool, bw2NewBackend(t))

	rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+bw2GoldenQuery+`","synthesize":false,"limit":3,"arm_ranks":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	body := bw2DecodeBody(t, rec)
	block := bw2Block(t, rec)

	rows, _ := block["rows"].([]any)
	if len(rows) == 0 {
		t.Fatal("arm_ranks.rows is empty")
	}
	for i, r := range rows {
		row, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("row %d is %T, want an object", i, r)
		}
		// The four rank columns must be PRESENT — null is a legitimate value
		// (the candidate is not in that arm) and an absent key is not.
		for _, k := range []string{"id", "rank_semantic", "rank_fts_de", "rank_fts_en",
			"rank_trigram", "cos_sim", "mass_factor", "type_factor"} {
			if _, ok := row[k]; !ok {
				t.Errorf("row %d lacks key %q: %v", i, k, row)
			}
		}
		if row["id"] == "" {
			t.Errorf("row %d has an empty id", i)
		}
	}

	// fusion_order is the PRE-post-stage ranking: it must be at least as long
	// as the delivered sources (limit 3 truncates the response, never this).
	order, _ := block["fusion_order"].([]any)
	if len(order) == 0 {
		t.Fatal("fusion_order is empty")
	}
	sources, _ := body["sources"].([]any)
	if len(order) < len(sources) {
		t.Errorf("fusion_order (%d) shorter than sources (%d) — it was captured after truncation",
			len(order), len(sources))
	}

	if got := block["effective_query"]; got != bw2GoldenQuery {
		t.Errorf("effective_query = %v, want %q", got, bw2GoldenQuery)
	}
	for _, k := range []string{"effective_query_spaced", "effective_temporal", "embed_model", "embed_cache_hit"} {
		if _, ok := block[k]; !ok {
			t.Errorf("arm_ranks lacks %q", k)
		}
	}
	if got := block["embed_model"]; got != "test-embed" {
		t.Errorf("embed_model = %v, want the chain head's model", got)
	}
	sel, ok := block["selector"].(map[string]any)
	if !ok {
		t.Fatalf("selector is %T, want an object", block["selector"])
	}
	for _, k := range []string{"mode", "reason", "estimate", "scan_tuples", "exact_cap"} {
		if _, ok := sel[k]; !ok {
			t.Errorf("selector lacks %q: %v", k, sel)
		}
	}
	if sel["mode"] != "ann" || sel["reason"] != "disabled" {
		t.Errorf("selector = %v, want the disabled-selector decision {ann, disabled}", sel)
	}
	t.Logf("rows=%d fusion_order=%d sources=%d selector=%v", len(rows), len(order), len(sources), sel)
}

// ---------------------------------------------------------------------------
// Gate (e): access-log provenance
// ---------------------------------------------------------------------------

// bw2Sources waits for the ASYNC access-log rows of one query and returns
// their metadata.source values (logAccess runs in its own goroutine).
func bw2Sources(t *testing.T, pool *pgxpool.Pool, queryText string) []string {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := pool.Query(context.Background(),
			`SELECT metadata->>'source' FROM context_access_log
			 WHERE action = 'query' AND query_text = $1 ORDER BY id`, queryText)
		if err != nil {
			t.Fatalf("select access log: %v", err)
		}
		var out []string
		for rows.Next() {
			var s *string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan source: %v", err)
			}
			if s == nil {
				out = append(out, "<null>")
			} else {
				out = append(out, *s)
			}
		}
		rows.Close()
		if len(out) > 0 {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("no context_access_log row for %q within 10s", queryText)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestBW2AccessLogSource is gate (e): a measurement leaves LABELLED rows, an
// ordinary query keeps writing "agent".
//
// RED against HEAD: `measurement row source = "agent", want "armsweep"`.
func TestBW2AccessLogSource(t *testing.T) {
	pool := bw2Setup(t)
	h := bw2Handler(t, pool, bw2NewBackend(t))

	const sweepQ = bw2GoldenQuery + " sweep"
	const plainQ = bw2GoldenQuery + " plain"

	if rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+sweepQ+`","synthesize":false,"arm_ranks":true}`); rec.Code != http.StatusOK {
		t.Fatalf("sweep request: status %d, body %s", rec.Code, rec.Body.String())
	}
	if rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+plainQ+`","synthesize":false}`); rec.Code != http.StatusOK {
		t.Fatalf("plain request: status %d, body %s", rec.Code, rec.Body.String())
	}

	for i, s := range bw2Sources(t, pool, sweepQ) {
		if s != "armsweep" {
			t.Errorf("measurement row %d source = %q, want %q", i, s, "armsweep")
		}
	}
	for i, s := range bw2Sources(t, pool, plainQ) {
		if s != "agent" {
			t.Errorf("ordinary row %d source = %q, want %q", i, s, "agent")
		}
	}
}

// ---------------------------------------------------------------------------
// Gate (f): non-regression without the flag
// ---------------------------------------------------------------------------

// bw2GoldenSHA is the SHA-256 of the response body of
//
//	{"query":"zzqqvv","synthesize":false,"limit":3}
//
// on the bw2Setup fixture. It was produced by running THIS test against
// HEAD 532643d with the B-W2 sources stashed and the new rrf files moved
// aside (`git stash push -- go/internal`, arms*.go out of the tree), i.e.
// against the pre-B-W2 handler. The seam must not move it by one byte.
//
// Reproduce: run the test, read the "golden body" line it logs on mismatch.
const bw2GoldenSHA = "5695c3122193a2230b6b071ebd0936b79552d40d444b244453827b2f578d3f3d"

// TestBW2NoFlagIsByteIdentical is the non-regression gate: a request WITHOUT
// the flag must produce the exact same bytes as before the seam existed. The
// fixture is chosen so the body is reproducible at all (see bw2GoldenQuery):
// only the semantic arm fires, over five strictly ordered embeddings, with
// age_days pinned to 0 by inserting at now().
func TestBW2NoFlagIsByteIdentical(t *testing.T) {
	pool := bw2Setup(t)
	h := bw2Handler(t, pool, bw2NewBackend(t))

	rec := bw2Do(t, h, bw2UserKeyID, false,
		`{"query":"`+bw2GoldenQuery+`","synthesize":false,"limit":3}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	sum := sha256.Sum256(rec.Body.Bytes())
	got := hex.EncodeToString(sum[:])
	if got != bw2GoldenSHA {
		t.Errorf("response body sha256 = %s, want %s\ngolden body: %s", got, bw2GoldenSHA, rec.Body.String())
	}
}

// bw2Rollbacks reads the per-database rolled-back transaction counter. The
// test database is exclusive to this test (testdb hands out one DATABASE per
// test), and nothing on the query path rolls back — except the measurement
// transaction, which ALWAYS does, because it is read-only and never commits.
// That makes this counter a direct census of "how many transactions did the
// seam open".
func bw2Rollbacks(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(context.Background(),
		`SELECT xact_rollback FROM pg_stat_database WHERE datname = current_database()`).Scan(&n); err != nil {
		t.Fatalf("read xact_rollback: %v", err)
	}
	return n
}

// bw2WaitRollbacks waits for the counter to reach want and then re-checks
// after a settle window. Postgres flushes backend statistics on an interval
// (~1s), so the counter LAGS — it never invents transactions. Both directions
// therefore need the settle: waiting proves "at least want happened", the
// re-check proves "and not more".
func bw2WaitRollbacks(t *testing.T, pool *pgxpool.Pool, want int64) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for bw2Rollbacks(t, pool) < want {
		if time.Now().After(deadline) {
			t.Fatalf("xact_rollback stuck at %d, want %d", bw2Rollbacks(t, pool), want)
		}
		time.Sleep(200 * time.Millisecond)
	}
	time.Sleep(2500 * time.Millisecond) // stats flush interval + margin
	if got := bw2Rollbacks(t, pool); got != want {
		t.Fatalf("xact_rollback = %d, want exactly %d", got, want)
	}
}

// TestBW2TransactionCensus is the statement-counter half of gate (f): without
// the flag the query path opens NO transaction at all, with the flag it opens
// exactly one — and that one is the read-only measurement transaction.
//
// The rrf-side counterpart (exactly one ctx_rrf + one ctx_rrf_arms through the
// same Querier) is TestBW2StatementCounts in internal/rrf.
//
// RED against HEAD: the flagged request took the ordinary path, so the counter
// never moved and the wait failed with `xact_rollback stuck at 0, want 1`.
func TestBW2TransactionCensus(t *testing.T) {
	pool := bw2Setup(t)
	h := bw2Handler(t, pool, bw2NewBackend(t))

	base := bw2Rollbacks(t, pool)

	// Three ordinary requests first: if any of them opened a transaction, the
	// final exact-count assertion below could not come out at base+1.
	for i := 0; i < 3; i++ {
		if rec := bw2Do(t, h, bw2UserKeyID, false,
			fmt.Sprintf(`{"query":"%s no flag %d","synthesize":false}`, bw2GoldenQuery, i)); rec.Code != http.StatusOK {
			t.Fatalf("no-flag request %d: status %d, body %s", i, rec.Code, rec.Body.String())
		}
	}
	time.Sleep(2500 * time.Millisecond)
	if got := bw2Rollbacks(t, pool); got != base {
		t.Fatalf("three flag-free requests moved xact_rollback %d → %d; the ordinary path must open no transaction", base, got)
	}

	if rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+bw2GoldenQuery+` flagged","synthesize":false,"arm_ranks":true}`); rec.Code != http.StatusOK {
		t.Fatalf("flagged request: status %d, body %s", rec.Code, rec.Body.String())
	}
	bw2WaitRollbacks(t, pool, base+1)
}

// ---------------------------------------------------------------------------
// Gate (h): the pins
// ---------------------------------------------------------------------------

// TestBW2PinnedTranslation pins that pinned_translation REPLACES the
// translation stage rather than merely overwriting its result: the chat
// counter must stay at zero.
//
// RED against HEAD: the field was ignored, the German query went to the
// translate backend anyway — `chat hits = 1, want 0`.
func TestBW2PinnedTranslation(t *testing.T) {
	pool := bw2Setup(t)
	b := bw2NewBackend(t)
	h := bw2Handler(t, pool, b)

	// Control: German query, no pin ⇒ the translate wire call happens. Without
	// this half the zero below could just mean "German was never detected".
	const german = `wie ist der status der datenbank`
	if rec := bw2Do(t, h, bw2UserKeyID, false,
		`{"query":"`+german+`","synthesize":false}`); rec.Code != http.StatusOK {
		t.Fatalf("control: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := b.chatHits.Load(); got == 0 {
		t.Fatal("control: no translate wire call — the pin assertion below would be vacuous")
	}

	b.chatHits.Store(0)
	rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+german+`","synthesize":false,"arm_ranks":true,"pinned_translation":"database status"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pinned: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := b.chatHits.Load(); got != 0 {
		t.Errorf("chat hits = %d, want 0 — the pin must skip the translate call entirely", got)
	}
	block := bw2Block(t, rec)
	if got := block["effective_query"]; got != "database status" {
		t.Errorf("effective_query = %v, want the pin", got)
	}
	if got := bw2DecodeBody(t, rec)["translated"]; got != true {
		t.Errorf("translated = %v, want true (the pin differs from the original)", got)
	}
}

// TestBW2PinnedTemporal pins the second half: an EMPTY pin is a value, not an
// omission — it means "no temporal expansion", and it suppresses the LLM
// fallback that would otherwise run for a query with temporal intent.
//
// RED against HEAD: the field was ignored — `chat hits = 1, want 0`.
func TestBW2PinnedTemporal(t *testing.T) {
	pool := bw2Setup(t)
	b := bw2NewBackend(t)
	h := bw2Handler(t, pool, b)

	// "recently" carries temporal intent but no parseable date, so the rules
	// return nil and the LLM fallback is the stage under test. English, so the
	// translation stage stays out of the count.
	const q = `what changed recently`
	if rec := bw2Do(t, h, bw2UserKeyID, false,
		`{"query":"`+q+`","synthesize":false}`); rec.Code != http.StatusOK {
		t.Fatalf("control: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := b.chatHits.Load(); got == 0 {
		t.Fatal("control: no temporal fallback call — the pin assertion below would be vacuous")
	}

	b.chatHits.Store(0)
	rec := bw2Do(t, h, bw2AdminKeyID, true,
		`{"query":"`+q+`","synthesize":false,"arm_ranks":true,"pinned_temporal":""}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("pinned: status %d, body %s", rec.Code, rec.Body.String())
	}
	if got := b.chatHits.Load(); got != 0 {
		t.Errorf("chat hits = %d, want 0 — the pin must skip the temporal LLM fallback", got)
	}
	block := bw2Block(t, rec)
	if got := block["effective_temporal"]; got != "" {
		t.Errorf("effective_temporal = %v, want the empty pin", got)
	}
}
