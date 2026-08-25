//go:build integration

// E-M4: the MCP write tools `store` and `blob_store` take an OPTIONAL `scope`,
// gated by exactly the REST gate — decision D4 (no scope field on MCP writes)
// is superseded as of 2026-08-25.
//
// Ist-Stand before this wave: both tools resolved the write scope to
// ar.HomeScope unconditionally. A `scope` key on the wire was not refused —
// it was silently DROPPED by the JSON decode (no such struct field), so a
// client asking for a scope it may legitimately write got a block/blob in its
// home scope and a success answer. One principal, two answers for one scope:
// REST /api/store and /api/blob/store have taken an explicit `scope` through
// writableBlockScopes since forever, while the MCP transport of the same key
// could not reach it at all.
//
// The probes drive the tools through their WIRE form (map → JSON →
// json.Unmarshal into the tool input) rather than filling the Go struct: that
// is what an MCP client actually sends, and it is what makes these probes RED
// against the pre-wave tree instead of merely uncompilable — an ignored field
// lands in the home scope and the assertion sees it.
//
//	a BlockExplicitScope   — store{scope:b} ⇒ block in b (RED: home scope a)
//	b BlobExplicitScope    — blob_store{scope:b} ⇒ blob in b (RED: home scope a)
//	c ForeignScopeDenied   — both tools, unwritable scope ⇒ code scope_denied,
//	                         prose byte-identical to REST, nothing written and
//	                         NO budget booked (the scope gate precedes the
//	                         budget on both paths — pinned with a budget of 1
//	                         that is already spent: the answer must still be
//	                         scope_denied, never rate_limit)
//	d AbsentScopeGolden    — no field ⇒ home scope, for a key that demonstrably
//	                         holds a second write scope (regression pin)
//	e StagedScopeTravels   — a confirm_writes key: a foreign scope is refused
//	                         BEFORE the stage (no card at all), an allowed one
//	                         rides in the canonical payload AND on the stage row,
//	                         and the confirm writes into that scope
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPWriteScope -count=1 -v
package handler

import (
	"context"
	"encoding/base64"
	"encoding/json"
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
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// em4ScopeDeniedMsg is the ONE prose of the scope gate. It is written out
// here rather than read from the code so a wording drift on either surface
// fails the probe instead of moving with it.
const em4ScopeDeniedMsg = "Cannot write to requested scope"

func TestMCPWriteScope(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)

	// Default sensitivity PUBLIC: the production default is 'credentials',
	// which is irrelevant here but would make every stored row look alike.
	cfgFor := func(blockLimit, blobLimit int) staticConfigStore {
		return staticConfigStore{cfg: &config.Config{
			Query: config.QueryConfig{RateLimitWrite: blockLimit},
			Pool: config.PoolConfig{
				DefaultBlockSensitivity: backends.SensPublic,
				BlobRateLimitWrite:      blobLimit,
				BlobStageMaxBytes:       1 << 20,
			},
			Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
		}}
	}
	mcpCfg := func(blockLimit, blobLimit int) MCPConfig {
		return MCPConfig{Pool: pool, Cfg: cfgFor(blockLimit, blobLimit), Blocktypes: reg}
	}

	// em4Key mints a REAL key row (context_access_log.api_key_id is FK-bound
	// and both paths book synchronously) and returns an AuthResult whose scope
	// sets are the PROBE's — only the key id has to exist in the table.
	em4Key := func(label, home string, allowed, write []string, confirmWrites bool) (string, context.Context, *auth.AuthResult) {
		t.Helper()
		row, _, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create api key %q: %v", label, err)
		}
		if confirmWrites {
			if _, err := pool.Exec(ctx,
				`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("opt %q into confirm_writes: %v", label, err)
			}
		}
		ar := &auth.AuthResult{
			ApiKeyID:      row.ID,
			IsValid:       true,
			HomeScope:     home,
			AllowedScopes: allowed,
			ReadScopes:    append([]string{home}, allowed...),
			WriteScopes:   write,
			ConfirmWrites: confirmWrites,
		}
		return row.ID, context.WithValue(ctx, authResultKey, ar), ar
	}

	// --- wire drivers ------------------------------------------------------
	//
	// The map goes through JSON into the tool input struct, exactly as the MCP
	// SDK decodes a client call. A field the struct does not know is dropped
	// here, silently — which is the pre-wave behaviour these probes are red
	// against.
	storeTool := func(keyCtx context.Context, cfg MCPConfig, body map[string]any) *mcp.CallToolResult {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal store input: %v", err)
		}
		var in storeInput
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode store input %s: %v", raw, err)
		}
		r, _, err := mcpStoreHandler(cfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("store %v: protocol error %v", body["title"], err)
		}
		return r
	}
	blobTool := func(keyCtx context.Context, cfg MCPConfig, body map[string]any) *mcp.CallToolResult {
		t.Helper()
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal blob_store input: %v", err)
		}
		var in blobStoreInput
		if err := json.Unmarshal(raw, &in); err != nil {
			t.Fatalf("decode blob_store input %s: %v", raw, err)
		}
		r, _, err := mcpBlobStoreHandler(cfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("blob_store %v: protocol error %v", body["title"], err)
		}
		return r
	}
	blockBody := func(title, content, scope string) map[string]any {
		b := map[string]any{"category": "test", "title": title, "content": content}
		if scope != "" {
			b["scope"] = scope
		}
		return b
	}
	blobBody := func(title, scope string, data []byte) map[string]any {
		b := map[string]any{
			"category": "reference", "title": title, "filename": title + ".bin",
			"mime_type": "application/octet-stream",
			"file":      base64.StdEncoding.EncodeToString(data),
		}
		if scope != "" {
			b["scope"] = scope
		}
		return b
	}

	// --- readers -----------------------------------------------------------

	blockScope := func(title string) string {
		t.Helper()
		var scope string
		if err := pool.QueryRow(ctx,
			`SELECT scope FROM context_blocks WHERE title = $1`, title).Scan(&scope); err != nil {
			t.Fatalf("read block scope %q: %v", title, err)
		}
		return scope
	}
	blobScope := func(title string) string {
		t.Helper()
		var scope string
		if err := pool.QueryRow(ctx,
			`SELECT scope FROM context_blobs WHERE title = $1`, title).Scan(&scope); err != nil {
			t.Fatalf("read blob scope %q: %v", title, err)
		}
		return scope
	}
	blockRows := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blocks %q: %v", title, err)
		}
		return n
	}
	blobRows := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blobs WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count blobs %q: %v", title, err)
		}
		return n
	}
	actionRows := func(keyID, action string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_access_log
			  WHERE api_key_id = $1::uuid AND action = $2`, keyID, action).Scan(&n); err != nil {
			t.Fatalf("count %s rows: %v", action, err)
		}
		return n
	}
	pendingRows := func(keyID string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_pending_writes WHERE api_key_id = $1::uuid`, keyID).Scan(&n); err != nil {
			t.Fatalf("count pending writes: %v", err)
		}
		return n
	}
	// stagedRow returns the stage row's own scope column and the canonical
	// payload's scope. Both, because the confirm compares them (a mismatch is
	// a tampered row and collapses into the generic miss) and because the
	// D1-M1 re-validation reads the ROW.
	stagedRow := func(keyID, title string) (rowScope string, cw store.CanonicalWrite) {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT scope, payload FROM context_pending_writes
			  WHERE api_key_id = $1::uuid AND payload->>'title' = $2
			  ORDER BY created_at DESC LIMIT 1`, keyID, title).Scan(&rowScope, &raw); err != nil {
			t.Fatalf("read stage row %q: %v", title, err)
		}
		if err := json.Unmarshal(raw, &cw); err != nil {
			t.Fatalf("decode staged payload %q: %v", title, err)
		}
		return rowScope, cw
	}

	// restStoreError posts the same block write over REST and returns the
	// status + the error prose — the reference the MCP verdict is compared to.
	restStoreError := func(keyCtx context.Context, body map[string]any) (int, string) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw)))
		req = req.WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfgFor(0, 0), reg).HandleStore(rec, req)
		var resp struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &resp)
		return rec.Code, resp.Error
	}

	t.Run("a_BlockExplicitScope", func(t *testing.T) {
		// home=em4a, allowed=[em4a,em4b], write=[em4b] ⇒ writableBlockScopes
		// is [em4a, em4b]. RED pre-wave: the wire field is dropped and the
		// block lands in em4a.
		_, keyCtx, _ := em4Key("em4-a", "em4a", []string{"em4a", "em4b"}, []string{"em4b"}, false)

		r := storeTool(keyCtx, mcpCfg(0, 0), blockBody("em4-a-block", "explicit scope over MCP", "em4b"))
		if r.IsError {
			t.Fatalf("store with scope=em4b was refused: %s (code %q)", resultText(t, r), ecMCPCode(t, r))
		}
		if got := blockScope("em4-a-block"); got != "em4b" {
			t.Errorf("block landed in scope %q, want em4b — the MCP store tool must honour an explicit "+
				"scope the key may write, exactly as REST /api/store does", got)
		}
	})

	t.Run("b_BlobExplicitScope", func(t *testing.T) {
		_, keyCtx, _ := em4Key("em4-b", "em4a", []string{"em4a", "em4b"}, []string{"em4b"}, false)

		r := blobTool(keyCtx, mcpCfg(0, 0), blobBody("em4-b-blob", "em4b", []byte("explicit")))
		if r.IsError {
			t.Fatalf("blob_store with scope=em4b was refused: %s (code %q)", resultText(t, r), ecMCPCode(t, r))
		}
		if got := blobScope("em4-b-blob"); got != "em4b" {
			t.Errorf("blob landed in scope %q, want em4b — blob_store must reach the same scope set the "+
				"REST blob route reaches (blobWriteGate/writableBlockScopes)", got)
		}
	})

	t.Run("c_ForeignScopeDenied", func(t *testing.T) {
		// em4x is held neither as a read nor as a write right. The budgets are
		// set to 1 and SPENT below, so a gate order that put the budget first
		// would answer rate_limit here — the probe pins scope-before-budget on
		// both paths, which is what keeps foreign-scope spray from draining a
		// legitimate key's quota.
		keyID, keyCtx, _ := em4Key("em4-c", "em4a", []string{"em4a", "em4b"}, []string{"em4b"}, false)
		cfg := mcpCfg(1, 1)

		// Spend the block budget and the blob budget with one legitimate write each.
		if r := storeTool(keyCtx, cfg, blockBody("em4-c-warm", "spends the block budget", "")); r.IsError {
			t.Fatalf("warm-up store refused: %s", resultText(t, r))
		}
		if r := blobTool(keyCtx, cfg, blobBody("em4-c-warm", "", []byte("warm"))); r.IsError {
			t.Fatalf("warm-up blob_store refused: %s", resultText(t, r))
		}
		blockBudget, blobBudget := actionRows(keyID, "write"), actionRows(keyID, store.ActionBlobWrite)
		if blockBudget != 1 || blobBudget != 1 {
			t.Fatalf("fixture: budgets booked = %d block / %d blob, want 1 / 1", blockBudget, blobBudget)
		}

		// c1: the block tool.
		r := storeTool(keyCtx, cfg, blockBody("em4-c-block", "foreign", "em4x"))
		if !r.IsError {
			t.Fatalf("store into an unwritable scope succeeded: %s", resultText(t, r))
		}
		if code := ecMCPCode(t, r); code != "scope_denied" {
			t.Errorf("store rejection code = %q, want scope_denied (text %q)", code, resultText(t, r))
		}
		if txt := resultText(t, r); txt != em4ScopeDeniedMsg {
			t.Errorf("store rejection prose = %q, want %q", txt, em4ScopeDeniedMsg)
		}
		if n := blockRows("em4-c-block"); n != 0 {
			t.Errorf("%d refused block(s) reached context_blocks, want 0", n)
		}

		// c2: the blob tool.
		rb := blobTool(keyCtx, cfg, blobBody("em4-c-blob", "em4x", []byte("foreign")))
		if !rb.IsError {
			t.Fatalf("blob_store into an unwritable scope succeeded: %s", resultText(t, rb))
		}
		if code := ecMCPCode(t, rb); code != "scope_denied" {
			t.Errorf("blob_store rejection code = %q, want scope_denied (text %q)", code, resultText(t, rb))
		}
		if txt := resultText(t, rb); txt != em4ScopeDeniedMsg {
			t.Errorf("blob_store rejection prose = %q, want %q", txt, em4ScopeDeniedMsg)
		}
		if n := blobRows("em4-c-blob"); n != 0 {
			t.Errorf("%d refused blob(s) reached context_blobs, want 0", n)
		}

		// c3: neither refusal booked budget.
		if got := actionRows(keyID, "write"); got != blockBudget {
			t.Errorf("block write rows = %d after the refusal, want %d — the scope gate must precede the booking", got, blockBudget)
		}
		if got := actionRows(keyID, store.ActionBlobWrite); got != blobBudget {
			t.Errorf("blob-write rows = %d after the refusal, want %d — blobWriteGate books only past the scope gate", got, blobBudget)
		}

		// c4: REST answers the identical prose for the identical request. The
		// budget is disabled on the REST wiring, so a 429 cannot masquerade.
		status, restMsg := restStoreError(keyCtx, blockBody("em4-c-rest", "foreign", "em4x"))
		if status != http.StatusForbidden {
			t.Fatalf("REST /api/store with an unwritable scope status = %d, want 403 (error %q)", status, restMsg)
		}
		if restMsg != em4ScopeDeniedMsg {
			t.Fatalf("REST rejection prose = %q, want %q — the constant this probe compares against is stale", restMsg, em4ScopeDeniedMsg)
		}
	})

	t.Run("d_AbsentScopeGolden", func(t *testing.T) {
		// The regression pin: no field ⇒ home scope, byte-identical to the
		// pre-wave behaviour, for a key that demonstrably HOLDS a second write
		// scope (subtests a/b write into it). A default that silently widened
		// would show up here and nowhere else.
		_, keyCtx, _ := em4Key("em4-d", "em4a", []string{"em4a", "em4b"}, []string{"em4b"}, false)
		cfg := mcpCfg(0, 0)

		if r := storeTool(keyCtx, cfg, blockBody("em4-d-block", "no scope field", "")); r.IsError {
			t.Fatalf("store without a scope field was refused: %s", resultText(t, r))
		}
		if got := blockScope("em4-d-block"); got != "em4a" {
			t.Errorf("block without a scope field landed in %q, want the home scope em4a", got)
		}
		if r := blobTool(keyCtx, cfg, blobBody("em4-d-blob", "", []byte("no scope"))); r.IsError {
			t.Fatalf("blob_store without a scope field was refused: %s", resultText(t, r))
		}
		if got := blobScope("em4-d-blob"); got != "em4a" {
			t.Errorf("blob without a scope field landed in %q, want the home scope em4a", got)
		}

		// An EMPTY scope string is the same case as an absent one — a client
		// that always sends the key must not be handed a different answer.
		if r := storeTool(keyCtx, cfg, map[string]any{
			"category": "test", "title": "em4-d-empty", "content": "empty scope string", "scope": "",
		}); r.IsError {
			t.Fatalf("store with an empty scope string was refused: %s", resultText(t, r))
		}
		if got := blockScope("em4-d-empty"); got != "em4a" {
			t.Errorf("block with an empty scope string landed in %q, want the home scope em4a", got)
		}
	})

	t.Run("e_StagedScopeTravels", func(t *testing.T) {
		keyID, keyCtx, _ := em4Key("em4-e", "em4a", []string{"em4a", "em4b"}, []string{"em4b"}, true)
		cfg := mcpCfg(0, 0)

		// e1: a foreign scope is refused BEFORE the stage. A card is a promise
		// that the confirm will succeed — a promise to write a scope this key
		// may not write must never be issued, and must cost no stage row.
		r := storeTool(keyCtx, cfg, blockBody("em4-e-denied", "foreign", "em4x"))
		if code := ecMCPCode(t, r); code != "scope_denied" {
			t.Errorf("staged store into an unwritable scope: code = %q, want scope_denied (text %q)", code, resultText(t, r))
		}
		rb := blobTool(keyCtx, cfg, blobBody("em4-e-denied", "em4x", []byte("foreign")))
		if code := ecMCPCode(t, rb); code != "scope_denied" {
			t.Errorf("staged blob_store into an unwritable scope: code = %q, want scope_denied (text %q)", code, resultText(t, rb))
		}
		if n := pendingRows(keyID); n != 0 {
			t.Fatalf("%d stage row(s) written for refused scopes, want 0", n)
		}

		// e2: an allowed foreign scope rides into the card — on the row AND
		// inside the canonical payload, which is what the payload_hash binds
		// and what the confirm re-validates (D1-M1).
		r = storeTool(keyCtx, cfg, blockBody("em4-e-block", "staged into em4b", "em4b"))
		txt := resultText(t, r)
		if !strings.Contains(txt, "STAGED — NOT saved yet") {
			t.Fatalf("staged store did not stage: %s (code %q)", txt, ecMCPCode(t, r))
		}
		rowScope, cw := stagedRow(keyID, "em4-e-block")
		if rowScope != "em4b" || cw.Scope != "em4b" {
			t.Errorf("stage row scope = %q, canonical scope = %q, want em4b for both", rowScope, cw.Scope)
		}
		m := hashRe.FindStringSubmatch(txt)
		if m == nil {
			t.Fatalf("no payload_hash in the staged answer: %s", txt)
		}
		cr, _, err := mcpConfirmHandler(cfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm: protocol error %v", err)
		}
		if cr.IsError {
			t.Fatalf("confirm of the staged block write failed: %s", resultText(t, cr))
		}
		if got := blockScope("em4-e-block"); got != "em4b" {
			t.Errorf("confirmed block landed in scope %q, want em4b", got)
		}

		// e3: the same for the blob arm.
		rb = blobTool(keyCtx, cfg, blobBody("em4-e-blob", "em4b", []byte("staged")))
		txt = resultText(t, rb)
		if !strings.Contains(txt, "STAGED — NOT saved yet") {
			t.Fatalf("staged blob_store did not stage: %s (code %q)", txt, ecMCPCode(t, rb))
		}
		rowScope, cw = stagedRow(keyID, "em4-e-blob")
		if rowScope != "em4b" || cw.Scope != "em4b" {
			t.Errorf("staged blob row scope = %q, canonical scope = %q, want em4b for both", rowScope, cw.Scope)
		}
		m = hashRe.FindStringSubmatch(txt)
		if m == nil {
			t.Fatalf("no payload_hash in the staged blob answer: %s", txt)
		}
		cr, _, err = mcpConfirmHandler(cfg)(keyCtx, nil, confirmInput{PayloadHash: m[1]})
		if err != nil {
			t.Fatalf("confirm blob: protocol error %v", err)
		}
		if cr.IsError {
			t.Fatalf("confirm of the staged blob write failed: %s", resultText(t, cr))
		}
		if got := blobScope("em4-e-blob"); got != "em4b" {
			t.Errorf("confirmed blob landed in scope %q, want em4b", got)
		}
	})
}
