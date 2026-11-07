package oai

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/llmlog"
	"github.com/GottZ/ctx/internal/modelalias"
)

// Deps are the request-time dependencies of the gateway handlers (design/02
// §4.13), injected by cmd/ctxd: internal/oai imports neither
// internal/handler (the model-key door imports this package) nor
// internal/store (depguard oai-isolation), so the consumer, the
// knowledge-credential probe and the grant reads arrive as functions. This
// is the first form (W02-05: the model list and Resolve); every endpoint
// wave adds what it reads (the backend pool, the planner, the limiter).
//
// A missing field is a wiring defect: the handler answers 503
// internal_error with x-should-retry: false and logs an ERROR (design/02
// L11) — it never serves without it.
type Deps struct {
	// Consumer returns the request's model consumer, the ConsumerAuth the
	// door put into the context at stage 8 (= handler.ConsumerAuthFromContext).
	Consumer func(context.Context) *auth.ConsumerAuth
	// KnowledgeAuth reports whether the request carries a knowledge
	// AuthResult (= handler.AuthResultFromContext(ctx) != nil) — never under
	// /v1 (masterplan I-K0); every handler checks it again (§4.2 stage 6).
	KnowledgeAuth func(context.Context) bool
	// Aliases is the process's model-alias registry (W02-03,
	// events.Scheduler.ModelAliasRegistry): one snapshot per request.
	Aliases *modelalias.Registry
	// GrantIDs lists the alias ids granted to a consumer — one range of the
	// grants' primary key (store.ConsumerGrantIDs); read once per model list.
	GrantIDs func(ctx context.Context, consumerID string) ([]string, error)
	// HasGrant probes one grant — one primary-key probe
	// (store.ConsumerGrantsAlias); read once per resolved alias.
	HasGrant func(ctx context.Context, consumerID, aliasID string) (bool, error)

	// The inference routes (W02-07: POST /v1/chat/completions). None of
	// these is in missing(): the model list does not read them, and a
	// missing one must not take GET /v1/models down (A03-W8 "Vertragsform");
	// the inference handler checks them itself (inferenceMissing).

	// Settings is the gateway settings getter: one snapshot per request.
	Settings func() Settings
	// Pool is the backend pool: the chain (Pool.Chain), the health report
	// of every attempt (llm.PoolReporter) and the identity strings the
	// upstream-text sanitizer replaces (Pool.Snapshot).
	Pool *backends.Pool
	// Quota is the tenant gate of the chain (masterplan K17, design/02
	// §4.4.3 step 5): backends.QuotaAccountant.GateTenant.
	Quota TenantGate
	// Federation is the federation half of the chain build (F-W9).
	Federation Federation
	// TenantSensitivity is the tenant-overlay term of RequiredSensitivity:
	// pool.default_query_sensitivity of the request's tenant
	// (config.Store.SnapshotForRequest, which resolves the consumer's tenant
	// under /v1).
	TenantSensitivity func(context.Context) backends.Sensitivity
	// Record writes the request's ONE llmlog row (llmlog.RecordGateway over
	// the process pool, design/06 §4.6).
	Record func(llmlog.GatewayRow)
	// MaxWait is the dispatcher's external wait budget
	// (dispatch.external_max_wait): the write deadline at entry covers it
	// (design/02 §4.12).
	MaxWait func() time.Duration
	// RetryAfterSteps are the dispatcher's resolved Retry-After steps
	// ((*dispatch.Dispatcher).ExternalRetryAfterSteps): the 503 of a missing
	// limiter carries the largest one (lead decision O5 (b), the
	// not-offered contract of A03-W8) — no literal on the Retry-After path.
	RetryAfterSteps func() []time.Duration
	// Planner is the admission contract (A03-W8, handler.V1Admission). nil
	// ⇒ 503 no_eligible_backend before any acquire (design/02 §4.11.4, L11:
	// no fallback class).
	Planner AdmissionPlanner
	// Limiter is the per-consumer meter (design/06 §4.1 steps 7–9, W06-10).
	// nil ⇒ 503 service_unavailable (L11).
	Limiter Limiter
	// Lifecycle, when set, ends every running request at server shutdown
	// (design/02 §4.11.3 D7): the request context is bound to it.
	Lifecycle context.Context
}

// TenantGate is the tenant quota gate of a /v1 chain (masterplan K17):
// *backends.QuotaAccountant implements it. An empty tenant is an error
// (backends.ErrNoTenant), never an ungated chain.
type TenantGate interface {
	GateTenant(tenantID string, chain []backends.Backend) ([]backends.Backend, error)
}

// Federation is the federation half of the chain build (design/04 §4.5,
// §4.7; F-W9). Injected: internal/federation imports this package (its
// PeerIngress answers in the envelope), so it cannot be imported here.
// cmd/ctxd binds federation.PolicyFrom, federation.ScanRaise and
// federation.FilterChain.
type Federation struct {
	// Floor is federation.PolicyFrom(ctx).Required — the first term of
	// RequiredSensitivity.
	Floor func(context.Context) backends.Sensitivity
	// Scan is federation.ScanRaise over the request body.
	Scan func(required backends.Sensitivity, body []byte) backends.Sensitivity
	// Filter is federation.FilterChain with the request's inbound hop
	// state, its policy carrying the EFFECTIVE requirement (F-W9 review 2
	// F2-7), and one Pool.LearnedInstances snapshot.
	Filter func(ctx context.Context, chain []backends.Backend, required backends.Sensitivity) ([]backends.Backend, error)
}

// Limiter is the per-consumer meter hook of the model API (design/06 §4.1
// steps 7–9: seeded?, Admit L0, L1 concurrency; wave W06-10 implements it).
// It refuses at once, never waits (L5). The handler holds the reservation
// for the request and ends it exactly once: Settle after a wire call, Abort
// without one, Reject when the dispatcher refused after Admit.
type Limiter interface {
	Admit(ctx context.Context, ca *auth.ConsumerAuth, alias modelalias.Alias, est Estimate) (LimitReservation, *LimitRefusal)
}

// Estimate is what the limiter charges ahead (design/06 §4.2 "Reservierung
// je op"): the op, the effective max_tokens (design/06 K-06-11) and whether
// the client named it, the size of the prompt in bytes and in runes (the
// meter counts runes, lead decision M3 (a)), and the body's service_tier
// (the lane input, lead decision O-10-3 (a)).
type Estimate struct {
	Op        string
	MaxTokens int
	// ClientMaxTokens reports whether the client sent max_tokens or
	// max_completion_tokens; without it the meter reserves
	// gateway.tpm_reserve_default (capped by MaxTokens) for the completion.
	ClientMaxTokens bool
	PromptBytes     int
	// PromptRunes are the runes of the chat messages as they go upstream.
	PromptRunes int
	// InputRunes are Σ input runes of an embedding request, Σ (query +
	// document) runes of a rerank request (W02-09, W02-10).
	InputRunes  int
	ServiceTier string
}

// LimitReservation is one admitted request's share of the meter. It ends
// exactly once: Settle with the usage of a wire call, Keep after a wire call
// whose usage is unknown (an upstream error, an answer without usage, a
// panic or a gone client after the wire — never a full refund after wire
// contact), Abort without a wire call, Reject when the dispatcher refused.
type LimitReservation interface {
	Settle(usage dispatch.Usage)
	Keep()
	Abort()
	Reject(reason string, waited time.Duration)
	// Headers are the x-ratelimit-* headers of the admission (design/06
	// §4.4): a route writes them on its successful answer before its first
	// byte, never on a refusal (lead decision M2 (c)).
	Headers() http.Header
	// Held is the tokens the admission charged — what Keep books (the
	// meter's estimate, at most the bucket's capacity). A route that weighs
	// its own booking against the reservation reads it here, never rebuilds
	// it (one source, lead decision 2026-11-06).
	Held() int64
}

// LimitRefusal is the limiter's answer for a refused request: a code of the
// vocabulary (rate_limit_exceeded, concurrency_limit_exceeded,
// insufficient_quota, service_unavailable), its text, the Retry-After and
// whether a retry can help.
type LimitRefusal struct {
	Code        string
	Message     string
	RetryAfter  time.Duration
	ShouldRetry bool
}

// AdmissionPlanner is the admission contract of every /v1 inference handler
// (design/02 §4.13, design/03 §4.9; wave A03-W8). handler.V1Admission
// implements it; W02-07 wires it into Deps for the first inference route.
// The interface lives here because the handlers do and internal/oai cannot
// import internal/handler. The handler's order (design/03 §4.9 "Ablaufvertrag"):
// Plan, then llm.WalkChain over Plan.Chain with Plan.Acquirer and
// WalkRules{AllowedClasses: AllowedClasses()}; an error of Plan, an acquire
// error of the walk (llm.IsAdmissionError) or the walk's class assert
// (llm.ErrWalkClass) goes to WriteAdmissionError(r.Context(), w, err)
// before anything else is written — no byte and no status before the First acquire returned. An
// attempt error (an upstream status, a wire timeout) is the route's own
// mapping, never the admission writer's.
type AdmissionPlanner interface {
	// AllowedClasses is the ONE source of WalkRules.AllowedClasses of every
	// /v1 route (the external classes the planner binds); no route writes
	// the class list itself.
	AllowedClasses() []dispatch.Class
	// Plan resolves the request's lane, filters its chain to the rows that
	// offer the external class and returns the request's acquirer. ctx is
	// the request's: its deadline becomes Request.ServeBy, its inbound
	// federation state (Ctx-Class, Ctx-Path, Ctx-Max-Wait-Ms) is read
	// through fedhop.InboundFrom. A lane or chain that is not offered is an
	// error for WriteAdmissionError, before any acquire.
	Plan(ctx context.Context, ca *auth.ConsumerAuth, in AdmissionInput) (Plan, error)
	// WriteAdmissionError answers an admission failure (an error of Plan,
	// an acquire error of the walk, the walk's class assert) and returns
	// the code it wrote (the refusal reason of the access log and the
	// ledger, W06-9/W06-12). ctx is the request's own context
	// (r.Context(), with or without a request deadline on it): only when it
	// was canceled — the client is gone — nothing is written and ""
	// returned. A deadline that ended it, and every other context error of
	// an acquire (the route's deadline, a released lease's run context), is
	// a 503 with a body.
	WriteAdmissionError(ctx context.Context, w http.ResponseWriter, err error) string
}

// AdmissionInput is what a route hands the planner: the request's chain
// (resolved and sensitivity-filtered, W02-07), the service_tier of the body,
// and the engine work of the request for the weight plan (design/03 §4.7).
type AdmissionInput struct {
	Chain       []backends.Backend
	ServiceTier string
	// Units is the request's engine parallelism: 1 for chat (n > 1 is RC-F,
	// E4), the inputs of an embedding request, the documents of a rerank
	// request; 0 reads as 1.
	Units int
	// SubBatch is the alias policy's per-lease cap (embedding.sub_batch,
	// rerank.sub_batch); 0 = none. A lease carries at most
	// min(SubBatch, ExternalWeightCap(origin)) units.
	SubBatch int
}

// Plan is the planner's answer for one request.
type Plan struct {
	// Acquirer is the request's acquirer: First blocks (the one wait point),
	// Next never blocks (failover), Continue serves the further sub-blocks
	// of the attempt's row; Blocks is the weight plan of that row.
	Acquirer RequestAcquirer
	// Chain is the input chain filtered to the rows whose origin offers the
	// external class, in order.
	Chain []backends.Backend
	// Lane is the resolved lane, "urgent" or "bulk".
	Lane string
	// Continuation reports that Acquirer.Continue is the continuation door
	// of the dispatcher (A03-W17, masterplan K21): a request whose weight
	// plan has more than one block may run them. false ⇒ k = 1 — a route
	// refuses a request that needs a second block with 400 unsupported_value
	// (design/02 §4.8, fail-closed). handler.V1Admission sets it.
	Continuation bool
}

// RequestAcquirer is the per-request acquirer of a Plan: llm.Acquirer plus
// the weight plan of the current attempt's row (W02-09/W02-10 split their
// inputs by it: block i carries Blocks()[i] units).
type RequestAcquirer interface {
	llm.Acquirer
	Blocks() []int
}

// missing names the unset fields ("" = complete).
func (d Deps) missing() string {
	var m []string
	if d.Consumer == nil {
		m = append(m, "Consumer")
	}
	if d.KnowledgeAuth == nil {
		m = append(m, "KnowledgeAuth")
	}
	if d.Aliases == nil {
		m = append(m, "Aliases")
	}
	if d.GrantIDs == nil {
		m = append(m, "GrantIDs")
	}
	if d.HasGrant == nil {
		m = append(m, "HasGrant")
	}
	return strings.Join(m, ", ")
}

// inferenceMissing names the unset fields an inference route needs beyond
// missing() ("" = complete). Planner and Limiter are not here: each has its
// own fail-closed answer (L11).
func (d Deps) inferenceMissing() string {
	var m []string
	for _, f := range []struct {
		name  string
		unset bool
	}{
		{"Settings", d.Settings == nil}, {"Pool", d.Pool == nil}, {"Quota", d.Quota == nil},
		{"Federation.Floor", d.Federation.Floor == nil}, {"Federation.Scan", d.Federation.Scan == nil},
		{"Federation.Filter", d.Federation.Filter == nil}, {"TenantSensitivity", d.TenantSensitivity == nil},
		{"Record", d.Record == nil}, {"MaxWait", d.MaxWait == nil}, {"RetryAfterSteps", d.RetryAfterSteps == nil},
	} {
		if f.unset {
			m = append(m, f.name)
		}
	}
	return strings.Join(m, ", ")
}

// admit is the invariant check every gateway handler runs before anything
// else (design/02 §4.2 stage 6, masterplan I-K0): the dependencies are
// wired, the request carries a valid consumer with a tenant, and no
// knowledge AuthResult. Anything else is a wiring defect behind the door:
// 503 internal_error, x-should-retry: false, ERROR — and false.
func (d Deps) admit(w http.ResponseWriter, r *http.Request) (*auth.ConsumerAuth, bool) {
	if m := d.missing(); m != "" {
		slog.Error("gateway: handler dependency missing (wiring defect)", "missing", m,
			"path", r.URL.Path, "request_id", w.Header().Get("X-Request-ID"))
		refuseInternal(w, r)
		return nil, false
	}
	ca := d.Consumer(r.Context())
	knowledge := d.KnowledgeAuth(r.Context())
	if ca == nil || !ca.IsValid || ca.TenantID == "" || knowledge {
		slog.Error("gateway: request invariant broken behind the model-key door (wiring defect)",
			"consumer", ca != nil, "valid", ca != nil && ca.IsValid, "tenant", ca != nil && ca.TenantID != "",
			"knowledge_auth", knowledge, "path", r.URL.Path, "request_id", w.Header().Get("X-Request-ID"))
		refuseInternal(w, r)
		return nil, false
	}
	return ca, true
}

// refuseInternal answers WriteInternal and names internal_error as the
// refusal reason of the access record (the group's rejection line counts by
// it).
func refuseInternal(w http.ResponseWriter, r *http.Request) {
	if rec := AccessRecordFrom(r.Context()); rec != nil {
		rec.Reason = CodeInternalError
	}
	WriteInternal(w)
}
