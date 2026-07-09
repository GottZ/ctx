// Manage family oauth-provider-* (OAuth Achse 04, Welle W4/L3, design/04
// §3.2 + §7-W4): the admin surface for the external-login IdP allowlist
// (context_oauth_providers, migration 100).
//
//	oauth-provider-create  register an IdP; seals the client_secret via
//	                       sealbox into context_secrets (reveal-never)
//	oauth-provider-list    config rows; secret existence ONLY as has_secret
//	oauth-provider-delete  remove row + sealed secret (one transaction)
//
// All three are tierServerAdmin (actionTier, S9-pinned): the provider table
// is the INV-C trust anchor. The client_secret arrives ONLY in create's data
// payload, is sealed before the row commits and is never echoed — error
// messages come from the store's validation layer, which carries no secret
// material. Flow handlers (/auth/login, /auth/callback) are wave W6/L5, not
// here.

package handler

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
)

// sealboxOrNil builds the process sealbox for provider-secret sealing. nil
// when no master key is configured — the store then refuses secret-bearing
// creates fail-closed (ErrOAuthProviderSealbox → 503), while token_auth='none'
// providers (no secret) still work. A field-injected openBox (tests) wins
// over env; the lazy fallback avoids churning NewManageHandler's call sites.
func (h *ManageHandler) sealboxOrNil() *sealbox.Box {
	open := h.openBox
	if open == nil {
		open = sealbox.FromEnv
	}
	box, err := open()
	if err != nil {
		return nil
	}
	return box
}

// oauthProviderCreateData is the JSON shape under req.Data for
// oauth-provider-create. client_secret is write-only: sealed immediately,
// never stored in the row, never echoed. allowed_claim stays raw here — the
// strict {claim, values[]} shape check lives in the store's validate
// (design/04 L1 handover: the schema does not enforce it).
type oauthProviderCreateData struct {
	Slug               string          `json:"slug"`
	Type               string          `json:"type"`
	DisplayName        string          `json:"display_name"`
	Issuer             string          `json:"issuer"`
	ClientID           string          `json:"client_id"`
	ClientSecret       string          `json:"client_secret"`
	TokenAuth          string          `json:"token_auth"`
	RedirectBase       string          `json:"redirect_base"`
	Scopes             []string        `json:"scopes"`
	AuthURL            string          `json:"auth_url"`
	TokenURL           string          `json:"token_url"`
	UserinfoURL        string          `json:"userinfo_url"`
	IDTokenAlgs        []string        `json:"id_token_algs"`
	SingleTenantIssuer *bool           `json:"single_tenant_issuer"`
	AllowedClaim       json.RawMessage `json:"allowed_claim"`
}

func (h *ManageHandler) handleOAuthProviderCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	var data oauthProviderCreateData
	if len(req.Data) > 0 && string(req.Data) != "null" {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid data"})
			return
		}
	}

	spec := store.CreateOAuthProviderSpec{
		Slug:         data.Slug,
		Type:         data.Type,
		DisplayName:  data.DisplayName,
		Issuer:       data.Issuer,
		ClientID:     data.ClientID,
		ClientSecret: data.ClientSecret,
		TokenAuth:    data.TokenAuth,
		RedirectBase: data.RedirectBase,
		Scopes:       data.Scopes,
		AuthURL:      data.AuthURL,
		TokenURL:     data.TokenURL,
		UserinfoURL:  data.UserinfoURL,
		IDTokenAlgs:  data.IDTokenAlgs,
		// Absent field defaults to true — the migration-100 schema default.
		// An explicit false then demands allowed_claim (store, fail-closed).
		SingleTenantIssuer: data.SingleTenantIssuer == nil || *data.SingleTenantIssuer,
		AllowedClaim:       data.AllowedClaim,
		CreatedBy:          ar.ApiKeyID,
	}

	provider, err := store.CreateOAuthProvider(r.Context(), h.pool, h.sealboxOrNil(), spec)
	if err != nil {
		switch {
		case errors.Is(err, store.ErrOAuthProviderInvalid):
			// Validation detail is safe to surface (never secret material) and
			// includes the mapped migration-100 CHECK errors (slug regex &c).
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		case errors.Is(err, store.ErrOAuthProviderExists):
			writeJSON(w, http.StatusConflict, map[string]any{"success": false, "error": err.Error()})
		case errors.Is(err, store.ErrOAuthProviderSealbox):
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"success": false, "error": "secrets unavailable: CTX_SECRETS_KEY not configured"})
		default:
			slog.Error("manage: create oauth provider failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		}
		return
	}

	// OAuthProvider carries no secret material by construction (has_secret
	// bool only) — the client_secret plaintext dies with this request.
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "provider": provider})
}

func (h *ManageHandler) handleOAuthProviderList(w http.ResponseWriter, r *http.Request) {
	providers, err := store.ListOAuthProviders(r.Context(), h.pool)
	if err != nil {
		slog.Error("manage: list oauth providers failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "providers": providers})
}

func (h *ManageHandler) handleOAuthProviderDelete(w http.ResponseWriter, r *http.Request, req manageRequest) {
	var data struct {
		Slug string `json:"slug"`
	}
	if len(req.Data) > 0 && string(req.Data) != "null" {
		if err := json.Unmarshal(req.Data, &data); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid data"})
			return
		}
	}
	if data.Slug == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "slug is required"})
		return
	}

	deleted, err := store.DeleteOAuthProvider(r.Context(), h.pool, data.Slug)
	if err != nil {
		slog.Error("manage: delete oauth provider failed", "error", err, "request_id", RequestIDFromContext(r.Context()))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	if !deleted {
		writeJSON(w, http.StatusOK, map[string]any{"success": false, "error": "provider not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": data.Slug})
}
