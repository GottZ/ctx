package main

// A02-W5 (design/02 §4.1d, §7 W5) — the boot seam of the conditional env
// seed. internal/backends pins the mechanism on synthetic tuples; this file
// pins the thing the wave is actually about: what the REAL registry defaults
// and the REAL env loader do to the verdict on a fresh install.
//
// Load-bearing because both halves are easy to break silently. A comparison
// base built from anything other than the registry defaults (an empty
// Config, the live snapshot itself) makes the verdict constant — in one
// direction every install keeps flooding itself with dead localhost rows, in
// the other every configured operator loses his seed without a word.

import (
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestBackendSeedSkipsUnconfiguredBoot: a boot with not a single backend env
// var set produces tuples identical to the comparison base — the seed stays a
// no-op, context_backends stays empty, and the W4 advisory (plus `ctx
// backends seed` / the init wizard) owns the fresh install.
//
// Mutation probe: point backendSeedDefaults at anything but config.Defaults()
// and this goes red.
func TestBackendSeedSkipsUnconfiguredBoot(t *testing.T) {
	resetAllEnv(t)
	cfg, _ := config.FromEnv()

	if !backendBootstrapInput(cfg).MatchesDefaults(backendSeedDefaults()) {
		t.Error("an unconfigured boot counts as configured — the fresh install would be seeded with the default localhost rows again")
	}
}

// TestBackendSeedStillFiresForConfiguredEnv is the negative half and the
// reason W5 is not W6: the env path stays fully alive for the population the
// deprecation window exists for. One moved key is enough.
func TestBackendSeedStillFiresForConfiguredEnv(t *testing.T) {
	resetAllEnv(t)
	t.Setenv("CTX_CHAT_HOST", "http://gpu.example:8089")
	cfg, _ := config.FromEnv()

	if backendBootstrapInput(cfg).MatchesDefaults(backendSeedDefaults()) {
		t.Error("a configured CTX_CHAT_HOST counts as untouched default — the env seed would die a release early")
	}
}

// TestBackendSeedDefaultsEqualEmptyEnv pins the identity the comparison base
// rests on: config.Defaults() is what the env loader produces when no source
// supplies anything. Were the two to drift apart, the verdict above would be
// measured against a config that no installation ever has.
func TestBackendSeedDefaultsEqualEmptyEnv(t *testing.T) {
	resetAllEnv(t)
	cfg, _ := config.FromEnv()

	if cfg.Chat.Host != config.Defaults().Chat.Host || cfg.Embed.Model != config.Defaults().Embed.Model {
		t.Errorf("empty-env config (%s / %s) diverges from config.Defaults() (%s / %s)",
			cfg.Chat.Host, cfg.Embed.Model, config.Defaults().Chat.Host, config.Defaults().Embed.Model)
	}
}
