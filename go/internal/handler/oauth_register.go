// Dynamic Client Registration (RFC 7591) — POST /register (design 02 §4b,
// wave 02-W4a). The route is always mounted; CTX_OAUTH_DCR_MODE decides per
// request: off → 404 (fail-closed default, the metadata document advertises
// no registration_endpoint either), admin → server-admin bearer key required,
// open → unauthenticated. Open registration grants ZERO data access (INV-B:
// client.scopes is a requestable ceiling, never authoritative — the api key
// entered at /authorize stays the hard authority cap); the open mode is
// switched on in production only after the consent screen (K7) lands.
package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/store"
)

// dcrRequest is the consumed subset of RFC 7591 §2 client metadata.
// jwks/jwks_uri are deliberately NOT consumed — there is no private_key_jwt
// path in the MVP (design 02 §3); unknown fields are ignored per RFC 7591 §2.
type dcrRequest struct {
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	ClientName              string   `json:"client_name"`
	Scope                   string   `json:"scope"`
	ClientURI               string   `json:"client_uri"`
	LogoURI                 string   `json:"logo_uri"`
	Contacts                []string `json:"contacts"`
	TosURI                  string   `json:"tos_uri"`
	PolicyURI               string   `json:"policy_uri"`
	SoftwareID              string   `json:"software_id"`
}

// dcrAuthMethods is the advertised token_endpoint_auth_method set (§4c —
// mirrors token_endpoint_auth_methods_supported in the metadata document).
// Anything else, notably private_key_jwt, is invalid_client_metadata.
var dcrAuthMethods = map[string]bool{
	"none":                true,
	"client_secret_basic": true,
	"client_secret_post":  true,
}

// dcrGrantTypes is the allowed grant_types superset. refresh_token is
// registerable as client DATA already (the 097 column), even though /token
// only serves it once 03/S4 issues refresh tokens — registration stores the
// client's declared intent, the endpoint stays the enforcement point.
var dcrGrantTypes = map[string]bool{
	"authorization_code": true,
	"refresh_token":      true,
}

// oauthClientLabelMax mirrors context_oauth_clients.label VARCHAR(200) —
// client_name is truncated to it (design 02 §4b: cap, never 500/22001).
const oauthClientLabelMax = 200

// Register handles POST /register — RFC 7591 Dynamic Client Registration.
func (h *OAuthHandler) Register(w http.ResponseWriter, r *http.Request) {
	mode := dcrMode()
	if mode == "off" {
		// Same response an unmounted route would give — DCR switched off
		// advertises nothing and serves nothing (fail-closed default).
		http.NotFound(w, r)
		return
	}

	createdBy := "" // open mode: anonymous; forensics via rate-limit logs (W4b)
	if mode == "admin" {
		ar, err := auth.Authenticate(r.Context(), h.pool, apiKeyFromRequest(r))
		if err != nil {
			slog.Error("oauth: dcr admin auth", "error", err)
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		if ar == nil || !ar.IsValid {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
			return
		}
		if !ar.IsAdmin {
			writeJSON(w, http.StatusForbidden, map[string]string{"error": "admin key required"})
			return
		}
		createdBy = ar.ApiKeyID
	}

	// Body cap analogous to /authorize and /token (8192, design 02 §4b).
	r.Body = http.MaxBytesReader(w, r.Body, 8192)
	var req dcrRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		// Covers malformed JSON and the MaxBytesReader overrun alike —
		// RFC 7591 §3.2.2 error shape, HTTP 400.
		oauthError(w, "invalid_client_metadata", "malformed or oversized JSON body")
		return
	}

	spec, errCode, errDesc := req.spec()
	if errCode != "" {
		oauthError(w, errCode, errDesc)
		return
	}
	spec.CreatedBy = createdBy

	client, secret, err := store.RegisterOAuthClient(r.Context(), h.pool, spec)
	if err != nil {
		slog.Error("oauth: dcr register client", "error", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// RFC 7591 §3.2.1: 201 + the registered metadata. client_secret is
	// echoed EXACTLY once, here, and only for confidential clients;
	// client_secret_expires_at:0 states the honest never-expiry.
	resp := map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.CreatedAt.Unix(),
		"redirect_uris":              client.RedirectURIs,
		"token_endpoint_auth_method": client.TokenEndpointAuthMethod,
		"grant_types":                client.GrantTypes,
		"response_types":             client.ResponseTypes,
	}
	if client.Label != "" {
		resp["client_name"] = client.Label
	}
	if spec.TokenEndpointAuthMethod != "none" {
		resp["client_secret"] = secret
		resp["client_secret_expires_at"] = 0
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resp)
}

// spec validates and normalizes the consumed RFC 7591 metadata into a
// registration spec (design 02 §4b, fail-closed). A non-empty errCode is
// the RFC 7591 §3.2.2 error to answer with; CreatedBy is the caller's.
func (req *dcrRequest) spec() (spec store.RegisterOAuthClientSpec, errCode, errDesc string) {
	// redirect_uris — REQUIRED, ≥1, each through the SAME registration rule
	// the CLI pre-registration path uses (validateRegisteredRedirectURI:
	// https for real hosts, plain http only on loopback — the §4b core fix;
	// this list is what 03 matches EXACTLY at /authorize).
	if len(req.RedirectURIs) == 0 {
		return spec, "invalid_redirect_uri", "redirect_uris is required (at least one)"
	}
	for _, uri := range req.RedirectURIs {
		if err := validateRegisteredRedirectURI(uri); err != nil {
			return spec, "invalid_redirect_uri", err.Error()
		}
	}

	// token_endpoint_auth_method — default `none`: a DELIBERATE deviation
	// from the RFC 7591 §2 default client_secret_basic. The MCP client
	// population this server exists for is public-client-PKCE-dominated
	// (research §4.1); defaulting to a secret nobody stores would mint
	// dead credentials. Explicit values outside the advertised set → 400.
	authMethod := req.TokenEndpointAuthMethod
	if authMethod == "" {
		authMethod = "none"
	}
	if !dcrAuthMethods[authMethod] {
		return spec, "invalid_client_metadata", "unsupported token_endpoint_auth_method"
	}

	grantTypes := req.GrantTypes
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code"}
	}
	for _, gt := range grantTypes {
		if !dcrGrantTypes[gt] {
			return spec, "invalid_client_metadata", "unsupported grant_type"
		}
	}

	responseTypes := req.ResponseTypes
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	for _, rt := range responseTypes {
		if rt != "code" { // OAuth 2.1: no implicit, no hybrid
			return spec, "invalid_client_metadata", "unsupported response_type"
		}
	}

	// client_name → label, truncated to the column width (rune-safe: the
	// VARCHAR limit counts characters, and a byte cut could split a rune).
	label := req.ClientName
	if runes := []rune(label); len(runes) > oauthClientLabelMax {
		label = string(runes[:oauthClientLabelMax])
	}

	// RFC 7591 low-priority fields ride along in the 097 jsonb column.
	metadata := map[string]any{}
	for k, v := range map[string]string{
		"client_uri":  req.ClientURI,
		"logo_uri":    req.LogoURI,
		"tos_uri":     req.TosURI,
		"policy_uri":  req.PolicyURI,
		"software_id": req.SoftwareID,
	} {
		if v != "" {
			metadata[k] = v
		}
	}
	if len(req.Contacts) > 0 {
		metadata["contacts"] = req.Contacts
	}

	return store.RegisterOAuthClientSpec{
		Label:        label,
		RedirectURIs: req.RedirectURIs,
		// scope (space-separated) → requestable ceiling. INV-B: NEVER
		// authoritative — data only, the key authority stays the hard cap.
		Scopes:                  strings.Fields(req.Scope),
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		TokenEndpointAuthMethod: authMethod,
		Source:                  "dcr",
		Metadata:                metadata,
	}, "", ""
}
