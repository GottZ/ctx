package main

import (
	"context"
	"log/slog"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/pgxdb"
	"github.com/GottZ/ctx/internal/store"
)

// bootInstallDefaults hands the installation's defaults (migration 192, issue
// #492) to the config loader and returns the env layer the boot continues
// with. The env layer cc was parsed before the pool existed, so it carries the
// built-in default of dream.enabled; when the database says it was installed
// with Dream on and the environment left CTX_DREAM_ENABLED unset, the layer is
// parsed again — every later generation parses it the same way, because
// config.FromEnv reads the default this sets.
//
// Never fatal: a failed read keeps the built-in default (Dream off), the
// direction that orders no background chat calls, and says so.
func bootInstallDefaults(ctx context.Context, q pgxdb.Rower, cc *config.Config, issues []config.Issue) (*config.Config, []config.Issue) {
	on, err := store.LoadDreamInstallDefault(ctx, q)
	if err != nil {
		slog.Warn("config: reading the installation's defaults failed — dream.enabled keeps its built-in default (false) unless CTX_DREAM_ENABLED is set", "error", err)
		return cc, issues
	}
	config.SetDreamInstallDefault(on)
	if !on || cc.Source("dream.enabled") != "default" {
		return cc, issues
	}
	env, envIssues := config.FromEnv()
	return env, append(envIssues, config.Validate(env)...)
}

// dreamFirstRunNotice is what the boot says when Dream runs because the
// installation's default switched it on, not because CTX_DREAM_ENABLED did:
// that Dream is on, what it does with the chat backend, and the two ways to
// slow it or switch it off. Logged on every such boot — the operator who set
// nothing reads it on the first one.
const dreamFirstRunNotice = "dream: on by default on this new installation — it calls the chat model of the backend pool " +
	"(role dream) in the background, one block after another, for as long as ctxd runs; on a metered backend that is " +
	"cost. Slow it with `ctx dream throttle`, stop it until the next restart with `ctx dream off`, " +
	"switch it off for good with CTX_DREAM_ENABLED=false in .env and a restart (docs/operations.md, \"Dream on a new installation\")"

// logDreamInstallDefault logs dreamFirstRunNotice when the effective config
// takes dream.enabled from the installation's default.
func logDreamInstallDefault(c *config.Config) {
	if c.Dream.Enabled && c.Source("dream.enabled") == config.SourceInstall {
		slog.Info(dreamFirstRunNotice)
	}
}
