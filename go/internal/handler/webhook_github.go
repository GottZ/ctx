// POST /webhooks/github/{project_id} — the GitHub inbound surface (workflow W13,
// design/03-workflow-api-cli.md §3.4/§4.4/§5.3). This is the FIRST and only
// UNAUTHENTICATED write surface: GitHub must POST here, so there is no api key —
// HMAC-SHA256 over the raw body against the PER-PROJECT sealed secret takes the
// place of Auth(pool). It is therefore mounted OUTSIDE the /api Auth group
// (server.go), with only the 1 MB body cap in front of it.
//
// VERBINDLICHE PRÜF-REIHENFOLGE (§5.3 — the order itself is part of the security
// model, each step fail-closed):
//
//	Body-Cap → Projekt-Lookup → HMAC-Verify → Rate-Limit → INSERT → 202
//
//   - Signature BEFORE any DB write: a forged/absent signature is a 401 and
//     ZERO rows touch context_webhook_events (rot bei INSERT-vor-Verify).
//   - Rate-Limit AFTER Verify: the per-project budget counts ONLY signature-valid
//     deliveries, so an unsigned flood can never push a legitimate GitHub
//     delivery into 429 (Denial-of-Sync). 200 unsigned requests ⇒ next signed
//     delivery still 202 (rot bei Rate-Limit-vor-Verify).
//   - Unknown/unconfigured project ⇒ uniform 401 with STRUCTURELLE Konstant-
//     Arbeit: the same lookup→resolve-secret→hmac chain runs against a process-
//     constant random secret (no response oracle; timing angenähert, nicht
//     bewiesen — §5.3 honest claim).
//
// Events are TRIGGER, nie Autorität (§5.3): the INSERT only queues the delivery;
// the scheduler inbox arm debounces + fires a forge sync whose translator applies
// the 3-way content hash. A replay (new GUID, old payload) therefore changes NO
// block state — proven by the W13 replay gate, not by this handler.
package handler

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// webhookRateWindow is the fixed counting window for webhook.rate_limit ("120/min
// pro Projekt", §4.4): the config key carries the COUNT, the window is 60 s.
// Per-project via context_webhook_events + idx_webhook_project_recent (§3.4).
const webhookRateWindow = 60 * time.Second

// WebhookGitHubHandler serves POST /webhooks/github/{project_id}. dummySecret is
// a process-constant random key (crypto/rand, 32 bytes, minted once at
// construction) — the unknown/unconfigured-project path HMACs against it so the
// verify chain does structurally identical work to the valid path (§5.3
// constant-work). openBox builds the sealbox from env (field so tests inject
// keys). stages is a nil-in-production probe the W13 structural test sets to
// record the chain the request walked.
type WebhookGitHubHandler struct {
	pool        *pgxpool.Pool
	cfg         *config.Store
	openBox     func() (*sealbox.Box, error)
	dummySecret []byte
	stages      func(string)
}

// NewWebhookGitHubHandler mints the per-process dummy secret and wires the
// production sealbox factory.
func NewWebhookGitHubHandler(pool *pgxpool.Pool, cfg *config.Store) *WebhookGitHubHandler {
	dummy := make([]byte, 32)
	if _, err := rand.Read(dummy); err != nil {
		// A CSPRNG failure at boot is fatal-class; fall back to a fixed non-empty
		// key so the constant-work path still HMACs (an attacker cannot know it,
		// and no valid caller ever uses it — a NULL project has no real secret).
		for i := range dummy {
			dummy[i] = 0x5a
		}
	}
	return &WebhookGitHubHandler{pool: pool, cfg: cfg, openBox: sealbox.FromEnv, dummySecret: dummy}
}

// stage records a chain step for the structural constant-work probe (no-op in
// production).
func (h *WebhookGitHubHandler) stage(s string) {
	if h.stages != nil {
		h.stages(s)
	}
}

// HandleWebhook implements the inbound POST. See the file header for the fail-
// closed order. It never leaks whether a project exists or a secret is configured
// (uniform 401), never writes a row before the HMAC passes, and never returns the
// secret material.
func (h *WebhookGitHubHandler) HandleWebhook(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// 1 — Body-Cap. The MaxBodySize middleware wrapped r.Body in a MaxBytesReader
	// (1 MB, server.go); ReadAll fails past the cap. A body error is a 400 BEFORE
	// any lookup — the cheapest possible rejection.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "body unreadable or too large"})
		return
	}

	// 2 — Projekt-Lookup. A real DB error is a 500 (infra, not attacker-shaped); an
	// unknown/malformed id collapses to (nil,nil) → the constant-work reject path.
	projectID := chi.URLParam(r, "project_id")
	row, err := store.GetProjectByID(ctx, h.pool, projectID)
	if err != nil {
		slog.Error("webhook: project lookup", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	h.stage("lookup")

	// 3 — Resolve the verification secret (real for a configured project, the
	// process-constant dummy otherwise) then HMAC — the SAME two steps on both
	// paths (structural constant-work, §5.3). A configured-but-unresolvable secret
	// falls back to the dummy so the answer stays a uniform 401 (no misconfig
	// oracle), logged for the operator.
	secret := h.resolveSecret(ctx, row)
	h.stage("resolve_secret")
	valid := verifyGitHubSignature(secret, body, r.Header.Get("X-Hub-Signature-256"))
	h.stage("hmac")

	// Reject: unknown project OR bad signature ⇒ uniform 401, ZERO DB writes.
	if row == nil || !valid {
		h.stage("reject")
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	// 4 — Rate-Limit (AFTER verify: counts only signature-valid deliveries). Fail-
	// closed: a count error is a 500, never a silently-unmetered accept. Tenant
	// override honored via the project's scope (the path has no auth context).
	limit := h.cfg.SnapshotForTenant(ctx, row.Scope).Project.Webhook.RateLimit //nolint:forbidigo // W13: the unauthenticated webhook path resolves the tenant from the project scope, not the (absent) auth result — SnapshotForTenant is the correct per-tenant read here.
	if limit > 0 {
		count, err := store.CountWebhookEventsSince(ctx, h.pool, row.ID, webhookRateWindow)
		if err != nil {
			slog.Error("webhook: rate count", "error", err, "request_id", RequestIDFromContext(ctx))
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
			return
		}
		if count >= limit {
			h.stage("ratelimit")
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "webhook rate limit exceeded", "retry_after_s": int(webhookRateWindow.Seconds())})
			return
		}
	}

	// 5 — INSERT (redelivery-idempotent). A missing delivery id is a 400 (GitHub
	// always sends X-GitHub-Delivery; its absence is a malformed request, and we
	// are past the HMAC so this is no oracle). A non-JSON body past a VALID
	// signature is likewise a 400 — the ::jsonb cast would otherwise 500.
	delivery := strings.TrimSpace(r.Header.Get("X-GitHub-Delivery"))
	if delivery == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "missing X-GitHub-Delivery"})
		return
	}
	if !json.Valid(body) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "payload is not valid JSON"})
		return
	}
	event := strings.TrimSpace(r.Header.Get("X-GitHub-Event"))
	if _, err := store.InsertWebhookEvent(ctx, h.pool, row.ID, delivery, event, body); err != nil {
		slog.Error("webhook: insert", "error", err, "request_id", RequestIDFromContext(ctx))
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	h.stage("insert")
	// 202 Accepted: annahme != Verarbeitung (the scheduler inbox arm processes
	// asynchronously; the GitHub 10-s timeout never waits on LLM/embed work).
	writeJSON(w, http.StatusAccepted, map[string]any{"success": true})
}

// resolveSecret returns the HMAC key for the request: the project's sealed
// webhook secret when the project exists AND has one configured, else the
// process-constant dummy (unknown project, no webhook enabled, or an unresolvable
// secret). Returning the dummy — never an error to the caller — keeps the reject
// path a uniform 401 (§5.3 no oracle).
func (h *WebhookGitHubHandler) resolveSecret(ctx context.Context, row *store.ProjectRow) []byte {
	if row == nil || row.WebhookSecretRef == nil || *row.WebhookSecretRef == "" {
		return h.dummySecret
	}
	box, err := h.openBox()
	if err != nil {
		slog.Error("webhook: sealbox unavailable", "error", err, "request_id", RequestIDFromContext(ctx))
		return h.dummySecret
	}
	pt, err := store.ResolveSecret(ctx, h.pool, box, *row.WebhookSecretRef, row.Scope)
	if err != nil {
		slog.Error("webhook: secret resolve failed", "project", row.ID, "request_id", RequestIDFromContext(ctx))
		return h.dummySecret
	}
	return pt
}

// verifyGitHubSignature checks the X-Hub-Signature-256 header ('sha256=<hex>')
// against HMAC-SHA256(secret, body). Comparison is constant-time (hmac.Equal). A
// missing/malformed header, wrong prefix, or non-hex digest all fail — no panic,
// no early return that shortcuts the MAC computation on the reject path.
func verifyGitHubSignature(secret, body []byte, header string) bool {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := mac.Sum(nil)

	const prefix = "sha256="
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	got, err := hex.DecodeString(header[len(prefix):])
	if err != nil || len(got) != len(expected) {
		return false
	}
	return hmac.Equal(got, expected)
}
