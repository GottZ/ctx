package config

import (
	"fmt"
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
		// S0 telemetry SSE hub: the per-tick coalesce threshold of /api/events,
		// mirroring its project.events sibling one line up — same doctrine, same
		// loud abort on a typo'd degradation point.
		"events.llmcall_coalesce_threshold": true,
		// B-W1 overview liveness cap: the load-bearing guard against the
		// Louvain convergence wall — an int ceiling like the caps above; a
		// typo'd cap silently falling back to the default would hide the
		// intended liveness bound, so it aborts loudly. rebuild_timeout (a
		// DURATION) stays non-strict like the other cadences.
		"graph_overview.max_nodes": true,
		// S6+S7 (Achse 04): the ctx-engine emergency stop, same class as
		// max_nodes one line up — the ENGINE picks which of the two keys
		// applies, so a typo'd cap silently defaulting would hide the intended
		// bound for exactly the engine that is supposed to scale.
		"graph_overview.max_nodes_ctx": true,
		// S6+S7: the engine name is strict for a different reason than the caps
		// — not a ceiling but an IDENTITY. An unknown value must abort at boot
		// rather than fall back to gonum: switching the engine is a one-time
		// global partition break, and someone who writes engine=leiden and gets
		// a gonum partition would have no way to notice.
		"graph_overview.engine": true,
		// W13 (design/03 §3.4/§4.4) webhook inbound per-project rate ceiling — an
		// int cap like events.max_connections; a typo'd cap silently defaulting
		// would hide the intended Denial-of-Sync protection, so it aborts loudly.
		// webhook.retention is a Duration (Hours), non-strict (a janitor horizon is
		// not a security ceiling).
		"webhook.rate_limit": true,
		// Vorhaben E MW1 (design/01 §3.1 + design/03 §3.1): dispatcher
		// capacity caps and reaper clocks are fairness/security ceilings on
		// the process-shared inference targets — a typo'd value silently
		// falling back to the default would hide the intended ceiling (same
		// rationale as webchat.concurrent_turns). dispatch.enabled is the
		// deliberate exception: the exact-match bool parser cannot malform,
		// so a strict tag would be an unreachable classification here.
		"dispatch.background_queue_max":            true,
		"dispatch.interactive_queue_per_principal": true,
		"dispatch.interactive_queue_per_tenant":    true,
		"dispatch.interactive_queue_max":           true,
		"dispatch.lease_reap_grace":                true,
		"dispatch.lease_max_age":                   true,
		// MW18: the preempt watchdog fence is a security clock like the reaper
		// clocks above — a typo'd value silently defaulting would move the
		// force-release fence unnoticed, so it aborts loudly.
		"dispatch.preempt_release_timeout": true,
		// MW25: the aging escape is a deliberate, capped weakening of the
		// herald term (design/04 §4.6) — a typo'd value silently defaulting
		// to 0 would hide an intended activation, a malformed one must never
		// guess a weakening window, so it aborts loudly.
		"dispatch.background_aging_after": true,
		// B2: the blob write budget is a CEILING like its rate-limit siblings —
		// a typo'd value silently defaulting to 10 would hide the intended cap
		// on the one write surface that costs disk per request.
		"pool.blob_rate_limit_write": true,
		// H12: the context-window fallback is the ONE key that decides whether a
		// prompt carrying foreign text may be built for a chain member without a
		// declared window. A typo'd value silently defaulting to 0 would flip the
		// pass from "operator declared a floor" to "refuse" (or, read the other
		// way, hide that no floor was ever declared) — it aborts loudly.
		"pool.external_num_ctx_fallback": true,
		// E10-W2: the discovery TTL decides whether AUTO windows exist at all
		// (0 = off → back to the H12 refusal path). A typo'd value silently
		// defaulting to 3600 would present as "discovery running" while the
		// operator meant to turn it off — it aborts loudly.
		"pool.openrouter_window_ttl": true,
		// W-D: the two SIZE ceilings of the root map. budget_bytes bounds a
		// PERSISTED artefact against the 50 KB public write cap, super_max_nodes
		// bounds the γ-search of the meta level — a typo'd value silently
		// defaulting would widen a ceiling nobody asked to widen, on an object
		// that then travels through backups and exports.
		"root_map.budget_bytes":    true,
		"root_map.super_max_nodes": true,
		// W6 (Cluster-Topic-Map): the three label-pipeline integer bounds are
		// declared RESOURCE ceilings — the per-tick LLM-call cap, the corpus
		// complexity threshold that decides whether any labelling happens at
		// all, and the prompt token budget. A typo'd value silently falling
		// back to its default would hide exactly the limit the operator meant
		// to set, on the one arm of this axis that spends inference.
		// graph_overview.label_enabled / label_credentials_fallback_only stay
		// non-strict for the documented bool reason (dispatch.enabled).
		"graph_overview.label_batch":             true,
		"graph_overview.label_min_topics":        true,
		"graph_overview.label_prompt_max_titles": true,
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
		"server.db_password": "presence",
		"chat.api_key":       "fp",
		"embed.api_key":      "fp",
		"dream.api_key":      "fp",
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
// are replaced by F3 context_backends rows (since β1 the table is the only
// topology source — no boot-time seed reads these keys any more).
//
// The rerank group left this switch in β3: its tuple keys are out of the
// registry entirely, so the loop now asserts the OTHER half of the statement
// for what is left of the group — rerank.enabled/max_docs/blend_weight are
// live, unmarked keys. chat_fallback left it in β4 and dream_embed in β5, both
// leaving nothing behind: all of their keys were tuple keys, so neither group
// has a surviving half to assert anything about. Keeping the case labels would
// pin branches no registry entry can reach any more. That the cut names stay
// out of the registry is retired_test.go's ratchet, not this pin.
func TestRegistrySupersededSet(t *testing.T) {
	for _, e := range registry() {
		group, _, _ := strings.Cut(e.Key, ".")
		isRoleTuple := false
		switch group {
		case "chat", "embed":
			isRoleTuple = true
		case "dream":
			switch e.Key {
			case "dream.host", "dream.api_key", "dream.protocol", "dream.model",
				"dream.num_ctx", "dream.think":
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
		"pool.llm_audit_min_sensitivity":        true, // D4 §4.5-c: G41 audit verdict floor, born in settings like its pool siblings
		"pool.blob_rate_limit_write":            true, // B2: blob write budget, born in settings like its pool siblings
		"pool.external_num_ctx_fallback":        true, // H12: prompt-budget window floor, born in settings like its pool siblings
		"pool.openrouter_window_ttl":            true, // E10-W2: AUTO-window discovery TTL, born in settings like its pool siblings
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
		// the provider api_key secret_refs (TENANT-DECISION: per-tenant creds);
		// rerank.api_key left the allowlist with the β3 tuple cut,
		// chat_fallback.api_key with the β4 one, dream_embed.api_key with the β5 one
		"chat.api_key": true, "embed.api_key": true, "dream.api_key": true,
		// the re-dream back-off curve (atomic per-tenant unit)
		"dream.backoff_mode": true, "dream.backoff_factor": true, "dream.backoff_grace": true,
		"dream.backoff_cap": true, "dream.backoff_min": true, "dream.backoff_inert_offset": true,
		// rerank query-time tuning — since β3 the whole surviving group (the
		// topology half went with the tuple cut)
		"rerank.enabled": true, "rerank.max_docs": true, "rerank.blend_weight": true,
		// graph query-time expansion tuning (all knobs)
		"graph.enabled": true, "graph.directed": true, "graph.hop_depth": true,
		"graph.seed_count": true, "graph.seed_score_floor": true, "graph.per_seed_cap": true,
		"graph.max_injected": true, "graph.min_confidence": true, "graph.min_confidence_recurrent": true,
		"graph.boost_weight": true, "graph.hub_damping": true, "graph.weight_topical": true,
		"graph.weight_factual": true, "graph.weight_causal": true, "graph.weight_recurrent": true,
		"graph.new_placement_frac": true,
		// C0: cluster-consumption RANKING knobs — same class as the graph.*
		// expansion knobs above (query-time augmentation of the tenant's OWN
		// queries). The cluster.* WIRE and OPS knobs (ego_annotate*, facet/
		// route_enabled, max_staleness, centroid_build/timeout/batch/work_mem/
		// ann_threshold/ef_search) are deliberately ABSENT: a per-tenant
		// response shape is a shape no client can rely on, and the centroid
		// build is one process-wide job over a SHARED artefact.
		"cluster.enabled": true, "cluster.seed_count": true, "cluster.top_clusters": true,
		"cluster.min_share": true, "cluster.boost_weight": true, "cluster.size_damping": true,
		"cluster.centroid_enabled": true, "cluster.centroid_weight": true,
		"cluster.centroid_top_k": true, "cluster.inject_max": true,
		// query-path tuning
		"query.score_threshold": true, "query.confident_threshold": true, "query.prompt_version": true,
		"query.timezone": true, "query.rate_limit_write": true, "query.rate_limit_read": true,
		// per-tenant scope resolution (consumer MUST intersect with entitlements — T38)
		"scheduler.read_scopes": true, "scheduler.home_scope": true,
		// per-tenant trust policy
		"pool.default_query_sensitivity": true, "pool.default_block_sensitivity": true,
		"pool.scope_sensitivity_floor": true,
		// D4 §4.5-c G41 audit verdict floor — same class as the scope floor
		// above: it can only make the OWN tenant stricter (MaxSensitivity is
		// monotone), and the audit reads it from the per-tenant snapshot.
		"pool.llm_audit_min_sensitivity": true,
		// B2 per-tenant blob write budget — same class as the block budget it
		// falls back to (query.rate_limit_write, above)
		"pool.blob_rate_limit_write": true,
		// H12 per-tenant prompt-budget window floor — it only ever bounds the
		// tenant's OWN prompts, so a tenant on a stricter provider tier may
		// declare its own
		"pool.external_num_ctx_fallback": true,
		// E10-W2 per-tenant AUTO-window discovery TTL — it only changes how
		// often the tenant's OWN requests re-read a public metadata route
		"pool.openrouter_window_ttl": true,
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
	if got := len(overridable); got != 66 {
		t.Errorf("tenant-overridable allowlist has %d keys, expected 66 (change it with intent)", got)
	}
	// The NAMED global-only keys (design 03 §3.3) — the R-SCALE6 invariant: a
	// tenant override here would flush the process-wide embed cache / flip the
	// server GPU switch. Two of the five were dream_embed.host/protocol; they
	// left the registry in β5, and an unregistered name says nothing about this
	// invariant any more — IsGlobalOnly answers true for every unknown key, so
	// keeping them would have turned two real assertions into a second copy of
	// the fail-closed case below. Their absence is retired_test.go's ratchet.
	for _, k := range []string{"gaming.active", "embed.host", "embed.protocol",
		"nonexistent.key"} {
		if !IsGlobalOnly(k) {
			t.Errorf("%s must be global-only", k)
		}
	}
	// W03-3 (Evokoa-Clean-Room design/03 §4.4/§4.5): contract.mode joins the
	// N12-class kill-switch keys — a tenant override would let a
	// non-server-admin actor silence the process-wide schema-integrity
	// check. contract.recheck_interval has no such threat model on its own
	// (it is a cadence, not a switch) but the check itself is process-wide
	// (one shared schema, no per-tenant dimension), so both stay
	// global-only. Named here explicitly (not just covered by the
	// fail-closed default) per the wave brief: a deliberate, visible
	// registry extension, not a silent gate-loosening.
	for _, k := range []string{"contract.mode", "contract.recheck_interval"} {
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

// TestLLMCallCoalesceThresholdMirrorsProjectHub pins the S0 telemetry-coalescing
// knob to the SAME registry contract its ProjectHub sibling carries: ONE
// coalescing doctrine, two SSE hubs. A drift here would let /api/events and
// /api/project/events degrade their bursts by different rules.
func TestLLMCallCoalesceThresholdMirrorsProjectHub(t *testing.T) {
	sibling, ok := KeyByName("project.events.coalesce_threshold")
	if !ok {
		t.Fatal("project.events.coalesce_threshold missing — the coalescing doctrine has no reference")
	}
	got, ok := KeyByName("events.llmcall_coalesce_threshold")
	if !ok {
		t.Fatal("events.llmcall_coalesce_threshold is not registered")
	}
	if got.Type != sibling.Type || got.Mutability != sibling.Mutability || got.Tenancy != sibling.Tenancy {
		t.Errorf("contract drift: got type=%s mut=%s tenancy=%s, want %s/%s/%s (like %s)",
			got.Type, got.Mutability, got.Tenancy,
			sibling.Type, sibling.Mutability, sibling.Tenancy, sibling.Key)
	}
	if fmt.Sprint(got.Default) != "20" {
		t.Errorf("default = %v, want 20", got.Default)
	}
	if got.EnvVar != "CTX_EVENTS_LLMCALL_COALESCE_THRESHOLD" {
		t.Errorf("env var = %q, want CTX_EVENTS_LLMCALL_COALESCE_THRESHOLD", got.EnvVar)
	}
}
