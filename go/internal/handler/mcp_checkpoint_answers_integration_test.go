//go:build integration

// The answers of the MCP store, get and search tools that the Hermes plugin
// ctx_checkpoint parses (integrations/hermes/plugin/ctx_checkpoint/memory/
// mcp_client.py) are pinned against ONE fixture that both sides read:
// integrations/hermes/plugin/ctx_checkpoint/memory/tests/mcp_answers.json.
// This test drives the real handlers over the production write path and
// compares their answers with the fixture; the plugin's suites feed the same
// fixture to its parser. A wording change here, or a fixture edit to suit the
// parser, turns the other side red (GottZ/ctx#290).
//
// The plugin depends on two store answers in particular: the "(id: <uuid>"
// group of "Stored: …" is the block id of every checkpoint part, manifest and
// head (the title in front of it carries a UUID-shaped root session id), and
// "No change needed — … (id: <uuid>)" is how a retry reuses a part it already
// stored (plugin 0.10.0, action noop).
//
// Run with:
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestMCPAnswerFormsMatchCheckpointPluginFixture' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// checkpointAnswersFixture is the repository path of the shared fixture,
// relative to this package directory (go/internal/handler).
const checkpointAnswersFixture = "../../../integrations/hermes/plugin/ctx_checkpoint/memory/tests/mcp_answers.json"

type checkpointAnswers struct {
	Write struct {
		Category       string         `json:"category"`
		Title          string         `json:"title"`
		Content        string         `json:"content"`
		ChangedContent string         `json:"changed_content"`
		Sensitivity    string         `json:"sensitivity"`
		Type           string         `json:"type"`
		Tags           []string       `json:"tags"`
		Metadata       map[string]any `json:"metadata"`
	} `json:"write"`
	Store struct {
		Stored   string `json:"stored"`
		NoChange string `json:"no_change"`
	} `json:"store"`
	Get struct {
		Keys []string `json:"keys"`
	} `json:"get"`
	Search struct {
		Query   string   `json:"query"`
		Tags    []string `json:"tags"`
		Types   []string `json:"types"`
		RowKeys []string `json:"row_keys"`
		// TypesRefused is the answer of a server before #342 to Types; the
		// plugins fall back on exactly this text.
		TypesRefused string `json:"types_refused"`
	} `json:"search"`
}

func loadCheckpointAnswers(t *testing.T) checkpointAnswers {
	t.Helper()
	raw, err := os.ReadFile(filepath.FromSlash(checkpointAnswersFixture))
	if err != nil {
		t.Fatalf("read the shared fixture %s: %v", checkpointAnswersFixture, err)
	}
	var fx checkpointAnswers
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse %s: %v", checkpointAnswersFixture, err)
	}
	if fx.Store.Stored == "" || fx.Store.NoChange == "" || fx.Write.Title == "" || len(fx.Get.Keys) == 0 ||
		len(fx.Search.RowKeys) == 0 || len(fx.Search.Types) == 0 || fx.Search.TypesRefused == "" {
		t.Fatalf("%s lacks a template, the write or the key lists: %+v", checkpointAnswersFixture, fx)
	}
	return fx
}

// renderCheckpointAnswer fills the {title}, {id} and {category} placeholders
// of a store template.
func renderCheckpointAnswer(template, title, id, category string) string {
	return strings.NewReplacer("{title}", title, "{id}", id, "{category}", category).Replace(template)
}

func TestMCPAnswerFormsMatchCheckpointPluginFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	fx := loadCheckpointAnswers(t)
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}}
	_, plain, err := store.CreateApiKey(ctx, pool, "i290-checkpoint-answers", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)
	w := fx.Write

	storeOnce := func(content string) string {
		t.Helper()
		r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{
			Category:    w.Category,
			Title:       w.Title,
			Content:     content,
			Tags:        w.Tags,
			Metadata:    w.Metadata,
			Sensitivity: w.Sensitivity,
			Type:        w.Type,
		})
		if err != nil {
			t.Fatalf("mcp store: protocol error %v", err)
		}
		text := resultText(t, r)
		if r.IsError {
			t.Fatalf("mcp store rejected: %s", text)
		}
		return text
	}
	blockID := func() string {
		t.Helper()
		var id string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM context_blocks WHERE category = $1 AND title = $2 AND NOT is_archived`,
			w.Category, w.Title).Scan(&id); err != nil {
			t.Fatalf("read the stored block: %v", err)
		}
		return id
	}

	// Created, updated (same title, new content) and identical: the three
	// answers a checkpoint part, manifest or head can get.
	created := storeOnce(w.Content)
	id := blockID()
	if want := renderCheckpointAnswer(fx.Store.Stored, w.Title, id, w.Category); created != want {
		t.Errorf("store (created) answered\n  %q\nthe fixture says\n  %q", created, want)
	}
	updated := storeOnce(w.ChangedContent)
	if got := blockID(); got != id {
		t.Fatalf("the update made a new block: %s, was %s", got, id)
	}
	if want := renderCheckpointAnswer(fx.Store.Stored, w.Title, id, w.Category); updated != want {
		t.Errorf("store (updated) answered\n  %q\nthe fixture says\n  %q", updated, want)
	}
	identical := storeOnce(w.ChangedContent)
	if want := renderCheckpointAnswer(fx.Store.NoChange, w.Title, id, w.Category); identical != want {
		t.Errorf("store (identical) answered\n  %q\nthe fixture says\n  %q", identical, want)
	}

	// get: one JSON object with the keys the chain reads, the metadata as an
	// object carrying what the write put there.
	r, _, err := mcpGetHandler(mcpCfg)(keyCtx, nil, getInput{ID: id})
	if err != nil || r.IsError {
		t.Fatalf("mcp get: err=%v answer=%s", err, resultText(t, r))
	}
	var block map[string]any
	if err := json.Unmarshal([]byte(resultText(t, r)), &block); err != nil {
		t.Fatalf("get answer is not one JSON object: %v", err)
	}
	for _, key := range fx.Get.Keys {
		if _, ok := block[key]; !ok {
			t.Errorf("get answer lacks %q (keys the plugin reads: %v)", key, fx.Get.Keys)
		}
	}
	if block["id"] != id || block["title"] != w.Title || block["category"] != w.Category || block["type"] != w.Type {
		t.Errorf("get answer id/title/category/type = %v/%v/%v/%v, want %s/%s/%s/%s",
			block["id"], block["title"], block["category"], block["type"], id, w.Title, w.Category, w.Type)
	}
	meta, _ := block["metadata"].(map[string]any)
	for key, want := range w.Metadata {
		if got, _ := json.Marshal(meta[key]); string(got) != mustJSON(t, want) {
			t.Errorf("get answer metadata.%s = %s, want %s", key, got, mustJSON(t, want))
		}
	}

	// search (the head lookup: query = the title, category, tag, limit 10,
	// types since #342): a JSON array of rows, the head among them. The same
	// search without types is the plugins' fallback for a server before #342
	// and answers the head as well.
	for _, types := range [][]string{fx.Search.Types, nil} {
		r, _, err = mcpSearchHandler(mcpCfg)(keyCtx, nil, searchInput{
			Query:    fx.Search.Query,
			Category: w.Category,
			Tags:     fx.Search.Tags,
			Types:    types,
			Limit:    10,
		})
		if err != nil || r.IsError {
			t.Fatalf("mcp search types=%v: err=%v answer=%s", types, err, resultText(t, r))
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(resultText(t, r)), &rows); err != nil {
			t.Fatalf("search answer is not a JSON array: %v", err)
		}
		found := false
		for _, row := range rows {
			for _, key := range fx.Search.RowKeys {
				if _, ok := row[key]; !ok {
					t.Errorf("search row lacks %q (keys the plugin reads: %v): %v", key, fx.Search.RowKeys, row)
				}
			}
			if row["id"] == id && row["title"] == w.Title && row["type"] == w.Type {
				found = true
			}
		}
		if !found {
			t.Errorf("search types=%v for the head title did not return the head %s: %v", types, id, rows)
		}
	}

	// The refusal the plugins fall back on: what the search tool answered to
	// these types before #342. The query tool keeps that check
	// (resolveMCPVisibleTypes), so its answer is the server's own wording.
	r, _, err = mcpQueryHandler(mcpCfg)(keyCtx, nil, queryInput{Question: fx.Search.Query, Types: fx.Search.Types})
	if err != nil || !r.IsError {
		t.Fatalf("mcp query types=%v: err=%v, IsError=%t (%s) — want the refusal", fx.Search.Types, err, r != nil && r.IsError, resultText(t, r))
	}
	if got := resultText(t, r); got != fx.Search.TypesRefused {
		t.Errorf("the refusal of types=%v is\n  %q\nthe fixture says\n  %q", fx.Search.Types, got, fx.Search.TypesRefused)
	}
}

// checkpointOriginAnswers is the fixture's "origin" section (GottZ/ctx#295):
// the untrusted_origin record (#280) as the plugin's recall reads it.
type checkpointOriginAnswers struct {
	Origin struct {
		Note  string `json:"note"`
		Value string `json:"value"`
		Write struct {
			Category string `json:"category"`
			Title    string `json:"title"`
			Content  string `json:"content"`
			Type     string `json:"type"`
			Retype   string `json:"retype"`
		} `json:"write"`
		GetKeys       []string `json:"get_keys"`
		SearchRowKeys []string `json:"search_row_keys"`
		Query         struct {
			Answer json.RawMessage `json:"answer"`
			Text   string          `json:"text"`
		} `json:"query"`
		Retrieval struct {
			Answer json.RawMessage `json:"answer"`
			Text   string          `json:"text"`
		} `json:"retrieval"`
	} `json:"origin"`
}

// TestMCPAnswerFormsCarryTheUntrustedOriginForCheckpointPlugin pins the
// fixture's origin section: the note wording, get and search over the real
// write, re-type and read handlers, and the query tool's two renderings of an
// /api/query answer (synthesis text with superseded_by tails and note lines,
// retrieval-only JSON) — the texts the plugin's parser is fed.
func TestMCPAnswerFormsCarryTheUntrustedOriginForCheckpointPlugin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	raw, err := os.ReadFile(filepath.FromSlash(checkpointAnswersFixture))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fx checkpointOriginAnswers
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse the shared fixture: %v", err)
	}
	o := fx.Origin
	if o.Note == "" || o.Value == "" || o.Write.Title == "" || o.Write.Retype == "" || o.Query.Text == "" || o.Retrieval.Text == "" {
		t.Fatalf("the fixture lacks the origin section: %+v", o)
	}

	if want := strings.ReplaceAll(o.Note, "{origin}", o.Value); store.UntrustedOriginNote(o.Value) != want {
		t.Errorf("UntrustedOriginNote(%q) = %q, the fixture says %q", o.Value, store.UntrustedOriginNote(o.Value), want)
	}

	// The query tool's two renderings of a canned /api/query answer.
	if text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: string(o.Query.Answer)}, `{"question":"q"}`); isErr || text != o.Query.Text {
		t.Errorf("query (synthesis) answered (error %v)\n%s\nthe fixture says\n%s", isErr, text, o.Query.Text)
	}
	retrievalArgs := `{"question":"q","synthesize":false,"include_content":true}`
	if text, isErr := runMCPQuery(t, &cannedQueryDelegate{body: string(o.Retrieval.Answer)}, retrievalArgs); isErr || text != o.Retrieval.Text {
		t.Errorf("query (retrieval-only) answered (error %v)\n%s\nthe fixture says\n%s", isErr, text, o.Retrieval.Text)
	}

	// get and search over the production write path: stored in an untrusted
	// type, re-typed into a trusted one over MCP update.
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	mcpCfg := MCPConfig{Pool: pool, Blocktypes: reg, Cfg: staticConfigStore{cfg: &config.Config{
		Query:  config.QueryConfig{RateLimitWrite: 0},
		Pool:   config.PoolConfig{DefaultBlockSensitivity: backends.SensPublic},
		Writes: config.WritesConfig{ConfirmTTL: 10 * time.Minute},
	}}}
	_, plain, err := store.CreateApiKey(ctx, pool, "i295-origin-answers", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create key: %v", err)
	}
	keyCtx := mcpAuthCtx(t, ctx, pool, plain)
	w := o.Write
	r, _, err := mcpStoreHandler(mcpCfg)(keyCtx, nil, storeInput{Category: w.Category, Title: w.Title, Content: w.Content, Type: w.Type})
	if err != nil || r.IsError {
		t.Fatalf("mcp store: err=%v answer=%s", err, resultText(t, r))
	}
	var id string
	if err := pool.QueryRow(ctx,
		`SELECT id::text FROM context_blocks WHERE category = $1 AND title = $2 AND NOT is_archived`,
		w.Category, w.Title).Scan(&id); err != nil {
		t.Fatalf("read the stored block: %v", err)
	}
	r, _, err = mcpUpdateHandler(mcpCfg)(keyCtx, nil, updateInput{ID: id, Type: w.Retype})
	if err != nil || r.IsError {
		t.Fatalf("mcp update type %s: err=%v answer=%s", w.Retype, err, resultText(t, r))
	}

	r, _, err = mcpGetHandler(mcpCfg)(keyCtx, nil, getInput{ID: id})
	if err != nil || r.IsError {
		t.Fatalf("mcp get: err=%v answer=%s", err, resultText(t, r))
	}
	var block map[string]any
	if err := json.Unmarshal([]byte(resultText(t, r)), &block); err != nil {
		t.Fatalf("get answer is not one JSON object: %v", err)
	}
	for _, key := range o.GetKeys {
		if _, ok := block[key]; !ok {
			t.Errorf("get answer lacks %q (keys the plugin reads: %v)", key, o.GetKeys)
		}
	}
	if block["untrusted_origin"] != o.Value || block["type"] != w.Retype {
		t.Errorf("get answer type/untrusted_origin = %v/%v, want %s/%s", block["type"], block["untrusted_origin"], w.Retype, o.Value)
	}

	r, _, err = mcpSearchHandler(mcpCfg)(keyCtx, nil, searchInput{Query: w.Title, Category: w.Category, Limit: 10})
	if err != nil || r.IsError {
		t.Fatalf("mcp search: err=%v answer=%s", err, resultText(t, r))
	}
	var rows []map[string]any
	if err := json.Unmarshal([]byte(resultText(t, r)), &rows); err != nil {
		t.Fatalf("search answer is not a JSON array: %v", err)
	}
	var row map[string]any
	for _, candidate := range rows {
		if candidate["id"] == id {
			row = candidate
		}
	}
	if row == nil {
		t.Fatalf("search did not return the re-typed block %s: %v", id, rows)
	}
	for _, key := range o.SearchRowKeys {
		if _, ok := row[key]; !ok {
			t.Errorf("search row lacks %q (keys the plugin reads: %v): %v", key, o.SearchRowKeys, row)
		}
	}
	if row["untrusted_origin"] != o.Value {
		t.Errorf("search row untrusted_origin = %v, want %s", row["untrusted_origin"], o.Value)
	}
}
