package cli

// keytransport.go — the one place the CLI attaches its API key to a request
// (#413). Client.send (client.go) uses it for every keyed request of the API
// client, the ingest upload, the statusline and the init probes; ctx mcp serve
// (mcp_serve.go) builds its HTTP client on it. TestKeyCredentialOnlyInKeyTransport
// holds that no other code in the CLI sets the key header.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/GottZ/ctx/internal/httpx"
)

// keyHeader carries the API key. Only keyTransport.signed uses it.
const keyHeader = "X-Context-Key"

// maxRedirects is net/http's own hop limit (defaultCheckRedirect). Setting
// CheckRedirect replaces that default, so checkRedirect restates the limit and
// its error text: a chain within the origin ends where it ended before.
const maxRedirects = 10

// keyTransport binds the API key of the CLI configuration to the origin of
// CTX_BASE_URL: scheme, host and port, as httpx.OriginOf defines them (host
// case folded in the ASCII range only, the scheme's default port filled in).
// A request on that origin carries the key; a request anywhere else — a
// redirect hop above all — is refused before a byte of it is sent: the key,
// and with it the body of a POST, which is the user's content. net/http's own
// redirect rule does not cover this: on a host change it drops Authorization
// and Cookie, never a custom header such as X-Context-Key.
//
// One rule, two hooks. As a RoundTripper it serves ctx mcp serve, whose
// requests the MCP SDK builds: http.Client calls it once per hop. Client.send
// uses signed for the first request and checkRedirect for every hop, and
// leaves the client's own *http.Transport in place — net/http handles the
// timeout of such a client by context, and wrapping the transport would switch
// it to its timer path, with other error texts and more allocations per call.
//
// A change of scheme or port on the same host is another origin too. The
// common case is a proxy upgrading http to https; the key has then already
// crossed the wire in clear text on the first hop, so following the upgrade
// would hide a configuration that leaks it on every call. The refusal of such
// an upgrade names the URL to put into CTX_BASE_URL instead.
type keyTransport struct {
	key    string
	base   *url.URL // CTX_BASE_URL, parsed
	origin httpx.Origin
	next   http.RoundTripper // RoundTrip only; nil means http.DefaultTransport
}

// newKeyTransport binds key to the origin of baseURL, which must be an http
// or https URL with a host.
func newKeyTransport(baseURL, key string, next http.RoundTripper) (keyTransport, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return keyTransport{}, err
	}
	if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Hostname() == "" {
		return keyTransport{}, fmt.Errorf("%s has no http or https host", u.Redacted())
	}
	return keyTransport{key: key, base: u, origin: httpx.OriginOf(u), next: next}, nil
}

// admit is the rule: nil for a request on the origin, the refusal otherwise.
func (t keyTransport) admit(req *http.Request) error {
	to := httpx.OriginOf(req.URL)
	if to == t.origin {
		return nil
	}
	refusal := &keyOriginRefusal{to: to, origin: t.origin}
	if req.Response != nil { // set by http.Client on a redirect hop
		refusal.status = req.Response.StatusCode
	}
	// An http→https upgrade on the same host (80→443 or the same port, the
	// relation the backend pools follow) is most likely the server's own
	// address behind a proxy: the advice is CTX_BASE_URL with that scheme and
	// host, its path kept. Nothing else is suggested — another host, or
	// another port on the same one, can be someone else's service, and a
	// downgrade would put the key back into clear text.
	if httpx.SameOrigin(t.origin, to) {
		advice := *t.base
		advice.Scheme, advice.Host, advice.User = req.URL.Scheme, req.URL.Host, nil
		advice.RawQuery, advice.ForceQuery, advice.Fragment, advice.RawFragment = "", false, "", ""
		refusal.advice = advice.String()
	}
	return refusal
}

// signed returns a copy of req that carries the key, or the refusal — with
// req's body closed — when req leaves the origin.
func (t keyTransport) signed(req *http.Request) (*http.Request, error) {
	if err := t.admit(req); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	r := req.Clone(req.Context())
	r.Header.Set(keyHeader, t.key)
	return r, nil
}

func (t keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	r, err := t.signed(req)
	if err != nil {
		return nil, err
	}
	next := t.next
	if next == nil {
		next = http.DefaultTransport
	}
	return next.RoundTrip(r)
}

// checkRedirect is the CheckRedirect of Client.send: net/http's hop limit,
// then the rule. It is the only thing between the key and the hop: http.Client
// builds each hop with the headers of the first request, the signed one, and
// drops only Authorization, Cookie and WWW-Authenticate on a host change. A
// refused hop is not sent, and net/http closes its body.
func (t keyTransport) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return t.admit(req)
}

// keyOriginRefusal is a request keyTransport did not let through because it
// left the origin the key is bound to.
type keyOriginRefusal struct {
	to, origin httpx.Origin
	status     int    // the status of the redirect that led here; 0 for a first request
	advice     string // the CTX_BASE_URL to set; empty when none is suggested
}

func (e *keyOriginRefusal) Error() string {
	what := "a request"
	if e.status != 0 {
		what = fmt.Sprintf("a redirect (HTTP %d)", e.status)
	}
	msg := fmt.Sprintf("refused %s to %s: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, %s, and follows a redirect only within it",
		what, e.to, e.origin)
	if e.advice != "" {
		msg += fmt.Sprintf("; set CTX_BASE_URL to %s if that is the server's address", e.advice)
	}
	return msg
}
