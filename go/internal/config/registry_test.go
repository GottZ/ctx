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
		"scheduler.home_scope":           true,
		"pool.default_query_sensitivity": true,
		"pool.default_block_sensitivity": true,
		"pool.scope_sensitivity_floor":   true,
		"gaming.active":                  true,
		"gaming.disabled_backends":       true,
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
	type badDefault struct {
		X int `key:"a.x" env:"A_X" default:"abc" mut:"hot"`
	}
	type dupKey struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"hot"`
		Y string `key:"a.x" env:"A_Y" default:"" mut:"hot"`
	}
	type dupEnv struct {
		X string `key:"a.x" env:"A_X" default:"" mut:"hot"`
		Y string `key:"a.y" env:"A_X" default:"" mut:"hot"`
	}
	type unsupportedType struct {
		X map[string]string `key:"a.x" env:"A_X" default:"" mut:"hot"`
	}
	for name, rt := range map[string]reflect.Type{
		"untagged leaf":    reflect.TypeOf(untaggedLeaf{}),
		"missing default":  reflect.TypeOf(missingDefault{}),
		"bad mut":          reflect.TypeOf(badMut{}),
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
