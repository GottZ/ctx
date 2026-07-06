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

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/rerank"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// backendBootstrapInput assembles the env-era tuples for the one-time pool
// bootstrap from the effective config snapshot. Lives here (not in
// internal/backends) because backends cannot import config — config already
// imports backends for the tuple type.
func backendBootstrapInput(c *config.Config) backends.BootstrapInput {
	return backends.BootstrapInput{
		Chat:           c.ChatBackend(),
		Fallback:       c.ChatFallbackBackend(),
		Embed:          c.EmbedBackend(),
		Dream:          c.DreamBackend(),
		DreamEmbed:     c.DreamEmbedBackend(),
		RerankHost:     c.Rerank.Host,
		RerankKey:      c.Rerank.APIKey,
		RerankModel:    c.Rerank.Model,
		RerankTimeoutS: int(rerank.Timeout.Seconds()),
	}
}

// defaultListenAddr mirrors the registry default for server.listen_addr
// (pinned against drift by TestBootDefaults). The -health mode needs it
// WITHOUT the full config load: it must work in a crash-looping container
// where FromEnv could fail, so it reads LISTEN_ADDR raw.
const defaultListenAddr = ":8080"

// normalizeInterruptedSyncsBoot runs the W11 boot-time sync crash recovery
// (design/03 §3.1): a project left at sync_status='running' by a crash is a lie
// (the in-memory run-state died with the process), so normalise it to
// error:interrupted and close open run rows as interrupted. Idempotent, never
// fatal — a degraded normalisation must not block a boot that served yesterday.
func normalizeInterruptedSyncsBoot(ctx context.Context, pool *pgxpool.Pool) {
	if np, nr, err := store.NormalizeInterruptedSyncs(ctx, pool); err != nil {
		slog.Error("sync normalization failed", "error", err)
	} else if np > 0 || nr > 0 {
		slog.Info("normalized interrupted syncs on boot", "projects", np, "runs", nr)
	}
}

// bootstrapAdminKeyBoot runs the fail-closed first-key bootstrap (design 06
// §3.6, wave PV10a): the e2e live-tier compose stack generates a per-run random
// key and passes it here via CTX_BOOTSTRAP_ADMIN_KEY (plus a run id via
// CTX_BOOTSTRAP_RUN_ID for the label). When the api-key table is EMPTY, ctxd
// mints a single server-admin key with that plaintext's hash under the label
// e2e-bootstrap-<run-id>; on a POPULATED table it mints nothing and only logs —
// the credential is NEVER injected into a real deployment. The env is unset in
// every normal deployment, so this is a no-op there. It also covers the ops
// fresh-DB deploy henhouse-egg gap (same problem, same safe mechanic).
//
// The plaintext is NEVER logged — only the created flag, the label and the
// minted key id (an already-public identity, neither hash nor plaintext).
func bootstrapAdminKeyBoot(ctx context.Context, pool *pgxpool.Pool) {
	plaintext := os.Getenv("CTX_BOOTSTRAP_ADMIN_KEY")
	if plaintext == "" {
		return // not an e2e/bootstrap boot — the normal case, no-op
	}
	runID := os.Getenv("CTX_BOOTSTRAP_RUN_ID")
	if runID == "" {
		runID = "unknown"
	}
	label := "e2e-bootstrap-" + runID
	created, keyID, err := store.BootstrapAdminKey(ctx, pool, plaintext, label)
	if err != nil {
		slog.Error("bootstrap admin key failed", "error", err, "label", label)
		return
	}
	if created {
		slog.Info("bootstrap admin key minted (empty api-key table)", "label", label, "api_key_id", keyID)
	} else {
		slog.Info("bootstrap admin key ignored — api-key table already populated (fail-closed)", "label", label)
	}
}

func main() {
	// Health check mode: /ctx -health makes an HTTP request to the local server.
	// Used as Docker healthcheck in distroless containers (no curl/wget available).
	if len(os.Args) > 1 && os.Args[1] == "-health" {
		addr := os.Getenv("LISTEN_ADDR")
		if addr == "" {
			addr = defaultListenAddr
		}
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

	// Hidden overview-rebuild worker mode (E-A, design/05 §4.7): never
	// returns when argv[1] == overview.WorkerCommand (see overviewworker.go).
	dispatchOverviewWorkerMode()

	logger := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelInfo,
	}))
	slog.SetDefault(logger)

	// Parse + validate (internal/config), log EVERY issue, then fail fast on
	// any SeverityError — a misconfigured boot dies with field + reason for
	// each finding, never with just the first one. config.Config is the only
	// config type since F1-W7 (the cmd/ctxd bridge struct died).
	cc, issues := config.FromEnv()
	issues = append(issues, config.Validate(cc)...)
	for _, is := range issues {
		if is.Severity == config.SeverityError {
			slog.Error("config: invalid", "field", is.Field, "msg", is.Msg)
		} else {
			slog.Warn("config: issue", "field", is.Field, "msg", is.Msg)
		}
	}
	if config.HasErrors(issues) {
		slog.Error("config: refusing to start — fix the fields above in .env and restart")
		os.Exit(1)
	}

	// Root context cancelled on SIGINT/SIGTERM
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Database pool with pgvector support. The DSN comes from the env-layer
	// config: the settings overlay below needs this pool to even load, and the
	// CONTEXT_DB_* group is restart-only — the effective snapshot can never
	// carry a different DSN than the one we boot with.
	pool, err := store.NewPool(ctx, cc.DSN())
	if err != nil {
		slog.Error("failed to create database pool", "error", err)
		os.Exit(1)
	}
	defer pool.Close()

	slog.Info("database pool created", "host", cc.Server.DBHost, "db", cc.Server.DB)

	// Run database migrations
	if err := store.RunMigrations(ctx, pool); err != nil {
		slog.Error("migrations failed", "error", err)
		os.Exit(1)
	}

	// W11 (design/03 §3.1): normalise crash-orphaned sync run-state (running ⇒
	// interrupted) before the router serves — the in-memory state does not survive
	// a restart. Extracted to keep main() under the cyclop budget.
	normalizeInterruptedSyncsBoot(ctx, pool)

	// PV10a (design 06 §3.6): fail-closed first-key bootstrap for the e2e
	// live-tier (and ops fresh-DB deploys). No-op unless CTX_BOOTSTRAP_ADMIN_KEY
	// is set AND context_api_keys is empty — never injects a key into a
	// populated table. Runs after migrations (the table must exist) and before
	// the router serves.
	bootstrapAdminKeyBoot(ctx, pool)

	// WF T3 (design 01 §4.3): block-type registry — starts on the compiled-in
	// builtin set, then loads the context_block_types rows with the error-class
	// split (42P01 pre-072 ⇒ WARN + builtin; corrupt row ⇒ ERROR + /health
	// blocktype_registry=builtin-fallback + retry loop until healed). Never
	// fatal, but degradation is LOUD: the builtin fallback would silently
	// revert operator visibility narrowings otherwise. Wired before scheduler
	// and router so both see the registry from the first request/NOTIFY on.
	blocktypeReg := blocktype.NewRegistry()
	blocktypeReg.Boot(ctx, pool)

	// §3.6 master-key rotation: while CTX_SECRETS_KEY_PREV is set, re-seal
	// every prev-key secret with the current key (no-op otherwise, never
	// fatal). Boot-only — the NOTIFY reload path stays read-only by contract.
	settings.ReencryptSweep(ctx, pool)

	// F2-W4, §2.1 steps 4–7: overlay the context_settings overrides (DB >
	// env > default; secret_refs resolve in-memory via the sealbox) onto the
	// validated env config and publish the EFFECTIVE generation as the
	// initial snapshot. Never fatal — kill switch, missing table, corrupt
	// values and missing/wrong master key all degrade to WARN + env-only
	// (the env layer above already fail-fasted on real config errors).
	// Every consumer reads this store per operation (F1-W4–W7).
	effCfg, effIssues := settings.Bootstrap(ctx, pool, cc, issues)
	cfgStore := config.NewStore(effCfg)
	slog.Info("config: effective", config.BootDumpArgs(cfgStore.Snapshot(), effIssues)...) //nolint:forbidigo // MT 06 BLIND: boot-time effective-config dump — the _global generation, no tenant exists at boot.

	// MT 06-C3: install the per-tenant config overlay so SnapshotForTenant /
	// SnapshotForRequest can resolve a tenant's context_settings rows on top of
	// the _global base (the overlay needs the pool + secret resolver, hence the
	// settings layer supplies it). Set here at boot, before the scheduler and
	// HTTP server start, so the happens-before holds and the field needs no
	// synchronization. Behavior-neutral until a caller actually resolves a
	// tenant: SnapshotForRequest stays on base until C5 wires the scope hook,
	// SnapshotForTenant has no caller until the C6 background iteration, and a
	// single-tenant deployment (no per-tenant rows) inherits the base anyway.
	cfgStore.SetOverlay(settings.TenantOverlay(pool))

	// F3-P1: backend pool. The one-time bootstrap reads the EFFECTIVE
	// snapshot (X1: settings>env precedence, never raw os.Getenv) and seeds
	// context_backends when empty; afterwards the TABLE is the source of
	// truth. Both steps are non-fatal — P1 has no pool consumer yet, and a
	// degraded pool must not block a boot that served queries yesterday.
	backendPool := backends.NewPool(pool, settings.BackendSecretResolver(pool))
	if _, err := backends.Bootstrap(ctx, pool, backendBootstrapInput(cfgStore.Snapshot())); err != nil { //nolint:forbidigo // MT 06 BLIND: boot-time backend pool bootstrap reads the _global generation, no tenant exists at boot.
		slog.Error("backends: bootstrap failed", "error", err)
	}
	if err := backendPool.Reload(ctx); err != nil {
		slog.Error("backends: initial reload failed — pool starts empty", "error", err)
	}

	// Backfill temporal dimensions for blocks missing from context_temporal.
	if n, err := store.BackfillTemporal(ctx, pool); err != nil {
		slog.Error("temporal backfill failed", "error", err)
	} else if n > 0 {
		slog.Info("temporal backfill complete", "blocks_processed", n)
	}

	// Scheduler for background guard + digest + dream. Everything hot —
	// scopes, dream/embed backend tuples, idle wait, back-off policy — comes
	// from the cfgStore snapshot per cycle/run (F1-W6; the old events.Config
	// boot copy died here). StartupConfig carries only the restart-only
	// parameters: DSN from the SAME value the pool was built from (listener
	// and pool must point at one database), DreamEnabled and
	// DreamParallelism from the effective snapshot (validated + clamped;
	// both fix the worker-goroutine set once in Run). ReconnectDelay keeps
	// its zero value = pgxlisten 5s default.
	scheduler := events.NewScheduler(pool, cfgStore, backendPool, events.StartupConfig{
		DSN:              cc.DSN(),
		DreamEnabled:     effCfg.Dream.Enabled,
		DreamParallelism: effCfg.Dream.Parallelism,
	})
	// Boot happens-before (SetOverlay pattern): installed before Run starts
	// the listener goroutine, so the unsynchronized field write is safe.
	scheduler.SetBlocktypeRegistry(blocktypeReg)
	// W9: the SSE domain-event hub. Its flush loop is bound to ctx (server
	// lifecycle); the scheduler's listener forwards ctx_project_write (081) to it.
	// Installed before Run for the same boot happens-before contract.
	projectHub := events.NewProjectHub(ctx, pool, cfgStore)
	scheduler.SetProjectHub(projectHub)
	// E-A: wire the overview worker argv (the own binary's hidden subcommand)
	// under the same boot happens-before as SetBlocktypeRegistry.
	wireOverviewWorkerArgv(scheduler)
	go scheduler.Run(ctx)

	// HTTP server. ListenAddr is restart-only, read once from the effective
	// snapshot (== env value; the settings overlay rejects restart-only keys).
	listenAddr := cfgStore.Snapshot().Server.ListenAddr //nolint:forbidigo // MT 06 BLIND: restart-only server.listen_addr is a process-global env value (the overlay rejects restart-only keys), read once at boot.
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub)
	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      120 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	// Start HTTP server in background
	errCh := make(chan error, 1)
	go func() {
		slog.Info("starting HTTP server", "addr", listenAddr)
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
