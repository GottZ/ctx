//go:build integration

// Issue #565, part 1: an update of a block type through type-update or PUT
// /api/types lays the request's config over the row's stored config. A field
// the request leaves out keeps its stored value; null resets it. Every probe
// drives a production handler with a real registry, and asserts what the row
// stores and what the registry serves afterwards.
//
// Run with:
//
//	go test -tags=integration ./internal/handler/ -run TestTypeUpdate565 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// t565Transport is one of the two update transports: it sends cfg as the
// config of an update of name and returns status and body.
type t565Transport struct {
	label string
	send  func(t *testing.T, ar *auth.AuthResult, name, cfg string) *httptest.ResponseRecorder
}

func t565Transports(pool *pgxpool.Pool, reg *blocktype.Registry) []t565Transport {
	mh := NewManageHandler(pool, nil, nil, nil, nil, nil, nil, reg)
	return []t565Transport{
		{"manage type-update", func(t *testing.T, ar *auth.AuthResult, name, cfg string) *httptest.ResponseRecorder {
			t.Helper()
			body := fmt.Sprintf(`{"action":"type-update","id":%q,"data":{"config":%s}}`, name, cfg)
			req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			req = req.WithContext(context.WithValue(req.Context(), authResultKey, ar))
			rec := httptest.NewRecorder()
			mh.HandleManage(rec, req)
			return rec
		}},
		{"PUT /api/types", func(t *testing.T, ar *auth.AuthResult, name, cfg string) *httptest.ResponseRecorder {
			t.Helper()
			return c24TypesReq(t, pool, reg, ar, http.MethodPut, "/api/types/"+name, `{"config":`+cfg+`}`)
		}},
	}
}

// t565Stored is the row's stored config, compacted.
func t565Stored(t *testing.T, pool *pgxpool.Pool, name, scope string) string {
	t.Helper()
	var raw []byte
	if err := pool.QueryRow(context.Background(),
		`SELECT config FROM context_block_types WHERE name = $1 AND scope = $2`, name, scope).Scan(&raw); err != nil {
		t.Fatalf("read %s/%s: %v", name, scope, err)
	}
	var b bytes.Buffer
	_ = json.Compact(&b, raw)
	return b.String()
}

// t565Restore writes the row's original config back through PUT (a config
// naming every stored field writes itself, TestTypeUpdate565_FullConfig).
func t565Restore(t *testing.T, pool *pgxpool.Pool, reg *blocktype.Registry, ar *auth.AuthResult, name, scope, cfg string) {
	t.Helper()
	if rec := c24TypesReq(t, pool, reg, ar, http.MethodPut, "/api/types/"+name, `{"config":`+cfg+`}`); rec.Code != http.StatusOK {
		t.Fatalf("restore %s: %d %s", name, rec.Code, rec.Body.String())
	}
	if got := t565Stored(t, pool, name, scope); !jsonEqual(got, cfg) {
		t.Fatalf("restore %s: stored %s, want %s", name, got, cfg)
	}
}

func jsonEqual(a, b string) bool {
	var x, y any
	if json.Unmarshal([]byte(a), &x) != nil || json.Unmarshal([]byte(b), &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

func t565Fixture(t *testing.T) (*pgxpool.Pool, *blocktype.Registry, *auth.AuthResult, *auth.AuthResult) {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	reg := blocktype.NewRegistry()
	reg.Boot(ctx, pool)
	if reg.Health() != blocktype.HealthOK {
		t.Fatalf("registry boot degraded: %s", reg.Health())
	}
	actor, _, err := store.CreateApiKey(ctx, pool, "t565-actor", "private", nil, "")
	if err != nil {
		t.Fatalf("create actor key: %v", err)
	}
	serverAdmin := &auth.AuthResult{IsValid: true, IsAdmin: true, ApiKeyID: actor.ID, HomeScope: "private",
		ReadScopes: []string{store.GlobalScope}, TenantID: "_server", TenantRole: auth.RoleMember}
	tenantAdmin := &auth.AuthResult{IsValid: true, ApiKeyID: actor.ID, HomeScope: c24Tenant,
		ReadScopes: []string{c24Tenant}, TenantID: c24TenantID, TenantRole: auth.RoleAdmin}
	return pool, reg, serverAdmin, tenantAdmin
}

// TestTypeUpdate565_PartialKeepsTheRest is the issue's own probe: an operator
// tunes the damping factor of the builtin session-digest and nothing else. RED
// on 184c0012: 200, the row stores only what was sent, the registry serves the
// type full-pass or damped but TRUSTED, guard-checked, dream-linkable and in
// digest and overview.
func TestTypeUpdate565_PartialKeepsTheRest(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	orig := t565Stored(t, pool, "session-digest", store.GlobalScope)
	for _, tr := range t565Transports(pool, reg) {
		for _, body := range []string{
			`{"v":1,"retrieval":{"damping_factor":0.45}}`,
			`{"v":1,"retrieval":{"policy":"damped","damping_factor":0.45}}`,
			`{"v":1,"guard":{"mode":"flag"}}`,
		} {
			t.Run(tr.label+" "+body, func(t *testing.T) {
				t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "session-digest", store.GlobalScope, orig) })
				rec := tr.send(t, serverAdmin, "session-digest", body)
				if rec.Code != http.StatusOK {
					t.Fatalf("status %d, want 200 (%s)", rec.Code, rec.Body.String())
				}
				p, ok := reg.Snapshot().Resolve("session-digest")
				if !ok {
					t.Fatal("session-digest unresolved")
				}
				if !p.Retrieval.Untrusted || p.Retrieval.Kind != blocktype.RetrievalDamped ||
					p.Guard.Check || p.Guard.Candidate || p.Dream.Linkable || p.Digest.Include || p.Overview.Include {
					t.Fatalf("served after %s: untrusted=%v kind=%s guard=%v/%v dream=%v digest=%v overview=%v — an axis the update did not name was reset (stored %s)",
						body, p.Retrieval.Untrusted, p.Retrieval.Kind, p.Guard.Check, p.Guard.Candidate,
						p.Dream.Linkable, p.Digest.Include, p.Overview.Include, t565Stored(t, pool, "session-digest", store.GlobalScope))
				}
				wantFactor := 0.8
				if strings.Contains(body, "damping_factor") {
					wantFactor = 0.45
				}
				if p.Retrieval.DampingFactor != wantFactor {
					t.Fatalf("damping factor = %v, want %v", p.Retrieval.DampingFactor, wantFactor)
				}
				if strings.Contains(body, `"mode":"flag"`) && p.Guard.Mode != blocktype.GuardModeFlag {
					t.Fatalf("guard.mode = %q, want the named flag", p.Guard.Mode)
				}
				// The answer shows what the row stores.
				var resp struct {
					Type struct {
						Config json.RawMessage `json:"config"`
					} `json:"type"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
					t.Fatalf("decode answer: %v", err)
				}
				if got := t565Stored(t, pool, "session-digest", store.GlobalScope); !jsonEqual(string(resp.Type.Config), got) {
					t.Fatalf("answer config %s, row %s", resp.Type.Config, got)
				}
			})
		}
	}
}

// TestTypeUpdate565_FunctionalAxesKept: the call TestTypeManage_Integration
// makes on audit-trail (a new factor with its policy). RED on 184c0012: the
// ten classify title rules, the "dream-" source rule, the intent patterns and
// the structural link class go — a "Session …" title no longer classifies as
// audit-trail.
func TestTypeUpdate565_FunctionalAxesKept(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	orig := t565Stored(t, pool, "audit-trail", store.GlobalScope)
	for _, tr := range t565Transports(pool, reg) {
		t.Run(tr.label, func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "audit-trail", store.GlobalScope, orig) })
			rec := tr.send(t, serverAdmin, "audit-trail", `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5}}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
			}
			set := reg.Snapshot()
			if name, ok := set.Classify("Session handover 565", nil); !ok || name != "audit-trail" {
				t.Fatalf("Classify(\"Session handover 565\") = %q, %v — want audit-trail (title rules kept)", name, ok)
			}
			p, _ := set.Resolve("audit-trail")
			if len(p.Retrieval.IntentPatterns) != 10 || len(p.StructuralLinkClasses) != 1 || p.Retrieval.DampingFactor != 0.5 {
				t.Fatalf("audit-trail: intents %d, links %v, factor %v — want 10, [references], 0.5",
					len(p.Retrieval.IntentPatterns), p.StructuralLinkClasses, p.Retrieval.DampingFactor)
			}
		})
	}
}

// TestTypeUpdate565_TenantRows: the rule holds for a tenant's own type, on
// both transports. RED on 184c0012: the own type loses its untrusted mark, its
// pipeline posture and its title rule. (A tenant overlay of a builtin name:
// TestC24OverlayWriteGate/B.)
func TestTypeUpdate565_TenantRows(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, _, tenantAdmin := t565Fixture(t)
	ctx := context.Background()
	const own = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5,"untrusted":true},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},"classify":{"priority":40,"title_patterns":["sprint note "]}}`
	if rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/sprint565", `{"config":`+own+`}`); rec.Code != http.StatusOK {
		t.Fatalf("create tenant type: %d %s", rec.Code, rec.Body.String())
	}
	serverAdminInTenant := *tenantAdmin
	serverAdminInTenant.IsAdmin = true
	serverAdminInTenant.TenantID, serverAdminInTenant.TenantRole = "_server", auth.RoleMember
	for _, tr := range t565Transports(pool, reg) {
		ar := tenantAdmin
		if tr.label == "manage type-update" {
			ar = &serverAdminInTenant // the manage transport reaches the tenant row of its home scope
		}
		t.Run(tr.label+" own type", func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, tenantAdmin, "sprint565", c24Tenant, own) })
			rec := tr.send(t, ar, "sprint565", `{"v":1,"retrieval":{"damping_factor":0.3}}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
			}
			reg.InvalidateTenant(c24Tenant)
			set := reg.SnapshotForTenant(ctx, c24Tenant)
			p, ok := set.Resolve("sprint565")
			if !ok || !p.Retrieval.Untrusted || p.Retrieval.DampingFactor != 0.3 || p.Guard.Check || p.Dream.Linkable {
				t.Fatalf("served %+v (ok %v), want untrusted, factor 0.3, out of guard and dream", p.Retrieval, ok)
			}
			if name, ok := set.Classify("Sprint note 12", nil); !ok || name != "sprint565" {
				t.Fatalf("Classify = %q %v, want sprint565 (title rule kept)", name, ok)
			}
		})
	}
}

// TestTypeUpdate565_NullResets: a field or section named null goes back to its
// default; everything else stays. RED on 184c0012 for the "rest stays" half.
func TestTypeUpdate565_NullResets(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	orig := t565Stored(t, pool, "session-digest", store.GlobalScope)
	for _, tr := range t565Transports(pool, reg) {
		for _, tc := range []struct {
			body  string
			check func(p blocktype.Policy) string
		}{
			{`{"v":1,"retrieval":{"untrusted":null}}`, func(p blocktype.Policy) string {
				if p.Retrieval.Untrusted || p.Retrieval.Kind != blocktype.RetrievalDamped || p.Retrieval.DampingFactor != 0.8 || p.Guard.Check {
					return fmt.Sprintf("retrieval %+v guard.check %v — want untrusted reset, the rest kept", p.Retrieval, p.Guard.Check)
				}
				return ""
			}},
			{`{"v":1,"retrieval":{"untrusted":false}}`, func(p blocktype.Policy) string {
				if p.Retrieval.Untrusted || p.Retrieval.DampingFactor != 0.8 || p.Dream.Linkable {
					return fmt.Sprintf("retrieval %+v dream %v — want untrusted false, the rest kept", p.Retrieval, p.Dream.Linkable)
				}
				return ""
			}},
			{`{"v":1,"guard":null}`, func(p blocktype.Policy) string {
				if !p.Guard.Check || !p.Guard.Candidate || !p.Retrieval.Untrusted || p.Dream.Linkable {
					return fmt.Sprintf("guard %+v untrusted %v dream %v — want the guard section at its defaults, the rest kept", p.Guard, p.Retrieval.Untrusted, p.Dream.Linkable)
				}
				return ""
			}},
		} {
			t.Run(tr.label+" "+tc.body, func(t *testing.T) {
				t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "session-digest", store.GlobalScope, orig) })
				if rec := tr.send(t, serverAdmin, "session-digest", tc.body); rec.Code != http.StatusOK {
					t.Fatalf("status %d (%s)", rec.Code, rec.Body.String())
				}
				p, _ := reg.Snapshot().Resolve("session-digest")
				if msg := tc.check(p); msg != "" {
					t.Fatalf("%s (stored %s)", msg, t565Stored(t, pool, "session-digest", store.GlobalScope))
				}
			})
		}
	}
}

// TestTypeUpdate565_FullConfig: a config that names every stored field writes
// itself, for every '_global' row of a fresh database: sent back unchanged it
// leaves the row as it was, sent with a value changed it is what the row
// stores. Green on 184c0012 as well — this is the half of the change that must
// NOT move: the admin form and the documented flow send such configs.
func TestTypeUpdate565_FullConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	rows, err := store.ListBlockTypes(context.Background(), pool, []string{store.GlobalScope})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) < 12 {
		t.Fatalf("%d _global rows, want the 12 builtins", len(rows))
	}
	for _, tr := range t565Transports(pool, reg) {
		for _, row := range rows {
			orig := t565Stored(t, pool, row.Name, store.GlobalScope)
			if rec := tr.send(t, serverAdmin, row.Name, orig); rec.Code != http.StatusOK {
				t.Fatalf("%s %s: stored config sent back: %d %s", tr.label, row.Name, rec.Code, rec.Body.String())
			}
			if got := t565Stored(t, pool, row.Name, store.GlobalScope); !jsonEqual(got, orig) {
				t.Fatalf("%s %s: row %s after its own config, want %s", tr.label, row.Name, got, orig)
			}
			var m map[string]any
			_ = json.Unmarshal([]byte(orig), &m)
			changed := map[string]any{}
			for k, v := range m {
				changed[k] = v
			}
			changed["dream"] = map[string]any{"linkable": false}
			if d, ok := m["dream"].(map[string]any); ok {
				nd := map[string]any{}
				for k, v := range d {
					nd[k] = v
				}
				nd["linkable"] = false
				changed["dream"] = nd
			}
			full, _ := json.Marshal(changed)
			rec := tr.send(t, serverAdmin, row.Name, string(full))
			if rec.Code != http.StatusOK {
				t.Fatalf("%s %s: full config with dream.linkable=false: %d %s", tr.label, row.Name, rec.Code, rec.Body.String())
			}
			if got := t565Stored(t, pool, row.Name, store.GlobalScope); !jsonEqual(got, string(full)) {
				t.Fatalf("%s %s: row %s, want exactly the full config %s", tr.label, row.Name, got, full)
			}
			t565Restore(t, pool, reg, serverAdmin, row.Name, store.GlobalScope, orig)
		}
	}
}

// TestTypeUpdate565_LoudWhereMergedConfigFails: a merged config that does not
// decode is refused 422 naming the rule, and nothing is written — the one
// shape of a call that wrote under the replace and is refused now: new
// workflow states without their terminal set and forge map. null takes the
// stored companions out.
func TestTypeUpdate565_LoudWhereMergedConfigFails(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	orig := t565Stored(t, pool, "issue", store.GlobalScope)
	for _, tr := range t565Transports(pool, reg) {
		t.Run(tr.label, func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "issue", store.GlobalScope, orig) })
			rec := tr.send(t, serverAdmin, "issue", `{"v":1,"workflow":{"states":["todo","doing"],"initial":"todo"}}`)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "workflow.terminal") {
				t.Fatalf("new states over a stored terminal set: %d %s, want 422 naming workflow.terminal", rec.Code, rec.Body.String())
			}
			if got := t565Stored(t, pool, "issue", store.GlobalScope); !jsonEqual(got, orig) {
				t.Fatalf("refused write changed the row: %s", got)
			}
			rec = tr.send(t, serverAdmin, "issue",
				`{"v":1,"workflow":{"states":["todo","doing"],"initial":"todo","terminal":null,"forge_state_map":null}}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("the same with the companions named null: %d %s", rec.Code, rec.Body.String())
			}
			p, _ := reg.Snapshot().Resolve("issue")
			if !reflect.DeepEqual(p.Workflow.States, []string{"todo", "doing"}) || len(p.Workflow.Terminal) != 0 || p.Guard.Mode != blocktype.GuardModeFlag {
				t.Fatalf("issue after the reset: workflow %+v guard.mode %q", p.Workflow, p.Guard.Mode)
			}
			// The request part is still checked on its own: an unknown key or a
			// missing v is refused with the decoder's text, nothing written.
			for _, bad := range []string{`{"v":1,"guards":{"check":false}}`, `{"guard":{"check":false}}`} {
				rec := tr.send(t, serverAdmin, "issue", bad)
				if rec.Code != http.StatusUnprocessableEntity {
					t.Fatalf("%s: %d %s, want 422", bad, rec.Code, rec.Body.String())
				}
			}
		})
	}
}

// TestTypeUpdate565_ConcurrentUpdateIs409: the merge reads the row, the write
// comes after — a second write of the row in between must not be overwritten
// with a config merged over the old one. Interleaving: a second connection
// changes the config and holds the row lock; the update (both transports)
// reads the committed old config, merges and waits on the lock; the second
// connection commits. RED (mutation): the handler passes no ExpectConfig —
// 200, and the committed write's field is gone.
func TestTypeUpdate565_ConcurrentUpdateIs409(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	ctx := context.Background()
	orig := t565Stored(t, pool, "session-digest", store.GlobalScope)
	for _, tr := range t565Transports(pool, reg) {
		t.Run(tr.label, func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "session-digest", store.GlobalScope, orig) })
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `UPDATE context_block_types
				  SET config = jsonb_set(config, '{guard,mode}', '"flag"'::jsonb)
				WHERE name = 'session-digest' AND scope = '_global'`); err != nil {
				t.Fatal(err)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() { done <- tr.send(t, serverAdmin, "session-digest", `{"v":1,"retrieval":{"damping_factor":0.45}}`) }()
			for deadline := time.Now().Add(20 * time.Second); ; {
				var waiting bool
				if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_stat_activity
					  WHERE datname = current_database() AND wait_event_type = 'Lock'
					    AND query LIKE 'UPDATE context_block_types SET updated_at%')`).Scan(&waiting); err != nil {
					t.Fatal(err)
				}
				if waiting {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("the update never waited for the row lock")
				}
				time.Sleep(10 * time.Millisecond)
			}
			if err := holder.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			var rec *httptest.ResponseRecorder
			select {
			case rec = <-done:
			case <-time.After(20 * time.Second):
				t.Fatal("the update did not return after the holder committed")
			}
			if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "config changed") {
				t.Fatalf("update merged over a config that changed under it: %d %s, want 409", rec.Code, rec.Body.String())
			}
			var mode, factor string
			if err := pool.QueryRow(ctx, `SELECT config->'guard'->>'mode', config->'retrieval'->>'damping_factor'
				  FROM context_block_types WHERE name = 'session-digest' AND scope = '_global'`).Scan(&mode, &factor); err != nil {
				t.Fatal(err)
			}
			if mode != "flag" || factor != "0.8" {
				t.Fatalf("row after the refused update: guard.mode=%s damping_factor=%s, want flag and 0.8 (the committed write kept)", mode, factor)
			}
		})
	}
}
