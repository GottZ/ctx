//go:build integration

// Integration test for MT wave T35b (Achse 04-W3, chain+embed subset): the
// chained Q-only llm_log rows (translate/temporal/rerank, via ChainCall) and
// the embed-wire rows (via LogEmbedWire) now carry the caller's api_key_id,
// completing the caller attribution T35a began for the synthesis path
// (design/04 §4.3, §7 W3). Foreground carries the key; background paths (dream,
// scheduler backfill, sensitivity-audit classify) keep it NULL by construction —
// no caller. An empty key stays NULL (nullUUID, llmlog.go:168).
//
// External test package (llm_test): internal/testdb → store → rrf → llm
// (rrf/rerank.go uses llm.ChainCall) would cycle for an internal test. A bare
// httptest server stands in for the chat backend. llmlog.Record inserts
// asynchronously (go insert), so the assertions poll until the rows land.
//
//	go test -tags=integration ./internal/llm/ -run TestChainAndEmbed_AttributesAPIKeyID -count=1 -v
package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/testdb"
)

// waitForPipelineRows blocks until at least want rows for the given pipeline
// have landed (llmlog.Record inserts in a goroutine).
func waitForPipelineRows(t *testing.T, pool *pgxpool.Pool, pipeline string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(context.Background(),
			`SELECT count(*) FROM context_llm_log WHERE pipeline = $1`, pipeline).Scan(&n); err != nil {
			t.Fatalf("count rows: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d/%d %q rows landed (async Record)", n, want, pipeline)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// api_key_id has no FK (054:11-13, "key delete must not anonymize history"),
// so an arbitrary well-formed UUID stands in for a caller key.
const attrKey = "00000000-0000-0000-0000-0000000000bb"

func TestChainAndEmbed_AttributesAPIKeyID(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"completion_tokens":1,"prompt_tokens":1}}`)
	}))
	defer srv.Close()

	bpool := backends.NewPool(nil, nil)
	bpool.SeedSnapshotForTest([]backends.Backend{{
		ID: "wire", Name: "wire", Host: srv.URL, Protocol: backends.ProtocolOpenAI, Model: "m",
		Trust: backends.TrustFull, Enabled: true, Roles: []string{backends.RoleTranslate},
	}})

	// --- ChainCall.Do (translate/temporal/rerank class): foreground carries the key. ---
	if _, err := (llm.ChainCall{
		Pool: bpool, Role: backends.RoleTranslate, Required: backends.SensInternal,
		Pipeline: "query-translate", System: "s", User: "u",
		DefTimeout: 10 * time.Second, APIKeyID: attrKey,
	}).Do(context.Background(), pool, integrationAdmission(t)); err != nil {
		t.Fatalf("ChainCall.Do: %v", err)
	}
	waitForPipelineRows(t, pool, "query-translate", 1)
	var chainKey *string
	if err := pool.QueryRow(context.Background(),
		`SELECT api_key_id::text FROM context_llm_log
		  WHERE pipeline = 'query-translate' ORDER BY created_at DESC LIMIT 1`).Scan(&chainKey); err != nil {
		t.Fatalf("read chain row: %v", err)
	}
	if chainKey == nil || *chainKey != attrKey {
		t.Fatalf("ChainCall api_key_id = %v, want %s (translate/temporal/rerank were NULL before T35b)", chainKey, attrKey)
	}

	// Empty key (the background invariant) stays NULL — nullUUID drops it.
	if _, err := (llm.ChainCall{
		Pool: bpool, Role: backends.RoleTranslate, Required: backends.SensInternal,
		Pipeline: "query-translate", System: "s", User: "u",
		DefTimeout: 10 * time.Second, APIKeyID: "",
	}).Do(context.Background(), pool, integrationAdmission(t)); err != nil {
		t.Fatalf("ChainCall.Do (empty): %v", err)
	}
	waitForPipelineRows(t, pool, "query-translate", 2)
	var chainNull int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_llm_log WHERE pipeline = 'query-translate' AND api_key_id IS NULL`).Scan(&chainNull); err != nil {
		t.Fatalf("count chain null: %v", err)
	}
	if chainNull != 1 {
		t.Fatalf("ChainCall NULL rows = %d, want 1 (empty key → NULL)", chainNull)
	}

	// --- LogEmbedWire (embed path): foreground carries the key, background NULL. ---
	// served=nil keeps the row minimal (no ModelFor lookup); api_key_id is what we assert.
	llm.LogEmbedWire(pool, "query-embed", backends.RoleEmbed, backends.SensInternal,
		nil, 1, time.Millisecond, []string{"00000000-0000-0000-0000-000000000001"}, nil, attrKey)
	llm.LogEmbedWire(pool, "embed-backfill", backends.RoleEmbed, backends.SensInternal,
		nil, 1, time.Millisecond, []string{"00000000-0000-0000-0000-000000000002"}, nil, "")
	waitForPipelineRows(t, pool, "query-embed", 1)
	waitForPipelineRows(t, pool, "embed-backfill", 1)

	var embKey *string
	if err := pool.QueryRow(context.Background(),
		`SELECT api_key_id::text FROM context_llm_log
		  WHERE pipeline = 'query-embed' ORDER BY created_at DESC LIMIT 1`).Scan(&embKey); err != nil {
		t.Fatalf("read embed row: %v", err)
	}
	if embKey == nil || *embKey != attrKey {
		t.Fatalf("query-embed api_key_id = %v, want %s (embed wire was NULL before T35b)", embKey, attrKey)
	}
	var backfillKey *string
	if err := pool.QueryRow(context.Background(),
		`SELECT api_key_id::text FROM context_llm_log
		  WHERE pipeline = 'embed-backfill' ORDER BY created_at DESC LIMIT 1`).Scan(&backfillKey); err != nil {
		t.Fatalf("read backfill row: %v", err)
	}
	if backfillKey != nil {
		t.Fatalf("embed-backfill api_key_id = %v, want NULL (maintenance, not caller-attributed)", *backfillKey)
	}
}

// integrationAdmission is the MW3 pass-through admission for integration
// call sites (empty policy = Durchreiche; background avoids B8 noise).
func integrationAdmission(t *testing.T) llm.Admission {
	t.Helper()
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	return llm.Admission{Admitter: d, Class: dispatch.ClassBackground}
}
