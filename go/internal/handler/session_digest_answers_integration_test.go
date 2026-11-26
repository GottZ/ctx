//go:build integration

// Issue #289 point 4: the answers the Hermes plugin ctx_checkpoint reads when it
// names the block type session-digest on its digests and facts, pinned against
// the shared fixture integrations/hermes/plugin/ctx_checkpoint/memory/tests/
// mcp_answers.json (section session_digest). The plugin falls back to a store
// without the type on exactly the refusal below, so its wording and code are the
// server's own, and the plugin releases the refused write without a slot of the
// key's write window, so a refusal books nothing.
//
//	go test -tags=integration -p 1 ./internal/handler/ -run 'TestSessionDigestAnswersMatchCheckpointPluginFixture' -count=1 -v
package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/store"
)

type sessionDigestAnswers struct {
	SessionDigest struct {
		Type        string `json:"type"`
		UnknownType string `json:"unknown_type"`
		RestCode    string `json:"rest_code"`
	} `json:"session_digest"`
}

func TestSessionDigestAnswersMatchCheckpointPluginFixture(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	raw, err := os.ReadFile(filepath.FromSlash(checkpointAnswersFixture))
	if err != nil {
		t.Fatalf("read the shared fixture: %v", err)
	}
	var fx sessionDigestAnswers
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("parse the shared fixture: %v", err)
	}
	sd := fx.SessionDigest
	if sd.Type == "" || !strings.Contains(sd.UnknownType, "{type}") || sd.RestCode == "" {
		t.Fatalf("the fixture's session_digest section is incomplete: %+v", sd)
	}
	f := newI201Fixture(t)
	ctx := context.Background()
	ar := AuthResultFromContext(f.direct)

	// The type the plugin names is known: both surfaces accept it.
	sdtStoreMCP(t, f, map[string]any{
		"category": "agent-briefing", "title": "Session digest 0sdanswr seg 001 2026-11-26",
		"content": "Intent: the answers of the store", "type": sd.Type, "sensitivity": "internal",
	})
	code, resp := roRestStore(t, roStoreHandler(f), ar, map[string]any{
		"category": "learnings", "title": "fact: the answers of the store are pinned",
		"content": "The answers of the store are pinned.", "type": sd.Type,
	})
	if code != http.StatusOK || resp["success"] != true {
		t.Fatalf("REST store with type %s: code %d resp %v, want accepted", sd.Type, code, resp)
	}

	// The refusal of a type the server does not know, as the plugin reads it
	// from a server without migration 196.
	before, err := store.CheckRateLimit(ctx, f.pool, ar.ApiKeyID)
	if err != nil {
		t.Fatalf("rate count: %v", err)
	}
	const unknown = "session-digest-unknown"
	want := strings.ReplaceAll(sd.UnknownType, "{type}", unknown)
	a := f.call(t, f.direct, "store", map[string]any{
		"category": "agent-briefing", "title": "Session digest 0sdanswr seg 002 2026-11-26",
		"content": "Intent: an unknown type", "type": unknown, "sensitivity": "internal",
	})
	if !a.IsError || a.Text != want || a.Code != sd.RestCode {
		t.Errorf("MCP store with an unknown type: IsError=%t text=%q code=%q, the fixture says %q code %q",
			a.IsError, a.Text, a.Code, want, sd.RestCode)
	}
	code, resp = roRestStore(t, roStoreHandler(f), ar, map[string]any{
		"category": "learnings", "title": "fact: an unknown type is refused",
		"content": "An unknown type is refused.", "type": unknown,
	})
	if code != http.StatusUnprocessableEntity || resp["code"] != sd.RestCode || resp["error"] != want {
		t.Errorf("REST store with an unknown type: code %d resp %v, want 422 code %q error %q", code, resp, sd.RestCode, want)
	}
	after, err := store.CheckRateLimit(ctx, f.pool, ar.ApiKeyID)
	if err != nil {
		t.Fatalf("rate count: %v", err)
	}
	if after != before {
		t.Errorf("the two refused stores booked %d writes of the key's window, want none", after-before)
	}
}
