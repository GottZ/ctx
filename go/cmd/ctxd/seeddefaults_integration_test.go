//go:build integration

// A02-W5 (design/02 §4.1d, §7 W5 gate 1) against a real PG18 testcontainer:
//
//	go test -tags=integration ./cmd/ctxd/ -run TestBackendSeedFreshDatabase -count=1 -v
//
// The unit half (cmd/ctxd/seeddefaults_test.go) proves the verdict; this half
// proves the CONSEQUENCE on a fresh database, through the real assembly the
// boot uses. It lives here rather than in internal/backends because only the
// cmd/** layer may import internal/config (F1 layering rule, depguard) — and
// the whole point of the gate is that the comparison runs against the REAL
// registry defaults, not against a fixture that merely looks like them.
package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/testdb"
)

// TestBackendSeedFreshDatabaseStaysEmpty: a fresh database plus an
// installation nobody configured leaves context_backends EMPTY — no
// herbert-chat, no llama-embed, no localhost rows that are dead by
// construction inside the ctx container. That empty table is what makes the
// W4 advisory meaningful and the replacement seed paths (`ctx backends seed`,
// the init wizard) reachable at all: both go through the running API and used
// to arrive after the boot had already filled the table.
//
// The deprecation WARN must stay silent on this path — an operator who never
// used the env seed must not be told to migrate off it.
func TestBackendSeedFreshDatabaseStaysEmpty(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inserted, err := backends.Bootstrap(ctx, pool, backendBootstrapInput(config.Defaults()), backendSeedDefaults())
	if err != nil {
		t.Fatalf("Bootstrap on an unconfigured fresh install: %v, want a silent no-op", err)
	}
	if inserted != 0 {
		t.Errorf("inserted = %d, want 0 — the fresh install must stay seedable through the API", inserted)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM context_backends`).Scan(&count); err != nil {
		t.Fatalf("count context_backends: %v", err)
	}
	if count != 0 {
		t.Errorf("context_backends holds %d rows on an unconfigured fresh install, want 0", count)
	}
	if out := buf.String(); strings.Contains(out, "deprecation=env_backend_seed") {
		t.Errorf("log = %q, want no deprecation warning — the env path was not used here", out)
	}
}

// TestBackendSeedFreshDatabaseSeedsConfiguredEnv is the negative half and the
// reason this is W5 and not W6: one real env var is enough to keep the env
// seed fully alive, and the boot that uses it says so once, in the log, with
// the successor named.
func TestBackendSeedFreshDatabaseSeedsConfiguredEnv(t *testing.T) {
	pool := testdb.SetupTestDB(t)
	ctx := context.Background()

	resetAllEnv(t)
	t.Setenv("CTX_CHAT_HOST", "http://gpu.example:8089")
	cfg, _ := config.FromEnv()

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	inserted, err := backends.Bootstrap(ctx, pool, backendBootstrapInput(cfg), backendSeedDefaults())
	if err != nil {
		t.Fatalf("Bootstrap with a configured CTX_CHAT_HOST: %v", err)
	}
	if inserted == 0 {
		t.Fatal("a configured env seeded 0 rows — the deprecation window has to keep this population working")
	}
	if out := buf.String(); !strings.Contains(out, "deprecation=env_backend_seed") {
		t.Errorf("log = %q, want the deprecation attribute on the path that really ran", out)
	}
}
