//go:build integration

// W13 webhook inbound + secret-lifecycle gates (design/03-workflow-api-cli.md
// §7-W13, §5.3, §5.6). Every gate runs through the PRODUCTION handler chain
// (the webhook route + MountProjectWebhookSecret), so the 401/202/422/429 probes
// exercise exactly what server.go wires. Gates proven RED-then-GREEN:
//
//   - bad signature ⇒ 401 AND zero context_webhook_events rows (RED bei INSERT-vor-Verify);
//   - valid signature ⇒ 202, exactly 1 row; redelivery (same GUID) ⇒ still 1 row;
//   - flood-order: 200 unsigned ⇒ next signed still 202 (RED bei Rate-Limit-vor-Verify);
//   - unknown project ⇒ uniform 401 + STRUCTURAL constant-work (same lookup→resolve→hmac chain);
//   - secret rotation reveal-once (old secret 401 after rotate, new 202);
//   - foreign-scope secret listing does NOT contain the project's webhook secret;
//   - PATCH webhook_secret_ref ⇒ 422 (server-managed).
//
// Run: go test -tags=integration ./internal/handler/ -run TestWebhookW13 -count=1 -v
package handler

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/GottZ/ctx/internal/auth"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/sealbox"
	"github.com/GottZ/ctx/internal/store"
	"github.com/GottZ/ctx/internal/testdb"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// w13Key is a fixed 32-byte (64 hex) master key for the test sealbox.
const w13Key = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func w13Box(t *testing.T) *sealbox.Box {
	t.Helper()
	box, err := sealbox.New(w13Key, "")
	if err != nil {
		t.Fatalf("sealbox.New: %v", err)
	}
	return box
}

// w13Sign builds the X-Hub-Signature-256 header for body under secret.
func w13Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// w13Project provisions a tenant+scope+project and returns the row.
func w13Project(t *testing.T, pool *pgxpool.Pool, slug string) *store.ProjectRow {
	t.Helper()
	res, err := store.ProvisionProject(context.Background(), pool, store.ProvisionParams{
		Slug: slug, DisplayName: slug + "/repo", Scope: slug + ":main",
		Identity: "github:" + slug + "/repo",
	})
	if err != nil {
		t.Fatalf("provision %s: %v", slug, err)
	}
	return res.Project
}

// w13SecretRouter mounts MountProjectWebhookSecret with the box + ar injected.
func w13SecretRouter(pool *pgxpool.Pool, box *sealbox.Box, ar *auth.AuthResult) http.Handler {
	sh := NewWebhookSecretHandler(pool)
	sh.openBox = func() (*sealbox.Box, error) { return box, nil }
	r := chi.NewRouter()
	if ar != nil {
		r.Use(func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
				next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, ar)))
			})
		})
	}
	MountProjectWebhookSecret(r, sh)
	return r
}

// w13CreateSecret POSTs the webhook-secret endpoint and returns the reveal-once
// plaintext (fails the test on non-200 or a missing secret field).
func w13CreateSecret(t *testing.T, pool *pgxpool.Pool, box *sealbox.Box, ar *auth.AuthResult, projectID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/project/"+projectID+"/webhook-secret", nil)
	rec := httptest.NewRecorder()
	w13SecretRouter(pool, box, ar).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create secret: status %d (body=%s)", rec.Code, rec.Body.String())
	}
	var resp struct {
		Secret string `json:"secret"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("create secret unmarshal: %v", err)
	}
	if resp.Secret == "" {
		t.Fatalf("create secret: no reveal-once secret in body=%s", rec.Body.String())
	}
	return resp.Secret
}

// w13Inbound serves one POST to the webhook route through handler h.
func w13Inbound(h *WebhookGitHubHandler, projectID string, body []byte, sig, delivery, event string) *httptest.ResponseRecorder {
	r := chi.NewRouter()
	r.Post("/webhooks/github/{project_id}", h.HandleWebhook)
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github/"+projectID, bytes.NewReader(body))
	if sig != "" {
		req.Header.Set("X-Hub-Signature-256", sig)
	}
	if delivery != "" {
		req.Header.Set("X-GitHub-Delivery", delivery)
	}
	if event != "" {
		req.Header.Set("X-GitHub-Event", event)
	}
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)
	return rec
}

func w13TenantAdmin(tenantID, scope string) *auth.AuthResult {
	return &auth.AuthResult{IsValid: true, TenantID: tenantID, TenantRole: auth.RoleAdmin, ReadScopes: []string{scope}}
}

// w13Handler builds the inbound handler with the box + a config store at rate n.
func w13Handler(pool *pgxpool.Pool, box *sealbox.Box, rateLimit int) *WebhookGitHubHandler {
	c := &config.Config{}
	c.Project.Webhook.RateLimit = rateLimit
	h := NewWebhookGitHubHandler(pool, config.NewStore(c))
	h.openBox = func() (*sealbox.Box, error) { return box, nil }
	return h
}

func w13CountEvents(t *testing.T, pool *pgxpool.Pool, projectID string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM context_webhook_events WHERE project_id=$1::uuid`, projectID).Scan(&n); err != nil {
		t.Fatalf("count events: %v", err)
	}
	return n
}

func TestWebhookW13_Inbound_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := w13Box(t)

	proj := w13Project(t, pool, "w13in")
	admin := w13TenantAdmin(proj.TenantID, proj.Scope)
	secret := w13CreateSecret(t, pool, box, admin, proj.ID)
	h := w13Handler(pool, box, 120)
	body := []byte(`{"action":"opened","issue":{"number":1}}`)

	// GATE 1 — bad signature ⇒ 401 AND zero rows (RED bei INSERT-vor-Verify).
	t.Run("BadSignatureNoWrite", func(t *testing.T) {
		rec := w13Inbound(h, proj.ID, body, "sha256="+hex.EncodeToString(make([]byte, 32)), "d-bad", "issues")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("bad sig: status %d, want 401 (body=%s)", rec.Code, rec.Body.String())
		}
		if n := w13CountEvents(t, pool, proj.ID); n != 0 {
			t.Fatalf("bad sig wrote %d rows, want 0 (INSERT-vor-Verify)", n)
		}
	})

	// GATE 2+3 — valid ⇒ 202 + 1 row; redelivery same GUID ⇒ still 1 row.
	t.Run("ValidThenRedelivery", func(t *testing.T) {
		sig := w13Sign(secret, body)
		rec := w13Inbound(h, proj.ID, body, sig, "d-1", "issues")
		if rec.Code != http.StatusAccepted {
			t.Fatalf("valid: status %d, want 202 (body=%s)", rec.Code, rec.Body.String())
		}
		if n := w13CountEvents(t, pool, proj.ID); n != 1 {
			t.Fatalf("valid: %d rows, want 1", n)
		}
		// Redelivery: identical GUID ⇒ idempotent, still exactly 1 row.
		rec2 := w13Inbound(h, proj.ID, body, sig, "d-1", "issues")
		if rec2.Code != http.StatusAccepted {
			t.Fatalf("redelivery: status %d, want 202", rec2.Code)
		}
		if n := w13CountEvents(t, pool, proj.ID); n != 1 {
			t.Fatalf("redelivery: %d rows, want 1 (redelivery-idempotency)", n)
		}
	})
}

// GATE 4 — flood-order: many unsigned ⇒ next signed still 202. RED bei Rate-Limit-
// vor-Verify (an unsigned flood would fill the budget and 429 the legit delivery).
func TestWebhookW13_FloodOrder_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := w13Box(t)
	proj := w13Project(t, pool, "w13flood")
	admin := w13TenantAdmin(proj.TenantID, proj.Scope)
	secret := w13CreateSecret(t, pool, box, admin, proj.ID)
	h := w13Handler(pool, box, 120) // budget 120/min; 200 unsigned would trip IF counted

	body := []byte(`{"ping":true}`)
	for i := 0; i < 200; i++ {
		rec := w13Inbound(h, proj.ID, body, "sha256="+hex.EncodeToString(make([]byte, 32)), fmt.Sprintf("flood-%d", i), "ping")
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("unsigned #%d: status %d, want 401", i, rec.Code)
		}
	}
	if n := w13CountEvents(t, pool, proj.ID); n != 0 {
		t.Fatalf("unsigned flood wrote %d rows, want 0", n)
	}
	// The next SIGNED delivery must still be accepted — the unsigned flood never
	// counted toward the per-project budget (Rate-Limit AFTER Verify).
	rec := w13Inbound(h, proj.ID, body, w13Sign(secret, body), "flood-signed", "ping")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("signed after flood: status %d, want 202 (Rate-Limit-vor-Verify RED)", rec.Code)
	}
}

// GATE 5 — unknown project ⇒ uniform 401 + STRUCTURAL constant-work: the unknown-
// project path and the valid path walk the SAME lookup→resolve_secret→hmac chain.
func TestWebhookW13_UnknownProjectConstantWork_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := w13Box(t)
	proj := w13Project(t, pool, "w13ct")
	admin := w13TenantAdmin(proj.TenantID, proj.Scope)
	secret := w13CreateSecret(t, pool, box, admin, proj.ID)
	h := w13Handler(pool, box, 120)

	var stages []string
	h.stages = func(s string) { stages = append(stages, s) }
	prefix := []string{"lookup", "resolve_secret", "hmac"}

	// Unknown project (a well-formed UUID that does not exist) ⇒ 401.
	unknownID := "019f2299-0000-7000-9000-000000000000"
	stages = nil
	rec := w13Inbound(h, unknownID, []byte(`{}`), "sha256=deadbeef", "u-1", "issues")
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown project: status %d, want 401", rec.Code)
	}
	unknownStages := append([]string(nil), stages...)
	for i, s := range prefix {
		if i >= len(unknownStages) || unknownStages[i] != s {
			t.Fatalf("unknown path stages %v do not start with %v (structural constant-work broken)", unknownStages, prefix)
		}
	}
	if n := w13CountEvents(t, pool, proj.ID); n != 0 {
		t.Fatalf("unknown project wrote %d rows to a real project, want 0", n)
	}

	// Valid path walks the SAME prefix chain.
	body := []byte(`{"ok":true}`)
	stages = nil
	rec = w13Inbound(h, proj.ID, body, w13Sign(secret, body), "v-1", "issues")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid: status %d, want 202", rec.Code)
	}
	for i, s := range prefix {
		if i >= len(stages) || stages[i] != s {
			t.Fatalf("valid path stages %v do not start with %v", stages, prefix)
		}
	}
}

// GATE 6 — secret lifecycle: rotation reveal-once (old fails, new works),
// foreign-scope listing empty, PATCH webhook_secret_ref ⇒ 422.
func TestWebhookW13_SecretLifecycle_Integration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}
	pool := testdb.SetupTestDB(t)
	box := w13Box(t)
	proj := w13Project(t, pool, "w13sec")
	admin := w13TenantAdmin(proj.TenantID, proj.Scope)
	h := w13Handler(pool, box, 120)
	body := []byte(`{"x":1}`)

	// Create → sign with it → 202.
	s1 := w13CreateSecret(t, pool, box, admin, proj.ID)
	if rec := w13Inbound(h, proj.ID, body, w13Sign(s1, body), "s-1", "issues"); rec.Code != http.StatusAccepted {
		t.Fatalf("s1 delivery: status %d, want 202", rec.Code)
	}

	// ROTATE (another POST) → reveal a DIFFERENT secret; the OLD one now 401, the NEW one 202.
	s2 := w13CreateSecret(t, pool, box, admin, proj.ID)
	if s2 == s1 {
		t.Fatalf("rotation returned the same secret — not rotated")
	}
	if rec := w13Inbound(h, proj.ID, body, w13Sign(s1, body), "s-old", "issues"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("old secret after rotate: status %d, want 401", rec.Code)
	}
	if rec := w13Inbound(h, proj.ID, body, w13Sign(s2, body), "s-new", "issues"); rec.Code != http.StatusAccepted {
		t.Fatalf("new secret after rotate: status %d, want 202", rec.Code)
	}

	// Reveal-once: the secret lives ONLY in the project scope, never on the row.
	var ref *string
	if err := pool.QueryRow(context.Background(),
		`SELECT webhook_secret_ref FROM context_projects WHERE id=$1::uuid`, proj.ID).Scan(&ref); err != nil {
		t.Fatalf("load ref: %v", err)
	}
	if ref == nil || *ref != store.WebhookSecretName(proj.ID) {
		t.Fatalf("webhook_secret_ref = %v, want %q", ref, store.WebhookSecretName(proj.ID))
	}

	// Foreign-scope listing does NOT contain the webhook secret (the `ctx secrets`
	// surface scopes to the caller's own scope — a foreign scope never enumerates it).
	other := w13Project(t, pool, "w13other")
	metasForeign, err := store.ListSecretMeta(context.Background(), pool, other.Scope)
	if err != nil {
		t.Fatalf("list foreign: %v", err)
	}
	for _, m := range metasForeign {
		if m.Name == store.WebhookSecretName(proj.ID) {
			t.Fatalf("foreign scope %q lists the project's webhook secret — scope leak", other.Scope)
		}
	}
	// Own scope DOES list it (sanity: the secret exists where it belongs).
	metasOwn, err := store.ListSecretMeta(context.Background(), pool, proj.Scope)
	if err != nil {
		t.Fatalf("list own: %v", err)
	}
	found := false
	for _, m := range metasOwn {
		if m.Name == store.WebhookSecretName(proj.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("own scope does not list its own webhook secret")
	}

	// PATCH webhook_secret_ref ⇒ 422 (server-managed). Through the production
	// MountProject chain, so this is the same gate server.go wires.
	pr := chi.NewRouter()
	pr.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, rq *http.Request) {
			next.ServeHTTP(w, rq.WithContext(context.WithValue(rq.Context(), authResultKey, admin)))
		})
	})
	MountProject(pr, NewProjectHandler(pool))
	patchBody, _ := json.Marshal(map[string]any{"webhook_secret_ref": "webhook.github.evil"})
	preq := httptest.NewRequest(http.MethodPatch, "/api/project/"+proj.ID, bytes.NewReader(patchBody))
	preq.Header.Set("Content-Type", "application/json")
	prec := httptest.NewRecorder()
	pr.ServeHTTP(prec, preq)
	if prec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("PATCH webhook_secret_ref: status %d, want 422 (body=%s)", prec.Code, prec.Body.String())
	}

	// DELETE webhook-secret ⇒ deactivate: the row ref clears and the secret no longer verifies.
	dreq := httptest.NewRequest(http.MethodDelete, "/api/project/"+proj.ID+"/webhook-secret", nil)
	drec := httptest.NewRecorder()
	w13SecretRouter(pool, box, admin).ServeHTTP(drec, dreq)
	if drec.Code != http.StatusOK {
		t.Fatalf("delete secret: status %d, want 200", drec.Code)
	}
	if rec := w13Inbound(h, proj.ID, body, w13Sign(s2, body), "s-after-del", "issues"); rec.Code != http.StatusUnauthorized {
		t.Fatalf("delivery after secret delete: status %d, want 401 (webhook deactivated)", rec.Code)
	}
}
