//go:build integration

package oai_test

// The revocation of running requests on the real stream route (W01-8,
// masterplan K22 and R6, variant A on W02-08's stream): a real ctxm_ model
// key behind the real model-key door (handler.ModelAuth, mounted as
// cmd/ctxd's model group mounts it) with its in-flight registry and the
// registry's worker running, the production consumer read
// (handler.ConsumerAuthFromContext). The database's own verdict reaches the
// route through the registry's ctx_model_auth_live check (here the periodic
// sweep; the NOTIFY delivery through the production listener is pinned in
// internal/handler and cmd/ctxd — depguard keeps internal/events and
// internal/config out of this package's tests). The rest of the route's dependencies come from the chat rig
// (chat_harness_test.go), the upstream and the SSE client from the stream
// harness (chat_stream_harness_test.go). The probe-router half (every
// trigger and layer, the listener gap, the sweep cost, the leak probe) is
// internal/handler/model_inflight_integration_test.go.
//
// The consumer and its key are inserted with the SQL of
// store.CreateConsumerWithKey (insertConsumerTx, insertModelKeyTx) and the
// revocation is the column ctx_model_auth reads (context_model_keys.
// revoked_at): internal/oai may not import internal/store, tests included
// (depguard oai-isolation, .golangci.yml). The local revocation of the
// manage action is the SQL write followed by the registry's CancelKey — the
// action's own wiring (cancelled_inflight) is pinned in internal/handler.
//
//	go test -tags=integration -p 1 -count=1 -run 'RevokeEndsRunningStream|RevokedKeyEndsRunningNonStream|ModelKeySurvivesLongStream' -v ./internal/oai/

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/testdb"
)

// reauthTenant is store.DefaultTenantID (internal/store/api_keys.go), the
// tenant every fresh test database carries.
const reauthTenant = "00000000-0000-0000-0000-0000000d3fa0"

// reauthProdAuth are the door's production defaults
// (internal/config/config.go gateway.auth_*: failure limit 30, window 60 s,
// probe rate 50, DB concurrency 4) — the in-flight sweep an hour (the gates
// that are about another trigger keep it out; TestModelKeySurvivesLongStream
// shortens it).
var reauthProdAuth = oai.AuthSettings{FailureLimit: 30, FailureWindow: 60 * time.Second, ProbeRate: 50, DBConcurrency: 4,
	InflightSweepInterval: time.Hour}

// reauthEnv is one database, one consumer with one live ctxm_ key, one
// paced upstream and the chat rig whose route is mounted behind the door,
// the door's registry worker running for the test.
type reauthEnv struct {
	pool  *pgxpool.Pool
	key   modelkey.Key
	keyID string
	up    *upstream
	ended *upstreamEnd
	rig   *chatRig
	door  *handler.ModelDoor
}

func newReauthEnv(t *testing.T, steps []sseStep) *reauthEnv {
	t.Helper()
	return newReauthEnvWith(t, steps, 4, reauthProdAuth.InflightSweepInterval)
}

// newReauthEnvWith is newReauthEnv with slots dispatcher slots for the
// upstream row (the leak probe runs 200 streams at once) and the in-flight
// sweep interval set before the registry's worker starts (the worker reads
// it per period: a later change applies from the next one).
func newReauthEnvWith(t *testing.T, steps []sseStep, slots int, sweep time.Duration) *reauthEnv {
	t.Helper()
	e := &reauthEnv{pool: testdb.SetupTestDB(t), ended: newUpstreamEnd()}
	e.seedKey(t)
	e.up = newUpstream(t, "")
	e.up.setRespond(sseRespond(steps, nil, e.ended))
	e.rig = newChatRig(t, []backends.Backend{e.up.row("spark", backends.TrustFull, 10)}, slots, 1, withStreamSettings,
		func(r *chatRig) {
			r.setSettings(func(s *oai.Settings) { s.Auth = reauthProdAuth; s.Auth.InflightSweepInterval = sweep })
		})
	// The dispatcher's principal comes from the door's consumer, as at boot
	// (cmd/ctxd/main.go: dispatch.SetPrincipalHook(handler.RequestPrincipal)).
	dispatch.SetPrincipalHook(handler.RequestPrincipal)
	e.door = handler.ModelAuth(handler.ModelAuthConfig{
		Pool:     e.pool,
		Settings: func() oai.Settings { return *e.rig.settings.Load() },
		Usage:    reauthNoTouch{},
		Reject:   reauthReject,
		Logger:   slog.New(slog.DiscardHandler),
	})
	e.run(t)
	return e
}

// run drives the registry's worker until the test ends (cmd/ctxd:
// modelGateway.run).
func (e *reauthEnv) run(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); e.door.Inflight().Run(ctx) }()
	t.Cleanup(func() { cancel(); <-done })
}

// seedKey creates an app consumer of the default tenant (all models, urgent
// lane, public prompt floor — the rig's urgentApp) that may connect without
// TLS from loopback, and one live key.
func (e *reauthEnv) seedKey(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	key, err := modelkey.Generate()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	var consumer string
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO context_model_consumers (tenant_id, label, all_models, prompt_sensitivity, fair_weight, admission_lane, allowed_cidrs, allow_plaintext)
		 VALUES ($1::uuid, 'w01-8-revoke', true, 'public', 1, 'urgent', ARRAY['127.0.0.0/8','::1/128']::cidr[], true) RETURNING id::text`,
		reauthTenant).Scan(&consumer); err != nil {
		t.Fatalf("insert consumer: %v", err)
	}
	if err := e.pool.QueryRow(ctx,
		`INSERT INTO context_model_keys (consumer_id, key_hash, key_hint, created_at) VALUES ($1::uuid, $2, $3, clock_timestamp()) RETURNING id::text`,
		consumer, key.Hash(), key.Hint()).Scan(&e.keyID); err != nil {
		t.Fatalf("insert key: %v", err)
	}
	e.key = key
}

// revoke sets revoked_at the way store.RevokeKey does — the column gate 1 of
// ctx_model_auth reads.
func (e *reauthEnv) revoke(t *testing.T) {
	t.Helper()
	tag, err := e.pool.Exec(context.Background(),
		`UPDATE context_model_keys SET revoked_at = GREATEST(now(), created_at) WHERE id = $1::uuid AND revoked_at IS NULL`, e.keyID)
	if err != nil || tag.RowsAffected() != 1 {
		t.Fatalf("revoke: %v (%d rows)", err, tag.RowsAffected())
	}
}

// bearer is the request header pair of the consumer's key.
func (e *reauthEnv) bearer() []string {
	return []string{"Authorization", "Bearer " + e.key.Plaintext()}
}

// serve sets the route's credential dependencies to the production ones and
// mounts the route the way cmd/ctxd's mountModelGroup does: access record →
// oai.Recover → model-key door → LoopGuard → PeerIngress → in-flight budget
// → POST /v1/chat/completions.
func (e *reauthEnv) serve(t *testing.T) *httptest.Server {
	t.Helper()
	r := e.rig
	r.deps.Consumer = handler.ConsumerAuthFromContext
	r.deps.KnowledgeAuth = func(ctx context.Context) bool { return handler.AuthResultFromContext(ctx) != nil }
	rt := chi.NewRouter()
	rt.Route("/v1", func(g chi.Router) {
		g.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				rec := &oai.AccessRecord{}
				next.ServeHTTP(w, req.WithContext(oai.WithAccessRecord(fedhop.WithIdentity(req.Context(), rigIdentity), rec)))
				select {
				case r.recs <- rec:
				default:
				}
			})
		})
		g.Use(oai.Recover, e.door.Handler,
			handler.LoopGuard(handler.LoopGuardConfig{MaxDepth: func() int { return 3 }, Envelope: handler.LoopGuardOpenAI}),
			federation.PeerIngress(federation.PeerIngressConfig{Consumer: handler.ConsumerAuthFromContext, Logger: slog.New(slog.DiscardHandler)}),
			oai.InflightBudget(&r.budget, func() oai.Settings { return *r.settings.Load() }))
		g.Method(http.MethodPost, "/chat/completions", oai.ChatCompletions(r.deps))
	})
	return serveHTTP(t, rt)
}

// reauthNoTouch is the door's usage stamp (store.ModelKeyUsage batches it).
type reauthNoTouch struct{}

func (reauthNoTouch) Touch(string) {}

// reauthReject renders the door's refusals as cmd/ctxd's
// renderModelAuthRejection does: the 401 through WriteInvalidAPIKey,
// everything else through WriteCode.
func reauthReject(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
	if rej.Status == http.StatusUnauthorized {
		oai.WriteInvalidAPIKey(w)
		return
	}
	oai.WriteCode(w, rej.Code, "refused by the door", rej.ShouldRetry)
}

// reauthPaced is an sglang stream of n content chunks, one every `every`
// after the first, then the usage chunk and [DONE].
func reauthPaced(n int, every time.Duration) []sseStep {
	out := []sseStep{{data: sglangChunk("t0", "")}}
	for i := 1; i < n; i++ {
		fin := ""
		if i == n-1 {
			fin = "stop"
		}
		out = append(out, sseStep{wait: every, data: sglangChunk(fmt.Sprintf(" t%d", i), fin)})
	}
	return append(out, sseStep{data: sglangUsage}, sseStep{data: "[DONE]"})
}

// reauthLive is a stream read event by event while the test acts on it.
type reauthLive struct {
	start  time.Time
	status int
	events chan sseEvent
	done   chan streamAnswer
}

// openReauthLive opens a stream and reads it in the background.
func openReauthLive(t *testing.T, srv *httptest.Server, body string, hdr ...string) *reauthLive {
	t.Helper()
	ls := &reauthLive{start: time.Now(), events: make(chan sseEvent, 4096), done: make(chan streamAnswer, 1)}
	resp, err := openStream(context.Background(), srv, body, hdr...)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	ls.status = resp.StatusCode
	go func() {
		defer func() { _ = resp.Body.Close() }()
		var raw strings.Builder
		evs, rerr := readEvents(io.TeeReader(resp.Body, &raw), ls.events)
		ls.done <- streamAnswer{start: ls.start, status: resp.StatusCode, header: resp.Header, raw: raw.String(), events: evs, readErr: rerr, end: time.Now()}
	}()
	return ls
}

// firstData waits for the first data event (the stream has committed).
func (ls *reauthLive) firstData(t *testing.T, within time.Duration) sseEvent {
	t.Helper()
	deadline := time.After(within)
	for {
		select {
		case ev := <-ls.events:
			if ev.data != "" {
				return ev
			}
		case <-deadline:
			t.Fatalf("no data event within %v", within)
		}
	}
}

// answer waits for the end of the stream.
func (ls *reauthLive) answer(t *testing.T, within time.Duration) streamAnswer {
	t.Helper()
	select {
	case a := <-ls.done:
		return a
	case <-time.After(within):
		t.Fatalf("the stream did not end within %v", within)
		return streamAnswer{}
	}
}

// reauthLastData is the time of the answer's last data event.
func reauthLastData(a streamAnswer) time.Time {
	var at time.Time
	for _, ev := range a.events {
		if ev.data != "" {
			at = ev.at
		}
	}
	return at
}

// revokeBound is R6's bound: a revoked stream ends within 5 s (expected
// ≪ 1 s).
const revokeBound = 5 * time.Second

// TestRevokeEndsRunningStream is masterplan gate R6 on the real stream
// (design/05 V-01-e and reports/bau/W01-5.md name it): a committed POST
// /v1/chat/completions stream whose key is revoked — (local) the manage
// action's write and the registry's CancelKey right after it, (psql) an
// UPDATE of revoked_at that the registry's ctx_model_auth_live finds (the
// sweep at its 1-s floor; the NOTIFY path to the same check is pinned in
// internal/handler and cmd/ctxd) — ends within 5 s: the error frame invalid_api_key as the
// last data event, no [DONE] (W02-08's frame from context.Cause ==
// auth.ErrCredentialRevoked), the upstream request aborted, the dispatcher
// lease free, the reservation ended exactly once (its L1 slot with it), no
// health fault of the row; the key's next request is a 401 at the door.
// Red against the base (dd308cee + step 0, production re-auth period 60 s):
// 49 frames in the 5 s after the revocation, the end 60.002 s after it (the
// wave report, probe zz_w018).
func TestRevokeEndsRunningStream(t *testing.T) {
	steps := reauthPaced(300, 100*time.Millisecond) // 30 s of frames
	for _, c := range []struct {
		name  string
		sweep bool
		act   func(t *testing.T, e *reauthEnv) int
	}{
		{"local: revoke + CancelKey", false, func(t *testing.T, e *reauthEnv) int {
			e.revoke(t)
			return e.door.Inflight().CancelKey(e.keyID)
		}},
		{"psql: UPDATE revoked_at (sweep)", true, func(t *testing.T, e *reauthEnv) int {
			e.revoke(t)
			return -1
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			sweep := reauthProdAuth.InflightSweepInterval
			if c.sweep {
				sweep = time.Second
			}
			e := newReauthEnvWith(t, steps, 4, sweep)
			srv := e.serve(t)
			ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
			first := ls.firstData(t, 5*time.Second)
			revokedAt := time.Now()
			n := c.act(t, e)
			a := ls.answer(t, revokeBound+time.Second)
			t.Logf("first data +%v, revoked +%v; answer %d, %d events, read error %v", first.at.Sub(ls.start).Round(time.Millisecond),
				revokedAt.Sub(ls.start).Round(time.Millisecond), a.status, len(a.events), a.readErr)
			code, last := a.errorFrame(t)
			if a.status != http.StatusOK || code != oai.CodeInvalidAPIKey || !last || a.dones() != 0 {
				t.Fatalf("answer %d, error frame %q (last %v), [DONE] %d — want the error frame invalid_api_key last, no [DONE]:\n%s", a.status, code, last, a.dones(), a.raw)
			}
			frameAfter := reauthLastData(a).Sub(revokedAt)
			leaseAfter := time.Duration(-1)
			if waitHeld(e.rig.disp, e.up.srv.URL, 0, 2*time.Second) {
				leaseAfter = time.Since(revokedAt)
			}
			upAborted := e.ended.wait(2*time.Second) && e.ended.canceled
			t.Logf("revocation → error frame %v, → lease free %v (bound %v); upstream aborted %v; CancelKey counted %d",
				frameAfter.Round(time.Millisecond), leaseAfter.Round(time.Millisecond), revokeBound, upAborted, n)
			if frameAfter > revokeBound {
				t.Errorf("error frame %v after the revocation, want ≤ %v", frameAfter, revokeBound)
			}
			if leaseAfter < 0 || leaseAfter > revokeBound {
				t.Errorf("lease free %v after the revocation (-1 = still held), want ≤ %v", leaseAfter, revokeBound)
			}
			if !upAborted {
				t.Error("the upstream request ran on after the error frame")
			}
			if n >= 0 && n != 1 {
				t.Errorf("CancelKey counted %d running requests, want 1", n)
			}
			l := e.rig.limiter
			if !waitFor(time.Second, func() bool { return l.settles.Load()+l.keeps.Load()+l.aborts.Load()+l.rejects.Load() == 1 }) {
				t.Errorf("reservation ends settle/keep/abort/reject %d/%d/%d/%d — want exactly one (the slot freed once)",
					l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load())
			}
			t.Logf("reservation ends settle/keep/abort/reject %d/%d/%d/%d", l.settles.Load(), l.keeps.Load(), l.aborts.Load(), l.rejects.Load())
			if f := consecutiveFails(e.rig.pool, "spark"); f != 0 {
				t.Errorf("consecutive_fails %d, want 0 (a revocation is no fault of the row)", f)
			}
			if s := e.door.Stats(); !waitFor(time.Second, func() bool { return e.door.Stats().Inflight == 0 }) || s.InflightCancelled != 1 {
				t.Errorf("registry after the stream: %d running, %d ended by it — want 0 and 1", e.door.Stats().Inflight, s.InflightCancelled)
			}
			if b := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...); b.status != http.StatusUnauthorized {
				t.Errorf("next request with the revoked key: %d, want 401 at the door", b.status)
			}
		})
	}
}

// TestRevokedKeyEndsRunningNonStream: a non-stream request whose key is
// revoked while the upstream works ends at once with 401 invalid_api_key
// and WWW-Authenticate (nothing was written yet), the upstream aborted, the
// lease free.
func TestRevokedKeyEndsRunningNonStream(t *testing.T) {
	e := newReauthEnv(t, nil)
	b := newBlocker()
	t.Cleanup(b.open)
	e.up.setRespond(b.respond(answerSGLang))
	srv := e.serve(t)
	done := make(chan streamAnswer, 1)
	go func() { done <- postStream(context.Background(), t, srv, chatBody("chat"), e.bearer()...) }()
	if !waitFor(5*time.Second, func() bool { return e.door.Stats().Inflight == 1 && e.up.hits.Load() >= 1 }) {
		t.Fatalf("the request did not reach the upstream (registry %d, upstream hits %d)", e.door.Stats().Inflight, e.up.hits.Load())
	}
	at := time.Now()
	e.revoke(t)
	n := e.door.Inflight().CancelKey(e.keyID)
	var a streamAnswer
	select {
	case a = <-done:
	case <-time.After(revokeBound + time.Second):
		t.Fatal("the non-stream request still runs after the revocation")
	}
	took := a.end.Sub(at)
	t.Logf("revocation → answer %d after %v (CancelKey %d): %s", a.status, took.Round(time.Millisecond), n, strings.TrimSpace(a.raw))
	if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") == "" || !strings.Contains(a.raw, `"code":"invalid_api_key"`) {
		t.Fatalf("answer %d %v %s, want 401 invalid_api_key with WWW-Authenticate", a.status, a.header, a.raw)
	}
	if took > revokeBound {
		t.Errorf("answered %v after the revocation, want ≤ %v", took, revokeBound)
	}
	if !waitHeld(e.rig.disp, e.up.srv.URL, 0, 2*time.Second) {
		t.Error("the lease is still held")
	}
}

// TestModelKeySurvivesLongStream: a stream on a live ctxm_ key that runs
// through several in-flight sweeps (1 s, the floor; the stream ~3.5 s)
// runs to its [DONE] — each sweep's ctx_model_auth_live finds the key alive
// and ends nothing. (W02-08's G10 ran the same probe against the periodic
// re-auth; that re-auth is gone with W01-8.)
func TestModelKeySurvivesLongStream(t *testing.T) {
	e := newReauthEnvWith(t, reauthPaced(35, 100*time.Millisecond), 4, time.Second)
	srv := e.serve(t)
	s0 := e.door.Stats().InflightSweeps
	a := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...)
	s := e.door.Stats()
	t.Logf("answer %d after %v, %d events; sweeps during the stream %d, ended by the registry %d", a.status, a.end.Sub(a.start).Round(time.Millisecond),
		len(a.events), s.InflightSweeps-s0, s.InflightCancelled)
	if code, _ := a.errorFrame(t); code != "" || a.status != http.StatusOK || a.readErr != nil || a.dones() != 1 {
		t.Fatalf("answer %d, error frame %q, [DONE] %d, read error %v — want a full stream", a.status, code, a.dones(), a.readErr)
	}
	if s.InflightSweeps-s0 < 2 || s.InflightCancelled != 0 || s.InflightSweepErrors != 0 {
		t.Errorf("sweeps %d (errors %d), ended %d — want ≥ 2 sweeps over the live key, none ending it", s.InflightSweeps-s0, s.InflightSweepErrors, s.InflightCancelled)
	}
}

// TestRevokeLeakProbeOverTheStream is the leak probe over the real stream
// (watcher, registry and relay together; the probe-router form with 10⁴
// requests is internal/handler TestInflightLeakProbe): 200 streams through
// the door on one key, each ending at random — read to [DONE], the client
// gone after 0–80 ms, or the key's running requests ended in the registry
// (CancelKey, no database write: the key stays valid for the next stream)
// after 0–80 ms ⇒ the registry empty, every lease released, the in-flight
// budget back to 0, every limiter reservation ended once, and the goroutine
// count where it was once every connection is closed.
func TestRevokeLeakProbeOverTheStream(t *testing.T) {
	const n = 200
	e := newReauthEnvWith(t, nil, 41, reauthProdAuth.InflightSweepInterval)
	e.up.setRespond(func(w http.ResponseWriter, r *http.Request) {
		steps := []sseStep{{data: sglangChunk("hello", "")}, {wait: time.Duration(rand.IntN(20)) * time.Millisecond, data: sglangChunk(" from", "")},
			{wait: time.Duration(rand.IntN(20)) * time.Millisecond, data: sglangChunk(" the engine", "stop")}, {data: sglangUsage}, {data: "[DONE]"}}
		sseRespond(steps, nil, nil)(w, r)
	})
	// A request ended by the registry takes its hash out of the known-good
	// set, so the key's next request needs a probe token; the probe bucket
	// is not this gate's subject.
	e.rig.setSettings(func(s *oai.Settings) { s.Auth.ProbeRate = 1 << 20 })
	srv := e.serve(t)
	baseTotal := runtime.NumGoroutine()
	var outcomes [3]atomic.Int64
	var statusMu sync.Mutex
	statuses := map[int]int{}
	var wg sync.WaitGroup
	for range n {
		wg.Go(func() {
			o := rand.IntN(3)
			outcomes[o].Add(1)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if o > 0 {
				d := time.Duration(rand.IntN(80)) * time.Millisecond
				act := cancel
				if o == 2 {
					act = func() { e.door.Inflight().CancelKey(e.keyID) }
				}
				tm := time.AfterFunc(d, act)
				defer tm.Stop()
			}
			resp, err := openStream(ctx, srv, streamBody("chat"), e.bearer()...)
			if err != nil {
				return
			}
			statusMu.Lock()
			statuses[resp.StatusCode]++
			statusMu.Unlock()
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		})
	}
	wg.Wait()
	srv.Client().CloseIdleConnections()
	if !waitFor(2*time.Second, func() bool { return e.door.Stats().Inflight == 0 }) {
		t.Errorf("registry holds %d requests after the streams, want 0", e.door.Stats().Inflight)
	}
	if !waitHeld(e.rig.disp, e.up.srv.URL, 0, 2*time.Second) {
		held, _ := heldOn(e.rig.disp, e.up.srv.URL)
		t.Errorf("held %d after the streams, want 0", held)
	}
	if !waitFor(2*time.Second, func() bool { return e.rig.budget.InUse() == 0 }) {
		t.Errorf("in-flight budget %d after the streams, want 0", e.rig.budget.InUse())
	}
	l := e.rig.limiter
	if !waitFor(2*time.Second, func() bool {
		return l.settles.Load()+l.keeps.Load()+l.aborts.Load()+l.rejects.Load() == l.admits.Load()
	}) {
		t.Errorf("reservations: %d admitted, %d settled + %d kept + %d aborted + %d rejected", l.admits.Load(), l.settles.Load(), l.keeps.Load(),
			l.aborts.Load(), l.rejects.Load())
	}
	if !waitFor(3*time.Second, func() bool {
		srv.CloseClientConnections()
		e.up.srv.CloseClientConnections()
		runtime.GC()
		return runtime.NumGoroutine() <= baseTotal
	}) {
		t.Errorf("goroutines %d after the streams and closed connections, %d before", runtime.NumGoroutine(), baseTotal)
	}
	s := e.door.Stats()
	t.Logf("200 streams (to [DONE] %d, client gone %d, registry-ended %d), statuses %v: registry %d running, %d ended by it; %d admitted, %d settled, %d kept, %d aborted, %d rejected; goroutines %d → %d",
		outcomes[0].Load(), outcomes[1].Load(), outcomes[2].Load(), statuses, s.Inflight, s.InflightCancelled, l.admits.Load(), l.settles.Load(),
		l.keeps.Load(), l.aborts.Load(), l.rejects.Load(), baseTotal, runtime.NumGoroutine())
	if s.InflightCancelled == 0 {
		t.Error("no stream was ended by the registry — the probe did not exercise it")
	}
}
