package camo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/GottZ/ctx/internal/ssrfguard"
)

// maxRedirects caps the redirect chain (design §4.4.4). Each hop dials through
// the SAME SSRF-guarded transport, so the classic "302 → internal IP" Camo
// bypass is closed at the dial layer; this cap and the per-hop scheme recheck are
// belt-and-suspenders on top.
const maxRedirects = 3

// allowedContentTypes is the image allowlist (design §4.4.7, D5). image/svg+xml
// is DELIBERATELY absent: SVG is an active-content XSS vector (inline <script>,
// <foreignObject>, onload). SVG upstreams fall back to the renderer placeholder.
var allowedContentTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
	"image/avif": true,
}

// Fetcher runs the upstream fetch policy: SSRF-guarded dial, redirect cap, tight
// timeouts, size cap, and the content-type allowlist.
type Fetcher struct {
	client   *http.Client
	maxBytes int64
}

// NewFetcher builds the production policy-enforcing HTTP client. The dialer is
// the shared ssrfguard.GuardedDialer with a TIGHTER connect timeout than forge
// (images are small); a hung upstream must not pin a server goroutine.
func NewFetcher(maxBytes int64) *Fetcher {
	dialer := ssrfguard.GuardedDialer()
	dialer.Timeout = 5 * time.Second // design §4.4.5: dial 5s
	return newFetcher(maxBytes, dialer)
}

// newFetcher builds a Fetcher over a caller-supplied dialer. Production passes
// the SSRF-guarded dialer; tests pass a dialer that permits the loopback mock
// (which the production guard rightly refuses) while still delegating OTHER
// addresses — including redirect hops — to the guard, so the redirect-SSRF path
// is exercised against real guard logic.
func newFetcher(maxBytes int64, dialer *net.Dialer) *Fetcher {
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: 5 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableKeepAlives:     true, // one-shot fetches; no pooled foreign conns
	}
	client := &http.Client{
		Transport:     transport,
		Timeout:       10 * time.Second, // total per-fetch budget
		CheckRedirect: checkRedirect,
	}
	return &Fetcher{client: client, maxBytes: maxBytes}
}

// checkRedirect caps the redirect chain (design §4.4.4) and re-checks the scheme
// on each hop. The SSRF dial guard fires on every hop's resolved IP independently
// (it is the transport's Control hook), so the "302 → internal IP" bypass is
// closed at the dial layer; this is the belt-and-suspenders on top.
func checkRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("camo: too many redirects (>%d)", maxRedirects)
	}
	if req.URL.Scheme != "http" && req.URL.Scheme != "https" {
		return fmt.Errorf("camo: redirect to disallowed scheme %q", req.URL.Scheme)
	}
	return nil
}

// Result is a successfully fetched, allowlisted image.
type Result struct {
	ContentType string // the safe content-type to serve (allowlisted)
	Body        []byte
}

// FetchError carries the HTTP status the handler should emit. It never wraps the
// upstream URL or body into a client-visible message (no exfil surface).
type FetchError struct {
	Status int
	msg    string
}

func (e *FetchError) Error() string { return e.msg }

func fetchErr(status int, msg string) *FetchError { return &FetchError{Status: status, msg: msg} }

// Fetch pulls rawURL under the full policy and returns the image bytes + a safe
// content-type, or a FetchError with the status to emit. Precondition: the caller
// has ALREADY verified the signature — Fetch performs no auth, only policy.
func (f *Fetcher) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	// Scheme allowlist (design §4.4.2): only http/https, checked BEFORE any dial.
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return nil, fetchErr(http.StatusBadRequest, "camo: unparseable url")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fetchErr(http.StatusBadRequest, "camo: scheme not allowed")
	}
	if u.Host == "" {
		return nil, fetchErr(http.StatusBadRequest, "camo: missing host")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil) //nolint:gosec // G107: SSRF is enforced dial-time by the ssrfguard transport (every hop), not on this string.
	if err != nil {
		return nil, fetchErr(http.StatusBadRequest, "camo: bad request")
	}
	// Ask for images; identify ourselves without leaking the viewer.
	req.Header.Set("Accept", "image/png,image/jpeg,image/gif,image/webp,image/avif,image/*;q=0.8")
	req.Header.Set("User-Agent", "ctx-camo/1")

	resp, err := f.client.Do(req)
	if err != nil {
		// SSRF refusal (dial control), DNS failure, timeout, redirect-policy
		// violation — all collapse to a 502; a context-deadline is a 504.
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fetchErr(http.StatusGatewayTimeout, "camo: upstream timeout")
		}
		return nil, fetchErr(http.StatusBadGateway, "camo: upstream fetch failed")
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fetchErr(http.StatusBadGateway, fmt.Sprintf("camo: upstream status %d", resp.StatusCode))
	}

	// Content-Length pre-check (design §4.4.6): reject an upstream that DECLARES a
	// body over the cap before reading a single byte.
	if resp.ContentLength > 0 && resp.ContentLength > f.maxBytes {
		return nil, fetchErr(http.StatusBadGateway, "camo: upstream declares oversize body")
	}

	// Size cap: read at most maxBytes+1 so an overshoot is detectable. No unbounded
	// buffering — a lying Content-Length cannot make us read past the cap.
	body, err := io.ReadAll(io.LimitReader(resp.Body, f.maxBytes+1))
	if err != nil {
		return nil, fetchErr(http.StatusBadGateway, "camo: upstream read failed")
	}
	if int64(len(body)) > f.maxBytes {
		return nil, fetchErr(http.StatusBadGateway, "camo: upstream body exceeds size cap")
	}

	serveCT, ok := classifyImage(resp.Header.Get("Content-Type"), body)
	if !ok {
		// text/html, image/svg+xml, application/xml, a script — anything the
		// browser might render as active content, or a non-image (design §4.4.7).
		return nil, fetchErr(http.StatusUnsupportedMediaType, "camo: content-type not an allowed image")
	}
	return &Result{ContentType: serveCT, Body: body}, nil
}

// classifyImage enforces the two-layer content-type gate (design §4.4.7, D5) and
// returns the SAFE content-type to serve:
//
//  1. The upstream-declared type MUST be in the image allowlist (SVG rejected).
//  2. The sniffed type (http.DetectContentType on the first 512 bytes) MUST NOT
//     be active/markup content — this catches an upstream that LIES ("image/png"
//     over an actual HTML/SVG/JS payload).
//
// Deviation from the literal design §4.4.7 (documented): the card asks that the
// sniff ALSO be in the allowlist. Go's http.DetectContentType does not recognize
// AVIF (and some WebP encoders) — it returns application/octet-stream — so a
// literal "sniff ∈ allowlist" would reject valid AVIF. Instead the sniff is used
// as a NEGATIVE filter (reject active content), which is what the gate is FOR:
// keeping markup/script out. A binary sniff (octet-stream) over an allowlisted
// declared type is served as that declared type. SVG/HTML/JS never pass either
// layer.
func classifyImage(declaredHeader string, body []byte) (string, bool) {
	declared := mediaType(declaredHeader)
	if !allowedContentTypes[declared] {
		return "", false
	}
	sniff := mediaType(http.DetectContentType(body))
	if isActiveSniff(sniff) {
		return "", false
	}
	// Prefer an allowlisted sniff (PNG/JPEG/GIF/WebP that Go recognizes) as the
	// served type; fall back to the validated declared type (AVIF, some WebP).
	if allowedContentTypes[sniff] {
		return sniff, true
	}
	return declared, true
}

// isActiveSniff reports whether a sniffed media type is something a browser could
// interpret as markup or active content (HTML, XML, SVG, plain text, scripts).
// These must never be served through the image proxy regardless of the declared
// type — that is the whole point of the sniff.
func isActiveSniff(ct string) bool {
	if strings.HasPrefix(ct, "text/") {
		return true
	}
	switch {
	case strings.Contains(ct, "html"),
		strings.Contains(ct, "xml"),
		strings.Contains(ct, "svg"),
		strings.Contains(ct, "javascript"),
		strings.Contains(ct, "script"):
		return true
	}
	return false
}

// mediaType lowercases and strips parameters from a Content-Type header value
// ("image/png; charset=binary" → "image/png"). Malformed input yields "".
func mediaType(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if i := strings.IndexByte(v, ';'); i >= 0 {
		v = v[:i]
	}
	return strings.ToLower(strings.TrimSpace(v))
}
