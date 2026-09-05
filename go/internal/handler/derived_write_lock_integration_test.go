//go:build integration

// Wave W01-2a — the write lock I7 (design D-01 §1.3 I7, §4.3.1 S1/S2/S3, §5.2
// B14) against a real PG18 testcontainer.
//
// Ist-Stand before this wave, and the reason the wave exists: migration 143 put
// `insight` and `catalog` into the block-type registry, and NOTHING on the write
// path cared. `validateTypeNameAgainstSet` (stage_gates.go) asks the registry
// whether a name exists — never who may claim it — so any of the live six API
// keys could write `type:"catalog"` and be handed `type_source='manual'`,
// `guard.check=false`, `guard.candidate=false`, `untrusted=false` and the optics
// of a proven derivative. The upsert identity was equally open: `(category,
// title, scope)` are free strings, so a client upsert onto a derived block
// overwrote its `content` AND its `metadata` — the whole provenance.
//
// Subtests carry the sperre they probe. Every one of them was run against the
// UNCHANGED tree first (report reports/bau/w01-2a.md quotes the failures
// verbatim); "RED" below names what the pre-wave tree did instead.
//
//	a_S1_REST            REST type:"insight" ⇒ 422 reserved_type.        RED: 200, block stored.
//	b_S1_MCPDirect       MCP store type:"catalog" ⇒ same class.          RED: stored.
//	c_S1_MCPStaged       flagged key ⇒ refused BEFORE a card exists.     RED: card staged.
//	d_S1_ManageUpdate    manage update type ⇒ 422, block keeps its type. RED: re-typed.
//	e_S2_REST            reserved category ⇒ 403 reserved_category.      RED: 200, block stored.
//	f_S2_MCPDirect       same on the MCP store tool.                     RED: stored.
//	g_S2_Ingest          same on /api/ingest, block AND chunk mode.      RED: 200, blocks stored.
//	h_S2_ManageUpdate    manage update category ⇒ 403, unchanged.        RED: moved.
//	i_S2_MCPUpdate       MCP update category ⇒ refused, unchanged.       RED: moved.
//	j_S3_Upsert          upsert onto a provenance block ⇒ 403; content
//	                     AND metadata survive.                           RED: both overwritten.
//	k_S3_Update          update-by-id onto one ⇒ 403; both survive.      RED: both overwritten.
//	l_Net_Classify       client write WITHOUT a type whose title hits a
//	                     derived title pattern ⇒ NOT a derived type.     RED: type_name='catalog'.
//	m_Anchor             a plain write (no type, ordinary category) is
//	                     byte-identical to the Ist.        GREEN before AND after (the anchor).
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestDerivedWriteLock -count=1 -v
package handler

import (
	"context"
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
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// wlAnchorHash pins the canonical payload hash of the (m) fixture. Measured on
// the PRE-wave binary, not recomputed from the post-wave code: a client holding
// a staged card across the deploy must confirm the same hash afterwards. If a
// sperre ever starts touching the ordinary write path, this moves.
const wlAnchorHash = "425b532d97a9635e394e2b3db3161f5f679e16d777ab5d848be0a0455a1ea0a1"

// wlCatalogTitle hits catalog's classify title pattern ("katalog #", migration
// 143). It is the seed wave's own net vector (store/derived_manual_integration_
// test.go net_catches_a_writer_that_forgot_the_type) driven through a CLIENT
// surface instead of a direct store call.
const wlCatalogTitle = "Katalog #fedcba9876543210fedcba9876543299"

func TestDerivedWriteLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	set := reg.Snapshot()
	// Premise of the whole file, asserted rather than assumed: both derived
	// types ARE in the registry. Without migration 143 every rejection below
	// would fire for the wrong reason (unknown type) and prove nothing.
	for _, name := range []string{derived.TypeInsight, derived.TypeCatalog} {
		if _, ok := set.Resolve(name); !ok {
			t.Fatalf("registry does not carry %q — migration 143 is the premise of this wave", name)
		}
	}

	// Default sensitivity public: the production default (credentials) would
	// drag the G40 detector into probes that are about the write lock.
	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: cfg}

	mkKey := func(label string, flagged bool) context.Context {
		t.Helper()
		row, plain, err := store.CreateApiKey(ctx, pool, label, "private", nil, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		if flagged {
			if _, err := pool.Exec(ctx,
				`UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
				t.Fatalf("opt in %s: %v", label, err)
			}
		}
		ar, err := auth.Authenticate(ctx, pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate %s: %v", label, err)
		}
		return context.WithValue(ctx, authResultKey, ar)
	}

	// --- surface drivers ---------------------------------------------------

	restStore := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/store", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewStoreHandler(pool, cfg, reg).HandleStore(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/store body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	restIngest := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/ingest", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewIngestHandler(pool).HandleIngest(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/ingest body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	restManage := func(keyCtx context.Context, body map[string]any) (int, map[string]any) {
		t.Helper()
		raw, _ := json.Marshal(body)
		req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(string(raw))).WithContext(keyCtx)
		rec := httptest.NewRecorder()
		NewManageHandler(pool, cfg, nil, nil, nil, nil, nil, reg).HandleManage(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode /api/manage body %s: %v", rec.Body.String(), err)
		}
		return rec.Code, resp
	}
	mcpStore := func(keyCtx context.Context, in storeInput) *mcp.CallToolResult {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp store %q: protocol error %v", in.Title, err)
		}
		return r
	}
	mcpUpdate := func(keyCtx context.Context, in updateInput) *mcp.CallToolResult {
		t.Helper()
		r, _, err := mcpUpdateHandler(mcpCfg)(keyCtx, nil, in)
		if err != nil {
			t.Fatalf("mcp update %q: protocol error %v", in.ID, err)
		}
		return r
	}

	// --- readers -----------------------------------------------------------

	blockCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count %q: %v", title, err)
		}
		return n
	}
	typeOf := func(title string) (name, source string) {
		t.Helper()
		if err := pool.QueryRow(ctx,
			`SELECT type_name, type_source FROM context_blocks WHERE title = $1`, title).Scan(&name, &source); err != nil {
			t.Fatalf("read type of %q: %v", title, err)
		}
		return
	}
	// stateOf reads the three columns S3 is about straight out of the table.
	stateOf := func(id string) (category, content string, meta map[string]any) {
		t.Helper()
		var raw []byte
		if err := pool.QueryRow(ctx,
			`SELECT category, content, metadata FROM context_blocks WHERE id = $1`, id).
			Scan(&category, &content, &raw); err != nil {
			t.Fatalf("read state of %s: %v", id, err)
		}
		if err := json.Unmarshal(raw, &meta); err != nil {
			t.Fatalf("decode metadata of %s: %v", id, err)
		}
		return
	}
	pendingCount := func(title string) int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*)::int FROM context_pending_writes WHERE payload->>'title' = $1`, title).Scan(&n); err != nil {
			t.Fatalf("count staged %q: %v", title, err)
		}
		return n
	}
	restCode := func(resp map[string]any) string {
		c, _ := resp["code"].(string)
		return c
	}
	mcpCode := func(r *mcp.CallToolResult) string {
		t.Helper()
		if r == nil || r.StructuredContent == nil {
			return ""
		}
		raw, err := json.Marshal(r.StructuredContent)
		if err != nil {
			t.Fatalf("marshal StructuredContent: %v", err)
		}
		var env struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatalf("decode StructuredContent %s: %v", raw, err)
		}
		return env.Code
	}
	// seedProvenance writes a derived block the way the ARM will: straight
	// through store.UpsertBlock, past every handler gate, with a provenance
	// object in its metadata. It is the subject of S3, and it also proves that
	// the server path stays open — the sperren live on the client surfaces.
	seedProvenance := func(category, title string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, "derived body", nil,
			map[string]any{"provenance": map[string]any{
				"v": derived.ContractVersion, "stratum": int(derived.StratumDerived),
			}}, "private", false, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("seed provenance block %q: %v", title, err)
		}
		return b.ID
	}

	// --- S1: the derived TYPE is not client-claimable -----------------------

	t.Run("a_S1_REST", func(t *testing.T) {
		const title = "w012a rest claims insight"
		keyCtx := mkKey("wl-a-rest", false)
		status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title,
			"content": "a client naming a derived type on REST /api/store",
			"type":    derived.TypeInsight,
		})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "reserved_type" {
			t.Errorf("code = %q, want reserved_type (body %v)", got, resp)
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("rejected write stored %d block(s), want 0", got)
		}
	})

	t.Run("b_S1_MCPDirect", func(t *testing.T) {
		const title = "w012a mcp claims catalog"
		keyCtx := mkKey("wl-b-mcp", false)
		r := mcpStore(keyCtx, storeInput{
			Category: "test", Title: title,
			Content: "a client naming a derived type on the MCP store tool",
			Type:    derived.TypeCatalog,
		})
		if !r.IsError {
			t.Fatalf("MCP accepted type=%q: %s", derived.TypeCatalog, resultText(t, r))
		}
		if got := mcpCode(r); got != "reserved_type" {
			t.Errorf("code = %q, want reserved_type (text %q)", got, resultText(t, r))
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("rejected write stored %d block(s), want 0", got)
		}
	})

	t.Run("c_S1_MCPStaged", func(t *testing.T) {
		// A confirm_writes key must be refused BEFORE a card exists: a staged
		// card is hash-bound, so a claim admitted at stage time is a claim the
		// confirm can no longer refuse.
		const title = "w012a staged claims insight"
		keyCtx := mkKey("wl-c-staged", true)
		r := mcpStore(keyCtx, storeInput{
			Category: "test", Title: title,
			Content: "a flagged key naming a derived type",
			Type:    derived.TypeInsight,
		})
		if got := mcpCode(r); got != "reserved_type" {
			t.Errorf("code = %q, want reserved_type (text %q)", got, resultText(t, r))
		}
		if strings.Contains(resultText(t, r), "STAGED") {
			t.Errorf("a derived type claim produced a confirm card: %s", resultText(t, r))
		}
		if got := pendingCount(title); got != 0 {
			t.Errorf("%d staged card(s) for a refused claim, want 0", got)
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("refused claim stored %d block(s), want 0", got)
		}
	})

	t.Run("d_S1_ManageUpdate", func(t *testing.T) {
		// The fourth type-claiming surface, which D-01 §4.3.1 does not name:
		// manage update carries its own `type` and reaches the shared claim
		// gates through updateClaimReject (context_manage.go).
		const title = "w012a manage update claims catalog"
		keyCtx := mkKey("wl-d-manage", false)
		if status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "an ordinary block, first",
		}); status != http.StatusOK {
			t.Fatalf("seed write: status %d (body %v)", status, resp)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}
		before, _ := typeOf(title)

		status, resp := restManage(keyCtx, map[string]any{
			"action": "update", "id": id,
			"data": map[string]any{"type": derived.TypeCatalog},
		})
		if status != http.StatusUnprocessableEntity {
			t.Fatalf("status = %d, want 422 (body %v)", status, resp)
		}
		if name, source := typeOf(title); name != before || source == "manual" {
			t.Errorf("block re-typed to (%q, %q) despite the rejection, want %q kept on auto", name, source, before)
		}
	})

	// --- S2: the derived CATEGORIES are not client-claimable ----------------

	t.Run("e_S2_REST", func(t *testing.T) {
		const title = "w012a rest into session-insights"
		keyCtx := mkKey("wl-e-rest", false)
		status, resp := restStore(keyCtx, map[string]any{
			"category": "session-insights", "title": title,
			"content": "a client occupying the insight arm's category",
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "reserved_category" {
			t.Errorf("code = %q, want reserved_category (body %v)", got, resp)
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("rejected write stored %d block(s), want 0", got)
		}
	})

	t.Run("f_S2_MCPDirect", func(t *testing.T) {
		const title = "w012a mcp into catalog"
		keyCtx := mkKey("wl-f-mcp", false)
		r := mcpStore(keyCtx, storeInput{
			Category: "catalog", Title: title,
			Content: "a client occupying the catalog arm's category",
		})
		if !r.IsError {
			t.Fatalf("MCP accepted a write into a reserved category: %s", resultText(t, r))
		}
		if got := mcpCode(r); got != "reserved_category" {
			t.Errorf("code = %q, want reserved_category (text %q)", got, resultText(t, r))
		}
		if got := blockCount(title); got != 0 {
			t.Errorf("rejected write stored %d block(s), want 0", got)
		}
	})

	t.Run("g_S2_Ingest", func(t *testing.T) {
		// Ingest has TWO write paths — UpsertBlock (block mode) and InsertChunk
		// (chunk mode, source_id set) — and the second one never touches
		// UpsertBlock at all. Both are probed; the whole batch is refused,
		// because a partial success on a reserved category would still occupy it.
		keyCtx := mkKey("wl-g-ingest", false)
		// A REAL source row: context_blocks.source_id carries an FK
		// (113_baseline.sql fk_blocks_source), so a made-up uuid would make the
		// chunk arm fail for the wrong reason and the probe would prove nothing
		// about the reservation.
		var sourceID string
		if err := pool.QueryRow(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope)
			 VALUES ('/w012a/chunk-probe.md', 'w012a', 'private') RETURNING id`).Scan(&sourceID); err != nil {
			t.Fatalf("seed ingest source: %v", err)
		}
		for _, tc := range []struct {
			name  string
			chunk map[string]any
		}{
			{"block_mode", map[string]any{
				"category": "catalog", "title": "w012a ingest block mode", "content": "occupying via ingest",
			}},
			{"chunk_mode", map[string]any{
				"category": "session-insights", "title": "w012a ingest chunk mode", "content": "occupying via chunks",
				"source_id": sourceID, "chunk_index": 0,
			}},
		} {
			t.Run(tc.name, func(t *testing.T) {
				status, resp := restIngest(keyCtx, map[string]any{
					"source": "w012a", "chunks": []map[string]any{tc.chunk},
				})
				if status != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (body %v)", status, resp)
				}
				if got := blockCount(tc.chunk["title"].(string)); got != 0 {
					t.Errorf("rejected ingest stored %d block(s), want 0", got)
				}
			})
		}
	})

	t.Run("h_S2_ManageUpdate", func(t *testing.T) {
		// Moving an EXISTING block into a reserved category is the same claim
		// through a different door.
		const title = "w012a manage update into catalog"
		keyCtx := mkKey("wl-h-manage", false)
		if status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "an ordinary block, first",
		}); status != http.StatusOK {
			t.Fatalf("seed write: status %d (body %v)", status, resp)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}

		status, resp := restManage(keyCtx, map[string]any{
			"action": "update", "id": id,
			"data": map[string]any{"category": "catalog"},
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", status, resp)
		}
		if cat, _, _ := stateOf(id); cat != "test" {
			t.Errorf("block moved to category %q despite the rejection", cat)
		}
	})

	t.Run("i_S2_MCPUpdate", func(t *testing.T) {
		const title = "w012a mcp update into session-insights"
		keyCtx := mkKey("wl-i-mcpupd", false)
		if r := mcpStore(keyCtx, storeInput{
			Category: "test", Title: title, Content: "an ordinary block, first",
		}); r.IsError {
			t.Fatalf("seed write rejected: %s", resultText(t, r))
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("read seed id: %v", err)
		}
		cat := "session-insights"
		r := mcpUpdate(keyCtx, updateInput{ID: id, Category: &cat})
		if !r.IsError {
			t.Fatalf("MCP update moved a block into a reserved category: %s", resultText(t, r))
		}
		if got := mcpCode(r); got != "reserved_category" {
			t.Errorf("code = %q, want reserved_category (text %q)", got, resultText(t, r))
		}
		if got, _, _ := stateOf(id); got != "test" {
			t.Errorf("block moved to category %q despite the rejection", got)
		}
	})

	// --- S3: a provenance-bearing block is not client-overwritable ----------

	t.Run("j_S3_Upsert", func(t *testing.T) {
		// The B14 vector in full: same (category, title, scope), so the write
		// takes the ON CONFLICT branch and would replace content AND metadata.
		// The category here is NOT reserved on purpose — otherwise S2 would
		// answer and S3 would never be reached.
		const title = "w012a provenance upsert target"
		id := seedProvenance("learnings", title)
		_, contentBefore, metaBefore := stateOf(id)
		keyCtx := mkKey("wl-j-upsert", false)

		// The attacker payload carries NO provenance key of its own — otherwise
		// the S3-metadata gate (reserved_metadata, added in the Nachbesserung)
		// would answer first and the CONFLICT half under test here would never
		// be reached. Both halves are probed, each with the code that proves
		// which one fired.
		status, resp := restStore(keyCtx, map[string]any{
			"category": "learnings", "title": title,
			"content": "attacker text wearing a derivative's identity",
		})
		if status != http.StatusForbidden {
			t.Fatalf("status = %d, want 403 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "provenance_protected" {
			t.Errorf("code = %q, want provenance_protected (body %v)", got, resp)
		}
		_, contentAfter, metaAfter := stateOf(id)
		if contentAfter != contentBefore {
			t.Errorf("content overwritten: %q", contentAfter)
		}
		if _, ok := metaAfter["provenance"].(map[string]any); !ok {
			t.Errorf("provenance object replaced: %#v (was %#v)", metaAfter["provenance"], metaBefore["provenance"])
		}

		// Same vector WITH a planted provenance key: refused one gate earlier,
		// and the block is equally untouched.
		status, resp = restStore(keyCtx, map[string]any{
			"category": "learnings", "title": title,
			"content":  "attacker text with a forged provenance",
			"metadata": map[string]any{"provenance": "mine now"},
		})
		if status != http.StatusForbidden {
			t.Fatalf("planted-provenance status = %d, want 403 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "reserved_metadata" {
			t.Errorf("planted-provenance code = %q, want reserved_metadata (body %v)", got, resp)
		}

		// Same conflict vector on the MCP store tool.
		r := mcpStore(keyCtx, storeInput{
			Category: "learnings", Title: title,
			Content: "attacker text through the second surface",
		})
		if !r.IsError {
			t.Fatalf("MCP overwrote a provenance block: %s", resultText(t, r))
		}
		if got := mcpCode(r); got != "provenance_protected" {
			t.Errorf("MCP code = %q, want provenance_protected (text %q)", got, resultText(t, r))
		}
		if _, contentAfter, _ = stateOf(id); contentAfter != contentBefore {
			t.Errorf("MCP overwrote the content: %q", contentAfter)
		}
	})

	t.Run("k_S3_Update", func(t *testing.T) {
		// Update-by-id reaches the same block without ever naming its identity.
		const title = "w012a provenance update target"
		id := seedProvenance("learnings", title)
		_, contentBefore, _ := stateOf(id)
		keyCtx := mkKey("wl-k-update", false)

		// Content only, no metadata: the refusal must come from the store's
		// WHERE (provenance_protected), not from the metadata claim gate —
		// otherwise this probe would stop covering the update half of S3.
		newContent := "attacker text via manage update"
		status, resp := restManage(keyCtx, map[string]any{
			"action": "update", "id": id,
			"data": map[string]any{"content": newContent},
		})
		if status != http.StatusForbidden {
			t.Fatalf("manage update status = %d, want 403 (body %v)", status, resp)
		}
		if got := restCode(resp); got != "provenance_protected" {
			t.Errorf("manage update code = %q, want provenance_protected (body %v)", got, resp)
		}
		_, contentAfter, metaAfter := stateOf(id)
		if contentAfter != contentBefore {
			t.Errorf("manage update overwrote the content: %q", contentAfter)
		}
		if _, ok := metaAfter["provenance"].(map[string]any); !ok {
			t.Errorf("manage update replaced the provenance object: %#v", metaAfter["provenance"])
		}

		mcpContent := "attacker text via the MCP update tool"
		r := mcpUpdate(keyCtx, updateInput{ID: id, Content: &mcpContent})
		if !r.IsError {
			t.Fatalf("MCP update overwrote a provenance block: %s", resultText(t, r))
		}
		if _, contentAfter, _ = stateOf(id); contentAfter != contentBefore {
			t.Errorf("MCP update overwrote the content: %q", contentAfter)
		}
	})

	// --- the classify net (seed review finding #3) --------------------------

	t.Run("l_Net_Classify", func(t *testing.T) {
		// The path none of S1/S2/S3 covers: no `type`, an ordinary category, and
		// a title that hits catalog's classify pattern. Pre-wave the auto
		// classifier lifted it to type_name='catalog' with type_source='auto' —
		// the level granted by a title, which is exactly what §4.3.1 forbids.
		keyCtx := mkKey("wl-l-net", false)
		if name, matched := set.Classify(wlCatalogTitle, nil); !matched || name != derived.TypeCatalog {
			t.Fatalf("fixture: title classifies to (%q, %v), want (catalog, true) — otherwise this "+
				"probe cannot show the net being closed", name, matched)
		}
		status, resp := restStore(keyCtx, map[string]any{
			"category": "learnings", "title": wlCatalogTitle,
			"content": "a client that never named a type",
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 — the net is closed by NOT granting the type, "+
				"not by refusing the write (body %v)", status, resp)
		}
		name, source := typeOf(wlCatalogTitle)
		if derived.IsDerivedType(name) {
			t.Errorf("classify granted the derived type %q (source %q) to a client write with no type",
				name, source)
		}
	})

	// --- the pausability anchor --------------------------------------------

	t.Run("m_Anchor", func(t *testing.T) {
		// Green BEFORE and after: an ordinary write must not move by a byte.
		// Fixture kept fully deterministic (no tags, no metadata, benign
		// content, settings-default sensitivity) so the staged hash is stable.
		const title = "w012a regression anchor ordinary write"
		keyCtx := mkKey("wl-m-anchor", false)
		status, resp := restStore(keyCtx, map[string]any{
			"category": "test", "title": title, "content": "no type field, ordinary category",
		})
		if status != http.StatusOK {
			t.Fatalf("status = %d, want 200 (body %v)", status, resp)
		}
		if ok, _ := resp["success"].(bool); !ok {
			t.Errorf("success = %v, want true (body %v)", resp["success"], resp)
		}
		if _, has := resp["code"]; has {
			t.Errorf("an accepted write grew a code key: %v", resp)
		}
		if name, source := typeOf(title); name != "knowledge" || source != "auto" {
			t.Errorf("ordinary write landed on (%q, %q), want (knowledge, auto)", name, source)
		}

		// The staged form of the same write: the canonical bytes a client may
		// already hold. A sperre that touches the ordinary path moves this.
		const stagedTitle = "w012a regression anchor staged"
		stagedCtx := mkKey("wl-m-staged", true)
		r := mcpStore(stagedCtx, storeInput{
			Category: "test", Title: stagedTitle, Content: "no type field at all",
		})
		if !r.IsError || !strings.Contains(resultText(t, r), "STAGED — NOT saved yet") {
			t.Fatalf("flagged key did not stage an ordinary write: %s", resultText(t, r))
		}
		var hash string
		if err := pool.QueryRow(ctx,
			`SELECT payload_hash FROM context_pending_writes WHERE payload->>'title' = $1`, stagedTitle).
			Scan(&hash); err != nil {
			t.Fatalf("read staged hash: %v", err)
		}
		if hash != wlAnchorHash {
			t.Errorf("payload_hash = %q, want the pre-wave pin %q — the canonical bytes moved", hash, wlAnchorHash)
		}
	})
}
