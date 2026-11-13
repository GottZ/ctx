//go:build integration

// Issue #198, part A: an update by id whose title and content are byte-equal
// to the stored row must keep the block's embedding. Before the fix
// store.UpdateBlock reported needsReEmbed on the mere presence of `title` or
// `content` in the request, and finishBlockUpdate cleared the vector: a client
// that sends the full record with every update (title and content together
// with new tags) lost the vector on each call and put the block back into the
// embed backfill, although the embed input had not changed.
//
// The three callers of store.UpdateBlock are covered, each over its own
// surface: the MCP `update` tool, the same tool staged and run through
// `confirm`, and the REST manage `update` action. Every block is written
// through the MCP `store` tool and gets its vector through
// store.StoreEmbedding, the writer the backfill uses.
//
// The "changed" subtests are the control: a real change of the title or of the
// content still clears the vector. They pass before and after the fix.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestUpdateUnchangedKeepsEmbedding -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestUpdateUnchangedKeepsEmbedding(t *testing.T) {
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

	mcpCfg := MCPConfig{Pool: pool, Cfg: cfgStore}
	storeTool := mcpStoreHandler(mcpCfg)
	updateTool := mcpUpdateHandler(mcpCfg)
	confirmTool := mcpConfirmHandler(mcpCfg)
	manage := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)

	_, directPlain, err := store.CreateApiKey(ctx, pool, "u198-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "u198-flagged", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create flagged key: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE context_api_keys SET confirm_writes = true WHERE id = $1`, flaggedRow.ID); err != nil {
		t.Fatalf("opt in flagged key: %v", err)
	}
	directCtx := mcpAuthCtx(t, ctx, pool, directPlain)
	flaggedCtx := mcpAuthCtx(t, ctx, pool, flaggedPlain)
	restAR := &auth.AuthResult{IsValid: true, HomeScope: "private", ReadScopes: []string{"private"}}

	const (
		baseContent = "base content"
		model       = "u198-model"
	)
	vec := make([]float32, 1024)
	for i := range vec {
		vec[i] = 0.5
	}

	// mkBlock writes a block over the MCP store tool and gives it a vector the
	// way the backfill does.
	mkBlock := func(t *testing.T, title string) string {
		t.Helper()
		r, _, err := storeTool(directCtx, nil, storeInput{Category: "test", Title: title, Content: baseContent, Tags: []string{"old"}})
		if err != nil || r.IsError {
			t.Fatalf("fixture store %s failed: %v %v", title, err, r)
		}
		var id string
		if err := pool.QueryRow(ctx, `SELECT id::text FROM context_blocks WHERE title = $1`, title).Scan(&id); err != nil {
			t.Fatalf("fixture id: %v", err)
		}
		if err := store.StoreEmbedding(ctx, pool, id, model, vec); err != nil {
			t.Fatalf("fixture embedding: %v", err)
		}
		return id
	}

	type row struct {
		title, content, embedModel string
		tags                       []string
		hasEmbedding               bool
	}
	read := func(t *testing.T, id string) row {
		t.Helper()
		var r row
		if err := pool.QueryRow(ctx,
			`SELECT title, content, tags, embedding IS NOT NULL, COALESCE(embed_model, '')
			 FROM context_blocks WHERE id = $1::uuid`, id).
			Scan(&r.title, &r.content, &r.tags, &r.hasEmbedding, &r.embedModel); err != nil {
			t.Fatalf("read block: %v", err)
		}
		return r
	}

	// One updater per caller of store.UpdateBlock.
	surfaces := []struct {
		name   string
		update func(t *testing.T, id string, in updateInput)
	}{
		{"mcp_update", func(t *testing.T, id string, in updateInput) {
			t.Helper()
			in.ID = id
			r, _, err := updateTool(directCtx, nil, in)
			if err != nil || r.IsError {
				t.Fatalf("mcp update failed: %v %v", err, r)
			}
		}},
		{"mcp_update_confirmed", func(t *testing.T, id string, in updateInput) {
			t.Helper()
			in.ID = id
			r, _, err := updateTool(flaggedCtx, nil, in)
			if err != nil {
				t.Fatalf("stage update: %v", err)
			}
			m := hashRe.FindStringSubmatch(resultText(t, r))
			if !r.IsError || m == nil {
				t.Fatalf("update of a confirm_writes key was not staged: %s", resultText(t, r))
			}
			cr, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: m[1]})
			if err != nil || cr.IsError {
				t.Fatalf("confirm failed: %v %v", err, cr)
			}
		}},
		{"rest_manage_update", func(t *testing.T, id string, in updateInput) {
			t.Helper()
			raw, err := json.Marshal(in)
			if err != nil {
				t.Fatalf("marshal update data: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/manage", nil)
			manage.handleUpdate(rec, req, restAR, manageRequest{Action: "update", ID: id, Data: raw})
			var resp map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode update response: %v (body %s)", err, rec.Body.String())
			}
			if ok, _ := resp["success"].(bool); !ok {
				t.Fatalf("manage update failed: %v", resp)
			}
		}},
	}

	strptr := func(s string) *string { return &s }
	cases := []struct {
		name string
		// in builds the update from the block's stored title.
		in         func(title string) updateInput
		wantVector bool
	}{
		{"title_and_content_unchanged_with_new_tags", func(title string) updateInput {
			return updateInput{Title: strptr(title), Content: strptr(baseContent), Tags: []string{"new"}}
		}, true},
		{"title_unchanged", func(title string) updateInput {
			return updateInput{Title: strptr(title), Tags: []string{"new"}}
		}, true},
		{"content_unchanged", func(string) updateInput {
			return updateInput{Content: strptr(baseContent), Tags: []string{"new"}}
		}, true},
		{"content_changed", func(title string) updateInput {
			return updateInput{Title: strptr(title), Content: strptr(baseContent + " v2"), Tags: []string{"new"}}
		}, false},
		{"title_changed", func(title string) updateInput {
			return updateInput{Title: strptr(title + " v2"), Content: strptr(baseContent), Tags: []string{"new"}}
		}, false},
	}

	for _, s := range surfaces {
		for _, c := range cases {
			t.Run(s.name+"/"+c.name, func(t *testing.T) {
				title := "u198 " + s.name + " " + c.name
				id := mkBlock(t, title)
				if pre := read(t, id); !pre.hasEmbedding || pre.embedModel != model {
					t.Fatalf("precondition: block has no vector before the update (%+v)", pre)
				}
				in := c.in(title)

				s.update(t, id, in)

				got := read(t, id)
				// The update itself must have been applied in every case.
				if !slices.Equal(got.tags, []string{"new"}) {
					t.Errorf("tags = %v, want [new] (the update must still be written)", got.tags)
				}
				if in.Title != nil && got.title != *in.Title {
					t.Errorf("title = %q, want %q", got.title, *in.Title)
				}
				if in.Content != nil && got.content != *in.Content {
					t.Errorf("content = %q, want %q", got.content, *in.Content)
				}
				if c.wantVector {
					if !got.hasEmbedding || got.embedModel != model {
						t.Errorf("an update with unchanged title/content cleared the vector: has_embedding=%v embed_model=%q, want kept (%s)",
							got.hasEmbedding, got.embedModel, model)
					}
				} else if got.hasEmbedding || got.embedModel != "" {
					t.Errorf("a changed title/content kept the old vector: has_embedding=%v embed_model=%q, want cleared",
						got.hasEmbedding, got.embedModel)
				}
			})
		}
	}
}
