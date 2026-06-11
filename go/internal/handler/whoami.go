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
// scopes and the admin tier flag — everything the SPA needs for its UI gate.
//
// TODO(multi-tenant): the response is the flat single-server key view — admin
// is the server-global 052 tier and label/home_scope come straight from
// context_api_keys. A tenant-aware deployment adds the owning tenant identity
// and a per-tenant role here (and scopes the label lookup per tenant) so the
// SPA gate stops conflating "server admin" with "tenant admin".
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
	})
}
