package handler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/oai"
)

// The door's own codes (design/01 K-02-1), codes of the gateway vocabulary
// since the mount (oai.Codes, W02-05; TestDoorCodesInVocabulary). All three
// are 503 service_unavailable_error with x-should-retry: false; the renderer
// (cmd/ctxd renderModelAuthRejection) writes them through oai.WriteCode.
const (
	// ModelAuthCodeProxyMisconfigured: X-Forwarded-For arrives while
	// CTX_TRUSTED_PROXY is unset — the client address cannot be known
	// (stage 1, B22, E-01-13).
	ModelAuthCodeProxyMisconfigured = oai.CodeProxyMisconfigured
	// ModelAuthCodeOverloaded: the probe bucket is empty (stage 6, B18).
	ModelAuthCodeOverloaded = oai.CodeAuthOverloaded
	// ModelAuthCodeUnavailable: semaphore deadline, exhausted pool or
	// database error (stage 7). Never a 500: an SDK retries a 500 twice
	// into the saturation, and a peer ctx stops its chain on it.
	ModelAuthCodeUnavailable = oai.CodeAuthUnavailable
)

// ErrModelAuthUnavailable wraps every failure of the door's database step
// (semaphore deadline, pool, query).
var ErrModelAuthUnavailable = errors.New("handler: model-key check unavailable")

// ModelAuthRejection is the door's decision on a refused request: status,
// code, Retry-After and x-should-retry, decided by ModelAuth. The door sets
// the headers (Retry-After, x-should-retry, WWW-Authenticate) before it
// calls the renderer; the renderer (ModelAuthConfig.Reject, owned by
// W02-02, K-02-1) writes the status line and the OpenAI error body for
// Code, keeps the headers already set, writes x-should-retry from
// ShouldRetry, and echoes nothing of the request.
type ModelAuthRejection struct {
	Status      int
	Code        string
	RetryAfter  int // seconds; 0 = no Retry-After header
	ShouldRetry bool
}

// The door's rejections. Every 401 is the one rejInvalid — missing,
// malformed, unknown, revoked, refused by a gate, or a knowledge
// credential: the answer does not say which.
var (
	rejInvalid     = ModelAuthRejection{Status: http.StatusUnauthorized, Code: oai.CodeInvalidAPIKey}
	rejLocked      = ModelAuthRejection{Status: http.StatusTooManyRequests, Code: oai.CodeRateLimitExceeded}
	rejProxy       = ModelAuthRejection{Status: http.StatusServiceUnavailable, Code: ModelAuthCodeProxyMisconfigured}
	rejOverloaded  = ModelAuthRejection{Status: http.StatusServiceUnavailable, Code: ModelAuthCodeOverloaded, RetryAfter: 1}
	rejUnavailable = ModelAuthRejection{Status: http.StatusServiceUnavailable, Code: ModelAuthCodeUnavailable}
	rejInternal    = ModelAuthRejection{Status: http.StatusServiceUnavailable, Code: oai.CodeInternalError}
)

// modelAuthLogEvery throttles the door's per-request warnings (database
// errors): a database outage under load must not turn into one log line per
// request. The counters carry the volume.
const modelAuthLogEvery = time.Second

// ModelKeyToucher records a successful use of a model key; the production
// implementation is store.ModelKeyUsage (batched last_used_at, W01-3).
type ModelKeyToucher interface{ Touch(keyID string) }

// ModelAuthConfig wires the door. Every field but Logger is required; a
// missing one is a wiring bug and panics in ModelAuth.
type ModelAuthConfig struct {
	// Pool runs ctx_model_auth; the door's share of it is
	// gateway.auth_db_concurrency.
	Pool *pgxpool.Pool
	// Settings is the gateway getter (cmd/ctxd gatewaySettingsGetter); the
	// door reads Settings().Auth once per request (hot).
	Settings func() oai.Settings
	// TrustedProxy is CTX_TRUSTED_PROXY (EnvTrustedProxy) as read at boot:
	// the one proxy hop whose X-Forwarded-For and X-Forwarded-Proto count.
	TrustedProxy string
	// Usage stamps last_used_at in batches (store.ModelKeyUsage).
	Usage ModelKeyToucher
	// Reject renders a refusal (see ModelAuthRejection).
	Reject func(w http.ResponseWriter, r *http.Request, rej ModelAuthRejection)
	// Logger defaults to slog.Default().
	Logger *slog.Logger
}

// ModelAuthStats are the door's counters (K-06-7); the status exposition is
// design/06's. SemaphoreWaits counts every acquire of the DB semaphore,
// SemaphoreWaitTotal sums their waits.
type ModelAuthStats struct {
	Accepted           uint64
	NoCredential       uint64 // stage 3: no Authorization: Bearer
	FormFailures       uint64 // stage 4: not a well-formed model key
	Locked             uint64 // stage 5: 429 of the failure lock
	ProbeRejected      uint64 // stage 6: 503 auth_overloaded
	Invalid            uint64 // stage 7: ctx_model_auth refused
	SemaphoreTimeouts  uint64 // stage 7: 503, deadline in the semaphore
	DBErrors           uint64 // stage 7: 503, pool or query
	ClientGone         uint64 // stage 7: the request context ended first (client gone) — no overload signal
	SemaphoreWaits     uint64
	SemaphoreWaitTotal time.Duration
	ProxyMisconfigured uint64 // stage 1
	WiringConflicts    uint64 // stage 0

	// The in-flight registry (W01-8): requests running now, the distinct
	// model keys behind them and their expiry timers (≤ keys), requests
	// ended because their key stopped authenticating, liveness statements
	// (ctx_model_auth_live) run and failed.
	Inflight            int
	InflightKeys        int
	InflightTimers      int
	InflightCancelled   uint64
	InflightSweeps      uint64
	InflightSweepErrors uint64
}

type modelAuthCounters struct {
	accepted, noCredential, formFailures, locked, probeRejected, invalid,
	semaphoreTimeouts, dbErrors, clientGone, semaphoreWaits, semaphoreWaitNanos,
	proxyMisconfigured, wiringConflicts atomic.Uint64
}

// ModelDoor is the /v1 and /v2 model-key door (design/01 §4.2). One per
// process: the failure lock, the known-good set, the probe bucket and the
// DB semaphore are process-wide, so both groups share one door.
type ModelDoor struct {
	pool         *pgxpool.Pool
	settings     func() oai.Settings
	trustedProxy string     // CTX_TRUSTED_PROXY, normalised ("" = none; a value that is no address is "" too, logged at construction)
	trustedAddr  netip.Addr // the same as an address (port and brackets stripped, IPv4-mapped unmapped, no zone)
	usage        ModelKeyToucher
	reject       func(http.ResponseWriter, *http.Request, ModelAuthRejection)
	log          *slog.Logger

	limiter  *failLimiter
	known    *knownGoodLRU
	probes   *probeBucket
	sem      *authSemaphore
	inflight *InflightRegistry

	proxyOnce sync.Once
	lastWarn  atomic.Int64 // unix nanos of the last throttled warning
	st        modelAuthCounters
}

// ModelAuth builds the model-key door; cmd/ctxd builds it once per process
// and mounts it in the model groups (mountModelGroup, W02-05).
func ModelAuth(cfg ModelAuthConfig) *ModelDoor {
	switch {
	case cfg.Pool == nil:
		panic("handler.ModelAuth: nil Pool")
	case cfg.Settings == nil:
		panic("handler.ModelAuth: nil Settings getter")
	case cfg.Usage == nil:
		panic("handler.ModelAuth: nil Usage")
	case cfg.Reject == nil:
		panic("handler.ModelAuth: nil Reject renderer")
	}
	log := cfg.Logger
	if log == nil {
		log = slog.Default()
	}
	trusted := strings.TrimSpace(cfg.TrustedProxy)
	trustedAddr := canonicalProxy(trusted)
	if trusted != "" && !trustedAddr.IsValid() {
		// Review 2 FR2-1: a value that names no address (a host name, a
		// CIDR, a URL) can never equal a peer, so the door would run in the
		// silent B22 state — the proxy taken for the client, every request's
		// address the proxy's. Treated as unset instead: a request that
		// arrives with X-Forwarded-For answers 503 proxy_misconfigured, loud.
		log.Error("model auth: "+EnvTrustedProxy+" is not an IP address — treated as unset: every model-key request that arrives "+
			"with X-Forwarded-For is refused with 503 proxy_misconfigured; set it to the proxy's plain address as ctxd sees it "+
			"(a port or brackets are accepted and stripped, a host name is not)", "value", trusted)
		trusted = ""
	}
	d := &ModelDoor{
		pool: cfg.Pool, settings: cfg.Settings, trustedProxy: trusted, trustedAddr: trustedAddr,
		usage: cfg.Usage, reject: cfg.Reject, log: log,
		limiter: newFailLimiter(modelAuthLimiterCap, time.Now),
		known:   newKnownGood(modelAuthKnownGoodCap),
		probes:  newProbeBucket(time.Now),
		sem:     newAuthSemaphore(),
	}
	pool := cfg.Pool
	d.inflight = newInflightRegistry(
		func(ctx context.Context, ids []string) ([]string, error) { return auth.DeadModelKeys(ctx, pool, ids) },
		func() time.Duration { return cfg.Settings().Auth.InflightSweepInterval },
		d.known.remove, time.Now, log)
	return d
}

// Inflight is the door's in-flight registry (W01-8): the manage actions end
// a revoked key's running requests through it, the NOTIFY listener feeds it
// (events.ModelAccessSink), and its Run is one of the gateway's background
// loops.
func (d *ModelDoor) Inflight() *InflightRegistry { return d.inflight }

// Stats returns a snapshot of the counters.
func (d *ModelDoor) Stats() ModelAuthStats {
	in := d.inflight.stats()
	return ModelAuthStats{
		Inflight: in.running, InflightKeys: in.keys, InflightTimers: in.timers,
		InflightCancelled: in.cancelled, InflightSweeps: in.sweeps, InflightSweepErrors: in.sweepErrors,
		Accepted: d.st.accepted.Load(), NoCredential: d.st.noCredential.Load(),
		FormFailures: d.st.formFailures.Load(), Locked: d.st.locked.Load(),
		ProbeRejected: d.st.probeRejected.Load(), Invalid: d.st.invalid.Load(),
		SemaphoreTimeouts: d.st.semaphoreTimeouts.Load(), DBErrors: d.st.dbErrors.Load(), ClientGone: d.st.clientGone.Load(),
		SemaphoreWaits: d.st.semaphoreWaits.Load(), SemaphoreWaitTotal: time.Duration(d.st.semaphoreWaitNanos.Load()),
		ProxyMisconfigured: d.st.proxyMisconfigured.Load(), WiringConflicts: d.st.wiringConflicts.Load(),
	}
}

// Handler is the door as middleware: stages 0-8 of design/01 §4.2, each
// fail-closed, stages 0-6 without the database.
func (d *ModelDoor) Handler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		// 0 — mutual exclusion: a knowledge AuthResult here means the doors
		// are stacked (B6). A-internal fault: 503, never 500 (K5, K29).
		if AuthResultFromContext(ctx) != nil {
			d.st.wiringConflicts.Add(1)
			d.log.Error("model auth: request already carries a knowledge AuthResult — ModelAuth stacked behind handler.Auth (wiring defect)",
				"path", r.URL.Path, "request_id", RequestIDFromContext(ctx))
			d.refuse(w, r, rejInternal)
			return
		}
		// 1 — proxy consistency (B22): behind a proxy without
		// CTX_TRUSTED_PROXY every client looks like the proxy; allowlists
		// would check the proxy's address and the lock would merge clients.
		if d.trustedProxy == "" && strings.TrimSpace(headerList(r, "X-Forwarded-For")) != "" {
			d.st.proxyMisconfigured.Add(1)
			d.proxyOnce.Do(func() {
				d.log.Error("model auth: X-Forwarded-For arrives but " + EnvTrustedProxy + " is unset (or no IP address) — the client address " +
					"cannot be determined, every model-key request is refused with 503 proxy_misconfigured; set " +
					EnvTrustedProxy + " to the proxy's address as ctxd sees it (docs/operations.md). Logged once per process.")
			})
			d.refuse(w, r, rejProxy)
			return
		}
		// 2 — origin: client address and transport.
		ip, secure := d.origin(r)
		// 3 — the credential: Authorization: Bearer only.
		raw, ok := bearerToken(r)
		if !ok {
			d.st.noCredential.Add(1)
			d.refuse(w, r, rejInvalid)
			return
		}
		// 4 — form and checksum; a knowledge key or a token fails here.
		key, err := modelkey.Parse(raw)
		if err != nil {
			d.st.formFailures.Add(1)
			d.refuse(w, r, rejInvalid)
			return
		}
		s := d.settings().Auth
		lk := limiterKey(ip, key.HashPrefix())
		// 5 — failure lock of this key from this network.
		if locked, left := d.limiter.blocked(lk, s.FailureLimit, s.FailureWindow); locked {
			d.st.locked.Add(1)
			rej := rejLocked
			rej.RetryAfter = int(math.Ceil(left.Seconds()))
			d.refuse(w, r, rej)
			return
		}
		hash := key.Hash()
		// 6 — probe bucket for hashes without a past success.
		if !d.known.contains(hash) && !d.probes.take(s.ProbeRate) {
			d.st.probeRejected.Add(1)
			d.refuse(w, r, rejOverloaded)
			return
		}
		// 7 — the database check under the semaphore. The registry's mark
		// before it: a revocation signal during the check is caught at
		// registration (stage 8).
		mark := d.inflight.touchMark()
		ca, err := d.check(ctx, s.DBConcurrency, func(c context.Context) (*auth.ConsumerAuth, error) {
			return auth.AuthenticateModelKey(c, d.pool, hash, ip, secure)
		})
		if err != nil {
			d.warn("model auth: check unavailable", "error", err, "request_id", RequestIDFromContext(ctx))
			d.refuse(w, r, rejUnavailable)
			return
		}
		if !ca.IsValid {
			d.st.invalid.Add(1)
			d.limiter.fail(lk, s.FailureWindow)
			d.known.remove(hash)
			d.refuse(w, r, rejInvalid)
			return
		}
		// 8 — accepted. The identity goes into the access record; the
		// group's access log writes the request's one line from it (W06-9:
		// no line of the door's own per request). The request runs on a
		// context the in-flight registry can end (W01-8): a key that stops
		// authenticating while the request runs cancels it with
		// auth.ErrCredentialRevoked. It leaves the registry when the
		// handler returns — a panic included.
		d.known.add(hash)
		d.usage.Touch(ca.ModelKeyID)
		d.st.accepted.Add(1)
		if rec := oai.AccessRecordFrom(ctx); rec != nil {
			rec.ConsumerID, rec.ModelKeyID, rec.TenantID = ca.ConsumerID, ca.ModelKeyID, ca.TenantID
		}
		ctx, cancel := context.WithCancelCause(context.WithValue(ctx, consumerAuthKey, ca))
		unregister := d.inflight.register(ca, hash, cancel, mark)
		defer func() {
			unregister()
			cancel(context.Canceled)
		}()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// origin is stage 2: the client address and whether the request arrived
// over TLS. Only behind the trusted proxy (the direct peer equals
// CTX_TRUSTED_PROXY) do X-Forwarded-For and X-Forwarded-Proto count, and
// then only their rightmost entry — the one the proxy wrote. Every header
// LINE counts, not just the first one Header.Get returns: a proxy that
// appends its own line after a client-sent one would otherwise hand the
// client's forgery to the door (Fremd-Review F1). From any other peer both
// headers are ignored (auth_session.go: "spoofable"). The logic is
// clientIP's (auth_sso.go) plus the multi-line rule and the transport.
func (d *ModelDoor) origin(r *http.Request) (netip.Addr, bool) {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	host := peer
	viaProxy := d.isTrustedProxy(peer)
	if viaProxy {
		if c := lastListValue(headerList(r, "X-Forwarded-For")); c != "" {
			host = c
		}
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		ip = netip.Addr{}
	}
	secure := r.TLS != nil ||
		viaProxy && strings.EqualFold(lastListValue(headerList(r, "X-Forwarded-Proto")), "https")
	return ip, secure
}

// isTrustedProxy compares the direct peer with CTX_TRUSTED_PROXY as
// addresses (Fremd-Review N6, review 2 FR2-1): 2001:DB8::10, [2001:db8::10]
// and [2001:db8::10]:443 are one host, and so are an IPv4 address and its
// IPv4-mapped spelling. Without a valid configured address nothing is
// trusted (ModelAuth logged the value and dropped it).
func (d *ModelDoor) isTrustedProxy(peer string) bool {
	return d.trustedAddr.IsValid() && canonicalAddr(peer) == d.trustedAddr
}

// canonicalAddr parses s as an address without zone, IPv4-mapped as IPv4;
// the zero Addr if s is none.
func canonicalAddr(s string) netip.Addr {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// canonicalProxy is canonicalAddr for the configured value: the spellings
// an operator writes for one host — plain, with a port (192.0.2.10:443,
// [2001:db8::10]:443) or bracketed ([2001:db8::10]) — all name the address;
// anything else is the zero Addr.
func canonicalProxy(s string) netip.Addr {
	if ap, err := netip.ParseAddrPort(s); err == nil {
		return ap.Addr().Unmap().WithZone("")
	}
	return canonicalAddr(strings.TrimSuffix(strings.TrimPrefix(s, "["), "]"))
}

// lastListValue is the rightmost non-empty entry of a comma list — the one
// the nearest (trusted) proxy wrote.
func lastListValue(v string) string {
	parts := strings.Split(v, ",")
	for i := len(parts) - 1; i >= 0; i-- {
		if p := strings.TrimSpace(parts[i]); p != "" {
			return p
		}
	}
	return ""
}

// headerList joins every line of a comma-list header in arrival order.
func headerList(r *http.Request, name string) string {
	return strings.Join(r.Header.Values(name), ",")
}

// bearerToken is stage 3: the token of "Authorization: Bearer <token>"
// (scheme case-insensitive, RFC 9110 §11.1). X-Context-Key, the session
// cookie and the query string are never read.
func bearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return "", false
	}
	token = strings.TrimSpace(token)
	return token, token != ""
}

// check runs one database check under the semaphore, all of it inside
// modelAuthDBDeadline: semaphore wait, pool acquire and query. Every failure
// wraps ErrModelAuthUnavailable.
func (d *ModelDoor) check(ctx context.Context, limit int, run func(context.Context) (*auth.ConsumerAuth, error)) (*auth.ConsumerAuth, error) {
	dctx, cancel := context.WithTimeout(ctx, modelAuthDBDeadline)
	defer cancel()
	t0 := time.Now()
	release, err := d.sem.acquire(dctx, limit)
	d.st.semaphoreWaits.Add(1)
	d.st.semaphoreWaitNanos.Add(uint64(time.Since(t0)))
	if err != nil {
		d.countUnavailable(ctx, &d.st.semaphoreTimeouts)
		return nil, fmt.Errorf("%w: semaphore: %w", ErrModelAuthUnavailable, err)
	}
	defer release()
	ca, err := run(dctx)
	if err != nil {
		d.countUnavailable(ctx, &d.st.dbErrors)
		return nil, fmt.Errorf("%w: %w", ErrModelAuthUnavailable, err)
	}
	return ca, nil
}

// countUnavailable files a failed database step: as a gone client when the
// caller's context ended first (Fremd-Review N4) — that is no overload and
// must not inflate the semaphore and database counters the overload alarm
// reads (K-06-7) — otherwise on the counter c.
func (d *ModelDoor) countUnavailable(ctx context.Context, c *atomic.Uint64) {
	if ctx.Err() != nil {
		d.st.clientGone.Add(1)
		return
	}
	c.Add(1)
}

// refuse sets the decision headers, names the code as the refusal reason
// of the access record (the group's throttled refusal line counts by it)
// and hands the body to the renderer.
func (d *ModelDoor) refuse(w http.ResponseWriter, r *http.Request, rej ModelAuthRejection) {
	if rec := oai.AccessRecordFrom(r.Context()); rec != nil {
		rec.Reason = rej.Code
	}
	h := w.Header()
	if rej.Status == http.StatusUnauthorized {
		// No resource_metadata: this is not the MCP resource (design/01 §4.2).
		h.Set("WWW-Authenticate", "Bearer")
	}
	if rej.RetryAfter > 0 {
		h.Set("Retry-After", strconv.Itoa(rej.RetryAfter))
	}
	h.Set("x-should-retry", strconv.FormatBool(rej.ShouldRetry))
	d.reject(w, r, rej)
}

// warn logs at most once per modelAuthLogEvery.
func (d *ModelDoor) warn(msg string, args ...any) {
	now := time.Now().UnixNano()
	last := d.lastWarn.Load()
	if now-last < int64(modelAuthLogEvery) || !d.lastWarn.CompareAndSwap(last, now) {
		return
	}
	d.log.Warn(msg+" (throttled to one line per second; the door's counters carry the volume)", args...)
}
