//go:build integration

package handler

// Issue #483 point 1: which classifier a write path takes is held where it
// decides anything — a write whose answer is cut off. The commit of the write
// (a bracket's COMMIT, or the implicit commit of a single statement) waits on
// a deferred constraint trigger; the test cuts the connection under it — a TCP
// proxy between the handlers' pool and the database closes both sides, the
// way a broken network does — or lets its deadline pass. Every path's failure
// line and the MCP tool line must then say unknown, never rolled_back (the
// word for a write that left nothing behind): once the lock goes away, the
// held commit lands.
//
// A path whose store function is one statement or an unlabeled bracket must
// take statementOutcome (writeOutcome would say rolled_back for the cut); a
// labeled bracket takes writeOutcome, and its test also holds the commit
// label in the store (without it writeOutcome says rolled_back as well).
//
//	go test -tags=integration ./internal/handler/ -run 'TestWriteOutcomeCut|TestStatementOutcomeDeadline' -count=1 -v

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// cutHoldLock is the advisory lock a held commit waits on.
const cutHoldLock = 483483

// cutAppName marks the connections whose commits the hold trigger holds: the
// proxied pool's. The fixture's direct pool seeds, reads and holds the lock
// unhindered.
const cutAppName = "i483-cut"

// cutProxy forwards TCP connections to the database and cuts all of them on
// demand: both sides closed, no message — and until heal, a new connection
// is closed at once: the database is out of reach. That keeps the cancel
// request pgx sends on a new connection after a broken one
// (pgconn.asyncClose) from cancelling the held commit, so whether the commit
// lands does not race the test's release.
type cutProxy struct {
	ln      net.Listener
	target  string
	mu      sync.Mutex
	conns   []net.Conn
	refused bool
}

func newCutProxy(t *testing.T, target string) *cutProxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &cutProxy{ln: ln, target: target}
	go p.serve()
	t.Cleanup(func() {
		_ = ln.Close()
		p.cut()
	})
	return p
}

func (p *cutProxy) serve() {
	for {
		c, err := p.ln.Accept()
		if err != nil {
			return
		}
		p.mu.Lock()
		refused := p.refused
		p.mu.Unlock()
		if refused {
			_ = c.Close()
			continue
		}
		s, err := net.Dial("tcp", p.target)
		if err != nil {
			_ = c.Close()
			continue
		}
		p.mu.Lock()
		p.conns = append(p.conns, c, s)
		p.mu.Unlock()
		go func() { _, _ = io.Copy(s, c); _ = s.Close() }()
		go func() { _, _ = io.Copy(c, s); _ = c.Close() }()
	}
}

// cut closes every connection the proxy carries, both sides, and refuses new
// ones until heal.
func (p *cutProxy) cut() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refused = true
	for _, c := range p.conns {
		_ = c.Close()
	}
	p.conns = nil
}

// refuseNew refuses new connections until heal and leaves the open ones be.
func (p *cutProxy) refuseNew() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refused = true
}

// heal lets new connections through again.
func (p *cutProxy) heal() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refused = false
}

// cutFixture is a database with the hold trigger on the tables the write
// paths commit to (named by the trigger's argument: TG_TABLE_NAME of a
// TimescaleDB hypertable's row is its chunk's name), a pool through a cutProxy for the handlers, and the MCP
// wiring the production server passes.
type cutFixture struct {
	direct *pgxpool.Pool
	pool   *pgxpool.Pool
	proxy  *cutProxy
	reg    *blocktype.Registry
	cfg    MCPConfig
	plain  string
	// waiters: the handlers' backends that waited on a lock at the last cut.
	waiters []string
}

func newCutFixture(t *testing.T) *cutFixture {
	t.Helper()
	direct, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	pcfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	proxy := newCutProxy(t, net.JoinHostPort(pcfg.ConnConfig.Host, strconv.Itoa(int(pcfg.ConnConfig.Port))))
	host, port, _ := net.SplitHostPort(proxy.ln.Addr().String())
	p, _ := strconv.ParseUint(port, 10, 16)
	pcfg.ConnConfig.Host, pcfg.ConnConfig.Port, pcfg.ConnConfig.Fallbacks = host, uint16(p), nil
	pcfg.ConnConfig.RuntimeParams["application_name"] = cutAppName
	pool, err := pgxpool.NewWithConfig(ctx, pcfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	ddl := []string{
		`CREATE TABLE i483_hold (tbl text PRIMARY KEY)`,
		`CREATE FUNCTION i483_hold() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF current_setting('application_name') = 'i483-cut'
		      AND EXISTS (SELECT 1 FROM i483_hold WHERE tbl = TG_ARGV[0]) THEN
		     PERFORM pg_advisory_xact_lock(483483);
		   END IF;
		   RETURN NULL;
		 END $$`,
	}
	for _, tbl := range []string{"context_blocks", "context_blobs", "context_pending_writes", "context_dream_links",
		"context_tenants", "context_tenant_scopes", "context_tenant_grants", "context_block_grants", "context_tenant_quota",
		"context_structural_links", "context_backends"} {
		ddl = append(ddl, fmt.Sprintf(`CREATE CONSTRAINT TRIGGER i483_hold AFTER INSERT OR UPDATE OR DELETE ON %[1]s
			DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i483_hold('%[1]s')`, tbl))
	}
	for _, q := range ddl {
		if _, err := direct.Exec(ctx, q); err != nil {
			t.Fatalf("hold fixture: %v", err)
		}
	}

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, direct)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	cfgStore := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic, BlobStageMaxBytes: 1 << 20},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute, BatchMaxItems: 100},
	}}
	return &cutFixture{
		direct: direct, pool: pool, proxy: proxy, reg: reg, plain: abortKey(t, direct),
		cfg: MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg},
	}
}

// cutDuring runs call while the commit of a write to table waits on the hold
// lock, cuts the handlers' connections once it waits, waits for call to
// return, lets the held commit go on, and heals the proxy once the held
// backend is gone.
func (f *cutFixture) cutDuring(t *testing.T, table string, call func()) {
	t.Helper()
	ctx := context.Background()
	// The work of an earlier call still running on the handlers' pool would
	// wait on the hold too and pass for this call's commit.
	f.quiesce(t)
	if _, err := f.direct.Exec(ctx, `INSERT INTO i483_hold VALUES ($1) ON CONFLICT DO NOTHING`, table); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := f.direct.Exec(ctx, `DELETE FROM i483_hold WHERE tbl = $1`, table); err != nil {
			t.Errorf("drop the hold on %s: %v", table, err)
		}
	})
	release := lockedTx(t, f.direct, `SELECT pg_advisory_xact_lock($1)`, cutHoldLock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		call()
	}()
	waitLockWaiters(t, f.direct, 1)
	f.waiters = f.activity(t, `wait_event_type = 'Lock'`)
	f.proxy.cut()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("the call did not return after its connection was cut")
	}
	release()
	f.quiesce(t)
	f.proxy.heal()
}

// waitLanded polls query (a count) until it is positive: the held commit went
// on after its connection was cut, so unknown was the honest answer.
func (f *cutFixture) waitLanded(t *testing.T, query string, args ...any) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		var n int
		if err := f.direct.QueryRow(context.Background(), query, args...).Scan(&n); err != nil {
			t.Fatalf("landed query: %v", err)
		}
		if n > 0 {
			return
		}
	}
	t.Errorf("the held commit never landed (%s): the cut write was not committed after all (waiters at the cut: %v; backends now: %v)",
		query, f.waiters, f.activity(t, `true`))
}

// activity lists the handlers' backends that match where: their state, wait
// event and statement.
func (f *cutFixture) activity(t *testing.T, where string) []string {
	t.Helper()
	rows, err := f.direct.Query(context.Background(), `SELECT state || ' ' || coalesce(wait_event, '') || ' ' || left(query, 80)
		FROM pg_stat_activity WHERE datname = current_database() AND application_name = 'i483-cut' AND `+where)
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	out, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatalf("read pg_stat_activity: %v", err)
	}
	return out
}

// quiesce waits until no backend of the handlers' pool runs a statement.
func (f *cutFixture) quiesce(t *testing.T) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if len(f.activity(t, `state <> 'idle'`)) == 0 {
			return
		}
	}
	t.Fatalf("the handlers' pool still runs statements: %v", f.activity(t, `state <> 'idle'`))
}

// mcp serves the production MCP handler on the proxied pool (Auth included
// when ar is nil).
func (f *cutFixture) mcp(t *testing.T, ar *auth.AuthResult) (string, <-chan struct{}) {
	t.Helper()
	lf := &mcpLineFixture{pool: f.pool, reg: f.reg, cfg: f.cfg, plain: f.plain}
	return lf.serve(t, ar, nil)
}

// plainOf is the X-Context-Key of a call: none when ar is injected.
func (f *cutFixture) plainOf(ar *auth.AuthResult) string {
	if ar != nil {
		return ""
	}
	return f.plain
}

// toolAnswer is the text and isError of a tools/call answer.
func toolAnswer(t *testing.T, raw []byte) (string, bool) {
	t.Helper()
	var rpc struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &rpc); err != nil || rpc.Error != nil {
		t.Fatalf("tools/call answer %s (%v)", raw, err)
	}
	if len(rpc.Result.Content) == 0 {
		return "", rpc.Result.IsError
	}
	return rpc.Result.Content[0].Text, rpc.Result.IsError
}

// cutTool makes one tools/call of tool whose write to table is cut during
// its commit, and returns the answer and the capture.
func (f *cutFixture) cutTool(t *testing.T, ar *auth.AuthResult, table, tool string, args map[string]any) (string, bool, *rejectLogCapture) {
	t.Helper()
	logs := installRejectLogCapture(t)
	url, done := f.mcp(t, ar)
	req := mcpToolRequest(t, context.Background(), url, f.plainOf(ar), tool, args)
	var raw []byte
	var callErr error
	f.cutDuring(t, table, func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			callErr = err
			return
		}
		defer resp.Body.Close()
		raw, callErr = io.ReadAll(resp.Body)
	})
	if callErr != nil {
		t.Fatalf("%s: %v", tool, callErr)
	}
	waitDone(t, done)
	text, isErr := toolAnswer(t, raw)
	return text, isErr, logs
}

// cutREST posts body to h behind the production request chain (Auth on the
// proxied pool, plain as the key) while the commit of its write to table is
// held and cut, and checks the verdict: a 5xx (ingest answers per chunk),
// and the failure line msg says unknown.
func (f *cutFixture) cutREST(t *testing.T, h http.HandlerFunc, plain, table, path, body, msg string) {
	t.Helper()
	logs := installRejectLogCapture(t)
	srv, done := abortRoute(t, f.pool, h)
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Context-Key", plain)
	var status int
	f.cutDuring(t, table, func() {
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		status = resp.StatusCode
		_ = resp.Body.Close()
	})
	waitDone(t, done)
	if status < http.StatusInternalServerError && !strings.HasPrefix(path, "/api/ingest") {
		t.Errorf("%s answered %d, want a 5xx for the cut write", path, status)
	}
	wantCutUnknown(t, logs, msg, "", false)
}

// callOK makes one tools/call on the proxied pool without a cut and returns
// its text; an error answer fails unless the call stages (a staged card is an
// uncoded error by design).
func (f *cutFixture) callOK(t *testing.T, ar *auth.AuthResult, tool string, args map[string]any) string {
	t.Helper()
	installRejectLogCapture(t)
	url, done := f.mcp(t, ar)
	text, isErr := callTool(t, url, f.plainOf(ar), tool, args)
	waitDone(t, done)
	if isErr && !strings.Contains(text, "payload_hash: ") {
		t.Fatalf("%s answered an error: %s", tool, text)
	}
	return text
}

var cutHashRE = regexp.MustCompile(`payload_hash: ([0-9a-f]{64})`)

// stage makes a staged call as ar and returns the card's payload hash.
func (f *cutFixture) stage(t *testing.T, ar *auth.AuthResult, tool string, args map[string]any) string {
	t.Helper()
	m := cutHashRE.FindStringSubmatch(f.callOK(t, ar, tool, args))
	if m == nil {
		t.Fatalf("%s did not stage", tool)
	}
	return m[1]
}

// wantCutUnknown checks the verdict of every cut: the failure line msg (empty:
// the path has none) is one ERROR line with write_outcome=unknown, and the
// tool line (tool empty: a REST path) says unknown, staged as given.
func wantCutUnknown(t *testing.T, logs *rejectLogCapture, msg, tool string, staged bool) {
	t.Helper()
	if msg != "" {
		recs := logs.withMsg(msg)
		if len(recs) != 1 || recs[0].level != slog.LevelError || recs[0].attrs["write_outcome"] != writeUnknown {
			t.Errorf("%q = %+v, want one ERROR line with write_outcome=unknown (all: %+v)", msg, recs, logs.all())
		}
	}
	if tool == "" {
		return
	}
	line := toolLine(t, logs)
	if line.attrs["tool"] != tool || line.attrs["write_outcome"] != writeUnknown {
		t.Errorf("tool line = %+v, want tool=%s write_outcome=unknown", line.attrs, tool)
	}
	if got := line.attrs["staged"] == true; got != staged {
		t.Errorf("tool line = %+v, want staged=%v", line.attrs, staged)
	}
}

// confirmWritesAR is the fixture key with confirm_writes.
func (f *cutFixture) confirmWritesAR(t *testing.T) *auth.AuthResult {
	t.Helper()
	ar, err := auth.AuthenticateRaw(context.Background(), f.direct, f.plain)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate fixture key: %v", err)
	}
	ar.ConfirmWrites = true
	return ar
}

func (f *cutFixture) seedBlock(t *testing.T, title string) string {
	t.Helper()
	b, err := store.UpsertBlock(context.Background(), f.direct, "i483", title, "seeded: "+title, nil, nil, "private", false, store.SensitivityWrite{}, "")
	if err != nil {
		t.Fatalf("seed %q: %v", title, err)
	}
	return b.ID
}

func (f *cutFixture) flag(t *testing.T, id string) {
	t.Helper()
	if _, err := f.direct.Exec(context.Background(), `UPDATE context_blocks SET guard_status = 'needs_review' WHERE id = $1::uuid`, id); err != nil {
		t.Fatalf("flag %s: %v", id, err)
	}
}

func (f *cutFixture) seedBlob(t *testing.T, title string) string {
	t.Helper()
	b, err := store.UpsertBlob(context.Background(), f.direct, "i483", title, "probe.txt", "text/plain", "private", []byte("payload"), nil, nil, "")
	if err != nil {
		t.Fatalf("seed blob %q: %v", title, err)
	}
	return b.ID
}

func blobArgs(title string) map[string]any {
	return map[string]any{"category": "i483", "title": title, "filename": "probe.txt", "mime_type": "text/plain", "text": "blob payload"}
}

const landedByTitle = `SELECT count(*)::int FROM context_blocks WHERE title = $1`

// TestWriteOutcomeCutStatementPaths — the paths whose write is one statement
// or an unlabeled bracket (statementOutcome): a cut commit is unknown on the
// failure line and on the tool line. A swap to writeOutcome says rolled_back.
func TestWriteOutcomeCutStatementPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ar := f.confirmWritesAR(t)

	t.Run("blob_store", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blobs", "blob_store", blobArgs("i483 cut blob"))
		wantCutUnknown(t, logs, "blob-store: upsert error", "blob_store", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blobs WHERE title = 'i483 cut blob'`)
	})
	t.Run("blob_link", func(t *testing.T) {
		blob, block := f.seedBlob(t, "i483 cut link blob"), f.seedBlock(t, "i483 cut link block")
		_, _, logs := f.cutTool(t, nil, "context_blobs", "blob_link", map[string]any{"id": blob, "context_block_id": block})
		wantCutUnknown(t, logs, "mcp blob_link: update error", "blob_link", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blobs WHERE id = $1::uuid AND context_block_id = $2::uuid`, blob, block)
	})
	var issueID string
	t.Run("issue_create", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blocks", "issue_create", map[string]any{"title": "i483 cut issue"})
		wantCutUnknown(t, logs, "mcp issue_create error", "issue_create", false)
		// An issue's title carries its "#L<seq>" prefix.
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE title LIKE '%i483 cut issue'`)
		if err := f.direct.QueryRow(context.Background(),
			`SELECT id::text FROM context_blocks WHERE title LIKE '%i483 cut issue'`).Scan(&issueID); err != nil {
			t.Fatalf("read the issue: %v", err)
		}
	})
	t.Run("issue_comment", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blocks", "issue_comment", map[string]any{"issue_id": issueID, "content": "i483 cut comment"})
		wantCutUnknown(t, logs, "mcp issue_comment error", "issue_comment", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE content = 'i483 cut comment'`)
	})
	t.Run("issue_state", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blocks", "issue_state", map[string]any{"issue_id": issueID, "status": "in-progress"})
		wantCutUnknown(t, logs, "mcp issue_state error", "issue_state", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND workflow_status = 'in-progress'`, issueID)
	})

	landedStage := `SELECT count(*)::int FROM context_pending_writes WHERE op = $1`
	t.Run("staged store", func(t *testing.T) {
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "store", storeArgs("i483 cut staged store", "staged"))
		wantCutUnknown(t, logs, "mcp: stage pending write failed", "store", true)
		f.waitLanded(t, landedStage, "store")
	})
	t.Run("staged update", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut staged update")
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "update", map[string]any{"id": id, "content": "staged change"})
		wantCutUnknown(t, logs, "mcp: stage pending update failed", "update", true)
		f.waitLanded(t, landedStage, "update")
	})
	t.Run("staged archive", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut staged archive")
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "archive", map[string]any{"id": id})
		wantCutUnknown(t, logs, "mcp: stage pending archive failed", "archive", true)
		f.waitLanded(t, landedStage, store.OpArchive)
	})
	t.Run("staged blob_store", func(t *testing.T) {
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "blob_store", blobArgs("i483 cut staged blob"))
		wantCutUnknown(t, logs, "mcp: stage pending blob write failed", "blob_store", true)
		f.waitLanded(t, landedStage, store.OpBlobStore)
	})
	t.Run("staged blob_link", func(t *testing.T) {
		blob, block := f.seedBlob(t, "i483 cut staged link blob"), f.seedBlock(t, "i483 cut staged link block")
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "blob_link", map[string]any{"id": blob, "context_block_id": block})
		wantCutUnknown(t, logs, "mcp: stage pending blob link failed", "blob_link", true)
		f.waitLanded(t, landedStage, store.OpBlobLink)
	})

	t.Run("confirmed blob_store", func(t *testing.T) {
		hash := f.stage(t, ar, "blob_store", blobArgs("i483 cut confirmed blob"))
		_, _, logs := f.cutTool(t, ar, "context_blobs", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed blob write execute failed", "confirm", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blobs WHERE title = 'i483 cut confirmed blob'`)
	})
	t.Run("confirmed blob_link", func(t *testing.T) {
		blob, block := f.seedBlob(t, "i483 cut confirmed link blob"), f.seedBlock(t, "i483 cut confirmed link block")
		hash := f.stage(t, ar, "blob_link", map[string]any{"id": blob, "context_block_id": block})
		_, _, logs := f.cutTool(t, ar, "context_blobs", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed blob link execute failed", "confirm", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blobs WHERE id = $1::uuid AND context_block_id = $2::uuid`, blob, block)
	})
}

// TestWriteOutcomeCutLabeledPaths — the paths whose write is a labeled
// bracket (writeOutcome): a cut COMMIT is unknown. Without the commit label in
// the store, or with a classifier that ignores it, the line says rolled_back.
func TestWriteOutcomeCutLabeledPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ar := f.confirmWritesAR(t)

	t.Run("store", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blocks", "store", storeArgs("i483 cut store", "cut"))
		wantCutUnknown(t, logs, "mcp: store error", "store", false)
		f.waitLanded(t, landedByTitle, "i483 cut store")
	})
	t.Run("update", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut update")
		_, _, logs := f.cutTool(t, nil, "context_blocks", "update", map[string]any{"id": id, "content": "i483 cut update changed"})
		wantCutUnknown(t, logs, "mcp: update error", "update", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE content = 'i483 cut update changed'`)
	})
	t.Run("archive", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut archive")
		_, _, logs := f.cutTool(t, nil, "context_blocks", "archive", map[string]any{"id": id})
		wantCutUnknown(t, logs, "mcp: archive error", "archive", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND is_archived`, id)
	})
	t.Run("guard_resolve", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut guard")
		f.flag(t, id)
		_, _, logs := f.cutTool(t, nil, "context_blocks", "guard_resolve", map[string]any{"ids": []string{id}, "resolution": "keep"})
		wantCutUnknown(t, logs, "mcp: guard_resolve error", "guard_resolve", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND guard_status IS DISTINCT FROM 'needs_review'`, id)
	})
	t.Run("store_batch", func(t *testing.T) {
		_, _, logs := f.cutTool(t, nil, "context_blocks", "store_batch", map[string]any{"items": []map[string]any{
			storeArgs("i483 cut batch one", "one"), storeArgs("i483 cut batch two", "two")}})
		wantCutUnknown(t, logs, "store batch: upsert error", "store_batch", false)
		f.waitLanded(t, landedByTitle, "i483 cut batch two")
	})
	t.Run("staged store_batch", func(t *testing.T) {
		_, _, logs := f.cutTool(t, ar, "context_pending_writes", "store_batch", map[string]any{"items": []map[string]any{
			storeArgs("i483 cut staged batch one", "one")}})
		wantCutUnknown(t, logs, "mcp: stage pending batch failed", "store_batch", true)
		f.waitLanded(t, `SELECT count(*)::int FROM context_pending_writes WHERE op = $1`, store.OpStoreBatch)
	})

	t.Run("confirmed store", func(t *testing.T) {
		hash := f.stage(t, ar, "store", storeArgs("i483 cut confirmed store", "confirmed"))
		_, _, logs := f.cutTool(t, ar, "context_blocks", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed write execute failed", "confirm", false)
		f.waitLanded(t, landedByTitle, "i483 cut confirmed store")
	})
	t.Run("confirmed update", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut confirmed update")
		hash := f.stage(t, ar, "update", map[string]any{"id": id, "content": "i483 cut confirmed update changed"})
		_, _, logs := f.cutTool(t, ar, "context_blocks", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed update execute failed", "confirm", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE content = 'i483 cut confirmed update changed'`)
	})
	t.Run("confirmed archive", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut confirmed archive")
		hash := f.stage(t, ar, "archive", map[string]any{"id": id})
		_, _, logs := f.cutTool(t, ar, "context_blocks", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed archive execute failed", "confirm", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND is_archived`, id)
	})
	t.Run("confirmed store_batch", func(t *testing.T) {
		hash := f.stage(t, ar, "store_batch", map[string]any{"items": []map[string]any{storeArgs("i483 cut confirmed batch", "one")}})
		_, _, logs := f.cutTool(t, ar, "context_blocks", "confirm", map[string]any{"payload_hash": hash})
		wantCutUnknown(t, logs, "confirm: confirmed batch execute failed", "confirm", false)
		f.waitLanded(t, landedByTitle, "i483 cut confirmed batch")
	})

	// The REST-only paths: the client waits; the cut is the server's.
	rest := func(t *testing.T, h http.HandlerFunc, path, body, msg string) {
		t.Helper()
		f.cutREST(t, h, f.plain, "context_blocks", path, body, msg)
	}
	t.Run("REST manage update", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut rest update")
		rest(t, NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.reg).HandleManage, "/api/manage",
			fmt.Sprintf(`{"action":"update","id":%q,"data":{"content":"i483 cut rest update changed"}}`, id), "manage: block write error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE content = 'i483 cut rest update changed'`)
	})
	t.Run("REST guard-resolve with ids", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut rest guard")
		f.flag(t, id)
		rest(t, NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.reg).HandleManage, "/api/manage",
			fmt.Sprintf(`{"action":"guard-resolve","data":{"resolution":"keep","ids":[%q]}}`, id), "manage: guard-resolve batch error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND guard_status IS DISTINCT FROM 'needs_review'`, id)
	})
	t.Run("REST ingest in block mode", func(t *testing.T) {
		raw, _ := json.Marshal(map[string]any{"source": "i483", "chunks": []map[string]any{
			{"category": "i483", "title": "i483 cut ingest", "content": "cut during the commit"}}})
		rest(t, NewIngestHandler(f.pool, nil, f.reg).HandleIngest, "/api/ingest", string(raw), "ingest: upsert block error")
		f.waitLanded(t, landedByTitle, "i483 cut ingest")
	})
}

// cutDeadline is a context whose deadline passes when the test says so — the
// moment its COMMIT waits. pgconn turns its Err (context.DeadlineExceeded)
// into the error a real deadline produces ("timeout: context deadline
// exceeded", pgconn normalizeTimeoutError).
type cutDeadline struct {
	context.Context
	done chan struct{}
	once sync.Once
}

func newCutDeadline() *cutDeadline {
	return &cutDeadline{Context: context.Background(), done: make(chan struct{})}
}

func (c *cutDeadline) Done() <-chan struct{} { return c.done }
func (c *cutDeadline) Err() error {
	select {
	case <-c.done:
		return context.DeadlineExceeded
	default:
		return nil
	}
}
func (c *cutDeadline) pass() { c.once.Do(func() { close(c.done) }) }

// TestStatementOutcomeDeadlineAtCommit — Probe B of the review of #482 as a
// test: a commit that waits when its deadline passes ends with pgconn's
// timeout error — context.DeadlineExceeded inside, not SafeToRetry — and the
// database commits it anyway once it may go on. statementOutcome says unknown
// for each form; writeOutcome says unknown only for the labeled bracket, and
// rolled_back for the other two: the reason a write without a commit label
// takes statementOutcome.
func TestStatementOutcomeDeadlineAtCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	// The writes run on the proxied pool: pgx sends a cancel request on a new
	// connection after the deadline's read error, and the proxy refuses it
	// until the held commit went on (cutProxy), so whether it lands does not
	// race the release.
	f := newCutFixture(t)
	pool, direct := f.pool, f.direct
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE i483_deadline (k text NOT NULL)`,
		`CREATE FUNCTION i483_deadline_hold() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN PERFORM pg_advisory_xact_lock(483484); RETURN NULL; END $$`,
		`CREATE CONSTRAINT TRIGGER i483_deadline_hold AFTER INSERT ON i483_deadline
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i483_deadline_hold()`,
	} {
		if _, err := direct.Exec(ctx, q); err != nil {
			t.Fatalf("fixture DDL: %v", err)
		}
	}
	insert := func(k string) func(pgx.Tx) error {
		return func(tx pgx.Tx) error {
			_, err := tx.Exec(context.Background(), `INSERT INTO i483_deadline VALUES ($1)`, k)
			return err
		}
	}
	landed := func(t *testing.T, k string) {
		t.Helper()
		for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
			var n int
			if err := direct.QueryRow(ctx, `SELECT count(*)::int FROM i483_deadline WHERE k = $1`, k).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n == 1 {
				return
			}
		}
		t.Errorf("%s: the commit cut by its deadline never landed", k)
	}
	// run makes write wait at its commit, passes its deadline once it waits,
	// and returns the error.
	run := func(t *testing.T, write func(ctx context.Context) error) error {
		t.Helper()
		release := lockedTx(t, direct, `SELECT pg_advisory_xact_lock($1)`, 483484)
		dctx := newCutDeadline()
		errc := make(chan error, 1)
		go func() { errc <- write(dctx) }()
		waitLockWaiters(t, direct, 1)
		f.proxy.refuseNew()
		dctx.pass()
		err := <-errc
		release()
		f.quiesce(t)
		f.proxy.heal()
		return err
	}
	check := func(t *testing.T, err error, wantWrite string) {
		t.Helper()
		if !errors.Is(err, context.DeadlineExceeded) || !strings.Contains(err.Error(), "timeout: context deadline exceeded") || pgconn.SafeToRetry(err) {
			t.Fatalf("error = %v (SafeToRetry %v), want pgconn's timeout on the deadline after sending", err, pgconn.SafeToRetry(err))
		}
		if got := statementOutcome(err); got != writeUnknown {
			t.Errorf("statementOutcome = %q, want unknown", got)
		}
		if got := writeOutcome(err); got != wantWrite {
			t.Errorf("writeOutcome = %q, want %q", got, wantWrite)
		}
	}

	t.Run("an unlabeled bracket", func(t *testing.T) {
		err := run(t, func(ctx context.Context) error { return pgxdb.Write(ctx, pool, pgxdb.Stages{}, insert("unlabeled")) })
		check(t, err, writeRolledBack)
		landed(t, "unlabeled")
	})
	t.Run("a single statement", func(t *testing.T) {
		err := run(t, func(ctx context.Context) error {
			_, err := pool.Exec(ctx, `INSERT INTO i483_deadline VALUES ('statement')`)
			return err
		})
		check(t, err, writeRolledBack)
		landed(t, "statement")
	})
	t.Run("a labeled bracket", func(t *testing.T) {
		err := run(t, func(ctx context.Context) error { return pgxdb.Write(ctx, pool, pgxdb.At("i483"), insert("labeled")) })
		check(t, err, writeUnknown)
		landed(t, "labeled")
	})
	t.Run("a real deadline", func(t *testing.T) {
		// The deadline itself, not a stand-in: the COMMIT is sent at once and
		// waits on the lock until after Commit returned, so the deadline can
		// only pass while it waits (a deadline passed before sending would
		// fail the SafeToRetry check above, not pass silently).
		release := lockedTx(t, direct, `SELECT pg_advisory_xact_lock($1)`, 483484)
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if err := insert("real")(tx); err != nil {
			t.Fatal(err)
		}
		dctx, cancel := context.WithTimeout(ctx, time.Second)
		defer cancel()
		f.proxy.refuseNew()
		err = tx.Commit(dctx)
		release()
		f.quiesce(t)
		f.proxy.heal()
		check(t, err, writeRolledBack)
		landed(t, "real")
	})
}

// TestWriteOutcomeBrokenCommit — a connection that breaks while its COMMIT
// waits for the answer: pgx v5 reports it as pgconn.ErrConnClosed, the error
// of a call on a connection it had already closed, and marks it safe to
// retry (MultiResultReader.NextResult drops the read error). The database
// commits the held transaction once it may go on, so both classifiers must
// say unknown — read as "never sent" it would be rolled_back.
func TestWriteOutcomeBrokenCommit(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ctx := context.Background()
	for _, q := range []string{
		`CREATE TABLE i483_broken (k text NOT NULL)`,
		`CREATE CONSTRAINT TRIGGER i483_hold AFTER INSERT ON i483_broken
		 DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION i483_hold('i483_broken')`,
	} {
		if _, err := f.direct.Exec(ctx, q); err != nil {
			t.Fatalf("fixture DDL: %v", err)
		}
	}
	for _, tc := range []struct {
		name    string
		stages  pgxdb.Stages
		outcome func(error) string
	}{
		{"an unlabeled bracket: statementOutcome", pgxdb.Stages{}, statementOutcome},
		{"a labeled bracket: writeOutcome", pgxdb.At("i483"), writeOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var err error
			f.cutDuring(t, "i483_broken", func() {
				err = pgxdb.Write(ctx, f.pool, tc.stages, func(tx pgx.Tx) error {
					_, e := tx.Exec(ctx, `INSERT INTO i483_broken VALUES ($1)`, tc.name)
					return e
				})
			})
			if !errors.Is(err, pgconn.ErrConnClosed) || !pgconn.SafeToRetry(err) {
				t.Fatalf("error = %v (SafeToRetry %v), want pgx's ErrConnClosed marked safe to retry", err, pgconn.SafeToRetry(err))
			}
			if got := tc.outcome(err); got != writeUnknown {
				t.Errorf("outcome = %q, want unknown", got)
			}
			f.waitLanded(t, `SELECT count(*)::int FROM i483_broken WHERE k = $1`, tc.name)
		})
	}
}

// TestWriteOutcomeCutRemainingPaths — the paths #482 left without
// write_outcome (issue #483 point 8): their failure line names it, and a cut
// commit says unknown. manage delete and dream-link-resolve are labeled
// brackets; guard-resolve of one id, the chunk ingest, the blob delete and
// the chat's staged writes are one statement; the issue writes run an
// unlabeled bracket; a purge counts what earlier batches removed.
func TestWriteOutcomeCutRemainingPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ctx := context.Background()
	manage := NewManageHandler(f.pool, nil, nil, nil, nil, nil, nil, f.reg).HandleManage

	t.Run("manage delete", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut manage delete")
		f.cutREST(t, manage, f.plain, "context_blocks", "/api/manage",
			fmt.Sprintf(`{"action":"delete","id":%q}`, id), "manage: block write error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND is_archived`, id)
	})
	t.Run("guard-resolve of one id", func(t *testing.T) {
		id := f.seedBlock(t, "i483 cut guard one")
		f.flag(t, id)
		f.cutREST(t, manage, f.plain, "context_blocks", "/api/manage",
			fmt.Sprintf(`{"action":"guard-resolve","id":%q,"data":{"resolution":"keep"}}`, id), "manage: block write error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND guard_status IS DISTINCT FROM 'needs_review'`, id)
	})
	t.Run("dream-link-resolve", func(t *testing.T) {
		src, dst := f.seedBlock(t, "i483 cut link source"), f.seedBlock(t, "i483 cut link target")
		if _, err := f.direct.Exec(ctx, `INSERT INTO context_dream_links
			(source_block_id, target_block_id, relationship, confidence, raw_confidence, scope)
			VALUES ($1::uuid, $2::uuid, 'topical', 0.8, 0.8, 'private')`, src, dst); err != nil {
			t.Fatalf("seed link: %v", err)
		}
		f.cutREST(t, manage, f.plain, "context_dream_links", "/api/manage",
			fmt.Sprintf(`{"action":"dream-link-resolve","data":{"source_id":%q,"target_id":%q,"relationship":"topical","resolution":"confirm"}}`, src, dst),
			"manage: dream-link-resolve error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_dream_links WHERE source_block_id = $1::uuid AND pinned`, src)
	})
	t.Run("purge", func(t *testing.T) {
		for i := range 3 {
			if _, err := store.UpsertBlock(ctx, f.direct, "i483purge", fmt.Sprintf("i483 cut purge %d", i), "purged", nil, nil, "private", false, store.SensitivityWrite{}, ""); err != nil {
				t.Fatal(err)
			}
		}
		k, admin, err := store.CreateApiKey(ctx, f.direct, "i483-cut-admin", "private", nil, "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.direct.Exec(ctx, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, k.ID); err != nil {
			t.Fatal(err)
		}
		f.cutREST(t, manage, admin, "context_blocks", "/api/manage",
			`{"action":"purge","data":{"scope":"private","category":"i483purge","confirm_blocks":3}}`, "manage: purge failed")
		f.waitLanded(t, `SELECT (count(*) = 0)::int FROM context_blocks WHERE category = 'i483purge'`)
	})
	t.Run("ingest in chunk mode", func(t *testing.T) {
		source := seedSource(t, f.direct, "/i483/cut.md", "private")
		raw, _ := json.Marshal(map[string]any{"source": "i483", "chunks": []map[string]any{{
			"category": "i483", "title": "i483 cut chunk", "content": "cut during the commit", "source_id": source, "chunk_index": 0}}})
		f.cutREST(t, NewIngestHandler(f.pool, nil, f.reg).HandleIngest, f.plain, "context_blocks", "/api/ingest", string(raw), "ingest: insert chunk error")
		f.waitLanded(t, landedByTitle, "i483 cut chunk")
	})
	t.Run("blob delete", func(t *testing.T) {
		id := f.seedBlob(t, "i483 cut blob delete")
		f.cutREST(t, NewBlobHandler(f.pool, f.cfg.Cfg).HandleBlobManage, f.plain, "context_blobs", "/api/blob/manage",
			fmt.Sprintf(`{"action":"delete","id":%q}`, id), "blob-manage: delete error")
		f.waitLanded(t, `SELECT (count(*) = 0)::int FROM context_blobs WHERE id = $1::uuid`, id)
	})

	var issueID string
	t.Run("manage issue-create", func(t *testing.T) {
		f.cutREST(t, manage, f.plain, "context_blocks", "/api/manage",
			`{"action":"issue-create","data":{"title":"i483 cut rest issue"}}`, "issue-create error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE title LIKE '%i483 cut rest issue'`)
		if err := f.direct.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title LIKE '%i483 cut rest issue'`).Scan(&issueID); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("manage issue-update", func(t *testing.T) {
		f.cutREST(t, manage, f.plain, "context_blocks", "/api/manage",
			fmt.Sprintf(`{"action":"issue-update","id":%q,"data":{"status":"in-progress"}}`, issueID), "issue-update error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE id = $1::uuid AND workflow_status = 'in-progress'`, issueID)
	})
	t.Run("manage issue-comment-create", func(t *testing.T) {
		f.cutREST(t, manage, f.plain, "context_blocks", "/api/manage",
			fmt.Sprintf(`{"action":"issue-comment-create","data":{"parent_id":%q,"content":"i483 cut rest comment"}}`, issueID), "issue-comment-create error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE content = 'i483 cut rest comment'`)
	})
	var linkTarget string
	t.Run("manage issue-link-create", func(t *testing.T) {
		ar, err := auth.AuthenticateRaw(ctx, f.direct, f.plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate fixture key: %v", err)
		}
		_, resp := callManage(t, NewManageHandler(f.direct, nil, nil, nil, nil, nil, nil, f.reg), ar, "issue-create", "",
			map[string]any{"title": "i483 cut link target"})
		issue, _ := resp["issue"].(map[string]any)
		linkTarget, _ = issue["id"].(string)
		if linkTarget == "" {
			t.Fatalf("seed link target: %v", resp)
		}
		f.cutREST(t, manage, f.plain, "context_structural_links", "/api/manage",
			fmt.Sprintf(`{"action":"issue-link-create","data":{"source_id":%q,"target_id":%q,"link_class":"references"}}`, issueID, linkTarget),
			"issue-link-create error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_structural_links WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`, issueID, linkTarget)
	})
	t.Run("manage issue-link-delete", func(t *testing.T) {
		f.cutREST(t, manage, f.plain, "context_structural_links", "/api/manage",
			fmt.Sprintf(`{"action":"issue-link-delete","data":{"source_id":%q,"target_id":%q,"link_class":"references"}}`, issueID, linkTarget),
			"issue-link-delete error")
		f.waitLanded(t, `SELECT (count(*) = 0)::int FROM context_structural_links WHERE source_block_id = $1::uuid AND target_block_id = $2::uuid`, issueID, linkTarget)
	})
	t.Run("project issue create", func(t *testing.T) {
		var projectID string
		if err := f.direct.QueryRow(ctx, `INSERT INTO context_projects (tenant_id, scope, identity) VALUES ($1::uuid, 'private', 'i483-cut') RETURNING id::text`,
			store.DefaultTenantID).Scan(&projectID); err != nil {
			t.Fatalf("seed project: %v", err)
		}
		r := chi.NewRouter()
		MountProjectIssues(r, NewProjectIssuesHandler(f.pool, f.reg))
		f.cutREST(t, r.ServeHTTP, f.plain, "context_blocks", "/api/project/"+projectID+"/issues",
			`{"title":"i483 cut project issue"}`, "issue-create error")
		f.waitLanded(t, `SELECT count(*)::int FROM context_blocks WHERE title LIKE '%i483 cut project issue'`)
	})

	t.Run("chat's staged store and update", func(t *testing.T) {
		ar, err := auth.AuthenticateRaw(ctx, f.direct, f.plain)
		if err != nil || !ar.IsValid {
			t.Fatalf("authenticate fixture key: %v", err)
		}
		cctx := context.WithValue(context.WithValue(ctx, authResultKey, ar), requestIDKey, "i483-chat-stage")
		runner := &chatStageRunner{pool: f.pool, cfg: f.cfg.Cfg, blocktypes: f.reg}
		logs := installRejectLogCapture(t)
		f.cutDuring(t, "context_pending_writes", func() {
			_, _, _ = runner.StageWrite(cctx, "i483", "i483 cut chat stage", "staged by the chat", nil, nil)
		})
		wantCutUnknown(t, logs, "chat: stage pending write failed", "", false)
		id := f.seedBlock(t, "i483 cut chat update")
		content := "changed by the chat"
		logs = installRejectLogCapture(t)
		f.cutDuring(t, "context_pending_writes", func() {
			_, _, _ = runner.StageUpdate(cctx, id, nil, nil, &content, nil, nil)
		})
		wantCutUnknown(t, logs, "chat: stage pending update failed", "", false)
		f.waitLanded(t, `SELECT count(*)::int FROM context_pending_writes WHERE origin = 'chat' AND op = 'update'`)
	})
}

// TestWriteOutcomeCutAdminPaths — the admin writes of point 9: those whose
// store function is one statement or an unlabeled bracket take
// statementOutcome, tenant-create and scope-create a labeled bracket
// (writeOutcome), backend-create the unlabeled commit of answeredTx
// (statementOutcome in writeBackendTxFailure); a cut commit says unknown on
// every failure line — the constant rolled_back, or writeOutcome on an
// unlabeled commit, would not.
func TestWriteOutcomeCutAdminPaths(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newCutFixture(t)
	ctx := context.Background()
	k, adminPlain, err := store.CreateApiKey(ctx, f.direct, "i483-cut-admin", "private", nil, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.direct.Exec(ctx, `UPDATE context_api_keys SET is_admin = true WHERE id = $1::uuid`, k.ID); err != nil {
		t.Fatal(err)
	}
	admin, err := auth.AuthenticateRaw(ctx, f.direct, adminPlain)
	if err != nil || !admin.IsValid {
		t.Fatalf("authenticate admin: %v", err)
	}
	seedH := mcHandler(t, f.direct, false, false)
	a := auMakeTenant(ctx, t, f.direct, seedH, admin, "i483-cut-a")
	b := auMakeTenant(ctx, t, f.direct, seedH, admin, "i483-cut-b")
	if _, err := f.direct.Exec(ctx, `INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, 'true')`,
		store.AllowCrossTenantBlockGrantKey, admin.TenantID); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	granted, revoked := f.seedBlock(t, "i483 cut granted"), f.seedBlock(t, "i483 cut revoked")
	if status, resp := kaManage(t, seedH, admin, "i483-seed", map[string]any{"action": "block-grant-create",
		"data": map[string]any{"block_id": revoked, "grantee_tenant": b.id}}); status != http.StatusOK {
		t.Fatalf("seed block grant: %d %v", status, resp)
	}
	_, resp := kaManage(t, seedH, admin, "i483-seed-grant", map[string]any{"action": "tenant-grant-create",
		"data": map[string]any{"grantee_tenant": b.id, "granted_scope": a.scope}})
	grant, _ := resp["grant"].(map[string]any)
	grantID, _ := grant["id"].(string)
	if grantID == "" {
		t.Fatalf("seed tenant grant: %v", resp)
	}
	manage := mcHandler(t, f.pool, false, false).HandleManage
	cut := func(t *testing.T, table, body, msg string) {
		t.Helper()
		f.cutREST(t, manage, adminPlain, table, "/api/manage", body, msg)
	}

	t.Run("tenant-create", func(t *testing.T) {
		cut(t, "context_tenants", `{"action":"tenant-create","data":{"slug":"i483-cut-c","display_name":"I483 cut c"}}`, "manage: tenant-create failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenants WHERE slug = 'i483-cut-c'`)
	})
	t.Run("scope-create", func(t *testing.T) {
		cut(t, "context_tenant_scopes", fmt.Sprintf(`{"action":"scope-create","data":{"name":"cutx","tenant_id":%q}}`, a.id), "manage: scope-create failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenant_scopes WHERE scope = 'i483-cut-a:cutx'`)
	})
	t.Run("backend-create", func(t *testing.T) {
		cut(t, "context_backends", `{"action":"backend-create","data":{"name":"i483-cut-backend","base_url":"http://i483-cut-backend",`+
			`"roles":["chat"],"model_map":{"default":"stub-model"}}}`, "manage: backend-create failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_backends WHERE name = 'i483-cut-backend'`)
	})
	t.Run("tenant-update", func(t *testing.T) {
		cut(t, "context_tenants", fmt.Sprintf(`{"action":"tenant-update","id":%q,"data":{"display_name":"I483 cut renamed"}}`, a.id), "manage: tenant-update failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenants WHERE id = $1::uuid AND display_name = 'I483 cut renamed'`, a.id)
	})
	t.Run("tenant-limit-set", func(t *testing.T) {
		cut(t, "context_tenants", fmt.Sprintf(`{"action":"tenant-limit-set","id":%q,"data":{"max_scopes":7,"max_keys":7}}`, a.id), "manage: tenant-limit-set failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenants WHERE id = $1::uuid AND max_scopes = 7`, a.id)
	})
	t.Run("tenant-grant-create", func(t *testing.T) {
		cut(t, "context_tenant_grants", fmt.Sprintf(`{"action":"tenant-grant-create","data":{"grantee_tenant":%q,"granted_scope":%q}}`, a.id, b.scope), "manage: tenant-grant-create failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenant_grants WHERE grantee_tenant = $1::uuid`, a.id)
	})
	t.Run("tenant-grant-delete", func(t *testing.T) {
		cut(t, "context_tenant_grants", fmt.Sprintf(`{"action":"tenant-grant-delete","id":%q}`, grantID), "manage: tenant-grant-delete failed")
		f.waitLanded(t, `SELECT (count(*) = 0)::int FROM context_tenant_grants WHERE id = $1::uuid`, grantID)
	})
	t.Run("tenant-quota-set", func(t *testing.T) {
		cut(t, "context_tenant_quota", fmt.Sprintf(`{"action":"tenant-quota-set","data":{"scope":%q,"daily_calls":5}}`, a.scope), "manage: tenant-quota-set failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_tenant_quota WHERE scope = $1`, a.scope)
	})
	t.Run("block-grant-create", func(t *testing.T) {
		cut(t, "context_block_grants", fmt.Sprintf(`{"action":"block-grant-create","data":{"block_id":%q,"grantee_tenant":%q}}`, granted, a.id), "manage: block-grant-create failed")
		f.waitLanded(t, `SELECT count(*)::int FROM context_block_grants WHERE block_id = $1::uuid`, granted)
	})
	t.Run("block-grant-revoke", func(t *testing.T) {
		cut(t, "context_block_grants", fmt.Sprintf(`{"action":"block-grant-revoke","data":{"block_id":%q,"grantee_tenant":%q}}`, revoked, b.id), "manage: block-grant-revoke failed")
		f.waitLanded(t, `SELECT (count(*) = 0)::int FROM context_block_grants WHERE block_id = $1::uuid`, revoked)
	})
}
