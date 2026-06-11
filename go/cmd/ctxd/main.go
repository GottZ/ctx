package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dream"
	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
)

func main() {
	// Health check mode: /ctx -health makes an HTTP request to the local server.
	// Used as Docker healthcheck in distroless containers (no curl/wget available).
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		addr := getEnv("LISTEN_ADDR", defaultListenAddr)
		url := fmt.Sprintf("http://localhost%s/health", addr)
		resp, err := http.Get(url) //nolint:gosec,noctx // healthcheck is fire-and-forget
		if err != nil {
			fmt.Fprintf(os.Stderr, "health check failed: %v\n", err)
			os.Exit(1)
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			fmt.Fprintf(os.Stderr, "health check returned status %d\n", resp.StatusCode)
			os.Exit(1)
		}
		os.Exit(0)
	}

	// Break-glass decrypt mode: /ctx -secret-decrypt reads one
	// nonce_b64:ct_b64:name:scope record from stdin and prints the plaintext.
	// Env-only (CTX_SECRETS_KEY[_PREV]), no DB — see cmd/ctxd/sealbox.go and
	// break-glass.sh at the repo root.
	if len(os.Args) > 1 && os.Args[1] == "-secret-decrypt" {
		os.Exit(runSecretDecrypt(os.Stdin, os.Stdout, os.Stderr))
	}

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Parse + validate (internal/config), log EVERY issue, then fail fast on
	// any SeverityError — a misconfigured boot dies with field + reason for
	// each finding, never with just the first one.
	cfg, cc, issues, err := loadConfig()
	for _, is := range issues {
		if is.Severity == config.SeverityError {
			slog.Error("config: invalid", "field", is.Field, "msg", is.Msg)
		} else {
			slog.Warn("config: issue", "field", is.Field, "msg", is.Msg)
		}
	}
	if err != nil {
		slog.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	if config.HasErrors(issues) {
		slog.Error("config: refusing to start — fix the fields above in .env and restart")
		os.Exit(1)
	}

	// Root context cancelled on SIGINT/SIGTERM
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Database pool with pgvector support
	pool, err := store.NewPool(ctx, cfg.DSN())
	if err != nil {
		slog.Error("failed to create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	slog.Info("database pool created", "host", cfg.ContextDBHost, "db", cfg.ContextDB)

	// Set wire protocol defaults from config. The chat protocol + fallback now
	// travel as backends.Backend parameters (F1-W3); embed (F1-W5) and dream
	// (F1-W6) still read package state.
	embed.DefaultProtocol = cfg.EmbedProtocol
	dream.Protocol = cfg.DreamProtocol

	// Think modes are parsed per pipeline in server.go (chatThink) and scheduler config (dreamThink).
	// Dream think falls back to chat think if not explicitly set.
	dreamThink := parseThinkMode(cfg.DreamThink)
	if cfg.DreamThink == "" {
		dreamThink = parseThinkMode(cfg.ChatThink)
	}

	// Run database migrations
	if err := store.RunMigrations(ctx, pool); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}

	// F2-W4, §2.1 steps 4–7: overlay the context_settings overrides (DB >
	// env > default; secret_refs resolve in-memory via the sealbox) onto the
	// validated env config and publish the EFFECTIVE generation as the
	// initial snapshot. Never fatal — kill switch, missing table, corrupt
	// values and missing/wrong master key all degrade to WARN + env-only
	// (the env layer above already fail-fasted on real config errors).
	// Consumers adopt the store wave by wave (G09–G14) and read the legacy
	// bridge view until then.
	effCfg, effIssues := settings.Bootstrap(ctx, pool, cc, issues)
	cfgStore := config.NewStore(effCfg)
	slog.Info("config: effective", config.BootDumpArgs(cfgStore.Snapshot(), effIssues)...)

	// Backfill temporal dimensions for blocks missing from context_temporal.
	if n, err := store.BackfillTemporal(ctx, pool); err != nil {
		slog.Error("temporal backfill failed", "error", err)
	} else if n > 0 {
		slog.Info("temporal backfill complete", "blocks_processed", n)
	}

	// Scheduler for background guard + digest + dream.
	// Scopes come from the EFFECTIVE snapshot (scheduler.read_scopes parses
	// CTX_READ_SCOPES with the legacy semantics: comma-separated, empty ⇒
	// "private,shared,work"; v2.0.0 M045 dropped chk_scope, so any non-empty
	// string is a valid scope). home_scope has no env knob — default
	// "private", overridable via context_settings since F2-W4 (boot-time
	// only: the scheduler reads its config once).
	schedulerConfig := &events.Config{
		DSN:                     cfg.DSN(),
		HomeScope:               effCfg.Scheduler.HomeScope,
		ReadScopes:              effCfg.Scheduler.ReadScopes,
		DreamEnabled:            cfg.DreamEnabled,
		EmbedHost:               cfg.EmbedHost,
		EmbedAPIKey:             cfg.EmbedAPIKey,
		EmbedModel:              cfg.EmbedModel,
		EmbedNumCtx:             cfg.EmbedNumCtx,
		EmbedProtocol:           cfg.EmbedProtocol,
		DreamHost:               cfg.DreamHost,
		DreamAPIKey:             cfg.DreamAPIKey,
		ChatModel:               cfg.ChatModel,
		ChatNumCtx:              cfg.ChatNumCtx,
		DreamModel:              cfg.DreamModel,
		DreamThink:              dreamThink,
		DreamNumCtx:             cfg.DreamNumCtx,
		DreamIdleWait:           cfg.DreamIdleWait,
		DreamParallelism:        cfg.DreamParallelism,
		DreamBackoffMode:        cfg.DreamBackoffMode,
		DreamBackoffFactor:      cfg.DreamBackoffFactor,
		DreamBackoffGrace:       cfg.DreamBackoffGrace,
		DreamBackoffCapHours:    cfg.DreamBackoffCapHours,
		DreamBackoffMinHours:    cfg.DreamBackoffMinHours,
		DreamBackoffInertOffset: cfg.DreamBackoffInertOffset,
		DreamEmbedHost:          cfg.DreamEmbedHost,
		DreamEmbedAPIKey:        cfg.DreamEmbedAPIKey,
		DreamEmbedProtocol:      cfg.DreamEmbedProtocol,
		DreamEmbedModel:         cfg.DreamEmbedModel,
		DreamEmbedNumCtx:        cfg.DreamEmbedNumCtx,
	}
	scheduler := events.NewScheduler(pool, schedulerConfig)
	go scheduler.Run(ctx)

	// HTTP server
	router := NewRouter(pool, cfg, cfgStore, scheduler)
	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start HTTP server in background
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting HTTP server", "addr", cfg.ListenAddr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
		close(errCh)
	}()

	// Wait for shutdown signal or server error
	select {
	case <-ctx.Done():
		slog.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil {
			slog.Error("HTTP server error", "error", err)
			cancel()
		}
	}

	// Graceful shutdown: HTTP Stop -> Background Cancel -> Listener Stop -> Pool Close
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()

	slog.Info("shutting down HTTP server")
	if err := srv.Shutdown(shutdownCtx); err != nil {
		slog.Error("HTTP server shutdown error", "error", err)
	}

	// Wait for scheduler to finish (drains active dream cycle before pool closes).
	slog.Info("waiting for scheduler shutdown")
	scheduler.Wait()

	// Pool is closed via defer above.
	slog.Info("shutdown complete")
}
