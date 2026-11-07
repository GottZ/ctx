package handler

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"slices"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/federation"
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
//	404 {"success":false,"error":"model consumer not found"}  unknown or
//	                                          malformed consumer id (the
//	                                          model-consumer-get answer)
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

// previewRoles are the roles of the suggestion mode (design/05 §3.3).
var previewRoles = map[string]bool{"chat": true, "embed": true, "rerank": true}

// modelAliasPreviewRequest is the data of model-alias-preview.
type modelAliasPreviewRequest struct {
	Alias      string `json:"alias"`
	ConsumerID string `json:"consumer_id"`
	Role       string `json:"role"`
}

// previewAnswer is the chain mode's answer: the envelope, the consumer and
// the preview's fields at the top level.
type previewAnswer struct {
	Success    bool   `json:"success"`
	ConsumerID string `json:"consumer_id"`
	oai.ChainPreview
}

// handleModelAliasPreview answers model-alias-preview. The tier column has
// already admitted only a server admin.
func (h *ManageHandler) handleModelAliasPreview(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var d modelAliasPreviewRequest
	if err := decodeConsumerPayload(req.Data, &d); err != nil {
		writeConsumerError(w, http.StatusBadRequest, "invalid data: "+err.Error())
		return
	}
	switch {
	case d.Role != "" && d.Alias == "" && d.ConsumerID == "":
		h.previewRole(w, d.Role)
	case d.Role == "" && d.Alias != "" && d.ConsumerID != "":
		h.previewChain(w, r, ar, d)
	default:
		writeConsumerError(w, http.StatusBadRequest, "model-alias-preview takes either alias and consumer_id or role")
	}
}

// previewRole is the suggestion mode: the pool's primary model of the role.
func (h *ManageHandler) previewRole(w http.ResponseWriter, role string) {
	if !previewRoles[role] {
		writeConsumerError(w, http.StatusBadRequest, "role must be one of chat, embed, rerank")
		return
	}
	if h.rolePrimary == nil {
		writePreviewUnwired(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "role": role, "primary_model": h.rolePrimary(role)})
}

// previewChain is the chain mode: the stored consumer as the door would
// build it, its headerless /v1 context, the dry run.
func (h *ManageHandler) previewChain(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, d modelAliasPreviewRequest) {
	if h.chainPreview == nil {
		writePreviewUnwired(w)
		return
	}
	c, err := store.GetConsumer(r.Context(), h.pool, d.ConsumerID, accessOf(ar), time.Now())
	if err != nil {
		writeConsumerStoreError(w, r, err, "model-alias-preview")
		return
	}
	door, err := h.previewDoorNotes(r.Context(), c)
	if err == nil {
		ca := previewConsumerAuth(c)
		var pv oai.ChainPreview
		if pv, err = h.chainPreview(previewContext(r.Context(), ca), ca, d.Alias); err == nil {
			pv.Notes = append(pv.Notes, door...)
			writeJSON(w, http.StatusOK, previewAnswer{Success: true, ConsumerID: c.ID, ChainPreview: pv})
			return
		}
	}
	slog.ErrorContext(r.Context(), "model-alias-preview: chain dry run failed", "consumer_id", c.ID, "error", err)
	writeJSONCode(w, http.StatusServiceUnavailable, "service_unavailable", map[string]any{
		"success": false, "error": "model-alias-preview: the chain dry run failed",
	})
}

// previewDoorNotes are the door gates the consumer's keys would fail now
// (ctx_model_auth, migration 161): gate 2 the consumer is active, gate 3 its
// tenant is active, gate 4 its tenant still owns its tenant_scope. The
// preview still computes the chain — what the consumer's configuration
// reaches — and names why the door refuses.
func (h *ManageHandler) previewDoorNotes(ctx context.Context, c store.ModelConsumer) ([]string, error) {
	var notes []string
	if !c.Active {
		notes = append(notes, oai.PreviewNoteConsumerInactive)
	}
	t, err := store.GetTenant(ctx, h.pool, c.TenantID)
	switch {
	case errors.Is(err, store.ErrTenantNotFound):
		notes = append(notes, oai.PreviewNoteTenantInactive)
	case err != nil:
		return nil, err
	case t.Status != "active":
		notes = append(notes, oai.PreviewNoteTenantInactive)
	}
	if c.TenantScope != nil {
		scopes, err := store.TenantScopes(ctx, h.pool, c.TenantID)
		if err != nil {
			return nil, err
		}
		if !slices.Contains(scopes, *c.TenantScope) {
			notes = append(notes, oai.PreviewNoteScopeNotOwned)
		}
	}
	return notes, nil
}

// writePreviewUnwired answers a server without the /v1 dependencies.
func writePreviewUnwired(w http.ResponseWriter) {
	writeJSONCode(w, http.StatusServiceUnavailable, "service_unavailable", map[string]any{
		"success": false, "error": "model-alias-preview: the model API is not wired",
	})
}

// previewConsumerAuth is the ConsumerAuth a request of consumer c would
// carry behind the door (the ctx_model_auth columns, migration 161): valid,
// with its tenant, scope, kind, all_models, prompt_sensitivity, fair_weight,
// admission_lane and peer binding — no key id (a preview names no key). An
// inactive consumer still previews (the door would refuse it — the handler
// adds the note, previewDoorNotes).
func previewConsumerAuth(c store.ModelConsumer) *auth.ConsumerAuth {
	ca := &auth.ConsumerAuth{
		ConsumerID: c.ID, TenantID: c.TenantID, Kind: auth.KindApp, AllModels: c.AllModels,
		PromptSensitivity: c.PromptSensitivity, FairWeight: int(c.FairWeight), AdmissionLane: c.AdmissionLane, IsValid: true,
	}
	if c.Kind == string(auth.KindCtxPeer) {
		ca.Kind = auth.KindCtxPeer
	}
	if c.TenantScope != nil {
		ca.TenantScope = *c.TenantScope
	}
	if c.PeerInstanceID != nil {
		ca.PeerInstanceID = *c.PeerInstanceID
	}
	return ca
}

// previewContext is the context a /v1 request of ca without federation
// headers reaches the handler with: the consumer in the door's context key,
// the admin's knowledge identity shadowed (RequestTenantScope resolves the
// consumer's tenant_scope only without an AuthResult — with both it stays on
// the base snapshot, and the tenant overlay of the preview would differ from
// the request's), and the PeerIngress policy (federation.HeaderlessContext).
func previewContext(ctx context.Context, ca *auth.ConsumerAuth) context.Context {
	ctx = context.WithValue(ctx, authResultKey, (*auth.AuthResult)(nil))
	ctx = context.WithValue(ctx, consumerAuthKey, ca)
	return federation.HeaderlessContext(ctx, ca)
}
