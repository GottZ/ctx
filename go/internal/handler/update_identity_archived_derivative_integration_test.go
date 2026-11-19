//go:build integration

// The provenance gate (S3 / I7) on the update surfaces for an update that moves
// a block onto the identity of an ARCHIVED derivative.
//
// A client cannot write into a reserved category (S2, 403 reserved_category),
// but a block that already lives in one — written before the reservation or by
// a server writer without the derived layer's badge; the live corpus holds
// such blocks — carries its category along on an update that sends only a
// title or a scope. Before the store probed the identity an update moves onto,
// such a rename or scope move onto the identity of an archived derivative
// answered success on every surface. Each surface now answers the refusal it
// already gives for a derivative target: 403 provenance_protected, the
// confirm surfaces with the consumed-token wording, no ERROR line, and the
// block keeps its identity.
//
// The surfaces and their fixtures are the ones of issue #227
// (update_identity_conflict_integration_test.go).
//
//	go test -tags=integration ./internal/handler/ -run TestUpdateIdentityArchivedDerivative -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/derived"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/jackc/pgx/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestUpdateIdentityArchivedDerivative(t *testing.T) {
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
	updateTool := mcpUpdateHandler(mcpCfg)
	confirmTool := mcpConfirmHandler(mcpCfg)
	manage := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	confirmHTTP := NewConfirmHandler(pool, reg)

	// Default-tenant keys: home 'private', allowed_scopes '{shared}'.
	_, directPlain, err := store.CreateApiKey(ctx, pool, "u386-direct", "private", nil, store.DefaultTenantID)
	if err != nil {
		t.Fatalf("create direct key: %v", err)
	}
	flaggedRow, flaggedPlain, err := store.CreateApiKey(ctx, pool, "u386-flagged", "private", nil, store.DefaultTenantID)
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

	// The derived layer's category: the distill arm's default, reserved for
	// clients.
	const category = "session-insights"
	if !derived.IsReservedCategory(category) {
		t.Fatalf("%q is not a reserved category", category)
	}
	// derivative writes an arm's derivative through the badged upsert and
	// archives it (no Go path archives a derivative yet; §4.7.5 will).
	derivative := func(t *testing.T, title, scope string) {
		t.Helper()
		d, err := store.UpsertBlock(ctx, pool, category, title, "derivative text", nil,
			map[string]any{derived.MetadataKey: map[string]any{
				"v": derived.ContractVersion, "stratum": int(derived.StratumDerived), "arm": "u386-arm",
			}}, scope, true, store.SensitivityWrite{Value: backends.SensInternal, Derived: true}, derived.TypeInsight)
		if err != nil {
			t.Fatalf("derivative: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET is_archived = true WHERE id = $1::uuid`, d.ID); err != nil {
			t.Fatalf("archive derivative: %v", err)
		}
	}
	// legacy writes an ordinary block into the reserved category the way the
	// blocks the live corpus holds got there: a server write without the badge.
	legacy := func(t *testing.T, title, scope string) string {
		t.Helper()
		b, err := store.UpsertBlock(ctx, pool, category, title, "legacy text", nil, nil, scope, true, store.SensitivityWrite{}, "")
		if err != nil {
			t.Fatalf("legacy block: %v", err)
		}
		return b.ID
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
		scopeMove bool
		update    func(t *testing.T, id string, in updateInput, scope *string) i227Answer
	}
	surfaces := []surface{
		{"rest_manage_update", true, func(t *testing.T, id string, in updateInput, scope *string) i227Answer {
			t.Helper()
			data := map[string]any{}
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
			r, _, err := confirmTool(flaggedCtx, nil, confirmInput{PayloadHash: stage(t, in)})
			if err != nil {
				t.Fatalf("mcp confirm: %v", err)
			}
			return mcpAnswer(r)
		}},
		{"http_confirm", false, func(t *testing.T, id string, in updateInput, _ *string) i227Answer {
			t.Helper()
			in.ID = id
			body, err := json.Marshal(confirmRequest{PayloadHash: stage(t, in)})
			if err != nil {
				t.Fatalf("marshal confirm: %v", err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/confirm", bytes.NewReader(body)).WithContext(flaggedCtx)
			confirmHTTP.HandleConfirm(rec, req)
			return restAnswer(t, rec)
		}},
	}

	wantProtected := func(t *testing.T, s surface, got i227Answer, errLines []string) {
		t.Helper()
		if got.ok {
			t.Fatalf("a move onto an archived derivative's identity answered success: %+v", got)
		}
		if (s.name == "rest_manage_update" || s.name == "http_confirm") && got.status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (answer %+v)", got.status, got)
		}
		if got.code != "provenance_protected" {
			t.Errorf("code = %q, want provenance_protected (answer %+v)", got.code, got)
		}
		if strings.HasSuffix(s.name, "_confirm") && !strings.Contains(got.text, "the stage token is consumed") {
			t.Errorf("confirm answer %q does not say the stage token is consumed", got.text)
		}
		if len(errLines) != 0 {
			t.Errorf("a refusal wrote ERROR lines:\n%s", strings.Join(errLines, "\n"))
		}
	}
	strptr := func(s string) *string { return &s }

	// The issue verb's title change, on an issue that lives in a reserved
	// category: its lookup binds the type, not the category.
	t.Run("rest_issue_update/title_change_onto_archived_derivative_identity", func(t *testing.T) {
		iss, err := issueTx(ctx, pool, func(tx pgx.Tx) (*store.Block, error) {
			return store.InsertIssueBlock(ctx, tx, store.IssueFields{Scope: "private", Title: "u386 issue", Content: "issue body"},
				writableBlockScopes(directAR))
		})
		if err != nil {
			t.Fatalf("issue: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE context_blocks SET category = $2 WHERE id = $1::uuid`, iss.ID, category); err != nil {
			t.Fatalf("set up the legacy issue: %v", err)
		}
		prefix := strings.TrimSuffix(iss.Title, "u386 issue")
		derivative(t, prefix+"u386 issue target", "private")
		issueUpdate := func(title string) i227Answer {
			raw, err := json.Marshal(map[string]any{"title": title})
			if err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/api/manage", nil).WithContext(directCtx)
			manage.handleIssueUpdate(rec, req, directAR, manageRequest{Action: "issue-update", ID: iss.ID, Data: raw})
			return restAnswer(t, rec)
		}
		logs.take()
		got := issueUpdate("u386 issue target")
		wantProtected(t, surface{name: "rest_issue_update"}, got, i227ErrorLines(logs.take()))
		if got.status != http.StatusForbidden {
			t.Errorf("status = %d, want 403 (answer %+v)", got.status, got)
		}
		if g := identOf(t, iss.ID); g.title != iss.Title {
			t.Errorf("refused issue update changed the block: %+v", g)
		}
		if got := issueUpdate("u386 issue free"); !got.ok {
			t.Fatalf("a free issue rename was refused: %+v", got)
		}
	})

	for _, s := range surfaces {
		t.Run(s.name+"/title_rename_onto_archived_derivative_identity", func(t *testing.T) {
			title := "u386 insight " + s.name
			derivative(t, title, "private")
			mover := legacy(t, title+" (legacy)", "private")
			logs.take()
			got := s.update(t, mover, updateInput{Title: strptr(title)}, nil)
			wantProtected(t, s, got, i227ErrorLines(logs.take()))
			if g := identOf(t, mover); g != (ident{category, title + " (legacy)", "private"}) {
				t.Errorf("refused update changed the block: %+v", g)
			}
		})

		t.Run(s.name+"/free_rename_in_the_reserved_category_lands", func(t *testing.T) {
			// Control, before and after: the path is open for a block that
			// already lives in a reserved category, because a title-only update
			// claims no category.
			mover := legacy(t, "u386 free old "+s.name, "private")
			got := s.update(t, mover, updateInput{Title: strptr("u386 free new " + s.name)}, nil)
			if !got.ok {
				t.Fatalf("a free rename was refused: %+v", got)
			}
			if g := identOf(t, mover); g.title != "u386 free new "+s.name {
				t.Errorf("rename not applied: %+v", g)
			}
		})

		if s.scopeMove {
			t.Run(s.name+"/scope_move_onto_archived_derivative_identity", func(t *testing.T) {
				title := "u386 moved insight " + s.name
				derivative(t, title, "shared")
				mover := legacy(t, title, "private")
				logs.take()
				got := s.update(t, mover, updateInput{}, strptr("shared"))
				wantProtected(t, s, got, i227ErrorLines(logs.take()))
				if g := identOf(t, mover); g.scope != "private" {
					t.Errorf("refused move changed the block: %+v", g)
				}
			})
		}
	}
}
