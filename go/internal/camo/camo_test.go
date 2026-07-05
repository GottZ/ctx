package camo

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A valid 64-hex-char master key (openssl rand -hex 32 shape).
const testMasterKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testPrevKey = "fedcba9876543210fedcba9876543210fedcba9876543210fedcba9876543210"

// pngBytes is a minimal valid PNG (8-byte signature + IHDR) — enough for
// http.DetectContentType to sniff image/png.
var pngBytes = []byte{
	0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, // PNG signature
	0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52, // IHDR
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4, 0x89,
}

func newTestService(t *testing.T) *Service {
	t.Helper()
	svc, err := New(testMasterKey, "", DefaultTTL, DefaultMaxBytes)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc
}

// permitOnly builds a Dialer.Control that allows exactly allowAddr (the test
// mock's loopback host:port) and delegates every OTHER address to the real
// ssrfguard deny-list. So the first hop reaches the mock, but a redirect hop to
// a private/link-local range is refused by the SAME guard production uses.
func permitOnly(allowAddr string) func(network, address string, c syscall.RawConn) error {
	guarded := func(network, address string, c syscall.RawConn) error {
		return dialControlForTest(network, address)
	}
	return func(network, address string, c syscall.RawConn) error {
		if address == allowAddr {
			return nil
		}
		return guarded(network, address, c)
	}
}

// dialControlForTest mirrors ssrfguard.DialControl without importing syscall.
func dialControlForTest(_, address string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip != nil && (ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsUnspecified()) {
		return &net.AddrError{Err: "SSRF guard refusal", Addr: address}
	}
	return nil
}

// testFetcher builds a Fetcher that can reach the loopback mock at addr while
// still guarding every other dial (redirect hops included).
func testFetcher(maxBytes int64, mockAddr string) *Fetcher {
	return newFetcher(maxBytes, &net.Dialer{Timeout: 2 * time.Second, Control: permitOnly(mockAddr)})
}

func addrOf(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u := srv.URL
	u = strings.TrimPrefix(u, "http://")
	return u
}

// Signature.

func TestSignVerifyRoundTrip(t *testing.T) {
	svc := newTestService(t)
	url := "https://example.com/cat.png"
	path := svc.SignedPath(url)
	// Extract sig + exp from the minted path.
	sig, exp := parseSignedPath(t, path, url)
	if !svc.VerifySig(sig, url, exp) {
		t.Fatal("VerifySig on a freshly minted sig = false, want true")
	}
}

func TestVerifyRejectsForgedSig(t *testing.T) {
	svc := newTestService(t)
	url := "https://example.com/cat.png"
	_, exp := parseSignedPath(t, svc.SignedPath(url), url)
	if svc.VerifySig("deadbeef", url, exp) {
		t.Error("forged sig accepted")
	}
	// A valid sig for a DIFFERENT url must not verify against this url.
	otherSig, otherExp := parseSignedPath(t, svc.SignedPath("https://evil.example/x.png"), "https://evil.example/x.png")
	if svc.VerifySig(otherSig, url, otherExp) {
		t.Error("sig bound to another url accepted — url not covered by MAC")
	}
}

func TestVerifyBindsExpiry(t *testing.T) {
	svc := newTestService(t)
	url := "https://example.com/cat.png"
	sig, exp := parseSignedPath(t, svc.SignedPath(url), url)
	// Tampering with exp must invalidate the signature (exp is MAC-covered).
	if svc.VerifySig(sig, url, exp+1) {
		t.Error("sig verified against a tampered exp — expiry not covered by MAC")
	}
}

func TestPrevKeyRotation(t *testing.T) {
	// A sig minted under the OLD key must still verify while the old key sits in
	// the prev slot (rotation window).
	oldSvc, _ := New(testPrevKey, "", DefaultTTL, DefaultMaxBytes)
	url := "https://example.com/cat.png"
	sig, exp := parseSignedPath(t, oldSvc.SignedPath(url), url)

	// New service: current = new key, prev = old key.
	rotSvc, _ := New(testMasterKey, testPrevKey, DefaultTTL, DefaultMaxBytes)
	if !rotSvc.VerifySig(sig, url, exp) {
		t.Error("sig minted under prev key rejected during rotation")
	}
	// Without the prev slot, the old sig must NOT verify.
	newOnly, _ := New(testMasterKey, "", DefaultTTL, DefaultMaxBytes)
	if newOnly.VerifySig(sig, url, exp) {
		t.Error("old-key sig verified without the prev slot")
	}
}

func TestSubkeyIsDerivedNotMaster(t *testing.T) {
	// The signing subkey must not equal the raw master key bytes (key separation).
	sub, err := deriveSubkey(testMasterKey)
	if err != nil {
		t.Fatal(err)
	}
	if len(sub) != 32 {
		t.Fatalf("subkey len = %d, want 32", len(sub))
	}
	if string(sub) == testMasterKey {
		t.Error("subkey equals the master key string — no derivation")
	}
}

func TestDisabledServiceRefusesEverything(t *testing.T) {
	var s Service // zero value = disabled
	if s.Enabled() {
		t.Fatal("zero-value service reports enabled")
	}
	if s.VerifySig("x", "https://e.com/a.png", time.Now().Add(time.Hour).Unix()) {
		t.Error("disabled service verified a sig")
	}
	if s.AllowSign("k") {
		t.Error("disabled service allowed a sign")
	}
}

// SSRF.

func TestFetch_SSRF_DirectLoopbackRefused(t *testing.T) {
	// The PRODUCTION fetcher (real guarded dialer) must refuse a loopback target.
	f := NewFetcher(DefaultMaxBytes)
	_, err := f.Fetch(context.Background(), "http://127.0.0.1:80/x.png")
	if err == nil {
		t.Fatal("fetch to loopback succeeded, want SSRF refusal")
	}
	assertStatus(t, err, http.StatusBadGateway)
}

func TestFetch_SSRF_MetadataAndPrivateRefused(t *testing.T) {
	f := NewFetcher(DefaultMaxBytes)
	for _, target := range []string{
		"http://169.254.169.254/latest/meta-data/",
		"http://10.0.0.1/x.png",
		"http://192.168.1.1/x.png",
	} {
		if _, err := f.Fetch(context.Background(), target); err == nil {
			t.Errorf("fetch to %s succeeded, want SSRF refusal", target)
		}
	}
}

func TestFetch_SSRF_SchemeAllowlist(t *testing.T) {
	f := NewFetcher(DefaultMaxBytes)
	for _, target := range []string{"file:///etc/passwd", "gopher://x/", "data:image/png;base64,AAAA", "ftp://x/y"} {
		_, err := f.Fetch(context.Background(), target)
		if err == nil {
			t.Errorf("fetch of %s succeeded, want scheme rejection", target)
		}
		assertStatus(t, err, http.StatusBadRequest)
	}
}

func TestFetch_SSRF_RedirectHopRefused(t *testing.T) {
	// The classic Camo bypass (design §7.2): a public-looking upstream 302s to an
	// internal target. "internal" is modelled by a SECOND loopback mock serving a
	// perfectly valid PNG — reachable on the wire, forbidden by policy. The
	// redirect hop dials through the SAME guard, which refuses the loopback hop,
	// so the valid PNG is NEVER delivered.
	//
	// Red-probe: neutering dialControlForTest (return nil) lets the hop reach the
	// internal mock and the PNG comes back → this test goes red. The guard is what
	// blocks it, not mere unreachability.
	internal := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer internal.Close()

	entry := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, internal.URL+"/secret.png", http.StatusFound)
	}))
	defer entry.Close()

	// permitOnly admits ONLY the entry mock; the internal mock's loopback address
	// falls to the guard (dialControlForTest) → refused.
	f := testFetcher(DefaultMaxBytes, addrOf(t, entry))
	_, err := f.Fetch(context.Background(), entry.URL+"/img.png")
	if err == nil {
		t.Fatal("redirect to an internal target succeeded, want SSRF refusal on the hop")
	}
	assertStatus(t, err, http.StatusBadGateway)

	// Also: the production fetcher refuses the cloud-metadata IP outright.
	if _, err := NewFetcher(DefaultMaxBytes).Fetch(context.Background(), "http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Error("production fetch to 169.254.169.254 succeeded, want refusal")
	}
}

func TestFetch_RedirectCap(t *testing.T) {
	// A redirect loop must terminate at the cap, not spin.
	var srv *httptest.Server
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, srv.URL+"/again", http.StatusFound)
	}))
	defer srv.Close()
	f := testFetcher(DefaultMaxBytes, addrOf(t, srv))
	if _, err := f.Fetch(context.Background(), srv.URL+"/start"); err == nil {
		t.Fatal("redirect loop did not error, want redirect-cap refusal")
	}
}

// Size cap.

func TestFetch_SizeCap_Refused(t *testing.T) {
	const cap = 1024
	// Upstream streams more than the cap WITHOUT a Content-Length (chunked), so
	// only the LimitReader guard can stop it — proves no unbounded buffering.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		flusher, _ := w.(http.Flusher)
		chunk := make([]byte, 512)
		for i := 0; i < 8; i++ { // 4 KiB > 1 KiB cap
			_, _ = w.Write(chunk)
			if flusher != nil {
				flusher.Flush()
			}
		}
	}))
	defer mock.Close()
	f := testFetcher(cap, addrOf(t, mock))
	_, err := f.Fetch(context.Background(), mock.URL+"/big.png")
	if err == nil {
		t.Fatal("oversize upstream accepted, want size-cap refusal")
	}
	assertStatus(t, err, http.StatusBadGateway)
}

func TestFetch_SizeCap_ContentLengthPrecheck(t *testing.T) {
	const cap = 1024
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Content-Length", "999999")
		_, _ = w.Write(pngBytes)
	}))
	defer mock.Close()
	f := testFetcher(cap, addrOf(t, mock))
	_, err := f.Fetch(context.Background(), mock.URL+"/declared-big.png")
	if err == nil {
		t.Fatal("upstream declaring an oversize Content-Length accepted")
	}
	assertStatus(t, err, http.StatusBadGateway)
}

// Content-type allowlist.

func TestFetch_ContentType_PNGAccepted(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBytes)
	}))
	defer mock.Close()
	f := testFetcher(DefaultMaxBytes, addrOf(t, mock))
	res, err := f.Fetch(context.Background(), mock.URL+"/ok.png")
	if err != nil {
		t.Fatalf("valid PNG rejected: %v", err)
	}
	if res.ContentType != "image/png" {
		t.Errorf("served content-type = %q, want image/png", res.ContentType)
	}
}

func TestFetch_ContentType_SVGRefused(t *testing.T) {
	// SVG is an XSS vector — rejected whether declared honestly or masked.
	cases := []struct{ ct, body string }{
		{"image/svg+xml", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`},
		{"image/png", `<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`}, // lie: PNG header over SVG body
	}
	for _, c := range cases {
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", c.ct)
			_, _ = w.Write([]byte(c.body))
		}))
		f := testFetcher(DefaultMaxBytes, addrOf(t, mock))
		_, err := f.Fetch(context.Background(), mock.URL+"/x")
		mock.Close()
		if err == nil {
			t.Errorf("SVG (declared %q) accepted, want 415", c.ct)
			continue
		}
		assertStatus(t, err, http.StatusUnsupportedMediaType)
	}
}

func TestFetch_ContentType_HTMLRefused(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>hi</body></html>"))
	}))
	defer mock.Close()
	f := testFetcher(DefaultMaxBytes, addrOf(t, mock))
	_, err := f.Fetch(context.Background(), mock.URL+"/page")
	if err == nil {
		t.Fatal("text/html accepted, want 415")
	}
	assertStatus(t, err, http.StatusUnsupportedMediaType)
}

func TestFetch_ContentType_LyingHTMLAsPNGRefused(t *testing.T) {
	// Declared image/png but the bytes sniff as HTML → the sniff catches the lie.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("<!DOCTYPE html><script>alert(1)</script>"))
	}))
	defer mock.Close()
	f := testFetcher(DefaultMaxBytes, addrOf(t, mock))
	_, err := f.Fetch(context.Background(), mock.URL+"/lie")
	if err == nil {
		t.Fatal("HTML-behind-PNG accepted, want 415 on sniff")
	}
	assertStatus(t, err, http.StatusUnsupportedMediaType)
}

// Rate limit.

func TestRateLimiter_FixedWindow(t *testing.T) {
	rl := newRateLimiter(3, time.Minute)
	base := time.Now()
	rl.now = func() time.Time { return base }
	for i := 0; i < 3; i++ {
		if !rl.Allow("k") {
			t.Fatalf("hit %d denied within budget", i)
		}
	}
	if rl.Allow("k") {
		t.Error("4th hit allowed over budget")
	}
	// A different key has its own budget.
	if !rl.Allow("other") {
		t.Error("independent key denied")
	}
	// After the window rolls, the budget resets.
	rl.now = func() time.Time { return base.Add(time.Minute + time.Second) }
	if !rl.Allow("k") {
		t.Error("budget did not reset after the window")
	}
}

// helpers.

func parseSignedPath(t *testing.T, path, wantURL string) (sig string, exp int64) {
	t.Helper()
	// path = /api/img/<sig>?url=...&exp=...
	if !strings.HasPrefix(path, "/api/img/") {
		t.Fatalf("signed path %q missing /api/img/ prefix", path)
	}
	rest := strings.TrimPrefix(path, "/api/img/")
	qi := strings.IndexByte(rest, '?')
	if qi < 0 {
		t.Fatalf("signed path %q has no query", path)
	}
	sig = rest[:qi]
	vals, err := url.ParseQuery(rest[qi+1:])
	if err != nil {
		t.Fatal(err)
	}
	if got := vals.Get("url"); got != wantURL {
		t.Fatalf("signed url = %q, want %q", got, wantURL)
	}
	exp, err = strconv.ParseInt(vals.Get("exp"), 10, 64)
	if err != nil {
		t.Fatalf("exp %q not an int: %v", vals.Get("exp"), err)
	}
	return sig, exp
}

func assertStatus(t *testing.T, err error, want int) {
	t.Helper()
	var fe *FetchError
	if !errors.As(err, &fe) {
		t.Fatalf("error %v is not a *FetchError", err)
	}
	if fe.Status != want {
		t.Errorf("status = %d, want %d (err: %v)", fe.Status, want, err)
	}
}
