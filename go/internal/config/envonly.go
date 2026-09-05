package config

import "sort"

// envOnlyServerNames maps every SERVER-runtime environment variable that
// deliberately bypasses the settings registry onto the reason it may not be a
// DB row.
//
// The class is older than this file, but it was written down for three of
// seventeen names, in two files (sealbox.go:39-42, settings/reload.go:59-61),
// and nowhere else: fourteen names sat outside the registry with nothing
// saying why, and a fifteenth could have joined them tomorrow without
// anything going red.
// envonly_test.go turns the class into a gate — every env-name-shaped string
// literal in the ctxd package closure has to be in EnvVars(), in
// RetiredEnvNames() or in this map, reason included.
//
// The map is DATA, never an API surface. It NAMES the constants, it does not
// own them: EnvEnabled stays in internal/camo, EnvKey in internal/sealbox,
// EnvTrustedProxy in internal/handler, and no call site reads its value
// through here. A name that moves into the registry leaves this map in the
// same commit that moves it — the test that pins the remainder makes a stale
// entry red on its own (a name listed here that no longer appears anywhere
// else in the closure fails the set comparison).
//
// Two kinds of reason recur, and both are decisions rather than oversights
// (DECISIONS.md E05-D3 = B, E05-D4 = B):
//
//   - the switch cannot live in the thing it switches — the key that unseals
//     the rows, the kill switch over the override layer, the flags and
//     thresholds that guard the very API a settings row is written through;
//   - the value is a deployment identity or a boot-only override in a format
//     the registry does not speak — the canonical issuer, the redirect
//     allowlist, the Go durations, the e2e bootstrap credential.
//
// What is NOT in here: names of client and tooling binaries (CTX_BASE_URL,
// CTX_KEY, CTX_GOLDSET_DIR, …). They are not server runtime, and the scan set
// is defined so that they cannot reach this file by accident — see
// envscan.go and serverRuntimePackages in envonly_test.go.
var envOnlyServerNames = map[string]string{
	// sealbox — the master key and its rotation slot.
	"CTX_SECRETS_KEY": "the key that unseals DB rows can never itself live in the DB " +
		"(sealbox.go:39-42, the sentence this class is named after)",
	"CTX_SECRETS_KEY_PREV": "rotation slot of the master key — a key stored in the rows it " +
		"decrypts is not a key (sealbox.go:39-42); internal/camo reuses both names rather " +
		"than minting a secret of its own (camo.go:47-48)",

	// settings — the kill switch over the override layer itself.
	"CTX_SETTINGS_DISABLE": "the switch that turns off DB overrides cannot itself be a DB " +
		"override (settings/reload.go:59-61)",

	// camo — signing image proxy, read once at boot (E05-D3 = B).
	"CTX_CAMO_ENABLED": "fail-closed feature flag, read once at boot into an immutable " +
		"Service (camo.go:88, cmd/ctxd/server.go:195); a registry row would advertise a " +
		"hot reload the code does not perform, on the one switch that separates a signing " +
		"proxy from an open one",
	"CTX_CAMO_TTL": "Go duration syntax (\"24h\", docs/api.md) — the registry parses no " +
		"duration format, so registering the name would silently change its value contract " +
		"(design/05 §4.7); read once at boot with the flag",
	"CTX_CAMO_MAX_BYTES": "upstream size cap read once at boot with the flag; it bounds what " +
		"an unauthenticated GET /api/img/<sig> may pull from a third-party origin, which is " +
		"a deployment limit and not a row an API request can rewrite",

	// oauth — issuer, redirect surface, token lifetimes, DCR mode (E05-D4 = B).
	"CTX_CANONICAL_ISSUER": "deployment identity, not a parameter: it is the RFC 9207 `iss` " +
		"value clients verify against, folded into the handler at boot (oauth.go:32-37). An " +
		"issuer that a settings write can rewrite makes the mix-up protection it exists for " +
		"illusory",
	"CTX_OAUTH_REDIRECT_EXTRA": "extends the redirect allowlist (oauth.go:54-60) — the list " +
		"that keeps an authorization code from leaving the deployment. Reachable through the " +
		"API it protects, it would be its own escalation path",
	"CTX_OAUTH_ACCESS_TTL": "Go duration override of a decided value (oauth.go:123-129); the " +
		"registry parses no duration format, and the lifetime is server-global while every " +
		"registry key carries a tenancy",
	"CTX_OAUTH_REFRESH_TTL": "Go duration override of a decided value (oauth.go:123-129), " +
		"server-global like the access TTL — same two reasons",
	"CTX_OAUTH_FAMILY_CAP": "Go duration override of a decided value (oauth.go:123-129): the " +
		"absolute bound on how long rolling rotation keeps one authorization alive — " +
		"server-global, and not a tenant-scoped knob",
	"CTX_OAUTH_DCR_MODE": "fail-closed open|admin|off switch over dynamic client registration " +
		"(oauth.go:175-181); it decides whether unknown clients may register at all, so it " +
		"cannot be reachable through the API it gates",
	"CTX_OAUTH_MAX_CLIENTS": "resource backstop against table DoS in the open DCR mode " +
		"(oauth_register_guard.go:1-5, :29-31), read per request with a fail-closed default — " +
		"a threshold reachable through the surface it protects is not a backstop",
	"CTX_OAUTH_REGISTER_RATE": "per-IP /register budget of the same guard " +
		"(oauth_register_guard.go:32-34), same class as the client cap: the limit that " +
		"survives an attack cannot be writable by it",
	"CTX_TRUSTED_PROXY": "names the ONE proxy hop whose X-Forwarded-For appendix is trusted " +
		"(oauth_register_guard.go:35-37, :54-62) — a statement about the deployment topology " +
		"that only the operator knows, and the exact knob needed to forge the per-IP counting",

	// bootstrap — the fail-closed first-key path of the e2e stack.
	"CTX_BOOTSTRAP_ADMIN_KEY": "plaintext first-key credential of the e2e compose stack " +
		"(cmd/ctxd/main.go:278-289); it exists only while context_api_keys is empty and is " +
		"never injected into a real deployment — a DB row would contradict both halves",
	"CTX_BOOTSTRAP_RUN_ID": "label suffix of that same bootstrap run (cmd/ctxd/main.go:298); " +
		"it lives and dies with CTX_BOOTSTRAP_ADMIN_KEY and means nothing outside that boot",
}

// EnvOnlyServerNames returns the declared env-only server env names, sorted.
// Sorted rather than grouped like the map above because a test failure that
// lists names in a stable order is diffable — the same reasoning
// RetiredEnvNames() gives for its own order (retired.go:66-67).
func EnvOnlyServerNames() []string {
	out := make([]string, 0, len(envOnlyServerNames))
	for name := range envOnlyServerNames {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
