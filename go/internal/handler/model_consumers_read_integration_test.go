//go:build integration

// The read path of the model consumer administration (W05-02, folded into
// W01-5; design/05 §3.2, §7 gates (a)–(e)): the list's filter in SQL at scale,
// the parity of the Go status function with the SQL predicates, the audit
// trail of model-consumer-get through the redaction of GET /api/audit, the
// peer binding hidden from tenant-admins, and the optional consumer cap of
// tenant-limit-set.
//
//	go test -tags=integration -p 1 -run 'ConsumerList|ConsumerStatusParity|ModelConsumerAudit|ModelConsumerPeer|TenantLimitSetModelConsumers' ./internal/handler/ -count=1 -v
package handler

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

// mcRawConsumer inserts a consumer by SQL (unstamped) and returns its id.
func mcRawConsumer(ctx context.Context, t *testing.T, pool *pgxpool.Pool, tenant, label string, active bool) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label) VALUES ($1::uuid, $2) RETURNING id::text`,
		tenant, label).Scan(&id); err != nil {
		t.Fatalf("insert consumer %s: %v", label, err)
	}
	if !active {
		if _, err := pool.Exec(ctx, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, id); err != nil {
			t.Fatalf("deactivate %s: %v", label, err)
		}
	}
	return id
}

// mcRawKey inserts a key of consumer with the given expiry / revocation.
func mcRawKey(ctx context.Context, t *testing.T, pool *pgxpool.Pool, consumer string, expires *time.Time, revoked bool) {
	t.Helper()
	sum := sha256.Sum256([]byte(uuid.NewString()))
	var rev any
	if revoked {
		rev = "now"
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, expires_at, revoked_at)
		 VALUES ($1::uuid, $2, 'ctxm_…test', $3, CASE WHEN $4::text IS NULL THEN NULL ELSE clock_timestamp() + interval '1 second' END)`,
		consumer, hex.EncodeToString(sum[:]), expires, rev); err != nil {
		t.Fatalf("insert key of %s: %v", consumer, err)
	}
}

// TestConsumerListScale is W05-02 gate (b): 5 active consumers older than
// 10 000 inactive ones ⇒ the default list is ONE page with exactly the 5 and
// next_cursor = null — red against a filter after the page fetch (the first
// limit+1 rows would all be inactive). Plus the keyset: pages of 2 chain to
// the same 5, newest first, without a duplicate.
func TestConsumerListScale(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mcl-a")
	active := map[string]bool{}
	for i := range 5 {
		active[mcRawConsumer(ctx, t, pool, a.id, "mcl-active-"+string(rune('a'+i)), true)] = true
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, active, created_at)
		 SELECT $1::uuid, 'mcl-inactive-' || g, false, now() + g * interval '1 millisecond'
		   FROM generate_series(1, 10000) g`, a.id); err != nil {
		t.Fatalf("seed inactive: %v", err)
	}

	status, body, resp := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{})
	list, _ := resp["consumers"].([]any)
	if status != http.StatusOK || len(list) != 5 || resp["next_cursor"] != nil {
		t.Fatalf("default list over 5 active + 10000 inactive: status %d, %d consumer(s), next_cursor %v, want one page of 5 and null (body head %.300s)",
			status, len(list), resp["next_cursor"], body)
	}
	for _, c := range list {
		if id, _ := c.(map[string]any)["id"].(string); !active[id] {
			t.Errorf("list carries %s, not one of the 5 active consumers", id)
		}
	}

	var seen []string
	cursor := ""
	for page := 0; page < 5; page++ {
		data := map[string]any{"limit": 2}
		if cursor != "" {
			data["cursor"] = cursor
		}
		_, _, resp := mcCall(t, h, a.ar, "model-consumer-list", data)
		for _, c := range resp["consumers"].([]any) {
			seen = append(seen, c.(map[string]any)["id"].(string))
		}
		next, _ := resp["next_cursor"].(string)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(seen) != 5 {
		t.Fatalf("keyset pages of 2: %d consumer(s) %v, want the 5", len(seen), seen)
	}
	for i := 1; i < len(seen); i++ {
		if seen[i] >= seen[i-1] { // uuidv7, inserted in order: newest first is descending
			t.Errorf("keyset order broken at %d: %v", i, seen)
		}
	}

	inactive, _, resp := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{"status": []string{"inactive"}, "limit": 100})
	if inactive != http.StatusOK || len(resp["consumers"].([]any)) != 100 || resp["next_cursor"] == nil {
		t.Errorf("status=inactive: %d, %d row(s), next %v — want a full page of 100 and a cursor", inactive, len(resp["consumers"].([]any)), resp["next_cursor"])
	}
	if s, _, _ := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{"cursor": "bm90LWEtY3Vyc29y"}); s != http.StatusBadRequest {
		t.Errorf("forged cursor: status %d, want 400", s)
	}
	if s, _, resp := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{"q": "mcl-active-c"}); s != http.StatusOK || len(resp["consumers"].([]any)) != 1 {
		t.Errorf("q=mcl-active-c: status %d resp %v, want exactly one", s, resp)
	}
	if s, _, resp := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{"q": "mcl\\_active%"}); s != http.StatusOK || len(resp["consumers"].([]any)) != 0 {
		t.Errorf("q with escaped metacharacters: status %d resp %v, want none (the prefix is literal)", s, resp)
	}
}

// TestConsumerStatusParity is W05-02 gate (c): at an injected now the SQL
// status predicates (store.ListConsumers with Statuses = one status) and the
// Go function (consumerStatus over the whole key history) agree on every
// consumer, the boundaries included: expires_at = now is dead,
// expires_at = now + warn is expiring.
func TestConsumerStatusParity(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mcp-a")
	now := time.Now().Add(2 * time.Hour).Truncate(time.Microsecond) // injected, ahead of the DB clock
	warn := 24 * time.Hour
	at := func(d time.Duration) *time.Time { v := now.Add(d); return &v }

	want := map[string]string{}
	seed := func(label, status string, active bool, keys ...func(string)) {
		id := mcRawConsumer(ctx, t, pool, a.id, label, true)
		for _, k := range keys {
			k(id)
		}
		if !active {
			if _, err := pool.Exec(ctx, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, id); err != nil {
				t.Fatal(err)
			}
		}
		want[id] = status
	}
	key := func(exp *time.Time, revoked bool) func(string) {
		return func(id string) { mcRawKey(ctx, t, pool, id, exp, revoked) }
	}
	seed("inactive", store.ConsumerStatusInactive, false, key(nil, false))
	seed("no-key-empty", store.ConsumerStatusNoKey, true)
	seed("no-key-revoked", store.ConsumerStatusNoKey, true, key(nil, true))
	seed("no-key-expires-now", store.ConsumerStatusNoKey, true, key(at(0), false))
	seed("rotating", store.ConsumerStatusRotating, true, key(at(time.Hour), false), key(nil, false))
	seed("expiring", store.ConsumerStatusExpiring, true, key(at(time.Hour), false), key(nil, true))
	seed("expiring-edge", store.ConsumerStatusExpiring, true, key(at(warn), false))
	seed("active-past-edge", store.ConsumerStatusActive, true, key(at(warn+time.Microsecond), false))
	seed("active-no-expiry", store.ConsumerStatusActive, true, key(nil, false), key(at(-time.Minute), false))

	access := store.ModelAccess{TenantID: a.id}
	sqlStatus := map[string][]string{}
	for _, s := range store.ConsumerStatuses() {
		list, err := store.ListConsumers(ctx, pool, access, store.ListConsumersOptions{Statuses: []string{s}, Now: now, Warn: warn})
		if err != nil {
			t.Fatalf("ListConsumers(%s): %v", s, err)
		}
		for _, c := range list {
			sqlStatus[c.ID] = append(sqlStatus[c.ID], s)
		}
	}
	for id, w := range want {
		hist, err := store.KeyHistory(ctx, pool, id, access, nil, 100)
		if err != nil {
			t.Fatalf("KeyHistory: %v", err)
		}
		c, err := store.GetConsumer(ctx, pool, id, access, now)
		if err != nil {
			t.Fatalf("GetConsumer: %v", err)
		}
		goStatus := consumerStatus(c.Active, hist, now, warn)
		if goStatus != w || len(sqlStatus[id]) != 1 || sqlStatus[id][0] != w {
			t.Errorf("consumer %s (%s): Go %q, SQL %v, want %q in both", id, c.Label, goStatus, sqlStatus[id], w)
		}
	}
}

// mcAuditRow is one row of model-consumer-get's audit trail.
type mcAuditRow struct {
	EntityType string                     `json:"entity_type"`
	EntityKey  string                     `json:"entity_key"`
	Action     string                     `json:"action"`
	OldValue   json.RawMessage            `json:"old_value"`
	NewValue   json.RawMessage            `json:"new_value"`
	APIKeyID   *string                    `json:"api_key_id"`
	ActorLabel *string                    `json:"actor_label"`
	Metadata   map[string]json.RawMessage `json:"metadata"`
}

func mcGetTrail(t *testing.T, h *ManageHandler, ar *auth.AuthResult, id string) ([]mcAuditRow, map[string]any, []byte) {
	t.Helper()
	var resp struct {
		Success  bool           `json:"success"`
		Consumer map[string]any `json:"consumer"`
		Audit    []mcAuditRow   `json:"audit"`
	}
	status, body, _ := mcCall(t, h, ar, "model-consumer-get", map[string]any{"id": id})
	if err := json.Unmarshal(body, &resp); err != nil || status != http.StatusOK || !resp.Success {
		t.Fatalf("model-consumer-get %s: %d %s", id, status, body)
	}
	return resp.Audit, resp.Consumer, body
}

// TestModelConsumerAuditTrail is W05-02 gate (d)/(e): after create → rotate →
// revoke the trail of model-consumer-get holds all five rows, newest first;
// a tenant-admin reads the server-admin actor as "server-admin" (no key id,
// no actor_* metadata) and its own actor unmasked — red against a trail that
// skips the redaction of GET /api/audit.
func TestModelConsumerAuditTrail(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mca-a")
	c := mcCreate(t, h, admin, map[string]any{"label": "mca-app", "tenant_id": a.id})
	_, body, resp := mcCall(t, h, admin, "model-key-rotate", map[string]any{"consumer_id": c.id})
	k2, _ := resp["key"].(map[string]any)["id"].(string)
	if k2 == "" {
		t.Fatalf("rotate: %s", body)
	}
	if s, body, _ := mcCall(t, h, a.ar, "model-key-revoke", map[string]any{"key_id": k2}); s != http.StatusOK {
		t.Fatalf("revoke: %d %s", s, body)
	}

	wantSeq := []string{"model_key " + k2 + " revoke", "model_key " + k2 + " create", "model_key " + c.key + " update",
		"model_key " + c.key + " create", "model_consumer " + c.id + " create"}
	for _, reader := range []struct {
		name  string
		ar    *auth.AuthResult
		admin bool
	}{{"server_admin", admin, true}, {"tenant_admin", a.ar, false}} {
		t.Run(reader.name, func(t *testing.T) {
			trail, _, body := mcGetTrail(t, h, reader.ar, c.id)
			var got []string
			for _, r := range trail {
				got = append(got, r.EntityType+" "+r.EntityKey+" "+r.Action)
			}
			if strings.Join(got, "|") != strings.Join(wantSeq, "|") {
				t.Fatalf("trail %v, want %v", got, wantSeq)
			}
			for i, r := range trail {
				ownRow := i == 0 // the revoke is the tenant-admin's own act
				switch {
				case reader.admin:
					if r.APIKeyID == nil {
						t.Errorf("server-admin row %d: actor key masked", i)
					}
				case ownRow:
					if r.APIKeyID == nil || *r.APIKeyID != a.ar.ApiKeyID || r.ActorLabel == nil || *r.ActorLabel != a.ownerLabel {
						t.Errorf("tenant-admin's own row: actor %v/%v, want its own key and label", r.APIKeyID, r.ActorLabel)
					}
				default:
					if r.APIKeyID != nil || r.ActorLabel == nil || *r.ActorLabel != "server-admin" {
						t.Errorf("row %d: actor %v/%v, want masked server-admin", i, r.APIKeyID, r.ActorLabel)
					}
					for k := range r.Metadata {
						if strings.HasPrefix(k, "actor_") {
							t.Errorf("row %d: metadata %s survives for a tenant-admin", i, k)
						}
					}
				}
			}
			if bytes.Contains(body, []byte(admin.ApiKeyID)) != reader.admin {
				t.Errorf("server-admin key id in the trail: %v, want %v", !reader.admin, reader.admin)
			}
		})
	}
}

// TestModelConsumerPeerBindingHidden is W05-02 gate (e) with W06-7 Ü3 (f): a
// peer binding (set by raw SQL, as the F-W12 binding path will) reaches a
// tenant-admin neither in the consumer view, nor in GET
// /api/audit?entity=model_consumer, nor in the audit trail of
// model-consumer-get — red without the auditHiddenStateFields entry. A
// server-admin sees it everywhere.
func TestModelConsumerPeerBindingHidden(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mcb-a")
	peer := mcCreate(t, h, admin, map[string]any{"label": "mcb-peer", "tenant_id": a.id, "kind": "ctx-peer"})
	instance := uuid.NewString()
	if _, err := pool.Exec(ctx, `UPDATE context_model_consumers SET peer_instance_id = $2::uuid, peer_bound_at = now() WHERE id = $1::uuid`,
		peer.id, instance); err != nil {
		t.Fatalf("bind: %v", err)
	}
	ah := auditHandler(pool)
	for _, reader := range []struct {
		name  string
		ar    *auth.AuthResult
		admin bool
	}{{"server_admin", admin, true}, {"tenant_admin", a.ar, false}} {
		t.Run(reader.name, func(t *testing.T) {
			trail, consumer, getBody := mcGetTrail(t, h, reader.ar, peer.id)
			_, listBody, _ := mcCall(t, h, reader.ar, "model-consumer-list", map[string]any{})
			auditBody, ar := auditOK(t, ah, reader.ar, "?entity=model_consumer&id="+peer.id)
			binds := 0
			for _, r := range ar.Rows {
				// W06-18 (RC-D C2): /api/audit labels the TOFU binding row
				// "peer_bound" for a server-admin (masterplan gate W06-18);
				// the consumer's own trail keeps the trigger's "update".
				if r.Action == "update" || r.Action == "peer_bound" {
					binds++
				}
			}
			for _, r := range trail {
				if r.Action == "update" {
					binds++
				}
			}
			for name, body := range map[string][]byte{"get": getBody, "list": listBody, "audit": auditBody} {
				if has := bytes.Contains(body, []byte(instance)) || bytes.Contains(body, []byte("peer_bound_at")); has != reader.admin {
					t.Errorf("%s: binding visible = %v, want %v (body head %.400s)", name, has, reader.admin, body)
				}
			}
			if reader.admin {
				if consumer["peer"] == nil || binds != 2 {
					t.Errorf("server-admin: peer %v, %d binding row(s) (audit + trail), want the binding and 2", consumer["peer"], binds)
				}
				return
			}
			if consumer["peer"] != nil || binds != 0 {
				t.Errorf("tenant-admin: peer %v, %d binding row(s), want null and none", consumer["peer"], binds)
			}
		})
	}
}

// TestTenantLimitSetModelConsumers is gate (d) of the briefing (N22): the
// optional max_model_consumers of tenant-limit-set — absent keeps the column,
// a number sets it (and binds: the cap refuses the next create), null lifts
// it, a bad value is a 400; max_scopes/max_keys stay mandatory.
func TestTenantLimitSetModelConsumers(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, false, false)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mct-a")
	capOf := func() *int {
		var v *int
		if err := pool.QueryRow(ctx, `SELECT max_model_consumers FROM context_tenants WHERE id = $1::uuid`, a.id).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	set := func(data map[string]any) (int, map[string]any) {
		return kaManage(t, h, admin, "mct", map[string]any{"action": "tenant-limit-set", "id": a.id, "data": data})
	}
	if c := capOf(); c == nil || *c != 64 {
		t.Fatalf("default cap %v, want 64", c)
	}
	if s, resp := set(map[string]any{"max_scopes": nil, "max_keys": nil}); s != http.StatusOK || capOf() == nil || *capOf() != 64 {
		t.Fatalf("absent field: %d %v, cap %v — want 200 and 64 kept", s, resp, capOf())
	}
	s, resp := set(map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": 1})
	tn, _ := resp["tenant"].(map[string]any)
	if s != http.StatusOK || capOf() == nil || *capOf() != 1 || tn["max_model_consumers"] != float64(1) {
		t.Fatalf("set 1: %d %v, cap %v — want 200, 1 stored and echoed", s, resp, capOf())
	}
	mcCreate(t, h, admin, map[string]any{"label": "mct-1", "tenant_id": a.id})
	if s, body, _ := mcCall(t, h, admin, "model-consumer-create", map[string]any{"label": "mct-2", "tenant_id": a.id}); s != http.StatusTooManyRequests {
		t.Fatalf("create over the cap: %d %s, want 429", s, body)
	}
	if s, _ := set(map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": nil}); s != http.StatusOK || capOf() != nil {
		t.Fatalf("null: %d, cap %v — want unlimited", s, capOf())
	}
	for _, bad := range []any{-1, 1_000_001, "3", 1.5} {
		if s, _ := set(map[string]any{"max_scopes": nil, "max_keys": nil, "max_model_consumers": bad}); s != http.StatusBadRequest {
			t.Errorf("max_model_consumers=%v: %d, want 400", bad, s)
		}
	}
	if s, _ := set(map[string]any{"max_scopes": nil, "max_model_consumers": 5}); s != http.StatusBadRequest {
		t.Errorf("max_keys missing: %d, want 400 (mandatory fields unchanged)", s)
	}
}

// TestModelConsumerGetStatusBeyondTheChainCap is review F3: the status of
// model-consumer-get comes from ALL live keys, not from the chain cut at
// gateway.consumer_chain_max — with a cap of 1 a rotating consumer still
// reads rotating (as in the list), and its older live key superseded.
func TestModelConsumerGetStatusBeyondTheChainCap(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandlerOv(t, pool, map[string]string{"gateway.consumer_chain_max": "1"})
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mcs-a")
	c := mcCreate(t, h, admin, map[string]any{"label": "mcs-app", "tenant_id": a.id})
	if s, body, _ := mcCall(t, h, admin, "model-key-rotate", map[string]any{"consumer_id": c.id}); s != http.StatusOK {
		t.Fatalf("rotate: %d %s", s, body)
	}
	_, _, resp := mcCall(t, h, admin, "model-consumer-list", map[string]any{})
	listStatus := resp["consumers"].([]any)[0].(map[string]any)["status"]
	_, getBody, resp := mcCall(t, h, admin, "model-consumer-get", map[string]any{"id": c.id})
	cv := resp["consumer"].(map[string]any)
	if cv["status"] != "rotating" || listStatus != "rotating" || resp["keys_truncated"] != true || len(cv["keys"].([]any)) != 1 {
		t.Fatalf("chain cap 1: get status %v, list status %v, body %s — want rotating in both, one key shown, keys_truncated", cv["status"], listStatus, getBody)
	}
	_, _, resp = mcCall(t, h, admin, "model-key-history", map[string]any{"consumer_id": c.id, "limit": 1})
	if next, _ := resp["next_cursor"].(string); next != "" {
		_, body, resp := mcCall(t, h, admin, "model-key-history", map[string]any{"consumer_id": c.id, "limit": 1, "cursor": next})
		if k := resp["keys"].([]any)[0].(map[string]any); k["id"] != c.key || k["status"] != "superseded" {
			t.Errorf("second history page: %s, want the first key, superseded", body)
		}
	} else {
		t.Errorf("history limit 1: no next_cursor over two keys")
	}
}

// TestModelConsumerCreatorMasked is review F4: created_by_label of the
// consumer view follows the actor rule of the audit read surface — a
// tenant-admin reads a server-admin creator as "server-admin" and its own
// tenant's creator by label; a server-admin reads every label.
func TestModelConsumerCreatorMasked(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	h := mcHandler(t, pool, true, true)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, h, admin, "mcm-a")
	byAdmin := mcCreate(t, h, admin, map[string]any{"label": "mcm-by-admin", "tenant_id": a.id})
	byOwner := mcCreate(t, h, a.ar, map[string]any{"label": "mcm-by-owner"})
	label := func(ar *auth.AuthResult, id string) any {
		_, _, resp := mcCall(t, h, ar, "model-consumer-get", map[string]any{"id": id})
		return resp["consumer"].(map[string]any)["created_by_label"]
	}
	for _, tc := range []struct {
		name string
		ar   *auth.AuthResult
		id   string
		want string
	}{
		{"tenant_admin_reads_server_admin_creator", a.ar, byAdmin.id, "server-admin"},
		{"tenant_admin_reads_own_creator", a.ar, byOwner.id, a.ownerLabel},
		{"server_admin_reads_its_own_label", admin, byAdmin.id, "ka-server-admin"},
		{"server_admin_reads_the_owner_label", admin, byOwner.id, a.ownerLabel},
	} {
		if got := label(tc.ar, tc.id); got != tc.want {
			t.Errorf("%s: created_by_label %v, want %q", tc.name, got, tc.want)
		}
	}
	_, body, _ := mcCall(t, h, a.ar, "model-consumer-list", map[string]any{})
	if bytes.Contains(body, []byte("ka-server-admin")) {
		t.Errorf("tenant-admin list names the server-admin key: %s", body)
	}
}

// TestModelConsumerUpdateTierUnderLock is review F5 (the reviewer's probe,
// made deterministic): a server-admin transaction lowers fair_weight 50 → 10
// and holds the row while a tenant-admin with self-service re-submits 50.
// The unlocked pre-check reads 50 ("unchanged"); the check under the lock
// reads 10 ("raised", server-admin only) and refuses — red against a handler
// whose locked check passes everything.
func TestModelConsumerUpdateTierUnderLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	hOff := mcHandler(t, pool, false, false)
	hOn := mcHandler(t, pool, true, true)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, hOff, admin, "mcu-a")
	c := mcCreate(t, hOff, admin, map[string]any{"label": "mcu", "tenant_id": a.id, "fair_weight": 50})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET fair_weight = 10 WHERE id = $1::uuid`, c.id); err != nil {
		t.Fatal(err)
	}
	var (
		wg     sync.WaitGroup
		status int
		body   []byte
	)
	wg.Add(1)
	go func() {
		defer wg.Done()
		status, body, _ = mcCall(t, hOn, a.ar, "model-consumer-update", map[string]any{"id": c.id, "fair_weight": 50})
	}()
	mcAwaitLockWaiter(ctx, t, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var fw int
	if err := pool.QueryRow(ctx, `SELECT fair_weight FROM context_model_consumers WHERE id = $1::uuid`, c.id).Scan(&fw); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden || fw != 10 {
		t.Fatalf("re-submitted fair_weight 50 over a concurrent lowering to 10: status %d body %s, stored %d — want 403 and 10", status, body, fw)
	}
}

// mcAwaitLockWaiter waits until a backend of this test database waits on a
// heavyweight lock (the update queued behind the holding transaction).
func mcAwaitLockWaiter(ctx context.Context, t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND wait_event_type = 'Lock' AND pid <> pg_backend_pid()`).Scan(&n); err != nil {
			t.Fatalf("pg_stat_activity: %v", err)
		}
		if n > 0 {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("no lock waiter within 15 s — the update did not queue behind the holding transaction")
}
