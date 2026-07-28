//go:build integration

// Gap-C6-a integration probes: BOTH MCP store arms run the full write-gate
// chain, and BOTH book their write intent.
//
// Ist-Stand before Gap-C6-a (after H-W8, 521a519):
//
//   - the DIRECT arm (mcpStoreHandler) ran hand-rolled single gates — a rate
//     limit and an inline sensitivity copy — but NOT blockSizeLimit and NOT
//     applyWriteDetector. A 51 KB block and a ghp_ token both walked straight
//     into context_blocks, while the very same payload was rejected / upgraded
//     on REST /api/store and on the STAGED MCP arm (both run
//     runStageWriteGates).
//   - the STAGED arm (mcpStageStore → store.StagePendingWrite) CHECKED the
//     write limit (via runStageWriteGates) but never BOOKED one — no
//     context_access_log row with action='write' was ever written for a stage,
//     so store.CheckRateLimit read 0 forever and the limit could not bite a
//     purely staging key.
//
// Subtests (probe letters from the wave brief):
//
//	a DirectArmSizeCap        — 51 KB over the direct arm ⇒ IsError. RED: stored.
//	b DirectArmDetector       — ghp_ token ⇒ credentials + detector trace. RED: default.
//	c DirectArmBooksOneWrite  — H-W8 regression: exactly one write row. GREEN pre-fix.
//	d StagedArmObeysLimit     — N+1 staged stores ⇒ the (N+1)-th rejected. RED: stages.
//	e GateParityAcrossArms    — REST | MCP-direct | MCP-staged decide identically. RED.
//	f NoDoubleBooking         — one staged store ⇒ exactly ONE write row. RED: zero.
//	g ScopeGolden             — no scope field ⇒ HomeScope, unchanged. GREEN pre-fix.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPStoreGateChain -count=1 -v
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

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// gcCredContent carries a GitHub PAT shape (ghp_ + 36 alnum) — sensitivity.Scan
// rule "token-prefix". Deliberately the FIRST-matching structured rule, so the
// expected Kind is stable regardless of the surrounding prose.
const gcCredContent = "rotation note: ghp_0123456789abcdefghijABCDEFGHIJ012345 leaked into a paste"

// gcOversize is 52224 bytes — past the shared 50 KB content cap. 'x' repeats
// carry zero entropy, so the payload trips the SIZE gate and nothing else.
func gcOversize() string { return strings.Repeat("x", 51*1024) }

// gcOutcome is the arm-independent gate verdict: accepted (the write happened
// or was staged) vs. rejected with a reason. Probe (e) compares these across
// REST, MCP-direct and MCP-staged — the parity claim is exactly this shape.
type gcOutcome struct {
	accepted bool
	reason   string
}

func TestMCPStoreGateChain(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// Default sensitivity PUBLIC on purpose: the production default is
	// 'credentials' (config.go pool.default_block_sensitivity), which would
	// mask a detector upgrade — probe (b) must see the value MOVE.
	cfgWith := func(limit int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query:  config.QueryConfig{RateLimitWrite: limit},
			Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
			Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
		}}
	}
	mcpCfg := func(limit int) MCPConfig {
		return MCPConfig{Pool: pool, Cfg: cfgWith(limit), Blocktypes: reg}
	}

	// writeRows counts the write-action rows of one key — exactly the
	// population store.CheckRateLimit aggregates over.
	writeRows := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			 WHERE api_key_id = $1::uuid AND action = 'write'`, keyID).Scan(&n); err != nil {
			t.Fatalf("count write rows: %v", err)
		}
		return n
	}
	mkKey := func(name, homeScope string, allowed []string, flagged bool) (string, context.Context, *auth.AuthResult) {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, name, homeScope, allowed, store.DefaultTenantID)
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
		return row.ID, context.WithValue(ctx, authResultKey, ar), ar
	}
	blockCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blocks %q: %v", title, err)
		}
		return n
	}
	// blockRow reads the write-path-relevant columns of a stored block.
	blockRow := func(title string) (scope, sens, source string, md map[string]any) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT scope, sensitivity, sensitivity_source, metadata
			   FROM context_blocks WHERE title = $1`, title).Scan(&scope, &sens, &source, &md); err != nil {
			t.Fatalf("read block %q: %v", title, err)
		}
		return
	}
	// stagedPayload decodes the server-held canonical payload of ONE key's
	// stage for a given title — the staged arm's equivalent of a stored row.
	// Selecting by title, not by recency: a subtest that stages several
	// payloads must still be able to name the one it asserts about.
	stagedPayload := func(keyID, title string) store.CanonicalWrite {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT payload FROM context_pending_writes
			  WHERE api_key_id = $1::uuid AND payload->>'title' = $2
			  ORDER BY created_at DESC LIMIT 1`, keyID, title).Scan(&raw); err != nil {
			t.Fatalf("read staged payload %q: %v", title, err)
		}
		var cw store.CanonicalWrite
		if err := json.Unmarshal(raw, &cw); err != nil {
			t.Fatalf("decode staged payload: %v", err)
		}
		return cw
	}
	pendingCount := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_pending_writes WHERE api_key_id = $1::uuid`, keyID).Scan(&n); err != nil {
			t.Fatalf("count pending: %v", err)
		}
		return n
	}

	// --- arm drivers: one payload in, one gcOutcome out --------------------

	directArm := func(keyCtx context.Context, limit int, in storeInput) gcOutcome {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg(limit))(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp direct store %q: protocol error %v", in.Title, err)
		}
		if r.IsError {
			return gcOutcome{reason: resultText(t, r)}
		}
		return gcOutcome{accepted: true}
	}
	// stagedArm maps the D3-C3 contract: a STAGED result is IsError=true but
	// means the gates PASSED — only a non-staging IsError is a rejection.
	stagedArm := func(keyCtx context.Context, limit int, in storeInput) gcOutcome {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg(limit))(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp staged store %q: protocol error %v", in.Title, err)
		}
		txt := resultText(t, r)
		if strings.Contains(txt, "STAGED — NOT saved yet") {
			return gcOutcome{accepted: true}
		}
		return gcOutcome{reason: txt}
	}
	restArm := func(keyCtx context.Context, limit int, in storeInput) gcOutcome {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"category": in.Category, "title": in.Title, "content": in.Content,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body)))
		req = req.WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfgWith(limit), reg).HandleStore(rec, req)
		if rec.Code == http.StatusOK {
			return gcOutcome{accepted: true}
		}
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return gcOutcome{reason: resp.Error}
	}

	t.Run("a_DirectArmSizeCap", func(t *testing.T) {
		// RED pre-Gap-C6-a: the direct arm never called blockSizeLimit, so a
		// 51 KB payload was written — while REST answered 413 for the same bytes.
		_, keyCtx, _ := mkKey("gc-a-size", "private", nil, false)

		out := directArm(keyCtx, 0, storeInput{
			Category: "test", Title: "gc-a-oversize", Content: gcOversize(),
		})
		if out.accepted {
			t.Fatalf("direct MCP arm accepted a 51 KB block, want the 50 KB cap to reject it")
		}
		if !strings.Contains(out.reason, "Content exceeds 50KB") {
			t.Errorf("rejection reason = %q, want it to name the 50KB content cap", out.reason)
		}
		if got := blockCount("gc-a-oversize"); got != 0 {
			t.Errorf("oversize call wrote %d block(s), want 0", got)
		}
	})

	t.Run("b_DirectArmDetector", func(t *testing.T) {
		// RED pre-Gap-C6-a: no applyWriteDetector on the direct arm ⇒ the token
		// block stayed at the settings default with source 'default' and no trace.
		_, keyCtx, _ := mkKey("gc-b-detector", "private", nil, false)

		if out := directArm(keyCtx, 0, storeInput{
			Category: "test", Title: "gc-b-token", Content: gcCredContent,
		}); !out.accepted {
			t.Fatalf("credentials payload must still be STORED, got rejected: %s", out.reason)
		}

		_, sens, source, md := blockRow("gc-b-token")
		if sens != "credentials" {
			t.Errorf("sensitivity = %q, want credentials (G40 detector upgrade)", sens)
		}
		if source != "pattern" {
			t.Errorf("sensitivity_source = %q, want pattern", source)
		}
		trace, ok := md["sensitivity_detector"].(map[string]any)
		if !ok {
			t.Fatalf("metadata.sensitivity_detector missing, got metadata %v", md)
		}
		if trace["kind"] != "token-prefix" {
			t.Errorf("detector kind = %v, want token-prefix", trace["kind"])
		}
		if trace["reason"] != "vendor API token prefix" {
			t.Errorf("detector reason = %v, want the secret-free vendor-prefix reason", trace["reason"])
		}
	})

	t.Run("c_DirectArmBooksOneWrite", func(t *testing.T) {
		// H-W8 regression guard — GREEN pre-fix, must stay green.
		keyID, keyCtx, _ := mkKey("gc-c-producer", "private", nil, false)

		if out := directArm(keyCtx, 0, storeInput{
			Category: "test", Title: "gc-c-block", Content: "one direct store, one write row",
		}); !out.accepted {
			t.Fatalf("direct store rejected: %s", out.reason)
		}
		if got := writeRows(keyID); got != 1 {
			t.Fatalf("direct MCP store booked %d write rows, want exactly 1", got)
		}
	})

	t.Run("d_StagedArmObeysLimit", func(t *testing.T) {
		// RED pre-Gap-C6-a: the staged arm CHECKS the limit but never FEEDS it,
		// so writeCount stays 0 and stage number N+1 is staged like the rest.
		const limit = 2
		keyID, keyCtx, _ := mkKey("gc-d-staged", "private", nil, true)

		for i := 1; i <= limit; i++ {
			out := stagedArm(keyCtx, limit, storeInput{
				Category: "test",
				Title:    fmt.Sprintf("gc-d-staged-%d", i),
				Content:  fmt.Sprintf("budgeted stage %d", i),
			})
			if !out.accepted {
				t.Fatalf("stage %d must fit the budget, rejected: %s", i, out.reason)
			}
		}
		if got := writeRows(keyID); got != limit {
			t.Errorf("%d staged stores booked %d write rows, want %d (the limiter reads exactly these)",
				limit, got, limit)
		}

		out := stagedArm(keyCtx, limit, storeInput{
			Category: "test", Title: "gc-d-staged-over", Content: "over budget stage",
		})
		if out.accepted {
			t.Fatalf("stage %d must be rate-limited, got staged", limit+1)
		}
		if !strings.Contains(out.reason, "Rate limit exceeded") {
			t.Errorf("rejection reason = %q, want it to name the rate limit", out.reason)
		}
		if got := pendingCount(keyID); got != limit {
			t.Errorf("rate-limited call staged anyway (%d pending rows), want it to stay at %d", got, limit)
		}
		if got := writeRows(keyID); got != limit {
			t.Errorf("rate-limited call booked a write row (%d rows), want it to stay at %d", got, limit)
		}
	})

	t.Run("e_GateParityAcrossArms", func(t *testing.T) {
		// RED pre-Gap-C6-a on the oversize case: REST and MCP-staged reject,
		// MCP-direct accepts. Also the mutation canary for "three of the four
		// gates" — dropping any gate from the shared chain splits this table.
		_, restCtx, restAR := mkKey("gc-e-rest", "private", nil, false)
		_, directCtx, _ := mkKey("gc-e-direct", "private", nil, false)
		stagedID, stagedCtx, _ := mkKey("gc-e-staged", "private", nil, true)

		cases := []struct {
			name         string
			content      string
			wantAccepted bool
			wantReason   string
		}{
			{"oversize", gcOversize(), false, "Content exceeds 50KB"},
			{"credentials", gcCredContent, true, ""},
			{"clean", "an ordinary sentence with nothing sensitive in it", true, ""},
		}
		for _, tc := range cases {
			arms := []struct {
				arm string
				out gcOutcome
			}{
				{"rest", restArm(restCtx, 0, storeInput{
					Category: "test", Title: "gc-e-rest-" + tc.name, Content: tc.content})},
				{"mcp-direct", directArm(directCtx, 0, storeInput{
					Category: "test", Title: "gc-e-direct-" + tc.name, Content: tc.content})},
				{"mcp-staged", stagedArm(stagedCtx, 0, storeInput{
					Category: "test", Title: "gc-e-staged-" + tc.name, Content: tc.content})},
			}
			for _, a := range arms {
				if a.out.accepted != tc.wantAccepted {
					t.Errorf("[%s/%s] accepted=%v, want %v (reason %q)",
						tc.name, a.arm, a.out.accepted, tc.wantAccepted, a.out.reason)
					continue
				}
				if !tc.wantAccepted && !strings.Contains(a.out.reason, tc.wantReason) {
					t.Errorf("[%s/%s] reason = %q, want it to contain %q",
						tc.name, a.arm, a.out.reason, tc.wantReason)
				}
			}
		}

		// The credentials verdict must be identical in SUBSTANCE too, not just
		// in accept/reject: same resolved sensitivity, same provenance. The
		// MCP-direct comparison is the RED one (it read 'public'/'default'
		// pre-fix); the staged assertions below are the ANCHOR — that arm
		// already ran the chain, and its verdict must not move.
		_, restSens, restSource, _ := blockRow("gc-e-rest-credentials")
		_, dirSens, dirSource, _ := blockRow("gc-e-direct-credentials")
		if restSens != dirSens || restSource != dirSource {
			t.Errorf("credentials verdict diverges: REST %s/%s vs MCP-direct %s/%s",
				restSens, restSource, dirSens, dirSource)
		}
		cw := stagedPayload(stagedID, "gc-e-staged-credentials")
		if cw.Sensitivity != restSens {
			t.Errorf("staged payload sensitivity = %q, want the REST verdict %q", cw.Sensitivity, restSens)
		}
		if !cw.SensitivityDetect {
			t.Errorf("staged payload sensitivity_detector = false, want the pattern provenance")
		}
		if restAR.HomeScope != cw.Scope {
			t.Errorf("staged payload scope = %q, want the home scope %q", cw.Scope, restAR.HomeScope)
		}
	})

	t.Run("f_NoDoubleBooking", func(t *testing.T) {
		// RED pre-Gap-C6-a at ZERO (the staged arm books nothing). RED at TWO
		// against a variant that books inside the gate chain AND at the stage
		// site — the intent must be charged exactly once per call.
		stagedID, stagedCtx, _ := mkKey("gc-f-staged", "private", nil, true)
		if out := stagedArm(stagedCtx, 0, storeInput{
			Category: "test", Title: "gc-f-staged-block", Content: "one stage, one write row",
		}); !out.accepted {
			t.Fatalf("stage rejected: %s", out.reason)
		}
		time.Sleep(300 * time.Millisecond) // settle window: a second booking would surface
		if got := writeRows(stagedID); got != 1 {
			t.Fatalf("one staged store booked %d write rows, want exactly 1", got)
		}

		directID, directCtx, _ := mkKey("gc-f-direct", "private", nil, false)
		if out := directArm(directCtx, 0, storeInput{
			Category: "test", Title: "gc-f-direct-block", Content: "one direct store, one write row",
		}); !out.accepted {
			t.Fatalf("direct store rejected: %s", out.reason)
		}
		time.Sleep(300 * time.Millisecond)
		if got := writeRows(directID); got != 1 {
			t.Fatalf("one direct store booked %d write rows, want exactly 1", got)
		}

		// A hash NOOP changes nothing, so it must neither book nor be charged
		// (H-W8 semantics, kept by Gap-C6-a).
		if out := directArm(directCtx, 0, storeInput{
			Category: "test", Title: "gc-f-direct-block", Content: "one direct store, one write row",
		}); !out.accepted {
			t.Fatalf("hash NOOP must answer success, got: %s", out.reason)
		}
		time.Sleep(300 * time.Millisecond)
		if got := writeRows(directID); got != 1 {
			t.Fatalf("hash NOOP booked a write row (%d rows), want it to stay at 1", got)
		}
	})

	t.Run("g_ScopeGolden", func(t *testing.T) {
		// GREEN pre-fix and after: the MCP store tool carries NO scope field
		// (decision D4), so the block lands in the key's home_scope with
		// scopeExplicit=false — routing the arm through runStageWriteGates must
		// not move a single byte of that.
		_, privCtx, privAR := mkKey("gc-g-private", "private", nil, false)
		if privAR.HomeScope != "private" {
			t.Fatalf("fixture: home scope = %q, want private", privAR.HomeScope)
		}
		if out := directArm(privCtx, 0, storeInput{
			Category: "test", Title: "gc-g-private-block", Content: "lands in the private home scope",
		}); !out.accepted {
			t.Fatalf("private store rejected: %s", out.reason)
		}
		if scope, _, _, _ := blockRow("gc-g-private-block"); scope != privAR.HomeScope {
			t.Errorf("block scope = %q, want the home scope %q", scope, privAR.HomeScope)
		}

		// Non-trivial fixture: a key whose home scope is NOT 'private' must
		// land there too (proves the resolution is read from the key, not
		// hardcoded).
		_, shCtx, shAR := mkKey("gc-g-shared", "shared", []string{"shared"}, false)
		if shAR.HomeScope != "shared" {
			t.Fatalf("fixture: home scope = %q, want shared", shAR.HomeScope)
		}
		if out := directArm(shCtx, 0, storeInput{
			Category: "test", Title: "gc-g-shared-block", Content: "lands in the shared home scope",
		}); !out.accepted {
			t.Fatalf("shared store rejected: %s", out.reason)
		}
		if scope, _, _, _ := blockRow("gc-g-shared-block"); scope != "shared" {
			t.Errorf("block scope = %q, want shared", scope)
		}
	})
}
