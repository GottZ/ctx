package handler

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/forge"
	"github.com/GottZ/ctx/internal/store"
)

// TestContractFreezeGolden pins the workflow-UI wire contract (masterplan §3 K5,
// design/04 §3 / U03): the SAME __fixtures__/*.json files are eaten by the SPA's
// e2e + vitest fixtures (go/web/src/lib/api/__fixtures__) and re-serialized here
// from the LIVE handler structs (W6 project_issues.go, W7 project_issues_write.go,
// W11 project_sync.go, W4 project.go, types.go). A drift on EITHER side turns this
// red: rename a field in the JSON (FE hand-copy) and the DeepEqual against the Go
// serialization fails; rename a struct json tag (Go) and the same compare fails.
// That closes e2e-playwright.md Finding 8 (fixture drift was ungated) structurally.
//
// The freeze JSONs are GENERATED from this test: run `UPDATE_FREEZE=1 go test
// ./internal/handler -run TestContractFreezeGolden` to (re)emit them, review the
// diff, commit. CI runs it in compare mode (no env) — the anti-collusion anchor.
//
// NOTE ON §3.1 DEVIATION (design/04 §3.1 vs. the shipped W6/W7/W11 wire — Ist
// wins, U03 return): the built list row is store.WorkflowBlockRow
// {id,scope,type_name,title,workflow_status,updated_at} — it has NO
// sync_state/external_ref/comment_count/labels/status and NO top-level
// total/writable; the envelope carries render:"untrusted" + an opaque base64
// `cursor` (not the {after_updated,after_id} pair). Detail/comment bodies are the
// full store.Block. These JSONs are the Ist truth the TS mirror (types.ts) tracks.
func TestContractFreezeGolden(t *testing.T) {
	update := os.Getenv("UPDATE_FREEZE") == "1"
	dir := filepath.Join("..", "..", "web", "src", "lib", "api", "__fixtures__")

	ts := time.Date(2026, 7, 3, 0, 0, 0, 0, time.UTC)      // updated_at
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC) // created_at
	const (
		issueID   = "11111111-1111-1111-1111-111111111111"
		commentID = "22222222-2222-2222-2222-222222222222"
		projectID = "33333333-3333-3333-3333-333333333333"
		tenantID  = "44444444-4444-4444-4444-444444444444"
		runID     = "55555555-5555-5555-5555-555555555555"
		scope     = "acme:main"
	)
	createdBy := "66666666-6666-6666-6666-666666666666"

	// --- W6 list row (store.WorkflowBlockRow) ---
	row := store.WorkflowBlockRow{
		ID: issueID, Scope: scope, TypeName: store.IssueTypeName,
		Title: "Example issue", WorkflowStatus: "open", UpdatedAt: ts,
	}

	// --- W6/W7 issue detail body (store.Block, issueScanCols order) ---
	issueBlock := store.Block{
		ID: issueID, Category: "task", Tags: []string{"bug", "p1"},
		Title: "Example issue", Content: "# Example\n\nBody markdown.",
		Metadata:          map[string]any{"labels": []any{"bug", "p1"}},
		Scope:             scope,
		Sensitivity:       "internal",
		SensitivitySource: "manual",
		TypeName:          store.IssueTypeName,
		LifecycleState:    "active",
		TypeSource:        "manual",
		WorkflowStatus:    "open",
		CreatedAt:         created, UpdatedAt: ts,
	}
	// --- comment body (store.Block; no workflow_status, type=comment) ---
	commentBlock := store.Block{
		ID: commentID, Category: "comment", Tags: []string{},
		Title: "", Content: "A comment on the issue.",
		Metadata:          map[string]any{},
		Scope:             scope,
		Sensitivity:       "internal",
		SensitivitySource: "auto",
		TypeName:          store.CommentTypeName,
		LifecycleState:    "active",
		TypeSource:        "manual",
		CreatedAt:         created, UpdatedAt: ts,
	}

	// --- W6 list envelope: exercise the REAL helper (byte-faithful) ---
	listRec := httptest.NewRecorder()
	writeIssueListPage(listRec, []store.WorkflowBlockRow{row}, nil)
	listEnvelope := decodeJSON(t, listRec.Body.Bytes())

	// --- W6 detail envelope (mirror of HandleDetail's map) ---
	detailEnvelope := map[string]any{
		"success": true, "render": "untrusted",
		"issue": issueBlock, "comments": []store.Block{commentBlock}, "comments_cursor": nil,
	}
	// --- W6 comment-thread envelope (mirror of HandleComments' map) ---
	commentsEnvelope := map[string]any{
		"success": true, "render": "untrusted",
		"comments": []store.Block{commentBlock}, "cursor": nil,
	}
	// --- W6 board envelope (mirror of HandleBoard's map) ---
	boardEnvelope := map[string]any{
		"success": true, "render": "untrusted",
		"columns": []map[string]any{
			{"status": "open", "count": 2, "issues": []store.WorkflowBlockRow{row}, "cursor": nil},
			{"status": "closed", "count": 0, "issues": []store.WorkflowBlockRow{}, "cursor": nil},
		},
	}
	// --- W7 create/patch envelope (mirror of HandleCreate/HandlePatch's map) ---
	issueMutateEnvelope := map[string]any{
		"success": true, "render": "untrusted", "issue": issueBlock,
	}
	// --- W7 comment-create envelope (mirror of HandleCommentCreate's map) ---
	commentCreateEnvelope := map[string]any{
		"success": true, "render": "untrusted", "comment": commentBlock,
	}

	// --- W4 project row + list envelope (store.ProjectRow) ---
	projectRow := store.ProjectRow{
		ID: projectID, TenantID: tenantID, Scope: scope,
		Identity: "github:acme/main", DisplayName: "Acme Main",
		Forge:      json.RawMessage(`{"kind":"github","owner":"acme","repo":"main"}`),
		SyncStatus: "idle", SyncEnabled: true, PushEnabled: false,
		LastSyncAt: &ts, SyncCursor: json.RawMessage("null"),
		CreatedAt: created, CreatedBy: &createdBy, Metadata: json.RawMessage("{}"),
	}
	projectListEnvelope := map[string]any{
		"success": true, "projects": []store.ProjectRow{projectRow},
	}

	// --- W11 sync status envelope (mirror of HandleStatus's map) ---
	runStatus := forge.SyncStatus{ProjectID: projectID, Running: false}
	lastRun := store.SyncRunRow{
		ID: runID, ProjectID: projectID, StartedAt: created, FinishedAt: &ts,
		Status: "done", Stats: json.RawMessage(`{"fetched":10,"applied":8}`),
	}
	syncEnvelope := map[string]any{
		"success":       true,
		"project_id":    projectID,
		"sync_status":   "idle",
		"sync_enabled":  true,
		"push_enabled":  false,
		"token_set":     false,
		"last_sync_at":  &ts,
		"backoff_until": nil,
		"last_error":    nil,
		"conflicts":     0,
		"run":           runStatus,
		"last_run":      lastRun,
		"recent_runs":   []store.SyncRunRow{lastRun},
	}

	// --- types.go effective-list envelope (typeView via resolveEffectiveTypes) ---
	typeListEnvelope := map[string]any{
		"success": true,
		"types": resolveEffectiveTypes([]store.BlockTypeRow{{
			ID: "77777777-7777-7777-7777-777777777777", Name: store.IssueTypeName,
			Scope: store.GlobalScope, DisplayName: "Issue", Description: "A tracked work item",
			Builtin: true, IsDefault: false,
			Config:    json.RawMessage(`{"v":1,"retrieval":{"policy":"full-pass"},"parent":{"mode":"none"}}`),
			CreatedAt: ts, UpdatedAt: ts,
		}}),
	}

	cases := []struct {
		file string
		val  any
	}{
		{"issue-list.json", listEnvelope},
		{"issue-detail.json", detailEnvelope},
		{"issue-comments.json", commentsEnvelope},
		{"board.json", boardEnvelope},
		{"issue-mutate.json", issueMutateEnvelope},
		{"comment-create.json", commentCreateEnvelope},
		{"project-list.json", projectListEnvelope},
		{"sync-status.json", syncEnvelope},
		{"type-list.json", typeListEnvelope},
	}

	for _, c := range cases {
		path := filepath.Join(dir, c.file)
		// Serialize the Go value to the canonical wire bytes.
		got, err := json.MarshalIndent(normalize(t, c.val), "", "  ")
		if err != nil {
			t.Fatalf("%s: marshal: %v", c.file, err)
		}
		got = append(got, '\n')

		if update {
			if err := os.WriteFile(path, got, 0o644); err != nil {
				t.Fatalf("%s: write: %v", c.file, err)
			}
			t.Logf("UPDATE_FREEZE: wrote %s (%d bytes)", c.file, len(got))
			continue
		}

		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%s: read freeze fixture (run UPDATE_FREEZE=1 to generate): %v", c.file, err)
		}
		// Compare structurally (order/whitespace-insensitive) so a hand-formatted
		// FE fixture still passes iff the SHAPE matches — the field names/values
		// are the contract, not the byte layout.
		if !reflect.DeepEqual(decodeJSON(t, got), decodeJSON(t, want)) {
			t.Errorf("%s: wire drift (K5 freeze — FE fixture and Go serialization diverged;\n"+
				"update go/web/src/lib/api/types.ts + the fixture in lockstep, or regen with UPDATE_FREEZE=1):\n"+
				" go:   %s\n file: %s", c.file, string(got), string(want))
		}
	}
}

// normalize round-trips a value through JSON so map[string]any envelopes carrying
// nested structs serialize with the structs' json tags (and numbers become the
// canonical wire form) before the golden comparison / file write.
func normalize(t *testing.T, v any) any {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("normalize marshal: %v", err)
	}
	return decodeJSON(t, raw)
}

// decodeJSON unmarshals bytes into an order-insensitive any tree for DeepEqual.
func decodeJSON(t *testing.T, b []byte) any {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var out any
	if err := dec.Decode(&out); err != nil {
		t.Fatalf("decode json: %v\n%s", err, string(b))
	}
	return out
}
