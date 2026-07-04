// GET /api/whoami — key identity for the SPA UI gate (F4-W3).
//
// Reads the AuthResult the Auth middleware put into the request context and
// resolves the key's label plus its owning tenant's slug + display name with a
// single context_api_keys ⋈ context_tenants lookup. The wire shape is pinned by
// TestWhoamiGoldenShape and mirrored by the hand-maintained TS type
// WhoamiResponse (go/web/src/lib/api/types.ts) per design 04-§2.5.

package handler

import (
	"context"
	"log/slog"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
)

// whoamiResponse is the GET /api/whoami wire envelope (design 04-§3.2).
type whoamiResponse struct {
	Success    bool     `json:"success"`
	Label      string   `json:"label"`
	HomeScope  string   `json:"home_scope"`
	ReadScopes []string `json:"read_scopes"`
	Admin      bool     `json:"admin"`
	// TenantID and Role carry the Modell-C tenant identity (060): the owning
	// tenant UUID and the key's per-tenant role (owner|admin|member, 059). They
	// are ORTHOGONAL to the server-global Admin flag (052) — the SPA gate needs
	// both to stop conflating "server admin" with "tenant admin". Appended after
	// the original five fields so existing consumers stay byte-compatible. Both
	// are populated for every request that reaches here; the sentinel paths that
	// leave them empty are stopped by the IsValid check below.
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
	// ApiKeyID is the UUID of the calling key itself (the already-authenticated
	// identity from ar.ApiKeyID — NOT a secret, neither the hash nor the
	// plaintext). The SPA uses it for the Self-Revoke-Schutz (design/05 §7.5 /
	// TK5): the row of the current key is marked so its revoke control can be
	// disabled, preventing a self-lockout. Additive read, no migration.
	ApiKeyID string `json:"api_key_id"`
	// TenantSlug and TenantDisplayName carry the owning tenant's human-readable
	// identity (059 context_tenants), resolved by a single LEFT JOIN on the
	// key's tenant_id (design/06 N9 / D1). They let the SPA header show a name
	// instead of the bare UUID. Additive read, no migration. They degrade to ""
	// when the key has no tenant row (NULL tenant_id or an unmapped tenant — the
	// LEFT JOIN yields NULL, scanned through nullable pointers); the default
	// tenant (059 backfill) always resolves, so this is only a defensive seam.
	TenantSlug        string `json:"tenant_slug"`
	TenantDisplayName string `json:"tenant_display_name"`
	// Capabilities carries feature flags that are DATA, not tier-derived (the
	// SPA derives tier caps itself from Admin/Role). workflow=true switches the
	// workflow UI (issues/board, U04 dark-launch cap viewWorkflow) visible; it
	// went GA with v4.3.0 (RC-2, user decision 2026-07-04) and is emitted
	// statically here — the field stays in the wire so a later per-tenant gate
	// (B9, tenant_limits-style) only changes THIS value, not the SPA. Additive.
	Capabilities whoamiCapabilities `json:"capabilities"`
}

// whoamiCapabilities is the whoami feature-flag bag (design 04-§3.2 B9).
type whoamiCapabilities struct {
	Workflow bool `json:"workflow"`
}

// whoamiIdentity is the per-key identity resolved from the DB in one round-trip:
// the key's label plus the owning tenant's human-readable slug + display name.
// The tenant fields are the additive N9/D1 read-enrichment (a single LEFT JOIN
// on context_tenants, no migration); they are empty when the key resolves to no
// tenant row.
type whoamiIdentity struct {
	label             string
	tenantSlug        string
	tenantDisplayName string
}

// WhoamiHandler handles GET /api/whoami.
type WhoamiHandler struct {
	pool *pgxpool.Pool
	// identityByKeyID resolves the key's label and its tenant's slug/display
	// name; a function field so unit tests can run the handler without a
	// database (-short).
	identityByKeyID func(ctx context.Context, apiKeyID string) (whoamiIdentity, error)
}

// NewWhoamiHandler creates a new WhoamiHandler.
func NewWhoamiHandler(pool *pgxpool.Pool) *WhoamiHandler {
	h := &WhoamiHandler{pool: pool}
	h.identityByKeyID = h.identityFromDB
	return h
}

// identityFromDB resolves the calling key's label and the owning tenant's
// human-readable slug + display name in a single LEFT JOIN keyed on the key's
// own tenant_id (equal to ar.TenantID). The join is LEFT so a key with a NULL
// tenant_id or a missing tenant row still returns its label; the tenant columns
// then scan as NULL through the nullable pointers and degrade to "".
func (h *WhoamiHandler) identityFromDB(ctx context.Context, apiKeyID string) (whoamiIdentity, error) {
	var id whoamiIdentity
	var slug, displayName *string
	err := h.pool.QueryRow(ctx,
		`SELECT k.label, t.slug, t.display_name
		   FROM context_api_keys k
		   LEFT JOIN context_tenants t ON t.id = k.tenant_id
		  WHERE k.id = $1`, apiKeyID,
	).Scan(&id.label, &slug, &displayName)
	if err != nil {
		return whoamiIdentity{}, err
	}
	if slug != nil {
		id.tenantSlug = *slug
	}
	if displayName != nil {
		id.tenantDisplayName = *displayName
	}
	return id, nil
}

// HandleWhoami returns the calling key's identity: label, home scope, read
// scopes, the server-global admin flag, the Modell-C tenant identity (tenant_id
// + per-tenant role), the key's own id (Self-Revoke-Schutz) and the owning
// tenant's slug + display name — everything the SPA needs for its UI gate.
//
// TODO(multi-tenant): the identity lookup is still server-global (a bare
// context_api_keys read, LEFT-joined to its tenant), not scoped per tenant —
// the remaining seam once per-tenant label visibility matters.
func (h *WhoamiHandler) HandleWhoami(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		// Defense in depth — the Auth group middleware already rejects these.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	id, err := h.identityByKeyID(r.Context(), ar.ApiKeyID)
	if err != nil {
		slog.Error("whoami: identity lookup failed",
			"error", err,
			"request_id", RequestIDFromContext(r.Context()),
		)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "internal error",
		})
		return
	}

	writeJSON(w, http.StatusOK, whoamiResponse{
		Success:           true,
		Label:             id.label,
		HomeScope:         ar.HomeScope,
		ReadScopes:        ar.ReadScopes,
		Admin:             ar.IsAdmin,
		TenantID:          ar.TenantID,
		Role:              string(ar.TenantRole),
		ApiKeyID:          ar.ApiKeyID,
		TenantSlug:        id.tenantSlug,
		TenantDisplayName: id.tenantDisplayName,
		Capabilities:      whoamiCapabilities{Workflow: true},
	})
}
