package config

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GottZ/ctx/internal/oai"
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

// gatewayStringBumps names a valid non-default value per string key of the
// gateway group (W06-9: gateway.body_logging).
var gatewayStringBumps = map[string]string{
	"gateway.body_logging": "full",
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
	case string:
		// A string key is an enum; its bump is a valid non-default member.
		if v, ok := gatewayStringBumps[e.Key]; ok && v != def {
			return v
		}
	case RoleList:
		// One role more than the default; "bump" is a valid pool role.
		return strings.Join(append(slices.Clone([]string(def)), "bump"), ",")
	}
	t.Fatalf("%s: no bump for default type %T", e.Key, e.defVal)
	return ""
}

// gatewayCapFloor is V39's floor at the default caps: 26 × 8 MiB + the read
// term at 8 MiB (8 MiB + 1 MiB, FOLGE-P Nachtrag C-1) + 128 KiB + the JSON
// check's nesting state (10 001 levels × 24 B, FOLGE-P hardening 5 B-2) +
// the scan's nesting state (10 001 levels × 104 B, hardening 7) + 2 × 4 MiB
// = 226.35 MiB (TestBudgetFloorHoldsTheScanBooking).
const gatewayCapFloor = 26*8<<20 + 8<<20 + 1<<20 + 128<<10 + gatewayNestingState + gatewayScanNestingState + 2*4<<20

// gatewayReadTerm is what reading a body of n bytes without a declared
// length spends beyond a declared read (oai.ChunkedReadReserve: the
// doubling chunks of 512 B … 1 MiB and their join) — hand-computed here:
// n + min(n + 512, 1 MiB).
func gatewayReadTerm(n int) int { return n + min(n+512, 1<<20) }

// gatewayNestingState is the most the chat route books for the JSON check's
// nesting state before it runs: one past the decoder's depth limit of
// 10 000, 24 B per level (oai jsonDepth) — hand-computed here.
const gatewayNestingState = (10000 + 1) * 24

// gatewayScanNestingState is the most the chat route books for the
// sensitivity scan's nesting state before the scan runs (oai coverScan,
// hardening 7): the same 10 001 levels, 104 B per level — hand-computed.
const gatewayScanNestingState = (10000 + 1) * 104

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
		// One request at every cap: 26 × 8 MiB + 128 KiB (request share and
		// scan booking, FOLGE-P F-0) + the read term (9 MiB, Nachtrag C-1) +
		// the nesting state (240 024 B, hardening 5 B-2) + the scan's nesting
		// state (1 040 104 B, hardening 7) + max(2 × 4 MiB, 2 × 1 MiB,
		// 4 × 1048576 + 64 KiB) = 226.35 MiB; one byte less is refused.
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = gatewayCapFloor - 1 }},
		// The response share alone decides the floor here (Fremd-Review
		// finding 5): the request share with the scan would fit, 2 ×
		// response cap does not.
		{"gateway.inflight_budget", func(c *Config) {
			g(c).InflightBudget = 26*g(c).MaxRequestBytes + gatewayReadTerm(g(c).MaxRequestBytes) + 128<<10 + gatewayNestingState + gatewayScanNestingState
		}},
		{"gateway.inflight_budget", func(c *Config) {
			g(c).EmbeddingMaxFloats = 16 << 20 // 64 MiB of float32 vectors
			g(c).InflightBudget = 26*g(c).MaxRequestBytes + gatewayReadTerm(g(c).MaxRequestBytes) + 128<<10 + gatewayNestingState + gatewayScanNestingState + 2*g(c).MaxResponseBytes
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
		// A03-W10: a frame cap above the default relay buffer needs a buffer
		// that holds one frame at the cap (V39) — the legal 4 MiB cap comes
		// with a buffer of 4 MiB + the record header.
		{"frame cap at the internal 4 MiB", func(c *Config) {
			g(c).MaxStreamFrameBytes, g(c).StreamRelayBufferBytes = 4<<20, 4<<20+oai.RelayRecordHeader
		}},
		{"deadline just below the SDK timeout", func(c *Config) { g(c).RequestDeadline = 599 * time.Second }},
		{"grace at 50 s", func(c *Config) { g(c).StreamCommitGrace = 50 * time.Second }},
		{"budget exactly one request at every cap", func(c *Config) { g(c).InflightBudget = gatewayCapFloor }},
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

// TestBudgetFloorHoldsTheScanBooking is V39 after FOLGE-P hardening H-8
// (Delta-Review 2, F-0): a request below credentials books the heap of the
// sensitivity scan on top of its request share before the scan runs (oai
// coverScan: 23 × the body + 128 KiB since hardening 8), so one request at
// every cap needs 26 × gateway.max_request_bytes + the read term + 128 KiB +
// the largest response share. The
// configuration does not know the sensitivity of the aliases a budget will
// serve, so the floor always counts the scan. The review's three
// configurations passed with 0 issues while a public request at the body
// cap was answered "overloaded, retry in 1 s" on every try; the defaults
// stay valid (226.35 MiB under 256 MiB, with the nesting states of
// hardening 5 B-2 and hardening 7 and the read term of Nachtrag C-1).
func TestBudgetFloorHoldsTheScanBooking(t *testing.T) {
	const mib = 1 << 20
	def := Defaults()
	if need, ok := gatewayBudgetFloor(&def.Gateway); !ok || need != gatewayCapFloor {
		t.Errorf("default floor = %d (ok %v), want %d (26 × 8 MiB + 9 MiB + 128 KiB + 240 024 + 1 040 104 + 2 × 4 MiB)", need, ok, gatewayCapFloor)
	}
	if issues := validateGateway(def); len(issues) != 0 {
		t.Errorf("the defaults must stay valid under the floor with the scan booking, got %+v", issues)
	}
	for _, tc := range []struct{ budget, request int }{
		{256 * mib, 10 * mib}, // 280.35 MiB needed
		{128 * mib, 8 * mib},  // 226.35 MiB needed
		{64 * mib, 8 * mib},   // 226.35 MiB needed
	} {
		c := Defaults()
		c.Gateway.InflightBudget, c.Gateway.MaxRequestBytes = tc.budget, tc.request
		hits := issuesOn(Validate(c), "gateway.inflight_budget")
		if len(hits) != 1 || hits[0].Severity != SeverityError {
			t.Errorf("budget %d MiB, max_request_bytes %d MiB: a request at the body cap below credentials needs %d B — want exactly one error, got %+v",
				tc.budget/mib, tc.request/mib, 26*tc.request+gatewayReadTerm(tc.request)+128<<10+gatewayNestingState+gatewayScanNestingState+2*4*mib, hits)
		}
	}
}

// TestBudgetFloorNamesTheWaysOut (FOLGE-P hardening 3, Delta-Review 3 R3-3):
// the floor's error names both ways out with their numbers — the budget it
// needs, the largest body cap the budget holds — and that the scan's share
// is booked only below credentials. Each way out is checked to be one: the
// budget it names, and the cap it names, validate; one byte past the cap
// does not. A budget that holds no body at all names the budget alone.
func TestBudgetFloorNamesTheWaysOut(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		budget, request int
		need, cap       int64 // hand-computed: 26 × request + the read term + 128 KiB + 240 024 + 1 040 104 + 8 MiB; the largest cap c with 26 × c + c + min(c + 512, 1 MiB) within budget − 128 KiB − 240 024 − 1 040 104 − 8 MiB: (room − 1 MiB) / 27 past the bend at 1 048 064 B, (room − 512) / 28 below it
	}{
		{256 * mib, 10 * mib, 293963904, 9540261},
		{128 * mib, 8 * mib, 237340800, 4569234},
		{64 * mib, 8 * mib, 237340800, 2083721},
		{32 * mib, 8 * mib, 237340800, 848361}, // below the read term's bend
		{8 * mib, 8 * mib, 237340800, 0},       // the response share alone fills it
	} {
		c := Defaults()
		c.Gateway.InflightBudget, c.Gateway.MaxRequestBytes = tc.budget, tc.request
		hits := issuesOn(Validate(c), "gateway.inflight_budget")
		if len(hits) != 1 {
			t.Fatalf("budget %d MiB, max_request_bytes %d MiB: want one error, got %+v", tc.budget/mib, tc.request/mib, hits)
		}
		msg := hits[0].Msg
		t.Logf("FOLGEP floor error, budget %d MiB, max_request_bytes %d MiB: %s", tc.budget/mib, tc.request/mib, msg)
		want := []string{"raise gateway.inflight_budget to >= " + strconv.FormatInt(tc.need, 10), "below credentials", "one attempt without failover",
			strconv.Itoa(gatewayScanNestingState) + " (the scan's nesting state",
			strconv.Itoa(gatewayReadTerm(tc.request)) + " (the read of a body at the cap without a declared length",
			"gateway.max_request_bytes + min(gateway.max_request_bytes + 512, 1048576))"}
		if tc.cap > 0 {
			want = append(want, "lower gateway.max_request_bytes to <= "+strconv.FormatInt(tc.cap, 10))
		} else if strings.Contains(msg, "lower gateway.max_request_bytes") {
			t.Errorf("budget %d MiB: no body cap fits it, but the error names one: %s", tc.budget/mib, msg)
		}
		for _, w := range want {
			if !strings.Contains(msg, w) {
				t.Errorf("budget %d MiB, max_request_bytes %d MiB: the error lacks %q: %s", tc.budget/mib, tc.request/mib, w, msg)
			}
		}
		raised := Defaults()
		raised.Gateway.InflightBudget, raised.Gateway.MaxRequestBytes = int(tc.need), tc.request
		if hits := issuesOn(Validate(raised), "gateway.inflight_budget"); len(hits) != 0 {
			t.Errorf("the budget the error names (%d) does not validate: %+v", tc.need, hits)
		}
		if tc.cap == 0 {
			continue
		}
		for _, d := range []int64{0, 1} {
			lowered := Defaults()
			lowered.Gateway.InflightBudget, lowered.Gateway.MaxRequestBytes = tc.budget, int(tc.cap+d)
			if hits := issuesOn(Validate(lowered), "gateway.inflight_budget"); len(hits) != int(d) {
				t.Errorf("budget %d MiB, max_request_bytes %d (the named cap + %d): want %d errors, got %+v", tc.budget/mib, tc.cap+d, d, d, hits)
			}
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
