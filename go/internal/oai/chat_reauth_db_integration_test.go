//go:build integration

package oai_test

// The watcher's database reads under a disturbed database and the consumer
// the re-authentication reads now (W02-08 hardening after Fremd-Review 2,
// H-1 and M-1), against a real database (testdb), the real door
// (handler.ReauthConsumer) and a HasGrant over the SQL of
// store.ConsumerGrantsAlias (inlined: depguard oai-isolation keeps
// internal/store out of internal/oai, tests included).
//
// H-1: every grant read of the watcher — the forced read of the re-auth
// period, the read of a tick after the alias registry moved, the check
// right before a non-stream write — and the grant read of the entry run
// under the door's own deadline (1 s). A grant read that fails or runs past
// it is the period's failed check, in the same count as a failed key check
// (reauthFailLimit); the kill switch and the alias check never wait behind
// a database read for longer than that deadline.
//
// M-1: the re-authentication hands back the consumer as the door reads it
// now, and the period resolves the alias with it — all_models withdrawn
// or a tenant_scope change that takes the alias away ends a running
// request like a withdrawn grant (model_not_found), stream and non-stream.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/testdb"
)

// doorDeadline is the door's database deadline (handler
// modelAuthDBDeadline), the bound of every grant read of a running request.
const doorDeadline = time.Second

type dbGateOpts struct {
	allModels   bool
	grant       bool          // a grant on the rig's alias "chat"
	tenantScope string        // the consumer's tenant_scope ("" = NULL)
	maxConns    int32         // > 0: the door and HasGrant share an own pool of this size
	steps       []sseStep     // stream upstream
	nsDelay     time.Duration // > 0: non-stream upstream answering after nsDelay
}

// dbGateEnv is one database, one consumer with one live ctxm_ key behind
// the real door, the chat rig, and the test's own admin pool for the acts.
type dbGateEnv struct {
	*reauthEnv
	admin     *pgxpool.Pool
	consumer  string
	grantHits atomic.Int64
}

func newDBGateEnv(t *testing.T, o dbGateOpts) *dbGateEnv {
	t.Helper()
	ctx := context.Background()
	admin, dsn := testdb.SetupTestDBWithDSN(t)
	e := &dbGateEnv{reauthEnv: &reauthEnv{ended: newUpstreamEnd()}, admin: admin}
	app := admin
	if o.maxConns > 0 {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err != nil {
			t.Fatalf("parse dsn: %v", err)
		}
		cfg.MaxConns, cfg.MinConns = o.maxConns, 0
		app, err = pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			t.Fatalf("app pool: %v", err)
		}
		t.Cleanup(app.Close)
	}
	e.pool = app
	if o.tenantScope != "" {
		e.exec(t, `INSERT INTO context_tenant_scopes (scope, tenant_id) VALUES ($1, $2::uuid) ON CONFLICT DO NOTHING`, o.tenantScope, reauthTenant)
	}
	var scope any
	if o.tenantScope != "" {
		scope = o.tenantScope
	}
	key, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, all_models, tenant_scope, prompt_sensitivity, fair_weight, admission_lane, allowed_cidrs, allow_plaintext)
		 VALUES ($1::uuid, 'w02-08-h2', $2, $3, 'public', 1, 'urgent', ARRAY['127.0.0.0/8','::1/128']::cidr[], true) RETURNING id::text`,
		reauthTenant, o.allModels, scope).Scan(&e.consumer); err != nil {
		t.Fatalf("insert consumer: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, created_at) VALUES ($1::uuid, $2, $3, clock_timestamp()) RETURNING id::text`,
		e.consumer, key.Hash(), key.Hint()).Scan(&e.keyID); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	e.key = key
	// The rig's alias "chat" (idChat) as a real row, so a grant can name it.
	e.exec(t, `INSERT INTO context_model_aliases (id, name, kind, role) VALUES ($1::uuid, 'chat', 'chat', 'chat')`, idChat)
	if o.grant {
		e.exec(t, `INSERT INTO context_model_consumer_grants (consumer_id, alias_id) VALUES ($1::uuid, $2::uuid)`, e.consumer, idChat)
	}
	e.up = newUpstream(t, answerSGLang)
	if o.nsDelay > 0 {
		d := o.nsDelay
		e.up.setRespond(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				e.ended.mark(true)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(answerSGLang))
			e.ended.mark(false)
		})
	} else {
		e.up.setRespond(sseRespond(o.steps, nil, e.ended))
	}
	row := e.up.row("spark", backends.TrustFull, 10)
	row.Timeouts = map[string]int{"chat": 300}
	e.rig = newChatRig(t, []backends.Backend{row}, 4, 1, withStreamSettings,
		func(r *chatRig) {
			r.setSettings(func(s *oai.Settings) {
				s.Auth = reauthProdAuth
				if o.tenantScope != "" {
					s.ExportableRoles = []string{"chat"} // the tenant alias reaches the _global row
				}
			})
		})
	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	e.door = handler.ModelAuth(handler.ModelAuthConfig{
		Pool:     app,
		Settings: func() oai.Settings { return *e.rig.settings.Load() },
		Usage:    reauthNoTouch{},
		Reject:   reauthReject,
		Logger:   slog.New(slog.DiscardHandler),
	})
	e.rig.deps.HasGrant = func(ctx context.Context, consumerID, aliasID string) (bool, error) {
		e.grantHits.Add(1)
		var ok bool
		err := app.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM context_model_consumer_grants WHERE consumer_id = $1::uuid AND alias_id = $2::uuid)`,
			consumerID, aliasID).Scan(&ok)
		if err != nil {
			return false, fmt.Errorf("model_grants: probe: %w", err)
		}
		return ok, nil
	}
	return e
}

func (e *dbGateEnv) exec(t *testing.T, sql string, args ...any) {
	t.Helper()
	if _, err := e.admin.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", sql, err)
	}
}

// exhaust holds every connection of the app pool until release is called.
func (e *dbGateEnv) exhaust(t *testing.T, n int) (release func()) {
	t.Helper()
	conns := make([]*pgxpool.Conn, 0, n)
	for i := 0; i < n; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		c, err := e.pool.Acquire(ctx)
		cancel()
		if err != nil {
			t.Fatalf("exhaust acquire %d: %v", i, err)
		}
		conns = append(conns, c)
	}
	var once atomic.Bool
	return func() {
		if once.CompareAndSwap(false, true) {
			for _, c := range conns {
				c.Release()
			}
		}
	}
}

// lockGrants takes the grants table ACCESS EXCLUSIVE (a migration, VACUUM
// FULL in the lock queue) until unlock is called; the rest of the database
// answers.
func (e *dbGateEnv) lockGrants(t *testing.T) (unlock func()) {
	t.Helper()
	tx, err := e.admin.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(context.Background(), `LOCK TABLE context_model_consumer_grants IN ACCESS EXCLUSIVE MODE`); err != nil {
		t.Fatal(err)
	}
	var once atomic.Bool
	unlock = func() {
		if once.CompareAndSwap(false, true) {
			_ = tx.Rollback(context.Background())
		}
	}
	t.Cleanup(unlock)
	return unlock
}

// waitCalls waits until n re-auth calls have returned.
func (e *dbGateEnv) waitCalls(t *testing.T, n int, within time.Duration) {
	t.Helper()
	if !waitFor(within, func() bool { return len(e.recorded()) >= n }) {
		t.Fatalf("re-auth call %d did not return within %v", n, within)
	}
}

// endOf reads the stream to its end and reports its error frame and the
// time of its last data event.
func endOf(t *testing.T, ls *reauthLive, within time.Duration) (code string, last bool, at time.Time, a streamAnswer) {
	t.Helper()
	a = ls.answer(t, within)
	code, last = a.errorFrame(t)
	return code, last, reauthLastData(a), a
}

// TestWatcherGrantReadDeadlineDB is H-1 on the real database: a grant
// consumer's running stream while (a) the process pool is exhausted (the
// door and HasGrant share it, as in production) or (b) the grants table is
// locked while the rest of the database answers. A revoked key ends within
// two re-auth periods plus the door's deadline (auth_unavailable: the key
// cannot be checked), the kill switch within one watcher tick plus that
// deadline, a lock that outlasts two periods ends the stream with
// auth_unavailable; the all_models consumer (no grant read) is the
// control. Red without the deadline: the watcher hangs in the grant read
// until the fault ends.
func TestWatcherGrantReadDeadlineDB(t *testing.T) {
	steps := reauthPaced(200, 100*time.Millisecond) // 20 s of frames
	revokedKey := func(t *testing.T, allModels bool) {
		const period = time.Second
		defer oai.SetReauthEvery(period)()
		e := newDBGateEnv(t, dbGateOpts{allModels: allModels, grant: !allModels, maxConns: 2, steps: steps})
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		e.waitCalls(t, 1, 3*time.Second)
		time.Sleep(50 * time.Millisecond) // the period's grant read is done
		revokedAt := time.Now()
		e.revoke(t)
		release := e.exhaust(t, 2)
		code, last, at, a := endOf(t, ls, 30*time.Second)
		release()
		took := at.Sub(revokedAt)
		t.Logf("all_models=%v: revoke → frame %q after %v (bound %v), dones %d, grant probes %d", allModels, code, took.Round(time.Millisecond),
			2*period+doorDeadline, a.dones(), e.grantHits.Load())
		if code != oai.CodeAuthUnavailable || !last || a.dones() != 0 {
			t.Fatalf("frame %q (last %v, [DONE] %d), want the error frame auth_unavailable", code, last, a.dones())
		}
		if took > 2*period+doorDeadline+500*time.Millisecond {
			t.Errorf("revoked key streamed %v after the revocation under an exhausted pool, want ≤ 2 periods + the door's deadline (%v)", took, 2*period+doorDeadline)
		}
	}
	t.Run("pool exhausted: revoked key, grant consumer", func(t *testing.T) { revokedKey(t, false) })
	t.Run("pool exhausted: revoked key, all_models control", func(t *testing.T) { revokedKey(t, true) })

	// killSwitch: the fault starts right after the commit; once the first
	// period is in it (ready), the kill switch is flipped 200 ms later — in
	// the base form the watcher then hangs in the period's grant read.
	killSwitch := func(t *testing.T, fault func(e *dbGateEnv) (release func()), ready func(e *dbGateEnv, hits0 int64) bool) {
		const period, tick = 4 * time.Second, time.Second
		defer oai.SetReauthEvery(period)()
		defer oai.SetWatchEvery(tick)()
		e := newDBGateEnv(t, dbGateOpts{grant: true, maxConns: 2, steps: steps})
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		hits0 := e.grantHits.Load()
		release := fault(e)
		defer release()
		inFault := waitFor(period+3*time.Second, func() bool { return ready(e, hits0) })
		time.Sleep(200 * time.Millisecond)
		flipAt := time.Now()
		e.rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
		go func() {
			time.Sleep(8 * time.Second)
			release()
		}()
		code, _, at, _ := endOf(t, ls, 30*time.Second)
		took := at.Sub(flipAt)
		t.Logf("kill switch → frame %q after %v (bound tick + deadline %v); fault reached %v, re-auth calls %d, grant probes %d",
			code, took.Round(time.Millisecond), tick+doorDeadline, inFault, len(e.recorded()), e.grantHits.Load())
		if code != oai.CodeNotFound {
			t.Fatalf("frame %q, want the kill switch's not_found", code)
		}
		if took > tick+doorDeadline+500*time.Millisecond {
			t.Errorf("the kill switch acted %v after the flip, want ≤ one watcher tick + the door's deadline (%v): the watcher waited in a database read", took, tick+doorDeadline)
		}
	}
	t.Run("pool exhausted: kill switch", func(t *testing.T) {
		// ready: the period's key check returned (failed: the pool is held).
		killSwitch(t, func(e *dbGateEnv) func() { return e.exhaust(t, 2) },
			func(e *dbGateEnv, _ int64) bool { return len(e.recorded()) >= 1 })
	})
	t.Run("grants locked: kill switch", func(t *testing.T) {
		// ready: the period's grant read started (the key check passed).
		killSwitch(t, func(e *dbGateEnv) func() { return e.lockGrants(t) },
			func(e *dbGateEnv, hits0 int64) bool { return e.grantHits.Load() > hits0 })
	})
	t.Run("grants locked: two failed periods end the stream", func(t *testing.T) {
		const period = time.Second
		defer oai.SetReauthEvery(period)()
		e := newDBGateEnv(t, dbGateOpts{grant: true, steps: steps})
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		lockedAt := time.Now()
		e.lockGrants(t)
		code, last, at, a := endOf(t, ls, 30*time.Second)
		took := at.Sub(lockedAt)
		t.Logf("grants locked → frame %q after %v (bound 2 × (period + deadline) %v), dones %d, re-auth calls %d (valid %d), grant probes %d",
			code, took.Round(time.Millisecond), 2*(period+doorDeadline), a.dones(), len(e.recorded()), func() int { v, _, _ := e.tally(); return v }(), e.grantHits.Load())
		if code != oai.CodeAuthUnavailable || !last || a.dones() != 0 {
			t.Fatalf("frame %q (last %v, [DONE] %d), want auth_unavailable: a grant read past the deadline is a failed check", code, last, a.dones())
		}
		if took > 2*(period+doorDeadline)+500*time.Millisecond {
			t.Errorf("the stream ended %v after the lock, want ≤ 2 × (period + deadline) = %v", took, 2*(period+doorDeadline))
		}
	})
}

// TestEntryGrantReadDeadlineDB is H-1's entry half (the reviewer's
// hypothesis, reproduced here): a new request of a grant consumer while
// the grants table is locked. The entry's grant read runs under the door's
// deadline and answers what a failed grant read answers (design/02
// §4.10.2 "Auth-DB-Fehler": 503 internal_error) — a stream, which carries
// no request deadline, and a non-stream request alike. Red without the
// deadline: the request waits for the lock (a stream until the client
// gives up).
func TestEntryGrantReadDeadlineDB(t *testing.T) {
	for _, stream := range []bool{true, false} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			o := dbGateOpts{grant: true, steps: reauthPaced(5, 10*time.Millisecond)}
			if !stream {
				o.nsDelay = 10 * time.Millisecond
			}
			e := newDBGateEnv(t, o)
			srv := e.serve(t, e.counted(handler.ReauthConsumer))
			unlock := e.lockGrants(t)
			go func() {
				time.Sleep(8 * time.Second)
				unlock()
			}()
			body := chatBody("chat")
			if stream {
				body = streamBody("chat")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a := postStream(ctx, t, srv, body, e.bearer()...)
			took := a.end.Sub(a.start)
			t.Logf("entry under a locked grants table: %d x-should-retry=%q after %v (bound deadline + 1 s %v): %s", a.status, a.header.Get("x-should-retry"),
				took.Round(time.Millisecond), doorDeadline+time.Second, strings.TrimSpace(a.raw))
			if a.status != http.StatusServiceUnavailable || !strings.Contains(a.raw, `"code":"internal_error"`) {
				t.Errorf("answer %d %s, want 503 internal_error (a grant read that cannot run)", a.status, a.raw)
			}
			if took > doorDeadline+time.Second {
				t.Errorf("answered after %v, want ≤ the door's deadline + 1 s: the entry's grant read waited for the lock", took)
			}
		})
	}
}

// TestReauthFreshConsumerDB is M-1: the period resolves the alias with the
// consumer the door reads now. all_models withdrawn (model-consumer-update
// all_models:false) and a tenant_scope change that takes the alias away end
// a running request with model_not_found — a stream within the O-08-2
// bound (two periods + the door's deadline), a non-stream request never
// with a 200; a change that keeps the access (a grant on the alias, a
// _global alias under the new scope) lets it run. Red: the period resolves
// with the consumer of the entry (the stream runs to [DONE], non-stream
// answers 200).
func TestReauthFreshConsumerDB(t *testing.T) {
	const period = 300 * time.Millisecond
	defer oai.SetReauthEvery(period)()
	steps := reauthPaced(40, 100*time.Millisecond) // 4 s of frames
	const (
		withdrawAll   = `UPDATE context_model_consumers SET all_models = false WHERE id = $1::uuid`
		withdrawScope = `UPDATE context_model_consumers SET tenant_scope = NULL WHERE id = $1::uuid`
	)
	streamCase := func(t *testing.T, o dbGateOpts, model, act, wantCode string) {
		o.steps = steps
		e := newDBGateEnv(t, o)
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		ls := openReauthLive(t, srv, streamBody(model), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		at := time.Now()
		e.exec(t, act, e.consumer)
		code, last, frameAt, a := endOf(t, ls, 30*time.Second)
		took := frameAt.Sub(at)
		t.Logf("stream %q after %q: frame %q last %v dones %d, %v after the act (bound %v)", model, act, code, last, a.dones(),
			took.Round(time.Millisecond), 2*period+doorDeadline)
		if wantCode == "" {
			if code != "" || a.dones() != 1 {
				t.Fatalf("frame %q, [DONE] %d — the access was kept, the stream must run to [DONE]", code, a.dones())
			}
			return
		}
		if code != wantCode || !last || a.dones() != 0 {
			t.Fatalf("frame %q (last %v, [DONE] %d), want the error frame %s", code, last, a.dones(), wantCode)
		}
		if took > 2*period+doorDeadline {
			t.Errorf("frame %v after the act, want within the O-08-2 bound %v", took, 2*period+doorDeadline)
		}
		if !waitHeld(e.rig.disp, e.up.srv.URL, 0, 3*time.Second) {
			t.Error("the lease is still held")
		}
	}
	nonStreamCase := func(t *testing.T, o dbGateOpts, model, act string, wantStatus int) {
		o.nsDelay = 4 * time.Second
		e := newDBGateEnv(t, o)
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		acted := make(chan error, 1)
		go func() {
			time.Sleep(500 * time.Millisecond)
			_, err := e.admin.Exec(context.Background(), act, e.consumer)
			acted <- err
		}()
		a := postStream(context.Background(), t, srv, chatBody(model), e.bearer()...)
		if err := <-acted; err != nil {
			t.Fatalf("%s: %v", act, err)
		}
		t.Logf("non-stream %q after %q: %d after %v: %s", model, act, a.status, a.end.Sub(a.start).Round(time.Millisecond), strings.TrimSpace(a.raw))
		if a.status != wantStatus {
			t.Fatalf("answer %d %s, want %d", a.status, a.raw, wantStatus)
		}
		if wantStatus == http.StatusNotFound && !strings.Contains(a.raw, `"code":"model_not_found"`) {
			t.Fatalf("answer %s, want model_not_found", a.raw)
		}
	}
	t.Run("stream: all_models withdrawn", func(t *testing.T) {
		streamCase(t, dbGateOpts{allModels: true}, "chat", withdrawAll, oai.CodeModelNotFound)
	})
	t.Run("non-stream: all_models withdrawn", func(t *testing.T) {
		nonStreamCase(t, dbGateOpts{allModels: true}, "chat", withdrawAll, http.StatusNotFound)
	})
	t.Run("stream: tenant_scope change takes the alias away", func(t *testing.T) {
		streamCase(t, dbGateOpts{allModels: true, tenantScope: tenantScope}, "tchat", withdrawScope, oai.CodeModelNotFound)
	})
	t.Run("non-stream: tenant_scope change takes the alias away", func(t *testing.T) {
		nonStreamCase(t, dbGateOpts{allModels: true, tenantScope: tenantScope}, "tchat", withdrawScope, http.StatusNotFound)
	})
	t.Run("control stream: all_models withdrawn, grant held", func(t *testing.T) {
		streamCase(t, dbGateOpts{allModels: true, grant: true}, "chat", withdrawAll, "")
	})
	t.Run("control non-stream: all_models withdrawn, grant held", func(t *testing.T) {
		nonStreamCase(t, dbGateOpts{allModels: true, grant: true}, "chat", withdrawAll, http.StatusOK)
	})
	t.Run("control stream: tenant_scope change keeps a _global alias", func(t *testing.T) {
		streamCase(t, dbGateOpts{allModels: true, tenantScope: tenantScope}, "chat", withdrawScope, "")
	})
}
