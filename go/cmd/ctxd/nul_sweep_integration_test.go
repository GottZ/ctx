//go:build integration

// Issue #519: a NUL character (U+0000) in any value a request carries — a
// JSON body field, a form field, a path parameter, a query parameter — is a
// value the database stores neither in text nor in JSON, and every statement
// it reached failed as a server fault (500 with an ERROR line, or an MCP tool
// error carrying the SQL text). TestNULSweep drives every route of the
// PRODUCTION router (cmd/ctxd.NewRouter, the whole middleware chain) and every
// MCP tool of its /mcp server with a NUL in each value it reads and pins:
// no 500, no line above INFO, no SQL text in an answer, the marker of a NUL
// value in no table, and a refusal that names the field. The routes come from
// chi.Walk and the tools from tools/list, so a route or tool added later
// fails the coverage check until the sweep knows how to reach it.
//
// N519_OUT=<file> writes the result table; N519_CLEAN_OUT=<file> writes the
// answers of the same calls WITHOUT the NUL (status and normalised body), the
// differential probe between two builds.
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// nulSweepEscape is the JSON spelling of a NUL character (backslash, u, 0000),
// assembled so no source line carries the escape itself.
const nulSweepEscape = "\\" + "u0000"

// nulSweepMark is the placeholder a case template carries where its value goes.
const nulSweepMark = "§"

// nulSweepRefusal is the text every refusal ends its field name with.
const nulSweepRefusal = " carries a NUL character (U+0000)"

type nulSweepRec struct {
	level slog.Level
	msg   string
	reqID string
	attrs string
}

type nulSweepLog struct {
	mu   sync.Mutex
	recs []nulSweepRec
}

func (c *nulSweepLog) Enabled(context.Context, slog.Level) bool { return true }
func (c *nulSweepLog) WithAttrs([]slog.Attr) slog.Handler       { return c }
func (c *nulSweepLog) WithGroup(string) slog.Handler            { return c }
func (c *nulSweepLog) Handle(_ context.Context, r slog.Record) error {
	rec := nulSweepRec{level: r.Level, msg: r.Message}
	var b strings.Builder
	r.Attrs(func(a slog.Attr) bool {
		v := fmt.Sprint(a.Value.Resolve().Any())
		if a.Key == "request_id" {
			rec.reqID = v
		}
		if a.Key == "error" {
			fmt.Fprintf(&b, " error=%s", v)
		}
		return true
	})
	rec.attrs = b.String()
	c.mu.Lock()
	c.recs = append(c.recs, rec)
	c.mu.Unlock()
	return nil
}

// above returns the records above INFO of one request id.
func (c *nulSweepLog) above(reqID string) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.recs {
		if r.reqID == reqID && r.level > slog.LevelInfo {
			out = append(out, r.level.String()+" "+r.msg+r.attrs)
		}
	}
	return out
}

func (c *nulSweepLog) n() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.recs)
}

// unattributedAbove returns the records above INFO from index i on that carry
// no request id of the sweep.
func (c *nulSweepLog) unattributedAbove(i int) []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []string
	for _, r := range c.recs[i:] {
		if r.level > slog.LevelInfo && !strings.HasPrefix(r.reqID, "0519-") {
			out = append(out, r.level.String()+" "+r.msg+" request_id="+r.reqID+r.attrs)
		}
	}
	return out
}

// nulSweepCase is one call with one value carrying the NUL.
type nulSweepCase struct {
	id     string
	route  string // "METHOD /pattern", as chi.Walk names it ("MCP <tool>" for a tool)
	field  string // the field the refusal names
	method string
	url    string // may carry the mark (a path or query value)
	body   string // may carry the mark (a JSON or form value)
	form   bool   // body is application/x-www-form-urlencoded
	cred   string // admin | confirm | member | "" (none)
	cookie bool   // the web session's cookies and CSRF token
	mcp    bool
	// legit: the NUL is a legitimate value (bytes stored as bytea or sealed):
	// the call must succeed.
	legit bool
	// unguarded: a route the NUL guard does not front (no input it stores,
	// or another API's envelope): only no 500, no line, no SQL text.
	unguarded bool
	// oauth: the refusal speaks the OAuth error envelope.
	oauth bool
	// ownCheck: the route refuses the NUL in its own dispatcher (/api/manage,
	// since #530), in its form: no code.
	ownCheck bool
	// chunk: an ingest chunk, which fails on its own (200, its result
	// "failed" naming the field) while the others are written.
	chunk bool
	// timeout: the call streams (SSE); its context ends after this long.
	timeout time.Duration
}

type nulSweepResult struct {
	c       nulSweepCase
	status  int
	answer  string
	isError bool
	code    string
	lines   []string
}

// nulValue renders the mark for a variant: a NUL (JSON escape or %00) or the
// same value without it. The z/c prefix tells the two apart in the tables.
func nulValue(id string, nul, inJSON bool) string {
	if !nul {
		return "c519" + id + "AB"
	}
	if inJSON {
		return "z519" + id + "A" + nulSweepEscape + "B"
	}
	return "z519" + id + "A%00B"
}

func nulSweepJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func nulSweepMCP(tool string, args map[string]any) string {
	return nulSweepJSON(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": tool, "arguments": args}})
}

// nulSweepMCPAnswer extracts isError, the text and the structured code of a
// /mcp answer (plain JSON or one SSE event).
func nulSweepMCPAnswer(body string) (isError bool, text, code string) {
	s := body
	if i := strings.Index(s, "data: "); i >= 0 {
		s = s[i+6:]
		if j := strings.Index(s, "\n"); j >= 0 {
			s = s[:j]
		}
	}
	var env struct {
		Result *struct {
			IsError bool `json:"isError"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			Structured *struct {
				Code string `json:"code"`
			} `json:"structuredContent"`
		} `json:"result"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &env); err != nil {
		return false, "unparsed: " + body, ""
	}
	if env.Error != nil {
		return true, fmt.Sprintf("jsonrpc-error %d %s", env.Error.Code, env.Error.Message), ""
	}
	if env.Result == nil {
		return false, "no result", ""
	}
	if len(env.Result.Content) > 0 {
		text = env.Result.Content[0].Text
	}
	if env.Result.Structured != nil {
		code = env.Result.Structured.Code
	}
	return env.Result.IsError, text, code
}

type nulSweepFixture struct {
	router  http.Handler
	pool    *pgxpool.Pool
	log     *nulSweepLog
	keys    map[string]string
	cookies string
	csrf    string
	seq     int
}

func (f *nulSweepFixture) do(t *testing.T, c nulSweepCase, nul bool) nulSweepResult {
	t.Helper()
	f.seq++
	reqID := fmt.Sprintf("0519-%05x", f.seq)
	u := strings.ReplaceAll(c.url, nulSweepMark, nulValue(c.id, nul, false))
	body := strings.ReplaceAll(c.body, nulSweepMark, nulValue(c.id, nul, !c.form))
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	timeout := c.timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req := httptest.NewRequest(c.method, u, rd).WithContext(ctx)
	req.RemoteAddr = "192.0.2.10:4000"
	req.Header.Set("X-Request-ID", reqID)
	switch {
	case c.form:
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	case c.mcp:
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json, text/event-stream")
	case body != "":
		req.Header.Set("Content-Type", "application/json")
	}
	if k := f.keys[c.cred]; k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	if c.cookie {
		req.Header.Set("Cookie", f.cookies)
		req.Header.Set("X-CSRF-Token", f.csrf)
	}
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	res := nulSweepResult{c: c, status: rec.Code, answer: rec.Body.String()}
	if c.mcp {
		res.isError, res.answer, res.code = nulSweepMCPAnswer(rec.Body.String())
	} else {
		var env struct {
			Code string `json:"code"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &env) == nil {
			res.code = env.Code
		}
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		res.answer += " Location=" + loc
	}
	// Fire-and-forget afterwork of a handler logs after its answer.
	time.Sleep(20 * time.Millisecond)
	res.lines = f.log.above(reqID)
	return res
}

func nulSweepRouter(t *testing.T) *nulSweepFixture {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	lg := &nulSweepLog{}
	prev := slog.Default()
	slog.SetDefault(slog.New(lg))
	t.Cleanup(func() { slog.SetDefault(prev) })

	applyEnv(t, map[string]string{
		"CTX_CAMO_ENABLED":   "true",
		"CTX_SECRETS_KEY":    strings.Repeat("ab", 32),
		"CTX_OAUTH_DCR_MODE": "admin",
	})
	t.Setenv("CTX_OAUTH_DCR_MODE", "admin")
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	const plaintext = "c7519000000000000000000000000000deadbeefdeadbeefdeadbeefdeadbeef"
	created, _, err := store.BootstrapAdminKey(ctx, pool, plaintext, "nul-sweep-admin")
	if err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, reg, projectHub, d)
	return &nulSweepFixture{router: router, pool: pool, log: lg, keys: map[string]string{"admin": plaintext}}
}

func nulSweepOne(t *testing.T, pool *pgxpool.Pool, q string, args ...any) string {
	t.Helper()
	var s string
	if err := pool.QueryRow(context.Background(), q, args...).Scan(&s); err != nil {
		t.Fatalf("setup lookup %q: %v", q, err)
	}
	return s
}

// nulSweepMarkerHits returns every stored value that carries a NUL variant's
// marker (z519<id>), per case id: a value written with its NUL removed or
// replaced shows up here.
func nulSweepMarkerHits(t *testing.T, pool *pgxpool.Pool) map[string][]string {
	t.Helper()
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT c.table_schema, c.table_name, c.column_name, c.data_type
		  FROM information_schema.columns c
		  JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		 WHERE t.table_type = 'BASE TABLE'
		   AND c.table_schema NOT IN ('pg_catalog', 'information_schema')
		   AND c.table_schema NOT LIKE '%timescaledb%'
		   AND c.data_type IN ('text', 'character varying', 'character', 'jsonb', 'json', 'ARRAY', 'bytea')`)
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	type col struct{ schema, table, column, typ string }
	var cols []col
	for rows.Next() {
		var c col
		if err := rows.Scan(&c.schema, &c.table, &c.column, &c.typ); err != nil {
			t.Fatalf("scan column: %v", err)
		}
		cols = append(cols, c)
	}
	rows.Close()
	marker := regexp.MustCompile(`z519([A-Za-z0-9_.-]+?)A`)
	hits := map[string][]string{}
	for _, c := range cols {
		ident := pgx.Identifier{c.column}.Sanitize()
		expr := ident + "::text"
		if c.typ == "bytea" {
			expr = "encode(" + ident + ", 'escape')"
		}
		q := fmt.Sprintf(`SELECT %s FROM %s WHERE strpos(%s, 'z519') > 0`, expr, pgx.Identifier{c.schema, c.table}.Sanitize(), expr)
		r, err := pool.Query(ctx, q)
		if err != nil {
			continue
		}
		for r.Next() {
			var v string
			if r.Scan(&v) != nil {
				continue
			}
			for _, m := range marker.FindAllStringSubmatch(v, -1) {
				hits[m[1]] = append(hits[m[1]], c.table+"."+c.column)
			}
		}
		r.Close()
	}
	return hits
}

// nulSweepWalk lists the router's routes as "METHOD /pattern".
func nulSweepWalk(t *testing.T, router http.Handler) []string {
	t.Helper()
	mux, ok := router.(chi.Routes)
	if !ok {
		t.Fatalf("router is %T, not chi.Routes", router)
	}
	seen := map[string]bool{}
	var out []string
	err := chi.Walk(mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		route = strings.ReplaceAll(route, "/*/", "/")
		k := method + " " + route
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	sort.Strings(out)
	return out
}

// nulSweepUnguarded names the routes the guard does not front: they read no
// value they store (health, metadata documents), or speak another API's
// envelope behind their own chain (the model API, off by default here).
func nulSweepUnguarded(route string) bool {
	_, path, _ := strings.Cut(route, " ")
	return path == "/health" || strings.HasPrefix(path, "/.well-known/") ||
		strings.HasPrefix(path, "/v1") || strings.HasPrefix(path, "/v2")
}

// nulSweepParamValue is a plausible value for a path parameter (the NUL
// variant appends the mark to it, so the lookup by that value runs).
func nulSweepParamValue(name, blockID, projectID string) string {
	switch name {
	case "id":
		return projectID
	case "block_id":
		return blockID
	case "project_id":
		return projectID
	case "provider":
		return "github"
	case "key":
		return "mcp.instructions"
	case "name":
		return "n519name"
	case "category":
		return "learnings"
	}
	return "n519"
}

func TestNULSweep(t *testing.T) {
	if testing.Short() {
		t.Skip("integration sweep")
	}
	f := nulSweepRouter(t)
	ctx := context.Background()
	adminKey := f.keys["admin"]

	// A knowledge key with confirm_writes (the staging path of the MCP tools)
	// and a plain one for the blob cases (the admin key's blob budget is
	// spent by the REST blob cases first).
	row, confirmPlain, err := store.CreateApiKey(ctx, f.pool, "n519-confirm", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("confirm key: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, row.ID); err != nil {
		t.Fatalf("confirm_writes: %v", err)
	}
	_, memberPlain, err := store.CreateApiKey(ctx, f.pool, "n519-member", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("member key: %v", err)
	}
	_, blobPlain, err := store.CreateApiKey(ctx, f.pool, "n519-blob", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("blob key: %v", err)
	}
	f.keys["confirm"], f.keys["member"], f.keys["blob"] = confirmPlain, memberPlain, blobPlain

	// ── fixtures through the production write paths ──
	setup := func(method, u, body, cred string) (int, string) {
		c := nulSweepCase{id: "S", method: method, url: u, body: body, cred: cred, mcp: u == "/mcp"}
		r := f.do(t, c, false)
		return r.status, r.answer
	}
	if st, a := setup(http.MethodPost, "/api/store", nulSweepJSON(map[string]any{"category": "learnings", "title": "n519 base block", "content": "base content"}), "admin"); st != 200 {
		t.Fatalf("setup store: %d %s", st, a)
	}
	baseID := nulSweepOne(t, f.pool, `SELECT id::text FROM context_blocks WHERE title = 'n519 base block'`)
	// The fixture has no backends: /health warns about that once per role,
	// written by its probe round — let it say so before the sweep.
	setup(http.MethodGet, "/health", "", "")
	time.Sleep(200 * time.Millisecond)
	if st, a := setup(http.MethodPost, "/api/blob/store", nulSweepJSON(map[string]any{"file": base64.StdEncoding.EncodeToString([]byte("hello")),
		"filename": "a.txt", "category": "files", "title": "n519 base blob", "mime_type": "text/plain"}), "admin"); st != 200 {
		t.Fatalf("setup blob: %d %s", st, a)
	}
	blobID := nulSweepOne(t, f.pool, `SELECT id::text FROM context_blobs WHERE title = 'n519 base blob'`)
	setup(http.MethodPost, "/mcp", nulSweepMCP("issue_create", map[string]any{"title": "n519 base issue", "content": "x"}), "admin")
	issueID := nulSweepOne(t, f.pool, `SELECT id::text FROM context_blocks WHERE title LIKE '%n519 base issue%' LIMIT 1`)
	setup(http.MethodPost, "/register", nulSweepJSON(map[string]any{"redirect_uris": []string{"http://127.0.0.1/cb"}, "client_name": "n519 client"}), "admin")
	clientID := nulSweepOne(t, f.pool, `SELECT client_id FROM context_oauth_clients WHERE label = 'n519 client'`)
	setup(http.MethodPost, "/api/project", nulSweepJSON(map[string]any{"identity": "manual:n519", "scope": "n519proj", "tenant_id": store.DefaultTenantID}), "admin")
	projectID := "00000000-0000-0000-0000-000000000519"
	_ = f.pool.QueryRow(ctx, `SELECT id::text FROM context_projects ORDER BY created_at LIMIT 1`).Scan(&projectID)
	ar, err := auth.Authenticate(ctx, f.pool, adminKey)
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate admin: %v", err)
	}
	sess, err := store.CreateSession(ctx, f.pool, ar.HomeScope, ar.ReadScopes, ar.ApiKeyID, "")
	if err != nil {
		t.Fatalf("chat session: %v", err)
	}
	{
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(nulSweepJSON(map[string]any{"api_key": adminKey})))
		req.Header.Set("Content-Type", "application/json")
		req.RemoteAddr = "192.0.2.10:4000"
		rec := httptest.NewRecorder()
		f.router.ServeHTTP(rec, req)
		var a struct {
			CSRF string `json:"csrf_token"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &a)
		var parts []string
		for _, ck := range rec.Result().Cookies() {
			parts = append(parts, ck.Name+"="+ck.Value)
		}
		f.cookies, f.csrf = strings.Join(parts, "; "), a.CSRF
	}

	js := nulSweepJSON
	m := nulSweepMark
	post := func(id, route, field, u string, body any) nulSweepCase {
		return nulSweepCase{id: id, route: route, field: field, method: http.MethodPost, url: u, body: js(body), cred: "admin"}
	}
	withMethod := func(c nulSweepCase, method string) nulSweepCase { c.method = method; return c }
	chunk := func(c nulSweepCase) nulSweepCase { c.chunk = true; return c }
	form := func(id, route, field, u string, v map[string]string) nulSweepCase {
		q := url.Values{}
		for k, s := range v {
			q.Set(k, s)
		}
		// url.Values escapes the mark; put it back.
		enc := strings.ReplaceAll(q.Encode(), url.QueryEscape(m), m)
		return nulSweepCase{id: id, route: route, field: field, method: http.MethodPost, url: u, body: enc, form: true, oauth: true}
	}
	authz := func(over map[string]string) map[string]string {
		v := map[string]string{"response_type": "code", "client_id": clientID, "redirect_uri": "http://127.0.0.1/cb",
			"code_challenge": "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM", "code_challenge_method": "S256",
			"consent": "yes", "api_key": adminKey, "state": "st"}
		for k, s := range over {
			v[k] = s
		}
		return v
	}
	blob := func(over map[string]any) map[string]any {
		v := map[string]any{"file": "aGk=", "filename": "f", "category": "files", "title": "n519 blob", "mime_type": "text/plain"}
		for k, s := range over {
			v[k] = s
		}
		return v
	}
	storeBody := func(over map[string]any) map[string]any {
		v := map[string]any{"category": "learnings", "title": "n519 store", "content": "c"}
		for k, s := range over {
			v[k] = s
		}
		return v
	}

	// ── body fields of the REST routes that read a body ──
	cases := []nulSweepCase{
		post("R01", "POST /api/store", "category", "/api/store", storeBody(map[string]any{"category": m})),
		post("R02", "POST /api/store", "title", "/api/store", storeBody(map[string]any{"title": m})),
		post("R03", "POST /api/store", "content", "/api/store", storeBody(map[string]any{"content": m})),
		post("R04", "POST /api/store", "tags[0]", "/api/store", storeBody(map[string]any{"tags": []string{m}})),
		post("R05", "POST /api/store", "metadata.source", "/api/store", storeBody(map[string]any{"metadata": map[string]any{"source": m}})),
		post("R06", "POST /api/store", "metadata", "/api/store", storeBody(map[string]any{"metadata": map[string]any{m: "v"}})),
		post("R07", "POST /api/store", "scope", "/api/store", storeBody(map[string]any{"scope": "private" + m})),
		post("R08", "POST /api/store", "sensitivity", "/api/store", storeBody(map[string]any{"sensitivity": "public" + m})),
		post("R09", "POST /api/store", "type", "/api/store", storeBody(map[string]any{"type": "reference" + m})),
		post("R10", "POST /api/store", "parent_id", "/api/store", storeBody(map[string]any{"parent_id": baseID + m})),
		post("R11", "POST /api/store", "supersedes", "/api/store", storeBody(map[string]any{"supersedes": baseID + m})),
		post("R12", "POST /api/store/batch", "items[0]: content", "/api/store/batch", map[string]any{"items": []any{storeBody(map[string]any{"content": m})}}),
		post("R13", "POST /api/store/batch", "items[1]: title", "/api/store/batch", map[string]any{"items": []any{storeBody(nil), storeBody(map[string]any{"title": m})}}),
		post("R14", "POST /api/store/batch", "items[0]: metadata.source", "/api/store/batch", map[string]any{"items": []any{storeBody(map[string]any{"metadata": map[string]any{"source": m}})}}),
		post("R15", "POST /api/store/batch", "items[0]: tags[0]", "/api/store/batch", map[string]any{"items": []any{storeBody(map[string]any{"tags": []string{m}})}}),
		post("R16", "POST /api/store/batch", "items[0]: id", "/api/store/batch", map[string]any{"items": []any{map[string]any{"id": baseID + m, "title": "t"}}}),
		post("R17", "POST /api/store/batch", "items[0]: content", "/api/store/batch", map[string]any{"items": []any{map[string]any{"id": baseID, "content": m}}}),
		chunk(post("R18", "POST /api/ingest", "content", "/api/ingest", map[string]any{"chunks": []any{storeBody(map[string]any{"title": "n519 ok"}), storeBody(map[string]any{"content": m})}})),
		chunk(post("R19", "POST /api/ingest", "title", "/api/ingest", map[string]any{"chunks": []any{storeBody(map[string]any{"title": m})}})),
		chunk(post("R20", "POST /api/ingest", "metadata", "/api/ingest", map[string]any{"chunks": []any{storeBody(map[string]any{"metadata": map[string]any{"k": m}})}})),
		chunk(post("R21", "POST /api/ingest", "source_id", "/api/ingest", map[string]any{"chunks": []any{storeBody(map[string]any{"source_id": m})}})),
		post("R22", "POST /api/ingest", "target_scope", "/api/ingest", map[string]any{"target_scope": "private" + m, "chunks": []any{storeBody(nil)}}),
		{id: "R23", route: "POST /api/confirm", field: "payload_hash", method: http.MethodPost, url: "/api/confirm", body: js(map[string]any{"payload_hash": "abc" + m}), cred: "confirm"},
		post("R24", "POST /api/blob/store", "category", "/api/blob/store", blob(map[string]any{"category": m})),
		post("R25", "POST /api/blob/store", "title", "/api/blob/store", blob(map[string]any{"title": m})),
		post("R26", "POST /api/blob/store", "filename", "/api/blob/store", blob(map[string]any{"filename": m})),
		post("R27", "POST /api/blob/store", "mime_type", "/api/blob/store", blob(map[string]any{"mime_type": "text/plain" + m})),
		post("R28", "POST /api/blob/store", "tags[0]", "/api/blob/store", blob(map[string]any{"tags": []string{m}})),
		post("R29", "POST /api/blob/store", "metadata.k", "/api/blob/store", blob(map[string]any{"metadata": map[string]any{"k": m}})),
		post("R30", "POST /api/blob/store", "context_block_id", "/api/blob/store", blob(map[string]any{"context_block_id": baseID + m})),
		post("R31", "POST /api/blob/manage", "id", "/api/blob/manage", map[string]any{"action": "get", "id": blobID + m}),
		post("R32", "POST /api/blob/manage", "id", "/api/blob/manage", map[string]any{"action": "delete", "id": blobID + m}),
		post("R33", "POST /api/blob/manage", "action", "/api/blob/manage", map[string]any{"action": "get" + m, "id": blobID}),
		post("R34", "POST /api/blob/fetch", "id", "/api/blob/fetch", map[string]any{"id": blobID + m}),
		post("R35", "POST /api/blob/fetch", "title", "/api/blob/fetch", map[string]any{"category": "files", "title": m}),
		post("R36", "POST /api/blob/search", "query", "/api/blob/search", map[string]any{"query": m}),
		post("R37", "POST /api/blob/search", "category", "/api/blob/search", map[string]any{"category": m}),
		post("R38", "POST /api/chat/stream", "message", "/api/chat/stream", map[string]any{"message": m}),
		post("R39", "POST /api/chat/stream", "session_id", "/api/chat/stream", map[string]any{"message": "hi", "session_id": sess.ID + m}),
		post("R40", "POST /api/img/sign", "urls[0]", "/api/img/sign", map[string]any{"urls": []string{"https://example.com/a" + m + ".png"}}),
		{id: "R41", route: "POST /api/session/select-key", field: "api_key_id", method: http.MethodPost, url: "/api/session/select-key",
			body: js(map[string]any{"api_key_id": ar.ApiKeyID + m}), cookie: true},
		{id: "R42", route: "POST /auth/login", field: "api_key", method: http.MethodPost, url: "/auth/login",
			body: js(map[string]any{"api_key": adminKey[:10] + m + adminKey[10:]})},
		form("R43", "POST /token", "client_id", "/token", map[string]string{"grant_type": "refresh_token", "client_id": clientID + m, "refresh_token": "x"}),
		form("R44", "POST /token", "refresh_token", "/token", map[string]string{"grant_type": "refresh_token", "client_id": clientID, "refresh_token": "ctxr_abc" + m}),
		form("R45", "POST /token", "code", "/token", map[string]string{"grant_type": "authorization_code", "client_id": clientID, "code": "abc" + m, "code_verifier": "v", "redirect_uri": "http://127.0.0.1/cb"}),
		{id: "R46", route: "POST /register", field: "client_uri", method: http.MethodPost, url: "/register", cred: "admin", oauth: true,
			body: js(map[string]any{"redirect_uris": []string{"http://127.0.0.1/cb"}, "client_uri": "https://x/" + m})},
		{id: "R47", route: "POST /register", field: "contacts[0]", method: http.MethodPost, url: "/register", cred: "admin", oauth: true,
			body: js(map[string]any{"redirect_uris": []string{"http://127.0.0.1/cb"}, "contacts": []string{m}})},
		{id: "R48", route: "POST /register", field: "software_id", method: http.MethodPost, url: "/register", cred: "admin", oauth: true,
			body: js(map[string]any{"redirect_uris": []string{"http://127.0.0.1/cb"}, "software_id": m})},
		{id: "R49", route: "POST /register", field: "client_name", method: http.MethodPost, url: "/register", cred: "admin", oauth: true,
			body: js(map[string]any{"redirect_uris": []string{"http://127.0.0.1/cb"}, "client_name": m})},
		form("R50", "POST /authorize", "client_id", "/authorize", authz(map[string]string{"client_id": clientID + m})),
		form("R51", "POST /authorize", "code_challenge", "/authorize", authz(map[string]string{"code_challenge": m})),
		form("R52", "POST /authorize", "state", "/authorize", authz(map[string]string{"state": m})),
		{id: "R53", route: "PUT /api/settings/{key}", field: "value", method: http.MethodPut, url: "/api/settings/mcp.instructions", cred: "admin", body: js(map[string]any{"value": m})},
		{id: "R54", route: "PUT /api/types/{name}", field: "display_name", method: http.MethodPut, url: "/api/types/n519-type-a", cred: "admin", body: js(map[string]any{"display_name": m})},
		{id: "R55", route: "PUT /api/types/{name}", field: "description", method: http.MethodPut, url: "/api/types/n519-type-b", cred: "admin", body: js(map[string]any{"description": m})},
		{id: "R56", route: "PUT /api/types/{name}", field: "config.retrieval.intent_patterns[0]", method: http.MethodPut, url: "/api/types/n519-type-c", cred: "admin",
			body: js(map[string]any{"config": map[string]any{"v": 1, "retrieval": map[string]any{"intent_patterns": []string{m}}}})},
		{id: "R57", route: "PUT /api/secrets/{name}", field: "value", method: http.MethodPut, url: "/api/secrets/n519secret", cred: "admin", body: js(map[string]any{"value": m}), legit: true},
		{id: "R58", route: "PUT /api/graph/category-hues/{category}", field: "hue", method: http.MethodPut, url: "/api/graph/category-hues/learnings", cred: "admin", body: js(map[string]any{"hue": m})},
		post("R59", "POST /api/query", "query", "/api/query", map[string]any{"query": m, "synthesize": false}),
		post("R60", "POST /api/query", "category", "/api/query", map[string]any{"query": "hello", "category": m, "synthesize": false}),
		post("R61", "POST /api/search", "query", "/api/search", map[string]any{"query": m}),
		post("R62", "POST /api/search", "tags[0]", "/api/search", map[string]any{"query": "x", "tags": []string{m}}),
		post("R63", "POST /api/recent", "category", "/api/recent", map[string]any{"category": m}),
		post("R64", "POST /api/recent", "types[0]", "/api/recent", map[string]any{"types": []string{m}}),
		{id: "R65", route: "POST /api/manage", field: "data.title", method: http.MethodPost, url: "/api/manage", cred: "admin", ownCheck: true,
			body: js(map[string]any{"action": "update", "data": map[string]any{"id": baseID, "title": m}})},
		post("R66", "POST /api/project", "identity", "/api/project", map[string]any{"identity": "manual:" + m, "scope": "n519p2", "tenant_id": store.DefaultTenantID}),
		post("R67", "POST /api/project", "display_name", "/api/project", map[string]any{"identity": "manual:n519b", "scope": "n519p3", "display_name": m, "tenant_id": store.DefaultTenantID}),
		withMethod(post("R68", "PATCH /api/project/{id}", "display_name", "/api/project/"+projectID, map[string]any{"display_name": m}), http.MethodPatch),
		post("R69", "POST /api/project/{id}/issues", "title", "/api/project/"+projectID+"/issues", map[string]any{"title": m}),
		post("R70", "POST /api/project/{id}/issues", "content", "/api/project/"+projectID+"/issues", map[string]any{"title": "t", "content": m}),
		withMethod(post("R71", "PATCH /api/project/{id}/issues/{block_id}", "title", "/api/project/"+projectID+"/issues/"+issueID, map[string]any{"title": m}), http.MethodPatch),
		post("R72", "POST /api/project/{id}/issues/{block_id}/comments", "content", "/api/project/"+projectID+"/issues/"+issueID+"/comments", map[string]any{"content": m}),
		post("R73", "POST /api/project/{id}/issues/{block_id}/comments", "author", "/api/project/"+projectID+"/issues/"+issueID+"/comments", map[string]any{"content": "c", "author": m}),
		{id: "R74", route: "POST /webhooks/github/{project_id}", field: "action", method: http.MethodPost, url: "/webhooks/github/" + projectID, body: js(map[string]any{"action": m})},
		{id: "R75", route: "POST /api/synthesize/daily", field: "?date", method: http.MethodPost, url: "/api/synthesize/daily?date=2026-01-01" + m, cred: "admin"},
		{id: "R76", route: "GET /api/graph/ego", field: "?id", method: http.MethodGet, url: "/api/graph/ego?id=" + baseID + m, cred: "admin"},
		{id: "R77", route: "GET /auth/callback/{provider}", field: "?state", method: http.MethodGet, url: "/auth/callback/github?state=" + m + "&code=c"},
		{id: "R78", route: "* /authorize", field: "?client_id", method: http.MethodGet, url: "/authorize?response_type=code&client_id=" + clientID + m + "&redirect_uri=http%3A%2F%2F127.0.0.1%2Fcb", oauth: true},
		// Legitimate NUL bytes: the REST blob file is base64 (its bytes carry a
		// NUL, stored as bytea).
		post("R79", "POST /api/blob/store", "file", "/api/blob/store", blob(map[string]any{"file": base64.StdEncoding.EncodeToString([]byte("A\x00B")), "title": "n519 blob nul"})),
	}
	cases[len(cases)-1].legit = true

	// ── the routes of the router: a path parameter and a query parameter
	// with a NUL on each, and the coverage check ──
	routes := nulSweepWalk(t, f.router)
	bodyRoutes := map[string]bool{}
	for _, c := range cases {
		bodyRoutes[c.route] = true
	}
	// Routes that read no body value (or none they store) — named, so a new
	// body route fails the check below until it has a case.
	noBody := map[string]bool{
		"POST /api/digest": true, "POST /auth/refresh": true, "POST /auth/logout": true,
		"POST /api/project/{id}/sync": true, "POST /api/project/{id}/webhook-secret": true,
	}
	paramRe := regexp.MustCompile(`\{([^}]+)\}`)
	sse := map[string]bool{"GET /api/events": true, "GET /api/project/events": true, "POST /api/chat/stream": true}
	n := 0
	for _, rt := range routes {
		method, pattern, _ := strings.Cut(rt, " ")
		if strings.HasSuffix(pattern, "/*") || pattern == "/*" {
			continue
		}
		switch method {
		case http.MethodGet, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		default:
			continue
		}
		// Mounted for every method; the handler serves these two.
		if (pattern == "/authorize" && method != http.MethodGet && method != http.MethodPost) ||
			(pattern == "/mcp" && method != http.MethodPost) {
			continue
		}
		if (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch) && !bodyRoutes[rt] && !noBody[rt] &&
			!nulSweepUnguarded(rt) && pattern != "/mcp" {
			t.Errorf("coverage: %s reads a body and has no case in the sweep (add one, or name it in noBody)", rt)
		}
		cred := "admin"
		if strings.HasPrefix(pattern, "/auth/") || pattern == "/authorize" || pattern == "/token" || strings.HasPrefix(pattern, "/webhooks/") || strings.HasPrefix(pattern, "/api/img/{") {
			cred = ""
		}
		var to time.Duration
		if sse[rt] {
			to = 500 * time.Millisecond
		}
		pid := projectID
		if strings.HasPrefix(pattern, "/api/chat/sessions/") {
			pid = sess.ID
		}
		concrete := paramRe.ReplaceAllStringFunc(pattern, func(p string) string {
			return url.PathEscape(nulSweepParamValue(p[1:len(p)-1], issueID, pid))
		})
		oauth := pattern == "/authorize" || pattern == "/token" || pattern == "/register"
		base := nulSweepCase{route: rt, method: method, cred: cred, unguarded: nulSweepUnguarded(rt), oauth: oauth, timeout: to}
		if pattern == "/mcp" {
			base.mcp = true
			base.body = nulSweepMCP("search", map[string]any{"query": "x"})
		} else if method != http.MethodGet && method != http.MethodDelete {
			base.body = "{}"
		}
		for _, p := range paramRe.FindAllStringSubmatch(pattern, -1) {
			n++
			c := base
			c.id = fmt.Sprintf("P%03d", n)
			c.field = "{" + p[1] + "}"
			v := url.PathEscape(nulSweepParamValue(p[1], issueID, pid))
			c.url = strings.Replace(concrete, v, v+m, 1)
			cases = append(cases, c)
		}
		n++
		c := base
		c.id = fmt.Sprintf("Q%03d", n)
		c.field = "?n519"
		c.url = concrete + "?n519=" + m
		if c.mcp {
			c.mcp = false // the query parameter is refused before the JSON-RPC body is read
		}
		cases = append(cases, c)
	}

	// ── MCP: every tool, every text argument ──
	tools := nulSweepTools(t, f)
	mcpCases, mcpMissing := nulSweepMCPCases(tools, baseID, blobID, issueID)
	for _, miss := range mcpMissing {
		t.Errorf("coverage: MCP tool %s has no baseline arguments in the sweep", miss)
	}
	cases = append(cases, mcpCases...)
	cases = append(cases,
		nulSweepCase{id: "M901", route: "MCP store (staged)", field: "content", method: http.MethodPost, url: "/mcp", cred: "confirm", mcp: true,
			body: nulSweepMCP("store", map[string]any{"category": "learnings", "title": "n519 staged", "content": m})},
		nulSweepCase{id: "M902", route: "MCP update (staged)", field: "content", method: http.MethodPost, url: "/mcp", cred: "confirm", mcp: true,
			body: nulSweepMCP("update", map[string]any{"id": baseID, "content": m})},
		nulSweepCase{id: "M903", route: "MCP blob_store (staged)", field: "title", method: http.MethodPost, url: "/mcp", cred: "confirm", mcp: true,
			body: nulSweepMCP("blob_store", map[string]any{"category": "files", "title": m, "filename": "f", "mime_type": "text/plain", "text": "x"})},
	)

	// ── the differential probe: the same calls without the NUL ──
	// (first, so two builds run it from the same state: a refused NUL call
	// spends no write budget a later call would see)
	if p := os.Getenv("N519_CLEAN_OUT"); p != "" {
		var out strings.Builder
		for _, c := range cases {
			r := f.do(t, c, false)
			fmt.Fprintf(&out, "%s %s %s | %d | %s\n", c.id, c.route, c.field, r.status, nulSweepNormalise(r.answer))
		}
		if err := os.WriteFile(p, []byte(out.String()), 0o600); err != nil {
			t.Errorf("write %s: %v", p, err)
		}
	}

	// ── run: the NUL variant of every case ──
	phaseStart := f.log.n()
	var results []nulSweepResult
	for _, c := range cases {
		results = append(results, f.do(t, c, true))
	}
	time.Sleep(time.Second)
	hits := nulSweepMarkerHits(t, f.pool)

	var table strings.Builder
	table.WriteString("routes: " + strings.Join(routes, ", ") + "\n\n")
	table.WriteString("| ID | route | field | status | code | answer | lines above INFO | marker in |\n|---|---|---|---|---|---|---|---|\n")
	for _, r := range results {
		c := r.c
		where := strings.Join(hits[c.id], ", ")
		ans := r.answer
		if len(ans) > 200 {
			ans = ans[:200] + "…"
		}
		esc := func(s string) string {
			s = strings.ReplaceAll(s, "|", "¦")
			s = strings.ReplaceAll(s, "\n", " ")
			return strings.ReplaceAll(s, "\x00", `\x00`)
		}
		fmt.Fprintf(&table, "| %s | %s | %s | %d | %s | %s | %s | %s |\n", c.id, esc(c.route), esc(c.field), r.status, r.code,
			esc(ans), esc(strings.Join(r.lines, "; ")), esc(where))

		label := c.id + " " + c.route + " " + c.field
		if r.status == http.StatusInternalServerError {
			t.Errorf("%s: status %d: %.200s", label, r.status, r.answer)
		}
		if len(r.lines) > 0 {
			t.Errorf("%s: lines above INFO: %v", label, r.lines)
		}
		if strings.Contains(r.answer, "SQLSTATE") {
			t.Errorf("%s: the answer carries SQL text: %.200s", label, r.answer)
		}
		if !c.legit && len(hits[c.id]) > 0 {
			t.Errorf("%s: the NUL value's marker was stored: %v", label, hits[c.id])
		}
		switch {
		case c.legit:
			if r.status != http.StatusOK || r.isError {
				t.Errorf("%s: a legitimate NUL is refused: %d %.200s", label, r.status, r.answer)
			}
		case c.unguarded:
		case c.chunk:
			want := `"status":"failed","error":"` + c.field + nulSweepRefusal
			if r.status != http.StatusOK || !strings.Contains(r.answer, want) {
				t.Errorf("%s: want the chunk failed naming %q, got %d %.300s", label, c.field, r.status, r.answer)
			}
		case c.mcp:
			want := c.field + nulSweepRefusal
			if !r.isError || !strings.HasPrefix(r.answer, want) || r.code != "invalid_body" {
				t.Errorf("%s: want the tool error %q (code invalid_body), got isError=%v code=%q %.200s", label, want, r.isError, r.code, r.answer)
			}
		default:
			want := c.field + nulSweepRefusal
			if r.status != http.StatusBadRequest || !strings.Contains(r.answer, want) {
				t.Errorf("%s: want 400 naming %q, got %d %.200s", label, c.field, r.status, r.answer)
			}
			if c.oauth && !strings.Contains(r.answer, `"error":"invalid_`) {
				t.Errorf("%s: want the OAuth error envelope, got %.200s", label, r.answer)
			}
			if !c.oauth && !c.ownCheck && r.code != "invalid_body" {
				t.Errorf("%s: want code invalid_body, got %q", label, r.code)
			}
		}
	}
	// A line the per-request attribution missed (a record without a request
	// id, written while the NUL variants ran).
	for _, l := range f.log.unattributedAbove(phaseStart) {
		t.Errorf("a line above INFO without a request id during the sweep: %s", l)
	}
	t.Logf("NUL sweep: %d cases (%d routes, %d MCP tools)", len(results), len(routes), len(tools))
	if p := os.Getenv("N519_OUT"); p != "" {
		if err := os.WriteFile(p, []byte(table.String()), 0o600); err != nil {
			t.Errorf("write %s: %v", p, err)
		}
	}

}

var nulSweepVolatile = []*regexp.Regexp{
	regexp.MustCompile(`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`),
	regexp.MustCompile(`\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?(Z|[+-]\d\d:\d\d)`),
	regexp.MustCompile(`[0-9a-f]{24,}`),
	regexp.MustCompile(`"(duration_ms|latency_ms|took_ms|elapsed_ms|client_id_issued_at|created_at|expires_at|updated_at)":\s*[0-9.]+`),
	regexp.MustCompile(`ctx[a-z]?_[A-Za-z0-9_-]{16,}`),
}

// nulSweepNormalise blanks the values that differ between two runs (ids,
// timestamps, tokens, durations).
func nulSweepNormalise(s string) string {
	for _, re := range nulSweepVolatile {
		s = re.ReplaceAllString(s, "<v>")
	}
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > 400 {
		s = s[:400]
	}
	return s
}

type nulSweepTool struct {
	Name        string `json:"name"`
	InputSchema struct {
		Properties map[string]struct {
			Type       any                        `json:"type"`
			Properties map[string]json.RawMessage `json:"properties"`
			Items      *struct {
				Type       any                        `json:"type"`
				Properties map[string]json.RawMessage `json:"properties"`
			} `json:"items"`
		} `json:"properties"`
	} `json:"inputSchema"`
}

func nulSweepTools(t *testing.T, f *nulSweepFixture) []nulSweepTool {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.keys["admin"])
	rec := httptest.NewRecorder()
	f.router.ServeHTTP(rec, req)
	s := rec.Body.String()
	if i := strings.Index(s, "data: "); i >= 0 {
		s = s[i+6:]
		if j := strings.Index(s, "\n"); j >= 0 {
			s = s[:j]
		}
	}
	var env struct {
		Result struct {
			Tools []nulSweepTool `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(s), &env); err != nil || len(env.Result.Tools) == 0 {
		t.Fatalf("tools/list: %v %.300s", err, rec.Body.String())
	}
	return env.Result.Tools
}

func nulSweepIsType(v any, want string) bool {
	switch x := v.(type) {
	case string:
		return x == want
	case []any:
		for _, e := range x {
			if e == want {
				return true
			}
		}
	}
	return false
}

// nulSweepMCPCases builds, for every tool and every text argument of its
// schema (a string, a list of strings, an object, a list of objects' string
// fields), a call with the tool's baseline arguments and a NUL in that one
// argument. A tool without baseline arguments is returned as missing.
func nulSweepMCPCases(tools []nulSweepTool, baseID, blobID, issueID string) ([]nulSweepCase, []string) {
	m := nulSweepMark
	baseline := map[string]map[string]any{
		"query":         {"question": "hello"},
		"search":        {"query": "hello"},
		"recent":        {},
		"get":           {"id": baseID},
		"store":         {"category": "learnings", "title": "n519 mcp", "content": "c"},
		"store_batch":   {"items": []any{map[string]any{"category": "learnings", "title": "n519 mcp batch", "content": "c"}}},
		"update":        {"id": baseID, "title": "n519 mcp update", "edits": []any{map[string]any{"old": "base", "new": "x"}}},
		"archive":       {"id": baseID},
		"confirm":       {"payload_hash": "abc"},
		"blob_store":    {"category": "files", "title": "n519 mcp blob", "filename": "f", "mime_type": "text/plain", "text": "x"},
		"blob_fetch":    {"id": blobID},
		"blob_link":     {"id": blobID, "context_block_id": baseID},
		"guard_list":    {},
		"guard_resolve": {"ids": []string{baseID}, "resolution": "keep"},
		"issue_create":  {"title": "n519 mcp issue"},
		"issue_comment": {"issue_id": issueID, "content": "c"},
		"issue_state":   {"issue_id": issueID, "status": "closed"},
	}
	// Tools whose calls a member key makes (the admin key's blob budget is
	// spent by the REST blob cases) and the legitimate NUL arguments.
	cred := map[string]string{"blob_store": "member", "blob_link": "member", "blob_fetch": "member"}
	legit := map[string]bool{"blob_store text": true}
	var out []nulSweepCase
	var missing []string
	n := 0
	for _, tl := range tools {
		args, ok := baseline[tl.Name]
		if !ok {
			missing = append(missing, tl.Name)
			continue
		}
		names := make([]string, 0, len(tl.InputSchema.Properties))
		for p := range tl.InputSchema.Properties {
			names = append(names, p)
		}
		sort.Strings(names)
		add := func(field string, a map[string]any) {
			n++
			c := nulSweepCase{id: fmt.Sprintf("M%03d", n), route: "MCP " + tl.Name, field: field, method: http.MethodPost,
				url: "/mcp", body: nulSweepMCP(tl.Name, a), cred: "admin", mcp: true}
			if k := cred[tl.Name]; k != "" {
				c.cred = k
			}
			if legit[tl.Name+" "+field] {
				c.legit, c.cred = true, "blob"
			}
			out = append(out, c)
		}
		with := func(p string, v any) map[string]any {
			a := map[string]any{}
			for k, x := range args {
				a[k] = x
			}
			a[p] = v
			return a
		}
		for _, p := range names {
			prop := tl.InputSchema.Properties[p]
			switch {
			case nulSweepIsType(prop.Type, "string"):
				v := any(m)
				if base, ok := args[p].(string); ok && (p == "id" || strings.HasSuffix(p, "_id")) {
					v = base + m
				}
				add(p, with(p, v))
			case nulSweepIsType(prop.Type, "object"):
				// An object of declared fields (a cursor) takes its first
				// text field; a free one (metadata) a key of its own.
				k := "k"
				inner := make([]string, 0, len(prop.Properties))
				for ip, raw := range prop.Properties {
					var ps struct {
						Type any `json:"type"`
					}
					if json.Unmarshal(raw, &ps) == nil && nulSweepIsType(ps.Type, "string") {
						inner = append(inner, ip)
					}
				}
				sort.Strings(inner)
				if len(inner) > 0 {
					k = inner[0]
				}
				add(p+"."+k, with(p, map[string]any{k: m}))
			case nulSweepIsType(prop.Type, "array") && prop.Items != nil && nulSweepIsType(prop.Items.Type, "string"):
				add(p+"[0]", with(p, []any{m}))
			case nulSweepIsType(prop.Type, "array") && prop.Items != nil && nulSweepIsType(prop.Items.Type, "object"):
				inner := make([]string, 0, len(prop.Items.Properties))
				for ip := range prop.Items.Properties {
					inner = append(inner, ip)
				}
				sort.Strings(inner)
				elem := map[string]any{}
				if list, ok := args[p].([]any); ok && len(list) > 0 {
					if e, ok := list[0].(map[string]any); ok {
						elem = e
					}
				}
				for _, ip := range inner {
					var raw struct {
						Type any `json:"type"`
					}
					_ = json.Unmarshal(prop.Items.Properties[ip], &raw)
					if !nulSweepIsType(raw.Type, "string") {
						continue
					}
					e := map[string]any{}
					for k, x := range elem {
						e[k] = x
					}
					e[ip] = m
					add(p+"[0]: "+ip, with(p, []any{e}))
				}
			}
		}
	}
	return out, missing
}
