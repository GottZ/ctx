package handler

import (
	"context"
	"log/slog"
	"net/http"
	"net/url"
	"strings"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/modelkey"
	"github.com/GottZ/ctx/internal/store"
)

// The B-side plaintext guard (design/01 §5 B24, K-04-6; wave F-W5). A model
// key (ctxm_) of a peer ctx instance is a bearer credential: on a plain
// http:// base_url every call carries it unencrypted (Authorization: Bearer,
// llm/client.go), and whoever reads the wire can send prompts in the peer
// consumer's name. The peer's door refuses plaintext transport unless its
// consumer allows it (gate 6 of ctx_model_auth); this side refuses to store
// the combination without an explicit confirm.
//
// One rule, fail-closed: EVERY http:// base_url counts, loopback and
// localhost included — no exception list to keep right. The texts carry no
// secret value, name or hint.
const (
	plaintextModelKeyRefusal = "base_url is plain http:// and the secret named by api_key_ref holds a ctx model key (ctxm_) — every call would carry the key unencrypted; requires confirm_plaintext_credential:true (or an https:// base_url)"
	plaintextModelKeyWarning = "base_url is plain http:// and api_key_ref holds a ctx model key — the key travels unencrypted on every call (confirmed with confirm_plaintext_credential)"
)

// plaintextModelKey reports whether row b, stored in rowScope, would carry a
// ctx model key without TLS: base_url scheme http and api_key_ref resolving —
// in the scopes the pool resolver uses for the row (store.BackendSecretScopes,
// first scope that resolves wins) — to a value modelkey.HasPrefix accepts
// (the prefix after invisible characters, well-formed or not). A ref that
// resolves nowhere, or no loaded sealbox, is no plaintext key: the pool could
// not send it either, and checkBackendSecretRef stays the truth for
// existence. Only a failed scope lookup is an error.
func (h *ManageHandler) plaintextModelKey(ctx context.Context, b *backends.Backend, rowScope string) (bool, error) {
	if strings.TrimSpace(b.APIKeyRef) == "" {
		return false, nil
	}
	if !plainHTTP(b.Host) {
		return false, nil
	}
	box := h.sealboxOrNil()
	if box == nil {
		return false, nil
	}
	scopes, err := store.BackendSecretScopes(ctx, h.pool, rowScope, b.APIKeyRef, b.Host)
	if err != nil {
		return false, err
	}
	for _, scope := range scopes {
		plaintext, err := store.ResolveSecret(ctx, h.pool, box, b.APIKeyRef, scope)
		if err != nil {
			continue
		}
		isKey := modelkey.HasPrefix(string(plaintext))
		clear(plaintext)
		return isKey, nil
	}
	return false, nil
}

// checkPlaintextModelKey gates one backend write on B24. armed says whether
// the write creates the exposure: always on create, on update only when the
// target (base_url) or the credential (api_key_ref) changes — re-saving a row
// whose plaintext transport was confirmed before stays friction-free, the
// doctrine of the trust and data-collection confirms. Without the confirm the
// write is a 400; with it the write passes, one WARN line names backend and
// scope, and the returned warning joins the response. Returns false after
// writing the response.
func (h *ManageHandler) checkPlaintextModelKey(w http.ResponseWriter, r *http.Request, b *backends.Backend, rowScope string, armed, confirmed bool) (string, bool) {
	if !armed {
		return "", true
	}
	risky, err := h.plaintextModelKey(r.Context(), b, rowScope)
	if err != nil {
		internalError(w, r.Context(), "backends: plaintext model-key check failed", err)
		return "", false
	}
	if !risky {
		return "", true
	}
	if !confirmed {
		writeJSON(w, http.StatusBadRequest, map[string]any{"success": false, "error": plaintextModelKeyRefusal})
		return "", false
	}
	slog.Warn("backends: confirmed plaintext model-key transport", "backend", b.Name, "scope", rowScope)
	return plaintextModelKeyWarning, true
}

// plainHTTP reports whether host is an http:// URL (url.Parse lowercases the
// scheme); validation admits http and https only, so every other row is TLS.
func plainHTTP(host string) bool {
	u, err := url.Parse(host)
	return err == nil && u.Scheme == "http"
}
