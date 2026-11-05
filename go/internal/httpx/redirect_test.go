package httpx

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// F-W1 (design/04 I17, S5): the pool clients follow a redirect only while it
// stays on the origin of the first request, or upgrades it to https on the
// same host. These tests drive the two production constructors over the
// network; the pure origin rule is pinned in redirect_rule_test.go.

// redirectPrompt is the request body every probe sends: if it shows up at a
// host the redirect must not reach, the failure message prints it verbatim.
const redirectPrompt = `{"messages":[{"role":"user","content":"PROMPT-MARKER-F-W1"}]}`

// redirectSink is a test endpoint that records every request it receives
// (method, path, the two credential headers a wire may carry, body).
type redirectSink struct {
	srv  *httptest.Server
	mu   sync.Mutex
	seen []string
}

func newRedirectSink(t *testing.T, useTLS bool, h http.HandlerFunc) *redirectSink {
	t.Helper()
	s := &redirectSink{}
	fn := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.seen = append(s.seen, fmt.Sprintf("%s %s x-api-key=%q authorization=%q body=%s",
			r.Method, r.URL.RequestURI(), r.Header.Get("X-Api-Key"), r.Header.Get("Authorization"), b))
		s.mu.Unlock()
		if h != nil {
			h(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"ok":true}`)
	})
	if useTLS {
		s.srv = httptest.NewTLSServer(fn)
	} else {
		s.srv = httptest.NewServer(fn)
	}
	t.Cleanup(s.srv.Close)
	return s
}

func (s *redirectSink) addr() string { return s.srv.Listener.Addr().String() }

func (s *redirectSink) hits() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.seen)
}

func (s *redirectSink) log() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return strings.Join(s.seen, " | ")
}

// redirectTo answers every request with code and Location loc.
func redirectTo(code int, loc string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", loc)
		w.WriteHeader(code)
		_, _ = io.WriteString(w, "moved")
	}
}

// poolConstructors are the two production clients under test.
var poolConstructors = []struct {
	name  string
	build func() *http.Client
}{
	{"open", PooledClient},
	{"guarded", GuardedPooledClient},
}

// rewire replaces only the dial step of c's transport with a fixed
// host:port → listener table, so a test can name distinct hosts
// (api.example, elsewhere.example, example.com on 80 and 443) while every
// listener sits on loopback. CheckRedirect — the code under test — stays the
// constructor's own; for the guarded client the table stands in for public
// hosts, which ssrfguard admits by design (it judges addresses, not origins).
func rewire(t *testing.T, c *http.Client, routes map[string]string, roots *x509.CertPool) *http.Client {
	t.Helper()
	tr := c.Transport.(*http.Transport).Clone()
	var d net.Dialer
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		target, ok := routes[addr]
		if !ok {
			return nil, fmt.Errorf("test dialer: no route to %s", addr)
		}
		return d.DialContext(ctx, network, target)
	}
	tr.TLSClientConfig = &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12}
	c.Transport = tr
	t.Cleanup(tr.CloseIdleConnections)
	return c
}

// postWithCredentials sends the probe POST the way the wires do: a
// bytes.Reader body (so GetBody is set and net/http may replay it) plus the
// two credential headers the model wires carry.
func postWithCredentials(t *testing.T, c *http.Client, url string) (int, error) {
	t.Helper()
	body := []byte(redirectPrompt)
	req := newReq(t, context.Background(), url, body)
	req.Header.Set("Authorization", "Bearer row-key")
	req.Header.Set("X-Api-Key", "row-key")
	resp, err := DoRetryOnce(c, req, body)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// TestPoolClientsRefuseCrossOriginRedirect — Gate (a) on the client: for
// every redirect status, a Location on another host is NOT followed. The
// caller gets the 3xx itself (http.ErrUseLastResponse) and the foreign host
// sees nothing — on 307/308 net/http would re-send the POST body, on
// 301/302/303 a body-less GET that still carries x-api-key.
func TestPoolClientsRefuseCrossOriginRedirect(t *testing.T) {
	for _, pc := range poolConstructors {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/%d", pc.name, code), func(t *testing.T) {
				foreign := newRedirectSink(t, false, nil)
				origin := newRedirectSink(t, false, redirectTo(code, "http://elsewhere.example/v1/chat/completions?k=v"))
				c := rewire(t, pc.build(), map[string]string{
					"api.example:80":       origin.addr(),
					"elsewhere.example:80": foreign.addr(),
				}, nil)

				got, err := postWithCredentials(t, c, "http://api.example/v1/chat/completions")
				if n := foreign.hits(); n != 0 {
					t.Errorf("the foreign host saw %d request(s): %s", n, foreign.log())
				}
				if err != nil {
					t.Fatalf("err = %v, want the %d response itself", err, code)
				}
				if got != code {
					t.Errorf("status = %d, want %d (the refused redirect is returned as the last response)", got, code)
				}
				if n := origin.hits(); n != 1 {
					t.Errorf("origin hits = %d, want 1", n)
				}
			})
		}
	}
}

// TestPoolClientsRefuseRedirectChainAwayFromOrigin — Gate (b), chain
// A→A→B: the first same-origin hop is followed, the second one leaves the
// origin and is refused at the third request.
func TestPoolClientsRefuseRedirectChainAwayFromOrigin(t *testing.T) {
	for _, pc := range poolConstructors {
		t.Run(pc.name, func(t *testing.T) {
			foreign := newRedirectSink(t, false, nil)
			origin := newRedirectSink(t, false, func(w http.ResponseWriter, r *http.Request) {
				loc := "/next"
				if r.URL.Path == "/next" {
					loc = "http://elsewhere.example/v1/chat/completions"
				}
				redirectTo(http.StatusTemporaryRedirect, loc)(w, r)
			})
			c := rewire(t, pc.build(), map[string]string{
				"api.example:80":       origin.addr(),
				"elsewhere.example:80": foreign.addr(),
			}, nil)

			got, err := postWithCredentials(t, c, "http://api.example/start")
			if n := foreign.hits(); n != 0 {
				t.Errorf("the foreign host saw %d request(s): %s", n, foreign.log())
			}
			if err != nil {
				t.Fatalf("err = %v, want the second 307 itself", err)
			}
			if got != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want 307", got)
			}
			if n := origin.hits(); n != 2 {
				t.Errorf("origin hits = %d, want 2 (start + the followed same-origin hop): %s", n, origin.log())
			}
		})
	}
}

// TestPoolClientsRefuseSchemeDowngradeAndPortChange — same host, different
// origin: https→http (the prompt would travel in clear), a different port,
// and an http→https upgrade onto a non-default port are all refused.
func TestPoolClientsRefuseSchemeDowngradeAndPortChange(t *testing.T) {
	cases := []struct {
		name, from, to, target string
		fromTLS, targetTLS     bool
	}{
		{"https to http", "https://example.com/v1/x", "http://example.com/v1/x", "example.com:80", true, false},
		{"other port", "http://example.com/v1/x", "http://example.com:8080/v1/x", "example.com:8080", false, false},
		{"upgrade to other port", "http://example.com/v1/x", "https://example.com:8443/v1/x", "example.com:8443", false, true},
	}
	for _, pc := range poolConstructors {
		for _, tc := range cases {
			t.Run(pc.name+"/"+tc.name, func(t *testing.T) {
				target := newRedirectSink(t, tc.targetTLS, nil)
				origin := newRedirectSink(t, tc.fromTLS, redirectTo(http.StatusTemporaryRedirect, tc.to))
				originKey := "example.com:80"
				if tc.fromTLS {
					originKey = "example.com:443"
				}
				roots := x509.NewCertPool()
				for _, s := range []*redirectSink{origin, target} {
					if cert := s.srv.Certificate(); cert != nil {
						roots.AddCert(cert)
					}
				}
				c := rewire(t, pc.build(), map[string]string{originKey: origin.addr(), tc.target: target.addr()}, roots)

				got, err := postWithCredentials(t, c, tc.from)
				if n := target.hits(); n != 0 {
					t.Errorf("the redirect target saw %d request(s): %s", n, target.log())
				}
				if err != nil {
					t.Fatalf("err = %v, want the 307 itself", err)
				}
				if got != http.StatusTemporaryRedirect {
					t.Errorf("status = %d, want 307", got)
				}
			})
		}
	}
}

// TestPoolClientsRefuseUnicodeFoldedHost — lead-review finding: a Location
// host that only a Unicode case fold makes equal to the configured one
// ('İbackend' vs 'ibackend') is dialed under its IDNA form, a different host.
func TestPoolClientsRefuseUnicodeFoldedHost(t *testing.T) {
	for _, pc := range poolConstructors {
		t.Run(pc.name, func(t *testing.T) {
			foreign := newRedirectSink(t, false, nil)
			origin := newRedirectSink(t, false, redirectTo(http.StatusTemporaryRedirect, "http://%C4%B0backend.example/v1/chat/completions"))
			c := rewire(t, pc.build(), map[string]string{
				"ibackend.example:80":         origin.addr(),
				"xn--ibackend-o0e.example:80": foreign.addr(),
			}, nil)

			got, err := postWithCredentials(t, c, "http://ibackend.example/v1/chat/completions")
			if n := foreign.hits(); n != 0 {
				t.Errorf("the IDNA host saw %d request(s): %s", n, foreign.log())
			}
			if err != nil {
				t.Fatalf("err = %v, want the 307 itself", err)
			}
			if got != http.StatusTemporaryRedirect {
				t.Errorf("status = %d, want 307", got)
			}
		})
	}
}

// TestPoolClientsFollowSameOriginRedirect — Gate (b) negative probes that
// must not flip: a same-origin redirect is followed for every status (307/
// 308 re-send the body), and http→https on the same host (80→443) is
// followed.
func TestPoolClientsFollowSameOriginRedirect(t *testing.T) {
	for _, pc := range poolConstructors {
		for _, code := range []int{301, 302, 303, 307, 308} {
			t.Run(fmt.Sprintf("%s/same-origin/%d", pc.name, code), func(t *testing.T) {
				origin := newRedirectSink(t, false, func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/final" {
						_, _ = io.WriteString(w, `{"ok":true}`)
						return
					}
					redirectTo(code, "/final")(w, r)
				})
				c := rewire(t, pc.build(), map[string]string{"api.example:80": origin.addr()}, nil)
				got, err := postWithCredentials(t, c, "http://api.example/start")
				if err != nil || got != http.StatusOK {
					t.Fatalf("status = %d, err = %v, want 200 after the same-origin hop", got, err)
				}
				if n := origin.hits(); n != 2 {
					t.Errorf("origin hits = %d, want 2: %s", n, origin.log())
				}
				if code == 307 || code == 308 {
					if l := origin.log(); strings.Count(l, "PROMPT-MARKER-F-W1") != 2 {
						t.Errorf("%d must re-send the body to the same origin: %s", code, l)
					}
				}
			})
		}
		t.Run(pc.name+"/http-to-https", func(t *testing.T) {
			secure := newRedirectSink(t, true, nil)
			plain := newRedirectSink(t, false, redirectTo(http.StatusPermanentRedirect, "https://example.com/v1/x"))
			roots := x509.NewCertPool()
			roots.AddCert(secure.srv.Certificate())
			c := rewire(t, pc.build(), map[string]string{"example.com:80": plain.addr(), "example.com:443": secure.addr()}, roots)
			got, err := postWithCredentials(t, c, "http://example.com/v1/x")
			if err != nil || got != http.StatusOK {
				t.Fatalf("status = %d, err = %v, want 200 after the https upgrade", got, err)
			}
			if l := secure.log(); !strings.Contains(l, "PROMPT-MARKER-F-W1") {
				t.Errorf("the upgraded hop did not carry the body: %q", l)
			}
		})
	}
}

// TestPoolClientsKeepRedirectLimit — Gate (b): the net/http default hop limit
// survives the custom CheckRedirect. Nine same-origin redirects are followed;
// the tenth is refused with net/http's own error after ten requests.
func TestPoolClientsKeepRedirectLimit(t *testing.T) {
	for _, pc := range poolConstructors {
		for _, tc := range []struct {
			redirects int
			wantErr   bool
		}{{9, false}, {10, true}, {11, true}} {
			t.Run(fmt.Sprintf("%s/%d", pc.name, tc.redirects), func(t *testing.T) {
				var mu sync.Mutex
				served := 0
				origin := newRedirectSink(t, false, func(w http.ResponseWriter, _ *http.Request) {
					mu.Lock()
					served++
					n := served
					mu.Unlock()
					if n > tc.redirects {
						_, _ = io.WriteString(w, `{"ok":true}`)
						return
					}
					redirectTo(http.StatusTemporaryRedirect, fmt.Sprintf("/hop/%d", n))(w, nil)
				})
				c := rewire(t, pc.build(), map[string]string{"api.example:80": origin.addr()}, nil)
				got, err := postWithCredentials(t, c, "http://api.example/hop/0")
				if !tc.wantErr {
					if err != nil || got != http.StatusOK {
						t.Fatalf("status = %d, err = %v, want 200", got, err)
					}
					return
				}
				if err == nil || !strings.Contains(err.Error(), "stopped after 10 redirects") {
					t.Fatalf("err = %v (status %d), want net/http's \"stopped after 10 redirects\"", err, got)
				}
				if n := origin.hits(); n != 10 {
					t.Errorf("origin hits = %d, want 10", n)
				}
			})
		}
	}
}
