//go:build integration

// The recording switch of a model consumer (issue #571, migration 197)
// through the real dispatch (HandleManage: table tier, then the field tiers):
//
//   - switching on (create or update) is server-admin only, also with
//     self-service — a tenant-admin gets the shared 403 of that tier — and
//     needs confirm_record_bodies:true (400 without it, after the tier);
//   - switching off is revoking: open to a tenant-admin of the consumer's
//     tenant, without self-service; re-sending the value it has is no change;
//   - the read form (create, update, get, list) names it, for every reader;
//   - the switch is audited (model-consumer-get's trail);
//   - the door reads it: AuthenticateModelKey after the update returns it —
//     the next request records, nothing caches the old value.
//
// The file uses only the wire (action names, JSON payloads), so it compiles
// against the tree before the wave — where every create or update naming
// record_bodies answers 400 "unknown field" (the base red).
//
//	go test -tags=integration -p 1 -run 'TestModelConsumerRecordSwitch' ./internal/handler/ -count=1 -v
package handler

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/testdb"
)

func TestModelConsumerRecordSwitch(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	hOff := mcHandler(t, pool, false, false) // the defaults: server-admin only
	hOn := mcHandler(t, pool, true, true)    // gateway on + self-service
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, hOff, admin, "rec-a")
	ta := a.ar

	const adminRequired = "admin key required"
	// The wire text of the refusal (recordConfirmRefusal), spelled out so the
	// file compiles against the base.
	const confirm = "record_bodies:true stores every request and answer of this consumer in full in the LLM log — " +
		"credentials in a prompt included, in clear text, readable by server-admins — requires confirm_record_bodies:true"
	recordOf := func(resp map[string]any) any {
		c, _ := resp["consumer"].(map[string]any)
		return c["record_bodies"]
	}

	// Create.
	for _, row := range []mcRow{
		{"create/server_admin_without_confirm", hOff, admin, "model-consumer-create",
			map[string]any{"label": "rec-c1", "tenant_id": a.id, "record_bodies": true}, http.StatusBadRequest, confirm},
		{"create/tenant_admin_with_self_service", hOn, ta, "model-consumer-create",
			map[string]any{"label": "rec-c2", "record_bodies": true, "confirm_record_bodies": true}, http.StatusForbidden, adminRequired},
		{"create/tenant_admin_with_self_service_without_confirm", hOn, ta, "model-consumer-create",
			map[string]any{"label": "rec-c3", "record_bodies": true}, http.StatusForbidden, adminRequired},
	} {
		row.run(t)
	}
	status, body, resp := mcCall(t, hOff, admin, "model-consumer-create",
		map[string]any{"label": "rec-on", "tenant_id": a.id, "record_bodies": true, "confirm_record_bodies": true})
	if status != http.StatusOK || recordOf(resp) != true {
		t.Fatalf("server-admin create with confirmation: %d %s, want 200 with record_bodies true", status, body)
	}
	onID := resp["consumer"].(map[string]any)["id"].(string)
	plain, _ := resp["api_key"].(string)
	plainOff := mcCreate(t, hOn, ta, map[string]any{"label": "rec-off"}) // self-service create without the switch
	if _, _, r := mcCall(t, hOff, ta, "model-consumer-get", map[string]any{"id": plainOff.id}); recordOf(r) != false {
		t.Fatalf("a consumer created without the switch reads record_bodies %v, want false", recordOf(r))
	}

	// Update: switching on.
	for _, row := range []mcRow{
		{"update_on/tenant_admin_with_self_service", hOn, ta, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": true, "confirm_record_bodies": true}, http.StatusForbidden, adminRequired},
		{"update_on/tenant_admin_without_self_service", hOff, ta, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": true, "confirm_record_bodies": true}, http.StatusForbidden, adminRequired},
		{"update_on/server_admin_without_confirm", hOff, admin, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": true}, http.StatusBadRequest, confirm},
		{"update_on/server_admin_confirm_false", hOff, admin, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": true, "confirm_record_bodies": false}, http.StatusBadRequest, confirm},
		{"update/record_bodies_null", hOff, admin, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": nil}, http.StatusBadRequest, "record_bodies must not be null"},
		{"update/record_bodies_string", hOff, admin, "model-consumer-update",
			map[string]any{"id": plainOff.id, "record_bodies": "yes"}, http.StatusBadRequest, "record_bodies has the wrong type"},
		// Re-sending the value a recording consumer has is no change.
		{"update_on/tenant_admin_unchanged", hOff, ta, "model-consumer-update",
			map[string]any{"id": onID, "record_bodies": true}, http.StatusOK, ""},
		// A confirmation alone switches nothing on.
		{"update/confirm_alone", hOff, admin, "model-consumer-update",
			map[string]any{"id": plainOff.id, "confirm_record_bodies": true}, http.StatusOK, ""},
	} {
		row.run(t)
	}
	if _, _, r := mcCall(t, hOff, admin, "model-consumer-get", map[string]any{"id": plainOff.id}); recordOf(r) != false {
		t.Fatalf("refused or confirmation-only updates switched recording on: %v", recordOf(r))
	}

	// The door reads the switch at the next check (ctx_model_auth by name,
	// so the file compiles against the base; the Go side of the column is
	// pinned through the router, cmd/ctxd TestV1RecordedConsumerRowAndItsReaders).
	key, err := modelkey.Parse(plain)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	doorSays := func() bool {
		t.Helper()
		var valid, record bool
		if err := pool.QueryRow(ctx, `SELECT is_valid, record_bodies FROM ctx_model_auth($1, '192.0.2.7'::inet, true)`, key.Hash()).
			Scan(&valid, &record); err != nil || !valid {
			t.Fatalf("authenticate: valid %v err %v", valid, err)
		}
		return record
	}
	if !doorSays() {
		t.Fatal("the door reads record_bodies false for a recording consumer")
	}

	// Switching off is revoking: a tenant-admin without self-service.
	status, body, resp = mcCall(t, hOff, ta, "model-consumer-update", map[string]any{"id": onID, "record_bodies": false})
	if status != http.StatusOK || recordOf(resp) != false {
		t.Fatalf("tenant-admin switches off: %d %s, want 200 with record_bodies false", status, body)
	}
	if doorSays() {
		t.Fatal("after switching off the door still reads record_bodies true")
	}
	// And on again by the server-admin, with the confirmation.
	status, body, resp = mcCall(t, hOff, admin, "model-consumer-update",
		map[string]any{"id": onID, "record_bodies": true, "confirm_record_bodies": true})
	if status != http.StatusOK || recordOf(resp) != true || !doorSays() {
		t.Fatalf("server-admin switches on: %d %s, want 200 with record_bodies true read by the door", status, body)
	}

	// The read form names it for every reader; the list too.
	for _, ar := range []*auth.AuthResult{admin, ta} {
		_, body, r := mcCall(t, hOff, ar, "model-consumer-list", map[string]any{"status": []string{"all"}})
		cs, _ := r["consumers"].([]any)
		seen := 0
		for _, c := range cs {
			m := c.(map[string]any)
			want := m["id"] == onID
			if m["record_bodies"] != want {
				t.Errorf("list (server-admin %v): consumer %v record_bodies %v, want %v", ar.IsServerAdmin(), m["label"], m["record_bodies"], want)
			}
			seen++
		}
		if seen == 0 {
			t.Fatalf("empty list: %s", body)
		}
	}

	// The trail: on (create), off, on — each an audited state.
	_, body, r := mcCall(t, hOff, ta, "model-consumer-get", map[string]any{"id": onID})
	trail, _ := r["audit"].([]any)
	var flips []string
	for _, row := range trail {
		m := row.(map[string]any)
		if m["entity_type"] != "model_consumer" {
			continue
		}
		nv, _ := m["new_value"].(map[string]any)
		flips = append(flips, m["action"].(string)+":"+strings.ToLower(strings.TrimSpace(fmtAny(nv["record_bodies"]))))
	}
	if strings.Join(flips, ",") != "update:true,update:false,create:true" {
		t.Errorf("audit trail of the switch %v (body %s), want update:true, update:false, create:true (newest first)", flips, body)
	}
}

// TestModelConsumerRecordSwitchUnderLock: a server-admin transaction switches
// recording off and holds the consumer row while a tenant-admin re-submits
// record_bodies:true. The unlocked pre-check reads "on" (no change, no tier);
// the check under the lock reads "off" — switching on, server-admin only —
// and refuses: the switch stays off. Red against a handler whose locked
// check judges the value read before the lock (the TestModelConsumerUpdate
// TierUnderLock pattern of review F5).
func TestModelConsumerRecordSwitchUnderLock(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	hOff := mcHandler(t, pool, false, false)
	hOn := mcHandler(t, pool, true, true)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, hOff, admin, "recl-a")
	c := mcCreate(t, hOff, admin, map[string]any{"label": "recl", "tenant_id": a.id, "record_bodies": true, "confirm_record_bodies": true})
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if _, err := tx.Exec(ctx, `UPDATE context_model_consumers SET record_bodies = false WHERE id = $1::uuid`, c.id); err != nil {
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
		status, body, _ = mcCall(t, hOn, a.ar, "model-consumer-update", map[string]any{"id": c.id, "record_bodies": true})
	}()
	mcAwaitLockWaiter(ctx, t, pool)
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
	var on bool
	if err := pool.QueryRow(ctx, `SELECT record_bodies FROM context_model_consumers WHERE id = $1::uuid`, c.id).Scan(&on); err != nil {
		t.Fatal(err)
	}
	if status != http.StatusForbidden || on {
		t.Fatalf("tenant-admin re-submitted record_bodies true over a concurrent switch-off: status %d body %s, stored %v — want 403 and false", status, body, on)
	}
}

func fmtAny(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case nil:
		return "<absent>"
	}
	return "?"
}
