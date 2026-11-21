package cli

import (
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

// closeRecorder is a request body that remembers whether it was closed.
type closeRecorder struct {
	io.Reader
	closed bool
}

func (c *closeRecorder) Close() error { c.closed = true; return nil }

// TestKeyTransportOriginRule pins what "the origin of CTX_BASE_URL" means for
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
			kt, err := newKeyTransport(tc.base, "K", base)
			if err != nil {
				t.Fatalf("newKeyTransport(%q): %v", tc.base, err)
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
			case !tc.sent && (sent != 0 || !errors.As(err, new(*keyOriginRefusal))):
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

// TestKeyTransportNeedsAnHTTPBase: a CTX_BASE_URL without an http or https
// host binds the key to nothing.
func TestKeyTransportNeedsAnHTTPBase(t *testing.T) {
	for _, base := range []string{"", "localhost:8080", "ftp://ctx.example", "http://", "http:///api", "http://ctx.example:bad"} {
		if _, err := newKeyTransport(base, "K", nil); err == nil {
			t.Errorf("newKeyTransport(%q) accepted it", base)
		}
	}
}

// TestKeyRefusalAdvice: only an http→https upgrade on the same host gets the
// CTX_BASE_URL advice; another host, another port and a downgrade do not.
func TestKeyRefusalAdvice(t *testing.T) {
	cases := []struct {
		base, to, advice string
	}{
		{"http://ctx.example", "https://ctx.example/api", "set CTX_BASE_URL to https://ctx.example if"},
		{"http://ctx.example:8080", "https://ctx.example:8080/api", "set CTX_BASE_URL to https://ctx.example:8080 if"},
		// The path of CTX_BASE_URL is kept (a server under a path prefix),
		// its userinfo is not repeated.
		{"http://ctx.example/ctx", "https://ctx.example/ctx/api/whoami", "set CTX_BASE_URL to https://ctx.example/ctx if"},
		{"http://u:pw@ctx.example/ctx/", "https://ctx.example/ctx/api", "set CTX_BASE_URL to https://ctx.example/ctx/ if"},
		{"http://ctx.example", "https://elsewhere.example/api", ""},
		{"http://ctx.example", "http://ctx.example:8080/api", ""},
		{"http://ctx.example:8080", "https://ctx.example:8443/api", ""},
		{"http://ctx.example", "https://ctx.example:8443/api", ""},
		{"https://ctx.example", "http://ctx.example/api", ""},
	}
	for _, tc := range cases {
		kt, err := newKeyTransport(tc.base, keyOriginFixtureKey, roundTripFunc(func(*http.Request) (*http.Response, error) {
			t.Fatal("sent")
			return nil, nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		to, _ := url.Parse(tc.to)
		_, err = kt.RoundTrip(&http.Request{URL: to, Header: http.Header{}, Response: &http.Response{StatusCode: http.StatusFound}})
		msg := err.Error()
		if !strings.Contains(msg, "HTTP 302") || strings.Contains(msg, keyOriginFixtureKey) {
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

// TestKeyCredentialOnlyInKeyTransport is the structural half of #413: a new
// request helper that put the key on a request itself would bypass the origin
// rule. So no CLI source spells a credential header — X-Context-Key, or
// Authorization, which the server takes a key in as well — except the
// keyHeader constant, and only keyTransport.signed uses that constant.
func TestKeyCredentialOnlyInKeyTransport(t *testing.T) {
	credential := map[string]bool{"X-Context-Key": true, "Authorization": true}
	var paths []string
	for _, dir := range []string{".", filepath.Join("..", "..", "cmd", "ctx")} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if n := e.Name(); strings.HasSuffix(n, ".go") && !strings.HasSuffix(n, "_test.go") {
				paths = append(paths, filepath.Join(dir, n))
			}
		}
	}
	if len(paths) < 20 {
		t.Fatalf("found %d source files — the scan lost its subject", len(paths))
	}

	fset := token.NewFileSet()
	var literals, uses []string
	declared := 0
	for _, path := range paths {
		f, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		var allowedLit *ast.BasicLit
		for _, d := range f.Decls {
			if gd, ok := d.(*ast.GenDecl); ok && gd.Tok == token.CONST {
				for _, s := range gd.Specs {
					vs := s.(*ast.ValueSpec)
					for i, n := range vs.Names {
						if n.Name == keyHeaderName && i < len(vs.Values) {
							declared++
							allowedLit, _ = vs.Values[i].(*ast.BasicLit)
						}
					}
				}
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING || lit == allowedLit {
				return true
			}
			if v, err := strconv.Unquote(lit.Value); err == nil && credential[http.CanonicalHeaderKey(strings.TrimSpace(v))] {
				literals = append(literals, fset.Position(lit.Pos()).String())
			}
			return true
		})
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				if id, ok := n.(*ast.Ident); ok && id.Name == keyHeaderName {
					uses = append(uses, funcName(fd)+" "+fset.Position(id.Pos()).String())
				}
				return true
			})
		}
	}
	if declared != 1 {
		t.Fatalf("%s declared %d times, want once", keyHeaderName, declared)
	}
	if len(literals) != 0 {
		t.Errorf("a credential header is spelled outside keyHeader — attach the key through keyTransport (Client.send) instead: %v", literals)
	}
	if len(uses) == 0 {
		t.Error("keyTransport.signed no longer uses keyHeader — this gate lost its subject")
	}
	for _, u := range uses {
		if !strings.HasPrefix(u, "keyTransport.signed ") {
			t.Errorf("keyHeader used outside keyTransport.signed: %s", u)
		}
	}
}

const keyHeaderName = "keyHeader"

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
