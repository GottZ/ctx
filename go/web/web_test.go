package web

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/GottZ/ctx/internal/handler"
)

// fixtureFS mimics a real Vite build output: hashed assets with .br/.gz
// siblings, an unhashed public/ file, and the index.html entry.
func fixtureFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html":            {Data: []byte("<!doctype html><html><body>ctx-spa-entry</body></html>")},
		"favicon.svg":           {Data: []byte("<svg xmlns='http://www.w3.org/2000/svg'/>")},
		"assets/app-abc123.js":  {Data: []byte("console.log('original-js')")},
		"assets/app-abc123.js.br": {Data: []byte("br-compressed-payload")},
		"assets/app-abc123.js.gz": {Data: []byte("gz-compressed-payload")},
		"assets/plain-xyz.css":  {Data: []byte(".plain{color:red}")},
	}
}

// placeholderFS models a build without frontend: dist/ carries only .gitkeep.
func placeholderFS() fstest.MapFS {
	return fstest.MapFS{".gitkeep": {Data: []byte("")}}
}

// stack wraps the web handler in the REAL SecurityHeaders middleware, which
// sets Cache-Control: no-store on every request — exactly what production
// does. The cache tests below prove the handler's override wins. Negative
// probe: removing the Set("Cache-Control", ...) lines in handlerFor makes
// TestCacheControlOverridesGlobalNoStore fail with "no-store".
func stack(h http.Handler) http.Handler {
	return handler.SecurityHeaders(h)
}

func doReq(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestSPAFallbackAcceptGuard: only HTML navigations get the history-API
// fallback; mistyped API URLs stay 404 so JSON clients never parse 200+HTML.
func TestSPAFallbackAcceptGuard(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	tests := []struct {
		name       string
		target     string
		accept     string
		wantStatus int
		wantSPA    bool
	}{
		{"html navigation to SPA route", "/settings", "text/html,application/xhtml+xml", http.StatusOK, true},
		{"deep link", "/settings/backends", "text/html", http.StatusOK, true},
		{"json client typo", "/api/missing", "application/json", http.StatusNotFound, false},
		{"wildcard accept (curl)", "/api/qery", "*/*", http.StatusNotFound, false},
		{"no accept header", "/noexist.js", "", http.StatusNotFound, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.accept != "" {
				hdr["Accept"] = tt.accept
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			if rec.Code != tt.wantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, tt.wantStatus)
			}
			gotSPA := strings.Contains(rec.Body.String(), "ctx-spa-entry")
			if gotSPA != tt.wantSPA {
				t.Errorf("SPA body served = %v, want %v (body %q)", gotSPA, tt.wantSPA, rec.Body.String())
			}
		})
	}
}

// TestCacheControlOverridesGlobalNoStore proves the handler replaces the
// global no-store through the real middleware stack: hashed assets become
// immutable, unhashed entry points become no-cache.
func TestCacheControlOverridesGlobalNoStore(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	tests := []struct {
		target string
		want   string
	}{
		{"/assets/app-abc123.js", "public, max-age=31536000, immutable"},
		{"/index.html", "no-cache"},
		{"/favicon.svg", "no-cache"},
		{"/", "no-cache"},
	}
	for _, tt := range tests {
		t.Run(tt.target, func(t *testing.T) {
			rec := doReq(t, h, http.MethodGet, tt.target, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Header().Get("Cache-Control"); got != tt.want {
				t.Errorf("Cache-Control = %q, want %q", got, tt.want)
			}
			// Sanity: the security middleware actually ran (override is an
			// override, not an unmounted middleware).
			if got := rec.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q — SecurityHeaders not in the stack?", got)
			}
		})
	}
}

// TestPreCompressedEncoding: .br/.gz selection by Accept-Encoding, original
// Content-Type, Vary on anything with compressed siblings.
func TestPreCompressedEncoding(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	tests := []struct {
		name         string
		target       string
		acceptEnc    string
		wantBody     string
		wantEncoding string
		wantVary     bool
	}{
		{"brotli preferred", "/assets/app-abc123.js", "gzip, deflate, br", "br-compressed-payload", "br", true},
		{"gzip only", "/assets/app-abc123.js", "gzip", "gz-compressed-payload", "gzip", true},
		{"identity client", "/assets/app-abc123.js", "", "console.log('original-js')", "", true},
		{"no compressed sibling", "/assets/plain-xyz.css", "gzip, br", ".plain{color:red}", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.acceptEnc != "" {
				hdr["Accept-Encoding"] = tt.acceptEnc
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200", rec.Code)
			}
			if got := rec.Body.String(); got != tt.wantBody {
				t.Errorf("body = %q, want %q", got, tt.wantBody)
			}
			if got := rec.Header().Get("Content-Encoding"); got != tt.wantEncoding {
				t.Errorf("Content-Encoding = %q, want %q", got, tt.wantEncoding)
			}
			ct := rec.Header().Get("Content-Type")
			if ct == "application/octet-stream" || ct == "" {
				t.Errorf("Content-Type = %q, want the ORIGINAL file's type", ct)
			}
			if strings.HasSuffix(tt.target, ".js") && !strings.Contains(ct, "javascript") {
				t.Errorf("Content-Type = %q, want a javascript type", ct)
			}
			gotVary := strings.Contains(rec.Header().Get("Vary"), "Accept-Encoding")
			if gotVary != tt.wantVary {
				t.Errorf("Vary Accept-Encoding = %v, want %v", gotVary, tt.wantVary)
			}
		})
	}
}

// wantCSP is the EXACT Content-Security-Policy byte string HTML responses must
// carry. It is a hand-written literal, deliberately NOT built from the package
// `csp` constant: TestCSPOnHTMLOnly only asserts `got == csp`, so a change to the
// constant (e.g. widening img-src to admit a foreign image host) would keep that
// test green while silently mutating the header. This literal pins the bytes, so
// ANY edit to the CSP — the img-src 'self' data: barrier in particular — turns
// TestCSPByteLiteralPin red before review (design 07-camo §4.6, Z-5). The Camo
// image proxy (W-CAMO-B) re-hosts foreign images under 'self' precisely so this
// string never needs to change; this pin is the proof it did not.
const wantCSP = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// TestCSPByteLiteralPin freezes the CSP header at the byte level. It asserts both
// that the package constant equals the pinned literal AND that a served HTML
// response emits exactly those bytes — so neither a constant edit nor a serving
// change can pass unnoticed. Negative probe: flipping any token in `csp`
// (web.go) — e.g. `img-src 'self' data: https://images.example` — makes this red
// while leaving TestCSPOnHTMLOnly green.
func TestCSPByteLiteralPin(t *testing.T) {
	if csp != wantCSP {
		t.Errorf("csp constant drifted from the pinned CSP:\n got  %q\n want %q", csp, wantCSP)
	}

	h := stack(handlerFor(fixtureFS()))
	rec := doReq(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	if got := rec.Header().Get("Content-Security-Policy"); got != wantCSP {
		t.Errorf("served CSP header drifted from the pinned CSP:\n got  %q\n want %q", got, wantCSP)
	}
}

// TestCSPOnHTMLOnly: HTML responses carry the CSP, asset responses do not.
func TestCSPOnHTMLOnly(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	tests := []struct {
		name    string
		target  string
		accept  string
		wantCSP bool
	}{
		{"root entry", "/", "", true},
		{"explicit index.html", "/index.html", "", true},
		{"spa fallback", "/settings", "text/html", true},
		{"hashed asset", "/assets/app-abc123.js", "", false},
		{"public file", "/favicon.svg", "", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hdr := map[string]string{}
			if tt.accept != "" {
				hdr["Accept"] = tt.accept
			}
			rec := doReq(t, h, http.MethodGet, tt.target, hdr)
			got := rec.Header().Get("Content-Security-Policy")
			if tt.wantCSP && got != csp {
				t.Errorf("CSP = %q, want the package csp constant", got)
			}
			if !tt.wantCSP && got != "" {
				t.Errorf("CSP = %q on non-HTML response, want none", got)
			}
		})
	}
}

// TestPlaceholderWithoutBuild: dist/ with only .gitkeep (go install / CI
// without bun) serves a 503 hint on HTML navigations and keeps non-HTML
// requests at 404 — and never panics.
func TestPlaceholderWithoutBuild(t *testing.T) {
	h := stack(handlerFor(placeholderFS()))

	rec := doReq(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "UI not built") {
		t.Errorf("body = %q, want the placeholder hint", rec.Body.String())
	}

	rec = doReq(t, h, http.MethodGet, "/settings", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("SPA route status = %d, want 503", rec.Code)
	}

	rec = doReq(t, h, http.MethodGet, "/api/anything", map[string]string{"Accept": "application/json"})
	if rec.Code != http.StatusNotFound {
		t.Errorf("API typo status = %d, want 404", rec.Code)
	}

	rec = doReq(t, h, http.MethodGet, "/", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("non-HTML / status = %d, want 404", rec.Code)
	}
}

// TestEmbeddedDistHandler exercises Handler() against the REAL embedded
// dist. It is build-state agnostic: with the committed .gitkeep placeholder
// it must answer 503, after a frontend build 200 — never panic, always
// no-cache on the entry response.
func TestEmbeddedDistHandler(t *testing.T) {
	h := stack(Handler())

	rec := doReq(t, h, http.MethodGet, "/", map[string]string{"Accept": "text/html"})
	if rec.Code != http.StatusOK && rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 200 (built) or 503 (placeholder)", rec.Code)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Errorf("Cache-Control = %q, want no-cache", got)
	}
}

// TestNoSPAFallbackForMutations: POST and friends never reach the SPA.
func TestNoSPAFallbackForMutations(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		rec := doReq(t, h, method, "/", map[string]string{"Accept": "text/html"})
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s / status = %d, want 404", method, rec.Code)
		}
	}
}

// TestPathTraversalStays404: cleaned/invalid paths cannot escape dist.
func TestPathTraversalStays404(t *testing.T) {
	h := stack(handlerFor(fixtureFS()))

	for _, target := range []string{"/../web.go", "/..%2fweb.go", "/assets/../../go.mod"} {
		req := httptest.NewRequest(http.MethodGet, "http://x", nil)
		req.URL.Path = strings.ReplaceAll(target, "%2f", "/")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code == http.StatusOK && !strings.Contains(rec.Body.String(), "ctx-spa-entry") {
			t.Errorf("GET %s served a non-SPA file (status %d)", target, rec.Code)
		}
	}
}

// TestCLIDoesNotImportWeb pins the release-pipeline invariant: cmd/ctx (the
// CLI that release.yml builds for 5 platforms and users fetch via
// `go install github.com/GottZ/ctx/cmd/ctx`) must never depend on this
// package, directly or transitively. The day it does, every release build
// would embed (and eventually require) frontend artifacts.
//
// Negative probe (must turn this test red): add
//
//	_ "github.com/GottZ/ctx/web"
//
// to cmd/ctx/main.go imports and re-run.
func TestCLIDoesNotImportWeb(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		// Compiled-test-binary-only environments lack the toolchain; CI and
		// local `go test` always have it, which is where the guard matters.
		t.Skipf("go binary not on PATH: %v", err)
	}
	cmd := exec.Command("go", "list", "-deps", "github.com/GottZ/ctx/cmd/ctx")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			t.Fatalf("go list -deps failed: %v\nstderr: %s", err, exitErr.Stderr)
		}
		t.Fatalf("go list -deps failed: %v", err)
	}
	for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(dep) == "github.com/GottZ/ctx/web" {
			t.Fatal("cmd/ctx imports github.com/GottZ/ctx/web — the CLI must stay web-free " +
				"so release.yml and `go install` never need the bun toolchain")
		}
	}
}
