//go:build integration

// Issue #565, review round 1: a row whose stored config does not decode, the
// jsonb comparison of the lost-update guard, configs whose characters JSON
// encoders escape, and the untrusted origin a re-classifying update records.
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/store"
)

// TestTypeUpdate565_BrokenStoredConfig: a row whose stored config the decoder
// rejects — a key a newer release wrote (zz_legacy), a key in another spelling
// (Digest), no object at all — cannot be merged over. A partial update fails
// loudly naming the broken stored config and what a whole config must name; the
// whole valid config replaces it and the registry (or the tenant's set) builds
// again. Both transports, a '_global' builtin row and a tenant's own type.
//
// RED on 02a1a26e: the whole config answers 422 too (the merge keeps the
// broken key), so the row can only be repaired by SQL.
func TestTypeUpdate565_BrokenStoredConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, tenantAdmin := t565Fixture(t)
	ctx := context.Background()
	const own = `{"v":1,"retrieval":{"policy":"damped","damping_factor":0.5,"untrusted":true},"guard":{"check":false,"candidate":false},"dream":{"linkable":false},"digest":{"include":false},"overview":{"include":false},"classify":{"priority":40,"title_patterns":["sprint note "]}}`
	if rec := c24TypesReq(t, pool, reg, tenantAdmin, http.MethodPut, "/api/types/sprint565b", `{"config":`+own+`}`); rec.Code != http.StatusOK {
		t.Fatalf("create tenant type: %d %s", rec.Code, rec.Body.String())
	}
	serverAdminInTenant := *tenantAdmin
	serverAdminInTenant.IsAdmin = true
	serverAdminInTenant.TenantID, serverAdminInTenant.TenantRole = "_server", auth.RoleMember
	digestOrig := t565Stored(t, pool, "session-digest", store.GlobalScope)

	rows := []struct {
		label, name, scope, orig string
	}{
		{"_global builtin", "session-digest", store.GlobalScope, digestOrig},
		{"tenant's own type", "sprint565b", c24Tenant, own},
	}
	forms := []struct{ label, expr string }{
		{"unknown top-level key", `config || '{"zz_legacy":{"x":1}}'::jsonb`},
		{"key in another spelling", `config || '{"Digest":{"include":false}}'::jsonb`},
		{"no object", `'[]'::jsonb`},
	}
	setRaw := func(t *testing.T, name, scope, expr string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE context_block_types SET config = `+expr+` WHERE name = $1 AND scope = $2`, name, scope); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}
	served := func(t *testing.T, name, scope string) bool {
		t.Helper()
		if scope == store.GlobalScope {
			return reg.Reload(ctx, pool) == nil
		}
		reg.InvalidateTenant(scope)
		_, ok := reg.SnapshotForTenant(ctx, scope).Resolve(name)
		return ok
	}
	for _, tr := range t565Transports(pool, reg) {
		for _, row := range rows {
			ar := serverAdmin
			if row.scope != store.GlobalScope {
				ar = tenantAdmin
				if tr.label == "manage type-update" {
					ar = &serverAdminInTenant
				}
			}
			for _, form := range forms {
				t.Run(tr.label+" "+row.label+" "+form.label, func(t *testing.T) {
					if _, err := pool.Exec(ctx, `UPDATE context_block_types SET config = $3::jsonb WHERE name = $1 AND scope = $2`, row.name, row.scope, row.orig); err != nil {
						t.Fatal(err)
					}
					setRaw(t, row.name, row.scope, form.expr)
					broken := t565Stored(t, pool, row.name, row.scope)
					if served(t, row.name, row.scope) {
						t.Fatalf("fixture premise: the broken row %s is served (registry builds)", broken)
					}
					// The request is checked as sent before its nulls are dropped:
					// an unknown key is refused even when it is named null.
					if rec := tr.send(t, ar, row.name, `{"v":1,"zz_legacy":null}`); rec.Code != http.StatusUnprocessableEntity ||
						!strings.Contains(rec.Body.String(), `unknown field \"zz_legacy\"`) {
						t.Fatalf("an unknown key named null over the broken row: %d %s — want 422 naming the unknown field", rec.Code, rec.Body.String())
					}
					for _, partial := range []string{`{"v":1,"retrieval":{"damping_factor":0.45}}`} {
						if form.label == "no object" && row.scope != store.GlobalScope {
							// A row that names no section, of a name with no shipped
							// definition: there is nothing a partial config could drop,
							// so any config that decodes replaces it.
							if rec := tr.send(t, ar, row.name, partial); rec.Code != http.StatusOK {
								t.Fatalf("%s over a row that names nothing: %d %s, want 200", partial, rec.Code, rec.Body.String())
							}
							setRaw(t, row.name, row.scope, form.expr)
							continue
						}
						rec := tr.send(t, ar, row.name, partial)
						if rec.Code != http.StatusUnprocessableEntity ||
							!strings.Contains(rec.Body.String(), "stored config does not decode") ||
							!strings.Contains(rec.Body.String(), "whole config") {
							t.Fatalf("partial %s over the broken row: %d %s — want 422 naming the broken stored config and the whole config", partial, rec.Code, rec.Body.String())
						}
						if got := t565Stored(t, pool, row.name, row.scope); !jsonEqual(got, broken) {
							t.Fatalf("refused write changed the row: %s", got)
						}
					}
					rec := tr.send(t, ar, row.name, row.orig)
					if rec.Code != http.StatusOK {
						t.Fatalf("the whole valid config over the broken row: %d %s, want 200", rec.Code, rec.Body.String())
					}
					if got := t565Stored(t, pool, row.name, row.scope); !jsonEqual(got, row.orig) {
						t.Fatalf("row after the whole config: %s, want %s", got, row.orig)
					}
					if !served(t, row.name, row.scope) {
						t.Fatal("after the repair the registry (or the tenant's set) still does not build")
					}
				})
			}
		}
	}
}

// TestTypeUpdate565_BrokenBuiltinNamesShippedFields: over a '_global' row of a
// builtin whose stored config does not decode, the repair must name every field
// the compiled definition sets — for each builtin marked untrusted, its
// retrieval.untrusted. A whole config whose retrieval leaves the mark out, is
// {} or is null is refused 422 and the row stays as it was; the healthy config
// repairs it and the registry serves the type untrusted; named false on purpose
// it is written and served so. Both transports, every builtin the compiled set
// marks untrusted, the healthy config read off the seeded row.
//
// RED on 9c701044: the three partial forms answered 200 and the registry served
// the type untrusted=false (PR #567 review, R2-1).
func TestTypeUpdate565_BrokenBuiltinNamesShippedFields(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	ctx := context.Background()
	rows, err := pool.Query(ctx, `SELECT name FROM context_block_types WHERE scope = $1 AND builtin ORDER BY name`, store.GlobalScope)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		if b, ok := blocktype.BuiltinPolicy(n); ok && b.Retrieval.Untrusted {
			names = append(names, n)
		}
	}
	rows.Close()
	if len(names) < 5 {
		t.Fatalf("fixture premise: %d builtin rows marked untrusted (%v), want at least 5", len(names), names)
	}
	withRetrieval := func(t *testing.T, cfg string, edit func(map[string]any) any) string {
		t.Helper()
		var m map[string]any
		if err := json.Unmarshal([]byte(cfg), &m); err != nil {
			t.Fatal(err)
		}
		m["retrieval"] = edit(m["retrieval"].(map[string]any))
		out, _ := json.Marshal(m)
		return string(out)
	}
	served := func(t *testing.T, name string) (blocktype.Policy, bool) {
		t.Helper()
		if err := reg.Reload(ctx, pool); err != nil {
			return blocktype.Policy{}, false
		}
		return reg.Snapshot().Resolve(name)
	}
	for _, tr := range t565Transports(pool, reg) {
		for _, name := range names {
			healthy := t565Stored(t, pool, name, store.GlobalScope)
			partials := map[string]string{
				"untrusted left out": withRetrieval(t, healthy, func(r map[string]any) any { delete(r, "untrusted"); return r }),
				"retrieval {}":       withRetrieval(t, healthy, func(map[string]any) any { return map[string]any{} }),
				"retrieval null":     withRetrieval(t, healthy, func(map[string]any) any { return nil }),
			}
			for label, body := range partials {
				t.Run(tr.label+" "+name+" "+label, func(t *testing.T) {
					if _, err := pool.Exec(ctx, `UPDATE context_block_types SET config = config || '{"zz_legacy":{"x":1}}'::jsonb WHERE name = $1 AND scope = $2`, name, store.GlobalScope); err != nil {
						t.Fatal(err)
					}
					broken := t565Stored(t, pool, name, store.GlobalScope)
					rec := tr.send(t, serverAdmin, name, body)
					if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "retrieval.untrusted") {
						p, ok := served(t, name)
						t.Fatalf("%s over the broken row: %d %s — want 422 naming retrieval.untrusted (served now: ok=%v untrusted=%v)",
							label, rec.Code, rec.Body.String(), ok, p.Retrieval.Untrusted)
					}
					if got := t565Stored(t, pool, name, store.GlobalScope); !jsonEqual(got, broken) {
						t.Fatalf("refused write changed the row: %s", got)
					}
					if rec := tr.send(t, serverAdmin, name, healthy); rec.Code != http.StatusOK {
						t.Fatalf("the healthy config over the broken row: %d %s", rec.Code, rec.Body.String())
					}
					if p, ok := served(t, name); !ok || !p.Retrieval.Untrusted {
						t.Fatalf("after the repair: served ok=%v untrusted=%v, want the mark", ok, p.Retrieval.Untrusted)
					}
				})
			}
			t.Run(tr.label+" "+name+" untrusted false on purpose", func(t *testing.T) {
				if _, err := pool.Exec(ctx, `UPDATE context_block_types SET config = config || '{"zz_legacy":{"x":1}}'::jsonb WHERE name = $1 AND scope = $2`, name, store.GlobalScope); err != nil {
					t.Fatal(err)
				}
				off := withRetrieval(t, healthy, func(r map[string]any) any { r["untrusted"] = false; return r })
				if rec := tr.send(t, serverAdmin, name, off); rec.Code != http.StatusOK {
					t.Fatalf("untrusted named false over the broken row: %d %s, want 200", rec.Code, rec.Body.String())
				}
				if p, ok := served(t, name); !ok || p.Retrieval.Untrusted {
					t.Fatalf("served ok=%v untrusted=%v, want the type served without the mark it was deliberately given", ok, p.Retrieval.Untrusted)
				}
				t565Restore(t, pool, reg, serverAdmin, name, store.GlobalScope, healthy)
			})
		}
	}
}

// TestTypeUpdate565_ConcurrentSameValuesNo409: the lost-update guard compares
// configs as jsonb, not as text. A concurrent writer that stores the same
// values in another spelling (another key order, 0.80 for 0.8) changed
// nothing the merge kept, so the waiting update writes (200); and a row stored
// in such a spelling beforehand is updated like any other. RED (mutation):
// `config::text = $n` — 409 for a change that is none.
func TestTypeUpdate565_ConcurrentSameValuesNo409(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	ctx := context.Background()
	orig := t565Stored(t, pool, "session-digest", store.GlobalScope)
	const respelled = `{"retrieval":{"untrusted":true,"damping_factor":0.80,"policy":"damped"},"overview":{"include":false},"digest":{"include":false},"dream":{"linkable":false},"guard":{"candidates":"all","mode":"archive","candidate":false,"check":false},"parent":{"mode":"none"},"classify":{},"v":1}`
	if !jsonEqual(orig, respelled) {
		t.Fatalf("fixture premise: %s and %s differ in value", orig, respelled)
	}
	for _, tr := range t565Transports(pool, reg) {
		t.Run(tr.label+" concurrent respelling", func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "session-digest", store.GlobalScope, orig) })
			holder, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = holder.Rollback(ctx) }()
			if _, err := holder.Exec(ctx, `UPDATE context_block_types SET config = $1::jsonb
				WHERE name = 'session-digest' AND scope = '_global'`, respelled); err != nil {
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
			if rec.Code != http.StatusOK {
				t.Fatalf("a concurrent write of the same values in another spelling: %d %s, want 200", rec.Code, rec.Body.String())
			}
			p, _ := reg.Snapshot().Resolve("session-digest")
			if p.Retrieval.DampingFactor != 0.45 || !p.Retrieval.Untrusted {
				t.Fatalf("served %+v, want factor 0.45 and untrusted", p.Retrieval)
			}
		})
		t.Run(tr.label+" stored respelled", func(t *testing.T) {
			t.Cleanup(func() { t565Restore(t, pool, reg, serverAdmin, "session-digest", store.GlobalScope, orig) })
			if _, err := pool.Exec(ctx, `UPDATE context_block_types SET config = $1::jsonb
				WHERE name = 'session-digest' AND scope = '_global'`, respelled); err != nil {
				t.Fatal(err)
			}
			if rec := tr.send(t, serverAdmin, "session-digest", `{"v":1,"retrieval":{"damping_factor":0.45}}`); rec.Code != http.StatusOK {
				t.Fatalf("update of a row stored in another spelling: %d %s, want 200", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestTypeUpdate565_EscapedCharactersFullConfig: a valid config whose strings
// a JSON encoder would HTML-escape (& < >) keeps its size through the merge. 24
// title patterns of 128 ampersands are 3 KiB as sent; escaped they would be 18
// KiB, over the 16 KiB cap, and the unchanged config sent back would be
// refused. RED on 02a1a26e: 422 "config exceeds 16384 bytes".
func TestTypeUpdate565_EscapedCharactersFullConfig(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool, reg, serverAdmin, _ := t565Fixture(t)
	pats := make([]string, 24)
	for i := range pats {
		pats[i] = strings.Repeat("&", 127) + string(rune('a'+i%26))
	}
	pats[0] = strings.Repeat("<", 64) + strings.Repeat(">", 64)
	var cfg bytes.Buffer
	cfg.WriteString(`{"v":1,"classify":{"priority":50,"title_patterns":[`)
	for i, p := range pats { // & < > and letters: no character JSON needs escaped
		if i > 0 {
			cfg.WriteString(",")
		}
		cfg.WriteString(`"` + p + `"`)
	}
	cfg.WriteString(`]`)
	cfg.WriteString(`}}`)
	if cfg.Len() > 16*1024/4 {
		t.Fatalf("fixture premise: %d bytes as sent", cfg.Len())
	}
	if rec := c24TypesReq(t, pool, reg, serverAdmin, http.MethodPut, "/api/types/amp565", `{"config":`+cfg.String()+`}`); rec.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	for _, tr := range t565Transports(pool, reg) {
		for _, body := range []string{cfg.String(), `{"v":1,"classify":{"priority":60}}`} {
			if rec := tr.send(t, serverAdmin, "amp565", body); rec.Code != http.StatusOK {
				t.Fatalf("%s %.60s…: %d %.300s, want 200", tr.label, body, rec.Code, rec.Body.String())
			}
		}
	}
	p, ok := reg.Snapshot().Resolve("amp565")
	if !ok || len(p.Classify.TitlePatterns) != 24 || p.Classify.TitlePatterns[1] != pats[1] || p.Classify.TitlePatterns[0] != pats[0] {
		t.Fatalf("served classify %+v (ok %v)", p.Classify.Priority, ok)
	}
}

// TestStoreAnswerType565_UntrustedOrigin: an update whose re-classification
// takes an auto block out of a type marked untrusted makes trg_block_retype
// record that type; the answer names the new type AND that record, as the row
// holds them. RED on 02a1a26e: type audit-trail without the origin.
func TestStoreAnswerType565_UntrustedOrigin(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	f := newSBFixture(t)
	ctx := context.Background()
	k := f.key(t, false)
	body, _ := json.Marshal(map[string]any{"category": "notes", "title": "Compaction checkpoint tool index t565", "content": "x"})
	req := httptest.NewRequest(http.MethodPost, "/api/store", bytes.NewReader(body)).WithContext(k.ctx)
	rec := httptest.NewRecorder()
	NewStoreHandler(f.pool, f.cfg, f.reg).HandleStore(rec, req)
	var stored struct {
		Block struct {
			ID   string `json:"id"`
			Type string `json:"type"`
		} `json:"block"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &stored) != nil || stored.Block.Type != "tool-evidence" {
		t.Fatalf("fixture premise: store answered %d %s, want type tool-evidence", rec.Code, rec.Body.String())
	}
	body, _ = json.Marshal(map[string]any{"action": "update", "id": stored.Block.ID,
		"data": map[string]any{"title": "Session handover after tools t565"}})
	req = httptest.NewRequest(http.MethodPost, "/api/manage", bytes.NewReader(body)).WithContext(k.ctx)
	rec = httptest.NewRecorder()
	NewManageHandler(f.pool, f.cfg, nil, nil, nil, nil, nil, f.reg).HandleManage(rec, req)
	var resp struct {
		Block struct {
			Type            string `json:"type"`
			UntrustedOrigin string `json:"untrusted_origin"`
		} `json:"block"`
	}
	if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil {
		t.Fatalf("manage update: %d %s", rec.Code, rec.Body.String())
	}
	var rowType, rowOrigin string
	if err := f.pool.QueryRow(ctx, `SELECT type_name, COALESCE(untrusted_origin, '') FROM context_blocks WHERE id = $1::uuid`,
		stored.Block.ID).Scan(&rowType, &rowOrigin); err != nil {
		t.Fatal(err)
	}
	if rowType != "audit-trail" || rowOrigin != "tool-evidence" {
		t.Fatalf("fixture premise: row (%s, origin %q), want (audit-trail, tool-evidence)", rowType, rowOrigin)
	}
	if resp.Block.Type != rowType || resp.Block.UntrustedOrigin != rowOrigin {
		t.Fatalf("answer (%s, origin %q), row (%s, origin %q)", resp.Block.Type, resp.Block.UntrustedOrigin, rowType, rowOrigin)
	}
}
