package main

// Issue #492: bootInstallDefaults never turns a failed read into Dream on —
// the boot keeps the built-in default and the env layer it had.

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/GottZ/ctx/internal/config"
)

// failingRow is a pgx.Row whose Scan fails like a lost connection.
type failingRow struct{}

func (failingRow) Scan(...any) error { return errors.New("conn closed") }

// failingRower answers every QueryRow with failingRow.
type failingRower struct{}

func (failingRower) QueryRow(context.Context, string, ...any) pgx.Row { return failingRow{} }

// trueRow is the row of a database installed with Dream on.
type trueRow struct{}

func (trueRow) Scan(dest ...any) error {
	*dest[0].(*bool) = true
	return nil
}

// trueRower answers every QueryRow with trueRow.
type trueRower struct{}

func (trueRower) QueryRow(context.Context, string, ...any) pgx.Row { return trueRow{} }

// TestBootInstallDefaultsKeepsTheEnvValidation (issue #495): on a database
// installed with Dream on, the boot parses the env layer a second time, and
// that layer's validation findings must reach the boot dump ("config:
// effective", its issues) exactly as the first parse's do — a WARN of the
// environment does not disappear because the installation's default applied.
func TestBootInstallDefaultsKeepsTheEnvValidation(t *testing.T) {
	applyEnv(t, map[string]string{"CTX_DREAM_BACKOFF_MODE": "banana"})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	warned := func(list []config.Issue) bool {
		for _, is := range list {
			if is.Field == "dream.backoff_mode" && is.Severity == config.SeverityWarn {
				return true
			}
		}
		return false
	}
	if !warned(issues) {
		t.Fatalf("fixture: the first parse has no WARN on dream.backoff_mode: %v", issues)
	}
	config.SetDreamInstallDefault(false)
	t.Cleanup(func() { config.SetDreamInstallDefault(false) })

	got, gotIssues := bootInstallDefaults(context.Background(), trueRower{}, cc, issues)
	if got == cc || got.Source("dream.enabled") != config.SourceInstall {
		t.Fatalf("fixture: the env layer was not parsed again (source %q)", got.Source("dream.enabled"))
	}
	if !warned(gotIssues) {
		t.Errorf("the re-parsed env layer lost its validation WARN on dream.backoff_mode: %v", gotIssues)
	}
	// What the operator reads: the boot dump's issues.
	args := config.BootDumpArgs(got, gotIssues)
	var dumped []map[string]string
	for i := 0; i+1 < len(args); i += 2 {
		if args[i] == "issues" {
			dumped, _ = args[i+1].([]map[string]string)
		}
	}
	found := false
	for _, d := range dumped {
		if d["field"] == "dream.backoff_mode" && d["severity"] == config.SeverityWarn.String() {
			found = true
		}
	}
	if !found {
		t.Errorf("the boot dump carries no WARN on dream.backoff_mode: %v", dumped)
	}
}

func TestBootInstallDefaultsReadFailureKeepsTheBuiltInDefault(t *testing.T) {
	applyEnv(t, map[string]string{})
	cc, issues := loadFromEnv()
	if config.HasErrors(issues) {
		t.Fatalf("env fixture invalid: %v", issues)
	}
	config.SetDreamInstallDefault(false)
	t.Cleanup(func() { config.SetDreamInstallDefault(false) })

	got, _ := bootInstallDefaults(context.Background(), failingRower{}, cc, issues)
	if got != cc {
		t.Error("a failed read replaced the env layer")
	}
	if got.Dream.Enabled || got.Source("dream.enabled") != "default" {
		t.Errorf("after a failed read dream.enabled = %v (source %q), want false (source \"default\")",
			got.Dream.Enabled, got.Source("dream.enabled"))
	}
	if again, _ := config.FromEnv(); again.Dream.Enabled {
		t.Error("a failed read left an installation default of true behind for later generations")
	}
}
