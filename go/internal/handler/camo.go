// Camo image proxy HTTP surface (design 07-camo-image-proxy.md §4.1/§4.2). Two
// routes with opposite auth postures, wired in cmd/ctxd/server.go:
//
//	POST /api/img/sign   — AUTH-GATED (inside the Auth group). Mints proxy paths
//	                       for foreign image URLs; rate-limited per API key.
//	GET  /api/img/{sig}  — AUTH-LESS (its own group, like /webhooks). A browser
//	                       <img> carries no key; the HMAC signature IS the
//	                       capability. Signature verified BEFORE any upstream
//	                       fetch (no SSRF/timing oracle without a valid sig).
//
// Both routes 404 when the feature is disabled (CTX_CAMO_ENABLED off or no master
// key), so the frontend's placeholder fallback is the safe default (§4.3).
package handler

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/GottZ/ctx/internal/camo"
	"github.com/go-chi/chi/v5"
)

// CamoHandler serves the two image-proxy routes over a camo.Service.
type CamoHandler struct {
	svc *camo.Service
}

// NewCamoHandler wires the handler to a (possibly disabled) service.
func NewCamoHandler(svc *camo.Service) *CamoHandler { return &CamoHandler{svc: svc} }

// signRequest is the batch mint body: the foreign image URLs a rendered document
// wants to proxy.
type signRequest struct {
	URLs []string `json:"urls"`
}

// signResponse maps each proxiable input URL to its signed proxy path. URLs that
// are not proxiable (relative, data:, mailto:, unparseable) are simply omitted —
// the renderer keeps the placeholder for those.
type signResponse struct {
	Success    bool              `json:"success"`
	Signatures map[string]string `json:"signatures"`
}

// HandleSign implements POST /api/img/sign (auth-gated, rate-limited). The Auth
// middleware has already validated the key; we read its id only to key the sign
// rate limiter (oracle-abuse bound, §5).
func (h *CamoHandler) HandleSign(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		writeJSON(w, http.StatusNotFound, map[string]any{"success": false, "error": "image proxy not enabled"})
		return
	}

	keyID := ""
	if ar := AuthResultFromContext(r.Context()); ar != nil {
		keyID = ar.ApiKeyID
	}
	if !h.svc.AllowSign(keyID) {
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"success": false, "error": "image sign rate limit exceeded"})
		return
	}

	var req signRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": "invalid request body"})
		return
	}

	sigs := make(map[string]string, len(req.URLs))
	for _, u := range req.URLs {
		if _, done := sigs[u]; done {
			continue
		}
		if !camo.ProxiableURL(u) {
			continue
		}
		sigs[u] = h.svc.SignedPath(u)
	}
	writeJSON(w, http.StatusOK, signResponse{Success: true, Signatures: sigs})
}

// HandleFetch implements GET /api/img/{sig} (auth-less, signature = capability).
// Order (design §4.4): parse exp → verify sig (constant work, ZERO fetch on
// failure) → expiry → fetch under policy → stream with a safe content-type and
// immutable cache headers. Errors carry NO image body and a no-store cache header
// (D7) so a broken upstream cannot trigger a retry storm and the browser simply
// shows a broken image (the renderer may re-instate its placeholder).
func (h *CamoHandler) HandleFetch(w http.ResponseWriter, r *http.Request) {
	if !h.svc.Enabled() {
		camoError(w, http.StatusNotFound)
		return
	}

	sig := chi.URLParam(r, "sig")
	q := r.URL.Query()
	rawURL := q.Get("url")

	exp, err := strconv.ParseInt(q.Get("exp"), 10, 64)
	if err != nil {
		// A malformed exp cannot be part of any signature we minted → treat as a
		// forged capability, ZERO fetch.
		camoError(w, http.StatusForbidden)
		return
	}

	// Verify the signature FIRST — a forged/absent sig is a uniform 403 and no
	// upstream request ever happens (no SSRF probe, no timing oracle).
	if !h.svc.VerifySig(sig, rawURL, exp) {
		camoError(w, http.StatusForbidden)
		return
	}
	// A once-valid but expired capability is distinguishable (410) — its holder
	// legitimately had it — but still triggers ZERO fetch.
	if exp <= time.Now().Unix() {
		camoError(w, http.StatusGone)
		return
	}

	res, ferr := h.svc.Fetch(r.Context(), rawURL)
	if ferr != nil {
		status := http.StatusBadGateway
		var fe *camo.FetchError
		if errors.As(ferr, &fe) {
			status = fe.Status
		}
		camoError(w, status)
		return
	}

	w.Header().Set("Content-Type", res.ContentType)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Override the global SecurityHeaders no-store: a signed image is immutable
	// per (sig) route identity for its TTL (design §4.5). Set before WriteHeader.
	w.Header().Set("Cache-Control", "public, max-age="+strconv.Itoa(h.svc.TTLSeconds())+", immutable")
	w.Header().Set("Content-Length", strconv.Itoa(len(res.Body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		// res.Body is size-capped upstream image bytes whose content-type passed
		// the two-layer allowlist (declared ∈ image/*, sniff not active content);
		// it is served with the sniffed image content-type + X-Content-Type-Options:
		// nosniff, so the browser cannot reinterpret it as markup. Not an XSS sink.
		_, _ = w.Write(res.Body) //nolint:gosec // G705: allowlisted, sniffed, nosniff'd image bytes — never HTML/SVG.
	}
}

// camoError writes a bodyless error with a no-store cache header (D7: never cache
// a failure long, never emit a substitute image — especially not SVG). The
// browser shows a broken image; the renderer fallback keeps the placeholder.
func camoError(w http.ResponseWriter, status int) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
}
