// Package camo is the signing image proxy (design 07-camo-image-proxy.md): it
// re-hosts foreign remote images under the ctx origin so the SPA can render them
// under `img-src 'self'` WITHOUT widening the CSP, and so the viewer's browser
// never talks to the foreign origin (no tracking-pixel deanonymization, E04-9).
//
// Two surfaces cooperate (§4.2, D2b):
//
//   - POST /api/img/sign — AUTH-GATED mint. The frontend has no secret, so it
//     asks the server to sign the foreign image URLs it wants to proxy; the
//     server returns {url → /api/img/<sig>?url=…&exp=…}. Rate-limited per key so
//     the signature oracle cannot be abused (§5, D2b restrisiko).
//   - GET  /api/img/<sig> — AUTH-LESS fetch. A browser <img> carries no API key
//     (ctx auth lives in sessionStorage, not cookies), so the HMAC signature IS
//     the capability. The signature binds the URL AND an expiry; the fetch runs
//     the SSRF / size-cap / content-type policy in fetch.go.
//
// The signing key is a labeled-HMAC subkey of the sealbox master key
// CTX_SECRETS_KEY (D3, key separation) — no new secret, rotation piggybacks on
// the existing CTX_SECRETS_KEY_PREV slot. The feature is fail-closed: disabled
// unless CTX_CAMO_ENABLED is truthy AND a master key is present.
package camo

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables (design §3). All default-safe.
const (
	EnvEnabled       = "CTX_CAMO_ENABLED"     // feature flag, default false (fail-closed)
	EnvTTL           = "CTX_CAMO_TTL"         // signature + cache lifetime, default 24h
	EnvMaxBytes      = "CTX_CAMO_MAX_BYTES"   // upstream size cap, default 10 MiB
	envMasterKey     = "CTX_SECRETS_KEY"      // sealbox master key (reused, never a new secret)
	envMasterKeyPrev = "CTX_SECRETS_KEY_PREV" // rotation slot
)

// Defaults (design §3).
const (
	DefaultMaxBytes = 10 << 20       // 10 MiB — images are small; API JSON uses 32 MiB
	DefaultTTL      = 24 * time.Hour // bounds the auth-less fetch endpoint's relay window
	masterKeyBytes  = 32             // AES-256 master key length (sealbox.KeySize)
	// subkeyLabel domain-separates the Camo signing subkey from every other use
	// of the master key. Bumping the version invalidates every minted signature
	// (they simply expire / are re-minted on next render) — it is NOT a stored
	// format, so it is safe to rotate.
	subkeyLabel = "ctx:camo:sig:v1"
	// signRateLimit is the per-key sign budget over signWindow. A document render
	// signs ALL its foreign images in ONE batch call, so this bounds documents
	// signed per minute per key, not images.
	signRateLimit = 120
	signWindow    = time.Minute
)

// Service is the wired proxy: a signer, a fetch policy, a per-key sign rate
// limiter, and the enabled flag. A disabled Service answers 404 on both routes.
type Service struct {
	signer  *signer
	fetcher *Fetcher
	limiter *rateLimiter
	enabled bool
}

// signer holds the derived HMAC subkey(s) and the signature TTL.
type signer struct {
	current []byte // HMAC-SHA256 subkey derived from CTX_SECRETS_KEY
	prev    []byte // nil unless CTX_SECRETS_KEY_PREV is set (rotation in flight)
	ttl     time.Duration
}

// NewFromEnv builds the Service from the environment. It NEVER returns an error:
// a misconfiguration (enabled but no/invalid master key) is logged and collapses
// to a disabled Service — fail-closed, so a boot mistake can never leave an
// unsigned open proxy running.
func NewFromEnv() *Service {
	if !parseBool(os.Getenv(EnvEnabled)) {
		return &Service{enabled: false}
	}
	ttl := parseTTL(os.Getenv(EnvTTL), DefaultTTL)
	maxBytes := parseBytes(os.Getenv(EnvMaxBytes), DefaultMaxBytes)
	svc, err := New(os.Getenv(envMasterKey), os.Getenv(envMasterKeyPrev), ttl, maxBytes)
	if err != nil {
		slog.Error("camo: enabled but disabled at boot — master key unusable", "error", err)
		return &Service{enabled: false}
	}
	return svc
}

// New builds an enabled Service from explicit master keys (used by tests and by
// NewFromEnv). prevHex may be empty. It errors only when the CURRENT master key
// is missing or malformed — that is the fail-closed boundary.
func New(currentHex, prevHex string, ttl time.Duration, maxBytes int64) (*Service, error) {
	cur, err := deriveSubkey(currentHex)
	if err != nil {
		return nil, err
	}
	var prev []byte
	if strings.TrimSpace(prevHex) != "" {
		prev, err = deriveSubkey(prevHex)
		if err != nil {
			return nil, err
		}
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	if maxBytes <= 0 {
		maxBytes = DefaultMaxBytes
	}
	return &Service{
		signer:  &signer{current: cur, prev: prev, ttl: ttl},
		fetcher: NewFetcher(maxBytes),
		limiter: newRateLimiter(signRateLimit, signWindow),
		enabled: true,
	}, nil
}

// deriveSubkey turns the 64-hex-char master key into the Camo signing subkey via
// labeled HMAC (HMAC-SHA256(masterKey, label)). This is the D3 key-separation:
// the subkey is cryptographically independent of the sealbox AEAD key, so a Camo
// signature can never be traded against a sealed provider secret.
func deriveSubkey(masterHex string) ([]byte, error) {
	raw, err := hex.DecodeString(strings.TrimSpace(masterHex))
	if err != nil {
		return nil, fmt.Errorf("camo: %s is not valid hex", envMasterKey)
	}
	if len(raw) != masterKeyBytes {
		return nil, fmt.Errorf("camo: %s must be %d hex chars (%d bytes)", envMasterKey, masterKeyBytes*2, masterKeyBytes)
	}
	mac := hmac.New(sha256.New, raw)
	mac.Write([]byte(subkeyLabel))
	return mac.Sum(nil), nil
}

// Enabled reports whether the proxy is live. Both handlers 404 when false.
func (s *Service) Enabled() bool { return s.enabled }

// TTLSeconds is the signature/cache lifetime in seconds (for Cache-Control).
func (s *Service) TTLSeconds() int {
	if !s.enabled {
		return 0
	}
	return int(s.signer.ttl.Seconds())
}

// AllowSign reports whether the caller (identified by its API-key id) is under
// the per-key sign budget. An empty key still counts (shared bucket) so the
// endpoint can never be flooded through a missing identity.
func (s *Service) AllowSign(keyID string) bool {
	if !s.enabled {
		return false
	}
	return s.limiter.Allow(keyID)
}

// SignedPath mints the proxy path for a foreign image URL: /api/img/<sig>?url=…&
// exp=…. The URL is signed with the current key and the expiry is now+TTL.
func (s *Service) SignedPath(rawURL string) string {
	return s.SignedPathAt(rawURL, time.Now().Add(s.signer.ttl).Unix())
}

// SignedPathAt mints a proxy path with an explicit expiry (Unix seconds). Used by
// SignedPath (exp = now+TTL) and by callers/tests that need a chosen expiry.
func (s *Service) SignedPathAt(rawURL string, exp int64) string {
	sig := sigFor(s.signer.current, rawURL, exp)
	q := url.Values{}
	q.Set("url", rawURL)
	q.Set("exp", strconv.FormatInt(exp, 10))
	return "/api/img/" + sig + "?" + q.Encode()
}

// VerifySig checks sig against the current key, then the previous key (rotation),
// in constant time. It does NOT check expiry — the caller checks exp separately
// so a forged sig and an expired-but-valid sig get distinct statuses (403 vs 410)
// while an attacker WITHOUT a valid sig always sees the same 403 (no oracle).
func (s *Service) VerifySig(sig, rawURL string, exp int64) bool {
	if !s.enabled {
		return false
	}
	got, err := hex.DecodeString(sig)
	if err != nil {
		return false
	}
	if constEqHex(got, sigFor(s.signer.current, rawURL, exp)) {
		return true
	}
	if s.signer.prev != nil && constEqHex(got, sigFor(s.signer.prev, rawURL, exp)) {
		return true
	}
	return false
}

// Fetch pulls the upstream image under the full policy (SSRF / size-cap /
// content-type). Precondition: the caller has verified the signature (§4.4). A
// disabled service refuses every fetch.
func (s *Service) Fetch(ctx context.Context, rawURL string) (*Result, error) {
	if !s.enabled {
		return nil, fetchErr(http.StatusNotFound, "camo: disabled")
	}
	return s.fetcher.Fetch(ctx, rawURL)
}

// ProxiableURL reports whether rawURL is a candidate for proxying: a parseable
// absolute http/https URL. SSRF is enforced at fetch time (fetch.go); this only
// filters obvious non-images (mailto:, data:, relative) at mint time.
func ProxiableURL(rawURL string) bool {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// sigFor computes hex(HMAC-SHA256(key, url + "\n" + exp)) — the wire signature.
func sigFor(key []byte, rawURL string, exp int64) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(rawURL))
	mac.Write([]byte{'\n'})
	mac.Write([]byte(strconv.FormatInt(exp, 10)))
	return hex.EncodeToString(mac.Sum(nil))
}

// constEqHex compares a raw digest against a hex-encoded expected digest in
// constant time (subtle.ConstantTimeCompare, like hmac.Equal).
func constEqHex(got []byte, expectedHex string) bool {
	want, err := hex.DecodeString(expectedHex)
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// parseBool treats "1", "true", "yes", "on" (case-insensitive) as true.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// parseTTL parses a Go duration ("24h", "30m"); falls back to def on empty/error.
func parseTTL(v string, def time.Duration) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

// parseBytes parses a plain byte count; falls back to def on empty/error.
func parseBytes(v string, def int64) int64 {
	v = strings.TrimSpace(v)
	if v == "" {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
