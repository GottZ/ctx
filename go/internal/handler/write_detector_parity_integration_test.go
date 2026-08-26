//go:build integration

// Wissens-Ebenen V-W8 (design/05 §7 row V-W8, §5 B3): the handler half of the
// asymmetry probe, plus the staged-write contract that must survive the move
// of the G40 detector into store.UpsertBlock.
//
// Subtests:
//
//	a_RestArmRaises      — POST /api/store with a key shape ⇒ credentials/pattern/trace.
//	                       GREEN before and after: this is the side the asymmetry compares AGAINST.
//	b_PathParity         — the same content over REST and over a bare store.UpsertBlock
//	                       (the digest/dream/ingest argument shape) ⇒ identical row.
//	                       RED before V-W8: the in-process row reads 'default' with no trace.
//	c_StagedConfirmContract — stage ⇒ payload keeps SensitivityDetect (hash-bound,
//	                       store/confirm_payload.go:47-51); confirm ⇒ credentials/pattern with the
//	                       trace present exactly once. GREEN before and after (payload-hash contract).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestWriteDetectorPathParity -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// wdCred carries an AWS access key id shape (AKIA + 16 base32 chars) —
// sensitivity.Scan rule "aws-key", the first-matching structured rule.
// Synthetic: the payload is a constant run, never a real credential.
var wdCred = "rotation note: AKIA" + strings.Repeat("Z", 16) + " showed up in a paste"

func TestWriteDetectorPathParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// Default sensitivity PUBLIC on purpose: the production default is
	// 'credentials', which would mask the detector upgrade — the value must
	// be seen to MOVE.
	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Cfg: cfg, Blocktypes: reg}

	mkKey := func(name string, flagged bool) (string, context.Context) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, name, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", name, err)
		}
		if flagged {
			if _, err := pool.Exec(ctx,
				`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("opt in %s: %v", name, err)
			}
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v (valid=%v)", name, err, ar != nil && ar.IsValid)
		}
		return row.ID, context.WithValue(ctx, authResultKey, ar)
	}
	wdRow := func(title string) (sens, source string, md map[string]any) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT sensitivity, sensitivity_source, metadata
			   FROM context_blocks WHERE title = $1 AND NOT is_archived`,
			title).Scan(&sens, &source, &md); err != nil {
			t.Fatalf("read block %q: %v", title, err)
		}
		return
	}
	restStore := func(keyCtx context.Context, title, content string) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"category": "learnings", "title": title, "content": content,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body)))
		req = req.WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("REST store %q: status %d body %s", title, rec.Code, rec.Body.String())
		}
	}

	t.Run("a_RestArmRaises", func(t *testing.T) {
		const title = "vw8 rest arm with a key"
		_, keyCtx := mkKey("wd-a-rest", false)
		restStore(keyCtx, title, wdCred)

		sens, source, md := wdRow(title)
		if sens != "credentials" || source != "pattern" {
			t.Fatalf("REST row = %s/%s, want credentials/pattern", sens, source)
		}
		trace, ok := md["sensitivity_detector"].(map[string]any)
		if !ok {
			t.Fatalf("metadata.sensitivity_detector missing, got %v", md)
		}
		if trace["kind"] != "aws-key" || trace["reason"] != "AWS access key id pattern" {
			t.Errorf("trace = %v, want kind=aws-key with the secret-free reason", trace)
		}
	})

	t.Run("b_PathParity", func(t *testing.T) {
		// The asymmetry the wave closes: the SAME content, one write over the
		// gated REST path, one over the bare store.UpsertBlock call every
		// in-process writer makes (digest.go:146 argument shape).
		const restTitle, procTitle = "vw8 parity rest", "vw8 parity in-process"
		_, keyCtx := mkKey("wd-b-rest", false)
		restStore(keyCtx, restTitle, wdCred)

		if _, err := store.UpsertBlock(ctx, pool, "learnings", procTitle, wdCred, nil, nil,
			"private", true, store.SensitivityWrite{}, ""); err != nil {
			t.Fatalf("in-process upsert: %v", err)
		}

		restSens, restSource, restMD := wdRow(restTitle)
		procSens, procSource, procMD := wdRow(procTitle)
		if restSens != procSens || restSource != procSource {
			t.Errorf("paths diverge: REST %s/%s vs in-process %s/%s",
				restSens, restSource, procSens, procSource)
		}
		if !reflect.DeepEqual(restMD, procMD) {
			t.Errorf("detector metadata diverges:\n REST       %v\n in-process %v", restMD, procMD)
		}
		if procSource != "pattern" {
			t.Errorf("in-process sensitivity_source = %q, want pattern", procSource)
		}
	})

	t.Run("c_StagedConfirmContract", func(t *testing.T) {
		// The staged path pins the verdict INTO the canonical payload
		// (SensitivityDetect, hash-bound), so the handler-side detector call
		// must stay where it is. Stage, read the server-held payload, confirm,
		// and check the executed block.
		const title = "vw8 staged key through confirm"
		keyID, keyCtx := mkKey("wd-c-staged", true)

		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{
			Category: "learnings", Title: title, Content: wdCred,
		})
		if err != nil {
			t.Fatalf("mcp store: protocol error %v", err)
		}
		if txt := resultText(t, r); !strings.Contains(txt, "STAGED — NOT saved yet") {
			t.Fatalf("flagged key did not stage: %s", txt)
		}

		var (
			raw  []byte
			hash string
		)
		if err := pool.QueryRow(ctx,
			`SELECT payload, payload_hash FROM context_pending_writes
			  WHERE api_key_id = $1::uuid AND payload->>'title' = $2
			  ORDER BY created_at DESC LIMIT 1`, keyID, title).Scan(&raw, &hash); err != nil {
			t.Fatalf("read staged payload: %v", err)
		}
		var cw store.CanonicalWrite
		if err := json.Unmarshal(raw, &cw); err != nil {
			t.Fatalf("decode staged payload: %v", err)
		}
		if !cw.SensitivityDetect {
			t.Error("staged payload SensitivityDetect = false, want the pattern provenance in the hash")
		}
		if cw.Sensitivity != "credentials" || cw.SensitivityManual {
			t.Errorf("staged payload sensitivity = %q manual=%v, want credentials/false",
				cw.Sensitivity, cw.SensitivityManual)
		}
		if _, ok := cw.Metadata["sensitivity_detector"]; !ok {
			t.Errorf("staged payload metadata carries no trace: %v", cw.Metadata)
		}

		cr, _, err := mcpConfirmHandler(mcpCfg)(keyCtx, nil, confirmInput{PayloadHash: hash})
		if err != nil {
			t.Fatalf("confirm: protocol error %v", err)
		}
		if cr.IsError {
			t.Fatalf("confirm rejected: %s", resultText(t, cr))
		}
		sens, source, md := wdRow(title)
		if sens != "credentials" || source != "pattern" {
			t.Errorf("confirmed row = %s/%s, want credentials/pattern", sens, source)
		}
		trace, ok := md["sensitivity_detector"].(map[string]any)
		if !ok {
			t.Fatalf("confirmed block lost the trace: %v", md)
		}
		if trace["kind"] != "aws-key" {
			t.Errorf("trace kind = %v, want aws-key", trace["kind"])
		}
		// Applied twice on this path (stage gates + store) — the trace is one
		// JSON key, so it can only ever be present once.
		if n := len(md); n != 1 {
			t.Errorf("confirmed metadata has %d keys (%v), want exactly sensitivity_detector", n, md)
		}
	})
}
