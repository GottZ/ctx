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
