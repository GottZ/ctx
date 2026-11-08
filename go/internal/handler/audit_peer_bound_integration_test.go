//go:build integration

// Red gate of W06-18 (design/06 §7 W06-18, §4.10; RC-D-INDEX §4 peer_bound
// hook; package doc of internal/usageevents) on GET /api/audit: the TOFU
// binding of a ctx-peer (cluster C3, F-W12) is one row of the model_consumer
// trigger of migration 161, written in the binding's own statement.
//
//   - Gate: a server-admin reading entity=model_consumer&id=<consumer> gets
//     exactly one row of the binding, labelled action "peer_bound" (red
//     today: the trigger writes "update"), whose new_value names the
//     peer_instance_id.
//
//   - Pin (green today): the peer_bound hook of usageevents writes no second
//     row — one audit row with the consumer's entity_key since the UPDATE.
//
//   - Pin (green today, regression guard of auditInvisibleUpdate): a
//     tenant-admin of the consumer's tenant does not see the binding row at
//     all — the label must not lift a binding-only update out of the
//     invisible class.
//
//     go test -tags=integration -p 1 -count=1 -run 'TestAuditPeerBound' ./internal/handler/
package handler

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/GottZ/ctx/internal/testdb"
	"github.com/GottZ/ctx/internal/usageevents"
)

// apbPeerID reads peer_instance_id from an audit state; "" when the state is
// absent, not an object, or the field is null.
func apbPeerID(state json.RawMessage) string {
	var m map[string]any
	if len(state) == 0 || json.Unmarshal(state, &m) != nil {
		return ""
	}
	s, _ := m["peer_instance_id"].(string)
	return s
}

func TestAuditPeerBoundLabel(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	ctx := context.Background()
	pool := testdb.SetupTestDB(t)
	mh := t43Handler(pool)
	admin, _ := kaServerAdmin(ctx, t, pool)
	a := auMakeTenant(ctx, t, pool, mh, admin, "apb-a")
	var consumer string
	if err := pool.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, kind, tenant_scope)
		 VALUES ($1::uuid, 'apb-peer', 'ctx-peer', $2) RETURNING id::text`, a.id, a.scope).Scan(&consumer); err != nil {
		t.Fatalf("insert ctx-peer consumer: %v", err)
	}
	peer := uuid.NewString()
	var before time.Time
	if err := pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&before); err != nil {
		t.Fatalf("clock: %v", err)
	}
	// The TOFU binding of cluster C3: first use proves the instance, one row.
	tag, err := pool.Exec(ctx,
		`UPDATE context_model_consumers SET peer_instance_id = $1::uuid, peer_bound_at = now()
		  WHERE id = $2::uuid AND peer_instance_id IS NULL`, peer, consumer)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("TOFU binding: %v rows %d, want 1", err, tag.RowsAffected())
	}
	// The hook C3 calls after an UPDATE that touched one row. A failing
	// flush is reported, not fatal: the label gate below has its own red
	// reason and must show it.
	rec := usageevents.NewRecorder(pool, nil)
	rec.Note(usageevents.Event{Kind: usageevents.KindPeerBound, ConsumerID: consumer, TenantID: a.id, PeerInstanceID: peer})
	fctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := rec.Flush(fctx); err != nil {
		t.Errorf("W06-18 gate (peer_bound hook): Flush: %v", err)
	}
	h := auditHandler(pool)
	query := "?entity=model_consumer&id=" + consumer

	t.Run("server_admin_reads_one_peer_bound_row", func(t *testing.T) {
		_, r := auditOK(t, h, admin, query)
		var bound []auditRowJSON
		for _, row := range r.Rows {
			if apbPeerID(row.NewValue) == peer && apbPeerID(row.OldValue) == "" {
				bound = append(bound, row)
			}
		}
		if len(bound) != 1 {
			t.Fatalf("W06-18 gate (TOFU binding = one audit row): %d rows binding %s among %+v, want 1", len(bound), peer, r.Rows)
		}
		if bound[0].Action != "peer_bound" {
			t.Errorf("W06-18 gate (TOFU binding labelled peer_bound for a server-admin): action %q, want \"peer_bound\" (new_value %s)", bound[0].Action, bound[0].NewValue)
		}
		if len(r.Rows) != 2 {
			t.Errorf("W06-18 gate (TOFU binding = one audit row): %d model_consumer rows, want 2 (create, binding)", len(r.Rows))
		}
	})

	t.Run("pin_no_second_write", func(t *testing.T) {
		var n int64
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM context_settings_audit WHERE entity_key = $1 AND created_at >= $2`, consumer, before).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Errorf("W06-18 pin (no second write): %d audit rows of the consumer since the binding, want 1", n)
		}
	})

	t.Run("pin_tenant_admin_does_not_see_the_binding", func(t *testing.T) {
		body, r := auditOK(t, h, a.ar, query)
		if len(r.Rows) != 1 || r.Rows[0].Action != "create" {
			t.Errorf("W06-18 pin (auditInvisibleUpdate): tenant-admin reads %d row(s) %+v, want only the create row", len(r.Rows), r.Rows)
		}
		if strings.Contains(string(body), peer) {
			t.Errorf("W06-18 pin (auditInvisibleUpdate): the tenant-admin's body names the peer instance %s: %s", peer, body)
		}
	})
}
