//go:build integration

// Gates of W02-04 (model-alias-* manage actions, design/02 §4.4.2, §5 B10/B13;
// design/05 V-02-a/V-02-c, K-05-2) against a real database at migration 162:
//
//   - tier matrix: four actions × member / tenant-admin / server-admin × own /
//     foreign / _global scope, with the 404 body byte-identical to an id that
//     does not exist (no oracle);
//   - sensitivity: the masterplan gate (tenant alias under the _global floor ⇒
//     422), B13 (a tenant-admin lowers ⇒ 403, a server-admin without confirm ⇒
//     422, with ⇒ 200 and the audit row's confirm flag), the floor on the
//     second write path (a _global alias raised above a shadow), the name lock;
//   - policy: fail-closed parse from the raw bytes, the code ceiling, the
//     setting-bound ceilings and the tenant ceiling per key (B10), the reload
//     clamp; identity (400 immutable_field); kind↔role; explicit enabled.
//
// The red side of every semantic gate is a scratch mutation of the handler
// (report W02-04 §Gates); a run against the base tree fails to compile.
//
//	go test -tags=integration -p 1 -count=1 -run 'TestModelAliasManage' ./internal/handler/
package handler

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

type maHarness struct {
	t      *testing.T
	ctx    context.Context
	pool   *pgxpool.Pool
	h      *ManageHandler
	reg    *modelalias.Registry
	ceilMu sync.Mutex // the registry's drain goroutines read the ceilings
	ceil   modelalias.Ceilings
	tenA   string // tenant UUIDs
	tenB   string
	admin  *auth.AuthResult
	ten    *auth.AuthResult // tenant-admin of ma-a
	tenB2  *auth.AuthResult // tenant-admin of ma-b
	member *auth.AuthResult // member of ma-a
}

// maDefaultCeilings are the design defaults (design/02 §3.3; W02-02 keys).
func maDefaultCeilings() modelalias.Ceilings {
	return modelalias.Ceilings{
		MaxOutputTokens: 32768, MaxRequestBytes: 8 << 20, EmbeddingMaxFloats: 1 << 20,
		Tenant: modelalias.TenantCeiling{MaxOutputTokens: 8192, MaxN: 1, TimeoutS: 600,
			EmbeddingMaxInputs: 32, RerankMaxDocuments: 128, ToolsMaxCount: 64},
	}
}

func setupModelAliasHarness(t *testing.T) *maHarness {
	t.Helper()
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()
	m := &maHarness{t: t, ctx: ctx, pool: pool, ceil: maDefaultCeilings()}
	m.tenA = mintGateTenant(t, pool, "ma-a")
	mintGateMapScope(t, pool, "ma-a", m.tenA)
	m.tenB = mintGateTenant(t, pool, "ma-b")
	mintGateMapScope(t, pool, "ma-b", m.tenB)

	// The kind↔role check reads the pool snapshot: openai and anthropic rows
	// for the embedding roles, a tenant-private anthropic row in ma-b.
	bp := backends.NewPool(nil, nil)
	bp.SeedSnapshotForTest([]backends.Backend{
		{Name: "g-chat", Scope: backends.GlobalScope, Protocol: backends.ProtocolOpenAI, Roles: []string{"chat"}},
		{Name: "g-emb-ok", Scope: backends.GlobalScope, Protocol: backends.ProtocolOllama, Roles: []string{"emb-ok"}},
		{Name: "g-emb-bad", Scope: backends.GlobalScope, Protocol: backends.ProtocolAnthropic, Roles: []string{"emb-mixed"}},
		{Name: "g-emb-mixed", Scope: backends.GlobalScope, Protocol: backends.ProtocolOpenAI, Roles: []string{"emb-mixed"}},
		{Name: "b-emb", Scope: "ma-b", Protocol: backends.ProtocolAnthropic, Roles: []string{"emb-b"}},
	})
	m.h = NewManageHandler(pool, nil, nil, bp, nil, nil, nil, nil)
	m.reg = modelalias.NewRegistry(pool)
	m.reg.SetCeilings(m.ceilings)
	m.h.SetModelAliases(m.reg, m.ceilings)

	key := func(label, home string) string {
		k, _, err := store.CreateApiKey(ctx, pool, label, home, nil, "")
		if err != nil {
			t.Fatalf("create key %s: %v", label, err)
		}
		return k.ID
	}
	m.admin = &auth.AuthResult{ApiKeyID: key("ma-admin", "private"), HomeScope: backends.GlobalScope, IsValid: true, IsAdmin: true}
	m.ten = &auth.AuthResult{ApiKeyID: key("ma-ten", "ma-a"), HomeScope: "ma-a", TenantID: m.tenA,
		TenantRole: auth.RoleAdmin, ReadScopes: []string{"ma-a"}, IsValid: true}
	m.tenB2 = &auth.AuthResult{ApiKeyID: key("ma-ten-b", "ma-b"), HomeScope: "ma-b", TenantID: m.tenB,
		TenantRole: auth.RoleAdmin, ReadScopes: []string{"ma-b"}, IsValid: true}
	m.member = &auth.AuthResult{ApiKeyID: key("ma-member", "ma-a"), HomeScope: "ma-a", TenantID: m.tenA,
		TenantRole: auth.RoleMember, ReadScopes: []string{"ma-a"}, IsValid: true}
	return m
}

func (m *maHarness) ceilings() modelalias.Ceilings {
	m.ceilMu.Lock()
	defer m.ceilMu.Unlock()
	return m.ceil
}

func (m *maHarness) setCeilings(c modelalias.Ceilings) {
	m.ceilMu.Lock()
	defer m.ceilMu.Unlock()
	m.ceil = c
}

// doRaw drives one /api/manage request with a literal body.
func (m *maHarness) doRaw(ar *auth.AuthResult, body string) (int, map[string]any, string) {
	m.t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(body))
	req = req.WithContext(context.WithValue(m.ctx, authResultKey, ar))
	rec := httptest.NewRecorder()
	m.h.HandleManage(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, string(bytes.TrimSpace(rec.Body.Bytes()))
}

func (m *maHarness) do(ar *auth.AuthResult, action, id string, data map[string]any) (int, map[string]any, string) {
	m.t.Helper()
	body := map[string]any{"action": action}
	if id != "" {
		body["id"] = id
	}
	if data != nil {
		body["data"] = data
	}
	raw, err := json.Marshal(body)
	if err != nil {
		m.t.Fatalf("marshal: %v", err)
	}
	return m.doRaw(ar, string(raw))
}

// seed inserts an alias by SQL (no handler involved) and returns its id.
func (m *maHarness) seed(scope, name, kind, role, sens, policy string) string {
	m.t.Helper()
	var id string
	if err := m.pool.QueryRow(m.ctx, `INSERT INTO context_model_aliases (scope, name, kind, role, sensitivity, policy)
	        VALUES ($1, $2, $3, $4, $5, $6::jsonb) RETURNING id::text`, scope, name, kind, role, sens, policy).Scan(&id); err != nil {
		m.t.Fatalf("seed %s/%s: %v", scope, name, err)
	}
	return id
}

func (m *maHarness) col(id, col string) string {
	m.t.Helper()
	var v string
	if err := m.pool.QueryRow(m.ctx, `SELECT `+col+`::text FROM context_model_aliases WHERE id = $1::uuid`, id).Scan(&v); err != nil {
		return "<none>"
	}
	return v
}

func (m *maHarness) createdID(out map[string]any) string {
	m.t.Helper()
	a, _ := out["model_alias"].(map[string]any)
	id, _ := a["id"].(string)
	if id == "" {
		m.t.Fatalf("response carries no model_alias.id: %v", out)
	}
	return id
}

// fieldsOf renders the 422 field list as "field,field".
func fieldsOf(out map[string]any) string {
	fs, _ := out["fields"].([]any)
	names := make([]string, 0, len(fs))
	for _, f := range fs {
		if fm, ok := f.(map[string]any); ok {
			names = append(names, fm["field"].(string))
		}
	}
	return strings.Join(names, ",")
}

func chatSpec(name string, extra map[string]any) map[string]any {
	d := map[string]any{"name": name, "kind": "chat", "role": "chat", "enabled": true}
	for k, v := range extra {
		d[k] = v
	}
	return d
}

const maNotFoundBody = `{"error":"model alias not found","success":false}`

// TestModelAliasManage_TierMatrix_Integration — the escalation matrix.
func TestModelAliasManage_TierMatrix_Integration(t *testing.T) {
	m := setupModelAliasHarness(t)
	globalID := m.seed("_global", "chat", "chat", "chat", "credentials", "{}")
	ownID := m.seed("ma-a", "own", "chat", "chat", "credentials", "{}")
	foreignID := m.seed("ma-b", "foreign", "chat", "chat", "credentials", "{}")
	const missing = "01900000-0000-7000-8000-000000000000"

	// member: every action is the tier gate's 403, before any handler.
	for _, a := range []string{"model-alias-list", "model-alias-create", "model-alias-update", "model-alias-delete"} {
		code, _, body := m.do(m.member, a, ownID, chatSpec("m", nil))
		if code != http.StatusForbidden || body != `{"error":"admin key required","success":false}` {
			t.Errorf("member %s = %d %s, want 403 admin key required", a, code, body)
		}
	}

	// tenant-admin: _global, foreign, missing and malformed ids answer one
	// byte-identical 404 on update and delete; nothing is written.
	for _, a := range []string{"model-alias-update", "model-alias-delete"} {
		for _, id := range []string{globalID, foreignID, missing, "not-a-uuid"} {
			code, _, body := m.do(m.ten, a, id, map[string]any{"enabled": false})
			if code != http.StatusNotFound || body != maNotFoundBody {
				t.Errorf("tenant-admin %s %s = %d %s, want 404 %s", a, id, code, body, maNotFoundBody)
			}
		}
	}
	if m.col(globalID, "enabled") != "true" || m.col(foreignID, "enabled") != "true" {
		t.Errorf("a 404 write changed a row: _global enabled=%s foreign enabled=%s", m.col(globalID, "enabled"), m.col(foreignID, "enabled"))
	}
	// create outside the own scope: the same body for _global, a foreign and
	// an unknown scope.
	for _, scope := range []string{"_global", "ma-b", "nowhere"} {
		code, _, body := m.do(m.ten, "model-alias-create", "", chatSpec("x-"+scope, map[string]any{"scope": scope}))
		if code != http.StatusNotFound || body != maNotFoundBody {
			t.Errorf("tenant-admin create in %s = %d %s, want 404 %s", scope, code, body, maNotFoundBody)
		}
	}
	if code, _, body := m.do(m.ten, "model-alias-create", "", chatSpec("t", map[string]any{"tenant_id": m.tenA})); code != http.StatusForbidden ||
		body != `{"error":"tenant_id is server-admin only","success":false}` {
		t.Errorf("tenant-admin create with tenant_id = %d %s, want 403", code, body)
	}
	// own scope: create (scope absent or own), update, disable, delete.
	code, out, body := m.do(m.ten, "model-alias-create", "", chatSpec("mine", nil))
	if code != http.StatusOK {
		t.Fatalf("tenant-admin create own = %d %s", code, body)
	}
	mineID := m.createdID(out)
	if m.col(mineID, "scope") != "ma-a" {
		t.Errorf("tenant-admin alias landed in scope %s, want ma-a", m.col(mineID, "scope"))
	}
	if code, _, body := m.do(m.ten, "model-alias-create", "", chatSpec("mine2", map[string]any{"scope": "ma-a"})); code != http.StatusOK {
		t.Errorf("tenant-admin create with its own scope = %d %s", code, body)
	}
	if code, _, body := m.do(m.ten, "model-alias-update", ownID, map[string]any{"enabled": false, "description": "off"}); code != http.StatusOK || m.col(ownID, "enabled") != "false" {
		t.Errorf("tenant-admin disables its own alias = %d %s (enabled=%s), want 200", code, body, m.col(ownID, "enabled"))
	}
	if code, _, body := m.do(m.ten, "model-alias-delete", mineID, nil); code != http.StatusOK || m.col(mineID, "name") != "<none>" {
		t.Errorf("tenant-admin deletes its own alias = %d %s", code, body)
	}
	// list: own + _global, writable only on the own scope; never foreign.
	code, out, body = m.do(m.ten, "model-alias-list", "", nil)
	if code != http.StatusOK {
		t.Fatalf("tenant-admin list = %d %s", code, body)
	}
	var seen []string
	for _, it := range out["model_aliases"].([]any) {
		a := it.(map[string]any)
		seen = append(seen, a["scope"].(string)+"/"+a["name"].(string)+"="+map[bool]string{true: "w", false: "r"}[a["writable"].(bool)])
	}
	if got := strings.Join(seen, ","); got != "_global/chat=r,ma-a/mine2=w,ma-a/own=w" {
		t.Errorf("tenant-admin list = %s, want _global/chat=r,ma-a/mine2=w,ma-a/own=w", got)
	}
	if code, out, _ := m.do(m.tenB2, "model-alias-list", "", nil); code != http.StatusOK || out["count"] != float64(2) {
		t.Errorf("tenant-admin B list = %d count %v, want 2 (_global/chat, ma-b/foreign)", code, out["count"])
	}

	// server-admin: every scope; a tenant alias names tenant and scope.
	if code, _, body := m.do(m.admin, "model-alias-update", foreignID, map[string]any{"description": "by operator"}); code != http.StatusOK {
		t.Errorf("server-admin update foreign = %d %s", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-create", "", chatSpec("op", map[string]any{"tenant_id": m.tenA, "scope": "ma-a"})); code != http.StatusOK {
		t.Errorf("server-admin create for tenant A in ma-a = %d %s", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("op2", map[string]any{"tenant_id": m.tenA, "scope": "ma-b"})); code != http.StatusUnprocessableEntity || fieldsOf(out) != "scope" {
		t.Errorf("server-admin create for tenant A in B's scope = %d %s, want 422 at scope", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("op3", map[string]any{"scope": "ma-a"})); code != http.StatusUnprocessableEntity || fieldsOf(out) != "tenant_id" {
		t.Errorf("server-admin tenant alias without tenant_id = %d %s, want 422 at tenant_id", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("op4", map[string]any{"tenant_id": "not-a-uuid", "scope": "ma-a"})); code != http.StatusUnprocessableEntity || fieldsOf(out) != "scope" {
		t.Errorf("server-admin tenant alias with a malformed tenant_id = %d %s, want 422 at scope", code, body)
	}
	// uuid.Parse accepts the URN and brace spellings; the lookup gets the
	// canonical form (Fremd-Review F-2), never a 22P02 → 500.
	for i, id := range []string{"urn:uuid:" + m.tenA, "{" + m.tenA + "}"} {
		if code, _, body := m.do(m.admin, "model-alias-create", "", chatSpec("op-urn"+string(rune('a'+i)), map[string]any{"tenant_id": id, "scope": "ma-a"})); code != http.StatusOK {
			t.Errorf("server-admin tenant alias with tenant_id %q = %d %s, want 200", id, code, body)
		}
	}
	if code, out, _ := m.do(m.admin, "model-alias-list", "", nil); code != http.StatusOK || out["count"] != float64(7) {
		t.Errorf("server-admin list count = %v, want 7 (every scope)", out["count"])
	}
	if code, _, body := m.do(m.admin, "model-alias-delete", foreignID, nil); code != http.StatusOK || m.col(foreignID, "name") != "<none>" {
		t.Errorf("server-admin delete foreign = %d %s", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-delete", missing, nil); code != http.StatusNotFound || body != maNotFoundBody {
		t.Errorf("server-admin delete missing = %d %s", code, body)
	}
}

// maAuditMeta returns the metadata of the alias's audit rows, oldest first.
func maAuditMeta(t *testing.T, m *maHarness, id string) []map[string]any {
	t.Helper()
	rows, err := m.pool.Query(m.ctx, `SELECT metadata FROM context_settings_audit
	        WHERE entity_type = 'model_alias' AND entity_key = $1 ORDER BY created_at, id`, id)
	if err != nil {
		t.Fatalf("audit rows: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan: %v", err)
		}
		var md map[string]any
		_ = json.Unmarshal(raw, &md)
		out = append(out, md)
	}
	return out
}

// TestModelAliasManage_Sensitivity_Integration — masterplan gate V-02-c, B13,
// the floor on both write paths, the confirm flag in the audit row.
func TestModelAliasManage_Sensitivity_Integration(t *testing.T) {
	m := setupModelAliasHarness(t)
	if code, _, body := m.do(m.admin, "model-alias-create", "", chatSpec("chat", nil)); code != http.StatusOK {
		t.Fatalf("server-admin _global chat = %d %s", code, body)
	}

	// Masterplan gate: a tenant alias chat at internal under the _global chat
	// at credentials ⇒ 422 at sensitivity, nothing written; credentials ⇒ 200.
	code, out, body := m.do(m.ten, "model-alias-create", "", chatSpec("chat", map[string]any{"sensitivity": "internal"}))
	if code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" || !strings.Contains(body, "shadows") {
		t.Errorf("tenant shadow below the _global floor = %d %s, want 422 at sensitivity", code, body)
	}
	// The floor holds independently of the lowering tier: a server-admin with
	// the confirm flag is refused the same shadow (Fremd-Review N-3 — without
	// this case a missing floor would only show as the tier's 403 above).
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("chat", map[string]any{
		"tenant_id": m.tenB, "scope": "ma-b", "sensitivity": "internal", "confirm_sensitivity_downgrade": true})); code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" {
		t.Errorf("server-admin + confirm: shadow below the _global floor = %d %s, want 422 at sensitivity", code, body)
	}
	if code, _, body := m.do(m.ten, "model-alias-create", "", chatSpec("chat", map[string]any{"sensitivity": "credentials"})); code != http.StatusOK {
		t.Fatalf("tenant shadow at the floor = %d %s, want 200", code, body)
	}

	// B13: a tenant-admin lowers its own alias ⇒ 403 (also without a floor in
	// play, also with the confirm flag), and on create below credentials.
	code, out, _ = m.do(m.ten, "model-alias-create", "", chatSpec("mine", nil))
	if code != http.StatusOK {
		t.Fatalf("create mine = %d", code)
	}
	mineID := m.createdID(out)
	for _, data := range []map[string]any{
		{"sensitivity": "internal"},
		{"sensitivity": "public", "confirm_sensitivity_downgrade": true},
	} {
		if code, _, body := m.do(m.ten, "model-alias-update", mineID, data); code != http.StatusForbidden || body != `{"error":"admin key required","success":false}` {
			t.Errorf("tenant-admin lowers its own alias %v = %d %s, want 403", data, code, body)
		}
	}
	if code, _, body := m.do(m.ten, "model-alias-create", "", chatSpec("low", map[string]any{"sensitivity": "personal", "confirm_sensitivity_downgrade": true})); code != http.StatusForbidden {
		t.Errorf("tenant-admin creates below credentials = %d %s, want 403", code, body)
	}
	if got := m.col(mineID, "sensitivity"); got != "credentials" {
		t.Errorf("mine after the refused lowering: %s, want credentials", got)
	}
	// server-admin: without confirm 422 (settings wording), with confirm 200,
	// a warning, and the audit row of exactly that write carries the flag.
	code, _, body = m.do(m.admin, "model-alias-update", mineID, map[string]any{"sensitivity": "internal"})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `lowering credentials → internal`) || !strings.Contains(body, `\"confirm_sensitivity_downgrade\": true`) {
		t.Errorf("server-admin lowers without confirm = %d %s, want 422 with the confirm hint", code, body)
	}
	code, _, body = m.do(m.admin, "model-alias-update", mineID, map[string]any{"sensitivity": "internal", "confirm_sensitivity_downgrade": true})
	if code != http.StatusOK || !strings.Contains(body, "sensitivity lowered credentials → internal (confirmed)") {
		t.Fatalf("server-admin lowers with confirm = %d %s, want 200 with a warning", code, body)
	}
	meta := maAuditMeta(t, m, mineID)
	if len(meta) != 2 || meta[0]["confirm_sensitivity_downgrade"] != nil || meta[1]["confirm_sensitivity_downgrade"] != true {
		t.Errorf("audit metadata of mine = %v, want [create without flag, update with confirm_sensitivity_downgrade=true]", meta)
	}
	// Raising is free for a tenant-admin; re-saving at the same level needs no confirm.
	if code, _, body := m.do(m.ten, "model-alias-update", mineID, map[string]any{"sensitivity": "personal"}); code != http.StatusOK {
		t.Errorf("tenant-admin raises internal → personal = %d %s, want 200", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-update", mineID, map[string]any{"sensitivity": "personal", "description": "same level"}); code != http.StatusOK {
		t.Errorf("re-save at the same level = %d %s, want 200 without confirm", code, body)
	}
	if meta := maAuditMeta(t, m, mineID); meta[len(meta)-1]["confirm_sensitivity_downgrade"] != nil {
		t.Errorf("a non-lowering write recorded the confirm flag: %v", meta[len(meta)-1])
	}
	// A _global alias created below credentials needs the confirm as well.
	if code, _, _ := m.do(m.admin, "model-alias-create", "", chatSpec("pub", map[string]any{"sensitivity": "public"})); code != http.StatusUnprocessableEntity {
		t.Errorf("server-admin _global public without confirm = %d, want 422", code)
	}
	if code, _, body := m.do(m.admin, "model-alias-create", "", chatSpec("pub", map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true})); code != http.StatusOK {
		t.Errorf("server-admin _global public with confirm = %d %s", code, body)
	}

	// The floor binds a server-admin's confirmed lowering too.
	shadowID := m.seedID(t, "ma-a", "chat")
	if code, out, body := m.do(m.admin, "model-alias-update", shadowID, map[string]any{"sensitivity": "internal", "confirm_sensitivity_downgrade": true}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" {
		t.Errorf("server-admin lowers a shadow below the floor with confirm = %d %s, want 422", code, body)
	}

	// Second write path: a _global alias may not be created or raised above a
	// tenant shadow of its name.
	if code, _, body := m.do(m.admin, "model-alias-create", "", chatSpec("g3", map[string]any{"tenant_id": m.tenA, "scope": "ma-a", "sensitivity": "public", "confirm_sensitivity_downgrade": true})); code != http.StatusOK {
		t.Fatalf("tenant g3 public = %d %s", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("g3", nil)); code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" || !strings.Contains(body, "ma-a=public") {
		t.Errorf("_global g3 credentials over a public shadow = %d %s, want 422 naming ma-a=public", code, body)
	}
	code, out, body = m.do(m.admin, "model-alias-create", "", chatSpec("g3", map[string]any{"sensitivity": "public", "confirm_sensitivity_downgrade": true}))
	if code != http.StatusOK {
		t.Fatalf("_global g3 public (at the shadow) = %d %s", code, body)
	}
	g3ID := m.createdID(out)
	if code, out, body := m.do(m.admin, "model-alias-update", g3ID, map[string]any{"sensitivity": "credentials"}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" {
		t.Errorf("raise _global g3 above its shadow = %d %s, want 422", code, body)
	}
	if code, _, body := m.do(m.ten, "model-alias-update", m.seedID(t, "ma-a", "g3"), map[string]any{"sensitivity": "credentials"}); code != http.StatusOK {
		t.Fatalf("tenant-admin raises its shadow = %d %s", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-update", g3ID, map[string]any{"sensitivity": "credentials"}); code != http.StatusOK {
		t.Errorf("raise _global g3 once the shadow is up = %d %s, want 200", code, body)
	}

	// Removing access is never blocked by the floor: a shadow pushed below
	// it by psql is still disabled and deleted by its tenant-admin.
	if _, err := m.pool.Exec(m.ctx, `UPDATE context_model_aliases SET sensitivity = 'public' WHERE id = $1::uuid`, shadowID); err != nil {
		t.Fatalf("psql lowering: %v", err)
	}
	if code, _, body := m.do(m.ten, "model-alias-update", shadowID, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Errorf("tenant-admin disables a below-floor shadow = %d %s, want 200", code, body)
	}
	if code, _, body := m.do(m.ten, "model-alias-delete", shadowID, nil); code != http.StatusOK {
		t.Errorf("tenant-admin deletes a below-floor shadow = %d %s, want 200", code, body)
	}
}

// seedID returns the id of the alias scope/name.
func (m *maHarness) seedID(t *testing.T, scope, name string) string {
	t.Helper()
	var id string
	if err := m.pool.QueryRow(m.ctx, `SELECT id::text FROM context_model_aliases WHERE scope = $1 AND name = $2`, scope, name).Scan(&id); err != nil {
		t.Fatalf("alias %s/%s: %v", scope, name, err)
	}
	return id
}

// TestModelAliasManage_FloorLock_Integration: the floor read happens under the
// name lock. A transaction that holds the lock (an in-flight _global create)
// makes the tenant create wait; once it commits its _global alias at
// credentials, the waiting create reads it and answers 422. Without the lock
// the create would not wait, read no floor and commit a shadow below it.
func TestModelAliasManage_FloorLock_Integration(t *testing.T) {
	m := setupModelAliasHarness(t)
	tx, err := m.pool.Begin(m.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(m.ctx) }()
	if err := store.LockModelAliasName(m.ctx, tx, "race"); err != nil {
		t.Fatalf("lock: %v", err)
	}
	type result struct {
		code int
		body string
	}
	done := make(chan result, 1)
	raw, err := json.Marshal(map[string]any{"action": "model-alias-create", "data": chatSpec("race", map[string]any{
		"tenant_id": m.tenA, "scope": "ma-a", "sensitivity": "internal", "confirm_sensitivity_downgrade": true})})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	go func() {
		code, _, body := m.doRaw(m.admin, string(raw))
		done <- result{code, body}
	}()
	select {
	case r := <-done:
		t.Fatalf("the tenant create did not wait for the name lock: %d %s", r.code, r.body)
	case <-time.After(300 * time.Millisecond):
	}
	if _, err := tx.Exec(m.ctx, `INSERT INTO context_model_aliases (name, kind, role) VALUES ('race', 'chat', 'chat')`); err != nil {
		t.Fatalf("insert _global race: %v", err)
	}
	if err := tx.Commit(m.ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	select {
	case r := <-done:
		if r.code != http.StatusUnprocessableEntity || !strings.Contains(r.body, "shadows") {
			t.Errorf("tenant create after the _global commit = %d %s, want 422 (floor credentials)", r.code, r.body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tenant create never finished")
	}
}

// TestModelAliasManage_PolicyIdentityKind_Integration — B10, V-02-a, kind↔role,
// K-05-2, the reload clamp, Trigger after the commit.
func TestModelAliasManage_PolicyIdentityKind_Integration(t *testing.T) {
	m := setupModelAliasHarness(t)

	// B10: the tenant ceiling, per key, binds a tenant-admin AND a
	// server-admin writing a tenant alias; at the ceiling is fine.
	type pc struct {
		kind, key string
		over, at  int
	}
	for i, c := range []pc{
		{"chat", "max_output_tokens", 8193, 8192},
		{"chat", "default_max_tokens", 8193, 8192},
		{"chat", "max_n", 2, 1},
		{"chat", "timeout_s", 3600, 600},
		{"chat", "tools.max_count", 65, 64},
		{"embedding", "embedding.max_inputs", 33, 32},
		{"rerank", "rerank.max_documents", 129, 128},
	} {
		role := map[string]string{"chat": "chat", "embedding": "emb-ok", "rerank": "chat"}[c.kind]
		spec := func(name string, v int) map[string]any {
			return map[string]any{"name": name, "kind": c.kind, "role": role, "enabled": true, "policy": map[string]any{c.key: v}}
		}
		for _, ar := range []*auth.AuthResult{m.ten, m.admin} {
			s := spec("over-"+c.key, c.over)
			if ar == m.admin {
				s["tenant_id"], s["scope"] = m.tenA, "ma-a"
			}
			if code, out, body := m.do(ar, "model-alias-create", "", s); code != http.StatusUnprocessableEntity || fieldsOf(out) != "policy."+c.key {
				t.Errorf("%s over the tenant ceiling (admin=%v) = %d %s, want 422 at policy.%s", c.key, ar.IsAdmin, code, body, c.key)
			}
		}
		if code, _, body := m.do(m.ten, "model-alias-create", "", spec("at-"+string(rune('a'+i)), c.at)); code != http.StatusOK {
			t.Errorf("%s at the tenant ceiling = %d %s, want 200", c.key, code, body)
		}
	}
	// _global: up to the code ceiling and the setting-bound ceilings.
	for _, c := range []struct {
		policy, field string
	}{
		{`{"max_n": 9}`, "policy.max_n"},
		{`{"max_output_tokens": 32769}`, "policy.max_output_tokens"},
		{`{"timeout_s": 3601}`, "policy.timeout_s"},
		{`{"max_tokens": 5}`, "policy.max_tokens"},
		{`{"max_n": "2"}`, "policy.max_n"},
		{`{"max_n": 1, "max_n": 1}`, "policy.max_n"},
		{`{"max_n": 1e0}`, "policy.max_n"},
		{`[1]`, "policy"},
		{`null`, "policy"},
	} {
		body := `{"action":"model-alias-create","data":{"name":"gp","kind":"chat","role":"chat","enabled":true,"policy":` + c.policy + `}}`
		if code, out, resp := m.doRaw(m.admin, body); code != http.StatusUnprocessableEntity || fieldsOf(out) != c.field {
			t.Errorf("_global policy %s = %d %s, want 422 at %s", c.policy, code, resp, c.field)
		}
	}
	code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("gp", map[string]any{"policy": map[string]any{"max_n": 8, "timeout_s": 3600, "max_output_tokens": 32768}}))
	if code != http.StatusOK {
		t.Fatalf("_global policy at the ceilings = %d %s", code, body)
	}
	gpID := m.createdID(out)
	lowered := maDefaultCeilings()
	lowered.MaxOutputTokens = 16384 // hot: the next write check reads the new value
	m.setCeilings(lowered)
	if code, out, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"policy": map[string]any{"max_output_tokens": 20000}}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "policy.max_output_tokens" {
		t.Errorf("update over a lowered gateway.max_output_tokens_ceiling = %d %s, want 422", code, body)
	}
	// The stored policy is not re-judged when the update leaves it alone.
	if code, _, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"enabled": false}); code != http.StatusOK {
		t.Errorf("disable an alias whose policy is over the lowered ceiling = %d %s, want 200", code, body)
	}
	m.setCeilings(maDefaultCeilings())

	// V-02-a: identity fields in an update ⇒ 400 immutable_field + param,
	// even when the value is unchanged; the row stays.
	for _, k := range []string{"name", "kind", "scope", "tenant_id"} {
		code, out, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{k: map[string]string{"name": "gp", "kind": "chat", "scope": "_global", "tenant_id": m.tenA}[k]})
		if code != http.StatusBadRequest || out["code"] != "immutable_field" || out["param"] != k {
			t.Errorf("update {%s} = %d %s, want 400 immutable_field param %s", k, code, body, k)
		}
	}
	for _, k := range []string{"name", "kind", "scope"} {
		code, out, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{k: map[string]string{"name": "renamed", "kind": "embedding", "scope": "ma-a"}[k]})
		if code != http.StatusBadRequest || out["code"] != "immutable_field" || out["param"] != k {
			t.Errorf("update {%s} to a new value = %d %s, want 400 immutable_field param %s", k, code, body, k)
		}
	}
	if m.col(gpID, "name") != "gp" || m.col(gpID, "kind") != "chat" || m.col(gpID, "scope") != "_global" {
		t.Errorf("identity changed: %s/%s/%s", m.col(gpID, "scope"), m.col(gpID, "name"), m.col(gpID, "kind"))
	}

	// kind↔role (design/02 §3.1): an embedding alias over a role carried by
	// an anthropic row ⇒ 422 at role; a role nobody carries ⇒ 200 + warning;
	// a foreign tenant's row is invisible.
	emb := func(name, role string) map[string]any {
		return map[string]any{"name": name, "kind": "embedding", "role": role, "enabled": true}
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", emb("e1", "emb-mixed")); code != http.StatusUnprocessableEntity || fieldsOf(out) != "role" || !strings.Contains(body, "anthropic") {
		t.Errorf("embedding over an anthropic row = %d %s, want 422 at role naming anthropic", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", emb("e2", "emb-ok")); code != http.StatusOK || out["warnings"] != nil {
		t.Errorf("embedding over an ollama row = %d %s, want 200 without warnings", code, body)
	}
	code, out, body = m.do(m.ten, "model-alias-create", "", emb("e3", "emb-b"))
	if code != http.StatusOK || !strings.Contains(body, "no backend row visible") {
		t.Errorf("tenant A embedding over B's private role = %d %s, want 200 + no-row warning", code, body)
	}
	e3ID := m.createdID(out)
	if code, out, body := m.do(m.ten, "model-alias-update", e3ID, map[string]any{"role": "emb-mixed"}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "role" {
		t.Errorf("update an embedding alias onto an anthropic role = %d %s, want 422", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-create", "", map[string]any{"name": "c1", "kind": "chat", "role": "emb-mixed", "enabled": true}); code != http.StatusOK {
		t.Errorf("a chat alias is not protocol-checked = %d %s", code, body)
	}

	// K-05-2: enabled must be explicit; field validation and the 409.
	if code, out, body := m.do(m.admin, "model-alias-create", "", map[string]any{"name": "noen", "kind": "chat", "role": "chat"}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "enabled" {
		t.Errorf("create without enabled = %d %s, want 422 at enabled", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", map[string]any{"name": "-bad", "kind": "vision", "role": "UPPER", "enabled": true,
		"sensitivity": "secret", "description": strings.Repeat("x", 501)}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "name,kind,role,sensitivity,description" {
		t.Errorf("create with five bad fields = %d %s, want 422 at name,kind,role,sensitivity,description", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"description": "nul\x00byte"}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "description" {
		t.Errorf("update with a NUL byte in description = %d %s, want 422 at description (text cannot hold NUL)", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"sensitivity": nil}); code != http.StatusUnprocessableEntity || fieldsOf(out) != "sensitivity" {
		t.Errorf("update {sensitivity: null} = %d %s, want 422", code, body)
	}
	if code, out, body := m.do(m.admin, "model-alias-create", "", chatSpec("gp", nil)); code != http.StatusConflict || out["code"] != "constraint" {
		t.Errorf("duplicate create = %d %s, want 409 constraint", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"confirm_sensitivity_downgrade": true}); code != http.StatusBadRequest || !strings.Contains(body, "nothing to update") {
		t.Errorf("update without a field = %d %s, want 400", code, body)
	}
	if code, _, body := m.do(m.admin, "model-alias-update", gpID, map[string]any{"colour": "red"}); code != http.StatusBadRequest || !strings.Contains(body, "unknown field") {
		t.Errorf("update with an unknown field = %d %s, want 400", code, body)
	}
	// encoding/json matches struct fields case-insensitively; the key set is
	// exact (Fremd-Review F-1): a differently cased identity or policy key is
	// an unknown field, never a silently dropped or silently applied one.
	for _, data := range []map[string]any{
		{"Name": "renamed", "description": "a"},
		{"KIND": "embedding", "description": "a"},
		{"Policy": map[string]any{"max_n": 99}, "description": "a"},
		{"Sensitivity": "public"},
	} {
		if code, _, body := m.do(m.ten, "model-alias-update", m.seedID(t, "ma-a", "at-a"), data); code != http.StatusBadRequest || !strings.Contains(body, "unknown field") {
			t.Errorf("update %v = %d %s, want 400 unknown field", data, code, body)
		}
	}
	if code, _, body := m.do(m.admin, "model-alias-create", "", map[string]any{"name": "cased", "kind": "chat", "role": "chat", "Enabled": true}); code != http.StatusBadRequest || !strings.Contains(body, "unknown field") {
		t.Errorf("create with a cased key = %d %s, want 400 unknown field", code, body)
	}

	// Trigger after the commit (W02-03 R-5): no listener runs here, yet the
	// registry picks the write up; then the reload clamp (design/02 §3.3) on
	// a tenant alias psql pushed over the ceiling.
	gen := m.reg.Generation()
	code, out, body = m.do(m.admin, "model-alias-create", "", chatSpec("clampme", map[string]any{"tenant_id": m.tenA, "scope": "ma-a", "policy": map[string]any{"max_n": 1}}))
	if code != http.StatusOK {
		t.Fatalf("create clampme = %d %s", code, body)
	}
	if !maAwait(func() bool { _, ok := modelalias.Lookup(m.reg.Snapshot(), "clampme", "ma-a"); return ok && m.reg.Generation() > gen }) {
		t.Fatalf("the registry never saw the committed create (generation %d → %d)", gen, m.reg.Generation())
	}
	if _, err := m.pool.Exec(m.ctx, `UPDATE context_model_aliases SET policy = '{"max_n": 8}' WHERE id = $1::uuid`, m.createdID(out)); err != nil {
		t.Fatalf("psql over the ceiling: %v", err)
	}
	if err := m.reg.Reload(m.ctx); err != nil {
		t.Fatalf("reload: %v", err)
	}
	if a, ok := modelalias.Lookup(m.reg.Snapshot(), "clampme", "ma-a"); !ok || a.Policy.MaxN.V != 1 {
		t.Errorf("tenant alias psql-set to max_n 8 serves %+v %v, want max_n 1 (the tenant ceiling)", a.Policy.MaxN, ok)
	}

	// Without the ceiling getter the writing actions refuse.
	unwired := NewManageHandler(m.pool, nil, nil, nil, nil, nil, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/manage", strings.NewReader(`{"action":"model-alias-create","data":{}}`))
	req = req.WithContext(context.WithValue(m.ctx, authResultKey, m.admin))
	rec := httptest.NewRecorder()
	unwired.HandleManage(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unwired create = %d %s, want 503", rec.Code, rec.Body.String())
	}
}

// maAwait polls cond for up to five seconds (Trigger reloads asynchronously).
func maAwait(cond func() bool) bool {
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}
