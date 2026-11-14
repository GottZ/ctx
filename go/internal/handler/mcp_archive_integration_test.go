//go:build integration

// Issue #201 integration probes (real PG18 testcontainer). Every MCP call goes
// through the real transport by tool NAME (NewMCPHandler → Streamable HTTP),
// so the probes compile against a server that does not have the tool or the
// field yet and fail there at run time — and they see the schema validation,
// decoding and recovery a client sees.
//
//   - TestMCPArchive: the `archive` tool — manage delete parity (writable
//     scopes, prefix resolution, provenance exclusion, no grant reach) and the
//     staged form for confirm_writes keys (stage, confirm, drift reject).
//   - TestMCPGetArchived: `get` answers an unknown, foreign or archived full id
//     alike (`block not found`), and `include_archived` reads an archived block
//     of the key's own scopes — never through a block grant or a cross-tenant
//     scope grant.
//   - TestManageGetArchived: the same opt-in on REST manage get, which the CLI
//     `ctx get --include-archived` posts.
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestMCPArchive|TestMCPGetArchived|TestManageGetArchived' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i201Answer is one tools/call answer as a client reads it.
type i201Answer struct {
	RPCError string // a JSON-RPC error instead of a result (unknown tool, invalid arguments)
	IsError  bool
	Text     string
	Code     string // structuredContent.code, "" when absent
}

var i201StoredID = regexp.MustCompile(`\(id: ([0-9a-f-]{36})`)

// i201Unknown is a well-formed block id no fixture ever writes.
const i201Unknown = "019f0000-0000-7000-8000-000000000201"

type i201Fixture struct {
	pool    *pgxpool.Pool
	mcp     http.Handler
	cfg     MCPConfig
	plain   string          // the direct key, for a re-authentication after a grant
	direct  context.Context // private, default tenant
	flagged context.Context // private, default tenant, confirm_writes
	flagID  string          // the flagged key's id
	other   context.Context // home scope i201-b, a second tenant
}

func newI201Fixture(t *testing.T) *i201Fixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}

	_, directPlain, err := store.CreateApiKey(ctx, pool, "i201-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "i201-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	other, err := store.CreateTenant(ctx, pool, "i201-b", "i201-b")
	if err != nil {
		t.Fatalf("create tenant: %v", err)
	}
	if _, err := store.AssignTenantScope(ctx, pool, other.ID, "i201-b", nil); err != nil {
		t.Fatalf("assign scope: %v", err)
	}
	_, otherPlain, err := store.CreateApiKey(ctx, pool, "i201-b", "i201-b", nil, other.ID)
	if err != nil {
		t.Fatalf("create other-tenant key: %v", err)
	}

	cfg := MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: v11Registry(t, pool)}
	return &i201Fixture{
		pool:    pool,
		mcp:     NewMCPHandler(cfg),
		cfg:     cfg,
		plain:   directPlain,
		direct:  mcpAuthCtx(t, ctx, pool, directPlain),
		flagged: mcpAuthCtx(t, ctx, pool, flaggedPlain),
		flagID:  flaggedRow.ID,
		other:   mcpAuthCtx(t, ctx, pool, otherPlain),
	}
}

// call posts one tools/call over the real MCP transport.
func (f *i201Fixture) call(t *testing.T, ctx context.Context, tool string, args map[string]any) i201Answer {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.mcp.ServeHTTP(rec, req)
	var resp struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			StructuredContent *struct {
				Code string `json:"code"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode tools/call %s: %v (status %d, body %s)", tool, err, rec.Code, rec.Body.String())
	}
	var a i201Answer
	if resp.Error != nil {
		a.RPCError = resp.Error.Message
		return a
	}
	if resp.Result == nil {
		t.Fatalf("tools/call %s: neither result nor error (body %s)", tool, rec.Body.String())
	}
	a.IsError = resp.Result.IsError
	if len(resp.Result.Content) > 0 {
		a.Text = resp.Result.Content[0].Text
	}
	if resp.Result.StructuredContent != nil {
		a.Code = resp.Result.StructuredContent.Code
	}
	return a
}

// store writes one block over the MCP store tool — the production write path —
// and returns its id.
func (f *i201Fixture) store(t *testing.T, ctx context.Context, title string) string {
	t.Helper()
	a := f.call(t, ctx, "store", map[string]any{
		"category": "test", "title": title, "content": "content of " + title, "sensitivity": "internal",
	})
	if a.RPCError != "" || a.IsError {
		t.Fatalf("fixture store %s: %+v", title, a)
	}
	m := i201StoredID.FindStringSubmatch(a.Text)
	if m == nil {
		t.Fatalf("fixture store %s: no id in %q", title, a.Text)
	}
	return m[1]
}

func (f *i201Fixture) archived(t *testing.T, id string) bool {
	t.Helper()
	var archived bool
	if err := f.pool.QueryRow(context.Background(),
		`SELECT is_archived FROM context_blocks WHERE id = $1`, id).Scan(&archived); err != nil {
		t.Fatalf("read is_archived of %s: %v", id, err)
	}
	return archived
}

func (f *i201Fixture) writeBookings(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_access_log WHERE api_key_id = $1 AND action = 'write'`, f.flagID).Scan(&n); err != nil {
		t.Fatalf("count write bookings: %v", err)
	}
	return n
}

func (f *i201Fixture) pending(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*)::int FROM context_pending_writes WHERE api_key_id = $1 AND consumed_at IS NULL`, f.flagID).Scan(&n); err != nil {
		t.Fatalf("count open stages: %v", err)
	}
	return n
}

var i201Hash = regexp.MustCompile(`payload_hash: ([0-9a-f]{64})`)

func TestMCPArchive(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()

	var retracted string
	t.Run("archive retracts the block from get, search and recent, and keeps the row", func(t *testing.T) {
		retracted = f.store(t, f.direct, "i201-retract")
		listing := map[string]any{"category": "test", "limit": 500}
		if s := f.call(t, f.direct, "search", listing); s.IsError || !strings.Contains(s.Text, retracted) {
			t.Fatalf("search does not list the live block (fixture broken): %+v", s)
		}
		a := f.call(t, f.direct, "archive", map[string]any{"id": retracted})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("archive failed: %+v", a)
		}
		if want := "Archived: i201-retract (id: " + retracted + ", category: test)"; a.Text != want {
			t.Errorf("archive answer = %q, want %q", a.Text, want)
		}
		if !f.archived(t, retracted) {
			t.Fatalf("block is not archived after the archive tool answered success")
		}
		if g := f.call(t, f.direct, "get", map[string]any{"id": retracted}); !g.IsError || g.Text != "block not found" {
			t.Errorf("get after archive = %+v, want the block-not-found error", g)
		}
		if s := f.call(t, f.direct, "search", listing); s.IsError || strings.Contains(s.Text, retracted) {
			t.Errorf("search after archive = %+v, want a listing without the block", s)
		}
		if r := f.call(t, f.direct, "recent", map[string]any{"limit": 50}); strings.Contains(r.Text, retracted) {
			t.Errorf("recent still returns the archived block: %s", r.Text)
		}
	})

	t.Run("an unambiguous prefix resolves within the writable scopes", func(t *testing.T) {
		id := f.store(t, f.direct, "i201-prefix")
		a := f.call(t, f.direct, "archive", map[string]any{"id": id[:32]})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("archive by prefix failed: %+v", a)
		}
		if !f.archived(t, id) {
			t.Fatalf("prefix archive answered success but the block is live")
		}
	})

	foreign := f.store(t, f.other, "i201-foreign")
	t.Run("a foreign, an unknown and an already archived id answer alike and nothing moves", func(t *testing.T) {
		answers := map[string]i201Answer{
			"foreign":  f.call(t, f.direct, "archive", map[string]any{"id": foreign}),
			"unknown":  f.call(t, f.direct, "archive", map[string]any{"id": i201Unknown}),
			"archived": f.call(t, f.direct, "archive", map[string]any{"id": retracted}),
		}
		ref := answers["unknown"]
		if ref.RPCError != "" || !ref.IsError || !strings.Contains(ref.Text, "not found") {
			t.Fatalf("unknown id: %+v, want a not-found tool error", ref)
		}
		for name, a := range answers {
			if a != ref {
				t.Errorf("%s id answers %+v, unknown id answers %+v — an existence oracle", name, a, ref)
			}
		}
		if f.archived(t, foreign) {
			t.Fatalf("the foreign tenant's block was archived")
		}
	})

	t.Run("a read grant does not open the archive", func(t *testing.T) {
		if _, err := store.CreateBlockGrant(ctx, f.pool, foreign, store.DefaultTenantID, ""); err != nil {
			t.Fatalf("grant: %v", err)
		}
		if g := f.call(t, f.direct, "get", map[string]any{"id": foreign}); g.IsError || !strings.Contains(g.Text, foreign) {
			t.Fatalf("granted block is not readable (fixture broken): %+v", g)
		}
		a := f.call(t, f.direct, "archive", map[string]any{"id": foreign})
		u := f.call(t, f.direct, "archive", map[string]any{"id": i201Unknown})
		if a.RPCError != "" || !a.IsError || a != u {
			t.Errorf("granted foreign id answers %+v, unknown id %+v", a, u)
		}
		if f.archived(t, foreign) {
			t.Fatalf("a read grant archived the grantor's block")
		}
	})

	t.Run("a derivative is refused provenance_protected and stays live, direct and flagged", func(t *testing.T) {
		md := map[string]any{derived.MetadataKey: map[string]any{
			"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "i201",
		}}
		b, err := store.UpsertBlock(ctx, f.pool, "learnings", "i201-derivative", "derived body", nil, md,
			"private", true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true}, derived.TypeCatalog)
		if err != nil {
			t.Fatalf("server-path derivative: %v", err)
		}
		before := f.pending(t)
		for name, c := range map[string]context.Context{"direct": f.direct, "flagged": f.flagged} {
			a := f.call(t, c, "archive", map[string]any{"id": b.ID})
			if !a.IsError || a.Code != "provenance_protected" || strings.Contains(a.Text, "STAGED") {
				t.Errorf("%s: archive of a derivative = %+v, want the provenance_protected refusal", name, a)
			}
		}
		if f.archived(t, b.ID) {
			t.Fatalf("the derivative was archived")
		}
		if got := f.pending(t); got != before {
			t.Errorf("a refused archive left a staged card (open %d → %d)", before, got)
		}
	})

	t.Run("a confirm_writes key stages the archive and confirm executes it", func(t *testing.T) {
		id := f.store(t, f.direct, "i201-staged")
		booked := f.writeBookings(t)
		s := f.call(t, f.flagged, "archive", map[string]any{"id": id})
		if !s.IsError || s.Code != "" || !strings.Contains(s.Text, "STAGED — NOT archived yet") {
			t.Fatalf("flagged archive = %+v, want an uncoded STAGED card", s)
		}
		if got := f.writeBookings(t); got != booked+1 {
			t.Errorf("write bookings %d → %d, want one booked at stage time", booked, got)
		}
		m := i201Hash.FindStringSubmatch(s.Text)
		if m == nil {
			t.Fatalf("staged archive carries no payload_hash: %s", s.Text)
		}
		if f.archived(t, id) {
			t.Fatalf("staging archived the block")
		}
		c := f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
		if c.IsError {
			t.Fatalf("confirm failed: %+v", c)
		}
		if want := "Confirmed and archived: i201-staged (id: " + id + ", category: test)"; c.Text != want {
			t.Errorf("confirm answer = %q, want %q", c.Text, want)
		}
		if !f.archived(t, id) {
			t.Fatalf("confirm answered success but the block is live")
		}
		if r := f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]}); !r.IsError || !strings.Contains(r.Text, "no confirmable staged write") {
			t.Errorf("replayed confirm = %+v, want the generic miss", r)
		}
	})

	t.Run("an archive staged over MCP confirms over POST /api/confirm", func(t *testing.T) {
		id := f.store(t, f.direct, "i201-http-confirm")
		s := f.call(t, f.flagged, "archive", map[string]any{"id": id})
		m := i201Hash.FindStringSubmatch(s.Text)
		if m == nil {
			t.Fatalf("no staged card: %+v", s)
		}
		raw, _ := json.Marshal(map[string]string{"payload_hash": m[1]})
		req := httptest.NewRequestWithContext(f.flagged, http.MethodPost, "/api/confirm", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		NewConfirmHandler(f.pool, f.cfg.Blocktypes).HandleConfirm(rec, req)
		var resp struct {
			Success bool   `json:"success"`
			Op      string `json:"op"`
			Block   struct {
				ID string `json:"id"`
			} `json:"block"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode confirm: %v (%s)", err, rec.Body.String())
		}
		if rec.Code != http.StatusOK || !resp.Success || resp.Op != store.OpArchive || resp.Block.ID != id {
			t.Fatalf("HTTP confirm = %d %s", rec.Code, rec.Body.String())
		}
		if !f.archived(t, id) {
			t.Fatalf("HTTP confirm answered success but the block is live")
		}
	})

	t.Run("a block changed after staging rejects the confirm and keeps the token", func(t *testing.T) {
		id := f.store(t, f.direct, "i201-drift")
		s := f.call(t, f.flagged, "archive", map[string]any{"id": id})
		m := i201Hash.FindStringSubmatch(s.Text)
		if m == nil {
			t.Fatalf("no staged card: %+v", s)
		}
		if u := f.call(t, f.direct, "update", map[string]any{"id": id, "content": "changed after staging"}); u.IsError {
			t.Fatalf("concurrent update: %+v", u)
		}
		open := f.pending(t)
		c := f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
		if !c.IsError || !strings.Contains(c.Text, "changed since this archive was staged") {
			t.Fatalf("drifted confirm = %+v, want the lost-update rejection", c)
		}
		if f.archived(t, id) {
			t.Fatalf("a drifted confirm archived the block")
		}
		if got := f.pending(t); got != open {
			t.Errorf("the drift rejection consumed the token (open %d → %d)", open, got)
		}
	})

	t.Run("a block archived after staging rejects the confirm and keeps the token", func(t *testing.T) {
		id := f.store(t, f.direct, "i201-gone")
		s := f.call(t, f.flagged, "archive", map[string]any{"id": id})
		m := i201Hash.FindStringSubmatch(s.Text)
		if m == nil {
			t.Fatalf("no staged card: %+v", s)
		}
		if a := f.call(t, f.direct, "archive", map[string]any{"id": id}); a.IsError {
			t.Fatalf("concurrent archive: %+v", a)
		}
		open := f.pending(t)
		c := f.call(t, f.flagged, "confirm", map[string]any{"payload_hash": m[1]})
		if !c.IsError || !strings.Contains(c.Text, "the staged archive stays pending until it expires") {
			t.Fatalf("confirm of a vanished target = %+v, want the TOCTOU-gone rejection", c)
		}
		if got := f.pending(t); got != open {
			t.Errorf("the gone rejection consumed the token (open %d → %d)", open, got)
		}
	})

	t.Run("a confirm_writes key stages nothing for an id outside its writable scopes", func(t *testing.T) {
		before := f.pending(t)
		a := f.call(t, f.flagged, "archive", map[string]any{"id": foreign})
		u := f.call(t, f.flagged, "archive", map[string]any{"id": i201Unknown})
		if !a.IsError || a != u {
			t.Errorf("flagged foreign id = %+v, unknown id = %+v, want the same not-found error", a, u)
		}
		if got := f.pending(t); got != before {
			t.Errorf("a foreign id was staged (open %d → %d)", before, got)
		}
	})
}

func TestMCPGetArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()

	live := f.store(t, f.direct, "i201-live")
	own := f.store(t, f.direct, "i201-own-archived")
	if _, err := store.DeleteBlock(ctx, f.pool, own, []string{"private"}); err != nil {
		t.Fatalf("archive own: %v", err)
	}
	foreignLive := f.store(t, f.other, "i201-foreign-live")
	// Granted to the caller's tenant, then archived by its owner: the grant
	// made it readable while it was live, and must not keep it readable now.
	foreignArchived := f.store(t, f.other, "i201-foreign-archived")
	if _, err := store.CreateBlockGrant(ctx, f.pool, foreignArchived, store.DefaultTenantID, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := store.DeleteBlock(ctx, f.pool, foreignArchived, []string{"i201-b"}); err != nil {
		t.Fatalf("archive foreign: %v", err)
	}
	foreignGranted := f.store(t, f.other, "i201-foreign-granted-live")
	if _, err := store.CreateBlockGrant(ctx, f.pool, foreignGranted, store.DefaultTenantID, ""); err != nil {
		t.Fatalf("grant live: %v", err)
	}

	t.Run("without the opt-in an unknown, foreign or archived full id answers block not found", func(t *testing.T) {
		for name, id := range map[string]string{
			"unknown": i201Unknown, "foreign": foreignLive, "own archived": own, "granted archived": foreignArchived,
		} {
			a := f.call(t, f.direct, "get", map[string]any{"id": id})
			if a.RPCError != "" || !a.IsError || a.Text != "block not found" {
				t.Errorf("%s: get = %+v, want the block-not-found tool error", name, a)
			}
		}
	})

	t.Run("include_archived reads an archived block of the caller's own scopes, marked archived", func(t *testing.T) {
		a := f.call(t, f.direct, "get", map[string]any{"id": own, "include_archived": true})
		if a.RPCError != "" || a.IsError {
			t.Fatalf("get include_archived = %+v", a)
		}
		var b map[string]any
		if err := json.Unmarshal([]byte(a.Text), &b); err != nil {
			t.Fatalf("answer is not a JSON block: %v (%s)", err, a.Text)
		}
		if b["id"] != own || b["content"] != "content of i201-own-archived" || b["archived"] != true {
			t.Errorf("archived read = id %v content %v archived %v", b["id"], b["content"], b["archived"])
		}
	})

	t.Run("include_archived never reaches through a grant or into a foreign scope", func(t *testing.T) {
		ref := f.call(t, f.direct, "get", map[string]any{"id": i201Unknown, "include_archived": true})
		if !ref.IsError || ref.Text != "block not found" {
			t.Fatalf("unknown id with include_archived = %+v", ref)
		}
		for name, id := range map[string]string{"granted archived": foreignArchived, "foreign live": foreignLive} {
			if a := f.call(t, f.direct, "get", map[string]any{"id": id, "include_archived": true}); a != ref {
				t.Errorf("%s with include_archived = %+v, want the unknown-id answer %+v", name, a, ref)
			}
		}
	})

	t.Run("include_archived on a live block answers the same bytes as without", func(t *testing.T) {
		plain := f.call(t, f.direct, "get", map[string]any{"id": live})
		opted := f.call(t, f.direct, "get", map[string]any{"id": live, "include_archived": true})
		if plain.IsError || plain != opted {
			t.Errorf("live block: plain %+v, opted %+v", plain, opted)
		}
		if strings.Contains(plain.Text, `"archived"`) {
			t.Errorf("a live block carries an archived key: %s", plain.Text)
		}
		// The grant arm of the live read is untouched by the opt-in.
		gPlain := f.call(t, f.direct, "get", map[string]any{"id": foreignGranted})
		gOpted := f.call(t, f.direct, "get", map[string]any{"id": foreignGranted, "include_archived": true})
		if gPlain.IsError || gPlain != gOpted {
			t.Errorf("granted live block: plain %+v, opted %+v", gPlain, gOpted)
		}
	})

	t.Run("include_archived with an id prefix is refused", func(t *testing.T) {
		a := f.call(t, f.direct, "get", map[string]any{"id": own[:20], "include_archived": true})
		if !a.IsError || !strings.Contains(a.Text, "full block id") {
			t.Errorf("prefix with include_archived = %+v, want the full-id refusal", a)
		}
	})

	// Last: it widens the direct key's read scopes for the rest of the test.
	t.Run("a cross-tenant scope grant does not carry the archived read", func(t *testing.T) {
		if _, err := store.CreateTenantGrant(ctx, f.pool, store.DefaultTenantID, "i201-b", ""); err != nil {
			t.Fatalf("scope grant: %v", err)
		}
		granted := mcpAuthCtx(t, ctx, f.pool, f.plain)
		if g := f.call(t, granted, "get", map[string]any{"id": foreignLive}); g.IsError || !strings.Contains(g.Text, foreignLive) {
			t.Fatalf("the scope grant does not read the live block (fixture broken): %+v", g)
		}
		ref := f.call(t, granted, "get", map[string]any{"id": i201Unknown, "include_archived": true})
		if a := f.call(t, granted, "get", map[string]any{"id": foreignArchived, "include_archived": true}); !a.IsError || a != ref {
			t.Errorf("archived block in a granted scope = %+v, want the unknown-id answer %+v", a, ref)
		}
	})
}

func TestManageGetArchived(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI201Fixture(t)
	ctx := context.Background()

	own := f.store(t, f.direct, "i201-rest-archived")
	if _, err := store.DeleteBlock(ctx, f.pool, own, []string{"private"}); err != nil {
		t.Fatalf("archive own: %v", err)
	}
	foreignArchived := f.store(t, f.other, "i201-rest-foreign")
	if _, err := store.CreateBlockGrant(ctx, f.pool, foreignArchived, store.DefaultTenantID, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if _, err := store.DeleteBlock(ctx, f.pool, foreignArchived, []string{"i201-b"}); err != nil {
		t.Fatalf("archive foreign: %v", err)
	}

	h := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.cfg.Blocktypes)
	get := func(body map[string]any) (int, map[string]any) {
		raw, _ := json.Marshal(body)
		req := httptest.NewRequestWithContext(f.direct, http.MethodPost, "/api/manage", bytes.NewReader(raw))
		rec := httptest.NewRecorder()
		h.HandleManage(rec, req)
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode manage get: %v (%s)", err, rec.Body.String())
		}
		return rec.Code, resp
	}

	t.Run("without the opt-in an archived block is not found", func(t *testing.T) {
		code, resp := get(map[string]any{"action": "get", "id": own})
		if code != http.StatusOK || resp["success"] != false || resp["error"] != "Block not found" {
			t.Errorf("manage get archived = %d %v", code, resp)
		}
	})

	t.Run("include_archived reads it, marked archived and not graph-focusable", func(t *testing.T) {
		code, resp := get(map[string]any{"action": "get", "id": own, "include_archived": true})
		if code != http.StatusOK || resp["success"] != true {
			t.Fatalf("manage get include_archived = %d %v", code, resp)
		}
		b, _ := resp["block"].(map[string]any)
		if b["id"] != own || b["archived"] != true || b["content"] != "content of i201-rest-archived" {
			t.Errorf("block = %v", b)
		}
		if resp["graph_visible"] != false {
			t.Errorf("graph_visible = %v, want false for an archived block", resp["graph_visible"])
		}
	})

	t.Run("include_archived on a live block answers the same as without", func(t *testing.T) {
		live := f.store(t, f.direct, "i201-rest-live")
		_, plain := get(map[string]any{"action": "get", "id": live})
		_, opted := get(map[string]any{"action": "get", "id": live, "include_archived": true})
		pb, _ := json.Marshal(plain)
		ob, _ := json.Marshal(opted)
		if plain["success"] != true || string(pb) != string(ob) {
			t.Errorf("live block: plain %s, opted %s", pb, ob)
		}
	})

	t.Run("include_archived never reaches through a grant", func(t *testing.T) {
		_, unknown := get(map[string]any{"action": "get", "id": i201Unknown, "include_archived": true})
		_, granted := get(map[string]any{"action": "get", "id": foreignArchived, "include_archived": true})
		if granted["success"] != false || granted["error"] != unknown["error"] {
			t.Errorf("granted archived = %v, unknown = %v", granted, unknown)
		}
	})

	t.Run("include_archived with an id prefix is refused", func(t *testing.T) {
		code, resp := get(map[string]any{"action": "get", "id": own[:20], "include_archived": true})
		if code != http.StatusBadRequest || !strings.Contains(resp["error"].(string), "full block id") {
			t.Errorf("prefix with include_archived = %d %v", code, resp)
		}
	})
}
