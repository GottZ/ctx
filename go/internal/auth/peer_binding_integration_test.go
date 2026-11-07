//go:build integration

// F-W12 (design/04 §3 M-F3, §4.8; RC-D S1): the trust-on-first-use peer
// binding against a real database. Gate (a) is a pin: the binding columns
// and both CHECKs are in migration 161 since masterplan K20, so a binding at
// an app consumer is refused (23514) already on the base. Gates (b)–(d) run
// auth.PeerBindings.BindPeer — red against the S1 stub.
//
//	go test -tags=integration -p 1 -count=1 -run 'PeerBind' ./internal/auth/
package auth_test

import (
	"context"
	"errors"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
)

const (
	pbInstanceB = "0190f000-0000-7000-8000-0000000000b0"
	pbInstanceC = "0190f000-0000-7000-8000-0000000000c0"
)

type pbEnv struct {
	pool  *pgxpool.Pool
	stamp store.AuditStamp
}

func pbSetup(t *testing.T) pbEnv {
	t.Helper()
	pool := testdb.SetupTestDB(t)
	admin, _, err := store.CreateApiKey(context.Background(), pool, "fw12-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	return pbEnv{pool: pool, stamp: store.APIStamp(admin.ID, "fw12")}
}

func (e pbEnv) consumer(t *testing.T, label, kind string) (store.ModelConsumer, modelkey.Key) {
	t.Helper()
	c, _, key, err := store.CreateConsumerWithKey(context.Background(), e.pool, store.ModelConsumerSpec{
		TenantID: store.DefaultTenantID, Label: label, Kind: kind, AllModels: true,
	}, nil, e.stamp)
	if err != nil {
		t.Fatalf("create consumer %s: %v", label, err)
	}
	return c, key
}

// binding reads the stored binding of a consumer ("" = none).
func (e pbEnv) binding(t *testing.T, consumerID string) (string, bool) {
	t.Helper()
	var id *string
	var at *time.Time
	if err := e.pool.QueryRow(context.Background(),
		`SELECT peer_instance_id::text, peer_bound_at FROM context_model_consumers WHERE id = $1::uuid`, consumerID).Scan(&id, &at); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if id == nil {
		return "", at != nil
	}
	return *id, at != nil
}

// bindAudits counts the consumer's audit rows whose new state carries a
// binding the old state did not.
func (e pbEnv) bindAudits(t *testing.T, consumerID string) int {
	t.Helper()
	var n int
	if err := e.pool.QueryRow(context.Background(), `SELECT count(*) FROM context_settings_audit
		WHERE entity_type = 'model_consumer' AND entity_key = $1
		  AND (old_value->>'peer_instance_id') IS NULL AND (new_value->>'peer_instance_id') IS NOT NULL`, consumerID).Scan(&n); err != nil {
		t.Fatalf("count audit rows: %v", err)
	}
	return n
}

// TestPeerBindCheckRefusesApp_Integration is gate (a) — a PIN, green on the
// base: a binding at a kind='app' consumer is refused by
// ck_model_consumers_peer_binding_kind (SQLSTATE 23514, Finding 24), and a
// half binding by ck_model_consumers_peer_binding_pair. The DDL is part of
// migration 161 (K20); F-W12 hands no DDL to C1's migration 168.
func TestPeerBindCheckRefusesApp_Integration(t *testing.T) {
	e := pbSetup(t)
	app, _ := e.consumer(t, "fw12-app", "app")
	peer, _ := e.consumer(t, "fw12-peer", "ctx-peer")
	for name, tc := range map[string]struct {
		id, sql, constraint string
	}{
		"app with binding": {app.ID, `UPDATE context_model_consumers SET peer_instance_id = $2::uuid, peer_bound_at = now() WHERE id = $1::uuid`,
			"ck_model_consumers_peer_binding_kind"},
		"peer, id without time": {peer.ID, `UPDATE context_model_consumers SET peer_instance_id = $2::uuid WHERE id = $1::uuid`,
			"ck_model_consumers_peer_binding_pair"},
	} {
		_, err := e.pool.Exec(context.Background(), tc.sql, tc.id, pbInstanceB)
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || pg.Code != "23514" || pg.ConstraintName != tc.constraint {
			t.Errorf("%s: err %v, want SQLSTATE 23514 from %s (design/04 Finding 24)", name, err, tc.constraint)
		}
	}
}

// TestPeerBindFirstContact_Integration is gate (b): the first BindPeer of a
// ctx-peer writes the binding (instance, peer_bound_at, one audit row) and
// says Created; a second call with the same instance writes nothing and
// answers the binding in force; a call with another instance answers the
// first one and leaves the row alone. The binding is keyed by the consumer
// id — the stable identity — and ctx_model_auth then returns it.
func TestPeerBindFirstContact_Integration(t *testing.T) {
	e := pbSetup(t)
	ctx := context.Background()
	peer, key := e.consumer(t, "fw12-first", "ctx-peer")
	b := auth.PeerBindings{Pool: e.pool}

	got, err := b.BindPeer(ctx, peer.ID, pbInstanceB)
	if err != nil {
		t.Fatalf("gate (b): first contact BindPeer: %v — the binding must be written on the first peer request", err)
	}
	if got.Instance != pbInstanceB || !got.Created {
		t.Fatalf("gate (b): first contact = %+v, want {Instance:%s Created:true}", got, pbInstanceB)
	}
	if id, at := e.binding(t, peer.ID); id != pbInstanceB || !at {
		t.Fatalf("gate (b): stored binding %q (bound_at %v), want %s with a time", id, at, pbInstanceB)
	}
	if n := e.bindAudits(t, peer.ID); n != 1 {
		t.Errorf("gate (b): %d audit rows for the binding, want 1 (an identity event, R-01-4)", n)
	}
	again, err := b.BindPeer(ctx, peer.ID, pbInstanceB)
	if err != nil || again.Instance != pbInstanceB || again.Created {
		t.Errorf("gate (b): second contact = %+v err %v, want {Instance:%s Created:false}", again, err, pbInstanceB)
	}
	other, err := b.BindPeer(ctx, peer.ID, pbInstanceC)
	if err != nil || other.Instance != pbInstanceB || other.Created {
		t.Errorf("gate (c): another instance = %+v err %v, want the first binding %s, not created", other, err, pbInstanceB)
	}
	if id, _ := e.binding(t, peer.ID); id != pbInstanceB {
		t.Errorf("gate (c): a second instance moved the binding to %q", id)
	}
	if n := e.bindAudits(t, peer.ID); n != 1 {
		t.Errorf("gate (b): %d audit rows after three calls, want 1", n)
	}
	ca, err := auth.AuthenticateModelKey(ctx, e.pool, key.Hash(), netip.MustParseAddr("10.1.2.3"), true)
	if err != nil || ca == nil || ca.PeerInstanceID != pbInstanceB {
		t.Errorf("gate (b): ctx_model_auth after the bind: %+v err %v, want PeerInstanceID %s", ca, err, pbInstanceB)
	}
}

// TestPeerBindAppConsumer_Integration: BindPeer on an app consumer (or one
// whose kind changed after the door) writes nothing and answers no binding
// — the caller refuses; never an error, never a 23514 at run time.
func TestPeerBindAppConsumer_Integration(t *testing.T) {
	e := pbSetup(t)
	app, _ := e.consumer(t, "fw12-app-bind", "app")
	got, err := auth.PeerBindings{Pool: e.pool}.BindPeer(context.Background(), app.ID, pbInstanceB)
	if err != nil || got.Instance != "" || got.Created {
		t.Errorf("BindPeer on an app = %+v err %v, want {} without error (kind='ctx-peer' in the WHERE)", got, err)
	}
	if id, _ := e.binding(t, app.ID); id != "" {
		t.Errorf("an app consumer got a binding %q", id)
	}
}

// TestPeerBindParallelFirstContact_Integration is gate (c), second half:
// parallel first requests produce exactly one binding. Same instance (the
// usual case: a peer opens several connections at start) ⇒ every call
// answers that instance, exactly one says Created — a CTE form of the
// statement would answer "" to the losers (pre-statement snapshot) and get
// the same peer refused. Two instances ⇒ exactly one wins, every call
// answers the winner.
func TestPeerBindParallelFirstContact_Integration(t *testing.T) {
	e := pbSetup(t)
	b := auth.PeerBindings{Pool: e.pool}
	const n = 16
	run := func(t *testing.T, consumerID string, instance func(i int) string) []auth.PeerBinding {
		t.Helper()
		out := make([]auth.PeerBinding, n)
		errs := make([]error, n)
		start := make(chan struct{})
		var wg sync.WaitGroup
		for i := range n {
			wg.Go(func() {
				<-start
				out[i], errs[i] = b.BindPeer(context.Background(), consumerID, instance(i))
			})
		}
		close(start)
		wg.Wait()
		for i, err := range errs {
			if err != nil {
				t.Fatalf("gate (c): parallel BindPeer %d: %v", i, err)
			}
		}
		return out
	}
	t.Run("same instance", func(t *testing.T) {
		peer, _ := e.consumer(t, "fw12-race-same", "ctx-peer")
		created := 0
		for i, r := range run(t, peer.ID, func(int) string { return pbInstanceB }) {
			if r.Instance != pbInstanceB {
				t.Errorf("gate (c): call %d answered %q, want %s — a parallel first request of the same peer must pass", i, r.Instance, pbInstanceB)
			}
			if r.Created {
				created++
			}
		}
		if created != 1 {
			t.Errorf("gate (c): %d calls created the binding, want exactly 1", created)
		}
		if a := e.bindAudits(t, peer.ID); a != 1 {
			t.Errorf("gate (c): %d audit rows, want 1", a)
		}
	})
	t.Run("two instances", func(t *testing.T) {
		peer, _ := e.consumer(t, "fw12-race-two", "ctx-peer")
		res := run(t, peer.ID, func(i int) string {
			if i%2 == 0 {
				return pbInstanceB
			}
			return pbInstanceC
		})
		winner, _ := e.binding(t, peer.ID)
		created := 0
		for i, r := range res {
			if r.Instance != winner {
				t.Errorf("gate (c): call %d answered %q, the stored binding is %q", i, r.Instance, winner)
			}
			if r.Created {
				created++
			}
		}
		if winner == "" || created != 1 {
			t.Errorf("gate (c): binding %q, %d created, want one winner created once", winner, created)
		}
	})
}

// TestPeerBindSurvivesKeyRotation_Integration is gate (d): the binding sits
// at the consumer, not at a key — a rotation (design/01 rotated_from) keeps
// it, and the new key authenticates with the same PeerInstanceID.
func TestPeerBindSurvivesKeyRotation_Integration(t *testing.T) {
	e := pbSetup(t)
	ctx := context.Background()
	peer, _ := e.consumer(t, "fw12-rotate", "ctx-peer")
	if _, err := (auth.PeerBindings{Pool: e.pool}).BindPeer(ctx, peer.ID, pbInstanceB); err != nil {
		t.Fatalf("gate (d): bind before the rotation: %v", err)
	}
	_, key, err := store.RotateKey(ctx, e.pool, peer.ID, store.ModelAccess{ServerAdmin: true}, 0, nil, e.stamp)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	ca, err := auth.AuthenticateModelKey(ctx, e.pool, key.Hash(), netip.MustParseAddr("10.1.2.3"), true)
	if err != nil || ca == nil || !ca.IsValid || ca.PeerInstanceID != pbInstanceB {
		t.Errorf("gate (d): the rotated key authenticates as %+v err %v, want PeerInstanceID %s", ca, err, pbInstanceB)
	}
	if id, _ := e.binding(t, peer.ID); id != pbInstanceB {
		t.Errorf("gate (d): the rotation changed the binding to %q", id)
	}
}
