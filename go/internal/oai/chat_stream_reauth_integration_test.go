//go:build integration

package oai_test

// G10 of W02-08 (design/02 §4.11.3 D6, masterplan K22) against a real
// database: a real ctxm_ model key behind the real model-key door
// (handler.ModelAuth, mounted as cmd/ctxd's model group mounts it), the
// production re-auth binding Deps.Reauth = handler.ReauthConsumer and the
// production consumer read Deps.Consumer = handler.ConsumerAuthFromContext.
// The rest of the route's dependencies come from the chat rig
// (chat_harness_test.go), the upstream and the SSE client from the stream
// harness (chat_stream_harness_test.go).
//
// The consumer and its key are inserted with the SQL of
// store.CreateConsumerWithKey (insertConsumerTx, insertModelKeyTx) and the
// revocation is the column ctx_model_auth reads (context_model_keys.
// revoked_at, migrations/161_model_consumers.sql gate 1): internal/oai may
// not import internal/store, tests included (depguard oai-isolation,
// .golangci.yml).
//
//	go test -tags=integration -p 1 -count=1 -run 'ModelKeySurvivesLongStream|RevokedKeyEndsStreamOnReauth|ReauthUnavailable' -v ./internal/oai/

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
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
// probe rate 50, DB concurrency 4).
var reauthProdAuth = oai.AuthSettings{FailureLimit: 30, FailureWindow: 60 * time.Second, ProbeRate: 50, DBConcurrency: 4}

type reauthFunc = func(context.Context, *auth.ConsumerAuth) (bool, error)

// reauthCall is one recorded re-authentication.
type reauthCall struct {
	n    int64
	at   time.Time
	took time.Duration
	ok   bool
	err  error
}

// reauthEnv is one database, one consumer with one live ctxm_ key, one
// paced upstream and the chat rig whose route is mounted behind the door.
type reauthEnv struct {
	pool  *pgxpool.Pool
	key   modelkey.Key
	keyID string
	up    *upstream
	ended *upstreamEnd
	rig   *chatRig
	door  *handler.ModelDoor

	mu    sync.Mutex
	calls []reauthCall
	n     atomic.Int64
}

func newReauthEnv(t *testing.T, steps []sseStep) *reauthEnv {
	t.Helper()
	e := &reauthEnv{pool: testdb.SetupTestDB(t), ended: newUpstreamEnd()}
	e.seedKey(t)
	e.up = newUpstream(t, "")
	e.up.setRespond(sseRespond(steps, nil, e.ended))
	e.rig = newChatRig(t, []backends.Backend{e.up.row("spark", backends.TrustFull, 10)}, 4, 1, withStreamSettings,
		func(r *chatRig) { r.setSettings(func(s *oai.Settings) { s.Auth = reauthProdAuth }) })
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
	return e
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
		 VALUES ($1::uuid, 'w02-08-reauth', true, 'public', 1, 'urgent', ARRAY['127.0.0.0/8','::1/128']::cidr[], true) RETURNING id::text`,
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

// setDBConcurrency sets gateway.auth_db_concurrency hot (the door reads it
// per check).
func (e *reauthEnv) setDBConcurrency(n int) {
	e.rig.setSettings(func(s *oai.Settings) { s.Auth.DBConcurrency = n })
}

// counted wraps f: every call is numbered and recorded.
func (e *reauthEnv) counted(f reauthFunc) reauthFunc {
	return func(ctx context.Context, ca *auth.ConsumerAuth) (bool, error) {
		n := e.n.Add(1)
		t0 := time.Now()
		ok, err := f(ctx, ca)
		e.mu.Lock()
		e.calls = append(e.calls, reauthCall{n: n, at: t0, took: time.Since(t0), ok: ok, err: err})
		e.mu.Unlock()
		return ok, err
	}
}

func (e *reauthEnv) recorded() []reauthCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]reauthCall(nil), e.calls...)
}

// tally counts the recorded calls by result.
func (e *reauthEnv) tally() (valid, invalid, failed int) {
	for _, c := range e.recorded() {
		switch {
		case c.err != nil:
			failed++
		case c.ok:
			valid++
		default:
			invalid++
		}
	}
	return valid, invalid, failed
}

// bearer is the request header pair of the consumer's key.
func (e *reauthEnv) bearer() []string { return []string{"Authorization", "Bearer " + e.key.Plaintext()} }

func (e *reauthEnv) logCalls(t *testing.T, since time.Time) {
	t.Helper()
	for _, c := range e.recorded() {
		t.Logf("re-auth #%d at +%v took %v: ok=%v err=%v", c.n, c.at.Sub(since).Round(time.Millisecond), c.took.Round(time.Millisecond), c.ok, c.err)
	}
}

// knowledgeReauth is the counterproof binding: the re-check through the
// knowledge credential path — handler.Auth, whose resolveCredential refuses
// every ctxm_ key (internal/handler/middleware.go) — with the key's
// plaintext as Bearer. A fresh request context: handler.Auth refuses one
// that already carries the door's ConsumerAuth (B6).
func (e *reauthEnv) knowledgeReauth() reauthFunc {
	knowledge := handler.Auth(e.pool)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	plain := e.key.Plaintext()
	return func(context.Context, *auth.ConsumerAuth) (bool, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/whoami", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		knowledge.ServeHTTP(rec, req)
		switch rec.Code {
		case http.StatusNoContent:
			return true, nil
		case http.StatusUnauthorized:
			return false, nil
		}
		return false, fmt.Errorf("knowledge auth answered %d", rec.Code)
	}
}

// serve sets the route's credential dependencies to the production ones
// with reauth as Deps.Reauth and mounts the route the way cmd/ctxd's
// mountModelGroup does: access record → oai.Recover → model-key door →
// LoopGuard → PeerIngress → in-flight budget → POST /v1/chat/completions.
func (e *reauthEnv) serve(t *testing.T, reauth reauthFunc) *httptest.Server {
	t.Helper()
	r := e.rig
	r.deps.Consumer = handler.ConsumerAuthFromContext
	r.deps.KnowledgeAuth = func(ctx context.Context) bool { return handler.AuthResultFromContext(ctx) != nil }
	r.deps.Reauth = reauth
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

// TestModelKeySurvivesLongStream is G10 (design/02 §4.11.3 D6, K22): a
// stream on a ctxm_ key that runs for nine re-auth periods (re-auth every
// 100 ms, the upstream delivers a frame every 100 ms for 800 ms) runs to
// its [DONE] with the production binding — the door's ReauthConsumer
// re-checks the key at least three times and each time finds it valid.
// Red (counterproof subtest): the same probe with the re-check through the
// knowledge credential path (handler.Auth's resolveCredential, which
// refuses every ctxm_ key) ends the stream at the first re-check with the
// error frame invalid_api_key.
func TestModelKeySurvivesLongStream(t *testing.T) {
	const tick = 100 * time.Millisecond
	defer oai.SetReauthEvery(tick)()
	steps := reauthPaced(9, tick)

	t.Run("door re-auth (production binding)", func(t *testing.T) {
		e := newReauthEnv(t, steps)
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		a := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...)
		e.logCalls(t, a.start)
		t.Logf("answer %d after %v, %d events, read error %v:\n%s", a.status, a.end.Sub(a.start).Round(time.Millisecond), len(a.events), a.readErr, a.raw)
		if code, _ := a.errorFrame(t); code != "" || a.status != http.StatusOK || a.readErr != nil {
			t.Fatalf("answer %d, error frame %q, read error %v — want a full stream", a.status, code, a.readErr)
		}
		if n := len(a.events); a.dones() != 1 || n == 0 || a.events[n-1].data != "[DONE]" {
			t.Errorf("[DONE] %d times in %d events — want exactly once and last", a.dones(), n)
		}
		if got := len(a.chunks()); got < 9 {
			t.Errorf("%d chunks, want the 9 content chunks", got)
		}
		valid, invalid, failed := e.tally()
		t.Logf("re-auth through the door: %d valid, %d invalid, %d failed over %v", valid, invalid, failed, a.end.Sub(a.start).Round(time.Millisecond))
		if valid < 3 || invalid != 0 || failed != 0 {
			t.Errorf("re-auth: %d valid, %d invalid, %d failed — want ≥ 3 valid, none else", valid, invalid, failed)
		}
		if !waitHeld(e.rig.disp, e.up.srv.URL, 0, time.Second) {
			t.Error("the lease is still held a second after the stream ended")
		}
	})

	t.Run("counterproof: re-auth over the knowledge credential path", func(t *testing.T) {
		e := newReauthEnv(t, steps)
		// The first re-check waits for the commit (the client's first data
		// event), so a loaded host cannot turn the error frame into the
		// pre-commit 401.
		committed := make(chan struct{})
		knowledge := e.knowledgeReauth()
		srv := e.serve(t, e.counted(func(ctx context.Context, ca *auth.ConsumerAuth) (bool, error) {
			select {
			case <-committed:
			case <-ctx.Done():
				return false, ctx.Err()
			}
			return knowledge(ctx, ca)
		}))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		close(committed)
		a := ls.answer(t, 10*time.Second)
		e.logCalls(t, a.start)
		t.Logf("answer %d after %v, %d events, read error %v:\n%s", a.status, a.end.Sub(a.start).Round(time.Millisecond), len(a.events), a.readErr, a.raw)
		code, last := a.errorFrame(t)
		if a.status != http.StatusOK || code != oai.CodeInvalidAPIKey || !last || a.dones() != 0 {
			t.Fatalf("answer %d, error frame %q (last %v), [DONE] %d — the knowledge path must end the stream with invalid_api_key", a.status, code, last, a.dones())
		}
		valid, invalid, failed := e.tally()
		if valid != 0 || invalid != 1 || failed != 0 {
			t.Errorf("re-auth: %d valid, %d invalid, %d failed — want the stream ended at the first re-check", valid, invalid, failed)
		}
		if n := len(a.chunks()); n >= 9 {
			t.Errorf("%d data events — the stream ran to the end before the error frame", n)
		}
	})
}

// TestRevokedKeyEndsStreamOnReauth is G10 (D6, K22): the key is revoked
// with SQL while the committed stream runs (after its first data event);
// the next re-check through the door ends it — the error frame
// invalid_api_key as the last data event, no [DONE], within one re-auth
// period plus 1 s of the revocation; the upstream request is aborted and
// the dispatcher lease is free. The key's next request is a 401 at the door.
// Red (subtest): without a re-check (Deps.Reauth always true) the same
// stream runs to its [DONE] although the key is revoked.
func TestRevokedKeyEndsStreamOnReauth(t *testing.T) {
	const tick = 200 * time.Millisecond
	defer oai.SetReauthEvery(tick)()
	steps := reauthPaced(30, 100*time.Millisecond) // 2.9 s of frames

	t.Run("door re-auth (production binding)", func(t *testing.T) {
		e := newReauthEnv(t, steps)
		srv := e.serve(t, e.counted(handler.ReauthConsumer))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		first := ls.firstData(t, 5*time.Second)
		revokedAt := time.Now()
		e.revoke(t)
		a := ls.answer(t, 10*time.Second)
		e.logCalls(t, ls.start)
		t.Logf("first data +%v, revoked +%v; answer %d, %d events, read error %v:\n%s", first.at.Sub(ls.start).Round(time.Millisecond),
			revokedAt.Sub(ls.start).Round(time.Millisecond), a.status, len(a.events), a.readErr, a.raw)
		code, last := a.errorFrame(t)
		if a.status != http.StatusOK || code != oai.CodeInvalidAPIKey || !last || a.dones() != 0 {
			t.Fatalf("answer %d, error frame %q (last %v), [DONE] %d — want the error frame invalid_api_key last, no [DONE]", a.status, code, last, a.dones())
		}
		frameAfter := reauthLastData(a).Sub(revokedAt)
		leaseAfter := time.Duration(-1)
		if waitHeld(e.rig.disp, e.up.srv.URL, 0, 2*time.Second) {
			leaseAfter = time.Since(revokedAt)
		}
		upAborted := e.ended.wait(2*time.Second) && e.ended.canceled
		t.Logf("revocation → error frame %v, → lease free %v (bound: re-auth period %v + 1 s); upstream aborted %v at +%v",
			frameAfter.Round(time.Millisecond), leaseAfter.Round(time.Millisecond), tick, upAborted, e.ended.at.Sub(revokedAt).Round(time.Millisecond))
		if frameAfter > tick+time.Second {
			t.Errorf("error frame %v after the revocation, want ≤ %v", frameAfter, tick+time.Second)
		}
		if leaseAfter < 0 || leaseAfter > tick+time.Second {
			t.Errorf("lease free %v after the revocation (-1 = still held), want ≤ %v", leaseAfter, tick+time.Second)
		}
		if !upAborted {
			t.Error("the upstream request ran on after the error frame")
		}
		if _, invalid, _ := e.tally(); invalid != 1 {
			t.Errorf("%d re-checks answered invalid, want 1", invalid)
		}
		if b := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...); b.status != http.StatusUnauthorized {
			t.Errorf("next request with the revoked key: %d, want 401 at the door", b.status)
		}
	})

	t.Run("red: no re-check (Reauth always true)", func(t *testing.T) {
		e := newReauthEnv(t, steps)
		srv := e.serve(t, e.counted(func(context.Context, *auth.ConsumerAuth) (bool, error) { return true, nil }))
		ls := openReauthLive(t, srv, streamBody("chat"), e.bearer()...)
		ls.firstData(t, 5*time.Second)
		revokedAt := time.Now()
		e.revoke(t)
		a := ls.answer(t, 10*time.Second)
		code, _ := a.errorFrame(t)
		t.Logf("without re-check: answer %d, error frame %q, [DONE] %d, last data %v after the revocation, %d re-checks (always true)",
			a.status, code, a.dones(), reauthLastData(a).Sub(revokedAt).Round(time.Millisecond), e.n.Load())
		if code != "" || a.dones() != 1 {
			t.Fatalf("without re-check: error frame %q, [DONE] %d — want the stream to run to its end", code, a.dones())
		}
		if reauthLastData(a).Sub(revokedAt) <= tick+time.Second {
			t.Errorf("the stream ended %v after the revocation — the red must outlive the green bound", reauthLastData(a).Sub(revokedAt))
		}
	})
}

// starve makes the calls numbered in fail run with no free slot in the
// door's DB semaphore (gateway.auth_db_concurrency 0, hot): the real
// ReauthConsumer waits out the door's deadline and reports
// ErrModelAuthUnavailable, as under saturation.
func (e *reauthEnv) starve(fail ...int64) reauthFunc {
	return func(ctx context.Context, ca *auth.ConsumerAuth) (bool, error) {
		n := e.n.Load() // counted numbers the call before it reaches here
		for _, f := range fail {
			if f == n {
				e.setDBConcurrency(0)
				defer e.setDBConcurrency(reauthProdAuth.DBConcurrency)
			}
		}
		return handler.ReauthConsumer(ctx, ca)
	}
}

// TestReauthUnavailableEndsStreamAfterTwoFailures is lead decision O-08-2
// (b), N = 2, through the real door: one re-check that cannot run (no free
// DB-semaphore slot ⇒ ErrModelAuthUnavailable after the door's 1 s
// deadline) and the committed stream runs on to its [DONE]; two in a row
// and it ends with the error frame auth_unavailable, the lease free.
func TestReauthUnavailableEndsStreamAfterTwoFailures(t *testing.T) {
	const tick = 200 * time.Millisecond
	defer oai.SetReauthEvery(tick)()

	t.Run("one failure: the stream runs on", func(t *testing.T) {
		e := newReauthEnv(t, reauthPaced(30, 100*time.Millisecond))
		srv := e.serve(t, e.counted(e.starve(2)))
		a := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...)
		e.logCalls(t, a.start)
		code, _ := a.errorFrame(t)
		valid, _, failed := e.tally()
		t.Logf("answer %d after %v: error frame %q, [DONE] %d; re-auth %d valid, %d failed; door semaphore timeouts %d",
			a.status, a.end.Sub(a.start).Round(time.Millisecond), code, a.dones(), valid, failed, e.door.Stats().SemaphoreTimeouts)
		if code != "" || a.dones() != 1 {
			t.Fatalf("error frame %q, [DONE] %d — one failed re-check must not end a committed stream", code, a.dones())
		}
		if failed != 1 || valid < 2 || e.door.Stats().SemaphoreTimeouts != 1 {
			t.Errorf("re-auth %d failed, %d valid, %d semaphore timeouts — want 1 failure between successes", failed, valid, e.door.Stats().SemaphoreTimeouts)
		}
	})

	t.Run("two failures in a row: error frame auth_unavailable", func(t *testing.T) {
		e := newReauthEnv(t, reauthPaced(60, 100*time.Millisecond))
		srv := e.serve(t, e.counted(e.starve(2, 3)))
		a := postStream(context.Background(), t, srv, streamBody("chat"), e.bearer()...)
		e.logCalls(t, a.start)
		code, last := a.errorFrame(t)
		t.Logf("answer %d after %v: error frame %q (last %v), [DONE] %d:\n%s", a.status, a.end.Sub(a.start).Round(time.Millisecond), code, last, a.dones(), a.raw)
		if code != oai.CodeAuthUnavailable || !last || a.dones() != 0 {
			t.Fatalf("error frame %q (last %v), [DONE] %d — want auth_unavailable last after the second failure", code, last, a.dones())
		}
		if _, _, failed := e.tally(); failed != 2 {
			t.Errorf("%d failed re-checks, want 2", failed)
		}
		if !waitHeld(e.rig.disp, e.up.srv.URL, 0, time.Second) {
			t.Error("the lease is still held a second after the error frame")
		}
	})
}
