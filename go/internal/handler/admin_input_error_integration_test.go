//go:build integration

package handler

// Issue #505: a value the database cannot store — a NUL character in text or
// JSON, a value longer than its column, a number out of its column's range —
// is the caller's mistake, like a taken backend name: the admin write routes
// that log their failures since #483 answer it 4xx and write no line. Before,
// several of them answered 500 (or 422) with an ERROR line, so an admin key
// could write ERROR lines at will (deploy.sh counts them after a deploy). A
// field of the wrong JSON type is refused before the store, as it was.
//
// The test drives each route with each kind of input error and prints the
// table route × input error ⇒ status, the lines above DEBUG. A display name
// and the backend fields are TEXT and JSONB without a length limit: no value
// is too long for them.
//
//	go test -tags=integration ./internal/handler/ -run 'TestAdminWriteInputErrors' -count=1 -v

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// inputErrorCase is one route driven with one input error.
type inputErrorCase struct {
	route, input string
	h            *ManageHandler
	as           *auth.AuthResult
	payload      map[string]any
	field        string // the field the 422 names (fields[].field), when the route refuses before the store
}

const i505NUL = "i505\x00nul"

var i505Long = strings.Repeat("l", 4096)

// runInputErrorCases runs every case and checks that each is answered with
// the status want names for it ("route | input") and without a line above
// DEBUG; it prints the table.
func runInputErrorCases(t *testing.T, cases []inputErrorCase, want map[string]int) {
	t.Helper()
	var table []string
	for i, c := range cases {
		t.Run(c.route+" "+c.input, func(t *testing.T) {
			wantStatus, ok := want[c.route+" | "+c.input]
			if !ok {
				t.Fatalf("no expected status for %s | %s", c.route, c.input)
			}
			logs := installRejectLogCapture(t)
			status, resp := kaManage(t, c.h, c.as, fmt.Sprintf("i505-%d", i), c.payload)
			var lines []string
			for _, r := range logs.all() {
				if r.level > slog.LevelDebug {
					lines = append(lines, r.level.String()+" "+r.msg)
				}
			}
			table = append(table, fmt.Sprintf("| %s | %s | %d | %s |", c.route, c.input, status, strings.Join(lines, "; ")))
			if status != wantStatus {
				t.Errorf("%s with %s answered %d %v, want %d: the request's value is the caller's mistake", c.route, c.input, status, resp, wantStatus)
			}
			if len(lines) != 0 {
				t.Errorf("%s with %s wrote %v, want no line above DEBUG", c.route, c.input, lines)
			}
			if c.field != "" && !namesField(resp, c.field) {
				t.Errorf("%s with %s answered %v, want a 422 naming the field %q (refused before the store)", c.route, c.input, resp, c.field)
			}
			for _, r := range logs.all() {
				if strings.Contains(r.text(), "i505") && strings.Contains(r.text(), "nul") {
					t.Errorf("a line carries the request's value: %s", r.text())
				}
			}
		})
	}
	sort.Strings(table)
	t.Logf("route × input error ⇒ status, lines above DEBUG:\n| route | input | status | lines |\n|---|---|---|---|\n%s", strings.Join(table, "\n"))
}

// namesField reports whether a validation answer lists field in its fields.
func namesField(resp map[string]any, field string) bool {
	fields, _ := resp["fields"].([]any)
	for _, f := range fields {
		if m, _ := f.(map[string]any); m["field"] == field {
			return true
		}
	}
	return false
}

func TestAdminWriteInputErrorsTenantRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := mcHandler(t, pool, false, false)
	a := auMakeTenant(ctx, t, pool, h, admin, "i505-a")
	b := auMakeTenant(ctx, t, pool, h, admin, "i505-b")
	block := mustBlock(t, pool, "private")
	if _, err := pool.Exec(ctx, `INSERT INTO context_settings (key, scope, value) VALUES ($1, $2, 'true')`,
		store.AllowCrossTenantBlockGrantKey, admin.TenantID); err != nil {
		t.Fatalf("opt in: %v", err)
	}
	c := func(route, input string, as *auth.AuthResult, payload map[string]any) inputErrorCase {
		if as == nil {
			as = admin
		}
		payload["action"] = route
		return inputErrorCase{route: route, input: input, h: h, as: as, payload: payload}
	}
	data := func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	cases := []inputErrorCase{
		c("tenant-create", "NUL", nil, map[string]any{"data": data("slug", "i505-n", "display_name", i505NUL)}),
		c("tenant-create", "invalid JSON field", nil, map[string]any{"data": data("slug", "i505-j", "display_name", 5)}),
		c("tenant-update", "NUL", nil, map[string]any{"id": a.id, "data": data("display_name", i505NUL)}),
		c("tenant-update", "invalid JSON field", nil, map[string]any{"id": a.id, "data": data("display_name", 5)}),
		c("tenant-limit-set", "out of range", nil, map[string]any{"id": a.id, "data": data("max_scopes", 1e12)}),
		c("tenant-limit-set", "invalid JSON field", nil, map[string]any{"id": a.id, "data": data("max_scopes", "seven")}),
		c("scope-create", "NUL", a.ar, map[string]any{"data": data("name", i505NUL)}),
		c("scope-create", "too long", a.ar, map[string]any{"data": data("name", i505Long)}),
		c("scope-create", "invalid JSON field", a.ar, map[string]any{"data": data("name", 5)}),
		c("tenant-grant-create", "NUL", nil, map[string]any{"data": data("grantee_tenant", a.id, "granted_scope", i505NUL)}),
		c("tenant-grant-create", "too long", nil, map[string]any{"data": data("grantee_tenant", a.id, "granted_scope", i505Long)}),
		c("tenant-grant-create", "invalid JSON field", nil, map[string]any{"data": data("grantee_tenant", 5, "granted_scope", b.scope)}),
		c("tenant-grant-delete", "NUL", nil, map[string]any{"id": i505NUL}),
		c("tenant-grant-delete", "too long", nil, map[string]any{"id": i505Long}),
		c("tenant-quota-set", "NUL", nil, map[string]any{"data": data("scope", a.scope, "on_exceed", i505NUL)}),
		c("tenant-quota-set", "out of range", nil, map[string]any{"data": data("scope", a.scope, "daily_cost_usd", 1e300)}),
		c("tenant-quota-set", "invalid JSON field", nil, map[string]any{"data": data("scope", a.scope, "daily_calls", "x")}),
		c("tenant-quota-set", "calls out of range", nil, map[string]any{"data": data("scope", a.scope, "daily_calls", 3000000000)}),
		c("block-grant-create", "NUL", nil, map[string]any{"data": data("block_id", block, "grantee_tenant", i505NUL)}),
		c("block-grant-create", "too long", nil, map[string]any{"data": data("block_id", block, "grantee_tenant", i505Long)}),
		c("block-grant-create", "invalid JSON field", nil, map[string]any{"data": data("block_id", 5, "grantee_tenant", a.id)}),
		c("block-grant-revoke", "NUL", nil, map[string]any{"data": data("block_id", block, "grantee_tenant", i505NUL)}),
		c("block-grant-revoke", "too long", nil, map[string]any{"data": data("block_id", block, "grantee_tenant", i505Long)}),
		c("tenant-delete", "NUL", nil, map[string]any{"id": i505NUL}),
		c("tenant-delete", "too long", nil, map[string]any{"id": i505Long}),
	}
	runInputErrorCases(t, cases, map[string]int{
		"tenant-create | NUL": 400, "tenant-create | invalid JSON field": 400,
		"tenant-update | NUL": 400, "tenant-update | invalid JSON field": 400,
		"tenant-limit-set | out of range": 400, "tenant-limit-set | invalid JSON field": 400,
		"scope-create | NUL": 400, "scope-create | too long": 400, "scope-create | invalid JSON field": 400,
		"tenant-grant-create | NUL": 400, "tenant-grant-create | too long": 400, "tenant-grant-create | invalid JSON field": 400,
		"tenant-grant-delete | NUL": 400, "tenant-grant-delete | too long": 404,
		"tenant-quota-set | NUL": 422, "tenant-quota-set | out of range": 400, "tenant-quota-set | invalid JSON field": 400,
		"tenant-quota-set | calls out of range": 400,
		"block-grant-create | NUL": 400, "block-grant-create | too long": 400, "block-grant-create | invalid JSON field": 400,
		"block-grant-revoke | NUL": 400, "block-grant-revoke | too long": 404,
		// An id that is no uuid names no tenant: 404 before PruneTenant runs.
		"tenant-delete | NUL": 404, "tenant-delete | too long": 404,
	})
}

func TestAdminWriteInputErrorsBackendRoutes(t *testing.T) {
	ph := setupProfileHarness(t)
	c := func(route, input string, payload map[string]any) inputErrorCase {
		payload["action"] = route
		return inputErrorCase{route: route, input: input, h: ph.h, as: ph.admin, payload: payload}
	}
	nul := func(route, input, field string, payload map[string]any) inputErrorCase {
		ic := c(route, input, payload)
		ic.field = field
		return ic
	}
	spec := func(name string, kv ...any) map[string]any {
		d := map[string]any{"name": name, "base_url": "http://" + name, "roles": []string{"chat"},
			"model_map": map[string]any{"default": "stub-model"}}
		for i := 0; i+1 < len(kv); i += 2 {
			d[kv[i].(string)] = kv[i+1]
		}
		return d
	}
	upd := ph.byID["chat-a"]
	var cases []inputErrorCase
	for _, route := range []string{"backend-create", "backend-update"} {
		withID := func(d map[string]any) map[string]any {
			p := map[string]any{"data": d}
			if route == "backend-update" {
				p["id"] = upd
				delete(d, "name")
				delete(d, "base_url")
			}
			return p
		}
		cases = append(cases,
			nul(route, "NUL in extra_headers", "extra_headers", withID(spec("i505-h", "extra_headers", map[string]any{"X-I505": i505NUL}))),
			nul(route, "NUL in metadata", "metadata", withID(spec("i505-m", "metadata", map[string]any{"note": i505NUL}))),
			nul(route, "NUL in extra_body", "extra_body", withID(spec("i505-b", "extra_body", map[string]any{"opt": i505NUL}))),
			nul(route, "NUL in a metadata key", "metadata", withID(spec("i505-k", "metadata", map[string]any{i505NUL: "v"}))),
			// api_key is no backend field (api_key_ref names the key): the write
			// ignores it, and answered 200 before the NUL check.
			nul(route, "NUL in a key the write does not read", "api_key", withID(spec("i505-a", "api_key", i505NUL))),
			c(route, "out of range", withID(spec("i505-r", "priority", 1e12))),
			c(route, "invalid JSON field", withID(spec("i505-j", "priority", "high"))),
		)
	}
	// The scope column is VARCHAR(50); scope is immutable through update. A
	// tenant row may not point at a private base_url, so the host is public.
	cases = append(cases, c("backend-create", "too long scope", map[string]any{"data": spec("i505-s",
		"scope", i505Long[:51], "base_url", "https://i505.example.org")}))
	want := map[string]int{"backend-create | too long scope": 422}
	for _, route := range []string{"backend-create", "backend-update"} {
		for _, input := range []string{"NUL in extra_headers", "NUL in metadata", "NUL in extra_body", "NUL in a metadata key",
			"NUL in a key the write does not read", "out of range"} {
			want[route+" | "+input] = 422
		}
		want[route+" | invalid JSON field"] = 400
	}
	runInputErrorCases(t, cases, want)
}

func TestAdminWriteInputErrorsModelAliasRoutes(t *testing.T) {
	m := setupModelAliasHarness(t)
	upd := m.seed("ma-a", "i505-upd", "chat", "chat", "credentials", "{}")
	c := func(route, input string, payload map[string]any) inputErrorCase {
		payload["action"] = route
		return inputErrorCase{route: route, input: input, h: m.h, as: m.ten, payload: payload}
	}
	cases := []inputErrorCase{
		c("model-alias-create", "NUL", map[string]any{"data": chatSpec(i505NUL, nil)}),
		c("model-alias-create", "NUL in policy", map[string]any{"data": chatSpec("i505-p", map[string]any{"policy": map[string]any{"note": i505NUL}})}),
		c("model-alias-create", "too long", map[string]any{"data": chatSpec(i505Long, nil)}),
		c("model-alias-create", "invalid JSON field", map[string]any{"data": chatSpec("i505-j", map[string]any{"enabled": "yes"})}),
		c("model-alias-update", "NUL in policy", map[string]any{"id": upd, "data": map[string]any{"policy": map[string]any{"note": i505NUL}}}),
		c("model-alias-update", "invalid JSON field", map[string]any{"id": upd, "data": map[string]any{"enabled": "yes"}}),
		c("model-alias-delete", "NUL", map[string]any{"id": i505NUL}),
		c("model-alias-delete", "too long", map[string]any{"id": i505Long}),
	}
	runInputErrorCases(t, cases, map[string]int{
		"model-alias-create | NUL": 422, "model-alias-create | NUL in policy": 422, "model-alias-create | too long": 422,
		"model-alias-create | invalid JSON field": 400, "model-alias-update | NUL in policy": 422,
		"model-alias-update | invalid JSON field": 400, "model-alias-delete | NUL": 404, "model-alias-delete | too long": 404,
	})
}

func TestAdminWriteInputErrorsEmbedMigrationRoutes(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	admin, _ := kaServerAdmin(context.Background(), t, pool)
	h := NewManageHandler(pool, nil, nil, backends.NewPool(nil, nil), nil, nil, nil, nil)
	emcSeedModels(t, pool)
	emcSeedBackend(t, pool)
	c := func(route, input string, payload map[string]any) inputErrorCase {
		payload["action"] = route
		return inputErrorCase{route: route, input: input, h: h, as: admin, payload: payload}
	}
	create := func(from, to, backend any) map[string]any {
		return map[string]any{"data": map[string]any{"from_model": from, "to_model": to, "to_backend": backend}}
	}
	runInputErrorCases(t, []inputErrorCase{
		c("embed-migration-create", "NUL", create(emcFromModel, emcToModel, i505NUL)),
		c("embed-migration-create", "NUL in a model", create(i505NUL, emcToModel, emcBackendName)),
		c("embed-migration-create", "too long", create(emcFromModel, emcToModel, i505Long)),
		c("embed-migration-create", "invalid JSON field", create(5, emcToModel, emcBackendName)),
		c("embed-migration-pause", "NUL", map[string]any{"id": i505NUL}),
		c("embed-migration-rollback", "NUL in the reason", map[string]any{"id": "01900000-0000-7000-8000-000000000505", "data": map[string]any{"reason": i505NUL}}),
	}, map[string]int{
		"embed-migration-create | NUL": 400, "embed-migration-create | NUL in a model": 400, "embed-migration-create | too long": 400,
		"embed-migration-create | invalid JSON field": 400, "embed-migration-pause | NUL": 404,
		"embed-migration-rollback | NUL in the reason": 404,
	})
	// A migration the abort finds: its reason reaches the database.
	status, resp := kaManage(t, h, admin, "i505-live", map[string]any{"action": "embed-migration-create",
		"data": map[string]any{"from_model": emcFromModel, "to_model": emcToModel, "to_backend": emcBackendName}})
	live, _ := resp["id"].(string)
	if status != 200 || live == "" {
		t.Fatalf("create a migration: %d %v", status, resp)
	}
	runInputErrorCases(t, []inputErrorCase{
		c("embed-migration-abort", "NUL in the reason", map[string]any{"id": live, "data": map[string]any{"reason": i505NUL}}),
	}, map[string]int{"embed-migration-abort | NUL in the reason": 400})
}

// TestTenantDeleteFailureKeepsItsLine — tenant-delete refuses an id that is
// no uuid before PruneTenant runs (404, as an unknown tenant), and by the
// time a later statement of PruneTenant fails, earlier ones have committed:
// every failure it returns is the server's, 500 with the ERROR line and
// write_outcome=unknown, whatever its SQLSTATE — here a class-22 refusal of
// a server value in the audit stamp (the actor key id, the request id) after
// the tenant's blocks are gone.
func TestTenantDeleteFailureKeepsItsLine(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := mcHandler(t, pool, false, false)
	for _, c := range []struct{ name, keyID, requestID, code, slug string }{
		{"actor key id that is no uuid", "i505-not-a-uuid", "i505-del", "22P02", "i505-del-a"},
		{"NUL in the request id", admin.ApiKeyID, "i505\x00del", "22021", "i505-del-b"},
	} {
		t.Run(c.name, func(t *testing.T) {
			tn := auMakeTenant(ctx, t, pool, h, admin, c.slug)
			block := mustBlock(t, pool, tn.scope)
			as := *admin
			as.ApiKeyID = c.keyID
			logs := installRejectLogCapture(t)
			status, resp := kaManage(t, h, &as, c.requestID, map[string]any{"action": "tenant-delete", "id": tn.id})
			if status != http.StatusInternalServerError {
				t.Fatalf("tenant-delete failing with %s answered %d %v, want 500", c.code, status, resp)
			}
			lines := logs.withMsg("manage: tenant-delete failed")
			if len(lines) != 1 || lines[0].level != slog.LevelError || lines[0].attrs["write_outcome"] != writeUnknown ||
				!strings.Contains(lines[0].text(), c.code) {
				t.Errorf("lines %+v, want one ERROR naming %s with write_outcome=%s", logs.all(), c.code, writeUnknown)
			}
			var blocks, tenants int
			if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_blocks WHERE id = $1::uuid),
				(SELECT count(*) FROM context_tenants WHERE id = $2::uuid)`, block, tn.id).Scan(&blocks, &tenants); err != nil {
				t.Fatal(err)
			}
			if blocks != 0 || tenants != 1 {
				t.Errorf("after the failure: %d blocks of the tenant, %d tenant rows, want 0 and 1 — the fixture did not fail after a committed step", blocks, tenants)
			}
		})
	}
}

// TestTenantDeleteGuardsTheDefaultTenantInEverySpelling — the default tenant
// cannot be deleted, however its id is spelled: PostgreSQL reads an id in
// capitals, in braces or without hyphens as the same uuid, so the guard
// compares the parsed id, not the string. Before, such a spelling passed the
// guard and pruned the default tenant.
func TestTenantDeleteGuardsTheDefaultTenantInEverySpelling(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	admin, _ := kaServerAdmin(ctx, t, pool)
	h := mcHandler(t, pool, false, false)
	block := mustBlock(t, pool, "private")
	for _, id := range []string{
		store.DefaultTenantID,
		strings.ToUpper(store.DefaultTenantID),
		"{" + store.DefaultTenantID + "}",
		strings.ReplaceAll(store.DefaultTenantID, "-", ""),
	} {
		status, resp := kaManage(t, h, admin, "i505-default", map[string]any{"action": "tenant-delete", "id": id})
		if status != http.StatusBadRequest {
			t.Errorf("tenant-delete %q answered %d %v, want 400: the default tenant cannot be deleted", id, status, resp)
		}
	}
	var blocks, tenants int
	if err := pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM context_blocks WHERE id = $1::uuid),
		(SELECT count(*) FROM context_tenants WHERE id = $2::uuid)`, block, store.DefaultTenantID).Scan(&blocks, &tenants); err != nil {
		t.Fatal(err)
	}
	if blocks != 1 || tenants != 1 {
		t.Fatalf("after the refused deletes: %d blocks in private, %d default tenant rows, want 1 and 1", blocks, tenants)
	}
}
