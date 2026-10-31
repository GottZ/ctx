package backends

import (
	"strings"
	"testing"
)

func validBackend() Backend {
	return Backend{
		Name:          "test-backend",
		Host:          "http://backend.example:8089",
		Protocol:      ProtocolOpenAI,
		ProviderClass: ProviderGeneric,
		Trust:         TrustPublic,
		Locality:      LocalityExternal,
		Roles:         []string{RoleSynthesis},
		ModelMap:      map[string]ModelSpec{"default": {Model: "m"}},
		Enabled:       true,
	}
}

func fieldHit(errs []FieldError, field string) bool {
	for _, e := range errs {
		if e.Field == field {
			return true
		}
	}
	return false
}

func TestValidateBackendHappyPath(t *testing.T) {
	b := validBackend()
	warnings, errs := ValidateBackend(&b)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %+v", errs)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
}

// TestHeaderDenylist: credential-carrier headers are 422 with the
// api_key_ref pointer — the convenient unfenced path for a plaintext key
// (row + every list response + append-only audit) stays closed.
func TestHeaderDenylist(t *testing.T) {
	for _, h := range []string{
		"Authorization", "authorization", "Proxy-Authorization", "Cookie",
		"X-Api-Key", "X-Custom-Token", "openai_key", "Provider-Key", "ACCESS_TOKEN",
	} {
		b := validBackend()
		b.ExtraHeaders = map[string]string{h: "secret-value"}
		_, errs := ValidateBackend(&b)
		if !fieldHit(errs, "extra_headers") {
			t.Errorf("header %q passed the denylist", h)
		}
	}

	// Harmless headers pass.
	b := validBackend()
	b.ExtraHeaders = map[string]string{"HTTP-Referer": "https://github.com/GottZ/ctx", "X-Title": "ctx"}
	if _, errs := ValidateBackend(&b); fieldHit(errs, "extra_headers") {
		t.Errorf("harmless headers rejected: %+v", errs)
	}
}

func TestExtraBodyCredentialKeys(t *testing.T) {
	b := validBackend()
	b.ExtraBody = map[string]any{"provider": map[string]any{"api_key": "sk-…"}}
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "extra_body") {
		t.Error("nested credential field passed")
	}

	b = validBackend()
	b.ExtraBody = map[string]any{"provider": map[string]any{"require_parameters": true}}
	if _, errs := ValidateBackend(&b); fieldHit(errs, "extra_body") {
		t.Error("harmless extra_body rejected")
	}
}

// TestLocalityCrossValidation: a publicly routable host must be external —
// the egress audit (partial index on locality) depends on this field.
func TestLocalityCrossValidation(t *testing.T) {
	b := validBackend()
	b.Host = "https://openrouter.ai/api/v1"
	b.Locality = LocalityLAN
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "locality") {
		t.Error("public host declared lan passed validation")
	}

	b.Locality = LocalityExternal
	if _, errs := ValidateBackend(&b); fieldHit(errs, "locality") {
		t.Error("correct external locality rejected")
	}

	// Conservative direction is allowed: a LAN host MAY be declared external
	// (more audit coverage, never less).
	b = validBackend()
	b.Host = "http://10.13.37.11:8089"
	b.Locality = LocalityExternal
	if _, errs := ValidateBackend(&b); fieldHit(errs, "locality") {
		t.Error("conservative external on private host rejected")
	}
}

func TestDeriveLocality(t *testing.T) {
	cases := []struct {
		url  string
		want string
	}{
		{"http://localhost:8089", LocalityLocal},
		{"http://127.0.0.1:11434", LocalityLocal},
		{"http://10.13.37.11:8089", LocalityLAN},
		{"http://192.168.1.5", LocalityLAN},
		{"http://172.16.0.7:8080", LocalityLAN},
		{"http://llama-cpu:8080", LocalityLAN}, // docker single-label
		{"http://llama-cpu.:8080", LocalityLAN}, // root dot keeps it single-label
		{"http://LOCALHOST:8080", LocalityLocal},
		{"http://localhost.:8080", LocalityLocal},
		{"http://0.0.0.0:8080", LocalityLocal}, // unspecified dials the local host
		{"http://[::]:8080", LocalityLocal},
		{"http://169.254.169.254", LocalityLAN}, // cloud metadata
		{"https://openrouter.ai/api/v1", LocalityExternal},
		{"http://8.8.8.8", LocalityExternal},
	}
	for _, c := range cases {
		got, err := DeriveLocality(c.url)
		if err != nil {
			t.Errorf("%s: %v", c.url, err)
			continue
		}
		if got != c.want {
			t.Errorf("DeriveLocality(%s) = %s, want %s", c.url, got, c.want)
		}
	}
	if _, err := DeriveLocality("://broken"); err == nil {
		t.Error("broken URL must error")
	}
}

// TestEmbedExternalBlock: embed roles on external backends are a hard 422
// without the equivalence proof — index corruption by foreign quantization
// is irreversible at scale.
func TestEmbedExternalBlock(t *testing.T) {
	for _, role := range []string{RoleEmbed, RoleDreamEmbed} {
		b := validBackend()
		b.Host = "https://api.example.com/v1"
		b.Locality = LocalityExternal
		b.Roles = []string{role}
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "roles") {
			t.Errorf("external %s passed without equivalence flag", role)
		}

		b.Metadata = map[string]any{"embed_equivalence_verified": true}
		if _, errs := ValidateBackend(&b); fieldHit(errs, "roles") {
			t.Errorf("external %s with equivalence flag rejected", role)
		}
	}

	// Local embed needs no flag.
	b := validBackend()
	b.Host = "http://127.0.0.1:8090"
	b.Locality = LocalityLocal
	b.Roles = []string{RoleEmbed}
	if _, errs := ValidateBackend(&b); fieldHit(errs, "roles") {
		t.Error("local embed rejected")
	}
}

// TestClassifyExternalBlock (G41): the classify role is hard-local. Unlike
// embed there is NO metadata escape hatch — audit prompts carry unclassified
// block content, which never crosses the locality border, full-trust ZDR
// included.
func TestClassifyExternalBlock(t *testing.T) {
	b := validBackend()
	b.Host = "https://api.example.com/v1"
	b.Locality = LocalityExternal
	b.Roles = []string{RoleClassify}
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "roles") {
		t.Error("external classify passed validation")
	}

	// No escape hatch: the embed equivalence flag must NOT unlock classify.
	b.Metadata = map[string]any{"embed_equivalence_verified": true}
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "roles") {
		t.Error("external classify passed with the embed equivalence flag — there must be no escape hatch")
	}

	// LAN and local are fine.
	for _, tc := range []struct{ host, locality string }{
		{"http://10.13.37.11:8089", LocalityLAN},
		{"http://127.0.0.1:8089", LocalityLocal},
	} {
		b := validBackend()
		b.Host = tc.host
		b.Locality = tc.locality
		b.Roles = []string{RoleClassify}
		if _, errs := ValidateBackend(&b); fieldHit(errs, "roles") {
			t.Errorf("%s classify rejected", tc.locality)
		}
	}
}

func TestModelMapCoverage(t *testing.T) {
	b := validBackend()
	b.Roles = []string{RoleSynthesis, RoleTranslate}
	b.ModelMap = map[string]ModelSpec{RoleSynthesis: {Model: "m"}} // translate uncovered, no default
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "model_map") {
		t.Error("uncovered core role passed")
	}

	b.ModelMap["default"] = ModelSpec{Model: "fallback"}
	if _, errs := ValidateBackend(&b); fieldHit(errs, "model_map") {
		t.Error("default-covered role rejected")
	}

	// Non-core roles warn but do not require coverage.
	b = validBackend()
	b.Roles = []string{"proxy:myapp"}
	b.ModelMap = nil
	warnings, errs := ValidateBackend(&b)
	if fieldHit(errs, "model_map") {
		t.Error("non-core role required model coverage")
	}
	if len(warnings) == 0 || !strings.Contains(warnings[0], "proxy:myapp") {
		t.Errorf("non-core role warning missing: %v", warnings)
	}
}

func TestDreamOnLocalWarns(t *testing.T) {
	b := validBackend()
	b.Host = "http://127.0.0.1:8089"
	b.Locality = LocalityLocal
	b.Roles = []string{RoleDream}
	warnings, _ := ValidateBackend(&b)
	found := false
	for _, w := range warnings {
		if strings.Contains(w, "dream role on a local backend") {
			found = true
		}
	}
	if !found {
		t.Errorf("dream-on-local warning missing: %v", warnings)
	}

	// A per-role dream timeout override is the designed CPU path — the
	// warning must fall silent for a correctly configured backend.
	b.Timeouts = map[string]int{RoleDream: 900}
	warnings, _ = ValidateBackend(&b)
	for _, w := range warnings {
		if strings.Contains(w, "dream role on a local backend") {
			t.Errorf("warning must be suppressed by a dream timeout override: %v", warnings)
		}
	}
}

func TestValidateEnums(t *testing.T) {
	b := validBackend()
	b.Protocol = "grpc"
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "protocol") {
		t.Error("bad protocol passed")
	}

	b = validBackend()
	b.Trust = "trusted-bro"
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "trust") {
		t.Error("bad trust passed")
	}

	b = validBackend()
	b.Host = "ftp://nope"
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
		t.Error("non-http URL passed")
	}

	b = validBackend()
	b.Name = "  "
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "name") {
		t.Error("blank name passed")
	}

	b = validBackend()
	b.Timeouts = map[string]int{RoleSynthesis: -5}
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "timeouts") {
		t.Error("negative timeout passed")
	}

	b = validBackend()
	b.Metadata = map[string]any{"score_domain": "probabilty"} // typo must be caught at write time
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "metadata.score_domain") {
		t.Error("bad score_domain passed")
	}

	b = validBackend()
	b.Metadata = map[string]any{"score_domain": 42} // non-string is invalid too
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "metadata.score_domain") {
		t.Error("non-string score_domain passed")
	}

	for _, v := range []string{ScoreDomainAuto, ScoreDomainLogit, ScoreDomainProbability} {
		b = validBackend()
		b.Metadata = map[string]any{"score_domain": v}
		if _, errs := ValidateBackend(&b); fieldHit(errs, "metadata.score_domain") {
			t.Errorf("valid score_domain %q rejected", v)
		}
	}
}

// TestVersionedRootRejected (issue #21): the openai/rerank wire paths append
// their version segment themselves — a base_url already ending in /v1 (the
// form cloud examples teach) double-segments every call to /v1/v1/… and
// produces three disconnected-looking failures on openrouter (probe
// unreachable, window discovery 404 → fail-closed synthesis, chat 404).
// Silent acceptance at create time is what made #21 hard to diagnose.
func TestVersionedRootRejected(t *testing.T) {
	// openrouter class: /v1 and /v1/ are both 422 — as are the two verified
	// bypass shapes: uppercase /V1 (404s case-sensitively anyway, but with
	// the same three disconnected symptoms) and a trailing space (the
	// realistic copy-paste entry; url.Parse is lenient about it).
	for _, host := range []string{
		"https://openrouter.ai/api/v1", "https://openrouter.ai/api/v1/",
		"https://openrouter.ai/api/V1", "https://openrouter.ai/api/v1 ",
	} {
		b := validBackend()
		b.ProviderClass = ProviderOpenRouter
		b.Host = host
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
			t.Errorf("openrouter base_url %q passed validation", host)
		}
	}

	// The correct openrouter root passes.
	b := validBackend()
	b.ProviderClass = ProviderOpenRouter
	b.Host = "https://openrouter.ai/api"
	if _, errs := ValidateBackend(&b); fieldHit(errs, "base_url") {
		t.Error("versionless openrouter root rejected")
	}

	// Generic openai backends share the same wire (chat /v1/chat/completions,
	// embed /v1/embeddings) — the doubling is identical, so the guard applies.
	b = validBackend()
	b.Host = "https://api.example.com/v1"
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
		t.Error("generic openai /v1 root passed validation")
	}

	// rerank protocol appends /v1/rerank itself.
	b = validBackend()
	b.Protocol = "rerank"
	b.Host = "http://10.13.37.11:8082/v1"
	b.Locality = LocalityLAN
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
		t.Error("rerank /v1 root passed validation")
	}

	// ollama protocol appends /api/… — /v1 there is odd but does NOT double,
	// so it stays outside this guard.
	b = validBackend()
	b.Protocol = ProtocolOllama
	b.Host = "http://10.13.37.11:11434/v1"
	b.Locality = LocalityLAN
	if _, errs := ValidateBackend(&b); fieldHit(errs, "base_url") {
		t.Error("ollama /v1 root rejected — guard must stay bounded to the proven doubling")
	}

	// A path that merely CONTAINS v1 is fine.
	b = validBackend()
	b.Host = "https://api.example.com/v1proxy"
	if _, errs := ValidateBackend(&b); fieldHit(errs, "base_url") {
		t.Error("non-suffix v1 segment rejected")
	}
}

// TestValidateUserinfoRejected is the pool-side pendant of the config check
// V7 (validateHostURL): a base_url carrying userinfo is a write-time 422, and
// the rejection itself must not repeat the credential — the error text is the
// one place a leak would reappear after the guard fired.
func TestValidateUserinfoRejected(t *testing.T) {
	const needle = "sk-live-0123456789abcdef"

	for _, host := range []string{
		"https://x:" + needle + "@api.example.com",
		"http://x:" + needle + "@api.example.com",
		"https://" + needle + "@api.example.com", // username-only shape
		"https://x:" + needle + "@api.example.com/base",
	} {
		b := validBackend()
		b.Host = host
		_, errs := ValidateBackend(&b)
		if !fieldHit(errs, "base_url") {
			t.Errorf("userinfo base_url %q passed validation", strings.Replace(host, needle, "REDACTED", 1))
			continue
		}
		for _, e := range errs {
			if strings.Contains(e.Message, needle) {
				t.Errorf("credential echoed back in the %s error: %s", e.Field, e.Message)
			}
		}
	}

	// Empty userinfo is still userinfo — url.Parse yields a non-nil User.
	b := validBackend()
	b.Host = "https://@api.example.com"
	if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
		t.Error("empty userinfo base_url passed validation")
	}

	// Negative: userinfo-free URLs stay accepted — including an @ in the
	// path, which is not userinfo.
	for _, host := range []string{
		"https://api.example.com",
		"http://backend.example:8089",
		"https://api.example.com/tenant@acme",
	} {
		b := validBackend()
		b.Host = host
		if _, errs := ValidateBackend(&b); fieldHit(errs, "base_url") {
			t.Errorf("userinfo-free base_url %q rejected", host)
		}
	}
}

func TestScoreDomainResolution(t *testing.T) {
	b := Backend{}
	if got := b.ScoreDomain(); got != ScoreDomainAuto {
		t.Errorf("no metadata: ScoreDomain() = %q, want auto", got)
	}
	b.Metadata = map[string]any{"score_domain": "probability"}
	if got := b.ScoreDomain(); got != ScoreDomainProbability {
		t.Errorf("ScoreDomain() = %q, want probability", got)
	}
	b.Metadata = map[string]any{"score_domain": "logit"}
	if got := b.ScoreDomain(); got != ScoreDomainLogit {
		t.Errorf("ScoreDomain() = %q, want logit", got)
	}
	// Hand-edited garbage resolves to auto at runtime (validation rejects
	// it at write time; this is the belt-and-suspenders path).
	b.Metadata = map[string]any{"score_domain": "bogus"}
	if got := b.ScoreDomain(); got != ScoreDomainAuto {
		t.Errorf("invalid value: ScoreDomain() = %q, want auto fallback", got)
	}
}

// anthropicBackend is the minimal row the native Messages API wire accepts:
// keyed, external, generic, with a declared window and a chat-class role.
func anthropicBackend() Backend {
	b := validBackend()
	b.Protocol = ProtocolAnthropic
	b.Host = "https://api.anthropic.com"
	b.APIKeyRef = "anthropic-chat"
	b.NumCtx = 100000
	b.Roles = []string{RoleSynthesis, RoleTranslate, RoleDream, RoleDigest, RoleChat}
	return b
}

// TestValidateAnthropicHappyPath pins that the documented minimal row passes
// — every rule test below mutates exactly one field away from it, so a
// regression in the fixture itself would turn all of them green for the
// wrong reason.
func TestValidateAnthropicHappyPath(t *testing.T) {
	b := anthropicBackend()
	if _, errs := ValidateBackend(&b); len(errs) != 0 {
		t.Fatalf("minimal anthropic row rejected: %v", errs)
	}
}

// TestValidateAnthropicRules walks the runtime failures the protocol turns
// into 422s: keyless (401 on first call), non-external locality (egress audit
// hole), missing num_ctx (synthesis fails closed, no window discovery),
// non-generic class (ZDR force / llama.cpp probe against the wrong provider),
// embed and rerank roles (no such endpoints — embed.go would silently take
// the Ollama path), and a mistyped effort in either carrier.
func TestValidateAnthropicRules(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(b *Backend)
		field string
	}{
		{"keyless", func(b *Backend) { b.APIKeyRef = "" }, "api_key_ref"},
		{"lan locality", func(b *Backend) { b.Host = "http://10.0.0.2:8080"; b.Locality = LocalityLAN }, "locality"},
		{"no num_ctx", func(b *Backend) { b.NumCtx = 0 }, "num_ctx"},
		{"openrouter class", func(b *Backend) { b.ProviderClass = ProviderOpenRouter }, "provider_class"},
		{"llamacpp class", func(b *Backend) { b.ProviderClass = ProviderLlamaCpp }, "provider_class"},
		{"embed role", func(b *Backend) { b.Roles = append(b.Roles, RoleEmbed) }, "roles"},
		{"dream-embed role", func(b *Backend) { b.Roles = append(b.Roles, RoleDreamEmbed) }, "roles"},
		{"rerank role", func(b *Backend) { b.Roles = append(b.Roles, RoleRerank) }, "roles"},
		{"extra_body effort typo", func(b *Backend) { b.ExtraBody = map[string]any{"effort": "mediumm"} }, "extra_body"},
		{"extra_body effort non-string", func(b *Backend) { b.ExtraBody = map[string]any{"effort": 2} }, "extra_body"},
		{"model_map effort typo", func(b *Backend) {
			b.ModelMap = map[string]ModelSpec{"default": {Model: "m", Params: map[string]any{"effort": "ultra"}}}
		}, "model_map"},
		{"versioned root", func(b *Backend) { b.Host = "https://api.anthropic.com/v1" }, "base_url"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := anthropicBackend()
			tc.mut(&b)
			if _, errs := ValidateBackend(&b); !fieldHit(errs, tc.field) {
				t.Errorf("want a %q error, got %v", tc.field, errs)
			}
		})
	}

	// Valid efforts pass in both carriers.
	for _, e := range []string{"low", "medium", "high", "xhigh", "max"} {
		b := anthropicBackend()
		b.ExtraBody = map[string]any{"effort": e}
		b.ModelMap = map[string]ModelSpec{"default": {Model: "m", Params: map[string]any{"effort": e}}}
		if _, errs := ValidateBackend(&b); len(errs) != 0 {
			t.Errorf("effort %q rejected: %v", e, errs)
		}
	}

	// The rules are protocol-bound: an openai row keeps its freedoms (keyless,
	// no num_ctx, embed roles on a verified external backend are the existing
	// contracts and must not regress).
	b := validBackend()
	b.NumCtx = 0
	if _, errs := ValidateBackend(&b); fieldHit(errs, "num_ctx") || fieldHit(errs, "api_key_ref") {
		t.Errorf("anthropic rules leaked onto an openai row: %v", errs)
	}
}

// TestValidateAnthropicRejectsUnservedKeys (review finding 3): both carriers
// merge verbatim into the Messages API body, so a key that body cannot take
// is a 400 on every call — turned into a 422 at write time like the effort
// typo above. extra_body: the sampling keys (400 on the Claude 5 models,
// removed on every model by API version 2026-10-21), the OpenAI-compatible /
// Ollama dialect fields, and the fields the wire builds itself. model_map
// params: the same, except the keys applyModelParams routes into dedicated
// Options fields this wire never marshals (temperature, top_p, top_k, min_p,
// repeat_penalty, presence_penalty, num_predict, max_tokens, think).
func TestValidateAnthropicRejectsUnservedKeys(t *testing.T) {
	bodyRejected := []string{
		"temperature", "top_p", "top_k", "min_p", "repeat_penalty", "presence_penalty",
		"chat_template_kwargs", "response_format", "reasoning", "reasoning_effort", "seed",
		"frequency_penalty", "logit_bias", "logprobs", "top_logprobs", "n", "stop",
		"stream_options", "parallel_tool_calls", "max_completion_tokens", "user", "provider",
		"format", "options", "keep_alive", "num_ctx",
		"model", "messages", "system", "stream", "tools", "tool_choice",
	}
	for _, k := range bodyRejected {
		b := anthropicBackend()
		b.ExtraBody = map[string]any{k: true}
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "extra_body") {
			t.Errorf("extra_body %q accepted: %v", k, errs)
		}
	}
	paramsRejected := []string{
		"chat_template_kwargs", "response_format", "reasoning", "seed", "frequency_penalty",
		"logit_bias", "stop", "user", "format", "stream", "tools", "tool_choice", "messages",
	}
	for _, k := range paramsRejected {
		b := anthropicBackend()
		b.ModelMap = map[string]ModelSpec{"default": {Model: "m"}, "dream": {Model: "m", Params: map[string]any{k: true}}}
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "model_map") {
			t.Errorf("model_map params %q accepted: %v", k, errs)
		}
	}
	for name, oc := range map[string]any{
		"output_config.effort (use effort)": map[string]any{"effort": "high"},
		"output_config not an object":       "high",
	} {
		b := anthropicBackend()
		b.ExtraBody = map[string]any{"output_config": oc}
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "extra_body") {
			t.Errorf("extra_body %s accepted: %v", name, errs)
		}
		b = anthropicBackend()
		b.ModelMap = map[string]ModelSpec{"default": {Model: "m", Params: map[string]any{"output_config": oc}}}
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "model_map") {
			t.Errorf("model_map params %s accepted: %v", name, errs)
		}
	}

	// What stays allowed: the dedicated-field params (never marshalled on
	// this wire), and the extra_body hatch for fields the wire does not model.
	b := anthropicBackend()
	b.ModelMap = map[string]ModelSpec{"default": {Model: "m", Params: map[string]any{
		"temperature": 0.2, "top_p": 0.9, "top_k": 40.0, "min_p": 0.05, "repeat_penalty": 1.1,
		"presence_penalty": 0.1, "num_predict": 900.0, "max_tokens": 900.0, "think": false, "effort": "high",
	}}}
	b.ExtraBody = map[string]any{
		"thinking":       map[string]any{"type": "adaptive", "display": "summarized"},
		"metadata":       map[string]any{"user_id": "ctx"},
		"stop_sequences": []any{"END"},
		"max_tokens":     4096.0,
		"output_config":  map[string]any{"task_budget": map[string]any{"type": "tokens", "total": 20000.0}},
	}
	if _, errs := ValidateBackend(&b); len(errs) != 0 {
		t.Errorf("allowed keys rejected: %v", errs)
	}

	// Protocol-bound: an openai row keeps its dialect.
	o := validBackend()
	o.ExtraBody = map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}, "temperature": 0.1}
	if _, errs := ValidateBackend(&o); fieldHit(errs, "extra_body") {
		t.Errorf("anthropic key rules leaked onto an openai row: %v", errs)
	}
}

// TestBaseURLQueryFragmentRejected: a '?' or '#' in base_url turns the
// appended wire path into query/fragment text (GHSA 2026-10-31 §3), so the
// request lands on whatever path the URL names. 422 for every scope.
func TestBaseURLQueryFragmentRejected(t *testing.T) {
	for _, scope := range []string{"", GlobalScope, "acme"} {
		for _, host := range []string{
			"https://api.example.com/_search#",
			"https://api.example.com/admin#x",
			"https://api.example.com/x?",
			"https://api.example.com/x?a=b",
		} {
			b := validBackend()
			b.Scope = scope
			b.Host = host
			if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
				t.Errorf("scope %q: base_url %q passed, want 422", scope, host)
			}
		}
	}
}

// TestTenantBackendNeedsPublicHost: a tenant row may only point at a
// publicly routable host; the same LAN host stays legal on a _global
// (server-admin) row and on an unscoped tuple.
func TestTenantBackendNeedsPublicHost(t *testing.T) {
	internal := []struct{ host, locality string }{
		{"http://127.0.0.1:11434", LocalityLocal},
		{"http://localhost:8080", LocalityLocal},
		{"http://0.0.0.0:8080", LocalityLocal},
		{"http://[::1]:8080", LocalityLocal},
		{"http://10.0.0.5:8080", LocalityLAN},
		{"http://192.168.1.5", LocalityLAN},
		{"http://169.254.169.254", LocalityLAN},
		{"http://[fd00::1]:8080", LocalityLAN},
		{"http://ollama:11434", LocalityLAN},
	}
	for _, c := range internal {
		b := validBackend()
		b.Scope = "acme"
		b.Host = c.host
		b.Locality = c.locality
		if _, errs := ValidateBackend(&b); !fieldHit(errs, "base_url") {
			t.Errorf("tenant row with %s passed, want base_url 422", c.host)
		}
		for _, shared := range []string{GlobalScope, ""} {
			b.Scope = shared
			if _, errs := ValidateBackend(&b); fieldHit(errs, "base_url") {
				t.Errorf("scope %q row with %s rejected: %+v", shared, c.host, errs)
			}
		}
	}

	b := validBackend()
	b.Scope = "acme"
	b.Host = "https://openrouter.ai/api"
	if _, errs := ValidateBackend(&b); len(errs) != 0 {
		t.Errorf("tenant row with a public host rejected: %+v", errs)
	}
}

// TestEgressGuarded pins the predicate the wire clients key on: tenant
// rows are guarded, _global and unscoped tuples are not.
func TestEgressGuarded(t *testing.T) {
	cases := map[string]bool{"": false, GlobalScope: false, "acme": true, "acme.team": true}
	for scope, want := range cases {
		b := Backend{Scope: scope}
		if got := b.EgressGuarded(); got != want {
			t.Errorf("EgressGuarded(scope %q) = %v, want %v", scope, got, want)
		}
	}
}
