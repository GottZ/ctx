//go:build integration

// W02-3 gates G1/G2 (+ the enabled=false hygiene assert and the runtime flip)
// of the Evokoa-Clean-Room design/02-strategy-selektor.md §4.7 / §7 "W02-3"
// against a real PG18 testcontainer, driven through the REAL request path:
// POST /api/query (retrieval-only, synthesize=false) with a fake embed backend
// — config snapshot → cfg.SelectorRRF() → rrf.Search → logAccess. The assertion
// is a DB read of context_access_log.metadata, never a log grep.
//
//	go test -tags=integration ./internal/handler/ -run TestSelectorW023 -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	w023KeyID      = "019f9201-2222-7000-9000-0000000000aa"
	w023SmallScope = "w023small"
	w023LargeScope = "w023large"
	// w023ExactMax is the clamp FLOOR (§5.4) — the smallest legal exact
	// threshold, so the "large" fixture stays at 65 rows instead of 4097.
	w023ExactMax = 64
	w023SmallN   = 5
	w023LargeN   = 70
)

// w023Embedding is the rrf fixture vector shape: base 0.1 everywhere, the
// first k+1 components raised to 0.9 (strictly ordered distances, no ties).
func w023Embedding(k int) []float32 {
	e := make([]float32, 1024)
	for i := range e {
		e[i] = 0.1
	}
	for i := 0; i <= k && i < len(e); i++ {
		e[i] = 0.9
	}
	return e
}

func w023Setup(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	insert := func(id, scope string, k int) {
		if _, err := pool.Exec(ctx,
			`INSERT INTO context_blocks (id, category, title, content, scope, embedding, type_name)
			 VALUES ($1::uuid, 'knowledge', 'w023-'||right($1::text, 4), 'w023 content alpha bravo', $2, $3, 'knowledge')`,
			id, scope, pgvec.NewVector(w023Embedding(k)),
		); err != nil {
			t.Fatalf("insert block %s: %v", id, err)
		}
	}
	for i := 0; i < w023SmallN; i++ {
		insert(fmt.Sprintf("019f9201-0000-7000-9000-0000000010%02d", i+1), w023SmallScope, i)
	}
	for i := 0; i < w023LargeN; i++ {
		insert(fmt.Sprintf("019f9201-0000-7000-9000-0000000020%02d", i+1), w023LargeScope, i%20)
	}

	if _, err := pool.Exec(ctx,
		`WITH p AS (INSERT INTO context_principals (display_name) VALUES ('w023') RETURNING id)
		 INSERT INTO context_api_keys (id, key_hash, label, home_scope, allowed_scopes, principal_id)
		 SELECT $1::uuid, 'w023-test-hash', 'w023', $2, $3, p.id FROM p`,
		w023KeyID, w023SmallScope, []string{w023SmallScope, w023LargeScope},
	); err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return pool
}

// w023Config is the request-path config with the Achse-02 group set. enabled
// is the only knob the tests flip; the thresholds stay at the §3.4 shape
// except exact_max, which sits on the clamp floor so the "large" fixture can
// be 70 rows instead of 4097.
func w023Config(enabled bool) *config.Config {
	return &config.Config{
		// The non-selector fields only exist so config.Store.Replace passes
		// Validate on the runtime-flip path (G3) — the F2 settings-write gate
		// rejects a generation with SeverityError issues.
		Server: config.ServerConfig{DBPass: "test-password"},
		Chat:   config.ChatConfig{Protocol: backends.ProtocolOllama},
		Embed:  config.EmbedConfig{Protocol: backends.ProtocolOllama},
		Dream:  config.DreamConfig{Protocol: backends.ProtocolOllama},
		Graph:  config.GraphConfig{HopDepth: 1},        // Enabled stays false
		Query:  config.QueryConfig{Timezone: time.UTC}, // RateLimitRead 0 = disabled
		Selector: config.SelectorConfig{
			Enabled:        enabled,
			ExactMax:       w023ExactMax,
			GreyMax:        65536,
			GreyScanTuples: 60000,
			StatsTTL:       60 * time.Second,
		},
	}
}

// w023Query drives POST /api/query retrieval-only for one scope and returns
// the recorder. queryText doubles as the access-log correlation key.
func w023Query(t *testing.T, h *QueryHandler, scope, queryText string) *httptest.ResponseRecorder {
	t.Helper()
	body := fmt.Sprintf(`{"query":%q,"synthesize":false,"limit":10}`, queryText)
	req := httptest.NewRequest(http.MethodPost, "/api/query", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ar := &auth.AuthResult{
		ApiKeyID:   w023KeyID,
		HomeScope:  scope,
		ReadScopes: []string{scope},
		IsValid:    true,
	}
	req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
	rec := httptest.NewRecorder()
	h.HandleQuery(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("query %q: status %d, body %s", queryText, rec.Code, rec.Body.String())
	}
	return rec
}

// w023Metadata waits for the ASYNC access-log rows of one query and returns
// their decoded metadata maps (logAccess runs in its own goroutine).
func w023Metadata(t *testing.T, pool *pgxpool.Pool, queryText string) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rows, err := pool.Query(context.Background(),
			`SELECT metadata::text FROM context_access_log
			 WHERE action = 'query' AND query_text = $1 ORDER BY id`, queryText)
		if err != nil {
			t.Fatalf("select access log: %v", err)
		}
		var out []map[string]any
		for rows.Next() {
			var raw string
			if err := rows.Scan(&raw); err != nil {
				t.Fatalf("scan metadata: %v", err)
			}
			m := map[string]any{}
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				t.Fatalf("decode metadata %q: %v", raw, err)
			}
			out = append(out, m)
		}
		rows.Close()
		if len(out) > 0 {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("no context_access_log row for query %q within 10s", queryText)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func w023Handler(t *testing.T, pool *pgxpool.Pool, store *config.Store) *QueryHandler {
	t.Helper()
	srv, _ := fakeEmbedServer(t)
	return NewQueryHandler(pool, store, embedPool(srv.URL), nil,
		blocktype.NewRegistry(), snapshotTestAdmitter(t))
}

// TestSelectorW023_G1_SelKeysPresentWhenEnabled is gate G1: with the master
// gate ARMED every context_access_log row of the query carries the three
// Achse-02 keys. Red against the Ist stand (which never writes sel_*).
func TestSelectorW023_G1_SelKeysPresentWhenEnabled(t *testing.T) {
	pool := w023Setup(t)
	h := w023Handler(t, pool, config.NewStore(w023Config(true)))

	const q = "w023 g1 armed alpha bravo"
	w023Query(t, h, w023SmallScope, q)

	metas := w023Metadata(t, pool, q)
	for i, m := range metas {
		for _, k := range []string{"sel_mode", "sel_reason", "sel_est"} {
			if _, ok := m[k]; !ok {
				t.Errorf("row %d: metadata %v lacks %q — the Achse-01 correlation input is missing", i, m, k)
			}
		}
		// §5.5: strategy metadata only — the decision must never carry content.
		if _, bad := m["query"]; bad {
			t.Errorf("row %d: metadata leaks query content: %v", i, m)
		}
	}
	t.Logf("G1 GREEN: %d access-log rows carry sel_* — example: %v", len(metas), metas[0])
}

// TestSelectorW023_G2_SelValuesPerScope is gate G2: a SMALL scope decides
// exact with the probe count as estimate, a scope beyond the (clamped)
// exact_max falls through the W02-2 pg_stats stub to stats_stale → ann with
// the CAPPED probe count (exact_max+1) as estimate. Asserted from the table,
// not from a log line.
func TestSelectorW023_G2_SelValuesPerScope(t *testing.T) {
	pool := w023Setup(t)
	h := w023Handler(t, pool, config.NewStore(w023Config(true)))

	cases := []struct {
		name, scope, query   string
		wantMode, wantReason string
		wantEst              float64
	}{
		{
			name: "small scope decides exact", scope: w023SmallScope,
			query: "w023 g2 small alpha bravo", wantMode: "exact",
			wantReason: "probe<=exact_max", wantEst: w023SmallN,
		},
		{
			name: "large scope degrades to ann", scope: w023LargeScope,
			query: "w023 g2 large alpha bravo", wantMode: "ann",
			wantReason: "stats_stale", wantEst: w023ExactMax + 1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w023Query(t, h, tc.scope, tc.query)
			metas := w023Metadata(t, pool, tc.query)
			for i, m := range metas {
				if m["sel_mode"] != tc.wantMode {
					t.Errorf("row %d: sel_mode = %v, want %q", i, m["sel_mode"], tc.wantMode)
				}
				if m["sel_reason"] != tc.wantReason {
					t.Errorf("row %d: sel_reason = %v, want %q", i, m["sel_reason"], tc.wantReason)
				}
				if m["sel_est"] != tc.wantEst {
					t.Errorf("row %d: sel_est = %v, want %v", i, m["sel_est"], tc.wantEst)
				}
				// The Ist keys stay untouched next to the new ones.
				for _, k := range []string{"score", "scope", "source"} {
					if _, ok := m[k]; !ok {
						t.Errorf("row %d: Ist key %q disappeared: %v", i, k, m)
					}
				}
			}
			t.Logf("G2 GREEN (%s): %d rows, example metadata: %v", tc.name, len(metas), metas[0])
		})
	}
}

// TestSelectorW023_HygieneDisabledWritesNoSelKeys is the enabled=false
// companion (design §7 W02-3 G1, second half — a GREEN assert, not a red
// probe): with the master gate CLOSED the access-log rows are byte-identical
// to the pre-selector state. This is the shipped configuration, so it is also
// the eval.sh-baseline guard on the write side (G4).
func TestSelectorW023_HygieneDisabledWritesNoSelKeys(t *testing.T) {
	pool := w023Setup(t)
	h := w023Handler(t, pool, config.NewStore(w023Config(false)))

	const q = "w023 hygiene disabled alpha bravo"
	w023Query(t, h, w023SmallScope, q)

	metas := w023Metadata(t, pool, q)
	for i, m := range metas {
		if len(m) != 3 {
			t.Errorf("row %d: metadata has %d keys (%v), want exactly the 3 Ist keys", i, len(m), m)
		}
		for _, k := range []string{"sel_mode", "sel_reason", "sel_est"} {
			if _, bad := m[k]; bad {
				t.Errorf("row %d: %q written while the master gate is closed: %v", i, k, m)
			}
		}
	}
	t.Logf("HYGIENE GREEN: %d rows, Ist-identical metadata: %v", len(metas), metas[0])
}

// TestSelectorW023_G3_RuntimeFlip is gate G3 at the request level: false →
// true → false through config.Store.Replace, no restart, no new handler. The
// evidence is the metadata of three consecutive queries on the SAME handler
// instance.
func TestSelectorW023_G3_RuntimeFlip(t *testing.T) {
	pool := w023Setup(t)
	store := config.NewStore(w023Config(false))
	h := w023Handler(t, pool, store)

	const qOff1 = "w023 g3 off first alpha"
	const qOn = "w023 g3 on alpha"
	const qOff2 = "w023 g3 off second alpha"

	w023Query(t, h, w023SmallScope, qOff1)
	if m := w023Metadata(t, pool, qOff1)[0]; m["sel_mode"] != nil {
		t.Fatalf("pre-flip row already carries sel_mode: %v", m)
	}

	if err := store.Replace(w023Config(true)); err != nil {
		t.Fatalf("Replace(enabled=true): %v", err)
	}
	w023Query(t, h, w023SmallScope, qOn)
	if m := w023Metadata(t, pool, qOn)[0]; m["sel_mode"] != "exact" {
		t.Errorf("post-flip row: sel_mode = %v, want \"exact\" (hot flip not effective)", m["sel_mode"])
	}

	if err := store.Replace(w023Config(false)); err != nil {
		t.Fatalf("Replace(enabled=false): %v", err)
	}
	w023Query(t, h, w023SmallScope, qOff2)
	if m := w023Metadata(t, pool, qOff2)[0]; m["sel_mode"] != nil {
		t.Errorf("post-disarm row still carries sel_mode: %v", m)
	}
	t.Log("G3 GREEN: enabled false→true→false effective without a restart")
}
