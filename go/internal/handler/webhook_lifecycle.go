// POST/DELETE /api/project/{id}/webhook-secret — the webhook HMAC secret
// lifecycle (workflow W13, design/03-workflow-api-cli.md §4.2/§5.6). ONE scope,
// ONE name, ONE verwaltungspfad:
//
//   - Scope: the PROJECT scope, pinned SERVER-SIDE from the project row — NOT the
//     caller's writeScope. This is why the lifecycle is a dedicated endpoint and
//     NOT `ctx secrets set` / PUT /api/secrets: that path maps a tenant-admin to
//     its HomeScope (tenant_scope.go:20-28) and could never reach the project
//     scope, so the secret it wrote would be invisible to the verifier and
//     unrotatable by the tenant (§5.6).
//   - Name: server-fixed store.WebhookSecretName(id) = 'webhook.github.<id>'. NOT
//     caller-choosable (webhook_secret_ref is server-managed, §3.1/§4.2) — a free
//     name would let PATCH point the public endpoint at a foreign secret and turn
//     it into an online HMAC-verification oracle (§5.3).
//
// POST create/ROTATE: server-generated (crypto/rand, 32 bytes hex), sealed
// (AES-256-GCM at rest, AAD binds name+scope), reveal-ONCE in the response (the
// user pastes it into the GitHub webhook config). Rotation = another POST (new
// secret, old row overwritten). DELETE deactivates the webhook (drop the secret
// row + NULL the register ref). tenant-admin of the OWNING tenant; a foreign/
// absent project ⇒ 404 uniform.
package handler

import (
	"crypto/rand"
	"encoding/hex"
	"log/slog"
	"net/http"

	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// WebhookSecretHandler serves the webhook-secret lifecycle. openBox is a field so
// the integration test injects a key without mutating process env (same pattern
// as SecretsHandler).
type WebhookSecretHandler struct {
	pool    *pgxpool.Pool
	openBox func() (*sealbox.Box, error)
}

// NewWebhookSecretHandler wires the pool and the production sealbox factory.
func NewWebhookSecretHandler(pool *pgxpool.Pool) *WebhookSecretHandler {
	return &WebhookSecretHandler{pool: pool, openBox: sealbox.FromEnv}
}

// MountProjectWebhookSecret mounts POST+DELETE /api/project/{id}/webhook-secret
// under ONE RequireAdminOrTenantAdmin group (a missing gate is a missing route,
// §5.1). The gate ADMITS; each handler then enforces project ownership (the K-T1
// pairing). Mount inside the /api Auth group (server.go).
func MountProjectWebhookSecret(r chi.Router, h *WebhookSecretHandler) {
	r.Group(func(r chi.Router) {
		r.Use(RequireAdminOrTenantAdmin)
		r.Post("/api/project/{id}/webhook-secret", h.HandleCreate)
		r.Delete("/api/project/{id}/webhook-secret", h.HandleDelete)
	})
}

// HandleCreate implements POST — create or rotate the webhook secret, reveal-once.
func (h *WebhookSecretHandler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, ok := h.ownedProject(w, r)
	if !ok {
		return
	}

	box, err := h.openBox()
	if err != nil {
		// Secrets are optional until CTX_SECRETS_KEY exists (§3.6): configuration,
		// not a caller mistake — 503, the error names the env var only.
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"success": false, "error": "secrets unavailable: " + err.Error()})
		return
	}

	// Server-generated 32-byte secret, hex-encoded (64 chars) — the value the user
	// pastes into GitHub. crypto/rand: a webhook secret must be high-entropy so the
	// 120/min endpoint is not a brute-force target (§5.3).
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		h.fail(w, r, "webhook-secret: rand", err)
		return
	}
	plaintext := hex.EncodeToString(raw)

	name := store.WebhookSecretName(row.ID)
	nonce, ct, err := box.Seal(name, row.Scope, []byte(plaintext))
	if err != nil {
		h.fail(w, r, "webhook-secret: seal", err)
		return
	}

	// Persist the sealed secret AND the register ref in ONE tx: the column and the
	// secret row are consistent by construction (no half-configured project).
	tx, err := h.pool.Begin(ctx) //nolint:forbidigo // handgebaute Tx-Klammer, fällt in T03-4b (K27)
	if err != nil {
		h.fail(w, r, "webhook-secret: begin", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.fail(w, r, "webhook-secret: tx meta", err)
		return
	}
	if _, err := store.PutSecret(ctx, tx, name, row.Scope, nonce, ct, 1, actorID(r)); err != nil {
		h.fail(w, r, "webhook-secret: persist", err)
		return
	}
	if err := store.SetProjectWebhookSecretRef(ctx, tx, row.ID, name); err != nil {
		h.fail(w, r, "webhook-secret: set ref", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.fail(w, r, "webhook-secret: commit", err)
		return
	}

	// Reveal-ONCE: the plaintext appears here and NOWHERE else (never in the row,
	// never in a GET, never in a log). deliver_to = the GitHub webhook config.
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "secret": plaintext, "name": name})
}

// HandleDelete implements DELETE — deactivate the webhook (drop the secret row +
// NULL the register ref, ONE tx). Idempotent: an already-absent secret still
// returns success (the register ref is nulled regardless).
func (h *WebhookSecretHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	row, ok := h.ownedProject(w, r)
	if !ok {
		return
	}
	tx, err := h.pool.Begin(ctx) //nolint:forbidigo // handgebaute Tx-Klammer, fällt in T03-4b (K27)
	if err != nil {
		h.fail(w, r, "webhook-secret: delete begin", err)
		return
	}
	defer func() { _ = tx.Rollback(ctx) }()
	if err := store.SetTxRequestID(ctx, tx, RequestIDFromContext(ctx)); err != nil {
		h.fail(w, r, "webhook-secret: delete tx meta", err)
		return
	}
	if _, err := store.DeleteSecret(ctx, tx, store.WebhookSecretName(row.ID), row.Scope, actorID(r)); err != nil {
		h.fail(w, r, "webhook-secret: delete secret", err)
		return
	}
	if err := store.ClearProjectWebhookSecretRef(ctx, tx, row.ID); err != nil {
		h.fail(w, r, "webhook-secret: clear ref", err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		h.fail(w, r, "webhook-secret: delete commit", err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"success": true})
}

// ownedProject loads {id} and enforces tenant ownership (server-admin always,
// else the tenant-admin of the project's OWN tenant). A foreign/absent project ⇒
// 404 uniform (no existence oracle). ok=false means a response was written.
func (h *WebhookSecretHandler) ownedProject(w http.ResponseWriter, r *http.Request) (*store.ProjectRow, bool) {
	ctx := r.Context()
	ar := AuthResultFromContext(ctx)
	row, err := store.GetProjectByID(ctx, h.pool, chi.URLParam(r, "id"))
	if err != nil {
		internalProjectError(w, ctx, "webhook-secret: project load", err)
		return nil, false
	}
	if row == nil || ar == nil || !ownsProject(ar, row) {
		projectNotFound(w)
		return nil, false
	}
	return row, true
}

// fail logs the cause and answers a value-free 500 — the secret error paths never
// echo request or key material.
func (h *WebhookSecretHandler) fail(w http.ResponseWriter, r *http.Request, msg string, err error) {
	slog.Error(msg, "error", err, "request_id", RequestIDFromContext(r.Context()))
	writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
}
