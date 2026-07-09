package oidc

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"math/big"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	testIssuer   = "https://idp.example.test"
	testClientID = "ctx-client"
	testNonce    = "nonce-1234"
	testKid      = "key-1"
)

// testKey is generated once — RSA keygen is the slow part of this suite.
var testKey = mustRSAKey()

func mustRSAKey() *rsa.PrivateKey {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		panic(err)
	}
	return k
}

// jwksFor builds a JWKS containing the RSA public key under kid.
func jwksFor(key *rsa.PrivateKey, kid string) *JWKS {
	pub := key.Public().(*rsa.PublicKey)
	return &JWKS{Keys: []JWK{{
		Kty: "RSA",
		Kid: kid,
		Alg: "RS256",
		N:   base64.RawURLEncoding.EncodeToString(pub.N.Bytes()),
		E:   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(pub.E)).Bytes()),
	}}}
}

// baseClaims returns a valid claim set; tests mutate single fields to probe
// exactly one rejection branch each.
func baseClaims() jwt.MapClaims {
	return jwt.MapClaims{
		"iss":            testIssuer,
		"sub":            "user-42",
		"aud":            testClientID,
		"exp":            time.Now().Add(time.Hour).Unix(),
		"nonce":          testNonce,
		"email":          "user@example.test",
		"email_verified": true,
		"name":           "User FortyTwo",
	}
}

// signRS256 signs claims with the test key under kid.
func signRS256(t *testing.T, key *rsa.PrivateKey, kid string, claims jwt.MapClaims) string {
	t.Helper()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tok.Header["kid"] = kid
	s, err := tok.SignedString(key)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	return s
}

func defaultParams() VerifyParams {
	return VerifyParams{
		Issuer:   testIssuer,
		ClientID: testClientID,
		Nonce:    testNonce,
		Algs:     []string{"RS256"},
	}
}

// mustReject asserts a verification failure whose error mentions fragment.
func mustReject(t *testing.T, token string, p VerifyParams, fragment string) {
	t.Helper()
	_, err := VerifyIDToken(token, jwksFor(testKey, testKid), p)
	if err == nil {
		t.Fatalf("want reject (%s), got nil error", fragment)
	}
	if !strings.Contains(err.Error(), fragment) {
		t.Fatalf("error %q does not mention %q", err, fragment)
	}
}

// W3 gate: valid token → identity claims extracted correctly.
func TestVerifyValidToken(t *testing.T) {
	token := signRS256(t, testKey, testKid, baseClaims())
	claims, err := VerifyIDToken(token, jwksFor(testKey, testKid), defaultParams())
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Errorf("Subject = %q", claims.Subject)
	}
	if claims.Issuer != testIssuer {
		t.Errorf("Issuer = %q", claims.Issuer)
	}
	if claims.Email != "user@example.test" || !claims.EmailVerified {
		t.Errorf("Email = %q verified=%v, want verified email", claims.Email, claims.EmailVerified)
	}
	if claims.Name != "User FortyTwo" {
		t.Errorf("Name = %q", claims.Name)
	}
}

// W3 gate: expired token → reject.
func TestVerifyExpiredToken(t *testing.T) {
	c := baseClaims()
	c["exp"] = time.Now().Add(-time.Hour).Unix()
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "expired")
}

// Missing exp → reject (WithExpirationRequired).
func TestVerifyMissingExpiry(t *testing.T) {
	c := baseClaims()
	delete(c, "exp")
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "exp claim is required")
}

// W3 gate: wrong issuer → reject.
func TestVerifyWrongIssuer(t *testing.T) {
	c := baseClaims()
	c["iss"] = "https://evil.example.test"
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "issuer mismatch")
}

// W3 gate: wrong audience → reject.
func TestVerifyWrongAudience(t *testing.T) {
	c := baseClaims()
	c["aud"] = "someone-else"
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "audience mismatch")
}

// W3 gate: wrong nonce → reject.
func TestVerifyWrongNonce(t *testing.T) {
	c := baseClaims()
	c["nonce"] = "stale-nonce"
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "nonce mismatch")
}

// W3 gate: HS256 token against an RS256-only provider → reject (key
// confusion: the attacker HMAC-signs with the PUBLIC key bytes).
func TestVerifyWrongAlgHS256KeyConfusion(t *testing.T) {
	pub := testKey.Public().(*rsa.PublicKey)
	tok := jwt.NewWithClaims(jwt.SigningMethodHS256, baseClaims())
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(pub.N.Bytes())
	if err != nil {
		t.Fatalf("sign HS256: %v", err)
	}
	mustReject(t, signed, defaultParams(), "verification failed")
}

// W3 gate: alg:none → reject.
func TestVerifyAlgNone(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodNone, baseClaims())
	tok.Header["kid"] = testKid
	signed, err := tok.SignedString(jwt.UnsafeAllowNoneSignatureType)
	if err != nil {
		t.Fatalf("sign none: %v", err)
	}
	mustReject(t, signed, defaultParams(), "verification failed")
}

// Unknown kid → reject (no matching JWKS key).
func TestVerifyUnknownKid(t *testing.T) {
	mustReject(t, signRS256(t, testKey, "key-unknown", baseClaims()), defaultParams(), "no matching JWKS key")
}

// Missing kid header → reject.
func TestVerifyMissingKid(t *testing.T) {
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, baseClaims())
	signed, err := tok.SignedString(testKey)
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	mustReject(t, signed, defaultParams(), "missing kid")
}

// W3 gate: multiple audiences WITHOUT azp → reject (OIDC Core §3.1.3.7).
func TestVerifyMultiAudienceWithoutAzp(t *testing.T) {
	c := baseClaims()
	c["aud"] = []string{testClientID, "other-client"}
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "azp claim missing")
}

// W3 gate: multiple audiences with CORRECT azp → pass.
func TestVerifyMultiAudienceWithCorrectAzp(t *testing.T) {
	c := baseClaims()
	c["aud"] = []string{testClientID, "other-client"}
	c["azp"] = testClientID
	claims, err := VerifyIDToken(signRS256(t, testKey, testKid, c), jwksFor(testKey, testKid), defaultParams())
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Subject != "user-42" {
		t.Errorf("Subject = %q", claims.Subject)
	}
}

// Multiple audiences with WRONG azp → reject.
func TestVerifyMultiAudienceWithWrongAzp(t *testing.T) {
	c := baseClaims()
	c["aud"] = []string{testClientID, "other-client"}
	c["azp"] = "other-client"
	mustReject(t, signRS256(t, testKey, testKid, c), defaultParams(), "azp mismatch")
}

// email_verified=false → email must stay empty (OIDC Core §5.7).
func TestVerifyUnverifiedEmailDropped(t *testing.T) {
	c := baseClaims()
	c["email_verified"] = false
	claims, err := VerifyIDToken(signRS256(t, testKey, testKid, c), jwksFor(testKey, testKid), defaultParams())
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Email != "" || claims.EmailVerified {
		t.Errorf("Email = %q verified=%v, want empty/false", claims.Email, claims.EmailVerified)
	}
}

// email_verified missing entirely → email dropped (fail-closed).
func TestVerifyMissingEmailVerifiedDropped(t *testing.T) {
	c := baseClaims()
	delete(c, "email_verified")
	claims, err := VerifyIDToken(signRS256(t, testKey, testKid, c), jwksFor(testKey, testKid), defaultParams())
	if err != nil {
		t.Fatalf("VerifyIDToken: %v", err)
	}
	if claims.Email != "" {
		t.Errorf("Email = %q, want empty", claims.Email)
	}
}

// Empty verify params are configuration rejects, never soft passes.
func TestVerifyEmptyParamsRejected(t *testing.T) {
	token := signRS256(t, testKey, testKid, baseClaims())
	jwks := jwksFor(testKey, testKid)
	cases := []struct {
		name string
		mut  func(*VerifyParams)
	}{
		{"missing issuer", func(p *VerifyParams) { p.Issuer = "" }},
		{"missing client_id", func(p *VerifyParams) { p.ClientID = "" }},
		{"missing nonce", func(p *VerifyParams) { p.Nonce = "" }},
		{"missing algs", func(p *VerifyParams) { p.Algs = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := defaultParams()
			tc.mut(&p)
			if _, err := VerifyIDToken(token, jwks, p); err == nil {
				t.Fatal("want configuration reject, got nil error")
			}
		})
	}
}

// W3 gate: ClaimFilter — contained value passes; missing claim, foreign
// value and empty filter reject.
func TestClaimFilter(t *testing.T) {
	f := &ClaimFilter{Claim: "tid", Values: []string{"tenant-a", "tenant-b"}}

	if err := f.Check(map[string]any{"tid": "tenant-a"}); err != nil {
		t.Errorf("contained value: %v", err)
	}
	if err := f.Check(map[string]any{"tid": "tenant-x"}); err == nil {
		t.Error("foreign value: want reject")
	}
	if err := f.Check(map[string]any{"sub": "u"}); err == nil {
		t.Error("missing claim: want reject")
	}
	// array-valued claim: any overlap counts
	if err := f.Check(map[string]any{"tid": []any{"nope", "tenant-b"}}); err != nil {
		t.Errorf("array overlap: %v", err)
	}
	if err := f.Check(map[string]any{"tid": []any{"nope"}}); err == nil {
		t.Error("array without overlap: want reject")
	}
	// non-string claim type → reject
	if err := f.Check(map[string]any{"tid": 42.0}); err == nil {
		t.Error("non-string claim: want reject")
	}
	// empty filter → reject
	empty := &ClaimFilter{}
	if err := empty.Check(map[string]any{"tid": "tenant-a"}); err == nil {
		t.Error("empty filter: want reject")
	}
	var nilFilter *ClaimFilter
	if err := nilFilter.Check(map[string]any{"tid": "tenant-a"}); err == nil {
		t.Error("nil filter: want reject")
	}
}
