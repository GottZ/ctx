// Package clientkey is the one place a ctx client attaches its API key to a
// request (#413, #423, #432). internal/cli sends every keyed request of ctx
// through it — the API client, the ingest upload, the statusline, the init
// probes and ctx mcp serve — internal/armsweep every request of ctx-armsweep,
// and internal/goldbench every keyed chat call of ctx-goldbench, whose key
// goes as a bearer token (NewBearer). TestKeyCredentialOnlyInKeyTransport
// holds that no other code of theirs sets a credential header.
//
// A leaf below both: it imports the standard library and internal/httpx (the
// Origin definition, which brings internal/ssrfguard) and nothing else, so a
// bench tool that sends through it links neither the command tree nor another
// server package (depguard clientkey-leaf).
package clientkey

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/GottZ/ctx/internal/httpx"
)

// keyHeader carries the API key. Only Transport.signed uses it.
const keyHeader = "X-Context-Key"

// bearerHeader carries the API key of a Transport from NewBearer, as
// "Bearer <key>". Only Transport.signed uses it.
const bearerHeader = "Authorization"

// maxRedirects is net/http's own hop limit (defaultCheckRedirect). Setting
// CheckRedirect replaces that default, so checkRedirect restates the limit and
// its error text: a chain within the origin ends where it ended before.
const maxRedirects = 10

// Names are the words a refusal uses for the program that holds the key
// (Client: "the ctx CLI") and for the setting its base URL comes from
// (BaseURL: "CTX_BASE_URL"). They change no rule, only the message.
type Names struct {
	Client  string
	BaseURL string
}

// Transport binds an API key to the origin of a base URL: scheme, host and
// port, as httpx.OriginOf defines them (host case folded in the ASCII range
// only, the scheme's default port filled in). A request on that origin carries
// the key; a request anywhere else — a redirect hop above all — is refused
// before a byte of it is sent: the key, and with it the body of a POST, which
// is the user's content. net/http's own redirect rule does not cover this: on
// a host change it drops Authorization and Cookie, never a custom header such
// as X-Context-Key, and it keeps Authorization on another port or scheme of
// the same host name and on a subdomain.
//
// One rule, two hooks. As a RoundTripper it serves ctx mcp serve, whose
// requests the MCP SDK builds: http.Client calls it once per hop. Do signs the
// first request and checks every hop in CheckRedirect, and leaves the client's
// own *http.Transport in place — net/http handles the timeout of such a client
// by context, and wrapping the transport would switch it to its timer path,
// with other error texts and more allocations per call.
//
// A change of scheme or port on the same host is another origin too. The
// common case is a proxy upgrading http to https; the key has then already
// crossed the wire in clear text on the first hop, so following the upgrade
// would hide a configuration that leaks it on every call. The refusal of such
// an upgrade names the URL to put into the base URL setting instead.
type Transport struct {
	key    string
	base   *url.URL // the base URL, parsed
	origin httpx.Origin
	next   http.RoundTripper // RoundTrip only; nil means http.DefaultTransport
	names  Names
	bearer bool // the key goes as "Authorization: Bearer <key>" (NewBearer), not as X-Context-Key
}

// New binds key to the origin of baseURL, which must be an http or https URL
// with a host. The key goes as X-Context-Key.
func New(baseURL, key string, next http.RoundTripper, names Names) (Transport, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return Transport{}, err
	}
	if s := strings.ToLower(u.Scheme); (s != "http" && s != "https") || u.Hostname() == "" {
		return Transport{}, fmt.Errorf("%s has no http or https host", u.Redacted())
	}
	return Transport{key: key, base: u, origin: httpx.OriginOf(u), next: next, names: names}, nil
}

// NewBearer is New for a key that goes as a bearer token — "Authorization:
// Bearer <key>", the form an OpenAI-compatible endpoint takes it in, ctx's
// /v1 among them — instead of as X-Context-Key. The origin rule and the
// refusal are New's; RoundTrip sends through http.DefaultTransport.
func NewBearer(baseURL, key string, names Names) (Transport, error) {
	t, err := New(baseURL, key, nil, names)
	if err != nil {
		return Transport{}, err
	}
	t.bearer = true
	return t, nil
}

// admit is the rule: nil for a request on the origin, the refusal otherwise.
func (t Transport) admit(req *http.Request) error {
	to := httpx.OriginOf(req.URL)
	if to == t.origin {
		return nil
	}
	refusal := &Refusal{to: to, origin: t.origin, names: t.names}
	if req.Response != nil { // set by http.Client on a redirect hop
		refusal.status = req.Response.StatusCode
	}
	// An http→https upgrade on the same host (80→443 or the same port, the
	// relation the backend pools follow) is most likely the server's own
	// address behind a proxy: the advice is the base URL with that scheme and
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
func (t Transport) signed(req *http.Request) (*http.Request, error) {
	if err := t.admit(req); err != nil {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, err
	}
	r := req.Clone(req.Context())
	if t.bearer {
		r.Header.Set(bearerHeader, "Bearer "+t.key)
	} else {
		r.Header.Set(keyHeader, t.key)
	}
	return r, nil
}

// RoundTrip sends req with the key through the next transport, or refuses it.
func (t Transport) RoundTrip(req *http.Request) (*http.Response, error) {
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

// Do sends req through a copy of hc — its budget, its own transport — with the
// key on the first request and checkRedirect on every hop. hc is not changed.
func (t Transport) Do(hc *http.Client, req *http.Request) (*http.Response, error) {
	signed, err := t.signed(req)
	if err != nil {
		return nil, err
	}
	c := *hc
	c.CheckRedirect = t.checkRedirect
	return c.Do(signed) //nolint:gosec // G704: signed let the request through only on the origin of the operator's own base URL, and checkRedirect holds every hop to it — the rule this package exists for.
}

// checkRedirect is the CheckRedirect of Do: net/http's hop limit, then the
// rule. It is the only thing between the key and the hop: http.Client builds
// each hop with the headers of the first request, the signed one, and drops
// only Authorization, Cookie and WWW-Authenticate on a host change. A refused
// hop is not sent, and net/http closes its body.
func (t Transport) checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	return t.admit(req)
}

// Refusal is a request Transport did not let through because it left the
// origin the key is bound to.
type Refusal struct {
	to, origin httpx.Origin
	status     int    // the status of the redirect that led here; 0 for a first request
	advice     string // the base URL to set; empty when none is suggested
	names      Names
}

func (e *Refusal) Error() string {
	what := "a request"
	if e.status != 0 {
		what = fmt.Sprintf("a redirect (HTTP %d)", e.status)
	}
	msg := fmt.Sprintf("refused %s to %s: %s sends its API key only to the origin of %s, %s, and follows a redirect only within it",
		what, written(e.to), e.names.Client, e.names.BaseURL, e.origin)
	if e.advice != "" {
		msg += fmt.Sprintf("; set %s to %s if that is the server's address", e.names.BaseURL, e.advice)
	}
	return msg
}

// written renders the origin a request went to as its URL wrote it where the
// URL left a part out: a URL without a scheme (protocol-relative, as a whole
// URL handed to the CLI's Client.Get can be) as //host[:port], and one whose
// scheme has no default port without a port. httpx.Origin.String spells every
// part, which made them "://host:port" and "ftp://host:". A redirect hop always
// has a scheme — net/http resolves the Location against the request — and the
// base URL's origin always has a scheme and a port.
func written(o httpx.Origin) string {
	hostPort := net.JoinHostPort(o.Host, o.Port)
	if o.Port == "" {
		hostPort = strings.TrimSuffix(hostPort, ":")
	}
	if o.Scheme == "" {
		return "//" + hostPort
	}
	return o.Scheme + "://" + hostPort
}
