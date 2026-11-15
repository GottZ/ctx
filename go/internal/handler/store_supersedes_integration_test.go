//go:build integration

// Issue #197: a writer declares on store that the written block supersedes
// another one (`supersedes: <block id>`). The declaration is applied in the
// write's own transaction as a pinned supersedes dream link, through the
// reconcile Dream uses, so the target becomes a snapshot pointing at the
// written block.
//
// Everything here drives the production write paths: POST /api/store
// (HandleStore), the MCP `store` and `confirm` tools over JSON-RPC (so an
// argument the tool does not know is refused by the SDK exactly as a client
// would see it), and the store functions behind archive and scope move.
//
//	go test -tags=integration ./internal/handler/ -run 'TestStoreSupersedes|TestMCPStoreSupersedes|TestSupersedesSuccessor' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	sdHome    = "sd-home"
	sdRead    = "sd-read"    // readable by the key, not its write scope
	sdForeign = "sd-foreign" // neither readable nor writable
)

// sdFixture is one database with the registry booted, a REST store handler and
// one key (home sd-home, may also read sd-read).
type sdFixture struct {
	pool   *pgxpool.Pool
	reg    *blocktype.Registry
	cfg    staticConfigStore
	h      *StoreHandler
	ar     *auth.AuthResult
	keyCtx context.Context
}

func sdSetup(t *testing.T) *sdFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	// A type whose policy keeps it out of the dream loop: a supersedes
	// declaration into it is refused (the reconcile could never produce that
	// state for it through dream).
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_block_types (name, scope, display_name, builtin, is_default, config)
		 VALUES ('sd-unlinkable', '_global', 'sd-unlinkable', false, false,
		         '{"v":1,"dream":{"linkable":false}}'::jsonb)`); err != nil {
		t.Fatalf("seed unlinkable type: %v", err)
	}
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfg := staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}
	_, plain, err := store.CreateApiKey(ctx, pool, "sd-key", sdHome, []string{sdHome, sdRead}, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	ar, err := auth.Authenticate(ctx, pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	return &sdFixture{
		pool: pool, reg: reg, cfg: cfg,
		h:      NewStoreHandler(pool, cfg, reg),
		ar:     ar,
		keyCtx: context.WithValue(ctx, authResultKey, ar),
	}
}

// rest posts one /api/store body and returns status, raw body and decoded map.
func (f *sdFixture) rest(t *testing.T, body map[string]any) (int, []byte, map[string]any) {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(raw)).WithContext(f.keyCtx)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.h.HandleStore(rec, req)
	var m map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &m)
	return rec.Code, rec.Body.Bytes(), m
}

// put stores a block through REST and returns its id; it fails the test on
// anything but 200.
func (f *sdFixture) put(t *testing.T, title, content string, extra map[string]any) string {
	t.Helper()
	body := map[string]any{"category": "sd", "title": title, "content": content}
	for k, v := range extra {
		body[k] = v
	}
	status, raw, m := f.rest(t, body)
	if status != http.StatusOK {
		t.Fatalf("store %q: status %d body %s", title, status, raw)
	}
	blk, _ := m["block"].(map[string]any)
	id, _ := blk["id"].(string)
	if id == "" {
		t.Fatalf("store %q: no block id in %s", title, raw)
	}
	return id
}

// foreignBlock writes a block straight through store.UpsertBlock into a scope
// the key does not write.
func (f *sdFixture) foreignBlock(t *testing.T, title, scope string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "sd", title, "foreign payload "+title,
		nil, nil, scope, true, store.SensitivityWrite{Value: backends.SensPublic}, "")
	if err != nil {
		t.Fatalf("seed %q: %v", title, err)
	}
	return b.ID
}

type sdState struct {
	lifecycle    string
	supersededBy string
	archived     bool
	content      string
	guardStatus  string
}

func (f *sdFixture) state(t *testing.T, id string) sdState {
	t.Helper()
	var s sdState
	var by, gs *string
	if err := f.pool.QueryRow(context.Background(),
		`SELECT lifecycle_state, superseded_by::text, is_archived, content, guard_status
		   FROM context_blocks WHERE id = $1::uuid`, id).Scan(&s.lifecycle, &by, &s.archived, &s.content, &gs); err != nil {
		t.Fatalf("read block %s: %v", id, err)
	}
	if by != nil {
		s.supersededBy = *by
	}
	if gs != nil {
		s.guardStatus = *gs
	}
	return s
}

type sdLink struct {
	relationship string
	pinned       bool
	raw          float64
	origin       string
}

// link reads the dream link source → target; ok=false when there is none.
func (f *sdFixture) link(t *testing.T, source, target string) (sdLink, bool) {
	t.Helper()
	var l sdLink
	var origin *string
	err := f.pool.QueryRow(context.Background(),
		`SELECT relationship, pinned, raw_confidence, metadata->>'origin'
		   FROM context_dream_links WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`,
		source, target).Scan(&l.relationship, &l.pinned, &l.raw, &origin)
	if err != nil {
		return sdLink{}, false
	}
	if origin != nil {
		l.origin = *origin
	}
	return l, true
}

func (f *sdFixture) countTitle(t *testing.T, title string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_blocks WHERE title = $1`, title).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", title, err)
	}
	return n
}

func TestStoreSupersedesDeclared(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()

	t.Run("a_rest_declares_in_one_write", func(t *testing.T) {
		target := f.put(t, "sd-a-old", "the old statement of a", nil)
		status, raw, m := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-a-new", "content": "the corrected statement of a",
			"supersedes": target,
		})
		if status != http.StatusOK {
			t.Fatalf("status %d body %s", status, raw)
		}
		blk, _ := m["block"].(map[string]any)
		newID, _ := blk["id"].(string)
		if got, _ := m["supersedes"].(string); got != target {
			t.Errorf("answer supersedes = %q, want %q (body %s)", got, target, raw)
		}
		l, ok := f.link(t, newID, target)
		if !ok {
			t.Fatalf("no dream link %s → %s", newID, target)
		}
		if l.relationship != "supersedes" || !l.pinned || l.raw != 1 || l.origin != "declared" {
			t.Errorf("link = %+v, want supersedes/pinned/raw 1/origin declared", l)
		}
		st := f.state(t, target)
		if st.lifecycle != "snapshot" || st.supersededBy != newID {
			t.Errorf("target state = %+v, want snapshot superseded_by %s", st, newID)
		}
	})

	t.Run("b_absent_field_unchanged", func(t *testing.T) {
		// Regression pin, green before and after: no field, no link, no key.
		status, raw, m := f.rest(t, map[string]any{"category": "sd", "title": "sd-b", "content": "plain"})
		if status != http.StatusOK {
			t.Fatalf("status %d body %s", status, raw)
		}
		if _, present := m["supersedes"]; present {
			t.Errorf("answer carries supersedes without the field: %s", raw)
		}
		blk, _ := m["block"].(map[string]any)
		var n int
		_ = f.pool.QueryRow(ctx, `SELECT count(*)::int FROM context_dream_links WHERE source_block_id = $1::uuid`,
			blk["id"]).Scan(&n)
		if n != 0 {
			t.Errorf("%d links written without the field", n)
		}
	})

	t.Run("c_uniform_refusal", func(t *testing.T) {
		archived := f.put(t, "sd-c-archived", "archived target", nil)
		if _, err := store.DeleteBlock(ctx, f.pool, archived, []string{sdHome}); err != nil {
			t.Fatalf("archive: %v", err)
		}
		foreign := f.foreignBlock(t, "sd-c-foreign", sdForeign)
		readable := f.foreignBlock(t, "sd-c-readable", sdRead)
		self := f.put(t, "sd-c-self", "self content", nil)
		cases := []struct{ name, title, target string }{
			{"unknown", "sd-c-w1", "019f0000-0000-7000-9000-00000000abcd"},
			{"archived", "sd-c-w2", archived},
			{"foreign_scope", "sd-c-w3", foreign},
			{"readable_not_write_scope", "sd-c-w4", readable},
			{"malformed", "sd-c-w5", "not-a-uuid"},
			{"self_by_upsert_identity", "sd-c-self", self},
		}
		var first []byte
		var firstStatus int
		for i, c := range cases {
			status, raw, _ := f.rest(t, map[string]any{
				"category": "sd", "title": c.title, "content": "changed content " + c.name, "supersedes": c.target,
			})
			if status != http.StatusUnprocessableEntity {
				t.Errorf("%s: status %d, want 422 (body %s)", c.name, status, raw)
			}
			if i == 0 {
				first, firstStatus = raw, status
			} else if !bytes.Equal(raw, first) || status != firstStatus {
				t.Errorf("%s: answer differs from %s:\n %s\n %s", c.name, cases[0].name, raw, first)
			}
		}
		if !strings.Contains(string(first), `"code":"unknown_supersedes"`) {
			t.Errorf("refusal carries no unknown_supersedes code: %s", first)
		}
		for _, c := range cases[:5] {
			if n := f.countTitle(t, c.title); n != 0 {
				t.Errorf("%s: %d blocks written behind the refusal", c.name, n)
			}
		}
		if st := f.state(t, self); st.content != "self content" {
			t.Errorf("self: content became %q, the refused write was applied", st.content)
		}
	})

	t.Run("d_identical_content_is_not_a_noop", func(t *testing.T) {
		target := f.put(t, "sd-d-old", "old d", nil)
		src := f.put(t, "sd-d-new", "new d", nil)
		status, raw, m := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-d-new", "content": "new d", "supersedes": target,
		})
		if status != http.StatusOK || m["action"] == "noop" {
			t.Fatalf("status %d body %s — a repeat save that adds supersedes must not answer noop", status, raw)
		}
		if _, ok := f.link(t, src, target); !ok {
			t.Errorf("no link after the repeat save with supersedes")
		}
	})

	t.Run("e_additive_and_idempotent", func(t *testing.T) {
		t1 := f.put(t, "sd-e-t1", "e one", nil)
		t2 := f.put(t, "sd-e-t2", "e two", nil)
		src := f.put(t, "sd-e-src", "e merged", map[string]any{"supersedes": t1})
		if again := f.put(t, "sd-e-src", "e merged", map[string]any{"supersedes": t2}); again != src {
			t.Fatalf("upsert moved the identity: %s vs %s", again, src)
		}
		f.put(t, "sd-e-src", "e merged", map[string]any{"supersedes": t2})
		f.put(t, "sd-e-src", "e merged, edited", nil) // field absent: declarations stay
		for _, tg := range []string{t1, t2} {
			if _, ok := f.link(t, src, tg); !ok {
				t.Errorf("declaration %s → %s is gone", src, tg)
			}
			if st := f.state(t, tg); st.lifecycle != "snapshot" || st.supersededBy != src {
				t.Errorf("target %s = %+v", tg, st)
			}
		}
	})

	t.Run("f_conflict_already_superseded", func(t *testing.T) {
		// The head of the chain, not the direct superseder: old ← mid ← head.
		target := f.put(t, "sd-f-old", "f old", nil)
		mid := f.put(t, "sd-f-mid", "f mid", map[string]any{"supersedes": target})
		head := f.put(t, "sd-f-head", "f head", map[string]any{"supersedes": mid})
		status, raw, _ := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-f-late", "content": "f late", "supersedes": target,
		})
		if status != http.StatusConflict {
			t.Fatalf("status %d, want 409 (body %s)", status, raw)
		}
		if !strings.Contains(string(raw), `"code":"supersedes_conflict"`) || !strings.Contains(string(raw), head) {
			t.Errorf("conflict answer must carry supersedes_conflict and the head %s: %s", head, raw)
		}
		if n := f.countTitle(t, "sd-f-late"); n != 0 {
			t.Errorf("%d blocks written behind the conflict", n)
		}
	})

	t.Run("g_conflict_cycle", func(t *testing.T) {
		b := f.put(t, "sd-g-b", "g b", nil)
		a := f.put(t, "sd-g-a", "g a", map[string]any{"supersedes": b})
		status, raw, _ := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-g-b", "content": "g b rewritten", "supersedes": a,
		})
		if status != http.StatusConflict || !strings.Contains(string(raw), `"code":"supersedes_conflict"`) {
			t.Fatalf("status %d body %s, want 409 supersedes_conflict", status, raw)
		}
		if st := f.state(t, b); st.content != "g b" {
			t.Errorf("cycle refusal left the upsert applied: content %q", st.content)
		}
	})

	t.Run("g2_conflict_longer_cycle", func(t *testing.T) {
		// a ← b ← c declared; c then declares to supersede... nothing new:
		// storing a (existing) with supersedes c closes a ← b ← c ← a.
		a := f.put(t, "sd-g2-a", "g2 a", nil)
		b := f.put(t, "sd-g2-b", "g2 b", map[string]any{"supersedes": a})
		c := f.put(t, "sd-g2-c", "g2 c", map[string]any{"supersedes": b})
		status, raw, _ := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-g2-a", "content": "g2 a rewritten", "supersedes": c,
		})
		if status != http.StatusConflict || !strings.Contains(string(raw), `"code":"supersedes_conflict"`) {
			t.Fatalf("status %d body %s, want 409 supersedes_conflict", status, raw)
		}
		if st := f.state(t, a); st.content != "g2 a" {
			t.Errorf("cycle refusal left the upsert applied: content %q", st.content)
		}
	})

	t.Run("g3_long_chain_of_corrections", func(t *testing.T) {
		// Every new correction supersedes the current head; the chain grows
		// past store.SupersedesChainDepth without any refusal — the bound is
		// on blocks ABOVE the written one, and a new block has none.
		root := f.put(t, "sd-g3-0", "g3 version 0", nil)
		head := root
		var chain []string
		for i := 1; i <= store.SupersedesChainDepth+6; i++ {
			title := "sd-g3-" + strconv.Itoa(i)
			status, raw, m := f.rest(t, map[string]any{
				"category": "sd", "title": title, "content": "g3 version " + strconv.Itoa(i), "supersedes": head,
			})
			if status != http.StatusOK {
				t.Fatalf("correction %d refused: status %d body %s", i, status, raw)
			}
			blk, _ := m["block"].(map[string]any)
			head, _ = blk["id"].(string)
			chain = append(chain, head)
		}
		// A late declaration against the root: the chain above it is longer
		// than the walk bound, so the conflict names no head — never a block
		// inside the chain, which would answer the same conflict again.
		status, raw, _ := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-g3-late", "content": "g3 late", "supersedes": root,
		})
		if status != http.StatusConflict || !strings.Contains(string(raw), "no head is named") {
			t.Fatalf("status %d body %s, want 409 without a head", status, raw)
		}
		for i, id := range chain {
			if strings.Contains(string(raw), id) {
				t.Errorf("the answer names chain block %d (%s) as head: %s", i+1, id, raw)
			}
		}
	})

	t.Run("h_not_allowed_chunk_and_type", func(t *testing.T) {
		var srcID string
		if err := f.pool.QueryRow(ctx,
			`INSERT INTO context_sources (file_path, file_hash, scope) VALUES ('/sd/doc.md', 'sd', $1) RETURNING id::text`,
			sdHome).Scan(&srcID); err != nil {
			t.Fatalf("seed source: %v", err)
		}
		chunk, err := store.InsertChunk(ctx, f.pool, srcID, 0, "sd", "sd-h-chunkrow", "chunk text", nil, nil, sdHome)
		if err != nil {
			t.Fatalf("insert chunk: %v", err)
		}
		unlinkable := f.put(t, "sd-h-unlinkable", "unlinkable target", map[string]any{"type": "sd-unlinkable"})
		for _, c := range []struct{ name, target string }{{"chunk", chunk.ID}, {"unlinkable_type", unlinkable}} {
			status, raw, _ := f.rest(t, map[string]any{
				"category": "sd", "title": "sd-h-" + c.name, "content": "h", "supersedes": c.target,
			})
			if status != http.StatusUnprocessableEntity || !strings.Contains(string(raw), `"code":"supersedes_not_allowed"`) {
				t.Errorf("%s: status %d body %s, want 422 supersedes_not_allowed", c.name, status, raw)
			}
			if n := f.countTitle(t, "sd-h-"+c.name); n != 0 {
				t.Errorf("%s: %d blocks written behind the refusal", c.name, n)
			}
		}
		if st := f.state(t, chunk.ID); st.lifecycle != "chunk" {
			t.Errorf("chunk left its lifecycle: %+v", st)
		}
	})

	t.Run("i_atomic_with_the_write", func(t *testing.T) {
		target := f.put(t, "sd-i-old", "i old", nil)
		// Fault injection after the block row exists: the link insert fails.
		for _, stmt := range []string{
			`CREATE FUNCTION sd_refuse_link() RETURNS trigger LANGUAGE plpgsql AS
			 $$BEGIN RAISE EXCEPTION 'sd: link insert refused'; END$$`,
			`CREATE TRIGGER sd_refuse_link BEFORE INSERT ON context_dream_links
			 FOR EACH ROW EXECUTE FUNCTION sd_refuse_link()`,
		} {
			if _, err := f.pool.Exec(ctx, stmt); err != nil {
				t.Fatalf("fault injection: %v", err)
			}
		}
		defer func() {
			_, _ = f.pool.Exec(ctx, `DROP TRIGGER sd_refuse_link ON context_dream_links`)
			_, _ = f.pool.Exec(ctx, `DROP FUNCTION sd_refuse_link()`)
		}()
		status, raw, _ := f.rest(t, map[string]any{
			"category": "sd", "title": "sd-i-new", "content": "i new", "supersedes": target,
		})
		if status != http.StatusInternalServerError {
			t.Errorf("status %d body %s, want 500", status, raw)
		}
		if n := f.countTitle(t, "sd-i-new"); n != 0 {
			t.Errorf("%d blocks committed although the declaration failed", n)
		}
		if st := f.state(t, target); st.lifecycle != "knowledge" || st.supersededBy != "" {
			t.Errorf("target changed although the write failed: %+v", st)
		}
	})
}

// sdMCP calls one MCP tool over JSON-RPC through the production handler.
func sdMCP(t *testing.T, cfg MCPConfig, keyCtx context.Context, tool string, args map[string]any) (text string, isError bool, code string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body)).WithContext(keyCtx)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	NewMCPHandler(cfg).ServeHTTP(rec, req)
	var resp struct {
		Result *struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError           bool `json:"isError"`
			StructuredContent struct {
				Code string `json:"code"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode %s: %v (status %d body %s)", tool, err, rec.Code, rec.Body.String())
	}
	if resp.Error != nil {
		return resp.Error.Message, true, ""
	}
	if resp.Result == nil || len(resp.Result.Content) == 0 {
		t.Fatalf("%s: empty result: %s", tool, rec.Body.String())
	}
	return resp.Result.Content[0].Text, resp.Result.IsError, resp.Result.StructuredContent.Code
}

func TestMCPStoreSupersedes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()
	mcpCfg := MCPConfig{Pool: f.pool, Cfg: f.cfg, Blocktypes: f.reg}

	blockID := func(t *testing.T, title string) string {
		t.Helper()
		var id string
		if err := f.pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
			t.Fatalf("lookup %q: %v", title, err)
		}
		return id
	}

	t.Run("a_direct", func(t *testing.T) {
		target := f.put(t, "sd-m-old", "mcp old", nil)
		text, isErr, _ := sdMCP(t, mcpCfg, f.keyCtx, "store", map[string]any{
			"category": "sd", "title": "sd-m-new", "content": "mcp new", "supersedes": target,
		})
		if isErr {
			t.Fatalf("store refused: %s", text)
		}
		if !strings.Contains(text, "supersedes: "+target) {
			t.Errorf("answer does not name the superseded block: %q", text)
		}
		src := blockID(t, "sd-m-new")
		if l, ok := f.link(t, src, target); !ok || !l.pinned {
			t.Errorf("link %+v ok=%v", l, ok)
		}
		if st := f.state(t, target); st.lifecycle != "snapshot" || st.supersededBy != src {
			t.Errorf("target = %+v", st)
		}
	})

	t.Run("b_direct_uniform_refusal", func(t *testing.T) {
		foreign := f.foreignBlock(t, "sd-mb-foreign", sdForeign)
		var first string
		for i, target := range []string{"019f0000-0000-7000-9000-00000000abce", foreign, "nope"} {
			text, isErr, code := sdMCP(t, mcpCfg, f.keyCtx, "store", map[string]any{
				"category": "sd", "title": "sd-mb-w", "content": "x", "supersedes": target,
			})
			if !isErr || code != "unknown_supersedes" {
				t.Errorf("target %s: isError=%v code=%q text=%q", target, isErr, code, text)
			}
			if i == 0 {
				first = text
			} else if text != first {
				t.Errorf("answers differ: %q vs %q", text, first)
			}
		}
		if n := f.countTitle(t, "sd-mb-w"); n != 0 {
			t.Errorf("%d blocks written behind the refusal", n)
		}
	})

	t.Run("c_staged_confirmed", func(t *testing.T) {
		_, plain, err := store.CreateApiKey(ctx, f.pool, "sd-confirm", sdHome, []string{sdHome}, store.DefaultTenantID)
		if err != nil {
			t.Fatalf("create key: %v", err)
		}
		if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE label = 'sd-confirm'`); err != nil {
			t.Fatalf("flag key: %v", err)
		}
		ar, err := auth.Authenticate(ctx, f.pool, plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate: %v", err)
		}
		cctx := context.WithValue(ctx, authResultKey, ar)

		// Stage time: a target the key cannot name gets no card.
		text, isErr, code := sdMCP(t, mcpCfg, cctx, "store", map[string]any{
			"category": "sd", "title": "sd-mc-bad", "content": "x", "supersedes": "019f0000-0000-7000-9000-00000000abcf",
		})
		if !isErr || code != "unknown_supersedes" || strings.Contains(text, "STAGED") {
			t.Errorf("stage of an unknown target: isError=%v code=%q text=%q", isErr, code, text)
		}

		target := f.put(t, "sd-mc-old", "staged old", nil)
		text, _, _ = sdMCP(t, mcpCfg, cctx, "store", map[string]any{
			"category": "sd", "title": "sd-mc-new", "content": "staged new", "supersedes": target,
		})
		hash := sdPayloadHash(t, text)
		text, isErr, _ = sdMCP(t, mcpCfg, cctx, "confirm", map[string]any{"payload_hash": hash})
		if isErr || !strings.Contains(text, "supersedes: "+target) {
			t.Fatalf("confirm: isError=%v text=%q", isErr, text)
		}
		src := blockID(t, "sd-mc-new")
		if st := f.state(t, target); st.lifecycle != "snapshot" || st.supersededBy != src {
			t.Errorf("target after confirm = %+v", st)
		}

		// The target changes between stage and confirm: the confirm re-runs
		// the gates and refuses, with the token spent.
		gone := f.put(t, "sd-mc-gone", "staged gone", nil)
		text, _, _ = sdMCP(t, mcpCfg, cctx, "store", map[string]any{
			"category": "sd", "title": "sd-mc-late", "content": "late", "supersedes": gone,
		})
		hash = sdPayloadHash(t, text)
		if _, err := store.DeleteBlock(ctx, f.pool, gone, []string{sdHome}); err != nil {
			t.Fatalf("archive target: %v", err)
		}
		text, isErr, code = sdMCP(t, mcpCfg, cctx, "confirm", map[string]any{"payload_hash": hash})
		if !isErr || code != "unknown_supersedes" || !strings.Contains(text, "stage token is consumed") {
			t.Errorf("confirm after the target left: isError=%v code=%q text=%q", isErr, code, text)
		}
		if n := f.countTitle(t, "sd-mc-late"); n != 0 {
			t.Errorf("%d blocks written by a refused confirm", n)
		}
	})
}

// sdPayloadHash extracts the payload_hash of a STAGED answer.
func sdPayloadHash(t *testing.T, text string) string {
	t.Helper()
	for _, line := range strings.Split(text, "\n") {
		if h, ok := strings.CutPrefix(line, "payload_hash: "); ok {
			return strings.TrimSpace(h)
		}
	}
	t.Fatalf("no payload_hash in %q", text)
	return ""
}

// TestSupersedesSuccessorLifecycle: the target of a supersedes link returns to
// knowledge when its successor leaves (archive, scope move), so a superseded
// block is never left as a snapshot pointing at nothing. Both arms run once
// with a dream-written link (the state machine predates the declaration) and
// once with a declared one.
func TestSupersedesSuccessorLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := sdSetup(t)
	ctx := context.Background()

	// pair writes old + new and links new → old, dream-written or declared,
	// and asserts the premise: the old block is a snapshot of the new one.
	pair := func(t *testing.T, prefix string, declared bool) (target, src string) {
		t.Helper()
		target = f.put(t, prefix+"-old", prefix+" old", nil)
		if declared {
			src = f.put(t, prefix+"-new", prefix+" new", map[string]any{"supersedes": target})
		} else {
			src = f.put(t, prefix+"-new", prefix+" new", nil)
			if n, err := dream.WriteLinks(ctx, f.pool, f.reg.Snapshot(), src, sdHome, 1.0,
				[]dream.Link{{TargetID: target, Relationship: "supersedes", Confidence: 0.9}}); err != nil || n != 1 {
				t.Fatalf("dream link: n=%d err=%v", n, err)
			}
		}
		if st := f.state(t, target); st.lifecycle != "snapshot" || st.supersededBy != src {
			t.Fatalf("premise: target = %+v, want snapshot of %s", st, src)
		}
		return target, src
	}

	for _, declared := range []bool{false, true} {
		kind := "dream"
		if declared {
			kind = "declared"
		}
		t.Run("archive_"+kind, func(t *testing.T) {
			target, src := pair(t, "sd-la-"+kind, declared)
			if _, err := store.DeleteBlock(ctx, f.pool, src, []string{sdHome}); err != nil {
				t.Fatalf("archive successor: %v", err)
			}
			if st := f.state(t, target); st.lifecycle != "knowledge" || st.supersededBy != "" {
				t.Errorf("target after the successor's archive = %+v, want knowledge without pointer", st)
			}
			// The daily sweep removes the link of the archived successor; the
			// target stays knowledge.
			if _, err := dream.CleanupDanglingLinks(ctx, f.pool); err != nil {
				t.Fatalf("cleanup: %v", err)
			}
			if _, ok := f.link(t, src, target); ok {
				t.Errorf("link of the archived successor survived the sweep")
			}
			if st := f.state(t, target); st.lifecycle != "knowledge" {
				t.Errorf("target after the sweep = %+v", st)
			}
		})

		t.Run("scope_move_target_answer_"+kind, func(t *testing.T) {
			// The TARGET moves away from its only superseder: the reconcile
			// returns it to knowledge, and the update's answer says so.
			target, _ := pair(t, "sd-lt-"+kind, declared)
			_, plain, err := store.CreateApiKey(ctx, f.pool, "sd-lt-key-"+kind, sdHome, []string{sdHome, "sd-lt-dest"}, store.DefaultTenantID)
			if err != nil {
				t.Fatalf("create key: %v", err)
			}
			if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET write_scopes = '{sd-lt-dest}' WHERE label = $1`, "sd-lt-key-"+kind); err != nil {
				t.Fatalf("write scopes: %v", err)
			}
			ar, err := auth.Authenticate(ctx, f.pool, plain)
			if err != nil || !ar.IsValid {
				t.Fatalf("authenticate: %v", err)
			}
			raw, _ := json.Marshal(map[string]any{"action": "update", "id": target, "data": map[string]any{"scope": "sd-lt-dest"}})
			req := httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(raw)).
				WithContext(context.WithValue(ctx, authResultKey, ar))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			NewManageHandler(f.pool, f.cfg, nil, nil, nil, nil, nil, f.reg).HandleManage(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("manage update: status %d body %s", rec.Code, rec.Body.String())
			}
			var resp map[string]any
			_ = json.Unmarshal(rec.Body.Bytes(), &resp)
			blk, _ := resp["block"].(map[string]any)
			if got, _ := blk["lifecycle_state"].(string); got != "knowledge" {
				t.Errorf("update answer lifecycle_state = %q, want knowledge (body %s)", got, rec.Body.String())
			}
			if st := f.state(t, target); st.lifecycle != "knowledge" {
				t.Errorf("moved target = %+v, want knowledge", st)
			}
		})

		t.Run("scope_move_"+kind, func(t *testing.T) {
			target, src := pair(t, "sd-lm-"+kind, declared)
			dest := "sd-move-dest"
			if _, _, err := store.UpdateBlock(ctx, f.pool, src, store.UpdateBlockData{Scope: &dest},
				[]string{sdHome, dest}); err != nil {
				t.Fatalf("move successor: %v", err)
			}
			if _, ok := f.link(t, src, target); ok {
				t.Errorf("cross-scope link survived the move")
			}
			if st := f.state(t, target); st.lifecycle != "knowledge" || st.supersededBy != "" {
				t.Errorf("target after the successor's move = %+v, want knowledge without pointer", st)
			}
		})
	}
}
