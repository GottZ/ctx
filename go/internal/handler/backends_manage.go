package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/backends"
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
	Name                  *string           `json:"name"`
	BaseURL               *string           `json:"base_url"`
	Protocol              *string           `json:"protocol"`
	ProviderClass         *string           `json:"provider_class"`
	APIKeyRef             *string           `json:"api_key_ref"`
	Trust                 *string           `json:"trust"`
	ConfirmTrustElevation bool              `json:"confirm_trust_elevation"`
	Locality              *string           `json:"locality"`
	Roles                 []string          `json:"roles"`
	ModelMap              json.RawMessage   `json:"model_map"`
	Timeouts              map[string]int    `json:"timeouts"`
	NumCtx                *int              `json:"num_ctx"`
	Priority              *int              `json:"priority"`
	Enabled               *bool             `json:"enabled"`
	ExtraHeaders          map[string]string `json:"extra_headers"`
	ExtraBody             map[string]any    `json:"extra_body"`
	Limits                map[string]any    `json:"limits"`
	Metadata              map[string]any    `json:"metadata"`
	Probe                 string            `json:"probe"` // backend-test only
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
		"locality": b.Locality, "roles": b.Roles, "model_map": b.ModelMap,
		"timeouts": b.Timeouts, "num_ctx": b.NumCtx, "priority": b.Priority,
		"enabled": b.Enabled, "extra_headers": b.ExtraHeaders,
		"extra_body": b.ExtraBody, "limits": b.Limits, "metadata": b.Metadata,
	}
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
	if prev == nil {
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
	found, err := store.UpdateBackend(ctx, tx, &next, by)
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
	name, found, err := store.DeleteBackend(ctx, tx, req.ID, by)
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

func (h *ManageHandler) handleBackendList(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, _ manageRequest) {
	// Rows + live status merged (admin-gated). last_error is the sanitized
	// ErrClass — full URLs/provider bodies exist in slog only (§2.9).
	snap := h.backendPool.Snapshot()
	status := h.backendPool.Status()
	statusByID := make(map[string]backends.BackendStatus, len(status))
	for _, s := range status {
		statusByID[s.ID] = s
	}
	list := make([]map[string]any, 0, len(snap))
	for i := range snap {
		v := backendView(&snap[i])
		if s, ok := statusByID[snap[i].ID]; ok {
			v["effective_state"] = s.EffectiveState
			v["cooldown_remaining_s"] = s.CooldownRemaining
			v["consecutive_fails"] = s.ConsecutiveFails
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
// OpenRouter detail checks (credits, zdr endpoint count) arrive with G29.
func (h *ManageHandler) handleBackendTest(w http.ResponseWriter, r *http.Request, _ *auth.AuthResult, req manageRequest) {
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
		probeChat(ctx, b, checks)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"success": true, "reachable": reachable,
		"latency_ms": latency, "checks": checks,
	})
}

func probeBackendURL(ctx context.Context, b *backends.Backend, checks map[string]string) bool {
	url := b.Host
	switch b.ProviderClass {
	case backends.ProviderLlamaCpp:
		url += "/health"
	case backends.ProviderOpenRouter:
		url += "/key"
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

func probeChat(ctx context.Context, b *backends.Backend, checks map[string]string) {
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
	_, err := chatProbe(ctx, probe)
	key := "model_" + spec.Model
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

// chatProbe is indirected for tests.
var chatProbe = func(ctx context.Context, b backends.Backend) (any, error) {
	return llm.Chat(ctx, b, "You are a connectivity probe.", "Reply with: ok",
		llm.Options{Temperature: 0, NumPredict: 1}, 15*time.Second)
}
