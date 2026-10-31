// oauth_consent.go — the /authorize consent screen (K7; advisory §6).
//
// Before K7 the GET form asked for the raw API key without saying WHO asked
// or WHERE the browser would be sent afterwards, and it validated nothing:
// with CTX_OAUTH_DCR_MODE=open anyone could register a client with an
// attacker-owned https callback and send a victim a genuine
// <ctx>/authorize link — the key went into a code for the attacker's
// client. The consent screen closes the UI half of that:
//
//   - the GET validates client_id (registered, active) and redirect_uri
//     (the same exact-match rule the POST enforces) BEFORE anything is
//     rendered; a failure is an error page WITHOUT a key field (OAuth 2.1
//     §4.1.2.1: an invalid client or redirect is never redirected to and
//     never offered the credential prompt);
//   - the form shows the client's name, its client_id and the redirect
//     ORIGIN (scheme + host, path shortened) — the address the code goes to;
//   - a client that registered itself (DCR without an acting admin key)
//     carries an explicit warning;
//   - an explicit consent checkbox, checked SERVER-side at POST — a key
//     alone no longer issues a code.
//
// The page is server-rendered html/template (contextual escaping) under a
// script-free CSP; there is no JavaScript on it at all.
package handler

import (
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/GottZ/ctx/internal/store"
)

// consentFieldValue is the value the consent checkbox posts. The POST
// accepts exactly this value — an absent or different value issues no code.
const consentFieldValue = "yes"

// consentCSP: no script source at all (the page has none), inline <style>
// only, no framing, no <base> rewrite, no plugins. form-action is
// DELIBERATELY absent: browsers apply form-action to the redirect that
// follows the form POST, and that redirect leaves for the client's callback
// host — form-action 'self' would break every successful authorization.
const consentCSP = "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'; base-uri 'none'"

// consentPathMax bounds the displayed redirect path (runes). The origin is
// the trust-relevant part and is always shown in full.
const consentPathMax = 48

// authorizeParams are the authorization-request parameters the consent form
// round-trips as hidden fields (GET query → POST form).
type authorizeParams struct {
	ResponseType        string
	ClientID            string
	RedirectURI         string
	CodeChallenge       string
	CodeChallengeMethod string
	Resource            string
	State               string
}

func authorizeParamsFrom(get func(string) string) authorizeParams {
	return authorizeParams{
		ResponseType:        get("response_type"),
		ClientID:            get("client_id"),
		RedirectURI:         get("redirect_uri"),
		CodeChallenge:       get("code_challenge"),
		CodeChallengeMethod: get("code_challenge_method"),
		Resource:            get("resource"),
		State:               get("state"),
	}
}

// authorizeReject is a validation failure of the client/redirect pair: an
// HTTP status plus a short human-readable reason for the error page.
type authorizeReject struct {
	status int
	title  string
	detail string
}

// resolveAuthorizeClient validates client_id and redirect_uri — shared by
// the GET (before rendering) and the POST (before touching the key), so both
// sides apply ONE rule:
//
//   - client_id is MANDATORY (S2/W03-2; OAuth 2.1 §4.1.1) and must name an
//     active registered client;
//   - redirect_uri must be an absolute http(s) URL (javascript:, data:,
//     file: … are an open-redirect vector) and pass the allowlist: a client
//     with REGISTERED redirect_uris is matched EXCLUSIVELY against them
//     (S6/W03-7 — exact array membership, no loopback rule, no static
//     fallback: the registration is the contract), an empty registration
//     (plain `ctx mcp add`, pre-097 rows) keeps the static S2 allowlist +
//     RFC 8252 loopback rule. Without it an attacker-controlled redirect_uri
//     carries the code off-site (the GHSA-p3jf-x27v-4jg6 core).
//
// The reconstruction through net/url also sanitises control characters that
// would otherwise be reflected into the Location header.
func (h *OAuthHandler) resolveAuthorizeClient(r *http.Request, clientID, redirectURI string) (*store.OAuthClient, *url.URL, *authorizeReject) {
	if clientID == "" {
		return nil, nil, &authorizeReject{http.StatusBadRequest, "Missing client_id",
			"The authorization request does not name a client."}
	}
	client, found, err := store.GetOAuthClientByClientID(r.Context(), h.pool, clientID)
	if err != nil {
		slog.Error("oauth: load client in authorize", "error", err)
		return nil, nil, &authorizeReject{http.StatusInternalServerError, "Internal error", "Please try again later."}
	}
	if !found {
		slog.Warn("oauth: invalid client_id in authorize", "client_id", clientID)
		return nil, nil, &authorizeReject{http.StatusBadRequest, "Unknown Client",
			"This client_id is not registered. Use ctx mcp add to create one."}
	}

	invalidRedirect := &authorizeReject{http.StatusBadRequest, "Invalid redirect_uri",
		"The address this request wants to return to is not an allowed callback for this client."}
	parsed, err := url.Parse(redirectURI)
	if redirectURI == "" || err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, nil, invalidRedirect
	}
	if len(client.RedirectURIs) > 0 {
		allowed, err := store.ClientAllowsRedirectURI(r.Context(), h.pool, clientID, redirectURI)
		if err != nil {
			slog.Error("oauth: check registered redirect_uri", "error", err)
			return nil, nil, &authorizeReject{http.StatusInternalServerError, "Internal error", "Please try again later."}
		}
		if !allowed {
			slog.Warn("oauth: redirect_uri not in client registration", "redirect_uri", redirectURI, "client_id", clientID)
			return nil, nil, invalidRedirect
		}
	} else if !redirectAllowed(redirectURI, parsed) {
		slog.Warn("oauth: redirect_uri not in allowlist", "redirect_uri", redirectURI)
		return nil, nil, invalidRedirect
	}
	return client, parsed, nil
}

// clientSelfRegistered reports whether the client registered itself: a DCR
// (or future CIMD) row without an acting admin key. Admin-mode DCR rows carry
// the admin's key id in created_by and are operator-made; a NULL creator on a
// non-admin source can only be open registration (or a hard-deleted admin
// key — the warning then errs on the cautious side).
func clientSelfRegistered(c *store.OAuthClient) bool {
	return c.RegistrationSource != "admin" && c.CreatedBy == nil
}

// consentView is the template data of the consent page.
type consentView struct {
	ClientName     string
	ClientID       string
	RedirectOrigin string
	RedirectPath   string
	NonASCIIHost   bool
	SelfRegistered bool
	Error          string
	ConsentValue   string
	Params         authorizeParams
}

// newConsentView derives the display values from the validated client and
// redirect. A host with non-ASCII runes is shown in escaped form (\uXXXX)
// and flagged: a Unicode look-alike host must never render as the domain it
// imitates.
func newConsentView(client *store.OAuthClient, redirect *url.URL, p authorizeParams, errMsg string) consentView {
	name := strings.TrimSpace(client.Label)
	if name == "" {
		name = "unnamed client"
	}
	host := redirect.Host
	nonASCII := false
	for _, r := range host {
		if r >= utf8.RuneSelf || r < 0x20 {
			nonASCII = true
			break
		}
	}
	if nonASCII {
		host = strings.Trim(strconv.QuoteToASCII(host), `"`)
	}
	path := redirect.EscapedPath()
	if runes := []rune(path); len(runes) > consentPathMax {
		path = string(runes[:consentPathMax]) + "…"
	}
	return consentView{
		ClientName:     name,
		ClientID:       client.ClientID,
		RedirectOrigin: redirect.Scheme + "://" + host,
		RedirectPath:   path,
		NonASCIIHost:   nonASCII,
		SelfRegistered: clientSelfRegistered(client),
		Error:          errMsg,
		ConsentValue:   consentFieldValue,
		Params:         p,
	}
}

const authorizeStyle = `
  body { font-family: system-ui; max-width: 460px; margin: 60px auto; padding: 0 20px; color: #111; background: #fff; }
  h1 { font-size: 1.4em; }
  .client { border: 1px solid #ccc; border-radius: 6px; padding: 12px 14px; margin: 16px 0; }
  .client dt { color: #555; font-size: 0.85em; margin-top: 8px; }
  .client dd { margin: 2px 0 0 0; word-break: break-all; }
  .origin { font-family: monospace; font-size: 1.1em; font-weight: bold; }
  .mono { font-family: monospace; }
  .warn { border: 2px solid #b45309; background: #fff7ed; color: #7c2d12; border-radius: 6px; padding: 10px 14px; margin: 16px 0; }
  .error { border: 2px solid #b91c1c; background: #fef2f2; color: #7f1d1d; border-radius: 6px; padding: 10px 14px; margin: 16px 0; }
  input[type=password] { width: 100%; padding: 10px; font-size: 1em; font-family: monospace; box-sizing: border-box; }
  label.consent { display: block; margin-top: 14px; }
  button { margin-top: 14px; padding: 10px 24px; font-size: 1em; cursor: pointer; }
  .hint { color: #555; font-size: 0.85em; margin-top: 8px; }
`

var consentTmpl = template.Must(template.New("consent").Parse(`<!DOCTYPE html>
<html><head><title>ctx — Authorize</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>` + authorizeStyle + `</style>
</head><body>
<h1>ctx — Authorize MCP Access</h1>
<p>An application is asking for access to ctx with your API key.</p>
<dl class="client">
  <dt>Application</dt><dd><strong>{{.ClientName}}</strong></dd>
  <dt>Client ID</dt><dd class="mono">{{.ClientID}}</dd>
  <dt>You will be sent back to</dt><dd><span class="origin">{{.RedirectOrigin}}</span><span class="mono">{{.RedirectPath}}</span></dd>
</dl>
{{if .NonASCIIHost}}<div class="warn"><strong>Warning:</strong> the return address contains non-ASCII characters and is shown escaped. It may imitate a different domain.</div>
{{end}}{{if .SelfRegistered}}<div class="warn"><strong>Warning:</strong> This client registered itself. Only continue if you started this flow from that application and recognise the address above.</div>
{{end}}{{if .Error}}<div class="error">{{.Error}}</div>
{{end}}<form method="POST" action="/authorize">
  <label for="key">API Key:</label><br>
  <input type="password" id="key" name="api_key" required autofocus autocomplete="off"><br>
  <p class="hint">The application receives an access token with all rights of this key — not the key itself.</p>
  <label class="consent"><input type="checkbox" name="consent" value="{{.ConsentValue}}" required> I want to grant this client access with my API key</label>
  <input type="hidden" name="response_type" value="{{.Params.ResponseType}}">
  <input type="hidden" name="client_id" value="{{.Params.ClientID}}">
  <input type="hidden" name="redirect_uri" value="{{.Params.RedirectURI}}">
  <input type="hidden" name="code_challenge" value="{{.Params.CodeChallenge}}">
  <input type="hidden" name="code_challenge_method" value="{{.Params.CodeChallengeMethod}}">
  <input type="hidden" name="resource" value="{{.Params.Resource}}">
  <input type="hidden" name="state" value="{{.Params.State}}">
  <button type="submit">Authorize</button>
</form>
</body></html>`))

var authorizeErrorTmpl = template.Must(template.New("authorize-error").Parse(`<!DOCTYPE html>
<html><head><title>ctx — Authorize</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>` + authorizeStyle + `</style>
</head><body>
<h1>{{.Title}}</h1>
<p>{{.Detail}}</p>
<p class="hint">No access was granted. Return to the application and start the connection again.</p>
</body></html>`))

// writeAuthorizeHTML sets the page headers shared by the consent and error
// pages and executes the template.
func writeAuthorizeHTML(w http.ResponseWriter, status int, tmpl *template.Template, data any) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", consentCSP)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.WriteHeader(status)
	if err := tmpl.Execute(w, data); err != nil {
		slog.Error("oauth: render authorize page", "error", err)
	}
}

// writeAuthorizeError renders the key-less error page.
func writeAuthorizeError(w http.ResponseWriter, rej *authorizeReject) {
	writeAuthorizeHTML(w, rej.status, authorizeErrorTmpl, struct{ Title, Detail string }{rej.title, rej.detail})
}

// writeConsent renders the consent page (status 200 on GET; the POST
// re-renders it with an error message and a 4xx status).
func writeConsent(w http.ResponseWriter, status int, client *store.OAuthClient, redirect *url.URL, p authorizeParams, errMsg string) {
	writeAuthorizeHTML(w, status, consentTmpl, newConsentView(client, redirect, p, errMsg))
}
