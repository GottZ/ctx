package oai

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/llm"
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
	// (r.Context()), never one the route derived with a deadline: only when
	// it ended — the client is gone — nothing is written and "" returned.
	// Every other context error of an acquire (the route's deadline, a
	// released lease's run context) is a 503 with a body.
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
