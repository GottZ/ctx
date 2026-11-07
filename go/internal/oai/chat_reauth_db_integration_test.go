//go:build integration

package oai_test

// The grant reads of a running request and of the entry under a disturbed
// database (W02-08 hardening after Fremd-Review 2, H-1), against a real
// database (testdb), the real door (handler.ModelAuth, mounted by the
// integration harness of chat_stream_revoke_integration_test.go) and a
// HasGrant over the SQL of store.ConsumerGrantsAlias (inlined: depguard
// oai-isolation keeps internal/store out of internal/oai, tests included).
//
// H-1: every grant read of the watcher — the read of a tick after the alias
// registry moved, the check right before a non-stream write — and the grant
// read of the entry run under the door's own deadline (1 s); the kill
// switch never waits behind a database read for longer than that deadline.
// A key that stops authenticating is not the watcher's (W01-8: the door's
// in-flight registry cancels the request context) and not this file's.

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
	grant    bool          // a grant on the rig's alias "chat"
	maxConns int32         // > 0: the door and HasGrant share an own pool of this size
	steps    []sseStep     // stream upstream
	nsDelay  time.Duration // > 0: non-stream upstream answering after nsDelay
}

// dbGateEnv is one database, one consumer without all_models with one live
// ctxm_ key behind the real door, the chat rig, and the test's own admin
// pool for the acts.
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
	key, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if err := admin.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, all_models, prompt_sensitivity, fair_weight, admission_lane, allowed_cidrs, allow_plaintext)
		 VALUES ($1::uuid, 'w02-08-h2', false, 'public', 1, 'urgent', ARRAY['127.0.0.0/8','::1/128']::cidr[], true) RETURNING id::text`,
		reauthTenant).Scan(&e.consumer); err != nil {
		t.Fatalf("insert consumer: %v", err)
	}
	if _, err := admin.Exec(ctx,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, created_at) VALUES ($1::uuid, $2, $3, clock_timestamp())`,
		e.consumer, key.Hash(), key.Hint()); err != nil {
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
			r.setSettings(func(s *oai.Settings) { s.Auth = reauthProdAuth })
		})
	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	e.door = handler.ModelAuth(handler.ModelAuthConfig{
		Pool:     app,
		Settings: func() oai.Settings { return *e.rig.settings.Load() },
		Usage:    reauthNoTouch{},
		Reject:   reauthReject,
		Logger:   slog.New(slog.DiscardHandler),
	})
	e.run(t) // the door's registry worker, as in production
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

// endOf reads the stream to its end and reports its error frame and the
// time of its last data event.
func endOf(t *testing.T, ls *reauthLive, within time.Duration) (code string, at time.Time) {
	t.Helper()
	a := ls.answer(t, within)
	code, _ = a.errorFrame(t)
	return code, reauthLastData(a)
}

// TestWatcherGrantReadDeadlineDB is H-1 on the real database: a grant
// consumer's running stream while (a) the process pool is exhausted (the
// door and HasGrant share it, as in production) or (b) the grants table is
// locked while the rest of the database answers; the alias registry moves
// (a reload that keeps the alias), so the next watcher tick reads the grant
// — into the fault. The kill switch, flipped while that read hangs, ends the
// stream within one watcher tick plus the door's deadline. Red without the
// deadline: the watcher hangs in the grant read until the fault ends (8 s).
func TestWatcherGrantReadDeadlineDB(t *testing.T) {
	steps := reauthPaced(200, 100*time.Millisecond) // 20 s of frames
	killSwitch := func(t *testing.T, fault func(e *dbGateEnv) (release func())) {
		const tick = time.Second
		defer oai.SetWatchEvery(tick)()
		e := newDBGateEnv(t, dbGateOpts{grant: true, maxConns: 2, steps: steps})
		srv := e.serve(t)
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		hits0 := e.grantHits.Load()
		release := fault(e)
		defer release()
		e.rig.reg.reload(t, chatAliases()) // the generation moves: the next tick reads the grant
		if !waitFor(tick+3*time.Second, func() bool { return e.grantHits.Load() > hits0 }) {
			t.Fatal("the tick made no grant read after the generation moved")
		}
		time.Sleep(200 * time.Millisecond) // inside the tick's hanging read
		flipAt := time.Now()
		e.rig.setSettings(func(s *oai.Settings) { s.Enabled = false })
		go func() {
			time.Sleep(8 * time.Second)
			release()
		}()
		code, at := endOf(t, ls, 30*time.Second)
		took := at.Sub(flipAt)
		t.Logf("kill switch → frame %q after %v (bound tick + deadline %v); grant probes %d",
			code, took.Round(time.Millisecond), tick+doorDeadline, e.grantHits.Load())
		if code != oai.CodeNotFound {
			t.Fatalf("frame %q, want the kill switch's not_found", code)
		}
		if took > tick+doorDeadline+500*time.Millisecond {
			t.Errorf("the kill switch acted %v after the flip, want ≤ one watcher tick + the door's deadline (%v): the watcher waited in a database read", took, tick+doorDeadline)
		}
	}
	t.Run("pool exhausted: kill switch", func(t *testing.T) {
		killSwitch(t, func(e *dbGateEnv) func() { return e.exhaust(t, 2) })
	})
	t.Run("grants locked: kill switch", func(t *testing.T) {
		killSwitch(t, func(e *dbGateEnv) func() { return e.lockGrants(t) })
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
			srv := e.serve(t)
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
