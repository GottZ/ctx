package config

// Issue #492: the installation's default of dream.enabled (migration 192,
// handed over by cmd/ctxd through SetDreamInstallDefault) applies only where
// the environment left CTX_DREAM_ENABLED unset, in every generation the
// process builds — the env layer, a reload's Build and a tenant overlay's —
// and an explicit value decides in both directions.

import "testing"

// setDreamInstallDefault sets the process-wide default for one test and puts
// the built-in state back afterwards.
func setDreamInstallDefault(t *testing.T, on bool) {
	t.Helper()
	prev := dreamInstallDefault.Load()
	SetDreamInstallDefault(on)
	t.Cleanup(func() { SetDreamInstallDefault(prev) })
}

func TestDreamInstallDefaultOnlyWhereTheEnvironmentIsSilent(t *testing.T) {
	const key = "dream.enabled"
	for _, c := range []struct {
		name       string
		install    bool
		env        string // "" = unset
		wantOn     bool
		wantSource string
	}{
		{"upgraded_unset", false, "", false, "default"},
		{"upgraded_env_true", false, "true", true, "env"},
		{"upgraded_env_false", false, "false", false, "env"},
		{"installed_unset", true, "", true, SourceInstall},
		{"installed_env_false", true, "false", false, "env"},
		{"installed_env_true", true, "true", true, "env"},
		// Booleans match "true" exactly; any other set value is an explicit
		// false (parserFor typBool), so it beats the installation's default.
		{"installed_env_not_true", true, "yes", false, "env"},
	} {
		t.Run(c.name, func(t *testing.T) {
			resetBuildEnv(t)
			setDreamInstallDefault(t, c.install)
			t.Setenv("CTX_DREAM_ENABLED", c.env)

			env, _ := FromEnv()
			if env.Dream.Enabled != c.wantOn || env.Source(key) != c.wantSource {
				t.Errorf("FromEnv: dream.enabled = %v (source %q), want %v (source %q)",
					env.Dream.Enabled, env.Source(key), c.wantOn, c.wantSource)
			}
			// A reload builds its candidate through Build (settings.Reload,
			// the tenant overlay, the settings PUT): same answer, also when a
			// row on another key is overlaid.
			for _, overrides := range [][]Override{nil, {{Key: "rerank.blend_weight", Value: "0.4"}}} {
				built, issues := Build(overrides, nil)
				if HasErrors(issues) {
					t.Fatalf("Build(%v): %v", overrides, issues)
				}
				if built.Dream.Enabled != c.wantOn || built.Source(key) != c.wantSource {
					t.Errorf("Build(%v): dream.enabled = %v (source %q), want %v (source %q)",
						overrides, built.Dream.Enabled, built.Source(key), c.wantOn, c.wantSource)
				}
			}
		})
	}
}

// TestDreamInstallDefaultIsNoSettingsRow pins that the installation's default
// does not open dream.enabled to context_settings: the key stays
// restart-class, a row on it is ignored with the usual WARN, and the
// effective value keeps coming from the environment or the installation.
func TestDreamInstallDefaultIsNoSettingsRow(t *testing.T) {
	resetBuildEnv(t)
	setDreamInstallDefault(t, true)

	built, issues := Build([]Override{{Key: "dream.enabled", Value: "false"}}, nil)
	warnFor(t, issues, "dream.enabled", "not runtime-overridable")
	if !built.Dream.Enabled || built.Source("dream.enabled") != SourceInstall {
		t.Errorf("a settings row switched the installation's default: dream.enabled = %v (source %q)",
			built.Dream.Enabled, built.Source("dream.enabled"))
	}
}

// TestDefaultsCarryNoInstallDefault pins that Defaults stays the registry
// defaults alone ("what an installation on which nobody set anything runs
// on" before #492 — its callers compare against the built-in values).
func TestDefaultsCarryNoInstallDefault(t *testing.T) {
	setDreamInstallDefault(t, true)
	if d := Defaults(); d.Dream.Enabled || d.Source("dream.enabled") != "default" {
		t.Errorf("Defaults(): dream.enabled = %v (source %q), want false (source \"default\")",
			d.Dream.Enabled, d.Source("dream.enabled"))
	}
}
