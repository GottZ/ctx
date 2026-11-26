//go:build integration

// Issue #555, point 7: an MCP tool whose statement fails in the database
// answered with the database's text — the statement, the table, the SQLSTATE
// (`store failed: store: upsert block: ERROR: … (SQLSTATE …)`, `search
// failed: …`, `cannot resolve block id: …`). Every tool is called here with
// valid arguments over a pool whose every statement fails in the database
// (its search_path names no ctx table: 42P01, the relation's name in the
// text): no answer carries the database's text, and the cause stands in a
// log line instead.
package handler

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// dbTextPattern matches what a database error puts into a text.
var dbTextPattern = regexp.MustCompile(`SQLSTATE|ERROR:|context_[a-z_]+|relation "|does not exist`)

type recordedLines struct {
	mu    sync.Mutex
	lines []string
}

func (r *recordedLines) Enabled(context.Context, slog.Level) bool { return true }
func (r *recordedLines) WithAttrs([]slog.Attr) slog.Handler       { return r }
func (r *recordedLines) WithGroup(string) slog.Handler            { return r }
func (r *recordedLines) Handle(_ context.Context, rec slog.Record) error {
	var b strings.Builder
	b.WriteString(rec.Level.String() + " " + rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		b.WriteString(" " + a.Key + "=" + a.Value.String())
		return true
	})
	r.mu.Lock()
	r.lines = append(r.lines, b.String())
	r.mu.Unlock()
	return nil
}

func (r *recordedLines) since(n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.lines[n:]...)
}

func (r *recordedLines) n() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.lines)
}

func TestMCPToolsKeepTheDatabaseTextOutOfTheirAnswers(t *testing.T) {
	pool, dsn := testdb.SetupTestDBWithDSN(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	ar, err := auth.Authenticate(ctx, pool, abortKey(t, pool))
	if err != nil || !ar.IsValid {
		t.Fatalf("authenticate: %v", err)
	}
	pc, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	pc.ConnConfig.RuntimeParams["search_path"] = "pg_catalog" // no ctx table is found: every statement fails (42P01)
	broken, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(broken.Close)

	lines := &recordedLines{}
	prev := slog.Default()
	slog.SetDefault(slog.New(lines))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfgStore := staticConfigStore{cfg: &config.Config{
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute, BatchMaxItems: 100},
	}}
	inner := NewMCPHandler(MCPConfig{Pool: broken, Cfg: cfgStore, Blocktypes: reg})
	srv, served := abortServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inner.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authResultKey, ar)))
	}), nil)
	go func() { // the server reports each request on a channel of four
		for range served {
		}
	}()

	const fullID = "01a21054-0000-7000-8000-000000000555"
	const prefix = "01a21054"
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"store", map[string]any{"category": "learnings", "title": "t555", "content": "c"}},
		{"store_batch", map[string]any{"items": []any{map[string]any{"category": "learnings", "title": "t555", "content": "c"}}}},
		{"search", map[string]any{"query": "anything"}},
		{"get", map[string]any{"id": fullID}},
		{"get", map[string]any{"id": prefix}},
		{"recent", map[string]any{}},
		{"update", map[string]any{"id": prefix, "title": "renamed"}},
		{"update", map[string]any{"id": fullID, "title": "renamed"}},
		{"archive", map[string]any{"id": prefix}},
		{"archive", map[string]any{"id": fullID}},
		{"blob_fetch", map[string]any{"id": fullID}},
		{"blob_store", map[string]any{"category": "files", "title": "b555", "filename": "f.txt", "mime_type": "text/plain", "text": "x"}},
		{"blob_link", map[string]any{"id": fullID, "context_block_id": fullID}},
		{"guard_list", map[string]any{}},
		{"guard_resolve", map[string]any{"ids": []string{fullID}, "resolution": "keep"}},
		{"issue_create", map[string]any{"title": "i555", "content": "x"}},
		{"issue_comment", map[string]any{"issue_id": fullID, "content": "x"}},
		{"issue_state", map[string]any{"issue_id": fullID, "status": "closed"}},
		{"confirm", map[string]any{"payload_hash": strings.Repeat("ab", 32)}},
	}
	for _, c := range cases {
		before := lines.n()
		text, isErr := mcpCallText(t, srv.URL, c.tool, c.args)
		logged := lines.since(before)
		t.Logf("%s %v: isError=%v %.200q", c.tool, c.args["id"], isErr, text)
		if strings.HasPrefix(text, "validating \"arguments\"") {
			t.Fatalf("%s: the arguments do not fit the tool's schema: %s", c.tool, text)
		}
		if dbTextPattern.MatchString(text) {
			t.Errorf("%s answered with the database's text: %.300q", c.tool, text)
		}
		if !isErr {
			t.Errorf("%s answered without an error although every statement fails: %.200q", c.tool, text)
		}
		// The tool's own failure line, at ERROR: a WARN of an earlier read
		// (the grant resolve of the read tools) names the cause too, and is
		// not the line of the answer's fault.
		var cause bool
		for _, l := range logged {
			if strings.HasPrefix(l, "ERROR ") && (strings.Contains(l, "42P01") || strings.Contains(l, "does not exist")) {
				cause = true
			}
		}
		if !cause {
			t.Errorf("%s: no ERROR line names the database's cause (%d lines: %v)", c.tool, len(logged), logged)
		}
	}
}

// mcpCallText calls one tool and returns the answer's text and isError.
func mcpCallText(t *testing.T, url, tool string, args map[string]any) (string, bool) {
	t.Helper()
	resp, err := http.DefaultClient.Do(mcpToolRequest(t, context.Background(), url, "", tool, args))
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)
	if i := strings.Index(body, "data: "); i >= 0 {
		body = body[i+6:]
		if j := strings.Index(body, "\n"); j >= 0 {
			body = body[:j]
		}
	}
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
	if err := json.Unmarshal([]byte(body), &rpc); err != nil {
		t.Fatalf("%s: answer %d %s (%v)", tool, resp.StatusCode, raw, err)
	}
	if rpc.Error != nil {
		return "jsonrpc error: " + rpc.Error.Message, true
	}
	text := ""
	if len(rpc.Result.Content) > 0 {
		text = rpc.Result.Content[0].Text
	}
	return text, rpc.Result.IsError
}
