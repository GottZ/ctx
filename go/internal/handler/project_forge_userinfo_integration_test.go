//go:build integration

// Issue #474: forge.api_base takes no credentials. A backend row's base_url
// refuses userinfo (backends/validate.go); validateForge accepted it on
// project create, PATCH and provision, and the row then served it to every
// member of the scope (GET /api/project) and a sync without a token sent it
// to the forge as Basic credentials.
//
//	go test -tags=integration ./internal/handler/ -run TestForgeAPIBaseTakesNoCredentials -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/testdb"
)

func TestForgeAPIBaseTakesNoCredentials(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	withUser := []string{
		"https://user:secret@ghe.example.com/api/v3",
		"https://user@ghe.example.com/api/v3",
		"https://:secret@ghe.example.com/api/v3",
	}
	wantRefusal := func(t *testing.T, step string, code int, body string) {
		t.Helper()
		if code != http.StatusUnprocessableEntity || !strings.Contains(body, "credentials") {
			t.Errorf("%s: status %d (body=%s), want 422 naming the credentials", step, code, body)
		}
		if strings.Contains(body, "secret") {
			t.Errorf("%s: the answer echoes the credential: %s", step, body)
		}
	}

	tn := be5SeedTenant(t, pool, "w474ui")
	admin := w4TenantAdmin(tn, "w474ui:repo")
	for _, ab := range withUser {
		rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{
			"identity": "github:a/ui", "scope": "repo",
			"forge": map[string]any{"kind": "github", "owner": "a", "repo": "ui", "api_base": ab},
		})
		wantRefusal(t, "create with "+ab, rec.Code, rec.Body.String())
	}
	rec := w4Do(t, pool, admin, http.MethodPost, "/api/project", map[string]any{"identity": "github:a/ui", "scope": "repo"})
	id := w4ProjectField(t, rec, "id")
	for _, ab := range withUser {
		pr := w4Do(t, pool, admin, http.MethodPatch, "/api/project/"+id, map[string]any{
			"forge": map[string]any{"kind": "github", "owner": "a", "repo": "ui", "api_base": ab},
		})
		wantRefusal(t, "PATCH with "+ab, pr.Code, pr.Body.String())
	}

	h := t43Handler(pool)
	sa, _ := kaServerAdmin(context.Background(), t, pool)
	for i, ab := range withUser {
		status, resp := kaManage(t, h, sa, "w474-provision", map[string]any{
			"action": "project-provision", "data": map[string]any{
				"identity": "manual:w474-prov-" + string(rune('a'+i)),
				"forge":    map[string]any{"kind": "github", "owner": "a", "repo": "ui", "api_base": ab},
			},
		})
		msg, _ := resp["error"].(string)
		wantRefusal(t, "provision with "+ab, status, msg)
	}
}
