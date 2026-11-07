package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/federation"
	"github.com/GottZ/ctx/internal/fedhop"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/keyquota"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/llmusage"
	"github.com/GottZ/ctx/internal/modelalias"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
)

// gatewaySettingsGetter is the settings source of the OpenAI-compatible
// gateway (design/02 §3.3): internal/oai may not import config, so cmd/ctxd
// hands it this getter. Every call takes one snapshot of the live store, so
// the gateway reads each hot gateway.* key per request and a settings write
// applies from the next request on. The keys are global-only — one gateway
// per process — which is why the tenant-less Snapshot is the right read.
// Its consumers are the model groups' gate, door, access log, body cap and
// budget (newModelGateway).
func gatewaySettingsGetter(cfgStore *config.Store) func() oai.Settings {
	return func() oai.Settings {
		return cfgStore.Snapshot().GatewaySettings() //nolint:forbidigo // MT 06 BLIND: gateway.* is global-only (one gateway, one budget per process); the gate runs before any credential, so no tenant exists to resolve.
	}
}

// whoamiModelAPIGetter is the source of the whoami capability model_api
// (design/05 §4.6, W05-01): gateway.enabled, gateway.tenant_self_service and
// the app-key lifetime, rotation grace and expiry warning in whole seconds —
// the data the SPA derives viewModelApi/mintModelKeys from. One snapshot per
// call, so a settings write shows on the next whoami. The peer lifetime is not
// carried (the peer page, W05-14, owns it).
func whoamiModelAPIGetter(cfgStore *config.Store) func() handler.WhoamiModelAPI {
	return func() handler.WhoamiModelAPI {
		c := cfgStore.Snapshot() //nolint:forbidigo // MT 06 BLIND: gateway.enabled and the gateway.* consumer policy are global-only switches over a host-global good (design/01 §4.6) — the same _global read the manage handlers enforce with.
		g := c.Gateway.Consumers
		return handler.WhoamiModelAPI{
			Enabled:               c.Gateway.Enabled,
			SelfService:           g.TenantSelfService,
			KeyMaxLifetimeSeconds: int64(g.KeyMaxLifetimeApp / time.Second),
			RotationGraceSeconds:  int64(g.KeyRotationGrace / time.Second),
			ExpiryWarnSeconds:     int64(g.KeyExpiryWarn / time.Second),
		}
	}
}

// federationMaxDepthGetter is the federation.max_depth source of the
// LoopGuards (F-W8, design/04 §4.6 step 3): one snapshot per call, so the
// hot key applies from the next request on. Global-only like gateway.*: the
// guard runs before any tenant is resolved on /v1, and the depth of a chain
// is a property of the federation topology, not of a tenant.
func federationMaxDepthGetter(cfgStore *config.Store) func() int {
	return func() int {
		return cfgStore.Snapshot().Federation.MaxDepth //nolint:forbidigo // MT 06 BLIND: federation.max_depth is global-only (one federation topology per process); the guard runs before any tenant is resolved.
	}
}

// gatewayIngressAssert is the boot half of "no /v1 without its ingress
// protection" (F-W8, design/04 §4.6, §7.2 no. 1): gateway.enabled
// effectively true — from CTX_GATEWAY_ENABLED or a settings override, the
// snapshot does not care which — while the router's federation ingress is
// not registered refuses the boot; since F-W9 registered means the LoopGuard
// and the PeerIngress (fedhop.Ingress). The settings write of the key is the
// other half (handler/settings.go, 409); a psql write is caught here at the
// next start.
//
// The remedy names the layer that set the value: the settings API is not
// reachable while the boot refuses, and an env value does not beat a
// settings row (Fremd-Review FR-2).
func gatewayIngressAssert(c *config.Config, in *fedhop.Ingress) error {
	if !c.Gateway.Enabled || in.Registered() {
		return nil
	}
	src := c.Source("gateway.enabled")
	remedy := "unset CTX_GATEWAY_ENABLED or set it to false"
	if src == "settings" {
		remedy = "remove the settings row (psql: DELETE FROM context_settings WHERE key = 'gateway.enabled'), or — with CTX_GATEWAY_ENABLED unset or false — start once with " +
			settings.EnvDisable + "=1 and DELETE /api/settings/gateway.enabled"
	}
	return fmt.Errorf("gateway.enabled is true (source: %s) but no model group with its federation ingress (LoopGuard and PeerIngress) is mounted — the model API cannot be enabled before its federation ingress is mounted; %s, or run a build that mounts /v1",
		src, remedy)
}

// modelGateway is the process-wide part of the model groups (design/02 §4.2,
// masterplan K23): ONE access log, ONE model-key door, ONE in-flight budget,
// ONE usage batcher and ONE per-consumer meter (W06-10, design/06 §4.2
// "Single-Process-Annahme"), shared by /v1 and (W02-10) /v2 — so two groups
// never hold twice the door's database share, twice the budget, two
// rejection windows or two buckets per consumer. newRouter builds it and
// mounts the groups from it; main drives its background loops (run,
// startModelGateway).
type modelGateway struct {
	settings func() oai.Settings
	access   *handler.GatewayAccessLog
	door     *handler.ModelDoor
	usage    *store.ModelKeyUsage
	budget   oai.Budget
	meter    *keyquota.Meter
	// seedRead is the meter's boot seed read (llmusage.SeedConsumers over
	// the pool): run retries it while the meter is not seeded.
	seedRead meterSeedRead
}

// meterSeedRead reads what the consumers used in the UTC day and month of
// now (llmusage.SeedConsumers).
type meterSeedRead func(ctx context.Context, now time.Time) ([]llmusage.ConsumerUsage, error)

// meterSeedTimeout bounds one boot seed read. Hardcoded on purpose, like the
// quota accountant's refresh timeout: it bounds one statement of the process
// about itself, not anybody's policy.
const meterSeedTimeout = 10 * time.Second

// seedMeterOnce runs one seed read and, when it succeeds, opens meter with
// it. It reports whether the meter is seeded afterwards.
func seedMeterOnce(ctx context.Context, meter *keyquota.Meter, read meterSeedRead) bool {
	if meter.Seeded() {
		return true
	}
	rctx, cancel := context.WithTimeout(ctx, meterSeedTimeout)
	defer cancel()
	now := time.Now()
	rows, err := read(rctx, now)
	if err != nil {
		slog.Warn("gateway: meter boot seed failed, /v1 answers 429 warming until a retry succeeds",
			"error", err, "retry", keyquota.WarmingRetryAfter)
		return false
	}
	seed := meterSeedRows(rows)
	if err := meter.Seed(now, seed); err != nil && !errors.Is(err, keyquota.ErrSeeded) {
		slog.Error("gateway: meter boot seed refused", "error", err)
		return false
	}
	slog.Info("gateway: meter seeded from the usage rollup", "consumer_rows", len(seed))
	return true
}

// seedMeter retries the boot seed every retry until it succeeded or ctx
// ended (design/06 §4.2: fail-closed, Retry mit Backoff 5 s —
// keyquota.WarmingRetryAfter in production).
func seedMeter(ctx context.Context, meter *keyquota.Meter, read meterSeedRead, retry time.Duration) {
	t := time.NewTicker(retry)
	defer t.Stop()
	for !seedMeterOnce(ctx, meter, read) {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// meterSeedRows maps the rollup's rows onto the meter's: the pipeline names
// the op (llmlog.GatewayOp); a row of any other pipeline is dropped.
func meterSeedRows(rows []llmusage.ConsumerUsage) []keyquota.SeedRow {
	out := make([]keyquota.SeedRow, 0, len(rows))
	for _, r := range rows {
		op, ok := keyquota.ParseOp(llmlog.GatewayOp(r.Pipeline))
		if !ok {
			continue
		}
		out = append(out, keyquota.SeedRow{ConsumerID: r.ConsumerID, TenantID: r.TenantID, Op: op,
			DayTokens: r.DayTokens, DayRequests: r.DayCalls, MonthTokens: r.MonthTokens})
	}
	return out
}

// newModelGateway wires the gateway over pool and the live config store.
// The door's trusted proxy is CTX_TRUSTED_PROXY as read at boot.
func newModelGateway(pool *pgxpool.Pool, cfgStore *config.Store) *modelGateway {
	get := gatewaySettingsGetter(cfgStore)
	usage := store.NewModelKeyUsage(pool)
	// The meter opens with the boot seed of its day and month budgets from
	// the rollup (W06-13): one read right here, before the server serves,
	// so a healthy boot never answers warming; a failed read is retried by
	// run (seedMeter) and /v1 answers 429 warming until it succeeds.
	meter := keyquota.New(time.Now)
	read := func(ctx context.Context, now time.Time) ([]llmusage.ConsumerUsage, error) {
		return llmusage.SeedConsumers(ctx, pool, now)
	}
	seedMeterOnce(context.Background(), meter, read)
	return &modelGateway{
		meter:    meter,
		seedRead: read,
		settings: get,
		access:   handler.NewGatewayAccessLog(handler.GatewayAccessLogConfig{Settings: get}),
		door: handler.ModelAuth(handler.ModelAuthConfig{
			Pool: pool, Settings: get, Usage: usage, Reject: renderModelAuthRejection,
			TrustedProxy: strings.TrimSpace(os.Getenv(handler.EnvTrustedProxy)),
		}),
		usage: usage,
	}
}

// group is the modelGroupDeps of one prefix over the shared parts: the gate
// before the door (K4), and as inner stages the strict body cap after the
// door (no cap oracle for an anonymous caller, design/02 §4.2 stage 3) and
// the in-flight budget (stage 5). routes are the group's endpoints.
func (g *modelGateway) group(ingress *fedhop.Ingress, maxDepth func() int, routes []modelRoute) modelGroupDeps {
	return modelGroupDeps{
		access: g.access, gate: oai.EnabledGate(g.settings), door: g.door, ingress: ingress, maxDepth: maxDepth,
		inner: []func(http.Handler) http.Handler{
			handler.MaxBodySizeStrictWith(func() int64 { return g.settings().MaxRequestBytes }, oai.WriteRequestTooLarge),
			oai.InflightBudget(&g.budget, g.settings),
		},
		routes: routes,
	}
}

// run drives the gateway's three background loops until ctx ends — the
// access log's rejection window, the batched last_used_at flush and the
// in-flight registry's worker (W01-8: the liveness checks a NOTIFY or a
// listener reconnect asks for, and the periodic sweep) — and returns once
// the first two have made their final flush (the usage flush is bounded by
// store.ModelKeyUsageFinalFlushTimeout) and the worker has stopped.
func (g *modelGateway) run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Go(func() { g.access.Run(ctx) })
	wg.Go(func() { g.usage.Run(ctx) })
	wg.Go(func() { g.door.Inflight().Run(ctx) })
	if g.seedRead != nil {
		wg.Go(func() { seedMeter(ctx, g.meter, g.seedRead, keyquota.WarmingRetryAfter) })
	}
	wg.Wait()
}

// startModelGateway runs g's loops on a context of their own and returns the
// shutdown step that ends them: not the root context, which ends at SIGTERM
// while the HTTP server still drains — the stamps and refusals of the
// drained requests would miss the final flush (W01-3 F-7). The step cancels,
// waits for the final flushes and gives up after gatewayStopTimeout (a WARN;
// the loops then finish on their own, the budget stays bounded).
func startModelGateway(g *modelGateway) func() {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		g.run(ctx)
		close(done)
	}()
	return func() {
		cancel()
		begin := time.Now()
		t := time.NewTimer(gatewayStopTimeout)
		defer t.Stop()
		select {
		case <-done:
			slog.Info("gateway: usage stamps and rejection counts flushed at shutdown", "elapsed", time.Since(begin).Round(time.Millisecond))
		case <-t.C:
			slog.Warn("gateway: final flush incomplete at shutdown, pending last_used_at stamps may be lost", "timeout", gatewayStopTimeout)
		}
	}
}

// gatewayDeps are the request-time dependencies of the gateway handlers
// (oai.Deps): the door's consumer from the context, the knowledge-credential
// probe, the process's alias registry, and the two grant reads over pool —
// the store primitives of W01-9, adapted here because internal/oai may not
// import internal/store (depguard oai-isolation). The inference routes
// (W02-07) add the gateway settings, the backend pool, the tenant quota
// gate (K17), the federation half of the chain build (F-W9: internal/oai
// cannot import internal/federation, which imports it), the tenant overlay
// of RequiredSensitivity (K30), the llmlog writer, the dispatcher's wait
// budget and Retry-After steps, the planner (A03-W8), the lifecycle context
// (D7) and the limiter over the process's meter (W06-10, meterLimiter). A
// key that stops authenticating ends its running requests through the
// door's in-flight registry (W01-8, K22), not through a route dependency. A nil
// meter leaves the limiter nil, and the chat route then answers 503
// service_unavailable (L11: never served without it).
func gatewayDeps(lifecycle context.Context, pool *pgxpool.Pool, cfgStore *config.Store, aliases *modelalias.Registry,
	backendPool *backends.Pool, quota *backends.QuotaAccountant, dispatcher *dispatch.Dispatcher, meter *keyquota.Meter) oai.Deps {
	settings := gatewaySettingsGetter(cfgStore)
	d := oai.Deps{
		Consumer:      handler.ConsumerAuthFromContext,
		KnowledgeAuth: func(ctx context.Context) bool { return handler.AuthResultFromContext(ctx) != nil },
		Aliases:       aliases,
		GrantIDs: func(ctx context.Context, consumerID string) ([]string, error) {
			return store.ConsumerGrantIDs(ctx, pool, consumerID)
		},
		HasGrant: func(ctx context.Context, consumerID, aliasID string) (bool, error) {
			return store.ConsumerGrantsAlias(ctx, pool, consumerID, aliasID)
		},
		Settings:   settings,
		Pool:       backendPool,
		Federation: chainFederation(backendPool),
		TenantSensitivity: func(ctx context.Context) backends.Sensitivity {
			return cfgStore.SnapshotForRequest(ctx).Pool.DefaultQuerySensitivity
		},
		Record: func(row llmlog.GatewayRow) { llmlog.RecordGateway(pool, row) },
		MaxWait: func() time.Duration {
			return cfgStore.Snapshot().Dispatch.ExternalMaxWait //nolint:forbidigo // MT 06 BLIND: dispatch.external_max_wait is global-only (one dispatcher per process); the write deadline at entry covers the wait budget of every consumer alike.
		},
		Lifecycle: lifecycle,
	}
	if quota != nil {
		d.Quota = quota
	}
	if dispatcher != nil {
		d.RetryAfterSteps = dispatcher.ExternalRetryAfterSteps
		d.Planner = handler.NewV1Admission(handler.V1AdmissionConfig{Dispatch: dispatcher, Settings: settings})
	}
	if meter != nil {
		d.Limiter = meterLimiter{meter: meter, settings: settings}
	}
	return d
}

// meterLimiter adapts the per-consumer meter to the Limiter hook of the
// /v1 routes (W06-10, lead decision 2: internal/keyquota stays a leaf, the
// adapter lives here like chainFederation). Per request it reads the meter
// limits from one settings snapshot (hot keys), resolves the lane with the
// planner's own rule (handler.ResolveLane over the consumer, the inbound
// federation state and service_tier — lead decision O-10-3 (a): one lane
// logic, the meter stays before Plan) and maps the meter's decision onto the
// oai refusal vocabulary.
type meterLimiter struct {
	meter    *keyquota.Meter
	settings func() oai.Settings
}

func (l meterLimiter) Admit(ctx context.Context, ca *auth.ConsumerAuth, _ modelalias.Alias, est oai.Estimate) (oai.LimitReservation, *oai.LimitRefusal) {
	s := l.settings().Meter
	op, _ := keyquota.ParseOp(est.Op) // an unknown op stays "" and is refused as invalid
	var lim oai.OpLimits
	switch op {
	case keyquota.OpChat:
		lim = s.Chat
	case keyquota.OpEmbed:
		lim = s.Embed
	case keyquota.OpRerank:
		lim = s.Rerank
	}
	lane := keyquota.LaneUrgent
	if handler.ResolveLane(ca, fedhop.InboundFrom(ctx), est.ServiceTier) != handler.LaneUrgent {
		lane = keyquota.LaneBulk
	}
	var consumer, tenant string // a nil consumer stays empty and is refused as invalid
	if ca != nil {
		consumer, tenant = ca.ConsumerID, ca.TenantID
	}
	sub := keyquota.Subject{ConsumerID: consumer, TenantID: tenant, Op: op, Lane: lane}
	// The day and month budgets (W06-13): until the limits rows of W06-15
	// the registry defaults for every consumer, the envelopes the factors ×
	// them (design/06 §4.3).
	own := keyquota.Budget{DailyTokens: lim.DailyTokens, MonthlyTokens: s.MonthlyTokens, DailyRequests: s.DailyRequests}
	res, d := l.meter.Admit(sub,
		keyquota.Limits{RPM: lim.RPM, TPM: lim.TPM, CompletionReserve: s.TPMReserve, XUReserve: s.XUReserveRatio, Concurrency: s.Concurrency,
			Consumer: own, Tenant: own.Scale(s.TenantEnvelopeFactor), Instance: own.Scale(s.InstanceEnvelopeFactor)},
		keyquota.Estimate{PromptRunes: est.PromptRunes, MaxCompletion: est.MaxTokens, ClientMax: est.ClientMaxTokens, InputRunes: est.InputRunes})
	switch {
	case d.Allowed:
		return meterReservation{res}, nil
	case d.Reason.Quota():
		// A day or month budget (W06-13): 429 insufficient_quota, not
		// retryable, Retry-After until the window resets (design/06 §4.4).
		return nil, &oai.LimitRefusal{Code: oai.CodeInsufficientQuota, Message: d.Message, RetryAfter: d.RetryAfter, ShouldRetry: false}
	case d.Reason == keyquota.ReasonInvalid:
		slog.Error("gateway: the meter refused an invalid subject (wiring defect)", "op", est.Op,
			"consumer", sub.ConsumerID != "", "tenant", sub.TenantID != "", "rpm", lim.RPM, "tpm", lim.TPM, "concurrency", s.Concurrency)
		return nil, &oai.LimitRefusal{Code: oai.CodeInternalError, Message: d.Message, ShouldRetry: false}
	case d.Reason == keyquota.ReasonConcurrency:
		// L1 (W06-11): 429 concurrency_limit_exceeded, Retry-After 1 s, the
		// honest lower bound (design/06 §4.4).
		return nil, &oai.LimitRefusal{Code: oai.CodeConcurrencyLimitExceeded, Message: d.Message, RetryAfter: d.RetryAfter, ShouldRetry: d.Retryable}
	}
	// rate_requests, rate_tokens and warming: 429 rate_limit_exceeded with
	// the exact Retry-After (design/06 §4.4).
	return nil, &oai.LimitRefusal{Code: oai.CodeRateLimitExceeded, Message: d.Message, RetryAfter: d.RetryAfter, ShouldRetry: d.Retryable}
}

// meterReservation is a meter reservation as the route holds it: the usage
// of the wire arrives as dispatch.Usage. Keep, Abort, Reject and Headers
// are the meter's.
type meterReservation struct{ *keyquota.Reservation }

func (r meterReservation) Settle(u dispatch.Usage) {
	r.Reservation.Settle(keyquota.Usage{PromptTokens: u.PromptTokens, CompletionTokens: u.CompletionTokens})
}

// chainFederation binds the federation half of the chain build (F-W9
// handover, lead decision W02-07 no. 1): the floor is the PeerIngress's
// policy, the scan runs over the body, and the filter gets the request's
// inbound hop state, the policy with the EFFECTIVE requirement (F2-7 — the
// empty-chain error then names what the chain was built for) and one
// learned-instance snapshot per request.
func chainFederation(backendPool *backends.Pool) oai.Federation {
	return oai.Federation{
		Floor: func(ctx context.Context) backends.Sensitivity { return federation.PolicyFrom(ctx).Required },
		Scan:  federation.ScanRaise,
		Filter: func(ctx context.Context, chain []backends.Backend, required backends.Sensitivity) ([]backends.Backend, error) {
			pol := federation.PolicyFrom(ctx)
			pol.Required = required
			return federation.FilterChain(chain, fedhop.InboundFrom(ctx), pol, backendPool.LearnedInstances())
		},
	}
}

// v1Routes are the endpoints of the /v1 group (design/02 §4.2 route
// table); the inference waves (W02-07ff.) add theirs here. The model route
// is a wildcard: alias names may contain "/" (§3.1). No route is a
// WithScheduler mount: a /v1 request never raises the herald (design/02
// §4.11.4, B11).
func v1Routes(d oai.Deps) []modelRoute {
	return []modelRoute{
		{http.MethodGet, "/models", oai.ListModels(d)},
		{http.MethodGet, "/models/*", oai.GetModel(d)},
		{http.MethodPost, "/chat/completions", oai.ChatCompletions(d)},
		{http.MethodPost, "/embeddings", oai.Embeddings(d)},
		{http.MethodPost, "/rerank", oai.Rerank(d, oai.RerankJina)},
	}
}

// v2Routes are the endpoints of the /v2 group (W02-10, design/02 §4.2 route
// table): the Cohere-v2 form of rerank only — the same handler as /v1/rerank
// in the other dialect (return_documents defaults to false). Everything else
// under /v2 is the group's catch-all, behind the same gate and door as /v1.
func v2Routes(d oai.Deps) []modelRoute {
	return []modelRoute{
		{http.MethodPost, "/rerank", oai.Rerank(d, oai.RerankCohere)},
	}
}

// doorMessages are the texts of the door's refusals other than the 401 —
// fixed per code, nothing of the request in them.
var doorMessages = map[string]string{
	oai.CodeRateLimitExceeded:  "Too many failed authentication attempts; retry later.",
	oai.CodeProxyMisconfigured: "The server cannot determine the client address; the operator must configure the trusted proxy.",
	oai.CodeAuthOverloaded:     "Authentication is temporarily overloaded; retry shortly.",
	oai.CodeAuthUnavailable:    "Authentication is temporarily unavailable; retry shortly.",
	oai.CodeInternalError:      "The server could not complete the request.",
}

// renderModelAuthRejection is the door's renderer (ModelAuthConfig.Reject,
// the W01-6 contract): the 401 through oai.WriteInvalidAPIKey — its
// WWW-Authenticate: Bearer realm="ctx" replaces the door's bare Bearer (lead
// decision W02-05; design/02 §4.10.2) — every other refusal through
// oai.WriteCode with the door's x-should-retry. The Retry-After the door set
// stays.
func renderModelAuthRejection(w http.ResponseWriter, _ *http.Request, rej handler.ModelAuthRejection) {
	if rej.Status == http.StatusUnauthorized {
		oai.WriteInvalidAPIKey(w)
		return
	}
	msg, ok := doorMessages[rej.Code]
	if !ok {
		msg = doorMessages[oai.CodeInternalError]
	}
	oai.WriteCode(w, rej.Code, msg, rej.ShouldRetry)
}
