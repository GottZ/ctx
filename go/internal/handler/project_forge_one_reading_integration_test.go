//go:build integration

// Issue #474: the forge config a project row stores is the form the API
// checked, and every reader of the row reads it as the sync run does.
//
// validateForge decoded the request with encoding/json — object keys matched
// without regard to case, the last of duplicate keys winning — and the row
// stored the request's own text as JSONB, which keeps "api_base" and
// "API_BASE" as two keys. A second spelling of a key so carried a value the
// check never saw: credentials in forge.api_base passed (the check read the
// clean spelling), and so did an http:// or loopback api_base; the row then
// served them to every member of the scope. Now a forge with any key outside
// kind, owner, repo and api_base — another spelling included — or with a key
// twice is refused, and the row stores the decoded form. A row written before
// is served as the run reads it, without credentials.
//
//	go test -tags=integration ./internal/handler/ -run 'TestAForgeIsStoredInTheFormItWasChecked|TestAStoredForgeIsServedAsTheRunReadsIt' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestAForgeIsStoredInTheFormItWasChecked(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	refused := []struct{ name, forge string }{
		{"credentials in a second spelling", `{"kind":"github","owner":"a","repo":"ui","api_base":"https://user:secret@ghe.example.com/api/v3","API_BASE":"https://ghe.example.com/api/v3"}`},
		{"http in a second spelling", `{"kind":"github","owner":"a","repo":"ui","api_base":"http://ghe.example.com/api/v3","Api_Base":"https://ghe.example.com/api/v3"}`},
		{"loopback in a second spelling", `{"kind":"github","owner":"a","repo":"ui","api_base":"https://127.0.0.1/api/v3","API_BASE":"https://ghe.example.com/api/v3"}`},
		{"a key twice", `{"kind":"github","owner":"a","repo":"ui","api_base":"https://user:secret@ghe.example.com/api/v3","api_base":"https://ghe.example.com/api/v3"}`},
		{"owner in a second spelling", `{"kind":"github","owner":"a","OWNER":"b","repo":"ui"}`},
		{"a key outside the config", `{"kind":"github","owner":"a","repo":"ui","token":"secret"}`},
		{"a value that is not a string", `{"kind":"github","owner":"a","repo":["ui"]}`},
	}
	wantRefusal := func(t *testing.T, step string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity {
			t.Errorf("%s: status %d (body=%s), want 422", step, code, body)
		}
		if strings.Contains(body, "user:secret") || strings.Contains(body, `"secret"`) {
			t.Errorf("%s: the answer echoes the input: %s", step, body)
		}
	}
	tn := be5SeedTenant(t, pool, "w474one")
	admin := w4TenantAdmin(tn, "w474one:repo")
	for i, c := range refused {
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:a/one" + string(rune('a'+i)), "scope": "one" + string(rune('a'+i)), "forge": json.RawMessage(c.forge),
		})
		wantRefusal(t, "create, "+c.name, rec.Code, rec.Body.String())
	}
	rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/ui", "scope": "repo"})
	id := w4ProjectField(t, rec, "id")
	for _, c := range refused {
		pr := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{"forge": json.RawMessage(c.forge)})
		wantRefusal(t, "PATCH, "+c.name, pr.Code, pr.Body.String())
	}
	var stored string
	if err := pool.QueryRow(ctx, `SELECT forge::text FROM context_projects WHERE id = $1::uuid`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != "{}" {
		t.Errorf("the row after the refused PATCHes holds forge %s, want {}", stored)
	}
	h := t43Handler(pool)
	sa, _ := kaServerAdmin(ctx, t, pool)
	for i, c := range refused {
		status, resp := kaManage(t, h, sa, "w474-one-provision", map[string]any{
			"action": "project-provision", "data": map[string]any{
				"identity": "manual:w474-one-" + string(rune('a'+i)), "forge": json.RawMessage(c.forge),
			},
		})
		msg, _ := resp["error"].(string)
		wantRefusal(t, "provision, "+c.name, status, msg)
	}

	// A forge in the documented form is stored as checked: the decoded keys,
	// nothing else.
	pr := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{
		"forge": json.RawMessage(`{"kind":"github","owner":"a","repo":"ui","api_base":"https://ghe.example.com/api/v3"}`),
	})
	if pr.Code != http.StatusOK {
		t.Fatalf("PATCH with a valid forge: %d %s", pr.Code, pr.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT forge::text FROM context_projects WHERE id = $1::uuid`, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	var keys map[string]string
	if err := json.Unmarshal([]byte(stored), &keys); err != nil || len(keys) != 4 || keys["api_base"] != "https://ghe.example.com/api/v3" {
		t.Errorf("the row after a valid PATCH holds forge %s (%v)", stored, err)
	}
}

func TestAStoredForgeIsServedAsTheRunReadsIt(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	tn := be5SeedTenant(t, pool, "w474srv")
	admin := w4TenantAdmin(tn, "w474srv:repo")
	rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/srv", "scope": "repo"})
	id := w4ProjectField(t, rec, "id")
	member := w4TenantAdmin(tn, "w474srv:repo")
	member.TenantRole = "member"
	// Rows the API took before this change (it has no way to write them now).
	legacy := []struct{ name, forge, servedBase string }{
		{"credentials in a second spelling", `{"kind":"github","owner":"a","repo":"srv","api_base":"https://ghe.example.com/api/v3","API_BASE":"https://user:secret@ghe.example.com/api/v3"}`, ""},
		{"credentials before issue #474", `{"kind":"github","owner":"a","repo":"srv","api_base":"https://user:secret@ghe.example.com/api/v3"}`, "https://ghe.example.com/api/v3"},
		{"a key outside the config", `{"kind":"github","owner":"a","repo":"srv","token":"ghp_secret"}`, ""},
	}
	for _, c := range legacy {
		if _, err := pool.Exec(ctx, `UPDATE context_projects SET forge = $2::jsonb WHERE id = $1::uuid`, id, c.forge); err != nil {
			t.Fatal(err)
		}
		for _, step := range []struct {
			what string
			body string
		}{
			{"GET", w4Do(t, pool, member, http.MethodGet, "/api/project/"+id, nil).Body.String()},
			{"list", w4Do(t, pool, member, http.MethodGet, "/api/project", nil).Body.String()},
		} {
			if strings.Contains(step.body, "user:secret") || strings.Contains(step.body, "ghp_secret") {
				t.Errorf("%s, %s: the answer serves the credential: %s", c.name, step.what, step.body)
				continue
			}
			var resp struct {
				Project  struct{ Forge map[string]string } `json:"project"`
				Projects []struct{ Forge map[string]string } `json:"projects"`
			}
			if err := json.Unmarshal([]byte(step.body), &resp); err != nil {
				t.Fatalf("%s, %s: %v (%s)", c.name, step.what, err, step.body)
			}
			f := resp.Project.Forge
			if step.what == "list" && len(resp.Projects) == 1 {
				f = resp.Projects[0].Forge
			}
			if f["owner"] != "a" || f["repo"] != "srv" || f["api_base"] != c.servedBase {
				t.Errorf("%s, %s: forge served as %v, want owner a, repo srv, api_base %q", c.name, step.what, f, c.servedBase)
			}
		}
	}
}
