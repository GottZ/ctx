//go:build integration

// The MCP instructions as a setting (GottZ/ctx#315), end to end against a real
// PG18 testcontainer: the settings API writes through the production handler
// (validation, audit, NOTIFY-less in-process reload), and the text is read
// back the way an agent reads it — over a client/server handshake against ONE
// MCP server built before any write, so every change it sees is a hot one.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestMCPInstructionsSetting -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/testdb"
)

// The two wordings ctx has shipped, as literals: the test pins what a client
// receives byte for byte, not what a constant in the code says.
const (
	shippedInstructionsV1 = "ctx is a persistent knowledge store with hybrid retrieval. Use 'query' for questions, 'store' to save knowledge, 'search' for lightweight lookups."
	// instructionsCap is config.MCPInstructionsMaxChars, as a literal: the
	// documented limit is part of the contract this test pins.
	instructionsCap       = 2048
	shippedInstructionsV2 = "ctx is your memory across sessions. Before you answer from memory or start work, look it up: 'search' for a lightweight lookup, 'query' for a question that needs a synthesized answer. When you learn something that should outlast this session, save it with 'store' and put its source into metadata.source."
)

// handshakeInstructions connects fresh clients to server over in-memory
// transports — one per handshakeVersions entry, so both server/discover and
// initialize are read — and returns the instructions, which must agree.
func handshakeInstructions(t *testing.T, server *mcp.Server) string {
	t.Helper()
	var texts []string
	for _, version := range handshakeVersions {
		texts = append(texts, connectProbe(t, server, version).InitializeResult().Instructions)
	}
	if texts[0] != texts[1] {
		t.Fatalf("server/discover answered %q, initialize %q — one source, one text", texts[0], texts[1])
	}
	return texts[0]
}

// instructionsBlock is the settings view's comparison of mcp.instructions.
type instructionsBlock struct {
	State   string `json:"state"`
	Custom  bool   `json:"custom"`
	Matches int    `json:"matches"`
	Current struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	} `json:"current"`
	Base *struct {
		ID   int    `json:"id"`
		Text string `json:"text"`
	} `json:"base"`
	ChangedSinceBase bool `json:"changed_since_base"`
	History          []struct {
		ID    int    `json:"id"`
		Since string `json:"since"`
		Text  string `json:"text"`
	} `json:"history"`
}

type instructionsGet struct {
	Setting struct {
		Value        any                `json:"value"`
		Source       string             `json:"source"`
		Default      any                `json:"default"`
		Instructions *instructionsBlock `json:"instructions"`
	} `json:"setting"`
}

func getInstructions(t *testing.T, api *tenantAPI) instructionsGet {
	t.Helper()
	rec := api.as(operatorAR()).do(t, http.MethodGet, "/api/settings/mcp.instructions", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET mcp.instructions = %d body=%s", rec.Code, rec.Body.String())
	}
	var g instructionsGet
	if err := json.Unmarshal(rec.Body.Bytes(), &g); err != nil {
		t.Fatalf("unmarshal %s: %v", rec.Body.String(), err)
	}
	if g.Setting.Instructions == nil {
		t.Fatalf("GET mcp.instructions carries no instructions comparison: %s", rec.Body.String())
	}
	return g
}

func putJSON(t *testing.T, v string) string {
	t.Helper()
	b, err := json.Marshal(map[string]string{"value": v})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestMCPInstructionsSetting_Integration(t *testing.T) {
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
	server := newMCPServer(MCPConfig{Pool: pool, Cfg: api.cfg}) // built once: every change below is hot
	op := func() *tenantAPI { return api.as(operatorAR()) }
	rowValue := func() (string, bool) { return scopeRowValue(t, pool, "mcp.instructions", "_global") }
	rowBase := func() string {
		t.Helper()
		var base *string
		if err := pool.QueryRow(context.Background(),
			`SELECT metadata->>'shipped_base' FROM context_settings WHERE key='mcp.instructions' AND scope='_global'`).Scan(&base); err != nil {
			t.Fatalf("read base: %v", err)
		}
		if base == nil {
			return ""
		}
		return *base
	}

	t.Run("Default_SendsTheShippedWording", func(t *testing.T) {
		if got := handshakeInstructions(t, server); got != shippedInstructionsV2 {
			t.Fatalf("instructions = %q, want the shipped wording %q", got, shippedInstructionsV2)
		}
		g := getInstructions(t, api)
		in := g.Setting.Instructions
		if g.Setting.Source != "default" || g.Setting.Value != shippedInstructionsV2 || g.Setting.Default != shippedInstructionsV2 {
			t.Errorf("setting = value %q source %q default %q, want the shipped wording from the default", g.Setting.Value, g.Setting.Source, g.Setting.Default)
		}
		if in.State != "shipped" || in.Custom || in.Matches != 2 || in.Current.ID != 2 || in.Current.Text != shippedInstructionsV2 || in.Base != nil {
			t.Errorf("comparison = %+v, want shipped, matches 2, current 2, no base", in)
		}
		if len(in.History) != 2 || in.History[0].ID != 1 || in.History[0].Text != shippedInstructionsV1 || in.History[1].Text != shippedInstructionsV2 {
			t.Errorf("history = %+v, want #1 the old wording and #2 the current one", in.History)
		}
	})

	const own = "Our team's rules:\n- look up before you work\n- save decisions with their source"
	t.Run("CustomText_IsSentAndRecordsItsBase", func(t *testing.T) {
		rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != own {
			t.Fatalf("instructions after PUT = %q, want the custom text byte for byte", got)
		}
		if base := rowBase(); base != "2" {
			t.Errorf("row metadata shipped_base = %q, want 2 (the wording current at the write)", base)
		}
		in := getInstructions(t, api).Setting.Instructions
		if in.State != "custom" || !in.Custom || in.Matches != 0 || in.Base == nil || in.Base.ID != 2 || in.ChangedSinceBase {
			t.Errorf("comparison = %+v, want custom based on #2, unchanged since", in)
		}
	})

	t.Run("RuntimeChange_ReachesTheNextHandshake", func(t *testing.T) {
		const second = "Look things up in ctx first."
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, second)); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != second {
			t.Fatalf("instructions = %q, want the changed text without a restart", got)
		}
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own)); rec.Code != http.StatusOK {
			t.Fatalf("PUT back = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("Disabled_SendsNothing", func(t *testing.T) {
		rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions_enabled", `{"value":false}`)
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT enabled=false = %d body=%s", rec.Code, rec.Body.String())
		}
		var put struct {
			Instructions *instructionsBlock `json:"instructions"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &put); err != nil || put.Instructions == nil || put.Instructions.State != "off" {
			t.Errorf("PUT enabled=false answer carries comparison %+v (err %v), want state off — the page renders it", put.Instructions, err)
		}
		if got := handshakeInstructions(t, server); got != "" {
			t.Fatalf("instructions while disabled = %q, want none", got)
		}
		if in := getInstructions(t, api).Setting.Instructions; in.State != "off" || !in.Custom {
			t.Errorf("comparison while disabled = %+v, want off with the custom text kept", in)
		}
		if rec := op().do(t, http.MethodDelete, "/api/settings/mcp.instructions_enabled", ""); rec.Code != http.StatusOK {
			t.Fatalf("DELETE enabled = %d body=%s", rec.Code, rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != own {
			t.Fatalf("instructions after re-enabling = %q, want the custom text", got)
		}
	})

	t.Run("InvalidText_IsRefusedBeforePersist", func(t *testing.T) {
		cases := map[string]struct{ value, want string }{
			"over the cap": {strings.Repeat("x", instructionsCap+1), "at most 2048"},
			"blank":        {" \n\t ", "blank"},
			"control char": {"look it up\x1b[2J", "control character"},
		}
		for name, c := range cases {
			rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, c.value))
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), c.want) {
				t.Errorf("%s: PUT = %d body=%s, want 422 naming %q", name, rec.Code, rec.Body.String(), c.want)
			}
		}
		atCap := strings.Repeat("ä", instructionsCap) // characters, not bytes
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, atCap)); rec.Code != http.StatusOK {
			t.Errorf("PUT of exactly %d characters = %d body=%s, want 200", instructionsCap, rec.Code, rec.Body.String())
		}
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own)); rec.Code != http.StatusOK {
			t.Fatalf("PUT back = %d body=%s", rec.Code, rec.Body.String())
		}
	})

	t.Run("TenantKeys_CannotSetTheText", func(t *testing.T) {
		rec := api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, "tenant text"))
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("tenant-admin PUT = %d body=%s, want 422 (global-only key)", rec.Code, rec.Body.String())
		}
		rec = api.as(tenantMember("tenanta")).do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, "member text"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("tenant member PUT = %d body=%s, want 403", rec.Code, rec.Body.String())
		}
		rec = api.as(tenantAdmin("tenanta")).do(t, http.MethodPut, "/api/settings/mcp.instructions_enabled", `{"value":false}`)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("tenant-admin PUT enabled = %d body=%s, want 422 (global-only key)", rec.Code, rec.Body.String())
		}
		if _, ok := scopeRowValue(t, pool, "mcp.instructions", "tenanta"); ok {
			t.Errorf("a tenant-scope mcp.instructions row was written")
		}
		if got := handshakeInstructions(t, server); got != own {
			t.Errorf("instructions = %q, want the operator's text unchanged", got)
		}
	})

	t.Run("StoredOldShippedWording_FollowsTheNewest", func(t *testing.T) {
		rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, shippedInstructionsV1))
		if rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "equals shipped wording #1") {
			t.Errorf("PUT of shipped wording #1 answered %s, want a warning that it is no own text", rec.Body.String())
		}
		if got := handshakeInstructions(t, server); got != shippedInstructionsV2 {
			t.Fatalf("instructions = %q, want the newest shipped wording (a stored shipped wording is no custom text)", got)
		}
		if in := getInstructions(t, api).Setting.Instructions; in.State != "shipped" || in.Custom || in.Matches != 1 {
			t.Errorf("comparison = %+v, want shipped, matches #1", in)
		}
	})

	t.Run("Reset_FollowsTheShippedWordingAgain", func(t *testing.T) {
		if rec := op().do(t, http.MethodPut, "/api/settings/mcp.instructions", putJSON(t, own)); rec.Code != http.StatusOK {
			t.Fatalf("PUT = %d body=%s", rec.Code, rec.Body.String())
		}
		if rec := op().do(t, http.MethodDelete, "/api/settings/mcp.instructions", ""); rec.Code != http.StatusOK {
			t.Fatalf("DELETE = %d body=%s", rec.Code, rec.Body.String())
		}
		if _, ok := rowValue(); ok {
			t.Errorf("the override row survived the reset")
		}
		if got := handshakeInstructions(t, server); got != shippedInstructionsV2 {
			t.Fatalf("instructions after reset = %q, want the shipped wording", got)
		}
		if g := getInstructions(t, api); g.Setting.Source != "default" || g.Setting.Instructions.State != "shipped" {
			t.Errorf("after reset: source %q state %q, want default/shipped", g.Setting.Source, g.Setting.Instructions.State)
		}
	})
}
