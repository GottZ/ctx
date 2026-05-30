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

	"github.com/GottZ/ctx/internal/embed"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/llm"
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

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	cfg, err := LoadConfig()
	if err != nil {
		slog.Error("failed to load config", "error", err)
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

	// Set wire protocol defaults from config.
	llm.DefaultProtocol = cfg.ChatProtocol
	embed.DefaultProtocol = cfg.EmbedProtocol

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

	// Backfill temporal dimensions for blocks missing from context_temporal.
	if n, err := store.BackfillTemporal(ctx, pool); err != nil {
		slog.Error("temporal backfill failed", "error", err)
	} else if n > 0 {
		slog.Info("temporal backfill complete", "blocks_processed", n)
	}

	// Scheduler for background guard + digest + dream.
	// CTX_READ_SCOPES is comma-separated; empty/missing falls back to the
	// v1.x default ("private,shared,work") for backwards-compat. v2.0.0 M045
	// dropped chk_scope, so any non-empty string is a valid scope.
	readScopes := parseScopes(os.Getenv("CTX_READ_SCOPES"), []string{"private", "shared", "work"})
	schedulerConfig := &events.Config{
		DSN:                cfg.DSN(),
		HomeScope:          "private",
		ReadScopes:         readScopes,
		DreamEnabled:       cfg.DreamEnabled,
		EmbedHost:          cfg.EmbedHost,
		EmbedAPIKey:        cfg.EmbedAPIKey,
		EmbedModel:         cfg.EmbedModel,
		EmbedNumCtx:        cfg.EmbedNumCtx,
		EmbedProtocol:      cfg.EmbedProtocol,
		DreamHost:          cfg.DreamHost,
		DreamAPIKey:        cfg.DreamAPIKey,
		ChatModel:          cfg.ChatModel,
		DreamModel:         cfg.DreamModel,
		DreamThink:         dreamThink,
		DreamNumCtx:        cfg.DreamNumCtx,
		DreamIdleWait:      cfg.DreamIdleWait,
		DreamParallelism:   cfg.DreamParallelism,
		DreamEmbedHost:     cfg.DreamEmbedHost,
		DreamEmbedAPIKey:   cfg.DreamEmbedAPIKey,
		DreamEmbedProtocol: cfg.DreamEmbedProtocol,
		DreamEmbedModel:    cfg.DreamEmbedModel,
		DreamEmbedNumCtx:   cfg.DreamEmbedNumCtx,
	}
	scheduler := events.NewScheduler(pool, schedulerConfig)
	go scheduler.Run(ctx)

	// HTTP server
	router := NewRouter(pool, cfg, scheduler)
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
