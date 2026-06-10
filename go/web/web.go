// Package web embeds and serves the admin SPA (Svelte 5 + TypeScript + Vite).
//
// The dist/ directory is populated by the frontend build (Docker frontend
// stage or `bun run build` inside go/web). A committed dist/.gitkeep keeps
// the //go:embed pattern satisfiable, so `go build`, `go install`, CI and
// pre-commit lint never need the web toolchain. Such placeholder builds
// serve a 503 hint instead of the UI while every API route stays fully
// functional.
//
// Invariant guarded by TestCLIDoesNotImportWeb: cmd/ctx (the CLI built by
// release.yml for 5 platforms and installed via `go install`) must never
// import this package — that keeps the release pipeline bun-free.
package web

import (
	"embed"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"
)

// distFS holds the built SPA. The all: prefix also embeds dot-files, so the
// committed dist/.gitkeep placeholder satisfies the pattern without bun.
//
//go:embed all:dist
var distFS embed.FS

// csp is the Content-Security-Policy for HTML responses only (not assets,
// not API routes — those keep the global SecurityHeaders set).
//
//   - script-src 'self': the Svelte production build emits external JS/CSS,
//     no inline scripts; lazy chunks and modulepreload are same-origin.
//   - style-src 'unsafe-inline': static style="..." attributes in Svelte
//     templates count as inline styles; without it component positioning
//     breaks in browsers lacking style-src-attr differentiation.
//   - worker-src blob: graph rendering (F5) uses Blob-URL web workers;
//     workers remain bound by connect-src, so no extra exfil channel.
//   - connect-src/form-action/img-src 'self' are the API-key exfiltration
//     barrier (fetch/XHR/beacon, form posts, pixel exfil). CSP cannot block
//     navigation, so this reduces — not eliminates — XSS impact.
//   - frame-ancestors 'none' mirrors the global X-Frame-Options: DENY.
const csp = "default-src 'self'; script-src 'self'; " +
	"style-src 'self' 'unsafe-inline'; img-src 'self' data:; " +
	"font-src 'self' data:; connect-src 'self'; worker-src 'self' blob:; " +
	"frame-ancestors 'none'; base-uri 'none'; form-action 'self'; object-src 'none'"

// placeholderBody is served with status 503 when dist/ carries no
// index.html, i.e. the binary was built without the frontend stage.
const placeholderBody = "ctx UI not built — use the Docker image or run bun build\n"

// encodings lists the pre-compressed variants in preference order. The
// frontend build emits .br/.gz siblings next to the originals; serving them
// avoids any on-the-fly compression middleware (response-writer wrappers
// without Unwrap() silently break the heartbeat/streaming flush path).
var encodings = [...]struct{ name, ext string }{
	{"br", ".br"},
	{"gzip", ".gz"},
}

// Handler serves the embedded SPA: hashed /assets/* immutable, history-API
// fallback to index.html (Accept: text/html only), pre-compressed .br/.gz
// negotiation, CSP on HTML responses, 503 placeholder without a build.
func Handler() http.Handler {
	sub, err := fs.Sub(distFS, "dist")
	if err != nil {
		// The embed directive guarantees dist/ exists at compile time;
		// reaching this is a build-layout bug, not a runtime condition.
		panic("web: embedded dist/ missing: " + err.Error())
	}
	return handlerFor(sub)
}

// handlerFor implements the serving semantics on any fs.FS. Tests inject
// fstest fixtures; Handler wires the embedded dist.
func handlerFor(fsys fs.FS) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// No SPA fallback for mutations — POST / stays 404.
			http.NotFound(w, r)
			return
		}
		p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
		if p == "" || p == "." {
			p = "index.html"
		}

		if !regularFileExists(fsys, p) {
			// History-API fallback only for HTML navigations: a mistyped
			// API URL must stay a 404 — JSON clients would otherwise parse
			// 200+HTML as a success envelope.
			if !acceptsHTML(r) {
				http.NotFound(w, r)
				return
			}
			serveIndex(w, r, fsys)
			return
		}

		// Cache override: the global SecurityHeaders middleware sets
		// Cache-Control: no-store on every request; headers commit at
		// WriteHeader, so Set replaces it for static assets only. API
		// routes never reach this handler and keep no-store.
		if strings.HasPrefix(p, "assets/") {
			// Content-hashed by Vite — safe to cache forever.
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			// index.html and public/ files (favicon, ...) are NOT hashed.
			w.Header().Set("Cache-Control", "no-cache")
		}
		if strings.HasSuffix(p, ".html") {
			w.Header().Set("Content-Security-Policy", csp)
		}
		serveFile(w, r, fsys, p)
	})
}

// serveIndex serves the SPA entry point for / and history-API fallbacks, or
// the 503 placeholder when the binary was built without a frontend.
func serveIndex(w http.ResponseWriter, r *http.Request, fsys fs.FS) {
	w.Header().Set("Cache-Control", "no-cache")
	if !regularFileExists(fsys, "index.html") {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		if r.Method != http.MethodHead {
			_, _ = io.WriteString(w, placeholderBody)
		}
		return
	}
	w.Header().Set("Content-Security-Policy", csp)
	serveFile(w, r, fsys, "index.html")
}

// serveFile writes the file body, preferring a pre-compressed .br/.gz
// sibling when the client accepts it. Content-Type always derives from the
// ORIGINAL name — http.ServeContent on a .br path would otherwise emit
// application/octet-stream.
func serveFile(w http.ResponseWriter, r *http.Request, fsys fs.FS, p string) {
	serve := p
	if hasCompressedSibling(fsys, p) {
		// Caches must key on the encoding even when serving identity.
		w.Header().Add("Vary", "Accept-Encoding")
		if enc, cp := negotiateEncoding(r, fsys, p); enc != "" {
			w.Header().Set("Content-Encoding", enc)
			serve = cp
		}
	}
	w.Header().Set("Content-Type", contentType(p))
	serveContent(w, r, fsys, serve)
}

// serveContent streams a single file. http.ServeContent is used for its
// HEAD/Range handling but with the path passed through verbatim — unlike
// http.ServeFile it performs no /index.html redirect magic.
func serveContent(w http.ResponseWriter, r *http.Request, fsys fs.FS, p string) {
	f, err := fsys.Open(p)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		// fs.FS without seek support: write linearly, no range requests.
		w.WriteHeader(http.StatusOK)
		if r.Method != http.MethodHead {
			_, _ = io.Copy(w, f)
		}
		return
	}
	// embed.FS carries no modtimes (zero time) → no Last-Modified header.
	http.ServeContent(w, r, p, time.Time{}, rs)
}

// contentType resolves the MIME type from the original file extension.
func contentType(p string) string {
	if ct := mime.TypeByExtension(path.Ext(p)); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// hasCompressedSibling reports whether any pre-compressed variant exists.
func hasCompressedSibling(fsys fs.FS, p string) bool {
	for _, enc := range encodings {
		if regularFileExists(fsys, p+enc.ext) {
			return true
		}
	}
	return false
}

// negotiateEncoding picks the first encoding the client accepts that has a
// pre-compressed sibling. Returns ("", "") for identity.
func negotiateEncoding(r *http.Request, fsys fs.FS, p string) (encoding, compressedPath string) {
	ae := r.Header.Get("Accept-Encoding")
	for _, enc := range encodings {
		if !strings.Contains(ae, enc.name) {
			continue
		}
		if cp := p + enc.ext; regularFileExists(fsys, cp) {
			return enc.name, cp
		}
	}
	return "", ""
}

// regularFileExists reports whether p names an embedded regular file.
func regularFileExists(fsys fs.FS, p string) bool {
	if !fs.ValidPath(p) {
		return false
	}
	info, err := fs.Stat(fsys, p)
	return err == nil && !info.IsDir()
}

// acceptsHTML reports whether the request looks like a browser HTML
// navigation. JSON/CLI clients (Accept: application/json or */*) never
// receive the SPA fallback.
func acceptsHTML(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "text/html")
}
