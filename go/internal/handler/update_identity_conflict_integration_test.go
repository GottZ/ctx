//go:build integration

// Issue #227: an update by id that gives a block the (category, title, scope)
// of another ACTIVE block runs into uq_context_category_title_scope (23505).
// Before the fix every caller of store.UpdateBlock answered that as a server
// fault: REST manage update 500 "Internal server error", the MCP `update` tool
// "update failed: internal error", a confirmed staged update "confirmed update
// failed to execute" — each with an ERROR line in the log. The collision is an
// input error the caller resolves by picking another category or title, so
// every surface now answers it as a 409 of the constraint class, names the
// identity and the block that holds it, and writes no ERROR line.
//
// The surfaces are the four doors to store.UpdateBlock: REST manage `update`,
// the MCP `update` tool, and a staged update confirmed over MCP `confirm` and
// over HTTP POST /api/confirm. Every block is written through the MCP `store`
// tool. The scope move exists on REST manage update only (the MCP tool carries
// no scope field).
//
// The fault probe is the counter-check: a 23505 on any OTHER constraint is not
// a collision this change knows about and stays the opaque 500 with its ERROR
// line. A test trigger raises it, because no other unique constraint of
// context_blocks is reachable from UpdateBlock.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestUpdateIdentityConflict -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// i227LogBuf is a log sink that tolerates the writes of background goroutines.
type i227LogBuf struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *i227LogBuf) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// take returns everything logged since the previous take and empties the sink.
func (b *i227LogBuf) take() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.buf.String()
	b.buf.Reset()
	return s
}

// i227ErrorLines returns the ERROR lines of a text-handler log.
func i227ErrorLines(log string) []string {
	var out []string
	for _, l := range strings.Split(log, "\n") {
		if strings.Contains(l, "level=ERROR") {
			out = append(out, l)
		}
	}
	return out
}

// i227Answer is one surface's answer to an update, reduced to what the
// assertions read.
type i227Answer struct {
	ok     bool
	status int    // HTTP status (REST surfaces); 0 on MCP
	code   string // machine code: REST body "code", MCP structuredContent.code
	text   string // REST body "error" or the MCP text
}

func TestUpdateIdentityConflict(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
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

	mcpCfg := MCPConfig{Pool: pool, Cfg: cfgStore, Blocktypes: reg}
	storeTool := mcpStoreHandler(mcpCfg)
	updateTool := mcpUpdateHandler(mcpCfg)
	confirmTool := mcpConfirmHandler(mcpCfg)
	manage := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	confirmHTTP := NewConfirmHandler(pool, reg)

	// Default-tenant keys: home 'private', allowed_scopes '{shared}', so both
	// scopes are writable and readable for both keys.
	_, directPlain, err := store.CreateApiKey(ctx, pool, "i227-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "i227-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	directCtx := mcpAuthCtx(t, ctx, pool, directPlain)
	flaggedCtx := mcpAuthCtx(t, ctx, pool, flaggedPlain)
	directAR := AuthResultFromContext(directCtx)

	logs := &i227LogBuf{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	mkBlock := func(t *testing.T, category, title, scope string) string {
		t.Helper()
		r, _, err := storeTool(directCtx, nil, storeInput{Category: category, Title: title, Content: "i227 content of " + title, Scope: scope})
		if err != nil || r.IsError {
			t.Fatalf("fixture store %s/%s@%s failed: %v %v", category, title, scope, err, r)
		}
		var id string
		if err := pool.QueryRow(ctx,
			`SELECT id::text FROM context_blocks WHERE category = $1 AND title = $2 AND scope = $3 AND NOT is_archived`,
			category, title, scope).Scan(&id); err != nil {
			t.Fatalf("fixture id: %v", err)
		}
		return id
	}
	type ident struct{ category, title, scope string }
	identOf := func(t *testing.T, id string) ident {
		t.Helper()
		var r ident
		if err := pool.QueryRow(ctx,
			`SELECT category, title, scope FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&r.category, &r.title, &r.scope); err != nil {
			t.Fatalf("read block identity: %v", err)
		}
		return r
	}

	mcpAnswer := func(r *mcp.CallToolResult) i227Answer {
		a := i227Answer{ok: !r.IsError, text: resultText(t, r)}
		if env, ok := r.StructuredContent.(mcpErrorEnvelope); ok {
			a.code = env.Code
		}
		return a
	}
	restAnswer := func(t *testing.T, rec *httptest.ResponseRecorder) i227Answer {
		t.Helper()
		var resp map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode response: %v (body %s)", err, rec.Body.String())
		}
		a := i227Answer{status: rec.Code}
		a.ok, _ = resp["success"].(bool)
		a.code, _ = resp["code"].(string)
		a.text, _ = resp["error"].(string)
		return a
	}
	// stage runs the update through the flagged key and returns the hash.
	stage := func(t *testing.T, in updateInput) string {
		t.Helper()
		r, _, err := updateTool(flaggedCtx, nil, in)
		if err != nil {
			t.Fatalf("stage update: %v", err)
		}
		m := hashRe.FindStringSubmatch(resultText(t, r))
		if !r.IsError || m == nil {
			t.Fatalf("update of a confirm_writes key was not staged: %s", resultText(t, r))
		}
		return m[1]
	}

	type surface struct {
		name      string
		scopeMove bool // the surface can carry a scope
		update    func(t *testing.T, id string, in updateInput, scope *string) i227Answer
	}
	surfaces := []surface{
		{"rest_manage_update", true, func(t *testing.T, id string, in updateInput, scope *string) i227Answer {
			t.Helper()
			data := map[string]any{}
			if in.Category != nil {
				data["category"] = *in.Category
			}
			if in.Title != nil {
				data["title"] = *in.Title
			}
			if scope != nil {
				data["scope"] = *scope
			}
			raw, err := json.Marshal(data)
			if err != nil {
				t.Fatalf("marshal update data: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
			manage.handleUpdate(rec, req, directAR, manageRequest{Action: "update", ID: id, Data: raw})
			return restAnswer(t, rec)
		}},
		{"mcp_update", false, func(t *testing.T, id string, in updateInput, _ *string) i227Answer {
			t.Helper()
			in.ID = id
			r, _, err := updateTool(directCtx, nil, in)
			if err != nil {
				t.Fatalf("mcp update: %v", err)
			}
			return mcpAnswer(r)
		}},
		{"mcp_confirm", false, func(t *testing.T, id string, in updateInput, _ *string) i227Answer {
			t.Helper()
			in.ID = id
			hash := stage(t, in)
			r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: hash})
			if err != nil {
				t.Fatalf("mcp confirm: %v", err)
			}
			return mcpAnswer(r)
		}},
		{"http_confirm", false, func(t *testing.T, id string, in updateInput, _ *string) i227Answer {
			t.Helper()
			in.ID = id
			hash := stage(t, in)
			body, err := json.Marshal(confirmRequest{PayloadHash: hash})
			if err != nil {
				t.Fatalf("marshal confirm: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(body)).WithContext(flaggedCtx)
			confirmHTTP.HandleConfirm(rec, req)
			return restAnswer(t, rec)
		}},
	}

	const conflictPhrase = "another active block in this scope already has this category and title"
	wantConflict := func(t *testing.T, s surface, got i227Answer, holderID string, errLines []string) {
		t.Helper()
		if got.ok {
			t.Fatalf("collision answered success: %+v", got)
		}
		if s.name == "rest_manage_update" || s.name == "http_confirm" {
			if got.status != http.StatusConflict {
				t.Errorf("status = %d, want 409 (answer %+v)", got.status, got)
			}
		}
		if got.code != "constraint" {
			t.Errorf("code = %q, want %q (answer %+v)", got.code, "constraint", got)
		}
		if !strings.Contains(got.text, conflictPhrase) {
			t.Errorf("text %q does not name the identity collision", got.text)
		}
		if !strings.Contains(got.text, holderID) {
			t.Errorf("text %q does not name the holding block %s", got.text, holderID)
		}
		if strings.Contains(got.text, "23505") || strings.Contains(got.text, "uq_context_category_title_scope") {
			t.Errorf("text %q leaks SQL detail", got.text)
		}
		if len(errLines) != 0 {
			t.Errorf("an input error wrote ERROR lines:\n%s", strings.Join(errLines, "\n"))
		}
		// A confirmed update runs the statement after the consume: the answer
		// has to say the token is spent.
		if strings.HasSuffix(s.name, "_confirm") && !strings.Contains(got.text, "the stage token is consumed") {
			t.Errorf("confirm answer %q does not say the stage token is consumed", got.text)
		}
		t.Logf("answer: status=%d code=%q text=%q", got.status, got.code, got.text)
	}

	strptr := func(s string) *string { return &s }

	for _, s := range surfaces {
		t.Run(s.name+"/category_move_onto_taken_identity", func(t *testing.T) {
			// The issue's reproduction: A already lives in category c, B moves
			// there with the same title.
			title := "i227 cat " + s.name
			holder := mkBlock(t, "i227-c", title, "private")
			mover := mkBlock(t, "i227-b", title, "private")
			logs.take()
			got := s.update(t, mover, updateInput{Category: strptr("i227-c")}, nil)
			wantConflict(t, s, got, holder, i227ErrorLines(logs.take()))
			if g := identOf(t, mover); g != (ident{"i227-b", title, "private"}) {
				t.Errorf("refused update changed the mover: %+v", g)
			}
		})

		t.Run(s.name+"/title_rename_onto_taken_identity", func(t *testing.T) {
			holder := mkBlock(t, "i227-t", "i227 taken "+s.name, "private")
			mover := mkBlock(t, "i227-t", "i227 free "+s.name, "private")
			logs.take()
			got := s.update(t, mover, updateInput{Title: strptr("i227 taken " + s.name)}, nil)
			wantConflict(t, s, got, holder, i227ErrorLines(logs.take()))
			if g := identOf(t, mover); g.title != "i227 free "+s.name {
				t.Errorf("refused update changed the mover: %+v", g)
			}
		})

		t.Run(s.name+"/free_rename_still_succeeds", func(t *testing.T) {
			// Control: passes before and after the fix.
			mkBlock(t, "i227-f", "i227 neighbour "+s.name, "private")
			mover := mkBlock(t, "i227-f", "i227 old "+s.name, "private")
			got := s.update(t, mover, updateInput{Title: strptr("i227 new " + s.name)}, nil)
			if !got.ok {
				t.Fatalf("a free rename was refused: %+v", got)
			}
			if g := identOf(t, mover); g.title != "i227 new "+s.name {
				t.Errorf("rename not applied: %+v", g)
			}
		})

		if s.scopeMove {
			t.Run(s.name+"/scope_move_onto_taken_identity", func(t *testing.T) {
				title := "i227 scope " + s.name
				holder := mkBlock(t, "i227-s", title, "shared")
				mover := mkBlock(t, "i227-s", title, "private")
				logs.take()
				got := s.update(t, mover, updateInput{}, strptr("shared"))
				wantConflict(t, s, got, holder, i227ErrorLines(logs.take()))
				if g := identOf(t, mover); g.scope != "private" {
					t.Errorf("refused move changed the mover: %+v", g)
				}
			})
		}
	}

	// The fault probe: a 23505 on another constraint is a server fault as far as
	// this change knows, and keeps the opaque 500 with its ERROR line.
	if _, err := pool.Exec(ctx, `
		CREATE FUNCTION i227_other_unique() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'i227 fault probe' USING ERRCODE = 'unique_violation', CONSTRAINT = 'uq_i227_other';
		END $$;
		CREATE TRIGGER trg_i227_other_unique BEFORE UPDATE ON context_blocks
			FOR EACH ROW WHEN (NEW.title = 'i227 fault probe') EXECUTE FUNCTION i227_other_unique();`); err != nil {
		t.Fatalf("install fault probe: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS trg_i227_other_unique ON context_blocks; DROP FUNCTION IF EXISTS i227_other_unique()`)
	})
	for _, s := range surfaces {
		t.Run(s.name+"/other_unique_violation_stays_500", func(t *testing.T) {
			mover := mkBlock(t, "i227-p", "i227 probe "+s.name, "private")
			logs.take()
			got := s.update(t, mover, updateInput{Title: strptr("i227 fault probe")}, nil)
			errLines := i227ErrorLines(logs.take())
			if got.ok {
				t.Fatalf("fault probe answered success: %+v", got)
			}
			switch s.name {
			case "rest_manage_update":
				if got.status != http.StatusInternalServerError || got.text != "Internal server error" || got.code != "" {
					t.Errorf("answer = %+v, want the uncoded 500 Internal server error", got)
				}
			case "mcp_update":
				if got.text != "update failed: internal error" || got.code != "" {
					t.Errorf("answer = %+v, want the uncoded internal error", got)
				}
			case "mcp_confirm":
				if !strings.HasPrefix(got.text, "confirmed update failed to execute: ") || got.code != "" {
					t.Errorf("answer = %+v, want the uncoded execute error", got)
				}
			case "http_confirm":
				if got.status != http.StatusInternalServerError || got.code != "" ||
					got.text != "confirmed update failed to execute — the stage token is consumed; re-stage the update" {
					t.Errorf("answer = %+v, want the uncoded 500 execute error", got)
				}
			}
			if len(errLines) == 0 {
				t.Errorf("a server fault wrote no ERROR line")
			}
		})
	}
}
