package clientkey

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

const fixtureKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// cliNames are the CLI's words (internal/cli keyNames), so the texts below
// read as the CLI prints them.
var cliNames = Names{Client: "the ctx CLI", BaseURL: "CTX_BASE_URL"}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closeRecorder is a request body that remembers whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// TestKeyTransportOriginRule pins what "the origin of the base URL" means for
// the key: scheme, host and port, the default port filled in, host case
// folded in the ASCII range only. Everything else — another scheme on the same
// host included — is refused before the base transport sees the request.
func TestKeyTransportOriginRule(t *testing.T) {
	cases := []struct {
		name, base, to string
		sent           bool
	}{
		{"same origin, other path", "http://ctx.example", "http://ctx.example/api/query?x=1", true},
		{"base with a path", "https://ctx.example/sub/", "https://ctx.example/other", true},
		{"default port spelled out in the request", "http://ctx.example", "http://ctx.example:80/api", true},
		{"default port spelled out in the base", "https://ctx.example:443", "https://ctx.example/api", true},
		{"host case differs", "http://CTX.Example", "http://ctx.example/api", true},
		{"scheme case differs", "HTTP://ctx.example", "http://ctx.example/api", true},
		{"userinfo is not part of the origin", "http://ctx.example", "http://u:p@ctx.example/api", true},
		{"same explicit port", "http://10.0.0.5:8080", "http://10.0.0.5:8080/api", true},

		{"other host", "http://ctx.example", "http://elsewhere.example/api", false},
		{"subdomain", "http://ctx.example", "http://eu.ctx.example/api", false},
		{"parent domain", "http://eu.ctx.example", "http://ctx.example/api", false},
		{"trailing dot is another name", "http://ctx.example", "http://ctx.example./api", false},
		{"ip literal vs name", "http://127.0.0.1:8080", "http://localhost:8080/api", false},
		{"other port", "http://ctx.example", "http://ctx.example:8080/api", false},
		{"http to https on the same host", "http://ctx.example", "https://ctx.example/api", false},
		{"https to http on the same host", "https://ctx.example", "http://ctx.example/api", false},
		{"other scheme", "http://ctx.example", "ftp://ctx.example/api", false},
		{"no scheme", "http://ctx.example", "//ctx.example/api", false},
		// net/http dials the IDNA form of a non-ASCII host (xn--…): a Unicode
		// fold would call two hosts one origin.
		{"unicode İ is not i", "http://ikey.example", "http://%C4%B0key.example/api", false},
		{"kelvin sign is not k", "http://kkey.example", "http://Kkey.example/api", false},
		{"ipv6 zone keeps its case", "http://[fe80::1%25eth0]:8080", "http://[fe80::1%25ETH0]:8080/api", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotKey string
			sent := 0
			base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
				sent++
				gotKey = r.Header.Get(keyHeader)
				return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Request: r}, nil
			})
			kt, err := New(tc.base, "K", base, cliNames)
			if err != nil {
				t.Fatalf("New(%q): %v", tc.base, err)
			}
			to, err := url.Parse(tc.to)
			if err != nil {
				t.Fatal(err)
			}
			body := &closeRecorder{Reader: strings.NewReader("content")}
			req := &http.Request{Method: http.MethodPost, URL: to, Header: http.Header{}, Body: body}
			_, err = kt.RoundTrip(req)
			switch {
			case tc.sent && (err != nil || sent != 1 || gotKey != "K"):
				t.Errorf("%s → %s: err=%v sent=%d key=%q, want sent once with the key", tc.base, tc.to, err, sent, gotKey)
			case !tc.sent && (sent != 0 || !errors.As(err, new(*Refusal))):
				t.Errorf("%s → %s: err=%v sent=%d, want a refusal before sending", tc.base, tc.to, err, sent)
			case !tc.sent && !body.closed:
				t.Errorf("%s → %s: the refused request's body was not closed", tc.base, tc.to)
			}
			if req.Header.Get(keyHeader) != "" {
				t.Error("RoundTrip wrote the key into the caller's request")
			}
		})
	}
}

// TestKeyTransportNeedsAnHTTPBase: a base URL without an http or https host
// binds the key to nothing.
func TestKeyTransportNeedsAnHTTPBase(t *testing.T) {
	for _, base := range []string{"", "localhost:8080", "ftp://ctx.example", "http://", "http:///api", "http://ctx.example:bad"} {
		if _, err := New(base, "K", nil, cliNames); err == nil {
			t.Errorf("New(%q) accepted it", base)
		}
	}
}

// TestKeyRefusalAdvice: only an http→https upgrade on the same host gets the
// advice of the base URL to set; another host, another port and a downgrade do
// not.
func TestKeyRefusalAdvice(t *testing.T) {
	cases := []struct {
		base, to, advice string
	}{
		{"http://ctx.example", "https://ctx.example/api", "set CTX_BASE_URL to https://ctx.example if"},
		{"http://ctx.example:8080", "https://ctx.example:8080/api", "set CTX_BASE_URL to https://ctx.example:8080 if"},
		// The path of the base URL is kept (a server under a path prefix), its
		// userinfo is not repeated.
		{"http://ctx.example/ctx", "https://ctx.example/ctx/api/whoami", "set CTX_BASE_URL to https://ctx.example/ctx if"},
		{"http://u:pw@ctx.example/ctx/", "https://ctx.example/ctx/api", "set CTX_BASE_URL to https://ctx.example/ctx/ if"},
		{"http://ctx.example", "https://elsewhere.example/api", ""},
		{"http://ctx.example", "http://ctx.example:8080/api", ""},
		{"http://ctx.example:8080", "https://ctx.example:8443/api", ""},
		{"http://ctx.example", "https://ctx.example:8443/api", ""},
		{"https://ctx.example", "http://ctx.example/api", ""},
	}
	for _, tc := range cases {
		kt, err := New(tc.base, fixtureKey, roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("sent")
			return nil, nil
		}), cliNames)
		if err != nil {
			t.Fatal(err)
		}
		to, _ := url.Parse(tc.to)
		_, err = kt.RoundTrip(&http.Request{URL: to, Header: http.Header{}, Response: &http.Response{StatusCode: http.StatusFound}})
		msg := err.Error()
		if !strings.Contains(msg, "HTTP 302") || strings.Contains(msg, fixtureKey) {
			t.Errorf("%s → %s: %q", tc.base, tc.to, msg)
		}
		if tc.advice == "" && strings.Contains(msg, "set CTX_BASE_URL") {
			t.Errorf("%s → %s: advice given: %q", tc.base, tc.to, msg)
		}
		if tc.advice != "" && !strings.Contains(msg, tc.advice) {
			t.Errorf("%s → %s: %q lacks %q", tc.base, tc.to, msg, tc.advice)
		}
		if strings.Contains(msg, "pw") {
			t.Errorf("%s → %s: the refusal repeats userinfo: %q", tc.base, tc.to, msg)
		}
	}
}

// TestKeyRefusalText pins the refusal byte for byte under both callers' words:
// the program and the setting are the caller's, the rule and the origins are
// this package's. A target without a scheme or without a port is named as the
// URL wrote it.
func TestKeyRefusalText(t *testing.T) {
	sweep := Names{Client: "ctx-armsweep", BaseURL: "-base-url"}
	cases := []struct {
		names    Names
		base, to string
		status   int
		want     string
	}{
		{cliNames, "http://ctx.example:8080", "http://elsewhere.example/api", http.StatusTemporaryRedirect,
			"refused a redirect (HTTP 307) to http://elsewhere.example:80: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, http://ctx.example:8080, and follows a redirect only within it"},
		{cliNames, "http://ctx.example/ctx/", "https://ctx.example/ctx/api", http.StatusMovedPermanently,
			"refused a redirect (HTTP 301) to https://ctx.example:443: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, http://ctx.example:80, and follows a redirect only within it; set CTX_BASE_URL to https://ctx.example/ctx/ if that is the server's address"},
		{sweep, "http://ctx.example:8080", "https://ctx.example:8080/api/query", http.StatusFound,
			"refused a redirect (HTTP 302) to https://ctx.example:8080: ctx-armsweep sends its API key only to the origin of -base-url, http://ctx.example:8080, and follows a redirect only within it; set -base-url to https://ctx.example:8080 if that is the server's address"},
		{sweep, "https://ctx.example", "http://[::1]:9/api", 0,
			"refused a request to http://[::1]:9: ctx-armsweep sends its API key only to the origin of -base-url, https://ctx.example:443, and follows a redirect only within it"},
		{cliNames, "http://ctx.example", "//elsewhere.example:8080/health", 0,
			"refused a request to //elsewhere.example:8080: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, http://ctx.example:80, and follows a redirect only within it"},
		{cliNames, "http://ctx.example", "//[fe80::1%25eth0]/health", 0,
			"refused a request to //[fe80::1%eth0]: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, http://ctx.example:80, and follows a redirect only within it"},
		{cliNames, "http://ctx.example", "ftp://ctx.example/f", 0,
			"refused a request to ftp://ctx.example: the ctx CLI sends its API key only to the origin of CTX_BASE_URL, http://ctx.example:80, and follows a redirect only within it"},
	}
	for _, tc := range cases {
		kt, err := New(tc.base, fixtureKey, nil, tc.names)
		if err != nil {
			t.Fatal(err)
		}
		to, err := url.Parse(tc.to)
		if err != nil {
			t.Fatal(err)
		}
		req := &http.Request{URL: to, Header: http.Header{}}
		if tc.status != 0 {
			req.Response = &http.Response{StatusCode: tc.status}
		}
		_, err = kt.RoundTrip(req)
		if err == nil || err.Error() != tc.want {
			t.Errorf("%s → %s:\n got %v\nwant %s", tc.base, tc.to, err, tc.want)
		}
	}
}

// TestKeyDoLeavesTheClientAlone: Do sends through a copy of the caller's
// http.Client, so the rule never becomes the CheckRedirect of a client the
// caller also uses for requests without the key, and a request on the origin
// carries the key once.
func TestKeyDoLeavesTheClientAlone(t *testing.T) {
	var keyed atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(keyHeader) == fixtureKey {
			keyed.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	kt, err := New(srv.URL, fixtureKey, nil, cliNames)
	if err != nil {
		t.Fatal(err)
	}
	hc := &http.Client{}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/api/whoami", nil)
	resp, err := kt.Do(hc, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if hc.CheckRedirect != nil {
		t.Error("Do set the CheckRedirect of the caller's client")
	}
	if n := keyed.Load(); n != 1 {
		t.Errorf("the key arrived %d times, want 1", n)
	}
	if req.Header.Get(keyHeader) != "" {
		t.Error("Do wrote the key into the caller's request")
	}
}

// TestBearerTransport (#432): a Transport from NewBearer sends the key as
// "Authorization: Bearer <key>" and not as X-Context-Key, on New's origin
// rule — another port of the same host is refused before it is sent, with
// New's refusal — and refuses a base URL New refuses.
func TestBearerTransport(t *testing.T) {
	var bearer, contextKey, other atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+fixtureKey {
			bearer.Add(1)
		}
		if r.Header.Get(keyHeader) != "" {
			contextKey.Add(1)
		}
	}))
	t.Cleanup(srv.Close)
	elsewhere := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	t.Cleanup(elsewhere.Close)
	names := Names{Client: "ctx-goldbench", BaseURL: "-endpoint"}
	kt, err := NewBearer(srv.URL+"/v1/chat/completions", fixtureKey, names)
	if err != nil {
		t.Fatal(err)
	}
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/chat/completions", strings.NewReader("{}"))
	resp, err := kt.Do(&http.Client{}, req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if bearer.Load() != 1 || contextKey.Load() != 0 {
		t.Errorf("bearer=%d X-Context-Key=%d, want the key once as a bearer token and never as X-Context-Key", bearer.Load(), contextKey.Load())
	}
	if req.Header.Get("Authorization") != "" {
		t.Error("Do wrote the key into the caller's request")
	}

	away, _ := http.NewRequest(http.MethodPost, elsewhere.URL+"/v1/chat/completions", strings.NewReader("{}"))
	_, err = kt.Do(&http.Client{}, away)
	plain, _ := New(srv.URL+"/v1/chat/completions", fixtureKey, nil, names)
	_, want := plain.Do(&http.Client{}, away.Clone(away.Context()))
	if !errors.As(err, new(*Refusal)) || want == nil || err.Error() != want.Error() || other.Load() != 0 {
		t.Errorf("err=%v (New: %v), sent elsewhere %d times — want New's refusal before sending", err, want, other.Load())
	}

	if _, err := NewBearer("ftp://ctx.example", fixtureKey, names); err == nil {
		t.Error("NewBearer accepted a base URL without an http or https host")
	}
}

// TestKeyCredentialOnlyInKeyTransport is the structural half of #413, #423
// and #432: a request helper that put the key on a request itself would bypass
// the origin rule. So no source of a package that can hold a ctx API key
// spells a credential header — X-Context-Key, or Authorization, which the
// server takes a key in as well — except the credentialConsts, only
// Transport.signed uses them, and a header name those packages set in one of
// the forms computedHeaderNames reads is a literal (a syntax scan, not a type
// check: the rule itself is held by the transport, this test keeps the code
// from routing around it by accident). The packages: this one, the three that
// send through it and their binaries, and every other package that imports
// internal/clientconfig (where the key is resolved) or this one — a new tool
// joins the scan by its import. Subdirectories are scanned too.
func TestKeyCredentialOnlyInKeyTransport(t *testing.T) {
	credential := map[string]bool{"X-Context-Key": true, "Authorization": true}
	paths := keyedSources(t)

	fset := token.NewFileSet()
	var literals, computed []string
	declared, uses := map[string]int{}, map[string][]string{}
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		allowedLit := map[*ast.BasicLit]bool{}
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				for _, s := range gd.Specs {
					vs := s.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if isCredentialConst(n.Name) && i < len(vs.Values) {
							declared[n.Name]++
							if lit, ok := vs.Values[i].(*ast.BasicLit); ok {
								allowedLit[lit] = true
							}
						}
					}
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || allowedLit[lit] {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && credential[http.CanonicalHeaderKey(strings.TrimSpace(v))] {
				literals = append(literals, fset.Position(lit.Pos()).String())
			}
			return true
		})
		computed = append(computed, computedHeaderNames(fset, f)...)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && isCredentialConst(id.Name) {
					uses[id.Name] = append(uses[id.Name], funcName(fd)+" "+fset.Position(id.Pos()).String())
				}
				return true
			})
		}
	}
	for _, name := range credentialConsts {
		if declared[name] != 1 {
			t.Fatalf("%s declared %d times, want once", name, declared[name])
		}
	}
	if len(literals) != 0 {
		t.Errorf("a credential header is spelled outside %v — attach the key through clientkey.Transport instead: %v", credentialConsts, literals)
	}
	if len(computed) != 0 {
		t.Errorf("a header name is not a string literal, so this scan cannot tell whether it is a credential — spell it out, or attach the key through clientkey.Transport: %v", computed)
	}
	for _, name := range credentialConsts {
		if len(uses[name]) == 0 {
			t.Errorf("Transport.signed no longer uses %s — this gate lost its subject", name)
		}
		for _, u := range uses[name] {
			if !strings.HasPrefix(u, "Transport.signed ") {
				t.Errorf("%s used outside Transport.signed: %s", name, u)
			}
		}
	}
}

// credentialConsts are the constants that spell a credential header: the key
// header of New and the bearer header of NewBearer. Each is declared once, in
// this package, and used in Transport.signed only.
var credentialConsts = []string{"keyHeader", "bearerHeader"}

func isCredentialConst(name string) bool {
	for _, c := range credentialConsts {
		if name == c {
			return true
		}
	}
	return false
}

// keyedRoots are scanned whatever they import: this package, the three that
// send through it, and the binaries built on those three (cmd/ctx imports
// internal/cli only).
var keyedRoots = []string{".", "../cli", "../../cmd/ctx", "../armsweep", "../../cmd/ctx-armsweep", "../goldbench", "../../cmd/ctx-goldbench"}

// keyedSources lists the non-test Go files of keyedRoots and of every package
// of the module that imports internal/clientconfig or this package, each
// directory with its subdirectories.
func keyedSources(t *testing.T) []string {
	t.Helper()
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	module := abs(filepath.Join("..", ".."))
	dirs := map[string]bool{}
	for _, r := range keyedRoots {
		dirs[abs(r)] = true
	}
	fset := token.NewFileSet()
	err := filepath.WalkDir(module, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); p != module && (n == "testdata" || n == "node_modules" || strings.HasPrefix(n, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, p, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, imp := range f.Imports {
			if v, _ := strconv.Unquote(imp.Path.Value); v == "github.com/GottZ/ctx/internal/clientconfig" || v == "github.com/GottZ/ctx/internal/clientkey" {
				dirs[filepath.Dir(p)] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	seen := map[string]bool{}
	perRoot := map[string]int{}
	for dir := range dirs {
		err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() && p != dir && (d.Name() == "testdata" || strings.HasPrefix(d.Name(), ".")) {
				return filepath.SkipDir
			}
			if n := d.Name(); !d.IsDir() && strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				seen[p] = true
				perRoot[dir]++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range keyedRoots {
		if perRoot[abs(r)] == 0 {
			t.Fatalf("%s holds no source file — the scan lost its subject", r)
		}
	}
	paths := make([]string, 0, len(seen))
	for p := range seen {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) < 50 {
		t.Fatalf("found %d source files — the scan lost its subject", len(paths))
	}
	return paths
}

// computedHeaderNames lists every header name in f that is not a string
// literal, except the credentialConsts, whose uses the caller checks. A literal is
// checked against the credential names; anything else (a constant from another
// package, a concatenation) would slip past that check. It reads, by syntax:
// the name argument of Set and Add called on a header — x.Header, x.Header(),
// an http.Header literal, or a name that holds one in this file (assigned from
// one of those, or declared as http.Header: a variable, a parameter, a result)
// — and of the method expressions http.Header.Set/Add; an index into such a
// header; a key of an http.Header literal. A header reached through anything
// else (a struct field of type http.Header, a Clone) is not seen.
func computedHeaderNames(fset *token.FileSet, f *ast.File) []string {
	isHeaderType := func(x ast.Expr) bool { // http.Header
		sel, ok := x.(*ast.SelectorExpr)
		if !ok || sel.Sel.Name != "Header" {
			return false
		}
		id, ok := sel.X.(*ast.Ident)
		return ok && id.Name == "http"
	}
	aliases := map[string]bool{}
	var isHeader func(x ast.Expr) bool
	isHeader = func(x ast.Expr) bool {
		switch e := x.(type) {
		case *ast.SelectorExpr:
			return e.Sel.Name == "Header" && !isHeaderType(e)
		case *ast.CallExpr:
			sel, ok := e.Fun.(*ast.SelectorExpr)
			return ok && sel.Sel.Name == "Header"
		case *ast.CompositeLit:
			return isHeaderType(e.Type)
		case *ast.Ident:
			return aliases[e.Name]
		case *ast.ParenExpr:
			return isHeader(e.X)
		}
		return false
	}
	// Aliases first, so a use before the assignment in file order is seen too.
	for changed := true; changed; {
		changed = false
		mark := func(id *ast.Ident) {
			if id != nil && id.Name != "_" && !aliases[id.Name] {
				aliases[id.Name], changed = true, true
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch e := n.(type) {
			case *ast.AssignStmt:
				if len(e.Lhs) != len(e.Rhs) {
					break
				}
				for i, r := range e.Rhs {
					if id, ok := e.Lhs[i].(*ast.Ident); ok && isHeader(r) {
						mark(id)
					}
				}
			case *ast.ValueSpec:
				for i, id := range e.Names {
					if (e.Type != nil && isHeaderType(e.Type)) || (i < len(e.Values) && isHeader(e.Values[i])) {
						mark(id)
					}
				}
			case *ast.Field:
				if isHeaderType(e.Type) {
					for _, id := range e.Names {
						mark(id)
					}
				}
			}
			return true
		})
	}
	var out []string
	check := func(name ast.Expr) {
		if lit, ok := name.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			return
		}
		if id, ok := name.(*ast.Ident); ok && isCredentialConst(id.Name) {
			return
		}
		out = append(out, fset.Position(name.Pos()).String())
	}
	ast.Inspect(f, func(n ast.Node) bool {
		switch e := n.(type) {
		case *ast.CallExpr:
			sel, ok := e.Fun.(*ast.SelectorExpr)
			if !ok || (sel.Sel.Name != "Set" && sel.Sel.Name != "Add") {
				break
			}
			switch {
			case isHeaderType(sel.X) && len(e.Args) > 1: // http.Header.Set(h, name, v)
				check(e.Args[1])
			case isHeader(sel.X) && len(e.Args) > 0:
				check(e.Args[0])
			}
		case *ast.IndexExpr:
			if isHeader(e.X) {
				check(e.Index)
			}
		case *ast.CompositeLit:
			if isHeaderType(e.Type) {
				for _, el := range e.Elts {
					if kv, ok := el.(*ast.KeyValueExpr); ok {
						check(kv.Key)
					}
				}
			}
		}
		return true
	})
	return out
}

// TestComputedHeaderNamesSeesTheForms holds the detector to the forms its
// comment names: every computed name below is reported, and the literal ones
// are not.
func TestComputedHeaderNamesSeesTheForms(t *testing.T) {
	const src = `package p

import "net/http"

const other = "X-Other"

func f(req *http.Request, k string, hd http.Header) (out http.Header) {
	req.Header.Set("Content-Type", "a")
	req.Header.Set(other, k)
	h := req.Header
	h.Set("X-"+"Context-Key", k)
	hd.Add(other, k)
	http.Header.Set(req.Header, other, k)
	req.Header[other] = []string{k}
	_ = http.Header{other: {k}, "Accept": {"a"}}
	var g http.Header = req.Header
	g.Set(other, k)
	out = http.Header{}
	out.Add(other, k)
	later.Set(other, k)
	later := req.Header
	(h).Set("Accept", k)
	return out
}
`
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "p.go", src, 0)
	if err != nil {
		t.Fatal(err)
	}
	var lines []int
	for _, pos := range computedHeaderNames(fset, f) {
		_, line, _ := strings.Cut(pos, ":")
		n, _ := strconv.Atoi(strings.SplitN(line, ":", 2)[0])
		lines = append(lines, n)
	}
	want := []int{9, 11, 12, 13, 14, 15, 17, 19, 20}
	if len(lines) != len(want) {
		t.Fatalf("reported lines %v, want %v", lines, want)
	}
	for i := range want {
		if lines[i] != want[i] {
			t.Fatalf("reported lines %v, want %v", lines, want)
		}
	}
}

func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	typ := fd.Recv.List[0].Type
	if star, ok := typ.(*ast.StarExpr); ok {
		typ = star.X
	}
	if id, ok := typ.(*ast.Ident); ok {
		return id.Name + "." + fd.Name.Name
	}
	return fd.Name.Name
}
