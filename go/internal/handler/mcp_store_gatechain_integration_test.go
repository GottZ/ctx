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
//	h ProbeGateReachesAllArms — a gate hung into the chain (extraWriteGates)
//	                            answers identically on REST, MCP-direct and
//	                            MCP-staged. RED before T03-2: REST answered 200
//	                            and STORED the block, because HandleStore ran a
//	                            hand-written copy of the chain instead of the
//	                            chain. This is the DURABLE wiring probe (R4) —
//	                            the six probes above pin the seven gates, this
//	                            one pins that all three arms run THEM.
//	i ParentRequiredReachesAllArms — a type whose registry row says
//	                            parent.mode=required is refused on all three
//	                            arms with ONE code and ONE prose, and neither
//	                            written nor staged (T02-11). RED before: all
//	                            three accepted and stored, because
//	                            blocktype.Set.ParentMode had zero production
//	                            callers — a policy without a mechanism.
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

// gcProbeTitle is the marker the wiring probe's gate refuses (subtest h). It is
// a title no production write carries, so the gate is inert for every other
// subtest that shares the process.
const gcProbeTitle = "__gateprobe__"

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
		// GREEN pre-fix and after: a store call that names NO scope lands in
		// the key's home_scope with scopeExplicit=false — routing the arm
		// through runStageWriteGates must not move a single byte of that.
		// (Written under decision D4, when the tool had no scope field at all;
		// E-M4 made the field optional and this stayed the DEFAULT pin.)
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

	t.Run("h_ProbeGateReachesAllArms", func(t *testing.T) {
		// The wiring probe (R4). Every other subtest asserts over the SEVEN
		// production gates — and those are worded identically in a copy and in
		// the chain, so none of them can see whether an arm CALLS the chain. This
		// one hangs an eighth gate into extraWriteGates (stage_gates.go) and asks
		// all three arms the same question: a gate that only the chain knows
		// about must reach every surface that claims to run it.
		//
		// RED before T03-2 on the REST arm: HTTP 200 and the block in
		// context_blocks, while both MCP arms refused. GREEN after: one verdict,
		// three arms, identical code and identical prose.
		//
		// The class is built HERE rather than taken from errcode.go's closed
		// vocabulary: 418/gate_probe belongs to no production rejection, so a
		// match cannot be one of the seven gates firing by coincidence, and the
		// probe adds no code to the vocabulary docs/api.md publishes.
		probeClass := rejectClass{http.StatusTeapot, "gate_probe"}
		const probeMsg = "gate probe: this title is refused by the wiring probe gate"

		prev := extraWriteGates
		extraWriteGates = append(append([]func(storeRequest) *writeReject{}, prev...),
			func(req storeRequest) *writeReject {
				if req.Title == gcProbeTitle {
					return probeClass.reject(probeMsg)
				}
				return nil
			})
		t.Cleanup(func() { extraWriteGates = prev })

		// gcCoded is the full rejection surface a client can branch on. status
		// stays 0 on the MCP arms — that transport carries no HTTP status
		// (errcode.go: errResultReject drops rej.Status), so the comparison
		// below is over code and prose, and the status is pinned on REST alone.
		type gcCoded struct {
			rejected bool
			status   int
			code     string
			text     string
		}
		restRaw := func(keyCtx context.Context, in storeInput) gcCoded {
			t.Helper()
			body, _ := json.Marshal(map[string]any{
				"category": in.Category, "title": in.Title, "content": in.Content,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body)))
			req = req.WithContext(keyCtx)
			rec := httptest.NewRecorder()
			NewStoreHandler(pool, cfgWith(0), reg).HandleStore(rec, req)
			var resp struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			return gcCoded{
				rejected: rec.Code != http.StatusOK,
				status:   rec.Code,
				code:     resp.Code,
				text:     resp.Error,
			}
		}
		mcpRaw := func(keyCtx context.Context, in storeInput) gcCoded {
			t.Helper()
			r, _, err := mcpStoreHandler(mcpCfg(0))(keyCtx, nil, in)
			if err != nil {
				t.Fatalf("mcp store %q: protocol error %v", in.Title, err)
			}
			txt := resultText(t, r)
			out := gcCoded{
				// A STAGED answer is IsError=true and means the gates PASSED
				// (D3-C3) — only an IsError that is not a stage card is a
				// rejection.
				rejected: r.IsError && !strings.Contains(txt, "STAGED — NOT saved yet"),
				text:     txt,
			}
			if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
				out.code = env.Code
			}
			return out
		}

		_, restCtx, _ := mkKey("gc-h-rest", "private", nil, false)
		_, directCtx, _ := mkKey("gc-h-direct", "private", nil, false)
		stagedID, stagedCtx, _ := mkKey("gc-h-staged", "private", nil, true)

		// One payload per arm, differing in CONTENT: the MCP arms run their
		// scoped hash-NOOP check BEFORE the chain (mcp.go: resolveWriteScope +
		// IdenticalContentMatch ahead of the gate call), so a byte-identical payload
		// behind an arm that already stored it would answer "identical content
		// already exists" without ever reaching a gate — and the probe would
		// measure the NOOP instead of the wiring. The title is what the probe
		// gate keys on and stays the same on all three.
		arms := []struct {
			name string
			out  gcCoded
		}{
			{"rest", restRaw(restCtx, storeInput{
				Category: "test", Title: gcProbeTitle, Content: "probe payload for the REST arm"})},
			{"mcp-direct", mcpRaw(directCtx, storeInput{
				Category: "test", Title: gcProbeTitle, Content: "probe payload for the MCP-direct arm"})},
			{"mcp-staged", mcpRaw(stagedCtx, storeInput{
				Category: "test", Title: gcProbeTitle, Content: "probe payload for the MCP-staged arm"})},
		}
		for _, a := range arms {
			if !a.out.rejected {
				t.Errorf("[%s] accepted the probe payload, want the probe gate to refuse it (body %q)",
					a.name, a.out.text)
				continue
			}
			if a.out.code != probeClass.code {
				t.Errorf("[%s] code = %q, want %q", a.name, a.out.code, probeClass.code)
			}
			if a.out.text != probeMsg {
				t.Errorf("[%s] prose = %q, want %q", a.name, a.out.text, probeMsg)
			}
		}
		// Parity as an EQUALITY between the arms, not only against the constants
		// above: a future arm that renders its own wording is caught here even if
		// it picks the right class.
		for _, a := range arms[1:] {
			if a.out.code != arms[0].out.code || a.out.text != arms[0].out.text {
				t.Errorf("[%s] answers %q/%q, REST answers %q/%q — the arms diverge",
					a.name, a.out.code, a.out.text, arms[0].out.code, arms[0].out.text)
			}
		}
		if arms[0].out.status != http.StatusTeapot {
			t.Errorf("REST status = %d, want the probe class's %d", arms[0].out.status, http.StatusTeapot)
		}
		// The refusal has to STOP the write, on every arm — a verdict that is
		// rendered but not obeyed would satisfy the assertions above.
		if n := blockCount(gcProbeTitle); n != 0 {
			t.Errorf("%d block(s) titled %q stored, want the probe payload never written", n, gcProbeTitle)
		}
		if n := pendingCount(stagedID); n != 0 {
			t.Errorf("%d staged card(s) for the probe payload, want none", n)
		}

		// Counter-probe: with the marker gone the same three arms accept, so the
		// probe gate is a GATE and not a blanket refusal (an arm that rejected
		// everything would pass the block above).
		clean := []struct {
			name string
			out  gcCoded
		}{
			{"rest", restRaw(restCtx, storeInput{
				Category: "test", Title: "gc-h-rest-clean", Content: "no marker, ordinary write"})},
			{"mcp-direct", mcpRaw(directCtx, storeInput{
				Category: "test", Title: "gc-h-direct-clean", Content: "no marker, ordinary write"})},
			{"mcp-staged", mcpRaw(stagedCtx, storeInput{
				Category: "test", Title: "gc-h-staged-clean", Content: "no marker, ordinary write"})},
		}
		for _, a := range clean {
			if a.out.rejected {
				t.Errorf("[%s] refused an unmarked payload: %q", a.name, a.out.text)
			}
		}
	})

	t.Run("i_ParentRequiredReachesAllArms", func(t *testing.T) {
		// Welle T02-11 (design/02 §8 E02-4): parent.mode=required was a policy
		// the registry accepted and no write path delivered — blocktype.Set.
		// ParentMode resolved the value and had ZERO production callers, so a
		// tenant could configure orphan prevention and write orphans through
		// /api/store all the same. The fourth claim gate closes it.
		//
		// RED before T02-11: all three arms answer 200 / a stage card for a
		// required-parent type without a parent, and the block lands in
		// context_blocks. GREEN after: one verdict, three arms, identical code
		// and identical prose, nothing written and nothing staged.
		//
		// The class is the PRODUCTION class here (unlike subtest h's 418 probe):
		// the point of this wave is exactly that parent_required joins the
		// published rejection vocabulary of docs/api.md.
		//
		// BETRIEBS-NACHWEIS für fremde Installationen — der Sweep, den ein
		// Operator VOR dem Upgrade fährt, nennt die Population, deren generische
		// Schreibvorgänge ab jetzt abgelehnt werden:
		//
		//	SELECT scope, name, builtin FROM context_block_types
		//	 WHERE config->'parent'->>'mode' = 'required'
		//	 ORDER BY scope, name;
		//
		// Auf dieser Installation (2026-09-05, 18:52 UTC): genau EINE Zeile,
		// `_global | comment | t` — 11 Typen gesamt, 0 nicht-builtin. Deshalb
		// ist der Schnitt hier ein Minor und kein Bruch für einen Tenant.
		for _, seed := range []struct{ name, mode string }{
			{"gc-parent-required", blocktype.ParentModeRequired},
			{"gc-parent-optional", blocktype.ParentModeOptional},
			{"gc-parent-none", blocktype.ParentModeNone},
		} {
			if _, err := pool.Exec(ctx,
				`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
				 VALUES ($1::text, '_global', $1::text, false, false,
				         jsonb_build_object('v', 1, 'parent', jsonb_build_object('mode', $2::text)))`,
				seed.name, seed.mode); err != nil {
				t.Fatalf("seed fixture type %s: %v", seed.name, err)
			}
		}
		// A FRESH registry rather than reg.Reload: the shared reg belongs to the
		// subtests above and must keep the snapshot they booted with.
		regP := blocktype.NewRegistry()
		regP.Boot(ctx, pool)
		if regP.Health() != blocktype.HealthOK {
			t.Fatalf("registry boot degraded: %s — a fixture row does not decode", regP.Health())
		}
		// Premises, asserted rather than assumed: all four modes must be what
		// this subtest believes, or every verdict below proves something else.
		for _, p := range []struct{ name, want string }{
			{"gc-parent-required", blocktype.ParentModeRequired},
			{"gc-parent-optional", blocktype.ParentModeOptional},
			{"gc-parent-none", blocktype.ParentModeNone},
			{"comment", blocktype.ParentModeRequired}, // the one SHIPPED required type
		} {
			if got := regP.Snapshot().ParentMode(p.name); got != p.want {
				t.Fatalf("fixture ParentMode(%s) = %q, want %q — premise of this subtest",
					p.name, got, p.want)
			}
		}

		cfgP := staticConfigStore{cfg: &config.Config{
			Query:  config.QueryConfig{RateLimitWrite: 0},
			Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
			Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
		}}
		mcpCfgP := MCPConfig{Pool: pool, Cfg: cfgP, Blocktypes: regP}

		// Same shape as subtest h's gcCoded, but the drivers carry `type` —
		// which is the whole subject here and the one field restArm above drops.
		type pcCoded struct {
			rejected bool
			status   int
			code     string
			text     string
		}
		restTyped := func(keyCtx context.Context, in storeInput) pcCoded {
			t.Helper()
			body, _ := json.Marshal(map[string]any{
				"category": in.Category, "title": in.Title,
				"content": in.Content, "type": in.Type,
			})
			req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(body)))
			req = req.WithContext(keyCtx)
			rec := httptest.NewRecorder()
			NewStoreHandler(pool, cfgP, regP).HandleStore(rec, req)
			var resp struct {
				Error string `json:"error"`
				Code  string `json:"code"`
			}
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			return pcCoded{
				rejected: rec.Code != http.StatusOK,
				status:   rec.Code,
				code:     resp.Code,
				text:     resp.Error,
			}
		}
		mcpTyped := func(keyCtx context.Context, in storeInput) pcCoded {
			t.Helper()
			r, _, err := mcpStoreHandler(mcpCfgP)(keyCtx, nil, in)
			if err != nil {
				t.Fatalf("mcp store %q: protocol error %v", in.Title, err)
			}
			txt := resultText(t, r)
			out := pcCoded{
				rejected: r.IsError && !strings.Contains(txt, "STAGED — NOT saved yet"),
				text:     txt,
			}
			if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
				out.code = env.Code
			}
			return out
		}

		_, restCtx, _ := mkKey("gc-i-rest", "private", nil, false)
		_, directCtx, _ := mkKey("gc-i-direct", "private", nil, false)
		stagedID, stagedCtx, _ := mkKey("gc-i-staged", "private", nil, true)

		// One payload per arm, differing in CONTENT for the same reason subtest
		// h needs it: the MCP arms run their hash-NOOP check BEFORE the chain.
		probe := func(t *testing.T, typeName, title string) {
			wantMsg := fmt.Sprintf(
				"type: %q requires a parent (parent.mode=required) — this write surface carries "+
					"no parent, so a block of that type is created through its own domain path, "+
					"not claimed here", typeName)
			arms := []struct {
				name string
				out  pcCoded
			}{
				{"rest", restTyped(restCtx, storeInput{
					Category: "test", Title: title, Type: typeName,
					Content: "parent probe payload for the REST arm"})},
				{"mcp-direct", mcpTyped(directCtx, storeInput{
					Category: "test", Title: title, Type: typeName,
					Content: "parent probe payload for the MCP-direct arm"})},
				{"mcp-staged", mcpTyped(stagedCtx, storeInput{
					Category: "test", Title: title, Type: typeName,
					Content: "parent probe payload for the MCP-staged arm"})},
			}
			for _, a := range arms {
				if !a.out.rejected {
					t.Errorf("[%s] accepted type %q without a parent — parent.mode=required is a "+
						"policy without a mechanism (body %q)", a.name, typeName, a.out.text)
					continue
				}
				if a.out.code != "parent_required" {
					t.Errorf("[%s] code = %q, want parent_required", a.name, a.out.code)
				}
				if a.out.text != wantMsg {
					t.Errorf("[%s] prose = %q, want %q", a.name, a.out.text, wantMsg)
				}
			}
			// Parity as an EQUALITY between the arms, not only against the
			// constant: an arm that renders its own wording is caught here even
			// if it picks the right class.
			for _, a := range arms[1:] {
				if a.out.code != arms[0].out.code || a.out.text != arms[0].out.text {
					t.Errorf("[%s] answers %q/%q, REST answers %q/%q — the arms diverge",
						a.name, a.out.code, a.out.text, arms[0].out.code, arms[0].out.text)
				}
			}
			if arms[0].out.status != http.StatusUnprocessableEntity {
				t.Errorf("REST status = %d, want %d (the type-claim class)",
					arms[0].out.status, http.StatusUnprocessableEntity)
			}
			// A verdict that is rendered but not obeyed would satisfy everything
			// above; the refusal has to STOP the write on every arm.
			if n := blockCount(title); n != 0 {
				t.Errorf("%d block(s) titled %q stored, want the payload never written", n, title)
			}
		}

		t.Run("fixture_type", func(t *testing.T) { probe(t, "gc-parent-required", "gc-i-required") })
		// The SHIPPED case: `comment` is builtin required and is NOT
		// write.internal_only, so before this wave a client could claim it on
		// /api/store and receive a comment block with parent_id NULL — the exact
		// orphan the registry row forbids. Its own domain path
		// (store.InsertCommentBlock behind the issue verbs) is untouched and
		// keeps ErrCommentParentRequired as its FIRST refusal.
		t.Run("shipped_comment_type", func(t *testing.T) { probe(t, "comment", "gc-i-comment") })

		if n := pendingCount(stagedID); n != 0 {
			t.Errorf("%d staged card(s) for the refused payloads, want none — a card that "+
				"cannot be confirmed must never be issued", n)
		}

		// Counter-probe: optional and none stay claimable on all three arms, so
		// the gate is a GATE and not a blanket refusal of every explicit type.
		for _, typeName := range []string{"gc-parent-optional", "gc-parent-none"} {
			for _, a := range []struct {
				name string
				out  pcCoded
			}{
				{"rest", restTyped(restCtx, storeInput{
					Category: "test", Title: "gc-i-ok-rest-" + typeName, Type: typeName,
					Content: "control payload for the REST arm, " + typeName})},
				{"mcp-direct", mcpTyped(directCtx, storeInput{
					Category: "test", Title: "gc-i-ok-direct-" + typeName, Type: typeName,
					Content: "control payload for the MCP-direct arm, " + typeName})},
				{"mcp-staged", mcpTyped(stagedCtx, storeInput{
					Category: "test", Title: "gc-i-ok-staged-" + typeName, Type: typeName,
					Content: "control payload for the MCP-staged arm, " + typeName})},
			} {
				if a.out.rejected {
					t.Errorf("[%s] refused type %q: %s/%q — only parent.mode=required is gated",
						a.name, typeName, a.out.code, a.out.text)
				}
			}
		}
	})
}
