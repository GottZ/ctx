package handler

import (
	"context"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/oai"
	"github.com/GottZ/ctx/internal/store"
)

// model-alias-preview (wave W05-04, design/05 §4.3, masterplan §3.1): the
// dry run of the /v1 chain build for one (alias name, consumer) pair, and
// the pool's primary model of a role for the catalog's "suggest from pool"
// (design/05 §3.3). Server-admin only (tierServerAdmin, B-8: the answer names
// backends). The chain comes from oai.Deps.PreviewChain — the steps the
// inference routes run, in their order — over the context a /v1 request of
// the consumer without federation headers would reach the handler with
// (previewContext). No wire, no reservation, no quota, no llmlog, ledger or
// access-log row.
//
// Request (data, unknown fields 400):
//
//	{"alias": "<name>", "consumer_id": "<uuid>"}   chain mode
//	{"role": "chat"|"embed"|"rerank"}               suggestion mode
//
// exactly one mode. Answers:
//
//	chain mode:      {"success":true, "consumer_id":…, <oai.ChainPreview>}
//	suggestion mode: {"success":true, "role":…, "primary_model":"…"|""}
//	400 {"success":false,"error":…}         neither or both modes, missing
//	                                          consumer_id, unknown role/field
//	200 {"success":false,"error":"consumer not found"}   unknown consumer
//	403 {"success":false,"error":"admin key required"}   not server-admin
//	503 {"success":false,"error":…,"code":"service_unavailable"}  no /v1
//	                                          dependencies wired

// ChainPreviewer is the chain dry run cmd/ctxd binds over the /v1 group's
// oai.Deps (W05-04): (oai.Deps).PreviewChain. ctx is already the consumer's
// headerless /v1 context (previewContext).
type ChainPreviewer func(ctx context.Context, ca *auth.ConsumerAuth, alias string) (oai.ChainPreview, error)

// RolePrimaryModel is the suggestion source of the role mode:
// (*backends.Pool).PrimaryModel ("" = the pool offers nothing for the role).
type RolePrimaryModel func(role string) string

// SetChainPreview wires model-alias-preview (cmd/ctxd, next to the /v1
// group's dependencies). Without it the action answers 503.
func (h *ManageHandler) SetChainPreview(chain ChainPreviewer, primary RolePrimaryModel) {
	h.chainPreview, h.rolePrimary = chain, primary
}

// previewNotImplemented is the RC-D S1 stub answer.
const previewNotImplemented = "model-alias-preview: not implemented"

// handleModelAliasPreview answers model-alias-preview.
//
// RC-D S1: contract stub — 503 service_unavailable, nothing read.
func (h *ManageHandler) handleModelAliasPreview(w http.ResponseWriter, _ *http.Request, _ *auth.AuthResult, _ manageRequest) {
	writeJSONCode(w, http.StatusServiceUnavailable, "service_unavailable", map[string]any{
		"success": false,
		"error":   previewNotImplemented,
	})
}

// previewConsumerAuth is the ConsumerAuth a request of consumer c would
// carry behind the door (the ctx_model_auth columns, migration 161): valid,
// with its tenant, scope, kind, all_models, prompt_sensitivity, fair_weight,
// admission_lane and peer binding — no key id (a preview names no key).
//
// RC-D S1: contract stub — nil (no consumer: the preview refuses).
func previewConsumerAuth(c store.ModelConsumer) *auth.ConsumerAuth {
	_ = c
	return nil
}

// previewContext is the context a /v1 request of ca without federation
// headers reaches the handler with: the consumer in the door's context key,
// the admin's knowledge identity shadowed (RequestTenantScope resolves the
// consumer's tenant_scope only without an AuthResult — with both it stays on
// the base snapshot, and the tenant overlay of the preview would differ from
// the request's), and the PeerIngress policy (federation.HeaderlessContext).
//
// RC-D S1: contract stub — ctx unchanged.
func previewContext(ctx context.Context, ca *auth.ConsumerAuth) context.Context {
	_ = ca
	return ctx
}
