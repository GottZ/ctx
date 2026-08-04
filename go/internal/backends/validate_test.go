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
