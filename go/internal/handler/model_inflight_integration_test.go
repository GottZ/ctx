//go:build integration

package handler

// Gates of W01-8 (design/01 §4.6 "Widerruf wirkt", §5 B10/B13/B19/B29;
// masterplan K22, R6) on the probe router: the real model-key door over a
// real database at migration 165, its in-flight registry with its worker,
// and — where the gate is about the database signal — the production
// NOTIFY listener (events.NewPgxlistenListener) with the registry as the
// scheduler's sink. Behind the door a fake stream handler writes a frame
// every 100 ms until its context ends or 900 s pass
// (dispatch.external_lease_max) — the stream route itself is the
// internal/oai half (TestRevokeEndsRunningStream).
//
//	go test -tags=integration -p 1 -count=1 -run 'Inflight|Revoke' ./internal/handler/

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
)

// ifBound is R6's bound: a revoked stream ends within 5 s (expected ≪ 1 s).
const ifBound = 5 * time.Second

// ifLongSweep keeps the periodic sweep out of a gate that is about another
// trigger.
const ifLongSweep = time.Hour

// ifStream is the fake stream handler: a frame every 100 ms until the
// request context ends (or 900 s).
type ifStream struct {
	quiet   bool // no frames: wait for the end only (the 1 000-request gates)
	frames  atomic.Int64
	running atomic.Int64
	started chan struct{}
	ends    chan ifEnd
}

type ifEnd struct {
	at     time.Time
	cause  error
	frames int64
}

func newIFStream() *ifStream {
	return &ifStream{started: make(chan struct{}, 4096), ends: make(chan ifEnd, 4096)}
}

func (p *ifStream) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.running.Add(1)
	defer p.running.Add(-1)
	p.started <- struct{}{}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	limit := time.NewTimer(900 * time.Second)
	defer limit.Stop()
	if p.quiet {
		tick.Stop()
	}
	var n int64
	for {
		select {
		case <-r.Context().Done():
			p.ends <- ifEnd{at: time.Now(), cause: context.Cause(r.Context()), frames: n}
			return
		case <-limit.C:
			p.ends <- ifEnd{at: time.Now(), frames: n}
			return
		case <-tick.C:
			n++
			p.frames.Add(1)
			_, _ = fmt.Fprintf(w, "data: %d\n\n", n)
		}
	}
}

// ifEnv is a door with its registry's worker running, optionally the NOTIFY
// listener, and the probe stream behind it.
type ifEnv struct {
	*madEnv
	reqs   context.Context // the requests' base context, ended at cleanup
	door   *ModelDoor
	h      http.Handler
	stream *ifStream
	sink   *ifSink
	stop   context.CancelFunc
	done   sync.WaitGroup
}

// ifSink forwards to the registry and counts the backlog calls (the
// listener's connects).
type ifSink struct {
	reg       *InflightRegistry
	backlogs  atomic.Int64
	notifies  atomic.Int64
	noBacklog bool // mutation: the channel handler without its backlog sweep (B29 red)
}

func (s *ifSink) ModelAccessNotify(p string) { s.notifies.Add(1); s.reg.ModelAccessNotify(p) }
func (s *ifSink) ModelAccessBacklog() {
	s.backlogs.Add(1)
	if !s.noBacklog {
		s.reg.ModelAccessBacklog()
	}
}

type ifOpts struct {
	quiet     bool
	sweep     time.Duration
	listen    bool
	reconnect time.Duration
	noBacklog bool
}

func ifSetup(t *testing.T, o ifOpts) *ifEnv {
	t.Helper()
	e := &ifEnv{madEnv: madSetup(t), stream: newIFStream()}
	e.stream.quiet = o.quiet
	e.settings.auth(func(a *oai.AuthSettings) { a.InflightSweepInterval = o.sweep })
	e.door, e.h, _ = e.madEnv.door(t, "")
	e.h = e.door.Handler(e.stream)
	ctx, cancel := context.WithCancel(context.Background())
	e.stop = cancel
	reqs, endReqs := context.WithCancel(context.Background())
	e.reqs = reqs
	e.done.Go(func() { e.door.Inflight().Run(ctx) })
	if o.listen {
		e.sink = &ifSink{reg: e.door.Inflight(), noBacklog: o.noBacklog}
		cfg := config.NewStore(config.Defaults())
		sched := events.NewScheduler(e.seed, cfg, nil, events.StartupConfig{})
		sched.SetModelAccessSink(e.sink)
		if o.reconnect == 0 {
			o.reconnect = 100 * time.Millisecond
		}
		l := events.NewPgxlistenListener(e.dsn, o.reconnect, sched, e.seed, cfg, nil, nil, nil)
		e.done.Go(func() { _ = l.Listen(ctx) })
		if !ifWait(10*time.Second, func() bool { return e.sink.backlogs.Load() >= 1 }) {
			t.Fatal("the listener did not connect")
		}
	}
	t.Cleanup(func() {
		endReqs()
		if !ifWait(5*time.Second, func() bool { return e.stream.running.Load() == 0 }) {
			t.Errorf("%d streams still run at cleanup", e.stream.running.Load())
		}
		cancel()
		e.done.Wait()
	})
	return e
}

// ifServerAdmin is a server-admin knowledge key's auth result (the manage
// actions' caller): madSetup's key promoted, authenticated for real.
func ifServerAdmin(t *testing.T, e *ifEnv) *auth.AuthResult {
	t.Helper()
	ctx := context.Background()
	k, plain, err := store.CreateApiKey(ctx, e.seed, "if-admin", "private", nil, "")
	if err != nil {
		t.Fatalf("admin key: %v", err)
	}
	ifExec(t, e, `UPDATE context_api_keys SET is_admin = true, tenant_role = 'owner' WHERE id = $1::uuid`, k.ID)
	ar, err := auth.Authenticate(ctx, e.seed, plain)
	if err != nil || ar == nil || !ar.IsServerAdmin() {
		t.Fatalf("authenticate admin: ar=%+v err=%v", ar, err)
	}
	return ar
}

func ifWait(within time.Duration, ok func() bool) bool {
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(5 * time.Millisecond)
	}
	return true
}

// open starts one request on key in the background; it returns once the
// request runs behind the door (a refusal at the door fails the test).
func (e *ifEnv) open(t *testing.T, key modelkey.Key) {
	t.Helper()
	refused := make(chan int, 1)
	go func() {
		rec := httptest.NewRecorder()
		r := httptest.NewRequestWithContext(e.reqs, http.MethodPost, "/v1/chat/completions", nil)
		r.RemoteAddr = "10.7.7.7:40000"
		r.TLS = &tls.ConnectionState{}
		r.Header.Set("Authorization", "Bearer "+key.Plaintext())
		e.h.ServeHTTP(rec, r)
		refused <- rec.Code
	}()
	select {
	case <-e.stream.started:
	case code := <-refused:
		t.Fatalf("the door answered %d instead of reaching the stream", code)
	case <-time.After(5 * time.Second):
		t.Fatal("the request did not reach the stream behind the door")
	}
}

// ended waits for one stream end within d.
func (e *ifEnv) ended(t *testing.T, d time.Duration) (ifEnd, bool) {
	t.Helper()
	select {
	case x := <-e.stream.ends:
		return x, true
	case <-time.After(d):
		return ifEnd{}, false
	}
}

// still asserts the stream runs on for d: no end, frames keep coming.
func (e *ifEnv) still(t *testing.T, d time.Duration, what string) {
	t.Helper()
	before := e.stream.frames.Load()
	if x, ok := e.ended(t, d); ok {
		t.Fatalf("%s: the stream ended after %v (cause %v)", what, d, x.cause)
	}
	if after := e.stream.frames.Load(); after <= before {
		t.Fatalf("%s: no frame for %v", what, d)
	}
}

// revokedWithin asserts the stream ends within bound of at with the cause
// auth.ErrCredentialRevoked and returns the latency.
func (e *ifEnv) revokedWithin(t *testing.T, at time.Time, bound time.Duration, what string) time.Duration {
	t.Helper()
	x, ok := e.ended(t, bound+time.Second)
	if !ok {
		t.Fatalf("%s: the stream still runs %v after the act (frames %d)", what, bound+time.Second, e.stream.frames.Load())
	}
	took := x.at.Sub(at)
	t.Logf("%s: the stream ended %v after the act (bound %v), cause %v, %d frames", what, took.Round(time.Millisecond), bound, x.cause, x.frames)
	if !errors.Is(x.cause, auth.ErrCredentialRevoked) {
		t.Errorf("%s: cause %v, want auth.ErrCredentialRevoked", what, x.cause)
	}
	if took > bound {
		t.Errorf("%s: ended %v after the act, want ≤ %v", what, took, bound)
	}
	return took
}

func (e *ifEnv) emptyAfter(t *testing.T) {
	t.Helper()
	if !ifWait(2*time.Second, func() bool { s := e.door.Stats(); return s.Inflight == 0 && s.InflightTimers == 0 }) {
		s := e.door.Stats()
		t.Errorf("registry not empty after the requests: %d running, %d keys, %d timers", s.Inflight, s.InflightKeys, s.InflightTimers)
	}
}

// TestInflightRevokeEndsRunningRequest is R6 on the probe router (the
// stream route: internal/oai TestRevokeEndsRunningStream): a request runs
// behind the door; (local) model-key-revoke through the manage action ends
// it within 5 s with the cause auth.ErrCredentialRevoked and answers
// cancelled_inflight 1, the key's hash leaves the known-good set; (psql) an
// UPDATE of revoked_at reaches the registry through the NOTIFY of migration
// 165 and ends it the same way. A key without running requests answers 0.
// Red against the base: the action answers null and the stream runs on
// (see the wave report: 49 frames in the 5 s after the revocation, the end
// at the next 60-s re-auth of W02-08).
func TestInflightRevokeEndsRunningRequest(t *testing.T) {
	t.Run("local: model-key-revoke", func(t *testing.T) {
		e := ifSetup(t, ifOpts{sweep: ifLongSweep})
		admin := ifServerAdmin(t, e)
		mh := mcHandler(t, e.seed, true, false)
		mh.SetInflight(e.door.Inflight())
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		_, idle, _ := e.consumer(t, store.ModelConsumerSpec{})
		e.open(t, key)
		e.still(t, 300*time.Millisecond, "before the revocation")
		if !e.door.known.contains(key.Hash()) {
			t.Fatal("the accepted key is not known-good")
		}
		at := time.Now()
		status, body, resp := mcCall(t, mh, admin, "model-key-revoke", map[string]any{"key_id": k.ID})
		e.revokedWithin(t, at, ifBound, "model-key-revoke")
		if v, ok := resp["cancelled_inflight"].(float64); status != http.StatusOK || !ok || v != 1 {
			t.Errorf("model-key-revoke: %d %s, want 200 with cancelled_inflight 1", status, body)
		}
		if e.door.known.contains(key.Hash()) {
			t.Error("the revoked key's hash stays known-good (W01-6 review N2)")
		}
		if status, body, resp := mcCall(t, mh, admin, "model-key-revoke", map[string]any{"key_id": idle.ID}); status != http.StatusOK || resp["cancelled_inflight"] != float64(0) {
			t.Errorf("revoke of a key without running requests: %d %s, want cancelled_inflight 0", status, body)
		}
		e.emptyAfter(t)
	})
	t.Run("local: model-consumer-delete", func(t *testing.T) {
		e := ifSetup(t, ifOpts{sweep: ifLongSweep})
		admin := ifServerAdmin(t, e)
		mh := mcHandler(t, e.seed, true, false)
		mh.SetInflight(e.door.Inflight())
		c, _, key := e.consumer(t, store.ModelConsumerSpec{})
		e.open(t, key)
		e.open(t, key)
		at := time.Now()
		status, body, resp := mcCall(t, mh, admin, "model-consumer-delete", map[string]any{"id": c.ID})
		e.revokedWithin(t, at, ifBound, "model-consumer-delete (1)")
		e.revokedWithin(t, at, ifBound, "model-consumer-delete (2)")
		if v, ok := resp["cancelled_inflight"].(float64); status != http.StatusOK || !ok || v != 2 {
			t.Errorf("model-consumer-delete: %d %s, want 200 with cancelled_inflight 2", status, body)
		}
		e.emptyAfter(t)
	})
	t.Run("psql: UPDATE revoked_at (NOTIFY)", func(t *testing.T) {
		e := ifSetup(t, ifOpts{sweep: ifLongSweep, listen: true})
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		e.open(t, key)
		e.still(t, 300*time.Millisecond, "before the revocation")
		at := time.Now()
		if _, err := e.seed.Exec(context.Background(), `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, k.ID); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		e.revokedWithin(t, at, ifBound, "psql revoke")
		if e.door.known.contains(key.Hash()) {
			t.Error("the revoked key's hash stays known-good")
		}
		e.emptyAfter(t)
	})
}

// TestInflightExpiryEndsRunningRequest is B19: a key whose expires_at
// passes while a request runs ends it within 2 s of the expiry — (entry)
// the expiry the door read at the request's entry arms the key's timer, no
// query; (moved) an UPDATE of expires_at to now()+2 s reaches the timer
// through the NOTIFY payload. Red against the base: the stream runs on.
func TestInflightExpiryEndsRunningRequest(t *testing.T) {
	t.Run("entry", func(t *testing.T) {
		e := ifSetup(t, ifOpts{sweep: ifLongSweep})
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		var exp time.Time
		if err := e.seed.QueryRow(context.Background(),
			`UPDATE context_model_keys SET expires_at = now() + interval '2 seconds' WHERE id = $1::uuid RETURNING expires_at`, k.ID).Scan(&exp); err != nil {
			t.Fatalf("set expiry: %v", err)
		}
		e.open(t, key)
		if s := e.door.Stats(); s.InflightTimers != 1 {
			t.Errorf("%d expiry timers for one key with an expiry, want 1", s.InflightTimers)
		}
		e.revokedWithin(t, exp, 2*time.Second, "expiry known at entry")
		e.emptyAfter(t)
	})
	t.Run("moved by NOTIFY", func(t *testing.T) {
		e := ifSetup(t, ifOpts{sweep: ifLongSweep, listen: true})
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		e.open(t, key)
		var exp time.Time
		if err := e.seed.QueryRow(context.Background(),
			`UPDATE context_model_keys SET expires_at = now() + interval '2 seconds' WHERE id = $1::uuid RETURNING expires_at`, k.ID).Scan(&exp); err != nil {
			t.Fatalf("set expiry: %v", err)
		}
		e.still(t, time.Second, "before the expiry")
		e.revokedWithin(t, exp, 2*time.Second, "expiry moved by NOTIFY")
		e.emptyAfter(t)
	})
}

// TestInflightRotationGraceKeepsRunning is the negative probe of design/01
// §7 W01-8 and the timer re-arm from the payload: model-key-rotate with
// grace 3 s while a request runs on the old key — the request runs on until
// the grace ends (the rotation shortens the predecessor's expires_at, it
// does not revoke it), and ends within 2 s after. Red: a registry that does
// not re-arm the timer from the payload (mutant) lets it run until the
// sweep (an hour here).
func TestInflightRotationGraceKeepsRunning(t *testing.T) {
	e := ifSetup(t, ifOpts{sweep: ifLongSweep, listen: true})
	ctx := context.Background()
	admin := ifServerAdmin(t, e)
	mh := mcHandler(t, e.seed, true, false)
	mh.SetInflight(e.door.Inflight())
	c, k, key := e.consumer(t, store.ModelConsumerSpec{})
	e.open(t, key)
	at := time.Now()
	status, body, _ := mcCall(t, mh, admin, "model-key-rotate", map[string]any{"consumer_id": c.ID, "grace": 3})
	if status != http.StatusOK {
		t.Fatalf("model-key-rotate: %d %s", status, body)
	}
	var exp time.Time
	if err := e.seed.QueryRow(ctx, `SELECT expires_at FROM context_model_keys WHERE id = $1::uuid`, k.ID).Scan(&exp); err != nil {
		t.Fatalf("read the predecessor's expiry: %v", err)
	}
	t.Logf("rotated at +0, the predecessor expires %v after the rotation", exp.Sub(at).Round(time.Millisecond))
	e.still(t, time.Until(exp)-500*time.Millisecond, "inside the grace")
	e.revokedWithin(t, exp, 2*time.Second, "end of the grace")
	e.emptyAfter(t)
}

// ifTenant creates an active tenant owning scope.
func ifTenant(t *testing.T, e *ifEnv, slug, scope string) string {
	t.Helper()
	var id string
	if err := e.seed.QueryRow(context.Background(),
		`INSERT INTO context_tenants (slug, display_name) VALUES ($1, $2) RETURNING id::text`, slug, slug).Scan(&id); err != nil {
		t.Fatalf("tenant %s: %v", slug, err)
	}
	if scope != "" {
		if _, err := e.seed.Exec(context.Background(), `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid)`, scope, id); err != nil {
			t.Fatalf("scope %s: %v", scope, err)
		}
	}
	return id
}

// TestInflightLivenessPerLayer is B10 and the other layers of gates 2-4: a
// request runs on a consumer of a tenant with a tenant_scope; each of
// (consumer deactivated) UPDATE active=false, (tenant suspended) UPDATE
// status, (scope moved) UPDATE context_tenant_scopes SET tenant_id=<other>,
// (scope deleted) DELETE of the scope, (tenant prune) store.PruneTenant ends
// it within 5 s through the NOTIFY. Red against the base: it runs on.
func TestInflightLivenessPerLayer(t *testing.T) {
	ctx := context.Background()
	for _, c := range []struct {
		name string
		act  func(t *testing.T, e *ifEnv, tenant, scope, consumer string)
	}{
		{"consumer deactivated", func(t *testing.T, e *ifEnv, _, _, consumer string) {
			ifExec(t, e, `UPDATE context_model_consumers SET active = false WHERE id = $1::uuid`, consumer)
		}},
		{"tenant suspended", func(t *testing.T, e *ifEnv, tenant, _, _ string) {
			ifExec(t, e, `UPDATE context_tenants SET status = 'suspended' WHERE id = $1::uuid`, tenant)
		}},
		{"scope moved to another tenant", func(t *testing.T, e *ifEnv, _, scope, _ string) {
			other := ifTenant(t, e, "if-other", "")
			ifExec(t, e, `UPDATE context_tenant_scopes SET tenant_id = $2::uuid WHERE scope = $1`, scope, other)
		}},
		{"scope deleted", func(t *testing.T, e *ifEnv, _, scope, _ string) {
			ifExec(t, e, `DELETE FROM context_tenant_scopes WHERE scope = $1`, scope)
		}},
		{"tenant prune", func(t *testing.T, e *ifEnv, tenant, _, _ string) {
			if err := store.PruneTenant(ctx, e.seed, tenant, e.stamp); err != nil {
				t.Fatalf("prune: %v", err)
			}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			e := ifSetup(t, ifOpts{sweep: ifLongSweep, listen: true})
			scope := "ifscope"
			tenant := ifTenant(t, e, "if-tenant", scope)
			cons, _, key := e.consumer(t, store.ModelConsumerSpec{TenantID: tenant, TenantScope: &scope})
			e.open(t, key)
			e.still(t, 300*time.Millisecond, "before the act")
			at := time.Now()
			c.act(t, e, tenant, scope, cons.ID)
			e.revokedWithin(t, at, ifBound, c.name)
			e.emptyAfter(t)
		})
	}
}

func ifExec(t *testing.T, e *ifEnv, sql string, args ...any) {
	t.Helper()
	if _, err := e.seed.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// TestInflightListenerGap is B29: the listener's connection is cut
// (pg_terminate_backend), the key is revoked while it is away (its NOTIFY is
// lost), and the stream ends after the reconnect — the backlog asks the
// registry for a sweep — within the reconnect delay plus 2 s. Red: a channel
// handler without the backlog sweep (ifSink.noBacklog) lets it run until the
// periodic sweep (an hour here).
func TestInflightListenerGap(t *testing.T) {
	run := func(t *testing.T, noBacklog bool) {
		const reconnect = 2 * time.Second
		e := ifSetup(t, ifOpts{sweep: ifLongSweep, listen: true, reconnect: reconnect, noBacklog: noBacklog})
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		e.open(t, key)
		var killed int
		if err := e.seed.QueryRow(context.Background(),
			`SELECT count(pg_terminate_backend(pid)) FROM pg_stat_activity
			  WHERE datname = current_database() AND pid <> pg_backend_pid() AND query ILIKE 'listen %'`).Scan(&killed); err != nil || killed != 1 {
			t.Fatalf("cut the listener: %d connections (err %v), want 1", killed, err)
		}
		time.Sleep(200 * time.Millisecond) // inside the reconnect delay
		at := time.Now()
		ifExec(t, e, `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, k.ID)
		if noBacklog {
			e.still(t, reconnect+3*time.Second, "red: no backlog sweep")
			if e.sink.backlogs.Load() < 2 {
				t.Errorf("the listener did not reconnect (%d connects)", e.sink.backlogs.Load())
			}
			return
		}
		e.revokedWithin(t, at, reconnect+2*time.Second, "revoked in the listener gap")
		if n := e.sink.backlogs.Load(); n < 2 {
			t.Errorf("%d connects of the listener, want the reconnect", n)
		}
		e.emptyAfter(t)
	}
	t.Run("backlog sweep", func(t *testing.T) { run(t, false) })
	t.Run("red: channel handler without the backlog sweep", func(t *testing.T) { run(t, true) })
}

// TestInflightSweepIsTheSecondLine: without the NOTIFY (the four triggers of
// migration 165 dropped in the test — design/01 §7: "NOTIFY-Trigger im Test
// entfernt") a psql revocation still ends the stream within the sweep
// interval (1 s) plus 1 s. Red: no periodic sweep (an hour) — it runs on.
func TestInflightSweepIsTheSecondLine(t *testing.T) {
	for _, c := range []struct {
		name  string
		sweep time.Duration
	}{{"sweep 1 s", time.Second}, {"red: no periodic sweep", ifLongSweep}} {
		t.Run(c.name, func(t *testing.T) {
			e := ifSetup(t, ifOpts{sweep: c.sweep, listen: true})
			for _, trg := range []string{"trg_model_keys_access_notify ON context_model_keys", "trg_model_consumers_access_notify ON context_model_consumers",
				"trg_tenants_access_notify ON context_tenants", "trg_tenant_scopes_access_notify ON context_tenant_scopes"} {
				ifExec(t, e, `DROP TRIGGER `+trg)
			}
			_, k, key := e.consumer(t, store.ModelConsumerSpec{})
			e.open(t, key)
			at := time.Now()
			ifExec(t, e, `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, k.ID)
			if c.sweep == ifLongSweep {
				e.still(t, 3*time.Second, c.name)
				if n := e.sink.notifies.Load(); n != 0 {
					t.Errorf("%d notifications without the triggers, want 0", n)
				}
				return
			}
			e.revokedWithin(t, at, 2*time.Second, c.name)
			e.emptyAfter(t)
		})
	}
}

// TestInflightSweepCost: 1 000 requests running on 50 keys ⇒ exactly ONE
// ctx_model_auth_live statement per sweep period, whatever the number of
// requests (pgx QueryTracer over the door's pool); a failed sweep ends
// nothing.
func TestInflightSweepCost(t *testing.T) {
	e := ifSetup(t, ifOpts{sweep: time.Second, quiet: true})
	keys := make([]modelkey.Key, 50)
	for i := range keys {
		_, _, keys[i] = e.consumer(t, store.ModelConsumerSpec{})
	}
	for i := range 1000 {
		e.open(t, keys[i%len(keys)])
	}
	if s := e.door.Stats(); s.Inflight != 1000 || s.InflightKeys != 50 {
		t.Fatalf("registry %d running on %d keys, want 1000 on 50", s.Inflight, s.InflightKeys)
	}
	live := func() int { return int(e.tracer.live.Load()) }
	s0, q0 := e.door.Stats().InflightSweeps, live()
	time.Sleep(3500 * time.Millisecond)
	s1, q1 := e.door.Stats().InflightSweeps, live()
	t.Logf("3.5 s at a 1-s sweep with 1 000 running requests on 50 keys: %d sweeps, %d ctx_model_auth_live statements", s1-s0, q1-q0)
	if s1-s0 < 2 || s1-s0 > 4 || q1-q0 != int(s1-s0) {
		t.Errorf("%d sweeps, %d statements — want 3 ± 1 sweeps and exactly one statement each", s1-s0, q1-q0)
	}
	if e.stream.running.Load() != 1000 {
		t.Errorf("%d streams run, want 1000 (a sweep of live keys ends nothing)", e.stream.running.Load())
	}
}

// ifReached carries the channel a leak-probe request closes when it reaches
// the handler behind the door.
type ifReached struct{}

// TestInflightLeakProbe: 10⁴ requests through the door on 20 keys (10 with
// an expiry, so their timers are armed while they run), 8 at a time, each
// with a random end — done, client gone, a panic in the handler, or its key
// revoked in the registry while it runs — leave the registry empty, never
// more timers than distinct keys while they run, none afterwards, and the
// goroutine count where it was (± 0 after settling).
func TestInflightLeakProbe(t *testing.T) {
	e := ifSetup(t, ifOpts{sweep: ifLongSweep})
	// A revoked key leaves the known-good set, so its next request needs a
	// probe token; the probe bucket is not this gate's subject.
	e.settings.auth(func(a *oai.AuthSettings) { a.ProbeRate = 1 << 20 })
	ctx := context.Background()
	type keyOf struct {
		id  string
		key modelkey.Key
	}
	keys := make([]keyOf, 20)
	for i := range keys {
		_, k, key := e.consumer(t, store.ModelConsumerSpec{})
		keys[i] = keyOf{k.ID, key}
		if i%2 == 0 {
			ifExec(t, e, `UPDATE context_model_keys SET expires_at = now() + interval '1 hour' WHERE id = $1::uuid`, k.ID)
		}
	}
	var maxTimers, reachedN atomic.Int64
	leak := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s := e.door.Stats(); int64(s.InflightTimers) > maxTimers.Load() {
			maxTimers.Store(int64(s.InflightTimers))
		}
		reachedN.Add(1)
		close(r.Context().Value(ifReached{}).(chan struct{}))
		switch r.Header.Get("X-Outcome") {
		case "done":
			return
		case "panic":
			panic("w01-8 leak probe: a handler panics")
		}
		<-r.Context().Done()
	})
	h := e.door.Handler(leak)
	g0 := runtime.NumGoroutine()
	var tally [4]atomic.Int64
	var codesMu sync.Mutex
	codes := map[int]int{}
	var bodies []string
	outcomes := []string{"done", "abort", "panic", "revoke"}
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range 8 {
		wg.Go(func() {
			for i := range jobs {
				k := keys[i%len(keys)]
				o := rand.IntN(4)
				tally[o].Add(1)
				reached := make(chan struct{})
				rctx, cancel := context.WithCancel(context.WithValue(ctx, ifReached{}, reached))
				r := httptest.NewRequestWithContext(rctx, http.MethodPost, "/v1/chat/completions", nil)
				r.RemoteAddr = "10.7.7.7:40000"
				r.TLS = &tls.ConnectionState{}
				r.Header.Set("Authorization", "Bearer "+k.key.Plaintext())
				r.Header.Set("X-Outcome", outcomes[o])
				served := make(chan struct{})
				go func() {
					defer close(served)
					rec := httptest.NewRecorder()
					defer func() {
						_ = recover()
						codesMu.Lock()
						codes[rec.Code]++
						if rec.Code != http.StatusOK && len(bodies) < 3 {
							bodies = append(bodies, strings.TrimSpace(rec.Body.String()))
						}
						codesMu.Unlock()
					}()
					h.ServeHTTP(rec, r)
				}()
				// Client gone and revoked act while the request runs — after
				// it reached the handler: a request still at the door is not
				// registered yet (and in production a revoked key's own
				// database check refuses it).
				if o == 1 || o == 3 {
					select {
					case <-reached:
					case <-served:
					}
					time.Sleep(time.Duration(rand.IntN(500)) * time.Microsecond)
				}
				switch outcomes[o] {
				case "abort":
					cancel()
				case "revoke":
					e.door.Inflight().CancelKey(k.id)
				}
				select {
				case <-served:
				case <-time.After(10 * time.Second):
					t.Errorf("request %d (%s) still runs after 10 s", i, outcomes[o])
					cancel()
					<-served
				}
				cancel()
			}
		})
	}
	for i := range 10000 {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	e.emptyAfter(t)
	var g1 int
	settled := ifWait(5*time.Second, func() bool { g1 = runtime.NumGoroutine(); return g1 <= g0 })
	s := e.door.Stats()
	t.Logf("10⁴ requests (done %d, client gone %d, panic %d, revoked %d), %d reached the handler, recorder codes %v: registry %d running, %d keys, %d timers; max timers while running %d (keys 20); goroutines %d → %d; ended by the registry %d",
		tally[0].Load(), tally[1].Load(), tally[2].Load(), tally[3].Load(), reachedN.Load(), codes, s.Inflight, s.InflightKeys, s.InflightTimers, maxTimers.Load(), g0, g1, s.InflightCancelled)
	t.Logf("first refusals: %q; door stats %+v", bodies, s)
	if reachedN.Load() != 10000 {
		t.Errorf("%d of 10⁴ requests reached the handler behind the door, want all — the probe must exercise the registry, not the door's refusals", reachedN.Load())
	}
	if maxTimers.Load() > int64(len(keys)) {
		t.Errorf("%d timers while running, want ≤ %d distinct keys", maxTimers.Load(), len(keys))
	}
	if !settled {
		t.Errorf("goroutines %d → %d after settling, want ± 0", g0, g1)
	}
}

// TestInflightRevocationDuringTheDoorCheck (Fremd-Review 1 B5/X8): the key
// is revoked — the manage action's write and CancelKey — while the door's
// ctx_model_auth for a new request has returned "valid" but the request is
// not registered yet (a pgx tracer hook runs the revocation inside the
// door's database step). CancelKey finds no entry; the door's mark, read
// before its check, has moved, so the registration has the key checked once
// more and the request ends within the bound — not at the next sweep (an
// hour here). Red: the door reads its mark after the check (X8), or
// CancelKey moves no mark (X1).
func TestInflightRevocationDuringTheDoorCheck(t *testing.T) {
	e := ifSetup(t, ifOpts{sweep: ifLongSweep})
	_, k, key := e.consumer(t, store.ModelConsumerSpec{})
	var counted atomic.Int64
	hook := func() { // runs on the request's goroutine: no t.Fatal here
		if _, err := e.seed.Exec(context.Background(), `UPDATE context_model_keys SET revoked_at = now() WHERE id = $1::uuid`, k.ID); err != nil {
			t.Errorf("revoke inside the door's check: %v", err)
		}
		counted.Store(int64(e.door.Inflight().CancelKey(k.ID)))
	}
	e.tracer.afterAuth.Store(&hook)
	at := time.Now()
	e.open(t, key)
	if n := counted.Load(); n != 0 {
		t.Fatalf("CancelKey inside the door's check counted %d, want 0 (nothing registered yet)", n)
	}
	e.revokedWithin(t, at, ifBound, "revoked while the door's check ran")
	e.emptyAfter(t)
}
