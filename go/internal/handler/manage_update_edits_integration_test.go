//go:build integration

// Issue #406, REST surface: `manage update` takes `edits` (an ordered list of
// {old, new, replace_all}) and `metadata_merge` (a shallow merge, null removes
// a key) beside the replacement fields, and hands both to store.UpdateBlock.
// The bodies are raw JSON, as a REST client sends them.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestManageUpdateEdits -count=1 -v
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
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5/pgxpool"
)

// i406Fixture is the shared setup of the issue #406 handler tests: a real
// database, the registry, a direct key and a confirm_writes key, and the
// handlers of every update door.
type i406Fixture struct {
	ctx        context.Context
	pool       *pgxpool.Pool
	mcpCfg     MCPConfig
	manage     *ManageHandler
	directCtx  context.Context
	flaggedCtx context.Context
	directAR   *auth.AuthResult
	flaggedKey string
}

// i406Row is a block as the assertions read it.
type i406Row struct {
	content  string
	metadata map[string]any
	updated  time.Time
}

func newI406Fixture(t *testing.T) *i406Fixture {
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
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	f := &i406Fixture{ctx: ctx, pool: pool}
	f.mcpCfg = MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg}
	f.manage = NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	_, directPlain, err := store.CreateApiKey(ctx, pool, "i406-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "i406-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	f.flaggedKey = flaggedRow.ID
	f.directCtx = mcpAuthCtx(t, ctx, pool, directPlain)
	f.flaggedCtx = mcpAuthCtx(t, ctx, pool, flaggedPlain)
	f.directAR = AuthResultFromContext(f.directCtx)
	return f
}

// block writes a block through the MCP store tool (the production write path)
// and returns its id.
func (f *i406Fixture) block(t *testing.T, title, content string, metadata map[string]any) string {
	t.Helper()
	r, _, err := mcpStoreHandler(f.mcpCfg)(f.directCtx, nil, storeInput{Category: "i406", Title: title, Content: content, Metadata: metadata})
	if err != nil || r.IsError {
		t.Fatalf("fixture store %s: %v %v", title, err, r)
	}
	var id string
	if err := f.pool.QueryRow(f.ctx, `SELECT id::text FROM context_blocks WHERE category = 'i406' AND title = $1 AND NOT is_archived`, title).Scan(&id); err != nil {
		t.Fatalf("fixture id: %v", err)
	}
	return id
}

func (f *i406Fixture) row(t *testing.T, id string) i406Row {
	t.Helper()
	var r i406Row
	if err := f.pool.QueryRow(f.ctx, `SELECT content, metadata, updated_at FROM context_blocks WHERE id = $1`, id).
		Scan(&r.content, &r.metadata, &r.updated); err != nil {
		t.Fatalf("read block: %v", err)
	}
	return r
}

// restUpdate runs manage update with a raw JSON data object and returns the
// status and the decoded body.
func (f *i406Fixture) restUpdate(t *testing.T, id, data string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/manage", nil).WithContext(f.ctx)
	f.manage.handleUpdate(rec, req, f.directAR, manageRequest{Action: "update", ID: id, Data: json.RawMessage(data)})
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
	}
	return rec.Code, body
}

func TestManageUpdateEdits(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newI406Fixture(t)

	t.Run("edits apply in order", func(t *testing.T) {
		id := f.block(t, "rest-apply", "alpha beta gamma", nil)
		status, body := f.restUpdate(t, id, `{"edits":[{"old":"beta","new":"BETA"},{"old":"BETA gamma","new":"done"}]}`)
		if status != http.StatusOK || body["success"] != true {
			t.Fatalf("status %d body %v", status, body)
		}
		if got := f.row(t, id).content; got != "alpha done" {
			t.Fatalf("content = %q, want %q", got, "alpha done")
		}
		block, _ := body["block"].(map[string]any)
		if block["content"] != "alpha done" {
			t.Fatalf("answer block content = %v", block["content"])
		}
	})

	t.Run("replace_all", func(t *testing.T) {
		id := f.block(t, "rest-all", "x, x and x", nil)
		status, body := f.restUpdate(t, id, `{"edits":[{"old":"x","new":"y","replace_all":true}]}`)
		if status != http.StatusOK || f.row(t, id).content != "y, y and y" {
			t.Fatalf("status %d body %v content %q", status, body, f.row(t, id).content)
		}
	})

	for _, tc := range []struct {
		name, data, wantIn string
	}{
		{"no match", `{"edits":[{"old":"alpha","new":"A"},{"old":"absent","new":"x"}]}`, "edits[1]: old matches 0 times"},
		{"two matches", `{"edits":[{"old":"twice","new":"x"}]}`, "edits[0]: old matches 2 times"},
	} {
		t.Run("refused edit: "+tc.name, func(t *testing.T) {
			id := f.block(t, "rest-"+tc.name, "alpha twice twice", map[string]any{"k": "v"})
			before := f.row(t, id)
			status, body := f.restUpdate(t, id, strings.TrimSuffix(tc.data, "}")+`,"metadata_merge":{"k":"changed"}}`)
			if status != http.StatusConflict || body["code"] != "edit_mismatch" {
				t.Fatalf("status %d body %v, want 409 edit_mismatch", status, body)
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.wantIn) {
				t.Fatalf("error %q does not contain %q", msg, tc.wantIn)
			}
			after := f.row(t, id)
			if after.content != before.content || after.metadata["k"] != "v" || !after.updated.Equal(before.updated) {
				t.Fatalf("a refused update wrote: %+v (before %+v)", after, before)
			}
		})
	}

	for _, tc := range []struct {
		name, data, wantIn string
	}{
		{"content with edits", `{"content":"whole","edits":[{"old":"seed","new":"x"}]}`, "content and edits"},
		{"metadata with metadata_merge", `{"metadata":{},"metadata_merge":{"k":1}}`, "metadata and metadata_merge"},
		{"empty old", `{"edits":[{"old":"","new":"x"}]}`, "edits[0]: old is empty"},
		{"missing new", `{"edits":[{"old":"seed"}]}`, "edits[0]: new is missing"},
		{"empty edit list", `{"edits":[]}`, "at least one edit"},
	} {
		t.Run("refused patch: "+tc.name, func(t *testing.T) {
			id := f.block(t, "rest-patch-"+tc.name, "seed", nil)
			status, body := f.restUpdate(t, id, tc.data)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d body %v, want 400", status, body)
			}
			if msg, _ := body["error"].(string); !strings.Contains(msg, tc.wantIn) {
				t.Fatalf("error %q does not contain %q", msg, tc.wantIn)
			}
			if f.row(t, id).content != "seed" {
				t.Fatal("a refused patch wrote")
			}
		})
	}

	t.Run("an edit past the size cap", func(t *testing.T) {
		id := f.block(t, "rest-cap", "seed", nil)
		big, _ := json.Marshal(strings.Repeat("x", 50*1024+1))
		status, body := f.restUpdate(t, id, `{"edits":[{"old":"seed","new":`+string(big)+`}]}`)
		if status != http.StatusRequestEntityTooLarge || body["code"] != "size_cap" {
			t.Fatalf("status %d body %v, want 413 size_cap", status, body)
		}
		if f.row(t, id).content != "seed" {
			t.Fatal("an edit past the cap wrote")
		}
	})

	t.Run("metadata_merge: set, overwrite, null removes, the rest stays", func(t *testing.T) {
		id := f.block(t, "rest-merge", "body", map[string]any{"source": "s", "over": "old", "drop": "d"})
		status, body := f.restUpdate(t, id, `{"metadata_merge":{"over":"new","add":[1,2],"drop":null}}`)
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		m := f.row(t, id).metadata
		if m["source"] != "s" || m["over"] != "new" || m["add"] == nil {
			t.Fatalf("metadata = %v", m)
		}
		if _, ok := m["drop"]; ok {
			t.Fatalf("null did not remove the key: %v", m)
		}
	})

	t.Run("metadata_merge with the provenance key is refused like a replacement", func(t *testing.T) {
		id := f.block(t, "rest-merge-prov", "body", nil)
		for _, data := range []string{
			`{"metadata_merge":{"provenance":{"v":1}}}`,
			`{"metadata_merge":{"provenance":null}}`,
		} {
			status, body := f.restUpdate(t, id, data)
			if status != http.StatusForbidden || body["code"] != "reserved_metadata" {
				t.Fatalf("%s: status %d body %v, want 403 reserved_metadata", data, status, body)
			}
		}
	})

	// Edits are a content write: the afterwork extracts the dates of the
	// edited text, as it does for a new content.
	t.Run("edits run the temporal afterwork", func(t *testing.T) {
		id := f.block(t, "rest-dates", "meeting on 2026-01-05 in the hall", nil)
		status, body := f.restUpdate(t, id, `{"edits":[{"old":"2026-01-05","new":"2026-03-09"}]}`)
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		var times []time.Time
		if err := f.pool.QueryRow(f.ctx, `SELECT COALESCE(content_times, '{}') FROM context_blocks WHERE id = $1`, id).Scan(&times); err != nil {
			t.Fatal(err)
		}
		found := false
		for _, ts := range times {
			if ts.UTC().Format("2006-01-02") == "2026-03-09" {
				found = true
			}
			if ts.UTC().Format("2006-01-02") == "2026-01-05" {
				t.Fatalf("content_times %v still holds the replaced date", times)
			}
		}
		if !found {
			t.Fatalf("content_times %v lacks the edited date", times)
		}
	})

	t.Run("an update without the new fields answers as before", func(t *testing.T) {
		id := f.block(t, "rest-plain", "body", map[string]any{"a": 1})
		status, body := f.restUpdate(t, id, `{"content":"new body","metadata":{"b":2}}`)
		if status != http.StatusOK {
			t.Fatalf("status %d body %v", status, body)
		}
		r := f.row(t, id)
		if r.content != "new body" || len(r.metadata) != 1 || r.metadata["b"] != 2.0 {
			t.Fatalf("row = %+v, want the replacement", r)
		}
	})
}
