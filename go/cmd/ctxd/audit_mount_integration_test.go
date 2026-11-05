//go:build integration

// W06-7 mount probe: the PRODUCTION router serves GET /api/audit behind
// RequireAdminOrTenantAdmin (design/06 §4.10, N-06-16) — a server-admin and a
// tenant-admin get 200, a member key 403, no key 401. Red against a router
// without the mount (404 for every caller).

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestAuditMount_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	cfgStore := config.NewStore(cc)
	backendPool := backends.NewPool(pool, nil)
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{})
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	d := dispatch.New(nil, dispatch.DefaultSettings())
	t.Cleanup(d.Close)
	scheduler.SetDispatcher(d)

	const adminKey = "aa1120261103000000000000000000000a0d17a0d17a0d17a0d17a0d17a0d17a"
	if created, _, err := store.BootstrapAdminKey(ctx, pool, adminKey, "w067-mount-admin"); err != nil || !created {
		t.Fatalf("bootstrap admin key: created=%v err=%v", created, err)
	}
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktype.NewRegistry(), projectHub, d)

	do := func(method, path, key string, body any) (int, map[string]any) {
		t.Helper()
		var rd *bytes.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		} else {
			rd = bytes.NewReader(nil)
		}
		req := httptest.NewRequest(method, path, rd)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		var out map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	status, resp := do(http.MethodPost, "/api/manage", adminKey, map[string]any{
		"action": "tenant-create", "data": map[string]any{"slug": "w067-mount", "display_name": "W06-7 mount"},
	})
	ownerKey, _ := resp["owner_key"].(string)
	if status != http.StatusOK || ownerKey == "" {
		t.Fatalf("tenant-create: status %d resp %v", status, resp)
	}
	status, resp = do(http.MethodPost, "/api/manage", adminKey, map[string]any{
		"action": "api-key-create", "data": map[string]any{"label": "w067-member", "home_scope": "private"},
	})
	memberKey, _ := resp["api_key"].(string)
	if status != http.StatusOK || memberKey == "" {
		t.Fatalf("api-key-create: status %d resp %v", status, resp)
	}

	for _, c := range []struct {
		name, key string
		want      int
		rows      int
	}{
		{"server_admin", adminKey, http.StatusOK, 3}, // bootstrap admin, tenant owner, member
		{"tenant_admin", ownerKey, http.StatusOK, 1}, // its own owner key only
		{"member", memberKey, http.StatusForbidden, -1},
		{"no_key", "", http.StatusUnauthorized, -1},
	} {
		t.Run(c.name, func(t *testing.T) {
			status, resp := do(http.MethodGet, "/api/audit?entity=api_key", c.key, nil)
			if status != c.want {
				t.Fatalf("GET /api/audit as %s: status %d, want %d (resp %v)", c.name, status, c.want, resp)
			}
			if c.rows < 0 {
				return
			}
			rows, _ := resp["rows"].([]any)
			if len(rows) != c.rows {
				t.Errorf("GET /api/audit as %s: %d row(s), want %d (resp %v)", c.name, len(rows), c.rows, resp)
			}
		})
	}
}
