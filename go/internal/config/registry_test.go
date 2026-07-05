package config

import (
	"reflect"
	"strings"
	"testing"
)

// TestRegistryCoversEveryField is the registry-completeness gate: every
// exported leaf field of Config is reachable through exactly one registry
// entry — no config field can exist outside the F2 settings contract. The
// walker here counts leaves independently of buildRegistry.
func TestRegistryCoversEveryField(t *testing.T) {
	covered := map[string]bool{}
	for _, e := range registry() {
		covered[strings.Join(fieldPath(reflect.TypeOf(Config{}), e.path), ".")] = true
	}

	var leaves []string
	var walk func(rt reflect.Type, prefix string)
	walk = func(rt reflect.Type, prefix string) {
		for i := 0; i < rt.NumField(); i++ {
			f := rt.Field(i)
			if !f.IsExported() {
				continue
			}
			name := f.Name
			if prefix != "" {
				name = prefix + "." + name
			}
			if _, tagged := f.Tag.Lookup("key"); !tagged && f.Type.Kind() == reflect.Struct {
				walk(f.Type, name)
				continue
			}
			leaves = append(leaves, name)
		}
	}
	walk(reflect.TypeOf(Config{}), "")

	if len(leaves) != len(registry()) {
		t.Errorf("registry has %d entries, struct has %d leaf fields", len(registry()), len(leaves))
	}
	for _, leaf := range leaves {
		if !covered[leaf] {
			t.Errorf("field %s has no registry entry", leaf)
		}
	}
}

func fieldPath(rt reflect.Type, path []int) []string {
	var names []string
	for _, i := range path {
		f := rt.Field(i)
		names = append(names, f.Name)
		rt = f.Type
	}
	return names
}

// TestRegistryStrictSet pins the parse:"strict" classification to EXACTLY
// the pre-F1 fatal-parse paths (§3.3: 7 getEnvInt vars + timezone; the
// eighth, CTX_EMBED_DIMS, retired with the cmd/ctxd bridge in wave 7 —
// Delta 4, pinned by TestEmbedDimsRetired). A field joining or leaving this
// set changes boot semantics and must fail here first.
func TestRegistryStrictSet(t *testing.T) {
	want := map[string]bool{
		"server.db_port":         true,
		"embed.num_ctx":          true,
		"chat.num_ctx":           true,
		"dream.num_ctx":          true,
		"query.rate_limit_write": true,
		"query.rate_limit_read":  true,
		"query.timezone":         true,

		"scheduler.llmlog_retention_days": true,
		// F4-W6 (G33) read cap: a malformed limit is an operator typo worth
		// surfacing loudly, same as its telemetry sibling above.
		"llmlog.max_limit": true,
		// F4-W7 (G34) SSE connection cap: an int ceiling like llmlog.max_limit,
		// so it shares the loud-abort treatment — a typo'd cap that silently
		// falls back to the default would hide the intended ceiling. The events.*
		// DURATIONS (tick/queue_stats/ping) stay non-strict (bad value falls
		// back to default; a stream cadence is not a security ceiling).
		"events.max_connections": true,
		// F6-C4 (G37) per-scope turn semaphore: a per-tenant fairness ceiling
		// like events.max_connections — a typo'd cap silently falling back to
		// the default would hide the intended limit on the single llama.cpp
		// slot (R1). The other webchat.* budgets stay non-strict (engine
		// withDefaults() is their net).
		"webchat.concurrent_turns": true,
		// W11 (design/03 §4.4) project sync ceilings: per-project starts/h and the
		// process-global concurrency cap — int ceilings like events.max_connections,
		// a typo'd cap silently falling back to the default would hide the intended
		// GitHub-token protection, so both abort loudly.
		"project.sync.rate_limit":     true,
		"project.sync.max_concurrent": true,
		// W9 (design/03 §4.5/§6.2) project SSE hub caps: per-tenant connection
		// ceiling + per-project-per-tick coalesce threshold — int ceilings like
		// events.max_connections; a typo'd cap silently defaulting would hide the
		// intended fan-out limit, so both abort loudly. The DURATIONS
		// (flush_interval, ping_interval) stay non-strict (a stream cadence is not
		// a security ceiling).
		"project.events.max_connections":    true,
		"project.events.coalesce_threshold": true,
		// B-W1 overview liveness cap: the load-bearing guard against the
		// Louvain convergence wall — an int ceiling like the caps above; a
		// typo'd cap silently falling back to the default would hide the
		// intended liveness bound, so it aborts loudly. rebuild_timeout (a
		// DURATION) stays non-strict like the other cadences.
		"graph_overview.max_nodes": true,
		// W13 (design/03 §3.4/§4.4) webhook inbound per-project rate ceiling — an
		// int cap like events.max_connections; a typo'd cap silently defaulting
		// would hide the intended Denial-of-Sync protection, so it aborts loudly.
		// webhook.retention is a Duration (Hours), non-strict (a janitor horizon is
		// not a security ceiling).
		"webhook.rate_limit": true,
	}
	got := map[string]bool{}
	for _, e := range registry() {
		if e.Strict {
			got[e.Key] = true
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("strict set drifted:\ngot  %v\nwant %v", got, want)
	}
}

// TestRegistrySecretSet pins the masking classes: machine-generated keys are
// "fp" (fingerprint-eligible in the boot dump), the human-chosen db password
// is "presence" (never fingerprinted — offline dictionary oracle).
func TestRegistrySecretSet(t *testing.T) {
	want := map[string]string{
		"server.db_password":    "presence",
		"chat.api_key":          "fp",
		"chat_fallback.api_key": "fp",
		"embed.api_key":         "fp",
		"dream.api_key":         "fp",
		"dream_embed.api_key":   "fp",
		"rerank.api_key":        "fp",
	}
	got := map[string]string{}
	for _, e := range registry() {
		if e.Secret != "" {
			got[e.Key] = e.Secret
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("secret set drifted:\ngot  %v\nwant %v", got, want)
	}
}

// TestRegistrySupersededSet pins the lifetime markers: the role-tuple keys
// are replaced by F3 context_backends rows (bootstrap reads the EFFECTIVE
// snapshot, not raw env — Auflage X1).
func TestRegistrySupersededSet(t *testing.T) {
	for _, e := range registry() {
		group, _, _ := strings.Cut(e.Key, ".")
		isRoleTuple := false
		switch group {
		case "chat", "chat_fallback", "embed", "dream_embed":
			isRoleTuple = true
		case "dream":
			switch e.Key {
			case "dream.host", "dream.api_key", "dream.protocol", "dream.model",
				"dream.num_ctx", "dream.think":
				isRoleTuple = true
			}
		case "rerank":
			switch e.Key {
			case "rerank.host", "rerank.api_key", "rerank.model":
				isRoleTuple = true
			}
		}
		want := ""
		if isRoleTuple {
			want = "f3:context_backends"
		}
		if e.Superseded != want {
			t.Errorf("%s: superseded = %q, want %q", e.Key, e.Superseded, want)
		}
	}
}

// TestRegistryEnvNamespace pins the env-var naming: every env-sourced key
// maps to a CTX_*/CONTEXT_*/LISTEN_ADDR var and the mapping is unique
// (buildRegistry rejects duplicates; this guards the convention).
func TestRegistryEnvNamespace(t *testing.T) {
	// Settings-only keys (env:"-"): born in F2's context_settings, never
	// migrated from env vars. scheduler.home_scope (F1) + the F3-P3 trust-
	// gating policy surface + the F3-P6 gaming toggle (persistent by design).
	settingsOnly := map[string]bool{
		"scheduler.home_scope":                  true,
		"pool.default_query_sensitivity":        true,
		"pool.default_block_sensitivity":        true,
		"pool.scope_sensitivity_floor":          true,
		"gaming.active":                         true,
		"gaming.disabled_backends":              true,
		"tenant.allow_shared_secrets":           true, // MT3-W5: operator-set per-tenant opt-in flag (global-only)
		"tenant.allow_cross_tenant_block_grant": true, // MT T43 (07-W6): operator-set per-tenant cross-tenant block-grant opt-in (global-only)
	}
	seen := map[string]string{}
	for _, e := range registry() {
		if e.EnvVar == "-" {
			if !settingsOnly[e.Key] {
				t.Errorf("unexpected env-less field %s (settings-only keys are pinned here)", e.Key)
			}
			continue
		}
		if !strings.HasPrefix(e.EnvVar, "CTX_") && !strings.HasPrefix(e.EnvVar, "CONTEXT_") &&
			e.EnvVar != "LISTEN_ADDR" {
			t.Errorf("%s: env var %q outside the CTX_/CONTEXT_ namespace", e.Key, e.EnvVar)
		}
		if prev, dup := seen[e.EnvVar]; dup {
			t.Errorf("env var %s mapped twice: %s and %s", e.EnvVar, prev, e.Key)
		}
		seen[e.EnvVar] = e.Key
	}
	if len(EnvVars()) != len(seen) {
		t.Errorf("EnvVars() returned %d vars, registry has %d", len(EnvVars()), len(seen))
	}
}

// TestRegistryTenancySet pins the tenant-overridable allowlist (MT3-W2): EXACTLY
// these keys may a tenant override on top of _global; every other registry key
// is global-only. This is the security contract — a key silently joining the
// tenant-writable surface (or one of the five NAMED global-only keys silently
// leaving it) fails here first. Classification rationale lives in config.go's
// tag-doc block; §3.3 lists a representative subset, this is the normative set.
func TestRegistryTenancySet(t *testing.T) {
	overridable := map[string]bool{
		// the 6 provider api_key secret_refs (TENANT-DECISION: per-tenant creds)
		"chat.api_key": true, "chat_fallback.api_key": true, "embed.api_key": true,
		"dream.api_key": true, "dream_embed.api_key": true, "rerank.api_key": true,
		// the re-dream back-off curve (atomic per-tenant unit)
		"dream.backoff_mode": true, "dream.backoff_factor": true, "dream.backoff_grace": true,
		"dream.backoff_cap": true, "dream.backoff_min": true, "dream.backoff_inert_offset": true,
		// rerank query-time tuning (host/model stay global — F3 pool topology)
		"rerank.enabled": true, "rerank.max_docs": true, "rerank.blend_weight": true,
		// graph query-time expansion tuning (all knobs)
		"graph.enabled": true, "graph.directed": true, "graph.hop_depth": true,
		"graph.seed_count": true, "graph.seed_score_floor": true, "graph.per_seed_cap": true,
		"graph.max_injected": true, "graph.min_confidence": true, "graph.min_confidence_recurrent": true,
		"graph.boost_weight": true, "graph.hub_damping": true, "graph.weight_topical": true,
		"graph.weight_factual": true, "graph.weight_causal": true, "graph.weight_recurrent": true,
		"graph.new_placement_frac": true,
		// query-path tuning
		"query.score_threshold": true, "query.confident_threshold": true, "query.prompt_version": true,
		"query.timezone": true, "query.rate_limit_write": true, "query.rate_limit_read": true,
		// per-tenant scope resolution (consumer MUST intersect with entitlements — T38)
		"scheduler.read_scopes": true, "scheduler.home_scope": true,
		// per-tenant trust policy
		"pool.default_query_sensitivity": true, "pool.default_block_sensitivity": true,
		"pool.scope_sensitivity_floor": true,
		// per-tenant SSE cap
		"events.max_connections": true,
		// W11 per-project sync rate (max_concurrent is a PROCESS-global semaphore →
		// global-only, so it is deliberately absent here)
		"project.sync.rate_limit": true,
		// W9 per-tenant SSE domain-event connection cap (flush/ping cadences +
		// coalesce_threshold are process-global → deliberately absent here)
		"project.events.max_connections": true,
		// W13 per-project webhook inbound rate (retention is a process-global
		// janitor horizon → global-only, deliberately absent here)
		"webhook.rate_limit": true,
		// per-tenant web-chat surface
		"webchat.enabled": true, "webchat.max_iterations": true, "webchat.max_tokens": true,
		"webchat.completion_budget": true, "webchat.tool_result_max_chars": true,
		"webchat.history_budget_chars": true, "webchat.llm_timeout": true,
		"webchat.concurrent_turns": true, "webchat.session_retention": true,
	}
	for _, e := range registry() {
		gotOverridable := e.Tenancy == TenancyOverridable
		if gotOverridable != overridable[e.Key] {
			t.Errorf("%s: tenancy %q (overridable=%v), want overridable=%v",
				e.Key, e.Tenancy, gotOverridable, overridable[e.Key])
		}
		if !gotOverridable && e.Tenancy != TenancyGlobalOnly {
			t.Errorf("%s: non-overridable key must be %q, got %q", e.Key, TenancyGlobalOnly, e.Tenancy)
		}
	}
	if got := len(overridable); got != 55 {
		t.Errorf("tenant-overridable allowlist has %d keys, expected 55 (change it with intent)", got)
	}
	// The five NAMED global-only keys (design 03 §3.3) — the R-SCALE6 invariant:
	// a tenant override here would flush the process-wide embed cache / flip the
	// server GPU switch. Also exercises IsGlobalOnly incl. its fail-closed path.
	for _, k := range []string{"gaming.active", "embed.host", "embed.protocol",
		"dream_embed.host", "dream_embed.protocol", "nonexistent.key"} {
		if !IsGlobalOnly(k) {
			t.Errorf("%s must be global-only", k)
		}
	}
}

// TestBuildRegistryRejectsMalformedStructs proves a broken tag is a
// programmer error caught at first use, not a silently skipped field.
func TestBuildRegistryRejectsMalformedStructs(t *testing.T) {
	type untaggedLeaf struct {
		X string
	}
	type missingDefault struct {
		X string `key:"a.x" env:"A_X" mut:"hot"`
	}
	type badMut struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"warm"`
	}
	type missingTenancy struct {
		// every mandatory tag present EXCEPT tenancy — proves the tenancy
		// classification is enforced at boot, so no key escapes it (MT3-W2).
		X string `key:"a.x" env:"A_X" default:"" mut:"hot"`
	}
	type badTenancy struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"hot" tenancy:"sometimes"`
	}
	type badDefault struct {
		X int `key:"a.x" env:"A_X" default:"abc" mut:"hot" tenancy:"global-only"`
	}
	type dupKey struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"hot" tenancy:"global-only"`
		Y string `key:"a.x" env:"A_Y" default:"" mut:"hot" tenancy:"global-only"`
	}
	type dupEnv struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"hot" tenancy:"global-only"`
		Y string `key:"a.y" env:"A_X" default:"" mut:"hot" tenancy:"global-only"`
	}
	type unsupportedType struct {
		X map[string]string `key:"a.x" env:"A_X" default:"" mut:"hot" tenancy:"global-only"`
	}
	for name, rt := range map[string]reflect.Type{
		"untagged leaf":    reflect.TypeOf(untaggedLeaf{}),
		"missing default":  reflect.TypeOf(missingDefault{}),
		"bad mut":          reflect.TypeOf(badMut{}),
		"missing tenancy":  reflect.TypeOf(missingTenancy{}),
		"bad tenancy":      reflect.TypeOf(badTenancy{}),
		"bad default":      reflect.TypeOf(badDefault{}),
		"duplicate key":    reflect.TypeOf(dupKey{}),
		"duplicate env":    reflect.TypeOf(dupEnv{}),
		"unsupported type": reflect.TypeOf(unsupportedType{}),
	} {
		if _, err := buildRegistry(rt); err == nil {
			t.Errorf("%s: buildRegistry must reject, got nil error", name)
		}
	}
}
