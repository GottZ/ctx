package main

// A06-A1 (design/06 §3.4 #1, §7 A1) — the env half of the deprecation window's
// boot sweep.
//
// The row half needs a database and lives in retiredsources_integration_test.go.
// What this file pins is the part that has no other guard: the WORDING. Three
// situations get three texts, and each text is an instruction an operator will
// act on — "remove it", "it is already shadowed, still remove it", "do NOT
// remove this one". Getting the third one wrong is not a cosmetic bug: a
// uniform removal call on CTX_EMBED_MODEL would make this very release the
// cause of a silent model swap (see retiredEnvWarning for the two mechanisms).

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestWarnRetiredEnvVarsBoot is the A1 gate (a)-(e) in one table: which vars
// produce a line, and which line.
//
// Mutation probe: delete the `os.Getenv(info.EnvVar) == ""` guard in
// warnRetiredEnvVarsBoot and both silence subtests go red; swap the
// embed.model branch for the generic text and the CTX_EMBED_MODEL subtest goes
// red on the removal call it must never contain.
func TestWarnRetiredEnvVarsBoot(t *testing.T) {
	// Gate (a): a set retired var is named, once, with the way out.
	t.Run("set env var warns with the removal call", func(t *testing.T) {
		resetAllEnv(t)
		t.Setenv("CTX_CHAT_HOST", "https://api.groq.com/openai/v1")
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		out := buf.String()
		if n := strings.Count(out, "deprecation=retired_env"); n != 1 {
			t.Fatalf("log = %q, want exactly 1 retired_env line, got %d — one var, one line", out, n)
		}
		for _, want := range []string{
			"level=WARN",
			"CTX_CHAT_HOST is set",
			"remove the env var",
			"ctx backends",
			"key=chat.host",
			"env=CTX_CHAT_HOST",
			"shadowed=false",
		} {
			if !strings.Contains(out, want) {
				t.Errorf("log = %q, want %q", out, want)
			}
		}
		// The version anchor the runbook is written around. "the next major" was
		// the placeholder while E1 was open; naming v5.0.0 is what lets an
		// operator match the line to the release notes he is holding.
		if !strings.Contains(out, "v5.0.0") {
			t.Errorf("log = %q, want the removal release named", out)
		}
	})

	// Gate (b): the 80 % case — a deployment that already lives on the pool
	// hears nothing. An advisory that also fires on a clean installation is
	// noise, and noise is how the real signal gets skipped.
	t.Run("no retired var set stays silent", func(t *testing.T) {
		resetAllEnv(t)
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q with no retired var set, want silence", out)
		}
	})

	// Gate (c): empty env == unset, the loader's own semantics (load.go:291-296).
	// A compose file that declares CTX_CHAT_HOST without a value produces an
	// empty var on every single boot; warning about it would put a permanent
	// unfixable line into the logs of installations that are already clean.
	t.Run("empty env var is not set", func(t *testing.T) {
		resetAllEnv(t)
		t.Setenv("CTX_CHAT_HOST", "")
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q for an empty var, want silence — empty env == unset", out)
		}
	})

	// Gate (d): the double-configuration class. Row AND env set is the case a
	// source-following sweep would miss entirely — the row wins, so the var is
	// not the effective source, so a naive "warn about active sources" check
	// says nothing and the operator carries the var across the break. It fires,
	// and it fires with its OWN text: telling someone who has already moved his
	// value into a settings row to "configure backends via ctx backends" would
	// describe work he has demonstrably done.
	t.Run("settings row shadowing an env var still warns", func(t *testing.T) {
		resetAllEnv(t)
		t.Setenv("CTX_CHAT_HOST", "http://env.example.com")
		// config.Build validates, and validation fails the whole generation
		// without a DB password — unrelated to the sweep, but it would withdraw
		// the override this fixture exists to create.
		t.Setenv("CONTEXT_DB_PASSWORD", "test-password-123")
		buf := captureBootLog(t)

		cfg, issues := config.Build([]config.Override{{
			Key: "chat.host", Value: "http://row.example.com",
		}}, nil)
		if config.HasErrors(issues) {
			t.Fatalf("building the shadowed config: %v", issues)
		}
		if got := cfg.Source("chat.host"); got != config.SourceSettings {
			t.Fatalf("Source(chat.host) = %q, want %q — the fixture must actually shadow",
				got, config.SourceSettings)
		}
		warnRetiredEnvVarsBoot(cfg)

		out := buf.String()
		if n := strings.Count(out, "deprecation=retired_env"); n != 1 {
			t.Fatalf("log = %q, want exactly 1 retired_env line, got %d — a shadowed var is still a var to remove", out, n)
		}
		if !strings.Contains(out, "shadowed by a settings row") {
			t.Errorf("log = %q, want the shadow wording", out)
		}
		if !strings.Contains(out, "shadowed=true") {
			t.Errorf("log = %q, want shadowed=true", out)
		}
		// The env-source text must NOT appear: it would tell the operator to go
		// configure a pool he is evidently already past.
		if strings.Contains(out, "only feeds the first-boot seed") {
			t.Errorf("log = %q, want the shadow text INSTEAD of the env-source text", out)
		}
	})

	// Gate (e): the one var this window must not tell anybody to delete.
	t.Run("CTX_EMBED_MODEL keeps its own text and no removal call", func(t *testing.T) {
		resetAllEnv(t)
		t.Setenv("CTX_EMBED_MODEL", "qwen3-embedding-8b")
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		out := buf.String()
		if n := strings.Count(out, "deprecation=retired_env"); n != 1 {
			t.Fatalf("log = %q, want exactly 1 retired_env line, got %d", out, n)
		}
		if !strings.Contains(out, "keep it until") {
			t.Errorf("log = %q, want the keep-it instruction", out)
		}
		// The load-bearing negative. Removing this var on v4.38 re-seeds with the
		// registry default model on a fresh pool and breaks the v4.37 rollback's
		// channel probe — both silent.
		if strings.Contains(out, "remove the env var") {
			t.Errorf("log = %q, must NOT call for removal of CTX_EMBED_MODEL", out)
		}
	})

	// Coverage pin: the sweep watches the whole retirement, not the handful of
	// names a test author happened to think of. Every retired env name set at
	// once must produce exactly one line each — a key that silently loses its
	// env source, or a second line per var, shows up here and nowhere else.
	t.Run("every retired env name is swept exactly once", func(t *testing.T) {
		resetAllEnv(t)
		names := config.RetiredEnvNames()
		for _, name := range names {
			t.Setenv(name, "x")
		}
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		out := buf.String()
		if n := strings.Count(out, "deprecation=retired_env"); n != len(names) {
			t.Errorf("log carries %d retired_env lines, want %d (one per retired env var)", n, len(names))
		}
		for _, name := range names {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, want a line for %s", out, name)
			}
		}
	})

	// Absence pin: the sweep must stay inside the retirement. CTX_DREAM_ENABLED,
	// CTX_EMBED_DIMS and the rerank toggles look like neighbours of the 29 but
	// survive the cut — warning about them would send operators deleting live
	// configuration.
	t.Run("keys that survive the cut are not swept", func(t *testing.T) {
		resetAllEnv(t)
		for _, name := range []string{
			"CTX_DREAM_ENABLED", "CTX_DREAM_PARALLELISM", "CTX_DREAM_LANGUAGE",
			"CTX_EMBED_DIMS", "CTX_RERANK_ENABLED", "CTX_RERANK_MAX_DOCS",
		} {
			t.Setenv(name, "1")
		}
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		if out := buf.String(); out != "" {
			t.Errorf("log = %q, want silence — these keys survive the cut", out)
		}
	})
}
