package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/httpx"
	"github.com/GottZ/ctx/internal/llm"
	"github.com/GottZ/ctx/internal/store"
)

// Backend pool manage actions (F3-P1, design 03 §3.4). All five are
// admin-gated via actionRequiresAdmin — without the gate, backend-create
// would be a corpus exfiltration/SSRF API (risk R1; F3 must not go live
// before G03, which is satisfied).
//
// Snapshot propagation is normative: every mutation ends with a SYNCHRONOUS
// pool reload after commit (immediate effect in this process — the only
// instant brake against a compromised backend is backend-update
// enabled=false); the 053 NOTIFY trigger additionally covers psql
// break-glass edits and multi-process convergence.

// backendSpec is the create/update payload. Pointer fields distinguish
// "absent" from zero for patch semantics; presence is double-checked via the
// raw key set where the zero value is meaningful.
type backendSpec struct {
	Name                  *string `json:"name"`
	BaseURL               *string `json:"base_url"`
	Protocol              *string `json:"protocol"`
	ProviderClass         *string `json:"provider_class"`
	APIKeyRef             *string `json:"api_key_ref"`
	Trust                 *string `json:"trust"`
	ConfirmTrustElevation bool    `json:"confirm_trust_elevation"`
	// ConfirmDataCollection guards arming metadata.allow_data_collection —
	// the ONLY way to lift the forced zdr/deny of an openrouter-class
	// backend (never implicit via trust elevation, design 03 §3.3).
	ConfirmDataCollection bool    `json:"confirm_data_collection"`
	Locality              *string `json:"locality"`
	// Scope is the tenant dimension (062, Modell C). HONORED ONLY for a
	// server-admin on create (free choice, defaults to _global); a tenant-admin
	// always has it forced to ar.HomeScope, and update ignores it entirely
	// (scope is immutable through the patch path) — see backendCreateScope.
	Scope        *string           `json:"scope"`
	Roles        []string          `json:"roles"`
	ModelMap     json.RawMessage   `json:"model_map"`
	Timeouts     map[string]int    `json:"timeouts"`
	NumCtx       *int              `json:"num_ctx"`
	Priority     *int              `json:"priority"`
	Enabled      *bool             `json:"enabled"`
	ExtraHeaders map[string]string `json:"extra_headers"`
	ExtraBody    map[string]any    `json:"extra_body"`
	Limits       map[string]any    `json:"limits"`
	Metadata     map[string]any    `json:"metadata"`
	Probe        string            `json:"probe"` // backend-test only
}

// applySpec overlays the present payload fields onto b. keys is the raw
// field-presence set (patch semantics for maps/slices whose zero value is
// meaningful: an explicit empty roles list clears roles, an absent key
// keeps them).
func applySpec(b *backends.Backend, spec *backendSpec, keys map[string]json.RawMessage) error {
	if spec.Name != nil {
		b.Name = *spec.Name
	}
	if spec.BaseURL != nil {
		b.Host = *spec.BaseURL
	}
	if spec.Protocol != nil {
		b.Protocol = backends.Protocol(*spec.Protocol)
	}
	if spec.ProviderClass != nil {
		b.ProviderClass = *spec.ProviderClass
	}
	if spec.APIKeyRef != nil {
		b.APIKeyRef = *spec.APIKeyRef
	}
	if spec.Trust != nil {
		b.Trust = backends.Trust(*spec.Trust)
	}
	if spec.Locality != nil {
		b.Locality = *spec.Locality
	}
	if _, ok := keys["roles"]; ok {
		b.Roles = spec.Roles
	}
	if _, ok := keys["model_map"]; ok {
		mm, err := backends.ParseModelMap(spec.ModelMap)
		if err != nil {
			return fmt.Errorf("model_map: %w", err)
		}
		b.ModelMap = mm
	}
	if _, ok := keys["timeouts"]; ok {
		b.Timeouts = spec.Timeouts
	}
	if spec.NumCtx != nil {
		b.NumCtx = *spec.NumCtx
	}
	if spec.Priority != nil {
		b.Priority = *spec.Priority
	}
	if spec.Enabled != nil {
		b.Enabled = *spec.Enabled
	}
	if _, ok := keys["extra_headers"]; ok {
		b.ExtraHeaders = spec.ExtraHeaders
	}
	if _, ok := keys["extra_body"]; ok {
		b.ExtraBody = spec.ExtraBody
	}
	if _, ok := keys["limits"]; ok {
		b.Limits = spec.Limits
	}
	if _, ok := keys["metadata"]; ok {
		b.Metadata = spec.Metadata
	}
	return nil
}

func parseBackendSpec(data json.RawMessage) (*backendSpec, map[string]json.RawMessage, error) {
	if len(data) == 0 {
		return nil, nil, fmt.Errorf("data payload required")
	}
	var spec backendSpec
	if err := json.Unmarshal(data, &spec); err != nil {
		return nil, nil, fmt.Errorf("unparseable data payload")
	}
	var keys map[string]json.RawMessage
	if err := json.Unmarshal(data, &keys); err != nil {
		return nil, nil, fmt.Errorf("unparseable data payload")
	}
	return &spec, keys, nil
}

// backendView renders one pool row for list/create/update responses.
// api_key_ref is the secret NAME (harmless); resolved values never leave
// the in-memory snapshot. extra_headers values are denylist-validated
// non-credentials and admin-only here.
func backendView(b *backends.Backend) map[string]any {
	return map[string]any{
		"id": b.ID, "name": b.Name, "base_url": b.Host,
		"protocol": string(b.Protocol), "provider_class": b.ProviderClass,
		"api_key_ref": b.APIKeyRef, "trust": string(b.Trust),
		"locality": b.Locality, "scope": b.Scope, "roles": b.Roles,
		"model_map": b.ModelMap, "timeouts": b.Timeouts, "num_ctx": b.NumCtx,
		"priority": b.Priority, "enabled": b.Enabled,
		"extra_headers": b.ExtraHeaders, "extra_body": b.ExtraBody,
		"limits": b.Limits, "metadata": b.Metadata,
	}
}

// backendCreateScope resolves the tenant scope a new backend row gets, enforcing
// the two-tier rule (MT T37, 04-W5 §4.6): a tenant-admin's backend is ALWAYS
// pinned to its own tenant (ar.HomeScope — the payload scope is ignored, exactly
// like the /api/store write-scope guard); only a server-admin may choose a scope
// freely (e.g. provision a tenant-private backend FOR a tenant, or a shared
// _global one), defaulting to _global when none is given. Empty/whitespace from a
// server-admin normalizes to _global so a write never lands a blank scope.
func backendCreateScope(ar *auth.AuthResult, specScope *string) string {
	if !ar.IsServerAdmin() {
		return ar.HomeScope // tenant-admin: forced to own tenant, never caller-chosen
	}
	if specScope != nil && *specScope != "" {
		return *specScope
	}
	return backends.GlobalScope
}

// backendWriteScopes is the permitted-tenant set passed to the store gate for
// update/delete (MT T37, 04-W5 §4.6/§5.5): nil for a server-admin (authority
// over every tenant — no scope predicate) and []string{ar.HomeScope} for a
// tenant-admin (only its own rows). The same set drives the in-handler pre-check
// so a foreign id never reaches the validation path.
func backendWriteScopes(ar *auth.AuthResult) []string {
	if ar.IsServerAdmin() {
		return nil
	}
	return []string{ar.HomeScope}
}

// backendVisibleToCaller reports whether this admin caller may SEE a backend in
// the given scope (MT T37, 04-W5): a server-admin sees every row; a tenant-admin
// sees _global ∪ its own tenant, via the exact Chain egress predicate. Drives
// the backend-list filter (read visibility — a shared _global backend is visible
// to every tenant-admin, just not mutable by one).
func backendVisibleToCaller(ar *auth.AuthResult, scope string) bool {
	return ar.IsServerAdmin() || backends.VisibleTo(scope, ar.HomeScope)
}

// backendWritableByCaller reports whether this admin caller may MUTATE a backend
// in the given scope (MT T37, 04-W5 §4.6) — strictly narrower than visibility: a
// tenant-admin owns ONLY its own tenant's rows (not _global, which it can see but
// not change; not a foreign tenant's). It mirrors backendWriteScopes exactly
// (nil ⇒ server-admin ⇒ all; else only ar.HomeScope), so the update/delete
// pre-check answers 404 on the same set the store gate rejects — no 422-vs-404
// oracle, and the store gate stays the fail-closed backstop.
func backendWritableByCaller(ar *auth.AuthResult, scope string) bool {
	return ar.IsServerAdmin() || scope == ar.HomeScope
}

func writeBackendValidation(w http.ResponseWriter, errs []backends.FieldError) {
	writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
		"success": false, "error": "validation failed", "fields": errs,
	})
}

// reloadAfterMutation is the synchronous half of the snapshot propagation.
// The mutation is committed — a reload failure must not turn it into a
// client error; the NOTIFY path retries momentarily.
func (h *ManageHandler) reloadAfterMutation(ctx context.Context, action string) {
	if err := h.backendPool.Reload(ctx); err != nil {
		slog.Error("backends: post-mutation reload failed — NOTIFY path will converge",
			"action", action, "error", err)
	}
}

func (h *ManageHandler) poolBackendByID(id string) *backends.Backend {
	for _, b := range h.backendPool.Snapshot() {
		if b.ID == id {
			return &b
		}
	}
	return nil
}

func (h *ManageHandler) handleBackendCreate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	spec, keys, err := parseBackendSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	// DDL defaults, fail-closed: trust starts at public (new backends are
	// explicitly promoted), locality derives from the URL when absent.
	b := &backends.Backend{
		Protocol:      backends.ProtocolOpenAI,
		ProviderClass: backends.ProviderGeneric,
		Trust:         backends.TrustPublic,
		Enabled:       true,
	}
	if err := applySpec(b, spec, keys); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if b.Locality == "" && b.Host != "" {
		if derived, derr := backends.DeriveLocality(b.Host); derr == nil {
			b.Locality = derived
		}
	}

	// Tenant scope is server-assigned, never caller-chosen for a tenant-admin
	// (forced to ar.HomeScope); only a server-admin picks it (T37, §4.6). Set
	// before validation/insert so the persisted row carries the right owner.
	b.Scope = backendCreateScope(ar, spec.Scope)

	// Trust elevation needs the confirm flag ON CREATE TOO: without it the
	// update-confirm would be trivially bypassed via direct create or
	// delete+create — and create with trust=full-trust + external base_url
	// is exactly the most dangerous single mutation (decision E7/§7).
	if b.Trust != backends.TrustPublic && !spec.ConfirmTrustElevation {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("creating a backend with trust %q requires confirm_trust_elevation:true — trust decides which content may flow here", b.Trust),
		})
		return
	}
	// Same bypass logic as the trust confirm: without the create-side check
	// the update-confirm would fall to a direct create.
	if dataCollectionEscapeOn(b) && !spec.ConfirmDataCollection {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "metadata.allow_data_collection:true lifts the forced zdr/data_collection=deny of this openrouter backend — requires confirm_data_collection:true",
		})
		return
	}

	warnings, fieldErrs := backends.ValidateBackend(b)
	if len(fieldErrs) > 0 {
		writeBackendValidation(w, fieldErrs)
		return
	}

	by := actorID(r)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "transaction begin failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	id, err := store.CreateBackend(ctx, tx, b, by)
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "commit failed"})
		return
	}
	b.ID = id
	h.reloadAfterMutation(ctx, "backend-create")

	resp := map[string]any{"success": true, "backend": backendView(b)}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *ManageHandler) handleBackendUpdate(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	prev := h.poolBackendByID(req.ID)
	if prev == nil || !backendWritableByCaller(ar, prev.Scope) {
		// A foreign or _global row is "not found" to a tenant-admin — identical
		// body to a truly missing id (no existence oracle, mirrors chat.go:402-
		// 404), and the validation path never runs on a row it cannot touch.
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "backend not found"})
		return
	}
	spec, keys, err := parseBackendSpec(req.Data)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": err.Error()})
		return
	}

	next := *prev
	next.APIKey = "" // never round-trip the resolved key through a write
	if err := applySpec(&next, spec, keys); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}

	// Trust ELEVATION (toward full-trust = more content may flow) needs the
	// confirm flag — the opsec immutability edge in miniature; lowering is
	// free (design §3.4).
	if trustRankRose(prev.Trust, next.Trust) && !spec.ConfirmTrustElevation {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("raising trust from %q to %q requires confirm_trust_elevation:true — trust decides which content may flow here", prev.Trust, next.Trust),
		})
		return
	}
	// Confirm only when the escape ARMS (off → on) — re-saving a backend
	// that already carries it stays friction-free, mirroring trustRankRose.
	if dataCollectionEscapeOn(&next) && !dataCollectionEscapeOn(prev) && !spec.ConfirmDataCollection {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"success": false,
			"error":   "metadata.allow_data_collection:true lifts the forced zdr/data_collection=deny of this openrouter backend — requires confirm_data_collection:true",
		})
		return
	}

	warnings, fieldErrs := backends.ValidateBackend(&next)
	if len(fieldErrs) > 0 {
		writeBackendValidation(w, fieldErrs)
		return
	}

	by := actorID(r)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "transaction begin failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Store-layer scope gate (fail-closed backstop, §5.5): nil for a server-
	// admin, []string{ar.HomeScope} for a tenant-admin. A foreign row matches
	// zero rows atomically → found=false → 404 (no TOCTOU, no second call path).
	found, err := store.UpdateBackend(ctx, tx, &next, by, backendWriteScopes(ar))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"success": false, "error": err.Error()})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "backend not found"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "commit failed"})
		return
	}
	h.reloadAfterMutation(ctx, "backend-update")

	resp := map[string]any{"success": true, "backend": backendView(&next)}
	if len(warnings) > 0 {
		resp["warnings"] = warnings
	}
	writeJSON(w, http.StatusOK, resp)
}

func (h *ManageHandler) handleBackendDelete(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	by := actorID(r)
	tx, err := h.pool.Begin(ctx)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "transaction begin failed"})
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// Store-layer scope gate (§4.6/§5.5): a tenant-admin deleting a foreign/
	// _global id matches zero rows → found=false → 404 (no oracle, no TOCTOU).
	name, found, err := store.DeleteBackend(ctx, tx, req.ID, by, backendWriteScopes(ar))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "delete failed"})
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "backend not found"})
		return
	}
	if err := tx.Commit(ctx); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "commit failed"})
		return
	}
	h.reloadAfterMutation(ctx, "backend-delete")
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "deleted": name})
}

func (h *ManageHandler) handleBackendList(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, _ manageRequest) {
	// Rows + live status merged (admin-gated). last_error is the sanitized
	// ErrClass — full URLs/provider bodies exist in slog only (§2.9).
	// Tenant-scoped (T37, §4.6): a server-admin sees every row; a tenant-admin
	// sees only _global ∪ its own — a foreign tenant-private backend is not even
	// disclosed as existing (egress topology, the read counterpart to Chain's
	// by-construction exclusion).
	snap := h.backendPool.Snapshot()
	status := h.backendPool.Status()
	statusByID := make(map[string]backends.BackendStatus, len(status))
	for _, s := range status {
		statusByID[s.ID] = s
	}
	list := make([]map[string]any, 0, len(snap))
	for i := range snap {
		if !backendVisibleToCaller(ar, snap[i].Scope) {
			continue
		}
		v := backendView(&snap[i])
		if s, ok := statusByID[snap[i].ID]; ok {
			v["effective_state"] = s.EffectiveState
			v["cooldown_remaining_s"] = s.CooldownRemaining
			v["consecutive_fails"] = s.ConsecutiveFails
			// disabled_by_profiles rides the live-status merge (like
			// effective_state): the ACTIVE disable-profiles containing this
			// backend (092, U01-W2). Uniform for both admin tiers — a
			// tenant-admin sees it on the visible _global rows (U01-E2=a).
			if len(s.DisabledByProfiles) > 0 {
				v["disabled_by_profiles"] = s.DisabledByProfiles
			}
			if s.LastErrorClass != "" {
				v["last_error"] = s.LastErrorClass
			}
			if s.LastOK != "" {
				v["last_ok"] = s.LastOK
			}
		}
		list = append(list, v)
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "backends": list})
}

// handleBackendTest probes one backend without settings effect: a
// reachability GET (llamacpp: /health; openrouter: /key with auth; generic:
// base_url), optionally a 1-token chat probe against the default model.
// openrouter-class backends additionally report credits and the default
// model's ZDR endpoint count (G29) — the count that predicts whether the
// forced zdr:true leaves a non-empty provider set.
func (h *ManageHandler) handleBackendTest(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	ctx := r.Context()
	if req.ID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "id required"})
		return
	}
	b := h.poolBackendByID(req.ID)
	if b == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "backend not found"})
		return
	}
	var spec backendSpec
	if len(req.Data) > 0 {
		_ = json.Unmarshal(req.Data, &spec)
	}

	checks := map[string]string{}
	start := time.Now()
	reachable := probeBackendURL(ctx, b, checks)
	latency := time.Since(start).Milliseconds()

	if reachable && spec.Probe == "chat" {
		// The admin's principal derives from ctx inside Acquire (MW4,
		// design/03 §4.1.1) — ctx is the authenticated request context.
		probeChat(ctx, h.admitter, b, checks)
	}

	result := map[string]any{
		"success": true, "reachable": reachable,
		"latency_ms": latency, "checks": checks,
	}
	if reachable && b.ProviderClass == backends.ProviderOpenRouter {
		if det := openRouterDetails(ctx, b); len(det) > 0 {
			result["openrouter"] = det
		}
	}
	writeJSON(w, http.StatusOK, result)
}

// openRouterDetails gathers the G29 detail checks: account credits from
// GET /v1/key (authenticated; limit_remaining is null on unlimited keys,
// usage is always present) and the ZDR endpoint count of the backend's
// default model from the public GET /v1/endpoints/zdr. A zero zdr_endpoints
// means the forced zdr:true will fail permanently with "no providers"
// (ClassNoProviders) — visible here BEFORE the first failover needs the
// backend. The /v1 segment matches the chat path's b.Host+"/v1/chat/..."
// convention: base_url is the API root WITHOUT the version segment
// (llama.cpp: host:port; OpenRouter: https://openrouter.ai/api).
func openRouterDetails(ctx context.Context, b *backends.Backend) map[string]any {
	details := map[string]any{}
	if body, ok := openRouterGET(ctx, b.Host+"/v1/key", b.APIKey); ok {
		var key struct {
			Data struct {
				LimitRemaining *float64 `json:"limit_remaining"`
				Usage          *float64 `json:"usage"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &key) == nil {
			if key.Data.LimitRemaining != nil {
				details["credits_remaining"] = *key.Data.LimitRemaining
			}
			if key.Data.Usage != nil {
				details["usage_usd"] = *key.Data.Usage
			}
		}
	}
	model := b.ModelFor(backends.RoleSynthesis).Model
	if model == "" {
		return details
	}
	if body, ok := openRouterGET(ctx, b.Host+"/v1/endpoints/zdr", ""); ok {
		var zdr struct {
			Data []struct {
				ModelID string `json:"model_id"`
			} `json:"data"`
		}
		if json.Unmarshal(body, &zdr) == nil {
			n := 0
			for _, ep := range zdr.Data {
				if ep.ModelID == model {
					n++
				}
			}
			details["zdr_endpoints"] = n
		}
	}
	return details
}

// openRouterGET is one authenticated detail-check GET; non-200 or transport
// failure degrades to "field absent" — the reachability verdict was already
// made by probeBackendURL.
func openRouterGET(ctx context.Context, url, apiKey string) ([]byte, bool) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, false
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	resp, err := http.DefaultClient.Do(req) //nolint:gosec // G704: admin-gated runtime data; reaching the configured backend is this action's function
	if err != nil {
		return nil, false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	// The full ZDR endpoint list is ~1 MB at 628 endpoints; cap generously.
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, false
	}
	return body, true
}

// dataCollectionEscapeOn mirrors llm.allowsDataCollection: only a literal
// bool true on an openrouter-class row arms the non-ZDR escape.
func dataCollectionEscapeOn(b *backends.Backend) bool {
	if b.ProviderClass != backends.ProviderOpenRouter {
		return false
	}
	v, ok := b.Metadata["allow_data_collection"].(bool)
	return ok && v
}

func probeBackendURL(ctx context.Context, b *backends.Backend, checks map[string]string) bool {
	url := b.Host
	switch b.ProviderClass {
	case backends.ProviderLlamaCpp:
		url += "/health"
	case backends.ProviderOpenRouter:
		// /v1/key matches the chat path convention (base_url is the API root
		// without the version segment, b.Host+"/v1/...").
		url += "/v1/key"
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		checks["base_url"] = "error: invalid URL"
		return false
	}
	if b.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+b.APIKey)
	}
	resp, err := http.DefaultClient.Do(httpReq) //nolint:gosec // G704: admin-gated runtime data; reaching arbitrary URLs is this action's function
	if err != nil {
		if httpx.IsBackendUnavailable(err) {
			checks["base_url"] = "error: unreachable"
		} else {
			checks["base_url"] = "error: request failed"
		}
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		checks["base_url"] = "ok"
		checks["auth"] = fmt.Sprintf("error: status %d", resp.StatusCode)
		return true
	}
	if resp.StatusCode >= 400 {
		checks["base_url"] = fmt.Sprintf("error: status %d", resp.StatusCode)
		return false
	}
	checks["base_url"] = "ok"
	if b.APIKeyRef != "" {
		checks["auth"] = "ok"
	}
	return true
}

// probeChat runs the 1-token connectivity chat under a dispatch lease (MW3,
// design/01 §4.6 N8b): interactive — an admin waits synchronously; NumPredict
// 1 is negligible, but I-D1 knows no exception (every exception is a future
// unadmitted call site). The admin's principal derives from ctx inside
// Acquire (MW4, design/03 §4.1.1 — no principal parameter). Deliberately NO
// herald entry: 15 s, 1 token — no DB-/CPU-consideration the LLM-free arms
// would need signaled. The probe's usage stays unreported (the chatProbe
// seam returns no token counts) — it counts as one uncharged interactive
// release in the MW22 meter.
func probeChat(ctx context.Context, adm dispatch.Admitter, b *backends.Backend, checks map[string]string) {
	spec := b.ModelFor(backends.RoleChat)
	if spec.Model == "" {
		spec = b.ModelFor("default")
	}
	if spec.Model == "" {
		checks["chat"] = "error: no model in model_map"
		return
	}
	probe := *b
	probe.Model = spec.Model
	key := "model_" + spec.Model
	if adm == nil {
		// No unadmitted wire call (I-D1) — fail loudly instead.
		checks[key] = "error: dispatch admitter not wired"
		return
	}
	lease, runCtx, err := adm.Acquire(ctx, dispatch.Request{
		Target:     dispatch.Target{Origin: b.Host}, // Acquire normalizes defensively
		Class:      dispatch.ClassInteractive,
		Role:       backends.RoleChat,
		DeadlineIn: probeChatTimeout, // admission-anchored hint (rule V1)
	})
	if err != nil {
		// Acquire-error doctrine (§4.3): no attempt, no Classify, no health
		// report — the probe result stays generic.
		checks[key] = "error: admission rejected"
		return
	}
	defer lease.Release()
	_, err = chatProbe(runCtx, probe)
	if err != nil {
		checks[key] = "error: " + backends.Classify(err, b.ProviderClass).String()
		return
	}
	checks[key] = "ok"
}

// dispatchBackendAction fans the backend-* actions out (split from
// HandleManage's switch for cyclomatic budget).
func (h *ManageHandler) dispatchBackendAction(w http.ResponseWriter, r *http.Request, ar *auth.AuthResult, req manageRequest) {
	switch req.Action {
	case "backend-create":
		h.handleBackendCreate(w, r, ar, req)
	case "backend-update":
		h.handleBackendUpdate(w, r, ar, req)
	case "backend-delete":
		h.handleBackendDelete(w, r, ar, req)
	case "backend-list":
		h.handleBackendList(w, r, ar, req)
	case "backend-test":
		h.handleBackendTest(w, r, ar, req)
	}
}

// trustRankRose reports an elevation: the new trust admits MORE sensitive
// content than the old (the confirm-gated direction).
func trustRankRose(prev, next backends.Trust) bool {
	return next.Rank() > prev.Rank()
}

// probeChatTimeout is the 1-token connectivity probe's wire timeout; it
// doubles as the admission-anchored deadline hint of the probe's lease.
const probeChatTimeout = 15 * time.Second

// chatProbe is indirected for tests.
var chatProbe = func(ctx context.Context, b backends.Backend) (any, error) {
	return llm.Chat(ctx, b, "You are a connectivity probe.", "Reply with: ok",
		llm.Options{Temperature: 0, NumPredict: 1}, probeChatTimeout)
}
