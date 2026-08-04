package backends

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// FieldError is one 422-grade validation failure, addressable by field.
type FieldError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

// EmbedEquivalenceThreshold is the minimum per-sample cosine (over the
// DB-relevant 1024-truncated, L2-normalized vectors) at which the
// embed-equivalence probe declares an external backend's vector space
// equivalent to the local reference. Empirically anchored (2026-08-04,
// llama-embed CPU-quant vs DGX qwen3-embedding-8b): same weights across a
// precision/quantization boundary measure 0.9972–0.9986 over a 7-sample
// corpus incl. Umlaute, code and symbol noise; a genuinely different model
// lands far below 0.99. 0.995 separates the two populations with margin on
// both sides.
const EmbedEquivalenceThreshold = 0.995

// headerDenylist blocks credential-carrier headers in extra_headers
// (case-insensitive exact matches; suffix patterns below). The escape hatch
// is api_key_ref — same pattern as F2's secret_ref rule ("create the secret
// first"). Without this, {"extra_headers":{"Authorization":"Bearer sk-…"}}
// would put a plaintext provider key into context_backends, every
// list/create response AND (via the audit hook) forever into the
// append-only context_settings_audit.
var headerDenylist = map[string]bool{
	"authorization":       true,
	"proxy-authorization": true,
	"cookie":              true,
	"x-api-key":           true,
}

// bodyCredentialKeys blocks credential-semantic fields anywhere in
// extra_body (recursive walk — the field is small by construction).
var bodyCredentialKeys = map[string]bool{
	"api_key": true, "apikey": true, "api-key": true,
	"token": true, "secret": true, "password": true, "authorization": true,
}

func deniedHeader(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if headerDenylist[n] {
		return true
	}
	return strings.HasSuffix(n, "-token") || strings.HasSuffix(n, "-key") ||
		strings.HasSuffix(n, "_token") || strings.HasSuffix(n, "_key")
}

func findCredentialBodyKey(m map[string]any) string {
	for k, v := range m {
		if bodyCredentialKeys[strings.ToLower(k)] {
			return k
		}
		if nested, ok := v.(map[string]any); ok {
			if hit := findCredentialBodyKey(nested); hit != "" {
				return k + "." + hit
			}
		}
	}
	return ""
}

// DeriveLocality classifies a base_url host for the egress audit dimension:
// loopback/localhost → local, private ranges + single-label hostnames
// (docker service names) → lan, everything publicly routable → external.
// No DNS resolution happens here — classification is static so validation
// stays deterministic and network-free.
func DeriveLocality(baseURL string) (string, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return "", fmt.Errorf("unparseable URL")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("URL has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return LocalityLocal, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback():
			return LocalityLocal, nil
		case ip.IsPrivate(), ip.IsLinkLocalUnicast():
			return LocalityLAN, nil
		default:
			return LocalityExternal, nil
		}
	}
	if !strings.Contains(host, ".") {
		// Single-label hostname: docker service / LAN mDNS — not publicly
		// routable by construction.
		return LocalityLAN, nil
	}
	return LocalityExternal, nil
}

// ValidateBackend checks one complete row state (create payload, or the
// merge of an existing row with an update patch). Returned FieldErrors are
// 422-grade; warnings ship in the success response. Confirm mechanics
// (trust elevation) live at the handler — they need the previous state and
// the request flag, not the row.
//
// The trust elevation guard and this validation are the same opsec edge in
// two directions: a backend becoming more trusted and content becoming less
// sensitive both open the same flow.
func ValidateBackend(b *Backend) (warnings []string, errs []FieldError) {
	errs = append(errs, validateIdentity(b)...)
	w, e := validateRoles(b)
	warnings = append(warnings, w...)
	errs = append(errs, e...)
	errs = append(errs, validateCarriers(b)...)
	return warnings, errs
}

// validateIdentity covers name, URL, enums and the locality cross-check.
func validateIdentity(b *Backend) (errs []FieldError) {
	if strings.TrimSpace(b.Name) == "" {
		errs = append(errs, FieldError{"name", "name is required"})
	}

	u, err := url.Parse(b.Host)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, FieldError{"base_url", "must be an absolute http(s) URL"})
	}

	errs = append(errs, validateVersionedRoot(b)...)

	switch b.Protocol {
	case ProtocolOpenAI, ProtocolOllama, "rerank":
	default:
		errs = append(errs, FieldError{"protocol", "must be one of: openai, ollama, rerank"})
	}

	switch b.ProviderClass {
	case ProviderGeneric, ProviderLlamaCpp, ProviderOpenRouter:
	default:
		errs = append(errs, FieldError{"provider_class", "must be one of: generic, llamacpp, openrouter"})
	}

	// metadata.score_domain is the rerank dialect slot (see ScoreDomain).
	// Reject unknown values at write time so the runtime auto-fallback in
	// ScoreDomain never silently swallows a typo like "probabilty".
	if raw, ok := b.Metadata[scoreDomainKey]; ok {
		v, isStr := raw.(string)
		if !isStr || (v != ScoreDomainAuto && v != ScoreDomainLogit && v != ScoreDomainProbability) {
			errs = append(errs, FieldError{"metadata.score_domain", "must be one of: auto, logit, probability"})
		}
	}

	if !ValidTrust(b.Trust) {
		errs = append(errs, FieldError{"trust", "must be one of: full-trust, no-credentials, non-personal, public"})
	}

	// locality is NOT freely choosable: the egress audit (partial index on
	// backend_locality='external') depends on it. A public host declared
	// 'lan' would silently drop its calls from the egress trail.
	switch b.Locality {
	case LocalityLocal, LocalityLAN, LocalityExternal:
		if derived, derr := DeriveLocality(b.Host); derr == nil &&
			derived == LocalityExternal && b.Locality != LocalityExternal {
			errs = append(errs, FieldError{"locality",
				fmt.Sprintf("host is publicly routable — locality must be 'external', not %q", b.Locality)})
		}
	case "":
		errs = append(errs, FieldError{"locality", "locality is required (derive: local|lan|external)"})
	default:
		errs = append(errs, FieldError{"locality", "must be one of: local, lan, external"})
	}

	if b.NumCtx < 0 {
		errs = append(errs, FieldError{"num_ctx", "must be >= 0"})
	}
	return errs
}

// validateVersionedRoot rejects a base_url that already carries the version
// segment. The openai and rerank wire paths append it themselves
// (/v1/chat/completions, /v1/embeddings, /v1/rerank; openrouter additionally
// /v1/key, /v1/endpoints/zdr and /v1/models/…/endpoints). A base_url ending
// in /v1 — the form most cloud SDK examples teach — therefore double-segments
// EVERY call to /v1/v1/… and fails in three disconnected-looking ways
// (issue #21): probe unreachable, window discovery 404 → fail-closed
// synthesis, chat 404. Silent acceptance here is what made that hard to
// diagnose, so it is a 422. ollama protocol stays out: its wire appends
// /api/…, so a /v1 root there is odd but does not double.
func validateVersionedRoot(b *Backend) []FieldError {
	// TrimSpace: copy-paste with trailing whitespace is the realistic entry
	// path for exactly the audience this guard protects; ToLower: /V1 404s
	// case-sensitively anyway, but with the same three disconnected symptoms
	// this guard exists to prevent.
	root := strings.ToLower(strings.TrimRight(strings.TrimSpace(b.Host), "/"))
	if !strings.HasSuffix(root, "/v1") {
		return nil
	}
	switch {
	case b.ProviderClass == ProviderOpenRouter:
		return []FieldError{{"base_url",
			"openrouter-class base_url is the API root WITHOUT the version segment (wire paths append /v1/... themselves) — a /v1 root doubles to /v1/v1/... and breaks probe, window discovery and every synthesis"}}
	case b.Protocol == ProtocolOpenAI || b.Protocol == "rerank":
		return []FieldError{{"base_url",
			"base_url is the API root WITHOUT the version segment (the openai/rerank wire paths append /v1/... themselves) — a /v1 root doubles to /v1/v1/... and 404s chat, embeddings and rerank"}}
	}
	return nil
}

// validateRoles covers role/model coverage, the embed+external hard block
// and the dream-on-local warning.
func validateRoles(b *Backend) (warnings []string, errs []FieldError) {
	core := make(map[string]bool, len(CoreRoles))
	for _, r := range CoreRoles {
		core[r] = true
	}
	hasEmbedRole := false
	for _, r := range b.Roles {
		if embedRoles[r] {
			hasEmbedRole = true
		}
		if !core[r] {
			warnings = append(warnings, fmt.Sprintf("role %q is not a core role (allowed, routed via proxy vision)", r))
			continue
		}
		// Every core role must resolve to a model (directly or via default) —
		// otherwise the first call of that role fails at runtime instead of
		// at configuration time (risk 6.7, model_map typo class).
		if _, ok := b.ModelMap[r]; !ok {
			if _, ok := b.ModelMap["default"]; !ok {
				errs = append(errs, FieldError{"model_map",
					fmt.Sprintf("role %q has no model_map entry and no default", r)})
			}
		}
	}

	// Embed roles write into the shared vector(1024) space. A foreign
	// quantization corrupts retrieval silently; repair at 1M+ blocks is a
	// full re-embed — more irreversible than any cooldown damage. Hard 422,
	// liftable only via the cosine equivalence probe (backend-test
	// probe:"embed-equivalence" → metadata.embed_equivalence_verified).
	if hasEmbedRole && b.Locality == LocalityExternal {
		if verified, _ := b.Metadata["embed_equivalence_verified"].(bool); !verified {
			errs = append(errs, FieldError{"roles",
				"embed roles on an external backend require metadata.embed_equivalence_verified=true (vector-space equivalence proof)"})
		}
	}

	// The classify role reads block content BEFORE the block is classified —
	// its prompt is potential credentials by definition. Unlike embed there
	// is NO metadata escape hatch: audit content never crosses the locality
	// border, full-trust ZDR included (G41; the trust gate alone would let a
	// full-trust external backend through).
	if b.HasRole(RoleClassify) && b.Locality == LocalityExternal {
		errs = append(errs, FieldError{"roles",
			"classify role is hard-local — an external backend can never audit unclassified block content"})
	}

	// Dream evaluation on a local (CPU-class) backend: the DreamTimeout code
	// default (180s, GPU-calibrated p99+margin) tears at CPU token rates →
	// cooldown churn + corrupted back-off statistics (risk 6.5). Warning, not
	// error — policy is data. A per-role dream timeout override on the row is
	// the designed CPU path (TimeoutFor beats the code default at every chain
	// call), so a configured override silences the warning. Locality=local is
	// the CPU-class heuristic; lan stays out deliberately — it carries no
	// CPU/GPU signal (herbert-chat is lan+GPU) and would false-positive on
	// every healthy dream backend.
	if b.HasRole(RoleDream) && b.Locality == LocalityLocal {
		if secs, ok := b.Timeouts[RoleDream]; !ok || secs <= 0 {
			warnings = append(warnings, "dream role on a local backend without a dream timeout override: CPU-class inference tears the DreamTimeout code default and corrupts back-off statistics")
		}
	}
	return warnings, errs
}

// validateCarriers covers the credential-carrier denylist and timeouts.
func validateCarriers(b *Backend) (errs []FieldError) {
	for name := range b.ExtraHeaders {
		if deniedHeader(name) {
			errs = append(errs, FieldError{"extra_headers",
				fmt.Sprintf("header %q is a credential carrier — use api_key_ref (create the secret first)", name)})
		}
	}

	if hit := findCredentialBodyKey(b.ExtraBody); hit != "" {
		errs = append(errs, FieldError{"extra_body",
			fmt.Sprintf("field %q has credential semantics — use api_key_ref (create the secret first)", hit)})
	}

	for role, secs := range b.Timeouts {
		if secs <= 0 {
			errs = append(errs, FieldError{"timeouts",
				fmt.Sprintf("timeout for role %q must be > 0 seconds", role)})
		}
	}
	return errs
}
