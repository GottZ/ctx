//go:build integration

// Issue #211 integration probes (real PG18 testcontainer): the MCP store_batch
// tool over the real transport by tool NAME, REST POST /api/store/batch
// through its handler, and the staged form through both confirm surfaces.
// Every refusal is checked with what it must leave behind: the block rows, the
// dream links, the write log (which is also the write budget) and the open
// stage rows unchanged.
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestStoreBatch' -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// sbRateLimit and sbMaxItems are the fixture's query.rate_limit_write and
// writes.batch_max_items. Every subtest takes a fresh key, so each starts with
// an empty budget window.
const (
	sbRateLimit = 6
	sbMaxItems  = 4
	sbUnknownID = "019f0000-0000-7000-8000-000000000211"
)

var sbHashRe = regexp.MustCompile(`payload_hash: ([0-9a-f]{64})`)

type sbFixture struct {
	pool *pgxpool.Pool
	cfg  *config.Store
	reg  *blocktype.Registry
	mcp  http.Handler
	n    int
}

type sbAnswer struct {
	RPCError string
	IsError  bool
	Text     string
	Code     string
}

// sbKey is one fresh key: its id and its authenticated request context.
type sbKey struct {
	id, plain string
	ctx       context.Context
}

func newSBFixture(t *testing.T) *sbFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password")
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CTX_RATE_LIMIT_WRITE", fmt.Sprint(sbRateLimit))
	t.Setenv("CTX_WRITES_BATCH_MAX_ITEMS", fmt.Sprint(sbMaxItems))
	envCfg, issues := config.FromEnv()
	issues = append(issues, config.Validate(envCfg)...)
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(envCfg)
	if err := settings.Reload(ctx, pool, cfgStore); err != nil {
		t.Fatalf("settings reload: %v", err)
	}
	reg := v11Registry(t, pool)
	return &sbFixture{pool: pool, cfg: cfgStore, reg: reg,
		mcp: NewMCPHandler(MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg})}
}

// key creates a fresh key with home scope private (and the extra scopes it may
// read and write), flagged with confirm_writes when confirm is true.
func (f *sbFixture) key(t *testing.T, confirm bool, extra ...string) sbKey {
	t.Helper()
	ctx := context.Background()
	f.n++
	label := fmt.Sprintf("sb-key-%d", f.n)
	row, plain, err := store.CreateApiKey(ctx, f.pool, label, "private", append([]string{"private"}, extra...), store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = $2, write_scopes = COALESCE($3::text[], '{}') WHERE id = $1`,
		row.ID, confirm, extra); err != nil {
		t.Fatalf("key flags: %v", err)
	}
	return f.auth(t, row.ID, plain)
}

func (f *sbFixture) auth(t *testing.T, id, plain string) sbKey {
	t.Helper()
	ar, err := auth.Authenticate(context.Background(), f.pool, plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	return sbKey{id: id, plain: plain, ctx: context.WithValue(context.Background(), authResultKey, ar)}
}

// mcpCall posts one tools/call over the real MCP transport.
func (f *sbFixture) mcpCall(t *testing.T, k sbKey, tool string, args map[string]any) sbAnswer {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args},
	})
	req := httptest.NewRequestWithContext(k.ctx, http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	f.mcp.ServeHTTP(rec, req)
	var resp struct {
		Result *struct {
			IsError           bool `json:"isError"`
			Content           []struct{ Text string } `json:"content"`
			StructuredContent struct {
				Code string `json:"code"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode MCP answer %q: %v", rec.Body.String(), err)
	}
	if resp.Error != nil {
		return sbAnswer{RPCError: resp.Error.Message}
	}
	a := sbAnswer{IsError: resp.Result.IsError, Code: resp.Result.StructuredContent.Code}
	for _, c := range resp.Result.Content {
		a.Text += c.Text
	}
	return a
}

// rest posts one /api/store/batch body and returns status and decoded body.
func (f *sbFixture) rest(t *testing.T, k sbKey, body any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/store/batch", bytes.NewReader(raw)).WithContext(k.ctx)
	rec := httptest.NewRecorder()
	NewStoreHandler(f.pool, f.cfg, f.reg).HandleStoreBatch(rec, req)
	var m map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("decode REST answer %q: %v", rec.Body.String(), err)
	}
	return rec.Code, m
}

// sbCounts is what a refused batch must leave as it was.
type sbCounts struct {
	blocks, links, writes, openStages, deltas int
	budget                                    int // the key's write budget used in the current window
}

func (f *sbFixture) counts(t *testing.T, k sbKey) sbCounts {
	t.Helper()
	ctx := context.Background()
	var c sbCounts
	if err := f.pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM context_blocks)::int,
		(SELECT count(*) FROM context_dream_links)::int,
		(SELECT count(*) FROM context_access_log WHERE action = 'write')::int,
		(SELECT count(*) FROM context_pending_writes WHERE consumed_at IS NULL)::int,
		(SELECT count(*) FROM context_block_count_deltas)::int`).Scan(
		&c.blocks, &c.links, &c.writes, &c.openStages, &c.deltas); err != nil {
		t.Fatalf("counts: %v", err)
	}
	used, err := store.CheckRateLimit(ctx, f.pool, k.id)
	if err != nil {
		t.Fatalf("budget: %v", err)
	}
	c.budget = used
	return c
}

func (f *sbFixture) content(t *testing.T, id string) string {
	t.Helper()
	var s string
	if err := f.pool.QueryRow(context.Background(), `SELECT content FROM context_blocks WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("content of %s: %v", id, err)
	}
	return s
}

func (f *sbFixture) seed(t *testing.T, title, content string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, "learnings", title, content, nil, nil, "private", false,
		store.SensitivityWrite{Value: backends.SensInternal}, "")
	if err != nil {
		t.Fatalf("seed %s: %v", title, err)
	}
	return b.ID
}

func sbItem(title, content string, extra ...any) map[string]any {
	m := map[string]any{"category": "learnings", "title": title, "content": content}
	for i := 0; i+1 < len(extra); i += 2 {
		m[extra[i].(string)] = extra[i+1]
	}
	return m
}

func sbItems(items ...map[string]any) map[string]any {
	if items == nil {
		items = []map[string]any{}
	}
	return map[string]any{"items": items}
}

// TestStoreBatchRefusalsWriteNothing is the all-or-none contract, one case per
// refusal: the class and the named item on both surfaces, and nothing written,
// logged or spent. The in-transaction cases put a writing item IN FRONT of the
// refused one in the batch's write order, so their rollback is what is tested.
func TestStoreBatchRefusalsWriteNothing(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()

	existing := f.seed(t, "sb-0-existing", "original body")
	md := map[string]any{derived.MetadataKey: map[string]any{
		"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "i211",
	}}
	if _, err := store.UpsertBlock(ctx, f.pool, "learnings", "sb-derivative", "derived body", nil, md,
		"private", true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true}, derived.TypeCatalog); err != nil {
		t.Fatalf("server-path derivative: %v", err)
	}
	big := strings.Repeat("x", 50*1024+1)
	mutP, mutQ := f.seed(t, "sb-mut-p", "p body"), f.seed(t, "sb-mut-q", "q body")

	cases := []struct {
		name     string
		body     map[string]any
		code     string
		text     string
		restOnly bool // MCP's own schema refuses the shape before the core runs (no case left since #211's update items)
	}{
		{"no item", sbItems(), "missing_fields", "items: a batch needs at least one item", false},
		{"above the cap", sbItems(sbItem("sb-c1", "a"), sbItem("sb-c2", "b"), sbItem("sb-c3", "c"), sbItem("sb-c4", "d"), sbItem("sb-c5", "e")),
			"size_cap", "items: 5 items exceed the batch cap of 4", false},
		{"missing content", sbItems(sbItem("sb-m1", "a"), map[string]any{"category": "learnings", "title": "sb-m2"}),
			"missing_fields", "items[1]: Missing required fields", false},
		{"content above 50 KB", sbItems(sbItem("sb-s1", "a"), sbItem("sb-s2", big)), "size_cap", "items[1]: Content exceeds 50KB", false},
		{"invalid sensitivity", sbItems(sbItem("sb-v1", "a"), sbItem("sb-v2", "b", "sensitivity", "secret")),
			"invalid_sensitivity", "items[1]: Invalid sensitivity", false},
		{"unknown type", sbItems(sbItem("sb-t1", "a"), sbItem("sb-t2", "b", "type", "no-such-type")),
			"unknown_type", "items[1]: type: unknown block type", false},
		{"reserved type", sbItems(sbItem("sb-r1", "a"), sbItem("sb-r2", "b", "type", "issue")),
			"reserved_type", "items[1]: type:", false},
		{"reserved metadata", sbItems(sbItem("sb-p1", "a"), sbItem("sb-p2", "b", "metadata", md)),
			"reserved_metadata", "items[1]: metadata:", false},
		{"scope not writable", sbItems(sbItem("sb-w1", "a"), sbItem("sb-w2", "b", "scope", "sb-foreign")),
			"scope_denied", "items[1]: Cannot write to requested scope", false},
		{"one identity twice", sbItems(sbItem("sb-d1", "a"), sbItem("sb-d1", "b")),
			"constraint", "items[1]: same category, title and scope as items[0]", false},
		{"one supersedes target twice", sbItems(sbItem("sb-st1", "a"), sbItem("sb-st2", "b", "supersedes", existing),
			sbItem("sb-st3", "c", "supersedes", strings.ToUpper(existing))),
			"supersedes_conflict", "items[2]: supersedes the same block as items[1]", false},
		{"two blocks declaring each other in the transaction", sbItems(sbItem("sb-0-existing", "rewritten body"),
			sbItem("sb-mut-p", "p rewritten", "supersedes", mutQ), sbItem("sb-mut-q", "q rewritten", "supersedes", mutP)),
			"supersedes_conflict", "items[2]: supersedes: block " + mutP + " already supersedes this block", false},
		{"derivative in the transaction", sbItems(sbItem("sb-0-existing", "rewritten body"), sbItem("sb-a-new", "new"),
			sbItem("sb-derivative", "client body")), "provenance_protected", "items[2]: block carries derived provenance", false},
		{"unknown supersedes in the transaction", sbItems(sbItem("sb-0-existing", "rewritten body"), sbItem("sb-b-new", "new"),
			sbItem("sb-c-new", "new", "supersedes", sbUnknownID)), "unknown_supersedes", "items[2]: supersedes:", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			k := f.key(t, false)
			before := f.counts(t, k)

			status, resp := f.rest(t, k, c.body)
			if status < 400 || resp["code"] != c.code || !strings.HasPrefix(fmt.Sprint(resp["error"]), c.text) {
				t.Errorf("REST: status %d body %v; want code %s and text %q", status, resp, c.code, c.text)
			}
			if after := f.counts(t, k); after != before {
				t.Errorf("REST refusal changed the store: before %+v after %+v", before, after)
			}
			if !c.restOnly {
				a := f.mcpCall(t, k, "store_batch", c.body)
				if !a.IsError || a.Code != c.code || a.Text != resp["error"] {
					t.Errorf("MCP: %+v; want code %s and the REST text %q", a, c.code, resp["error"])
				}
				if after := f.counts(t, k); after != before {
					t.Errorf("MCP refusal changed the store: before %+v after %+v", before, after)
				}
			}
			if got := f.content(t, existing); got != "original body" {
				t.Fatalf("an item ahead of the refused one survived the rollback: content %q", got)
			}
		})
	}

	t.Run("the batch budget counts every write", func(t *testing.T) {
		k := f.key(t, false)
		for i := 0; i < 3; i++ {
			if a := f.mcpCall(t, k, "store", sbItem(fmt.Sprintf("sb-budget-single-%d", i), "x")); a.IsError {
				t.Fatalf("single store %d: %+v", i, a)
			}
		}
		before := f.counts(t, k)
		four := sbItems(sbItem("sb-bu-1", "a"), sbItem("sb-bu-2", "b"), sbItem("sb-bu-3", "c"), sbItem("sb-bu-4", "d"))
		a := f.mcpCall(t, k, "store_batch", four)
		if !a.IsError || a.Code != "rate_limit" ||
			!strings.HasPrefix(a.Text, fmt.Sprintf("Rate limit exceeded: max %d writes per 60 seconds", sbRateLimit)) {
			t.Fatalf("3 used + 4 > %d: %+v; want the rate_limit refusal", sbRateLimit, a)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("budget refusal changed the store: before %+v after %+v", before, after)
		}
		three := sbItems(sbItem("sb-bu-1", "a"), sbItem("sb-bu-2", "b"), sbItem("sb-bu-3", "c"))
		if a := f.mcpCall(t, k, "store_batch", three); a.IsError {
			t.Fatalf("3 used + 3 = %d: %+v; want it written", sbRateLimit, a)
		}
		if got := f.counts(t, k); got.budget != sbRateLimit || got.blocks != before.blocks+3 {
			t.Fatalf("after 3 used + 3: %+v; want budget %d and three more blocks", got, sbRateLimit)
		}
	})
}

// TestStoreBatchWrites is the success side on both surfaces: every item in one
// answer, an unchanged item not written and not booked, a declared supersedes
// applied, and the write log booking one write per written block.
func TestStoreBatchWrites(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	old := f.seed(t, "sb-w-old", "old statement")
	same := f.seed(t, "sb-w-same", "same body")

	t.Run("MCP", func(t *testing.T) {
		k := f.key(t, false)
		before := f.counts(t, k)
		a := f.mcpCall(t, k, "store_batch", sbItems(
			sbItem("sb-w-record", "the record"),
			sbItem("sb-w-same", "same body"),
			sbItem("sb-w-new", "the correction", "supersedes", old),
		))
		if a.IsError {
			t.Fatalf("store_batch: %+v", a)
		}
		lines := strings.Split(strings.TrimSpace(a.Text), "\n")
		if len(lines) != 4 || lines[0] != "Batch written in one transaction: 2 stored, 1 unchanged." ||
			!strings.HasPrefix(lines[1], "items[0]: Stored: sb-w-record (id: ") ||
			lines[2] != "items[1]: No change needed — identical content already exists (id: "+same+")" ||
			!strings.HasSuffix(lines[3], ", supersedes: "+old+")") {
			t.Fatalf("answer:\n%s", a.Text)
		}
		after := f.counts(t, k)
		if after.blocks != before.blocks+2 || after.writes != before.writes+2 || after.budget != 2 || after.links != before.links+1 {
			t.Fatalf("before %+v after %+v; want two blocks, two write rows and one declared link", before, after)
		}
		var supersededBy string
		if err := f.pool.QueryRow(ctx, `SELECT COALESCE(superseded_by::text, '') FROM context_blocks WHERE id = $1`, old).Scan(&supersededBy); err != nil {
			t.Fatal(err)
		}
		if supersededBy == "" {
			t.Fatalf("the superseded block carries no superseded_by")
		}
	})

	t.Run("REST", func(t *testing.T) {
		k := f.key(t, false)
		before := f.counts(t, k)
		status, resp := f.rest(t, k, sbItems(
			sbItem("sb-r-one", "one", "tags", []string{"x"}),
			sbItem("sb-w-same", "same body"),
		))
		if status != http.StatusOK || resp["success"] != true {
			t.Fatalf("status %d body %v", status, resp)
		}
		items, _ := resp["items"].([]any)
		if len(items) != 2 {
			t.Fatalf("items %v", resp["items"])
		}
		first, _ := items[0].(map[string]any)
		if block, _ := first["block"].(map[string]any); block["title"] != "sb-r-one" {
			t.Errorf("items[0] = %v; want the written block", first)
		}
		second, _ := items[1].(map[string]any)
		if second["action"] != "noop" || second["existing_id"] != same {
			t.Errorf("items[1] = %v; want the /api/store noop answer", second)
		}
		if after := f.counts(t, k); after.blocks != before.blocks+1 || after.budget != 1 {
			t.Fatalf("before %+v after %+v", before, after)
		}
	})
}

// TestStoreBatchStaged is the confirm_writes form: one card for the batch, the
// budget charged at stage time, the confirm writing every item in one
// transaction through either confirm surface, and the re-checks before and
// the refusals after the consume writing nothing.
func TestStoreBatchStaged(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()

	stage := func(t *testing.T, k sbKey, body map[string]any) string {
		t.Helper()
		a := f.mcpCall(t, k, "store_batch", body)
		m := sbHashRe.FindStringSubmatch(a.Text)
		if !a.IsError || a.Code != "" || !strings.HasPrefix(a.Text, "STAGED — NOT saved yet.") || m == nil {
			t.Fatalf("stage: %+v; want an uncoded STAGED answer with a payload_hash", a)
		}
		return m[1]
	}

	t.Run("stage then confirm over MCP", func(t *testing.T) {
		k := f.key(t, true)
		before := f.counts(t, k)
		body := sbItems(sbItem("sb-st-1", "one"), sbItem("sb-st-2", "two"))
		hash := stage(t, k, body)
		staged := f.counts(t, k)
		if staged.blocks != before.blocks || staged.openStages != before.openStages+1 || staged.budget != 2 {
			t.Fatalf("after stage %+v (before %+v); want no block, one open card, two intents", staged, before)
		}
		if again := stage(t, k, body); again != hash {
			t.Fatalf("the same batch staged twice: %s then %s", hash, again)
		}
		if other := stage(t, k, sbItems(sbItem("sb-st-1", "one"), sbItem("sb-st-2", "TWO"))); other == hash {
			t.Fatalf("a batch with one item changed has the same hash")
		}
		a := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
		if a.IsError || !strings.HasPrefix(a.Text, "Confirmed and stored 2 blocks in one transaction:\nitems[0]: sb-st-1 (id: ") {
			t.Fatalf("confirm: %+v", a)
		}
		after := f.counts(t, k)
		if after.blocks != before.blocks+2 || after.budget != staged.budget+4 {
			// +4: the re-armed stage and the stage of the changed batch booked
			// two intents each; the confirm books none.
			t.Fatalf("after confirm %+v (staged %+v); want two blocks and no booking of the confirm", after, staged)
		}
	})

	t.Run("stage then confirm over HTTP", func(t *testing.T) {
		k := f.key(t, true)
		hash := stage(t, k, sbItems(sbItem("sb-sh-1", "one"), sbItem("sb-sh-2", "two")))
		raw, _ := json.Marshal(map[string]any{"payload_hash": hash})
		req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(raw)).WithContext(k.ctx)
		rec := httptest.NewRecorder()
		NewConfirmHandler(f.pool, f.reg).HandleConfirm(rec, req)
		var resp struct {
			Success bool
			Op      string
			Blocks  []struct{ ID, Title, Category, Scope string }
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusOK || !resp.Success ||
			resp.Op != store.OpStoreBatch || len(resp.Blocks) != 2 || resp.Blocks[1].Title != "sb-sh-2" {
			t.Fatalf("HTTP confirm: status %d body %s", rec.Code, rec.Body.String())
		}
	})

	// The refusal names the item, as every other refusal of a batch does, on
	// both confirm surfaces (review of #389) — whether the lost scope is
	// another item's (sb-extra sorts after private) or the one the card's row
	// binds, the smallest of the items' scopes (aa-sb-extra sorts before it).
	t.Run("a scope that is no longer writable refuses the confirm, names the item and keeps the card", func(t *testing.T) {
		for _, extra := range []string{"sb-extra", "aa-sb-extra"} {
			t.Run(extra, func(t *testing.T) {
				k := f.key(t, true, extra)
				hash := stage(t, k, sbItems(sbItem("sb-sc-1-"+extra, "home"), sbItem("sb-sc-2-"+extra, "extra", "scope", extra)))
				if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET write_scopes = '{}' WHERE id = $1`, k.id); err != nil {
					t.Fatal(err)
				}
				k = f.auth(t, k.id, k.plain) // the confirm reads the rights of now
				want := "items[1]: " + confirmScopeRejectMsg(extra)
				before := f.counts(t, k)
				a := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
				if !a.IsError || a.Text != want {
					t.Errorf("MCP confirm after the shrink: %+v; want %q", a, want)
				}
				raw, _ := json.Marshal(map[string]any{"payload_hash": hash})
				req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(raw)).WithContext(k.ctx)
				rec := httptest.NewRecorder()
				NewConfirmHandler(f.pool, f.reg).HandleConfirm(rec, req)
				var resp map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || rec.Code != http.StatusForbidden || resp["error"] != want {
					t.Errorf("HTTP confirm after the shrink: status %d body %s; want 403 and %q", rec.Code, rec.Body.String(), want)
				}
				if after := f.counts(t, k); after != before {
					t.Fatalf("refused confirm changed the store or spent the card: before %+v after %+v", before, after)
				}
			})
		}
	})

	t.Run("a claim the registry of now refuses keeps the card", func(t *testing.T) {
		k := f.key(t, true)
		hash := stage(t, k, sbItems(sbItem("sb-cl-1", "one"), sbItem("sb-cl-2", "two", "type", "reference")))
		// The confirm re-runs the claim gates against the registry of the
		// moment it runs; a server without one refuses every named type.
		unwired := &sbFixture{pool: f.pool, mcp: NewMCPHandler(MCPConfig{Pool: f.pool, Cfg: f.cfg})}
		before := f.counts(t, k)
		a := unwired.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
		if !a.IsError || a.Code != "unknown_type" || !strings.HasPrefix(a.Text, "items[1]: type: block-type registry not wired") {
			t.Fatalf("confirm against no registry: %+v", a)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("refused confirm changed the store or spent the card: before %+v after %+v", before, after)
		}
		if a := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash}); a.IsError {
			t.Fatalf("the kept card does not confirm: %+v", a)
		}
	})

	t.Run("declarations that refuse each other refuse the stage", func(t *testing.T) {
		k := f.key(t, true)
		p, q := f.seed(t, "sb-sm-p", "p body"), f.seed(t, "sb-sm-q", "q body")
		before := f.counts(t, k)
		a := f.mcpCall(t, k, "store_batch", sbItems(
			sbItem("sb-sm-p", "p rewritten", "supersedes", q), sbItem("sb-sm-q", "q rewritten", "supersedes", p)))
		if !a.IsError || a.Code != "supersedes_conflict" || !strings.HasPrefix(a.Text, "items[1]: supersedes: block "+p) {
			t.Fatalf("stage of two declarations closing a cycle: %+v; want the refusal, no card", a)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("refused stage changed the store, staged a card or spent budget: before %+v after %+v", before, after)
		}
	})

	t.Run("a refusal after the consume writes nothing and spends the card", func(t *testing.T) {
		k := f.key(t, true)
		target := f.seed(t, "sb-pc-target", "target body")
		hash := stage(t, k, sbItems(sbItem("sb-pc-1", "first"), sbItem("sb-pc-2", "second", "supersedes", target)))
		// Another writer settles the target between stage and confirm.
		other := f.key(t, false)
		if a := f.mcpCall(t, other, "store", sbItem("sb-pc-other", "other", "supersedes", target)); a.IsError {
			t.Fatalf("other writer: %+v", a)
		}
		before := f.counts(t, k)
		a := f.mcpCall(t, k, "confirm", map[string]any{"payload_hash": hash})
		if !a.IsError || a.Code != "supersedes_conflict" ||
			!strings.HasPrefix(a.Text, confirmStoreBatchPrefix+"items[1]: supersedes: block "+target) {
			t.Fatalf("confirm: %+v", a)
		}
		after := f.counts(t, k)
		if after.blocks != before.blocks || after.links != before.links || after.openStages != before.openStages-1 {
			t.Fatalf("before %+v after %+v; want nothing written and the card consumed", before, after)
		}
	})
}

// TestStoreBatchSupersedesTargetAsTheBatchLeftIt (review of #389): an item that
// declares a supersedes is checked against its target as the items written
// before it in the batch left that target. Item 0 retypes the target to
// checkpoint, a type outside the dream loop, and is written first (its title
// sorts first); item 1 declares to supersede that target. A single store
// refuses such a declaration; the batch checked the type its lock pass had
// read before item 0's write and committed the link.
func TestStoreBatchSupersedesTargetAsTheBatchLeftIt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	retypeThenDeclare := func(target, tag string) map[string]any {
		return sbItems(sbItem("sb-rt-a-"+tag, "target body", "type", "checkpoint"),
			sbItem("sb-rt-b-"+tag, "correction", "supersedes", target))
	}
	wantRefusal := func(target string) string {
		return "items[1]: supersedes: block " + target + " cannot be superseded"
	}

	t.Run("direct, REST and MCP", func(t *testing.T) {
		target := f.seed(t, "sb-rt-a-direct", "target body")
		k := f.key(t, false)
		before := f.counts(t, k)
		body := retypeThenDeclare(target, "direct")
		status, resp := f.rest(t, k, body)
		if status != http.StatusUnprocessableEntity || resp["code"] != "supersedes_not_allowed" ||
			!strings.HasPrefix(fmt.Sprint(resp["error"]), wantRefusal(target)) {
			t.Errorf("REST: status %d body %v; want 422 supersedes_not_allowed for items[1]", status, resp)
		}
		a := f.mcpCall(t, k, "store_batch", body)
		if !a.IsError || a.Code != "supersedes_not_allowed" || !strings.HasPrefix(a.Text, wantRefusal(target)) {
			t.Errorf("MCP: %+v; want supersedes_not_allowed for items[1]", a)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("the batch changed the store: before %+v after %+v", before, after)
		}
	})

	// The card must not promise what the confirm refuses: the stage checks the
	// declaration against the batch's own writes too.
	t.Run("staged", func(t *testing.T) {
		target := f.seed(t, "sb-rt-a-staged", "target body")
		k := f.key(t, true)
		before := f.counts(t, k)
		a := f.mcpCall(t, k, "store_batch", retypeThenDeclare(target, "staged"))
		if !a.IsError || a.Code != "supersedes_not_allowed" || !strings.HasPrefix(a.Text, wantRefusal(target)) {
			t.Fatalf("stage: %+v; want the refusal and no card", a)
		}
		if after := f.counts(t, k); after != before {
			t.Fatalf("refused stage changed the store, staged a card or spent budget: before %+v after %+v", before, after)
		}
	})

	// The other write order — declare first, retype after (the target's title
	// sorts after the declaring item's) — is admitted, as the same two writes
	// are admitted as two single stores in that order: a block that is
	// superseded may change its type afterwards.
	t.Run("declare, then retype", func(t *testing.T) {
		target := f.seed(t, "sb-ru-z-target", "target body")
		k := f.key(t, false)
		status, resp := f.rest(t, k, sbItems(sbItem("sb-ru-z-target", "target body", "type", "checkpoint"),
			sbItem("sb-ru-a-new", "correction", "supersedes", target)))
		if status != http.StatusOK {
			t.Fatalf("batch: status %d body %v", status, resp)
		}
		single := f.seed(t, "sb-ru-z-single", "target body")
		if a := f.mcpCall(t, k, "store", sbItem("sb-ru-a-single", "correction", "supersedes", single)); a.IsError {
			t.Fatalf("single declare: %+v", a)
		}
		if a := f.mcpCall(t, k, "store", sbItem("sb-ru-z-single", "target body", "type", "checkpoint")); a.IsError {
			t.Fatalf("single retype of the superseded block: %+v", a)
		}
		var types []string
		if err := f.pool.QueryRow(ctx, `SELECT array_agg(type_name ORDER BY id) FROM context_blocks WHERE id IN ($1, $2)`,
			target, single).Scan(&types); err != nil {
			t.Fatal(err)
		}
		if len(types) != 2 || types[0] != "checkpoint" || types[1] != "checkpoint" {
			t.Fatalf("types %v; want both targets retyped", types)
		}
	})
}
