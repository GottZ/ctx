// /api/secrets — the F2-W6 write-only secrets API (design 02-§3.4).
//
// GET    /api/secrets         metadata only — names, key_version, timestamps,
//                             referenced_by. NEVER values, NEVER fingerprints
//                             (§7.5: a hash prefix is an offline oracle).
// PUT    /api/secrets/{name}  create or rotate; the value travels in the body
//                             and NEVER appears in any response or log
// DELETE /api/secrets/{name}  409 while a secret_ref setting references it;
//                             otherwise delete + reload (revocation must
//                             empty the snapshot immediately)
//
// All routes are admin-gated (MountSecrets, same negative-probe pattern as
// the settings routes). Rotation propagation: PUT/DELETE call settings.Reload
// after commit — a rotated provider key must reach the live snapshot WITHOUT
// a settings write (rotation IS the incident-response path; the W6 gate pins
// this red/green). The 051 secrets triggers additionally NOTIFY, covering
// psql edits through the same listener as settings rows.

package handler

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// SecretsHandler implements the /api/secrets routes.
type SecretsHandler struct {
	pool *pgxpool.Pool
	cfg  *config.Store
	// reload is settings.Reload bound to (pool, cfg); a function field so the
	// rotation-propagation negative probe can sever exactly this link.
	reload func(r *http.Request) error
	// openBox builds the sealbox from env; a field so tests can inject keys
	// without mutating process env.
	openBox func() (*sealbox.Box, error)
}

// NewSecretsHandler creates a new SecretsHandler.
func NewSecretsHandler(pool *pgxpool.Pool, cfg *config.Store) *SecretsHandler {
	h := &SecretsHandler{pool: pool, cfg: cfg, openBox: sealbox.FromEnv}
	h.reload = func(r *http.Request) error {
		return settings.Reload(r.Context(), pool, cfg)
	}
	return h
}

// MountSecrets mounts the /api/secrets routes behind RequireAdminOrTenantAdmin
// (03-W5): a tenant-admin manages its OWN tenant secrets, an operator manages
// _global. The handler scopes every read/write to writeScope (never enumerating
// foreign scopes, S5) — the looser gate only ADMITS. One function for server.go
// and the gate tests (same rationale as MountSettings: the 403 probe must
// exercise the production chain).
func MountSecrets(r chi.Router, h *SecretsHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Get("/api/secrets", h.HandleList)
		r.Put("/api/secrets/{name}", h.HandlePut)
		r.Delete("/api/secrets/{name}", h.HandleDelete)
	})
}

// secretView is one row of GET /api/secrets: SecretMeta plus the settings
// keys whose secret_ref currently points at it.
type secretView struct {
	store.SecretMeta
	ReferencedBy []string `json:"referenced_by"`
}

// HandleList implements GET /api/secrets — metadata only. Scoped to writeScope
// (S5): a tenant-admin lists ONLY its own secrets, an operator _global — never
// enumerating a foreign tenant's provider topology.
func (h *SecretsHandler) HandleList(w http.ResponseWriter, r *http.Request) {
	metas, err := store.ListSecretMeta(r.Context(), h.pool, writeScope(AuthResultFromContext(r.Context())))
	if err != nil {
		h.fail(w, r, "secrets: list failed", err)
		return
	}
	refs, err := h.referencedBy(r)
	if err != nil {
		h.fail(w, r, "secrets: reference scan failed", err)
		return
	}
	views := make([]secretView, 0, len(metas))
	for _, m := range metas {
		rb := refs[m.Name]
		if rb == nil {
			rb = []string{}
		}
		views = append(views, secretView{SecretMeta: m, ReferencedBy: rb})
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "secrets": views})
}

// HandlePut implements PUT /api/secrets/{name} — create or rotate. The
// response carries action and name, NEVER the value (response-scan-pinned).
func (h *SecretsHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if !store.ValidSecretName(name) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": "invalid secret name (lowercase [a-z0-9._-], max 128 chars)"})
		return
	}
	var body struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid JSON body"})
		return
	}
	if body.Value == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"success": false, "error": "value is required (write-only: set it here, read it never)"})
		return
	}

	box, err := h.openBox()
	if err != nil {
		// Configuration, not a caller mistake: secrets are optional until
		// CTX_SECRETS_KEY exists (§3.6). The error names the env var only.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"success": false, "error": fmt.Sprintf("secrets unavailable: %v", err)})
		return
	}
	// Seal under writeScope: the AAD binds name+scope, so the ciphertext is
	// cryptographically valid ONLY on the tenant's own row (§5.3). NEVER _global
	// for a tenant — that would cement the secret in the operator namespace.
	scope := writeScope(AuthResultFromContext(r.Context()))
	nonce, ct, err := box.Seal(name, scope, []byte(body.Value))
	if err != nil {
		h.fail(w, r, "secrets: seal failed", err)
		return
	}

	created, err := h.putSealed(r, name, nonce, ct, scope)
	if err != nil {
		h.fail(w, r, "secrets: persist failed", err)
		return
	}
	// Rotation propagation (§3.4): without this reload, the OLD plaintext of
	// a just-rotated provider key lives on in the snapshot until some
	// unrelated settings write — the incident-response path would be silently
	// inert. Negatively probed in the integration test.
	if err := h.reload(r); err != nil {
		h.fail(w, r, "secrets: persisted but reload failed", err)
		return
	}

	action := "rotate"
	if created {
		action = "create"
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "name": name, "action": action})
}

// HandleDelete implements DELETE /api/secrets/{name}. A referenced secret is
// a 409 with the referencing keys — deleting it would silently flip those
// settings to fail-open. An unreferenced delete reloads: revocation must
// remove the plaintext from the snapshot immediately.
func (h *SecretsHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	refs, err := h.referencedBy(r)
	if err != nil {
		h.fail(w, r, "secrets: reference scan failed", err)
		return
	}
	if keys := refs[name]; len(keys) > 0 {
		writeJSON(w, http.StatusConflict, map[string]any{
			"success": false,
			"error":   fmt.Sprintf("secret %q is referenced by settings — unset them first (DELETE /api/settings/{key})", name),
			"referenced_by": keys,
		})
		return
	}

	found, err := h.deleteSealed(r, name, writeScope(AuthResultFromContext(r.Context())))
	if err != nil {
		h.fail(w, r, "secrets: delete failed", err)
		return
	}
	if !found {
		writeJSON(w, http.StatusNotFound, map[string]any{
			"success": false, "error": fmt.Sprintf("no secret named %q", name)})
		return
	}
	if err := h.reload(r); err != nil {
		h.fail(w, r, "secrets: deleted but reload failed", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "name": name, "deleted": true})
}

// referencedBy maps secret name → settings keys whose override row is a
// secret_ref to it. Source of truth are the override ROWS (the snapshot
// carries resolved plaintext, useless for reference counting).
//
// Scope (§5.7, the reference-integrity barrier — NOT a crypto check, so the AAD
// is irrelevant here): a tenant-admin's DELETE scans its OWN scope. An operator's
// _global DELETE scans _global AND every opt-in tenant scope — a _global secret
// can be referenced VIA the gated fallback by an opted-in tenant's setting, and
// missing that reference would let the tenant setting silently fail-open to
// env/default. Without any opt-in tenant the set degenerates to today's
// _global-only scan.
func (h *SecretsHandler) referencedBy(r *http.Request) (map[string][]string, error) {
	ws := writeScope(AuthResultFromContext(r.Context()))
	scopes := []string{ws}
	if ws == store.GlobalScope {
		optIn, err := store.OptInTenantScopes(r.Context(), h.pool)
		if err != nil {
			return nil, err
		}
		scopes = append(scopes, optIn...)
	}

	rows, err := store.LoadSettingOverridesMulti(r.Context(), h.pool, scopes)
	if err != nil {
		return nil, err
	}
	sensitive := map[string]bool{}
	for _, info := range config.Keys() {
		if info.Sensitive {
			sensitive[info.Key] = true
		}
	}
	refs := map[string][]string{}
	seen := map[string]map[string]bool{} // name → set of keys (dedup across scopes)
	for _, row := range rows {
		if !sensitive[row.Key] {
			continue
		}
		name, err := settings.ScalarValue(row.Value)
		if err != nil {
			continue
		}
		if seen[name] == nil {
			seen[name] = map[string]bool{}
		}
		if seen[name][row.Key] {
			continue
		}
		seen[name][row.Key] = true
		refs[name] = append(refs[name], row.Key)
	}
	return refs, nil
}

// putSealed persists one sealed secret in an attributed transaction. scope is
// writeScope(ar) — the tenant's own scope or operator _global.
func (h *SecretsHandler) putSealed(r *http.Request, name string, nonce, ct []byte, scope string) (bool, error) {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return false, err
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(r.Context(), tx, RequestIDFromContext(r.Context())); err != nil {
		return false, err
	}
	created, err := store.PutSecret(r.Context(), tx, name, scope, nonce, ct, 1, actorID(r))
	if err != nil {
		return false, err
	}
	return created, tx.Commit(r.Context())
}

// deleteSealed removes one sealed secret in an attributed transaction. scope is
// writeScope(ar) — a tenant-admin deletes only its own row.
func (h *SecretsHandler) deleteSealed(r *http.Request, name, scope string) (bool, error) {
	tx, err := h.pool.Begin(r.Context())
	if err != nil {
		return false, err
	}
	defer tx.Rollback(r.Context()) //nolint:errcheck // rollback after commit is a no-op
	if err := store.SetTxRequestID(r.Context(), tx, RequestIDFromContext(r.Context())); err != nil {
		return false, err
	}
	found, err := store.DeleteSecret(r.Context(), tx, name, scope, actorID(r))
	if err != nil {
		return false, err
	}
	return found, tx.Commit(r.Context())
}

// fail logs the cause and answers a value-free 500 — secrets error paths
// never echo request material.
func (h *SecretsHandler) fail(w http.ResponseWriter, r *http.Request, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(r.Context()))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
