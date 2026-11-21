package oidc

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// #459: the discovery document names where the token request goes (code,
// PKCE verifier, client_secret) and which keys verify the ID token. A
// redirect of the discovery fetch to another origin let that origin name all
// of them: the issuer match compares a string any host can copy. The cache
// follows a redirect of the discovery document only within the issuer's
// origin, and a redirect of the JWKS only within the jwks_uri's origin. The
// client handed to the cache sets no redirect rule (as NewSSOHandler wires
// it), so the rule has to come from the cache.

var redirectCodes = []int{
	http.StatusMovedPermanently, http.StatusFound, http.StatusSeeOther,
	http.StatusTemporaryRedirect, http.StatusPermanentRedirect,
}

// leakMarker sits in the Location of every refused redirect: the error that
// reaches the log names the status, not the target.
const leakMarker = "leak-459"

// redirectTo answers with code and Location target, the Location repeated in
// the body the way servers commonly do.
func redirectTo(w http.ResponseWriter, target string, code int) {
	w.Header().Set("Location", target)
	w.WriteHeader(code)
	_, _ = fmt.Fprintf(w, `<a href="%s">moved</a>`, target)
}

// discoveryDoc writes an honest discovery document for issuer, with the
// given jwks_uri.
func discoveryDoc(w http.ResponseWriter, issuer, jwksURI string) {
	_ = json.NewEncoder(w).Encode(map[string]string{
		"issuer":                 issuer,
		"authorization_endpoint": issuer + "/authorize",
		"token_endpoint":         issuer + "/token",
		"jwks_uri":               jwksURI,
	})
}

func newPlainProvider(t *testing.T, issuer string) Provider {
	t.Helper()
	p, err := NewOIDC(NewCache(Options{Client: &http.Client{}}), OIDCConfig{
		Issuer: issuer, ClientID: testClientID, IDTokenAlgs: []string{"RS256"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDiscoveryRedirectStaysOnIssuerOrigin(t *testing.T) {
	for _, code := range redirectCodes {
		t.Run(fmt.Sprintf("other-origin-%d", code), func(t *testing.T) {
			var issuer string
			foreign := newGHSink(t, func(w http.ResponseWriter, r *http.Request) {
				// Copies the issuer string and names its own endpoints.
				_ = json.NewEncoder(w).Encode(map[string]string{
					"issuer":                 issuer,
					"authorization_endpoint": issuer + "/authorize",
					"token_endpoint":         "http://" + r.Host + "/steal-token",
					"jwks_uri":               "http://" + r.Host + "/attacker-jwks",
				})
			})
			idp := newGHSink(t, func(w http.ResponseWriter, _ *http.Request) {
				redirectTo(w, foreign.srv.URL+"/.well-known/openid-configuration?"+leakMarker, code)
			})
			issuer = idp.srv.URL
			p := newPlainProvider(t, issuer)

			_, tokenURL, jwksURL, _, err := p.Endpoints(context.Background())
			if n := foreign.count(); n != 0 {
				t.Fatalf("the discovery fetch followed a %d to another origin: %s", code, foreign.log())
			}
			if err == nil {
				t.Fatalf("a refused discovery redirect handed out endpoints: token=%q jwks=%q", tokenURL, jwksURL)
			}
			if !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), leakMarker) {
				t.Errorf("error %q: want the redirect named, its target left out", err)
			}
		})
	}
	t.Run("same-origin", func(t *testing.T) {
		var issuer string
		idp := newGHSink(t, func(w http.ResponseWriter, r *http.Request) {
			if strings.HasPrefix(r.URL.Path, "/moved/") {
				discoveryDoc(w, issuer, issuer+"/jwks")
				return
			}
			redirectTo(w, "/moved"+r.URL.Path, http.StatusPermanentRedirect)
		})
		issuer = idp.srv.URL
		_, tokenURL, _, _, err := newPlainProvider(t, issuer).Endpoints(context.Background())
		if err != nil || tokenURL != issuer+"/token" {
			t.Fatalf("a redirect within the issuer's origin: token %q, err %v", tokenURL, err)
		}
	})
}

func TestJWKSRedirectStaysOnItsOrigin(t *testing.T) {
	jwks := func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jwksFor(testKey, testKid))
	}
	// identity runs the OIDC identity path against an issuer whose honest
	// discovery document names jwksURI.
	identity := func(t *testing.T, jwksURI string) error {
		t.Helper()
		var issuer string
		idp := newGHSink(t, func(w http.ResponseWriter, _ *http.Request) { discoveryDoc(w, issuer, jwksURI) })
		issuer = idp.srv.URL
		c := baseClaims()
		c["iss"] = issuer
		_, err := newPlainProvider(t, issuer).Identity(context.Background(), Tokens{IDToken: signRS256(t, testKey, testKid, c)}, testNonce)
		return err
	}

	// A jwks_uri on another origin than the issuer is common (it comes from
	// the issuer's own document) and stays allowed.
	t.Run("jwks-on-another-origin", func(t *testing.T) {
		keys := newGHSink(t, jwks)
		if err := identity(t, keys.srv.URL+"/keys"); err != nil {
			t.Fatalf("a jwks_uri on another origin than the issuer: %v", err)
		}
	})
	for _, code := range redirectCodes {
		t.Run(fmt.Sprintf("other-origin-%d", code), func(t *testing.T) {
			// The foreign origin serves the very keys that signed the token:
			// only the refused fetch can make the identity fail.
			foreign := newGHSink(t, jwks)
			keys := newGHSink(t, func(w http.ResponseWriter, _ *http.Request) {
				redirectTo(w, foreign.srv.URL+"/keys?"+leakMarker, code)
			})
			err := identity(t, keys.srv.URL+"/keys")
			if n := foreign.count(); n != 0 {
				t.Fatalf("the JWKS fetch followed a %d to another origin: %s", code, foreign.log())
			}
			if err == nil {
				t.Fatal("an ID token was verified with keys behind a refused redirect")
			}
			if !strings.Contains(err.Error(), "redirect") || strings.Contains(err.Error(), leakMarker) {
				t.Errorf("error %q: want the redirect named, its target left out", err)
			}
		})
	}
	t.Run("same-origin", func(t *testing.T) {
		keys := newGHSink(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/rotated" {
				jwks(w, r)
				return
			}
			redirectTo(w, "/rotated", http.StatusFound)
		})
		if err := identity(t, keys.srv.URL+"/keys"); err != nil {
			t.Fatalf("a JWKS redirect within its origin: %v", err)
		}
	})
}
