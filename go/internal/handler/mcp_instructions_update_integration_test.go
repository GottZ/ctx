//go:build integration

// An update that ships a newer MCP instructions wording (GottZ/ctx#315),
// simulated through the mcpInstructionsHistory seam: the history gains one
// entry, as the next release's would. An instance without its own text follows
// the new wording; a custom text stays and its comparison reports that ctx has
// changed its wording since the text was written; a reset follows the newest.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPInstructionsUpdate -count=1 -v
package handler

import (
	"net/http"
	"testing"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestMCPInstructionsUpdate_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	for _, v := range config.EnvVars() {
		t.Setenv(v, "")
	}
	t.Setenv(settings.EnvDisable, "")
	t.Setenv("CONTEXT_DB_PASSWORD", "test-password") // the env base requires one
	t.Setenv(sealbox.EnvKey, freshMasterKey(t))
	t.Setenv(sealbox.EnvKeyPrev, "")

	api := newTenantAPI(t, pool)
	server := newMCPServer(MCPConfig{Pool: pool, Cfg: api.cfg})
	op := func() *tenantAPI { return api.as(operatorAR()) }

	// A custom text written under this release records base #2.
	const own = "Our own rules for agents."
	if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own)); rec.Code != http.StatusOK {
		t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
	}

	// The update: the next release appends wording #3.
	const v3 = "ctx is your memory. Search it first; store what lasts, with metadata.source."
	released := config.ShippedMCPInstructions()
	updated := append(config.ShippedMCPInstructions(), config.ShippedInstructions{ID: len(released) + 1, Since: "v9.99.0", Text: v3})
	mcpInstructionsHistory = func() []config.ShippedInstructions { return append([]config.ShippedInstructions(nil), updated...) }
	t.Cleanup(func() { mcpInstructionsHistory = config.ShippedMCPInstructions })

	t.Run("CustomText_StaysAndReportsTheChange", func(t *testing.T) {
		if got := handshakeInstructions(t, server); got != own {
			t.Fatalf("instructions after the update = %q, want the custom text untouched", got)
		}
		in := getInstructions(t, api).Setting.Instructions
		if in.State != "custom" || in.Base == nil || in.Base.ID != 2 || in.Current.ID != 3 || !in.ChangedSinceBase {
			t.Fatalf("comparison = %+v, want custom, base #2, current #3, changed since base", in)
		}
		if in.Base.Text != shippedInstructionsV2 || in.Current.Text != v3 || len(in.History) != 3 {
			t.Errorf("comparison texts = base %q current %q history %d, want #2, #3 and three entries", in.Base.Text, in.Current.Text, len(in.History))
		}
	})

	t.Run("SameTextAgain_KeepsTheBase", func(t *testing.T) {
		// An idempotent re-apply (config management) is not a new decision:
		// the note that ctx changed its wording must survive it.
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own)); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		in := getInstructions(t, api).Setting.Instructions
		if in.Base == nil || in.Base.ID != 2 || !in.ChangedSinceBase {
			t.Errorf("comparison after re-PUT of the same text = %+v, want base #2 still, changed since base", in)
		}
	})

	t.Run("EditAfterTheUpdate_MovesTheBase", func(t *testing.T) {
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own+" Edited.")); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		in := getInstructions(t, api).Setting.Instructions
		if in.Base == nil || in.Base.ID != 3 || in.ChangedSinceBase {
			t.Errorf("comparison = %+v, want base #3 and no change since", in)
		}
	})

	t.Run("NoCustomText_FollowsTheUpdate", func(t *testing.T) {
		if rec := op().do(t, http.MethodDelete, "/api/settings/mcp.instructions", ""); rec.Code != http.StatusOK {
			t.Fatalf("DELETE = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != v3 {
			t.Fatalf("instructions without a custom text = %q, want the new wording #3", got)
		}
		if in := getInstructions(t, api).Setting.Instructions; in.State != "shipped" || in.Current.ID != 3 || in.Matches != 2 {
			t.Errorf("comparison = %+v, want shipped, current #3 (the default still equals #2)", in)
		}
	})

	t.Run("StoredPreviousWording_FollowsTheUpdate", func(t *testing.T) {
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, shippedInstructionsV2)); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != v3 {
			t.Fatalf("instructions with stored wording #2 = %q, want #3", got)
		}
	})
}
