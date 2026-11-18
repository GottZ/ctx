//go:build integration

// Issue #345 — the MCP read tools leave the access record REST manage-get
// leaves.
//
// Before: REST POST /api/manage {get} booked one context_access_log row for the
// block it returned; MCP get, search and recent booked nothing, and the Hermes
// plugin reads through MCP. The age of a block at the moment an agent read it
// was therefore not recorded for the reader that matters (#291).
//
// Now every one of the four surfaces books one row per block it HANDED to the
// caller, through one function (store.RecordBlockReads): reader key and
// principal, block id, action = surface and tool, and the block's type and
// created_at in metadata, so the age at read survives the block's delete
// (block_id is ON DELETE SET NULL). A refused read books nothing, and a log
// that fails or stalls never fails or stalls the read.
//
// Fixtures go through the production write paths (store.UpsertBlock, the MCP
// store tool's store core, store.CreateBlockGrant, store.DeleteBlock).
//
//	go test -tags=integration -p 1 ./internal/handler/ -run TestMCPReadAccess -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i345Row is one context_access_log row as the read record writes it.
type i345Row struct {
	KeyID       *string
	BlockID     *string
	Action      string
	QueryText   *string
	PrincipalID *string
	Meta        map[string]any
}

// i345Settle waits for the read record's queue, so a test can count rows —
// including the absence of rows — after the read returned.
func i345Settle(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := store.SettleBlockReads(ctx, pool); err != nil {
		t.Fatalf("settle read record: %v", err)
	}
}

// i345Rows reads the rows of one action, oldest first.
func i345Rows(t *testing.T, pool *pgxpool.Pool, action string) []i345Row {
	t.Helper()
	i345Settle(t, pool)
	rows, err := pool.Query(context.Background(),
		`SELECT api_key_id::text, block_id::text, action, query_text, principal_id::text, metadata
		   FROM context_access_log WHERE action = $1 ORDER BY id`, action)
	if err != nil {
		t.Fatalf("read access log: %v", err)
	}
	defer rows.Close()
	var out []i345Row
	for rows.Next() {
		var r i345Row
		var meta []byte
		if err := rows.Scan(&r.KeyID, &r.BlockID, &r.Action, &r.QueryText, &r.PrincipalID, &meta); err != nil {
			t.Fatalf("scan access log: %v", err)
		}
		if err := json.Unmarshal(meta, &r.Meta); err != nil {
			t.Fatalf("decode metadata %s: %v", meta, err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("access log rows: %v", err)
	}
	return out
}

func i345Str(p *string) string {
	if p == nil {
		return "<nil>"
	}
	return *p
}

func i345BlockIDs(rows []i345Row) []string {
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.BlockID == nil {
			ids = append(ids, "<nil>")
			continue
		}
		ids = append(ids, *r.BlockID)
	}
	sort.Strings(ids)
	return ids
}

func i345Sorted(ids []string) []string {
	out := append([]string(nil), ids...)
	sort.Strings(out)
	return out
}

// i345Fixture is a key of the default tenant (home scope private) plus a
// second tenant with a scope of its own.
type i345Fixture struct {
	pool      *pgxpool.Pool
	cfg       MCPConfig
	reg       *blocktype.Registry
	ar        *auth.AuthResult
	keyCtx    context.Context
	otherScop string
	otherTen  string
}

func i345Setup(t *testing.T, pool *pgxpool.Pool) *i345Fixture {
	t.Helper()
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	_, plain, err := store.CreateApiKey(ctx, pool, "i345", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)
	other := bgTenant(t, pool, "i345-other")
	bgMapScope(t, pool, "i345-other", other)
	return &i345Fixture{
		pool:      pool,
		cfg:       MCPConfig{Pool: pool, Blocktypes: reg},
		reg:       reg,
		ar:        AuthResultFromContext(keyCtx),
		keyCtx:    keyCtx,
		otherScop: "i345-other",
		otherTen:  other,
	}
}

func (f *i345Fixture) upsert(t *testing.T, scope, category, title, typeName string) *store.Block {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.pool, category, title, "body of "+title, nil, nil, scope, true,
		store.SensitivityWrite{Value: backends.SensPublic, Manual: true}, typeName)
	if err != nil {
		t.Fatalf("upsert %s: %v", title, err)
	}
	return b
}

// createdAt is the block's created_at as the database holds it.
func (f *i345Fixture) createdAt(t *testing.T, id string) time.Time {
	t.Helper()
	var ts time.Time
	if err := f.pool.QueryRow(context.Background(),
		`SELECT created_at FROM context_blocks WHERE id = $1::uuid`, id).Scan(&ts); err != nil {
		t.Fatalf("created_at of %s: %v", id, err)
	}
	return ts
}

func (f *i345Fixture) get(t *testing.T, ctx context.Context, in getInput) (bool, string) {
	t.Helper()
	r, _, err := mcpGetHandler(f.cfg)(ctx, nil, in)
	if err != nil {
		t.Fatalf("mcp get %s: protocol error %v", in.ID, err)
	}
	return r.IsError, mcpTextOf(r)
}

// i345AssertRow checks the shape every read row carries.
func i345AssertRow(t *testing.T, r i345Row, keyID, principalID, blockID, typeName string, created time.Time) {
	t.Helper()
	if i345Str(r.KeyID) != keyID {
		t.Errorf("row %s: api_key_id = %s, want the reader's key %s", r.Action, i345Str(r.KeyID), keyID)
	}
	if i345Str(r.PrincipalID) != principalID {
		t.Errorf("row %s: principal_id = %s, want the reader's principal %s", r.Action, i345Str(r.PrincipalID), principalID)
	}
	if i345Str(r.BlockID) != blockID {
		t.Errorf("row %s: block_id = %s, want %s", r.Action, i345Str(r.BlockID), blockID)
	}
	if r.QueryText != nil {
		t.Errorf("row %s: query_text = %q, want none", r.Action, *r.QueryText)
	}
	if got, _ := r.Meta["type"].(string); got != typeName {
		t.Errorf("row %s: metadata.type = %v, want %q", r.Action, r.Meta["type"], typeName)
	}
	raw, _ := r.Meta["block_created_at"].(string)
	ts, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil || !ts.Equal(created) {
		t.Errorf("row %s: metadata.block_created_at = %q (%v), want %s", r.Action, raw, err, created)
	}
	if len(r.Meta) != 2 {
		t.Errorf("row %s: metadata = %v, want exactly type and block_created_at", r.Action, r.Meta)
	}
}

func TestMCPReadAccess_Get_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	f := i345Setup(t, pool)
	ctx := context.Background()

	cp := f.upsert(t, "private", "i345-cp", "i345 checkpoint part", "checkpoint")
	foreign := f.upsert(t, f.otherScop, "i345-foreign", "i345 foreign block", "")
	granted := f.upsert(t, f.otherScop, "i345-granted", "i345 granted block", "")
	if _, err := store.CreateBlockGrant(ctx, pool, granted.ID, store.DefaultTenantID, ""); err != nil {
		t.Fatalf("grant: %v", err)
	}
	archived := f.upsert(t, "private", "i345-arch", "i345 archived block", "")
	if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, archived.ID); err != nil {
		t.Fatalf("archive: %v", err)
	}

	// Refused reads first: none of them may leave a row (G9, the manage-get
	// oracle rule): an unknown id, a foreign block by full id, an archived
	// block without the opt-in.
	for _, in := range []getInput{
		{ID: "019e0000-0000-7000-8000-00000000dead"},
		{ID: foreign.ID},
		{ID: archived.ID},
	} {
		if isErr, text := f.get(t, f.keyCtx, in); !isErr {
			t.Fatalf("get %s answered %q, want a refusal", in.ID, text)
		}
	}
	if rows := i345Rows(t, pool, store.ActionMCPGet); len(rows) != 0 {
		t.Fatalf("refused gets booked %d rows (%v), want 0", len(rows), i345BlockIDs(rows))
	}

	// Delivered reads: own block by prefix, granted block of another tenant,
	// archived block with the opt-in.
	for _, in := range []getInput{
		{ID: cp.ID[:13]},
		{ID: granted.ID},
		{ID: archived.ID, IncludeArchived: true},
	} {
		if isErr, text := f.get(t, f.keyCtx, in); isErr {
			t.Fatalf("get %s refused: %s", in.ID, text)
		}
	}
	rows := i345Rows(t, pool, store.ActionMCPGet)
	if got, want := i345BlockIDs(rows), i345Sorted([]string{cp.ID, granted.ID, archived.ID}); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("3 delivered gets booked %v, want %v", got, want)
	}
	// Rows of reads that one statement wrote together carry no order among
	// themselves, so they are matched by block id.
	byBlock := map[string]i345Row{}
	for _, r := range rows {
		byBlock[*r.BlockID] = r
	}
	i345AssertRow(t, byBlock[cp.ID], f.ar.ApiKeyID, f.ar.PrincipalID, cp.ID, "checkpoint", f.createdAt(t, cp.ID))
	// The grant read is the READER's row: its key and principal, the owner's
	// block. Nothing of the owner's key enters it.
	i345AssertRow(t, byBlock[granted.ID], f.ar.ApiKeyID, f.ar.PrincipalID, granted.ID, "knowledge", f.createdAt(t, granted.ID))
	i345AssertRow(t, byBlock[archived.ID], f.ar.ApiKeyID, f.ar.PrincipalID, archived.ID, "knowledge", f.createdAt(t, archived.ID))

	// The record outlives the block. An archive (store.DeleteBlock) keeps the
	// row and its join; a purge deletes it, the foreign key clears block_id
	// (SET NULL), and type and created_at stay in the row, so the age at read
	// stays computable.
	created := f.createdAt(t, cp.ID)
	if _, err := store.DeleteBlock(ctx, pool, cp.ID, []string{"private"}); err != nil {
		t.Fatalf("archive: %v", err)
	}
	var joined int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM context_access_log a JOIN context_blocks b ON b.id = a.block_id
		  WHERE a.action = $1 AND b.id = $2::uuid AND b.is_archived`, store.ActionMCPGet, cp.ID).Scan(&joined); err != nil || joined != 1 {
		t.Fatalf("after archive: %d joined rows (%v), want 1", joined, err)
	}
	q := store.PurgeQuery{Scope: "private", AllTenants: true, Filter: store.PurgeFilter{Category: "i345-cp"}}
	dry, err := store.PurgeScope(ctx, pool, q)
	if err != nil || dry.Counted.Blocks != 1 {
		t.Fatalf("purge dry run: %+v, %v", dry, err)
	}
	q.ConfirmBlocks = &dry.Counted.Blocks
	if _, err := store.PurgeScope(ctx, pool, q); err != nil {
		t.Fatalf("purge: %v", err)
	}
	var blockID *string
	var typ, bc string
	if err := pool.QueryRow(ctx,
		`SELECT block_id::text, metadata->>'type', metadata->>'block_created_at' FROM context_access_log
		  WHERE action = $1 AND metadata->>'type' = 'checkpoint'`, store.ActionMCPGet).Scan(&blockID, &typ, &bc); err != nil {
		t.Fatalf("read the deleted block's row: %v", err)
	}
	ts, err := time.Parse(time.RFC3339Nano, bc)
	if blockID != nil || typ != "checkpoint" || err != nil || !ts.Equal(created) {
		t.Errorf("after delete: block_id %s, type %q, block_created_at %q (%v); want NULL, checkpoint, %s",
			i345Str(blockID), typ, bc, err, created)
	}
}

func TestMCPReadAccess_SearchRecent_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	f := i345Setup(t, pool)

	// The shipped list books checkpoint and insight rows of a listing.
	if got := config.Defaults().MCP.ReadLogTypes; fmt.Sprint(got) != "[checkpoint insight]" {
		t.Fatalf("mcp.read_log_types default = %v, want [checkpoint insight]", got)
	}
	// Without config wiring (nil Cfg) a listing books nothing.
	f.upsert(t, "private", "i345-nocfg", "i345 nocfg", "checkpoint")
	if ids := i206PageOfInput(t, f, searchInput{Category: "i345-nocfg"}); len(ids) != 1 {
		t.Fatalf("nocfg search answered %v", ids)
	}
	if rows := i345Rows(t, pool, store.ActionMCPSearch); len(rows) != 0 {
		t.Fatalf("a search without config wiring booked %d rows", len(rows))
	}

	f.cfg.Cfg = staticConfigStore{cfg: &config.Config{MCP: config.MCPServerConfig{ReadLogTypes: []string{"checkpoint"}}}}

	// 60 blocks in one category, every third a checkpoint: a browse page of 50
	// answers 50 of them, and the record books the checkpoint rows among them.
	for i := 0; i < 60; i++ {
		typ := ""
		if i%3 == 0 {
			typ = "checkpoint"
		}
		f.upsert(t, "private", "i345-list", fmt.Sprintf("i345 list %02d", i), typ)
	}
	f.upsert(t, f.otherScop, "i345-list", "i345 list foreign", "checkpoint")

	page := i206PageOfInput(t, f, searchInput{Category: "i345-list", Limit: 50, After: &store.SearchCursor{}})
	if len(page) != 50 {
		t.Fatalf("browse page answered %d rows, want 50", len(page))
	}
	want := f.ofType(t, page, "checkpoint")
	if len(want) == 0 || len(want) == len(page) {
		t.Fatalf("fixture: %d of %d answered rows are checkpoints, want a mix", len(want), len(page))
	}
	rows := i345Rows(t, pool, store.ActionMCPSearch)
	if got := i345BlockIDs(rows); fmt.Sprint(got) != fmt.Sprint(i345Sorted(want)) {
		t.Fatalf("search booked %d rows %v,\nwant the %d answered checkpoint rows %v", len(got), got, len(want), i345Sorted(want))
	}
	for _, r := range rows {
		i345AssertRow(t, r, f.ar.ApiKeyID, f.ar.PrincipalID, *r.BlockID, "checkpoint", f.createdAt(t, *r.BlockID))
	}

	// include_content with a budget that ends the page early books only the
	// rows the answer carries.
	for i := 0; i < 4; i++ {
		f.upsert(t, "private", "i345-big", fmt.Sprintf("i345 big %d", i), "checkpoint")
	}
	if _, err := pool.Exec(context.Background(),
		`UPDATE context_blocks SET content = repeat('x', 40000) WHERE category = 'i345-big'`); err != nil {
		t.Fatalf("grow blocks: %v", err)
	}
	answered := i206PageOfInput(t, f, searchInput{Category: "i345-big", Limit: 10, IncludeContent: true})
	if len(answered) == 0 || len(answered) >= 4 {
		t.Fatalf("include_content page answered %d of 4 rows, want a truncated page", len(answered))
	}
	if got := len(i345Rows(t, pool, store.ActionMCPSearch)) - len(rows); got != len(answered) {
		t.Errorf("truncated page: %d rows booked, %d answered", got, len(answered))
	}

	// recent: one row per answered checkpoint row.
	res, _, err := mcpRecentHandler(f.cfg)(f.keyCtx, nil, recentInput{Category: "i345-list", Limit: 7})
	if err != nil || res.IsError {
		t.Fatalf("recent: err %v, %s", err, mcpTextOf(res))
	}
	recentIDs := f.recentIDs(t, mcpTextOf(res))
	if len(recentIDs) != 7 {
		t.Fatalf("recent answered %d rows, want 7", len(recentIDs))
	}
	recent := i345Rows(t, pool, store.ActionMCPRecent)
	if got, want := i345BlockIDs(recent), i345Sorted(f.ofType(t, recentIDs, "checkpoint")); fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("recent booked %v, want the answered checkpoint rows %v", got, want)
	}
	for _, r := range recent {
		i345AssertRow(t, r, f.ar.ApiKeyID, f.ar.PrincipalID, *r.BlockID, "checkpoint", f.createdAt(t, *r.BlockID))
	}

	// An empty answer books nothing.
	if res, _, err := mcpRecentHandler(f.cfg)(f.keyCtx, nil, recentInput{Category: "i345-none"}); err != nil || res.IsError {
		t.Fatalf("empty recent: err %v", err)
	}
	if got := len(i345Rows(t, pool, store.ActionMCPRecent)); got != len(recent) {
		t.Errorf("empty recent changed the row count to %d", got)
	}
}

// ofType keeps the ids whose block has the given type.
func (f *i345Fixture) ofType(t *testing.T, ids []string, typeName string) []string {
	t.Helper()
	var out []string
	for _, id := range ids {
		var typ string
		if err := f.pool.QueryRow(context.Background(), `SELECT type_name FROM context_blocks WHERE id = $1::uuid`, id).Scan(&typ); err != nil {
			t.Fatalf("type of %s: %v", id, err)
		}
		if typ == typeName {
			out = append(out, id)
		}
	}
	return out
}

// recentIDs reads the ids off the recent tool's text answer.
func (f *i345Fixture) recentIDs(t *testing.T, text string) []string {
	t.Helper()
	var ids []string
	for _, line := range strings.Split(text, "\n") {
		if i := strings.Index(line, " id:"); i >= 0 {
			id := line[i+4:]
			if j := strings.IndexByte(id, ' '); j >= 0 {
				id = id[:j]
			}
			ids = append(ids, id)
		}
	}
	return ids
}

// i206PageOfInput runs the typed search handler and returns the ids of the
// rows it answered, in either answer form.
func i206PageOfInput(t *testing.T, f *i345Fixture, in searchInput) []string {
	t.Helper()
	r, _, err := mcpSearchHandler(f.cfg)(f.keyCtx, nil, in)
	if err != nil || r.IsError {
		t.Fatalf("search %+v: err %v, %s", in, err, mcpTextOf(r))
	}
	var rows []store.BlockPreview
	if in.After != nil || in.IncludeContent {
		var page struct {
			Results []store.BlockPreview `json:"results"`
		}
		if err := json.Unmarshal([]byte(mcpTextOf(r)), &page); err != nil {
			t.Fatalf("decode page: %v", err)
		}
		rows = page.Results
	} else if err := json.Unmarshal([]byte(mcpTextOf(r)), &rows); err != nil {
		t.Fatalf("decode rows: %v", err)
	}
	ids := make([]string, 0, len(rows))
	for _, row := range rows {
		ids = append(ids, row.ID)
	}
	return ids
}

// TestMCPReadAccess_ManageGet_Integration: REST manage-get books the same
// record through the same function.
func TestMCPReadAccess_ManageGet_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	f := i345Setup(t, pool)
	b := f.upsert(t, "private", "i345-rest", "i345 rest block", "checkpoint")
	foreign := f.upsert(t, f.otherScop, "i345-rest", "i345 rest foreign", "")
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, f.reg)

	if code, resp := callManage(t, mh, f.ar, "get", foreign.ID, nil); code != http.StatusOK || resp["success"] != false {
		t.Fatalf("manage get foreign: %d %v, want a not-found answer", code, resp)
	}
	if code, resp := callManage(t, mh, f.ar, "get", b.ID, nil); code != http.StatusOK || resp["success"] != true {
		t.Fatalf("manage get: %d %v", code, resp)
	}
	// Wait for the row whatever writes it, so a slow writer reads as a wrong
	// row and not as a missing one.
	rows := i345Rows(t, pool, store.ActionManageGet)
	for deadline := time.Now().Add(5 * time.Second); len(rows) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
		rows = i345Rows(t, pool, store.ActionManageGet)
	}
	if len(rows) != 1 {
		t.Fatalf("manage get booked %d rows (%v), want 1", len(rows), i345BlockIDs(rows))
	}
	i345AssertRow(t, rows[0], f.ar.ApiKeyID, f.ar.PrincipalID, b.ID, "checkpoint", f.createdAt(t, b.ID))
}

// TestMCPReadAccess_LogFailureNeverFailsRead_Integration: an insert the
// database refuses loses the row and nothing else.
func TestMCPReadAccess_LogFailureNeverFailsRead_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	f := i345Setup(t, pool)
	b := f.upsert(t, "private", "i345-fail", "i345 fail block", "")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION i345_refuse() RETURNS trigger LANGUAGE plpgsql AS
		  $$ BEGIN RAISE EXCEPTION 'i345: access log refuses'; END $$;
		CREATE TRIGGER i345_refuse BEFORE INSERT ON context_access_log
		  FOR EACH ROW EXECUTE FUNCTION i345_refuse();`); err != nil {
		t.Fatalf("install refusing trigger: %v", err)
	}
	failedBefore, _, _ := store.BlockReadsLost()
	if isErr, text := f.get(t, f.keyCtx, getInput{ID: b.ID}); isErr {
		t.Fatalf("get with a refusing access log failed: %s", text)
	}
	i345Settle(t, pool)
	if failedAfter, _, _ := store.BlockReadsLost(); failedAfter != failedBefore+1 {
		t.Errorf("failed rows %d → %d, want +1", failedBefore, failedAfter)
	}
}

// TestMCPReadAccess_StalledLogNeverStallsReads_Integration: while
// context_access_log is locked, the inserts wait — and the reads do not. On a
// production-sized pool (20 connections) 60 gets run side by side; a writer
// that took one connection per read would hold the whole pool behind the lock
// and every further read would wait for a connection.
func TestMCPReadAccess_StalledLogNeverStallsReads_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	base := testdb.SetupTestDB(t)
	pc := base.Config().Copy()
	pc.MaxConns = 20
	pool, err := pgxpool.NewWithConfig(context.Background(), pc)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	t.Cleanup(pool.Close)
	f := i345Setup(t, pool)
	b := f.upsert(t, "private", "i345-stall", "i345 stall block", "")

	// The lock holder: its own connection, outside the pool under test.
	lockConn, err := base.Acquire(context.Background())
	if err != nil {
		t.Fatalf("acquire lock conn: %v", err)
	}
	defer lockConn.Release()
	tx, err := lockConn.Begin(context.Background())
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(context.Background(), `LOCK TABLE context_access_log IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatalf("lock: %v", err)
	}

	const reads = 60
	start := time.Now()
	var wg sync.WaitGroup
	errs := make(chan string, reads)
	for i := 0; i < reads; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(f.keyCtx, 3*time.Second)
			defer cancel()
			r, _, err := mcpGetHandler(f.cfg)(ctx, nil, getInput{ID: b.ID})
			if err != nil || r.IsError {
				errs <- fmt.Sprintf("err %v, %s", err, mcpTextOf(r))
			}
		}()
	}
	wg.Wait()
	elapsed := time.Since(start)
	close(errs)
	for e := range errs {
		t.Errorf("read under a locked access log failed: %s", e)
	}
	if elapsed > 2*time.Second {
		t.Errorf("%d reads under a locked access log took %s, want them unaffected", reads, elapsed)
	}
	t.Logf("%d reads under a locked access log: %s", reads, elapsed)
	_ = tx.Rollback(context.Background())
	i345Settle(t, pool)
}
