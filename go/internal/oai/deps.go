package oai

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
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
