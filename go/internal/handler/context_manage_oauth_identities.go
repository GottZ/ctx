// Manage family oauth-identity-* (OAuth R5, design/05 §4.5, E4b
// admin-invite): the operator surface that pre-links external identities to
// principals — the ONLY provisioning path (the login flow never creates
// identity rows; an unlinked login answers 403).
//
//	oauth-identity-link    bind (provider_slug, subject) → principal_id;
//	                       re-binding to a DIFFERENT principal is a 409 —
//	                       unlink explicitly first (account-takeover shape)
//	oauth-identity-list    bindings, optionally filtered by principal_id
//	oauth-identity-unlink  remove one binding (idempotent)
//
// All three are tierServerAdmin (actionTier, S9-pinned): identity bindings
// decide WHO a login resolves to — the same trust altitude as the provider
// allowlist. The issuer is resolved server-side from the provider row
// (github → the fixed GitHubIssuer constant, oidc → the row's validated
// issuer) — the operator supplies a slug, never a raw issuer string, so the
// stored value always matches what the login path looks up.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/oidc"
	"github.com/GottZ/ctx/internal/store"
	"github.com/google/uuid"
)

// decodeIdentityData unmarshals req.Data (the family's shared shape); an
// absent/null payload folds to the zero value — the per-action validation
// decides what is required.
func decodeIdentityData(req manageRequest, data *oauthIdentityData) error {
	if len(req.Data) == 0 || string(req.Data) == "null" {
		return nil
	}
	return json.Unmarshal(req.Data, data)
}

// oauthIdentityData is the JSON shape under req.Data for the identity
// actions (link uses all fields, unlink provider_slug+subject, list an
// optional principal_id filter).
type oauthIdentityData struct {
	ProviderSlug string `json:"provider_slug"`
	Subject      string `json:"subject"`
	PrincipalID  string `json:"principal_id"`
	Email        string `json:"email"`
	DisplayName  string `json:"display_name"`
}

// identityIssuerForSlug resolves the trust issuer the login path will look
// up for this provider. found=false when the slug names no provider row.
func (h *ManageHandler) identityIssuerForSlug(r *http.Request, slug string) (provider, issuer string, found bool, err error) {
	p, ok, err := store.GetOAuthProviderBySlug(r.Context(), h.pool, slug)
	if err != nil || !ok {
		return "", "", false, err
	}
	if p.Type == "github" {
		return p.Type, oidc.GitHubIssuer, true, nil
	}
	return p.Type, p.Issuer, true, nil
}

func (h *ManageHandler) handleOAuthIdentityLink(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data oauthIdentityData
	if err := decodeIdentityData(req, &data); err != nil ||
		data.ProviderSlug == "" || data.Subject == "" || uuid.Validate(data.PrincipalID) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "provider_slug, subject and a valid principal_id are required",
		})
		return
	}
	provider, issuer, found, err := h.identityIssuerForSlug(r, data.ProviderSlug)
	if err != nil {
		slog.Error("oauth-identity-link: provider lookup", "error", err)
		writeInternal(w)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "unknown provider slug"})
		return
	}
	// The principal must exist (23503 would say so too, but a clean 404
	// beats a constraint error on an operator surface).
	var exists bool
	if err := h.pool.QueryRow(r.Context(),
		`SELECT EXISTS(SELECT 1 FROM context_principals WHERE id = $1::uuid)`, data.PrincipalID).Scan(&exists); err != nil {
		writeInternal(w)
		return
	}
	if !exists {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "unknown principal_id"})
		return
	}
	if err := store.LinkExternalIdentity(r.Context(), h.pool,
		provider, issuer, data.Subject, data.PrincipalID, data.Email, data.DisplayName); err != nil {
		if errors.Is(err, store.ErrIdentityConflict) {
			writeJSON(w, http.StatusConflict, map[string]any{
				"success": false, "error": "identity already linked to another principal — unlink first",
			})
			return
		}
		slog.Error("oauth-identity-link: store", "error", err)
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "issuer": issuer, "subject": data.Subject, "principal_id": data.PrincipalID,
	})
}

func (h *ManageHandler) handleOAuthIdentityList(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data oauthIdentityData
	_ = decodeIdentityData(req, &data) // empty data = unfiltered list
	if data.PrincipalID != "" && uuid.Validate(data.PrincipalID) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid principal_id"})
		return
	}
	list, err := store.ListExternalIdentities(r.Context(), h.pool, data.PrincipalID)
	if err != nil {
		slog.Error("oauth-identity-list: store", "error", err)
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "identities": list, "count": len(list)})
}

func (h *ManageHandler) handleOAuthIdentityUnlink(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data oauthIdentityData
	if err := decodeIdentityData(req, &data); err != nil || data.ProviderSlug == "" || data.Subject == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false, "error": "provider_slug and subject are required",
		})
		return
	}
	_, issuer, found, err := h.identityIssuerForSlug(r, data.ProviderSlug)
	if err != nil {
		slog.Error("oauth-identity-unlink: provider lookup", "error", err)
		writeInternal(w)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "unknown provider slug"})
		return
	}
	removed, err := store.UnlinkExternalIdentity(r.Context(), h.pool, issuer, data.Subject)
	if err != nil {
		slog.Error("oauth-identity-unlink: store", "error", err)
		writeInternal(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "removed": removed})
}

// dispatchOAuthIdentityAction fans the oauth-identity-* actions out (R5;
// same split pattern as dispatchMCPClientAction, cyclop budget).
func (h *ManageHandler) dispatchOAuthIdentityAction(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "oauth-identity-link":
		h.handleOAuthIdentityLink(w, r, req)
	case "oauth-identity-list":
		h.handleOAuthIdentityList(w, r, req)
	case "oauth-identity-unlink":
		h.handleOAuthIdentityUnlink(w, r, req)
	}
}
