// GET /api/whoami — key identity for the SPA UI gate (F4-W3).
//
// Reads the AuthResult the Auth middleware put into the request context and
// resolves the key's label with a single context_api_keys lookup. The wire
// shape is pinned by TestWhoamiGoldenShape and mirrored by the hand-maintained
// TS type WhoamiResponse (go/web/src/lib/api/types.ts) per design 04-§2.5.

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
}

// WhoamiHandler handles GET /api/whoami.
type WhoamiHandler struct {
	pool *pgxpool.Pool
	// labelByKeyID resolves the key's label; a function field so unit tests
	// can run the handler without a database (-short).
	labelByKeyID func(ctx context.Context, apiKeyID string) (string, error)
}

// NewWhoamiHandler creates a new WhoamiHandler.
func NewWhoamiHandler(pool *pgxpool.Pool) *WhoamiHandler {
	h := &WhoamiHandler{pool: pool}
	h.labelByKeyID = h.labelFromDB
	return h
}

func (h *WhoamiHandler) labelFromDB(ctx context.Context, apiKeyID string) (string, error) {
	var label string
	err := h.pool.QueryRow(ctx,
		`SELECT label FROM context_api_keys WHERE id = $1`, apiKeyID,
	).Scan(&label)
	return label, err
}

// HandleWhoami returns the calling key's identity: label, home scope, read
// scopes, the server-global admin flag and the Modell-C tenant identity
// (tenant_id + per-tenant role) — everything the SPA needs for its UI gate.
//
// TODO(multi-tenant): the label lookup is still server-global (a bare
// context_api_keys read), not scoped per tenant — the remaining seam once
// per-tenant label visibility matters.
func (h *WhoamiHandler) HandleWhoami(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		// Defense in depth — the Auth group middleware already rejects these.
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "unauthorized"})
		return
	}

	label, err := h.labelByKeyID(r.Context(), ar.ApiKeyID)
	if err != nil {
		slog.Error("whoami: label lookup failed",
			"error", err,
			"request_id", RequestIDFromContext(r.Context()),
		)
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"success": false, "error": "internal error",
		})
		return
	}

	writeJSON(w, http.StatusOK, whoamiResponse{
		Success:    true,
		Label:      label,
		HomeScope:  ar.HomeScope,
		ReadScopes: ar.ReadScopes,
		Admin:      ar.IsAdmin,
		TenantID:   ar.TenantID,
		Role:       string(ar.TenantRole),
	})
}
