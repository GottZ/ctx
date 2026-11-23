//go:build integration

package handler

// Issue #519: a NUL character (U+0000) in any text of a /api/manage request
// is refused by the dispatcher, before the action runs: PostgreSQL stores
// it neither in text (22021) nor in JSON (22P05), so every query it reaches
// fails, and an action whose failing query was a lookup ahead of its write
// answered 500 without a line (scope-create's tenant lookup) — #505 had
// classified the database's refusal only where a write failed. Now every
// action answers 400 naming the field, writes no line, and its handler never
// runs.
//
// The test drives EVERY action of the production table (manageActions) as a
// server admin, twice: a NUL in each top-level text field, and a NUL in the
// values of a broad set of data keys (strings, arrays, an object and an
// object key). It prints the table action × input ⇒ status, lines above
// DEBUG.
//
//	go test -tags=integration ./internal/handler/ -run 'TestManageRefusesANULCharacterInEveryAction' -count=1 -v

import (
	"context"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

const nulValue = "i519\x00nul"

// nulDataKeys are the data keys the actions read text from, by a sweep over
// their request structs; a key an action does not read is ignored by it.
var nulDataKeys = []string{
	"alias", "allowed", "base_url", "block_id", "body", "category", "client_id", "client_name", "comment",
	"consumer", "content", "description", "display_name", "email", "from_id", "host", "id", "issuer",
	"key_id", "kind", "label", "link_id", "mode", "model", "name", "note", "owner", "pattern", "principal",
	"profile", "project", "project_id", "protocol", "provider", "q", "query", "reason", "ref", "repo",
	"role", "scope", "secret", "slug", "source", "state", "status", "subject", "summary", "target",
	"tenant", "tenant_id", "text", "title", "to_id", "token", "type", "url", "version",
}

// nulData is a data object whose every key in nulDataKeys carries a NUL,
// plus arrays, an object and an object key with one.
func nulData() map[string]any {
	d := map[string]any{}
	for _, k := range nulDataKeys {
		d[k] = nulValue
	}
	for _, k := range []string{"scopes", "roles", "types", "tags", "members", "keywords"} {
		d[k] = []any{nulValue}
	}
	d["metadata"] = map[string]any{"note": nulValue, nulValue: "key"}
	return d
}

func TestManageRefusesANULCharacterInEveryAction(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	cfg := staticConfigStore{cfg: config.Defaults()}
	h := NewManageHandler(pool, cfg, &fakeDreamController{}, backends.NewPool(pool, nil), nil, nil, nil, reg)

	inputs := []struct {
		name    string
		payload func(action string) map[string]any
	}{
		{"NUL in the top-level fields", func(a string) map[string]any {
			return map[string]any{"action": a, "id": nulValue, "category": nulValue, "status": nulValue, "types": []any{nulValue}}
		}},
		{"NUL in data", func(a string) map[string]any {
			return map[string]any{"action": a, "id": "019a0000-0000-7000-8000-000000000519", "data": nulData()}
		}},
	}
	var table []string
	for _, action := range slices.Sorted(maps.Keys(manageActions)) {
		for _, in := range inputs {
			t.Run(action+" "+in.name, func(t *testing.T) {
				logs := installRejectLogCapture(t)
				status, resp, panicked := manageWithin(t, h, admin, in.payload(action))
				var lines []string
				for _, r := range logs.all() {
					if r.level > slog.LevelDebug {
						lines = append(lines, r.level.String()+" "+r.msg)
					}
				}
				table = append(table, fmt.Sprintf("| %s | %s | %d%s | %s |", action, in.name, status, panicked, strings.Join(lines, "; ")))
				if status != http.StatusBadRequest || panicked != "" {
					t.Errorf("%s with %s answered %d%s %v, want 400 — a value the database cannot store is the caller's mistake", action, in.name, status, panicked, resp)
				}
				if msg, _ := resp["error"].(string); !strings.Contains(msg, "NUL") {
					t.Errorf("%s with %s answered %v, want an error naming the NUL character", action, in.name, resp)
				}
				if len(lines) != 0 {
					t.Errorf("%s with %s wrote %v, want no line above DEBUG", action, in.name, lines)
				}
			})
		}
	}
	t.Logf("action × input ⇒ status, lines above DEBUG:\n%s", strings.Join(table, "\n"))
}

// manageWithin drives /api/manage like kaManage, with a deadline and a
// recovered panic reported as " panic: …" instead of ending the test binary.
func manageWithin(t *testing.T, h *ManageHandler, ar *auth.AuthResult, payload map[string]any) (status int, resp map[string]any, panicked string) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				panicked = fmt.Sprintf(" panic: %v", r)
			}
		}()
		status, resp = kaManage(t, h, ar, "i519-nul", payload)
	}()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatalf("%v did not answer within 30 s", payload["action"])
	}
	return status, resp, panicked
}
