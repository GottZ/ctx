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

// gatewayCapFloor is V39's floor at the default caps (RC-D C8): the largest
// need of one request over the routes and classes plus
// oai.UpstreamFrameReserve — the chat needs hand-computed (at credentials
// at the body cap of 8 MiB: 3 × 8 MiB + the read term + the JSON check's
// nesting state + 2 × 4 MiB; below credentials at the scan cap of 2 MiB:
// 26 × 2 MiB + the read term + 128 KiB + the nesting states + 2 × 4 MiB),
// the embeddings and rerank needs from oai (EmbedRequestFloor,
// RerankRequestFloor — gated against the routes' worst forms by oai's
// TestBudgetFloorHoldsTheWorstForms).
func gatewayCapFloor() int {
	const n, s, resp = 8 << 20, 2 << 20, 2 * 4 << 20
	chatCred := 3*n + gatewayReadTerm(n) + gatewayNestingState + resp
	chatPub := 26*s + gatewayReadTerm(s) + 128<<10 + gatewayNestingState + gatewayScanNestingState + resp
	return max(chatCred, chatPub,
		int(oai.EmbedRequestFloor(n, false, 1<<20)), int(oai.EmbedRequestFloor(s, true, 1<<20)),
		int(oai.RerankRequestFloor(n, false)), int(oai.RerankRequestFloor(s, true))) + oai.UpstreamFrameReserve
}

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
		// One request at every cap (RC-D C8: the largest need over the
		// routes and classes + oai.UpstreamFrameReserve); one byte less is
		// refused.
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = gatewayCapFloor() - 1 }},
		// Without the upstream frame reserve the largest need alone does
		// not hold one request whose backend row has a model name.
		{"gateway.inflight_budget", func(c *Config) { g(c).InflightBudget = gatewayCapFloor() - oai.UpstreamFrameReserve }},
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
		{"budget exactly one request at every cap", func(c *Config) { g(c).InflightBudget = gatewayCapFloor() }},
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
// and RC-D C8: a request below credentials books the heap of the
// sensitivity scan on top of its request share before the scan runs (oai
// coverScan: 23 × the body + 128 KiB + its nesting state), so the floor
// counts it — for the class below credentials, at the effective scan cap
// (a longer body is 413 before the scan). The configuration does not know
// the sensitivity of the aliases a budget will serve, so both classes
// count. The default floor is the hand-composed one (gatewayCapFloor);
// raising the scan cap to the body cap raises the floor to the classes
// below credentials at 8 MiB, and a scan cap above the body cap is the body
// cap (V58: legal, without effect).
func TestBudgetFloorHoldsTheScanBooking(t *testing.T) {
	def := Defaults()
	if need, ok := gatewayBudgetFloor(&def.Gateway); !ok || need != int64(gatewayCapFloor()) {
		t.Errorf("default floor = %d (ok %v), want %d (the largest route need + oai.UpstreamFrameReserve)", need, ok, gatewayCapFloor())
	}
	if issues := validateGateway(def); len(issues) != 0 {
		t.Errorf("the defaults must stay valid under the floor, got %+v", issues)
	}
	full, over := Defaults(), Defaults()
	full.Gateway.ScanMaxRequestBytes = full.Gateway.MaxRequestBytes
	over.Gateway.ScanMaxRequestBytes = 8 * full.Gateway.MaxRequestBytes
	nFull, _ := gatewayBudgetFloor(&full.Gateway)
	nOver, _ := gatewayBudgetFloor(&over.Gateway)
	const n = 8 << 20
	want := max(int64(26*n+gatewayReadTerm(n)+128<<10+gatewayNestingState+gatewayScanNestingState+2*4<<20),
		oai.EmbedRequestFloor(n, true, 1<<20), oai.RerankRequestFloor(n, true)) + oai.UpstreamFrameReserve
	if nFull != want || nOver != want {
		t.Errorf("floor with the scan cap at the body cap %d, above it %d; want %d (the classes below credentials at 8 MiB)", nFull, nOver, want)
	}
	if nFull <= int64(gatewayCapFloor()) {
		t.Errorf("the scan cap at the body cap must raise the floor: %d <= %d", nFull, gatewayCapFloor())
	}
	c := Defaults()
	c.Gateway.ScanMaxRequestBytes, c.Gateway.InflightBudget = c.Gateway.MaxRequestBytes, int(nFull-1)
	if hits := issuesOn(Validate(c), "gateway.inflight_budget"); len(hits) != 1 || hits[0].Severity != SeverityError {
		t.Errorf("budget one byte under the floor with the scan at the body cap: want exactly one error, got %+v", hits)
	}
}

// TestBudgetFloorNamesTheWaysOut (FOLGE-P hardening 3, Delta-Review 3 R3-3;
// RC-D C8): the floor's error names the deciding route and class, every
// route's needs, and the ways out with their numbers — the budget it needs,
// the largest body cap the budget holds and, when a class below
// credentials decides, the largest scan cap. Each way out is checked to be
// one: the budget it names validates; the cap it names validates, one byte
// past it does not. A budget that holds no body at all names no cap.
func TestBudgetFloorNamesTheWaysOut(t *testing.T) {
	const mib = 1 << 20
	floor := gatewayCapFloor()
	for _, tc := range []struct {
		name            string
		budget, scan    int
		wantCap, wantSc bool
	}{
		{"one byte under the floor", floor - 1, 2 * mib, true, false},
		{"half the floor", floor / 2, 2 * mib, true, false},
		{"64 MiB: no body cap holds the routes' fixed parts", 64 * mib, 2 * mib, false, false},
		{"scan at the body cap, one byte under its floor", 0, 8 * mib, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Gateway.ScanMaxRequestBytes = tc.scan
			need, _ := gatewayBudgetFloor(&c.Gateway)
			c.Gateway.InflightBudget = tc.budget
			if tc.budget == 0 {
				c.Gateway.InflightBudget = int(need - 1)
			}
			hits := issuesOn(Validate(c), "gateway.inflight_budget")
			if len(hits) != 1 {
				t.Fatalf("want one error, got %+v", hits)
			}
			msg := hits[0].Msg
			t.Logf("C8 floor error, budget %d, scan cap %d: %s", c.Gateway.InflightBudget, tc.scan, msg)
			for _, w := range []string{"raise gateway.inflight_budget to >= " + strconv.FormatInt(need, 10), "below credentials", "one attempt without failover",
				"oai.UpstreamFrameReserve", "chat ", "embeddings ", "rerank ", strconv.Itoa(gatewayScanNestingState) + ", its nesting state"} {
				if !strings.Contains(msg, w) {
					t.Errorf("the error lacks %q: %s", w, msg)
				}
			}
			raised := Defaults()
			raised.Gateway.ScanMaxRequestBytes, raised.Gateway.InflightBudget = tc.scan, int(need)
			if hits := issuesOn(Validate(raised), "gateway.inflight_budget"); len(hits) != 0 {
				t.Errorf("the budget the error names (%d) does not validate: %+v", need, hits)
			}
			for _, w := range []struct {
				key, prefix string
				want        bool
				set         func(c *Config, v int)
			}{
				{"gateway.max_request_bytes", "lower gateway.max_request_bytes to <= ", tc.wantCap, func(c *Config, v int) { c.Gateway.MaxRequestBytes = v }},
				{"gateway.scan_max_request_bytes", "lower gateway.scan_max_request_bytes to <= ", tc.wantSc, func(c *Config, v int) { c.Gateway.ScanMaxRequestBytes = v }},
			} {
				i := strings.Index(msg, w.prefix)
				if (i >= 0) != w.want {
					t.Errorf("way out %q named %v, want %v: %s", w.prefix, i >= 0, w.want, msg)
					continue
				}
				if i < 0 {
					continue
				}
				rest := msg[i+len(w.prefix):]
				v, err := strconv.Atoi(rest[:strings.IndexFunc(rest, func(r rune) bool { return r < '0' || r > '9' })])
				if err != nil {
					t.Fatalf("way out %q: no number in %q", w.prefix, rest)
				}
				for _, d := range []int{0, 1} {
					l := Defaults()
					l.Gateway.ScanMaxRequestBytes, l.Gateway.InflightBudget = tc.scan, c.Gateway.InflightBudget
					w.set(l, v+d)
					if hits := issuesOn(Validate(l), "gateway.inflight_budget"); len(hits) != d {
						t.Errorf("%s = %d (the named value + %d): want %d errors, got %+v", w.key, v+d, d, d, hits)
					}
				}
			}
		})
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
