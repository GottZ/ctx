package main

// A06-A1 (design/06 §3.4 #1, §7 A1) — the env half of the deprecation window's
// boot sweep.
//
// The row half needs a database and lives in retiredsources_integration_test.go.
// What this file pins is the part that has no other guard: the WORDING. Two
// situations get two texts, and each text is an instruction an operator will
// act on — "remove it" and "it is already shadowed, still remove it".
//
// A third text existed until β7: "do NOT remove this one", for CTX_EMBED_MODEL,
// whose deletion would break the v4.37 rollback's channel probe. β7 cut
// embed.model out of the registry, so the sweep is silent about the var — and
// the coverage of the must-not moved with it, into the partition pin below,
// whose cut half asserts that NO word about a cut name appears in the log. That
// is a strictly stronger statement than "no removal call"; carrying the
// instruction itself forward is β13's tombstone sweep.

import (
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/config"
)

// TestWarnRetiredEnvVarsBoot is the A1 gate (a)-(e) in one table: which vars
// produce a line, and which line.
//
// Mutation probe: delete the `os.Getenv(info.EnvVar) == ""` guard in
// warnRetiredEnvVarsBoot and both silence subtests go red; drop the
// `!ok` continue and the partition pin goes red on every cut name it then
// names, CTX_EMBED_MODEL among them.
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
		if strings.Contains(out, "the backend pool owns this value now") {
			t.Errorf("log = %q, want the shadow text INSTEAD of the env-source text", out)
		}
	})

	// Gate (e) named CTX_EMBED_MODEL as the one var this window must not tell
	// anybody to delete, and asserted both halves of its own text ("keep it
	// until …", never "remove the env var"). β7 cut embed.model out of the
	// registry; the sweep skips keys the registry no longer knows, so the var
	// gets no line at all and the subtest's first assertion (exactly one
	// retired_env line) could only be satisfied by re-registering the key.
	//
	// The must-not survives as an absence: the partition pin below sets EVERY
	// retired env name and requires the cut ones — CTX_EMBED_MODEL now among
	// them — to appear nowhere in the log. Probed: re-registering embed.model
	// makes it a "live" name there and the generic removal text is what it gets.

	// Coverage pin: the sweep watches the whole retirement, not the handful of
	// names a test author happened to think of. Every retired env name that is
	// still a registry key must produce exactly one line — a key that silently
	// loses its env source, or a second line per var, shows up here and nowhere
	// else.
	//
	// The partition is the cut train showing through, and it is the designed
	// hand-over, not a relaxation. This sweep reads its per-key wording from
	// c.sources (env vs. settings row), so it can only speak about keys the
	// loader still knows: "im Schnitt wird er durch den Tombstone 3.5 ersetzt,
	// weil c.sources die Keys dann nicht mehr kennt" (design/06 §4 Phase A #1).
	// The replacement — a static name list with the value filter and the
	// scaffold-default exceptions of design/01 §4 W9 — is β13. Widening THIS
	// sweep onto cut keys instead would fire on every unchanged v4 compose file,
	// where CTX_RERANK_HOST carries the scaffold default the design names
	// explicitly as a must-not-warn (design/01 §4, W9 bullet 2).
	//
	// So both halves are pinned: one line per still-registered name, and NOT A
	// WORD about a name whose key has been cut. The second half is what keeps
	// the gap visible until β13 closes it — a silent partial sweep would look
	// exactly like a working one.
	t.Run("every registered retired env name is swept exactly once", func(t *testing.T) {
		resetAllEnv(t)

		registered := map[string]bool{}
		for _, key := range config.RetiredKeyNames() {
			if info, ok := config.KeyByName(key); ok {
				registered[info.EnvVar] = true
			}
		}
		var live, cut []string
		for _, name := range config.RetiredEnvNames() {
			t.Setenv(name, "x")
			if registered[name] {
				live = append(live, name)
			} else {
				cut = append(cut, name)
			}
		}
		if len(live) == 0 {
			t.Fatal("no retired key is registered any more — this sweep is spent, β13's tombstone owns the statement now")
		}
		buf := captureBootLog(t)

		cfg, _ := config.FromEnv()
		warnRetiredEnvVarsBoot(cfg)

		out := buf.String()
		if n := strings.Count(out, "deprecation=retired_env"); n != len(live) {
			t.Errorf("log carries %d retired_env lines, want %d (one per registered retired env var)", n, len(live))
		}
		for _, name := range live {
			if !strings.Contains(out, "env="+name) {
				t.Errorf("log = %q, want a line for %s", out, name)
			}
		}
		for _, name := range cut {
			if strings.Contains(out, name) {
				t.Errorf("log = %q names %s — a cut key has no c.sources entry to describe; "+
					"the tombstone sweep (β13) is what warns about it", out, name)
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
