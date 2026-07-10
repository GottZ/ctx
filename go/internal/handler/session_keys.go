// Multi-Tenant-Key-/Tenant-Selektor (OAuth R6, design 05 §4.6):
//
//	GET  /api/session/keys       → aktive Keys des Session-Principals listen
//	POST /api/session/select-key → auf einen anderen eigenen Key wechseln
//
// Beide Routen leben INNERHALB der Auth-Gruppe (AuthResult kommt aus der
// Middleware; das CSRF-Synchronizer-Gate greift auf dem Cookie-Pfad bei
// POST automatisch). Der Wechsel mintet eine FRISCHE Token/Session-Familie
// auf den Ziel-Key und revoziert die alte — er mutiert NIE api_key_id einer
// bestehenden Row in-place: die Token-Row kann parallel als MCP-Bearer
// laufen (Universal-Credential, E2), ein In-Place-Umhängen wechselte deren
// Tenant still. INV-A: der Wechsel ersetzt EINEN Key durch EINEN anderen,
// unioniert NIE über die Keys eines Principals.
package handler

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/store"
)

// sessionKeyEntry ist eine Zeile der GET /api/session/keys Antwort. KEINE
// Hashes, KEINE Klartext-Keys im Wire — nur die Identitäts-Metadaten, die
// die SPA für den Tenant-Umschalter braucht.
type sessionKeyEntry struct {
	APIKeyID          string `json:"api_key_id"`
	Label             string `json:"label"`
	TenantSlug        string `json:"tenant_slug"`
	TenantDisplayName string `json:"tenant_display_name"`
	TenantRole        string `json:"tenant_role"`
	HomeScope         string `json:"home_scope"`
	// ActiveNow markiert den Key, auf dem die AKTUELLE Credential läuft
	// (== AuthResult.ApiKeyID) — die SPA disabled dessen Wechsel-Control.
	ActiveNow bool `json:"active_now"`
}

// HandleSessionKeys implementiert GET /api/session/keys: listet die AKTIVEN
// Keys des Session-Principals (read-only, harmlos auch am Header-Pfad —
// sinnvoll aber nur mit Cookie-Session). Attribution läuft über
// AuthResult.PrincipalID; ein Principal-loses AuthResult (Alt-Deployment
// ohne 095) → 400, nichts zu listen.
func (h *SessionHandler) HandleSessionKeys(w http.ResponseWriter, r *http.Request) {
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid {
		// Defense in depth — die Auth-Gruppe weist das bereits ab.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}
	if ar.PrincipalID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "no principal attached"})
		return
	}

	rows, err := h.pool.Query(r.Context(),
		`SELECT k.id::text, k.label,
		        COALESCE(t.slug, ''), COALESCE(t.display_name, ''),
		        COALESCE(k.tenant_role, ''), k.home_scope
		   FROM context_api_keys k
		   LEFT JOIN context_tenants t ON t.id = k.tenant_id
		  WHERE k.principal_id = $1::uuid AND k.active = true
		  ORDER BY k.created_at`,
		ar.PrincipalID,
	)
	if err != nil {
		slog.Error("session keys: list", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	defer rows.Close()

	keys := make([]sessionKeyEntry, 0, 4)
	for rows.Next() {
		var e sessionKeyEntry
		if err := rows.Scan(&e.APIKeyID, &e.Label, &e.TenantSlug, &e.TenantDisplayName, &e.TenantRole, &e.HomeScope); err != nil {
			slog.Error("session keys: scan", "error", err)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
			return
		}
		e.ActiveNow = e.APIKeyID == ar.ApiKeyID
		keys = append(keys, e)
	}
	if err := rows.Err(); err != nil {
		slog.Error("session keys: rows", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"success": true, "keys": keys})
}

// HandleSelectKey implementiert POST /api/session/select-key (05 §4.6):
// Tenant-Wechsel eines Multi-Tenant-Principals — NUR am Cookie-Pfad (die
// Session ist das, was gewechselt wird; ein Header-Credential-Request hat
// keine → 400). Ablauf mint-fresh, nie in-place:
//
//  1. Ziel-Key laden: active=true UND principal_id == Session-Principal,
//     sonst 403 (fremder Key; malformed UUID faltet in dieselbe 403 —
//     kein 22P02, kein Oracle zwischen „fremd" und „kaputt").
//  2. Neues MintTokenPair auf den Ziel-Key (Audiences wie Login: /mcp +
//     /web, issued_via='login'), neues csrf_secret, neue Overlay-Row.
//  3. ERST wenn die neue Session voll etabliert ist, stirbt die alte
//     Familie (DestroyWebSession) — schlägt das fehl, wird geloggt, nicht
//     abgebrochen: der User hat nie ein Fenster ganz ohne Session.
//
// Das CSRF-Gate der Auth-Middleware greift bei POST am Cookie-Pfad
// automatisch — dieser Handler sieht nur Requests mit gültigem Synchronizer.
func (h *SessionHandler) HandleSelectKey(w http.ResponseWriter, r *http.Request) {
	oldSessionID, isSession := requestCredential(r)
	if !isSession {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "cookie session required"})
		return
	}
	ar := AuthResultFromContext(r.Context())
	if ar == nil || !ar.IsValid || ar.PrincipalID == "" {
		// Defense in depth — die Auth-Gruppe weist das bereits ab.
		writeJSON(w, http.StatusUnauthorized, map[string]any{"success": false, "error": "unauthorized"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	var req struct {
		APIKeyID string `json:"api_key_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.APIKeyID) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "api_key_id required"})
		return
	}
	targetKeyID := strings.TrimSpace(req.APIKeyID)
	if uuid.Validate(targetKeyID) != nil {
		// Malformed faltet in die Fremder-Key-Antwort (kein 22P02-Leak).
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "key not selectable"})
		return
	}

	// (1) Ownership-Gate: der Ziel-Key muss aktiv sein UND demselben
	// Principal gehören. Fremd, revoked und unbekannt sind EINE 403.
	var ok bool
	err := h.pool.QueryRow(r.Context(),
		`SELECT true FROM context_api_keys
		  WHERE id = $1::uuid AND active = true AND principal_id = $2::uuid`,
		targetKeyID, ar.PrincipalID,
	).Scan(&ok)
	if errors.Is(err, pgx.ErrNoRows) {
		writeJSON(w, http.StatusForbidden, map[string]any{"success": false, "error": "key not selectable"})
		return
	}
	if err != nil {
		slog.Error("session select-key: ownership check", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	// (2) Mint-fresh auf den Ziel-Key — dieselbe Login-Prägung wie
	// HandleLogin (issued_via='login', beide Audiences), neue Familie.
	base := h.audienceBase(r)
	pair, err := store.MintTokenPair(r.Context(), h.pool, store.OAuthToken{
		APIKeyID:    targetKeyID,
		PrincipalID: ar.PrincipalID,
		ClientID:    "", // Web-Session hat keinen OAuth-Client (wie Login)
		Audiences:   []string{base + "/mcp", base + "/web"},
		IssuedVia:   "login",
	}, accessTokenTTL(), refreshTokenTTL())
	if err != nil {
		slog.Error("session select-key: mint pair", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	csrfBytes := make([]byte, 32)
	if _, err := rand.Read(csrfBytes); err != nil {
		slog.Error("session select-key: csrf rand", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}
	csrfSecret := hex.EncodeToString(csrfBytes)

	newSessionID, err := store.CreateWebSession(r.Context(), h.pool,
		pair.AccessToken, ar.PrincipalID, csrfSecret, r.UserAgent(), clientIP(r, h.trustedProxy))
	if err != nil {
		slog.Error("session select-key: create overlay", "error", err)
		writeJSON(w, http.StatusInternalServerError, map[string]any{"success": false, "error": "internal error"})
		return
	}

	// (3) Neue Session steht — jetzt stirbt die alte Familie. Ein Fehler
	// hier lässt die alte Session bestenfalls weiterleben (Reaper/GC räumt
	// nach), er darf den vollzogenen Wechsel nicht mehr abbrechen.
	if err := store.DestroyWebSession(r.Context(), h.pool, oldSessionID); err != nil {
		slog.Error("session select-key: destroy old session", "error", err)
	}

	h.setSessionCookies(w, newSessionID, pair.RefreshToken)
	writeJSON(w, http.StatusOK, map[string]any{"success": true, "csrf_token": csrfSecret})
}
