package backends

import (
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strings"

	"github.com/GottZ/ctx/internal/ssrfguard"
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
	// Lower-case and drop the root dot: "LOCALHOST" and "localhost." resolve
	// exactly like "localhost", and "llama-cpu." is still a single label.
	host := strings.TrimSuffix(strings.ToLower(u.Hostname()), ".")
	if host == "" {
		return "", fmt.Errorf("URL has no host")
	}
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return LocalityLocal, nil
	}
	if ip := net.ParseIP(host); ip != nil {
		switch {
		case ip.IsLoopback(), ip.IsUnspecified():
			// 0.0.0.0 / [::] connect to the local host on Linux.
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
	errs = append(errs, validateAnthropic(b)...)
	return warnings, errs
}

// validateIdentity covers name, URL, enums and the locality cross-check.
func validateIdentity(b *Backend) (errs []FieldError) {
	if strings.TrimSpace(b.Name) == "" {
		errs = append(errs, FieldError{"name", "name is required"})
	}

	u, err := url.Parse(b.Host)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		errs = append(errs, FieldError{"base_url", "must be an absolute http(s) URL"})
	case u.User != nil:
		// Pool-side pendant of the config check V7. userinfo in base_url
		// bypasses the field-name-based secret masking everywhere the host
		// flows: it is echoed by every backend list/create response (the
		// handler redacts api_key, not the URL), lands in error logs, and
		// via the audit hook permanently in the append-only settings audit.
		// Same opsec edge as headerDenylist above, same escape hatch:
		// api_key_ref. The message never echoes anything derived from the
		// input — url.Redacted() masks only the password and would still
		// carry the username.
		errs = append(errs, FieldError{"base_url",
			"credentials in base_url are not allowed (they would leak into every list response, log line and the append-only audit) — use api_key_ref instead: create the secret first"})
	}

	errs = append(errs, validateVersionedRoot(b)...)
	errs = append(errs, validateEgressTarget(b)...)

	switch b.Protocol {
	case ProtocolOpenAI, ProtocolOllama, ProtocolAnthropic, "rerank":
	default:
		errs = append(errs, FieldError{"protocol", "must be one of: openai, ollama, anthropic, rerank"})
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
// validateEgressTarget closes the two base_url shapes behind GHSA 2026-10-31
// §3 (tenant-admin SSRF):
//
//   - '?' or '#' anywhere in base_url. Every wire builds its URL as
//     base_url + "/v1/..." by string concatenation, so a query or fragment
//     marker turns the appended API path into query/fragment text that is
//     never routed: "http://internal:9200/_search#" POSTs the request body to
//     /_search instead of /v1/chat/completions. No legitimate API root
//     carries either, so the rule holds for every scope.
//   - a tenant-owned row (EgressGuarded) whose host is not publicly routable
//     (DeriveLocality local/lan: loopback, RFC 1918, link-local, unspecified,
//     single-label docker names). LAN backends stay a server-admin choice on
//     '_global' rows. This is the static, network-free half; the wire clients
//     re-check every resolved address at dial time (ssrfguard), which also
//     covers public names that resolve privately and redirects.
//
// Unparseable URLs are left to the absolute-URL rule above (one error each).
func validateEgressTarget(b *Backend) []FieldError {
	var errs []FieldError
	if strings.ContainsAny(b.Host, "?#") {
		errs = append(errs, FieldError{"base_url",
			"base_url must not contain '?' or '#' — wire paths are appended to it, and a query or fragment would cut them off"})
	}
	if b.EgressGuarded() {
		if loc, err := DeriveLocality(b.Host); err == nil && (loc != LocalityExternal || literalIPDenied(b.Host)) {
			errs = append(errs, FieldError{"base_url",
				"a tenant backend needs a publicly routable host — loopback, private, link-local, special-purpose and single-label hosts are reserved to _global (server-admin) backends"})
		}
	}
	return errs
}

// literalIPDenied reports whether base_url names a literal IP in the dial-time
// deny-list (ssrfguard.IsDeniedIP: CGNAT 100.64/10, 198.18/15, 240/4, NAT64,
// multicast, documentation ranges …). DeriveLocality only knows the net.IP
// predicates and calls those ranges "external"; without this the write path
// would admit a row the guarded client then refuses on every call. Hostnames
// stay to the dial-time check (no DNS here).
func literalIPDenied(baseURL string) bool {
	u, err := url.Parse(baseURL)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.TrimSuffix(u.Hostname(), "."))
	return ip != nil && ssrfguard.IsDeniedIP(ip)
}

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
	case b.Protocol == ProtocolAnthropic:
		// Same doubling on the Messages API: the wire path is
		// {base_url}/v1/messages, the probe {base_url}/v1/models.
		return []FieldError{{"base_url",
			"base_url is the API root WITHOUT the version segment (the anthropic wire appends /v1/messages itself) — use https://api.anthropic.com, not .../v1"}}
	}
	return nil
}

// validateAnthropic covers the row shape the native Messages API wire needs
// (llm/anthropic.go). Every rule here is a runtime failure turned into a 422:
// a keyless row 401s on the first call, a non-external locality would drop
// api.anthropic.com from the egress audit, a missing num_ctx fails synthesis
// closed (promptguard.ErrUndeclaredWindow — there is no window discovery for
// this wire), a non-generic provider class would run the OpenRouter ZDR force
// or the llama.cpp /health probe against a provider that has neither, and an
// embed/rerank role would fall onto the Ollama /api/embed path (embed.go
// dispatches "anything but openai" there) with a vector-space corruption
// risk — the Messages API has no embeddings endpoint at all.
func validateAnthropic(b *Backend) (errs []FieldError) {
	if b.Protocol != ProtocolAnthropic {
		return nil
	}
	if strings.TrimSpace(b.APIKeyRef) == "" {
		errs = append(errs, FieldError{"api_key_ref", "anthropic protocol requires api_key_ref (create the secret first; the key travels as x-api-key)"})
	}
	if b.Locality != "" && b.Locality != LocalityExternal {
		errs = append(errs, FieldError{"locality", "anthropic protocol requires locality 'external' (the Messages API is a hosted provider)"})
	}
	if b.NumCtx <= 0 {
		errs = append(errs, FieldError{"num_ctx", "anthropic protocol requires num_ctx > 0 (no window discovery on this wire; synthesis fails closed without a declared window)"})
	}
	if b.ProviderClass != "" && b.ProviderClass != ProviderGeneric {
		errs = append(errs, FieldError{"provider_class", "anthropic protocol requires provider_class 'generic'"})
	}
	for _, r := range b.Roles {
		if embedRoles[r] || r == RoleRerank {
			errs = append(errs, FieldError{"roles",
				fmt.Sprintf("role %q is not served by the anthropic protocol (the Messages API has no embeddings or rerank endpoint)", r)})
		}
	}
	if raw, ok := b.ExtraBody["effort"]; ok {
		if v, isStr := raw.(string); !isStr || !AnthropicEfforts[v] {
			errs = append(errs, FieldError{"extra_body", "effort must be one of: low, medium, high, xhigh, max"})
		}
	}
	for role, spec := range b.ModelMap {
		if raw, ok := spec.Params["effort"]; ok {
			if v, isStr := raw.(string); !isStr || !AnthropicEfforts[v] {
				errs = append(errs, FieldError{"model_map",
					fmt.Sprintf("role %q: params.effort must be one of: low, medium, high, xhigh, max", role)})
			}
		}
	}
	return append(errs, validateAnthropicCarrierKeys(b)...)
}

// anthropicSamplingKeys are rejected by the Claude 5 models with a 400 and
// removed on every model by Messages API version 2026-10-21
// (platform.claude.com/docs/en/api/versioning). The wire never marshals
// Options' sampling fields; these entries close the extra_body hatch.
var anthropicSamplingKeys = map[string]bool{"temperature": true, "top_p": true, "top_k": true}

// anthropicForeignKeys are request fields of the OpenAI-compatible,
// llama.cpp/vLLM, OpenRouter and Ollama dialects that the Messages API does
// not define — merged top-level they are a 400 on every call. Not a closed
// list of what the API rejects: the extra_body hatch stays open for Messages
// API fields the wire does not model (thinking.display, stop_sequences,
// metadata.user_id, …), so this names the keys a row migrated from an
// openai/ollama row is likely to carry.
var anthropicForeignKeys = map[string]bool{
	"chat_template_kwargs": true, "response_format": true, "reasoning": true, "reasoning_effort": true,
	"seed": true, "frequency_penalty": true, "presence_penalty": true, "logit_bias": true,
	"logprobs": true, "top_logprobs": true, "n": true, "stop": true, "stream_options": true,
	"parallel_tool_calls": true, "max_completion_tokens": true, "min_p": true, "repeat_penalty": true,
	"user": true, "provider": true, "transforms": true, "models": true,
	"format": true, "options": true, "keep_alive": true, "num_ctx": true,
}

// anthropicWireOwnedKeys are fields the anthropic wire builds itself; an
// override from a carrier would replace the request it parses the answer of
// (stream on the non-stream path, the cached system blocks, the closing
// call's tool_choice none — and forced tool_choice is a 400 on Claude Opus
// 5.5). max_tokens is deliberately NOT here: a numeric extra_body.max_tokens
// is the documented per-row cap override on every wire.
var anthropicWireOwnedKeys = map[string]bool{
	"model": true, "messages": true, "system": true, "stream": true, "tools": true, "tool_choice": true,
}

// anthropicDedicatedParams are the model_map params applyModelParams routes
// into dedicated Options fields; the anthropic wire marshals none of them
// (sampling) or derives max_tokens / ignores think itself, so they never
// reach the body and stay allowed in params.
var anthropicDedicatedParams = map[string]bool{
	"temperature": true, "top_p": true, "top_k": true, "min_p": true, "repeat_penalty": true,
	"presence_penalty": true, "num_predict": true, "max_tokens": true, "think": true,
}

// anthropicOutputConfigKey is merged INTO the wire's output_config
// (llm.buildAnthropicBody), so it must be an object and must not carry the
// effort, which has its own key and precedence.
const anthropicOutputConfigKey = "output_config"

// validateAnthropicCarrierKeys rejects carrier keys the Messages API body
// cannot take (review finding 3): extra_body merges top-level verbatim, and
// model_map params reach the body through applyModelParams' default branch.
func validateAnthropicCarrierKeys(b *Backend) (errs []FieldError) {
	for _, k := range slices.Sorted(maps.Keys(b.ExtraBody)) {
		if msg := anthropicKeyProblem(k, b.ExtraBody[k], false); msg != "" {
			errs = append(errs, FieldError{"extra_body", msg})
		}
	}
	for _, role := range slices.Sorted(maps.Keys(b.ModelMap)) {
		params := b.ModelMap[role].Params
		for _, k := range slices.Sorted(maps.Keys(params)) {
			if msg := anthropicKeyProblem(k, params[k], true); msg != "" {
				errs = append(errs, FieldError{"model_map", fmt.Sprintf("role %q: params.%s", role, msg)})
			}
		}
	}
	return errs
}

// anthropicKeyProblem names why one carrier key cannot reach the Messages
// API body, "" when it can. inParams: the key comes from model_map params,
// where the dedicated-field keys never reach the body.
func anthropicKeyProblem(k string, v any, inParams bool) string {
	if inParams && anthropicDedicatedParams[k] {
		return ""
	}
	switch {
	case anthropicSamplingKeys[k]:
		return fmt.Sprintf("%s is a sampling parameter the Claude 5 models reject with a 400 (removed on every model by API version 2026-10-21) — steer with effort", k)
	case anthropicForeignKeys[k]:
		return fmt.Sprintf("%s is not a Messages API field (OpenAI-compatible/Ollama dialect) — the anthropic wire would send it top-level and get a 400 on every call", k)
	case anthropicWireOwnedKeys[k]:
		return fmt.Sprintf("%s is built by the anthropic wire itself — an override would replace the request it parses", k)
	case k == anthropicOutputConfigKey:
		m, ok := v.(map[string]any)
		if !ok {
			return "output_config must be an object (it is merged into the wire's output_config)"
		}
		if _, has := m["effort"]; has {
			return "output_config.effort is not read — the effort comes from the effort key (model_map params.effort > extra_body.effort > medium)"
		}
	}
	return ""
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
