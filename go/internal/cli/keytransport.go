package cli

// keytransport.go — the one place the CLI attaches its API key to a request
// (#413). Client.send (client.go) puts it in front of every keyed request of
// the API client, the ingest upload, the statusline and the init probes;
// ctx mcp serve (mcp_serve.go) builds its HTTP client on it.
// TestKeyCredentialOnlyInKeyTransport holds that no other code in the CLI
// sets the key header.

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/GottZ/ctx/internal/httpx"
)

// keyHeader carries the API key. Only keyTransport.RoundTrip uses it.
const keyHeader = "X-Context-Key"

// keyTransport attaches the API key of the CLI configuration to a request and
// sends it only to the origin of CTX_BASE_URL: scheme, host and port, as
// httpx.OriginOf defines them (host case folded in the ASCII range only, the
// scheme's default port filled in). http.Client follows a redirect by calling
// the transport once per hop, so a redirect within the origin is followed as
// before, and a hop anywhere else is refused before a byte of it is sent —
// the key, and with it the body of a POST, which is the user's content.
// net/http's own redirect rule does not cover this: on a host change it drops
// Authorization and Cookie, never a custom header such as X-Context-Key.
//
// A change of scheme or port on the same host is another origin too. The
// common case is a proxy upgrading http to https; the key has then already
// crossed the wire in clear text on the first hop, so following the upgrade
// would hide a configuration that leaks it on every call. The refusal of an
// http→https upgrade on the same host names the URL to put into CTX_BASE_URL
// instead.
type keyTransport struct {
	key    string
	origin httpx.Origin
	base   http.RoundTripper
}

// newKeyTransport binds key to the origin of baseURL, which must be an http
// or https URL with a host. base nil means http.DefaultTransport.
func newKeyTransport(baseURL, key string, base http.RoundTripper) (keyTransport, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return keyTransport{}, err
	}
	if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Hostname() == "" {
		return keyTransport{}, fmt.Errorf("%s has no http or https host", u.Redacted())
	}
	if base == nil {
		base = http.DefaultTransport
	}
	return keyTransport{key: key, origin: httpx.OriginOf(u), base: base}, nil
}

func (t keyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if to := httpx.OriginOf(req.URL); to != t.origin {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		refusal := &keyOriginRefusal{target: req.URL, to: to, origin: t.origin}
		if req.Response != nil { // set by http.Client on a redirect hop
			refusal.status = req.Response.StatusCode
		}
		return nil, refusal
	}
	r := req.Clone(req.Context())
	r.Header.Set(keyHeader, t.key)
	return t.base.RoundTrip(r)
}

// keyOriginRefusal is a request keyTransport did not send because it left
// the origin the key is bound to.
type keyOriginRefusal struct {
	target     *url.URL
	to, origin httpx.Origin
	status     int // the status of the redirect that led here; 0 for a first request
}

func (e *keyOriginRefusal) Error() string {
	what := "a request"
	if e.status != 0 {
		what = fmt.Sprintf("a redirect (HTTP %d)", e.status)
	}
	msg := fmt.Sprintf("refused %s to %s: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, %s, and follows a redirect only within it",
		what, e.to, e.origin)
	if e.to.Host == e.origin.Host && e.origin.Scheme == "http" && e.to.Scheme == "https" {
		// An upgrade on the same host: most likely the server's own address
		// behind a proxy. Nothing else is suggested — another host, or
		// another port on the same one, can be someone else's service, and
		// a downgrade would put the key back into clear text.
		at := (&url.URL{Scheme: e.target.Scheme, Host: e.target.Host}).String()
		msg += fmt.Sprintf("; set CTX_BASE_URL to %s if that is the server's address", at)
	}
	return msg
}
