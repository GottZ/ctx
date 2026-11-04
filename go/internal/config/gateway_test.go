package config

import (
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// gatewayKeys lists the registry keys of the gateway group.
func gatewayKeys(t *testing.T) []entry {
	t.Helper()
	var out []entry
	for _, e := range registry() {
		if strings.HasPrefix(e.Key, "gateway.") {
			out = append(out, e)
		}
	}
	if len(out) == 0 {
		t.Fatal("no gateway.* key in the registry")
	}
	return out
}

// TestGatewayKeysAreHotGlobalOnly pins the class design/02 §3.3 gives the
// whole group: every key is hot (the getter reads one snapshot per request)
// and global-only (one gateway per process; masterplan K3).
func TestGatewayKeysAreHotGlobalOnly(t *testing.T) {
	for _, e := range gatewayKeys(t) {
		ki, ok := KeyByName(e.Key)
		if !ok {
			t.Fatalf("%s: not in the exported key view", e.Key)
		}
		if ki.Mutability != "hot" {
			t.Errorf("%s: mutability %q, want hot", e.Key, ki.Mutability)
		}
		if ki.Tenancy != "global-only" {
			t.Errorf("%s: tenancy %q, want global-only", e.Key, ki.Tenancy)
		}
	}
	if ki, _ := KeyByName("gateway.enabled"); ki.Default != false {
		t.Errorf("gateway.enabled default %#v, want false — the surface stays off until the release wave (masterplan §3.2 duty 1)", ki.Default)
	}
}

// gatewayConsumerKeys is the registry keys of GatewayConsumersConfig — the
// consumer administration, which the manage handler reads from the snapshot
// and the /v1 door never sees (wave W01-5). Read off the struct tags, so the
// exclusion below is exactly that struct and nothing a name pattern catches.
func gatewayConsumerKeys() map[string]bool {
	out := map[string]bool{}
	rt := reflect.TypeOf(GatewayConsumersConfig{})
	for i := range rt.NumField() {
		if k := rt.Field(i).Tag.Get("key"); k != "" {
			out[k] = true
		}
	}
	return out
}

// TestGatewayKeysReachSettings is the totality pin of the getter: every
// gateway.* key, set to a valid non-default value on its own, changes
// GatewaySettings(). A key added to the group without its Settings field —
// or mapped onto the wrong one — is red here. The keys of
// GatewayConsumersConfig are not /v1 settings; TestGatewayConsumerKeysReachConsumers
// holds them to the same standard against their own reader.
func TestGatewayKeysReachSettings(t *testing.T) {
	resetBuildEnv(t)
	base := Defaults().GatewaySettings()
	consumers := gatewayConsumerKeys()
	for _, e := range gatewayKeys(t) {
		if consumers[e.Key] {
			continue
		}
		raw := bumpedValue(t, e)
		c, issues := Build([]Override{{Key: e.Key, Value: raw}}, nil)
		for _, is := range issues {
			t.Errorf("%s=%s: build issue on %s: %s", e.Key, raw, is.Field, is.Msg)
		}
		if reflect.DeepEqual(c.GatewaySettings(), base) {
			t.Errorf("%s=%s does not reach oai.Settings — the getter misses the key", e.Key, raw)
		}
	}
}

// TestGatewayConsumerKeysReachConsumers is the totality pin of the consumer
// administration's keys (W01-5): every key of GatewayConsumersConfig,
// bumped on its own, builds without an issue and changes
// c.Gateway.Consumers — the value the manage handler reads. Eight keys, so a
// key dropped from the struct (and with it from the exclusion above) is red.
func TestGatewayConsumerKeysReachConsumers(t *testing.T) {
	resetBuildEnv(t)
	keys := gatewayConsumerKeys()
	if len(keys) != 8 {
		t.Fatalf("GatewayConsumersConfig has %d keys, want 8", len(keys))
	}
	base := Defaults().Gateway.Consumers
	for _, e := range gatewayKeys(t) {
		if !keys[e.Key] {
			continue
		}
		raw := bumpedValue(t, e)
		c, issues := Build([]Override{{Key: e.Key, Value: raw}}, nil)
		for _, is := range issues {
			t.Errorf("%s=%s: build issue on %s: %s", e.Key, raw, is.Field, is.Msg)
		}
		if reflect.DeepEqual(c.Gateway.Consumers, base) {
			t.Errorf("%s=%s does not reach Gateway.Consumers", e.Key, raw)
		}
	}
}

// TestValidateGatewayConsumers is V40 (W01-5): the grace ceiling and the
// three page floors, one error each, plus the legal edges.
func TestValidateGatewayConsumers(t *testing.T) {
	if issues := validateGatewayConsumers(Defaults()); len(issues) != 0 {
		t.Fatalf("defaults raise V40 issues: %+v", issues)
	}
	g := func(c *Config) *GatewayConsumersConfig { return &c.Gateway.Consumers }
	for _, tc := range []struct {
		key string
		mut func(c *Config)
	}{
		{"gateway.key_rotation_grace", func(c *Config) { g(c).KeyRotationGrace = gatewayMaxRotationGrace + time.Second }},
		{"gateway.consumer_list_max_limit", func(c *Config) { g(c).ListMaxLimit = 0 }},
		{"gateway.consumer_chain_max", func(c *Config) { g(c).ChainMax = 0 }},
		{"gateway.consumer_audit_trail_max", func(c *Config) { g(c).AuditTrailMax = 0 }},
	} {
		c := Defaults()
		tc.mut(c)
		hits := issuesOn(Validate(c), tc.key)
		if len(hits) != 1 || hits[0].Severity != SeverityError {
			t.Errorf("%s: %d issue(s) %+v, want exactly one error", tc.key, len(hits), hits)
		}
	}
	c := Defaults()
	g(c).KeyRotationGrace = gatewayMaxRotationGrace
	g(c).KeyMaxLifetimeApp, g(c).KeyMaxLifetimePeer, g(c).KeyExpiryWarn = 0, 0, 0
	g(c).ListMaxLimit, g(c).ChainMax, g(c).AuditTrailMax = 1, 1, 1
	if issues := validateGatewayConsumers(c); len(issues) != 0 {
		t.Errorf("legal edges raise V40 issues: %+v", issues)
	}
}

// bumpedValue is a valid value different from the key's default.
func bumpedValue(t *testing.T, e entry) string {
	t.Helper()
	switch def := e.defVal.(type) {
	case bool:
		return strconv.FormatBool(!def)
	case int:
		return strconv.Itoa(def + 1)
	case float64:
		return strconv.FormatFloat(def/2, 'f', -1, 64)
	case time.Duration:
		return strconv.Itoa(int(def/time.Second) + 1)
	}
	t.Fatalf("%s: no bump for default type %T", e.Key, e.defVal)
	return ""
}

// TestValidateGateway is V38/V39 (wave W02-02): every refused value of the
// gateway group, one error each, plus the legal edges.
func TestValidateGateway(t *testing.T) {
	if issues := validateGateway(Defaults()); len(issues) != 0 {
		t.Fatalf("defaults raise V38/V39 issues: %+v", issues)
	}
	g := func(c *Config) *GatewayConfig { return &c.Gateway }
	cases := []struct {
		key string
		mut func(c *Config)
	}{
		// V38 — zero sizes and durations.
		{"gateway.max_request_bytes", func(c *Config) { g(c).MaxRequestBytes = 0 }},
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = 0 }},
		{"gateway.max_response_bytes", func(c *Config) { g(c).MaxResponseBytes = 0 }},
		{"gateway.max_stream_frame_bytes", func(c *Config) { g(c).MaxStreamFrameBytes = 0 }},
		{"gateway.embedding_max_floats", func(c *Config) { g(c).EmbeddingMaxFloats = 0 }},
		{"gateway.default_max_tokens", func(c *Config) { g(c).DefaultMaxTokens = 0 }},
		{"gateway.max_output_tokens_ceiling", func(c *Config) { g(c).MaxOutputTokensCeiling = 0 }},
		{"gateway.tenant_alias_ceiling.max_output_tokens", func(c *Config) { g(c).TenantAliasCeiling.MaxOutputTokens = 0 }},
		{"gateway.tenant_alias_ceiling.max_n", func(c *Config) { g(c).TenantAliasCeiling.MaxN = 0 }},
		{"gateway.tenant_alias_ceiling.embedding_max_inputs", func(c *Config) { g(c).TenantAliasCeiling.EmbeddingMaxInputs = 0 }},
		{"gateway.tenant_alias_ceiling.rerank_max_documents", func(c *Config) { g(c).TenantAliasCeiling.RerankMaxDocuments = 0 }},
		{"gateway.tenant_alias_ceiling.timeout", func(c *Config) { g(c).TenantAliasCeiling.Timeout = 0 }},
		{"gateway.stream_commit_grace", func(c *Config) { g(c).StreamCommitGrace = 0 }},
		{"gateway.keepalive_interval", func(c *Config) { g(c).KeepaliveInterval = 0 }},
		{"gateway.client_write_window", func(c *Config) { g(c).ClientWriteWindow = 0 }},
		{"gateway.request_timeout", func(c *Config) { g(c).RequestTimeout = 0 }},
		{"gateway.request_deadline", func(c *Config) { g(c).RequestDeadline = 0 }},
		// V39 — caps and the one cross-field relation.
		{"gateway.max_response_bytes", func(c *Config) { g(c).MaxResponseBytes = 16<<20 + 1 }},
		{"gateway.max_stream_frame_bytes", func(c *Config) { g(c).MaxStreamFrameBytes = 4<<20 + 1 }},
		{"gateway.request_deadline", func(c *Config) { g(c).RequestDeadline = 600 * time.Second }},
		{"gateway.stream_commit_grace", func(c *Config) { g(c).StreamCommitGrace = 51 * time.Second }},
		// One request at every cap: 3 × 8 MiB + max(2 × 4 MiB, 2 × 1 MiB,
		// 4 × 1048576 + 64 KiB) = 32 MiB; one byte less is refused.
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = 32<<20 - 1 }},
		// The response share alone decides the floor here (Fremd-Review
		// finding 5): 3 × request cap would fit, 2 × response cap does not.
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = 3 * g(c).MaxRequestBytes }},
		{"gateway.inflight_budget", func(c *Config) {
			g(c).EmbeddingMaxFloats = 16 << 20 // 64 MiB of float32 vectors
			g(c).InflightBudget = 3*g(c).MaxRequestBytes + 2*g(c).MaxResponseBytes
		}},
		// Fremd-Review 2, FR2-3: caps whose reservation overflows int64 must
		// not wrap around into a negative floor that every budget clears.
		{"gateway.inflight_budget", func(c *Config) { g(c).MaxRequestBytes = 1 << 62 }},
		{"gateway.inflight_budget", func(c *Config) { g(c).EmbeddingMaxFloats = 1 << 62 }},
		{"gateway.retry_after_jitter", func(c *Config) { g(c).RetryAfterJitter = -0.01 }},
		{"gateway.retry_after_jitter", func(c *Config) { g(c).RetryAfterJitter = 1.01 }},
		{"gateway.retry_after_jitter", func(c *Config) { g(c).RetryAfterJitter = math.NaN() }},
	}
	for _, tc := range cases {
		c := Defaults()
		tc.mut(c)
		hits := issuesOn(Validate(c), tc.key)
		if len(hits) != 1 || hits[0].Severity != SeverityError {
			t.Errorf("%s: want exactly one error, got %+v", tc.key, hits)
		}
	}

	legal := []struct {
		name string
		mut  func(c *Config)
	}{
		{"response cap at httpx.MaxResponseBytes", func(c *Config) { g(c).MaxResponseBytes = 16 << 20 }},
		{"frame cap at the internal 4 MiB", func(c *Config) { g(c).MaxStreamFrameBytes = 4 << 20 }},
		{"deadline just below the SDK timeout", func(c *Config) { g(c).RequestDeadline = 599 * time.Second }},
		{"grace at 50 s", func(c *Config) { g(c).StreamCommitGrace = 50 * time.Second }},
		{"budget exactly one request at every cap", func(c *Config) { g(c).InflightBudget = 32 << 20 }},
		{"jitter 0 and 1", func(c *Config) { g(c).RetryAfterJitter = 0 }},
		{"jitter 1", func(c *Config) { g(c).RetryAfterJitter = 1 }},
		{"no tools for tenant aliases", func(c *Config) { g(c).TenantAliasCeiling.ToolsMaxCount = 0 }},
	}
	for _, tc := range legal {
		c := Defaults()
		tc.mut(c)
		if issues := validateGateway(c); len(issues) != 0 {
			t.Errorf("%s is legal, got %+v", tc.name, issues)
		}
	}
}

// TestBootDumpShowsGateway: the boot record "config: effective" carries the
// gateway group, so the operator sees gateway.enabled and the caps at boot.
func TestBootDumpShowsGateway(t *testing.T) {
	args := BootDumpArgs(Defaults(), nil)
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "gateway" {
			return
		}
	}
	t.Error("BootDumpArgs carries no gateway group (dumpGroupOrder)")
}
