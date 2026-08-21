package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GottZ/ctx/internal/backends"
	"github.com/GottZ/ctx/internal/blocktype"
	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/dispatch"
	"github.com/GottZ/ctx/internal/events"
	"github.com/GottZ/ctx/internal/handler"
	"github.com/GottZ/ctx/internal/settings"
	"github.com/GottZ/ctx/internal/store"
	"github.com/jackc/pgx/v5/pgxpool"
)

// advisePoolEmptyBoot logs the A02-W4 empty-pool advisory (design/02 §4.1c).
// Now that the seed path has left the boot (β1), an empty context_backends is
// the ordinary state of a fresh install until someone seeds it — no chain
// serves, every query dies on the embed call, and nothing in the boot log
// says why. This names the reason at the one moment the operator is watching,
// and names it the SAME way the admin status surface does
// (backends.AdvisorySubjectPool/AdvisoryStateEmpty → `backend_pool: empty`),
// so a log line and a status frame describe one state with one vocabulary.
//
// Fires on EVERY boot, not just the first: a pool emptied later is the same
// state as one never seeded. Deliberately NOT folded into the neighbouring
// reload error — a failed Reload keeps the previous snapshot (pool.go), so a
// genuinely empty pool only ever arrives on the success path.
func advisePoolEmptyBoot(p *backends.Pool) {
	if len(p.Snapshot()) != 0 {
		return
	}
	slog.Warn("backends: pool is empty — no LLM roles will serve; seed via 'ctx backends seed' or the web UI (docs/operations.md#backends)",
		"advisory", backends.AdvisorySubjectPool, "state", backends.AdvisoryStateEmpty)
}

// bootLoadBackendPool is the boot seam around the initial pool read: the load
// itself, and the two steps that are only MEANINGFUL on a pool that was
// actually read — the empty-pool advisory and the embed-cache coupled
// fingerprint reconcile. Both run on the SUCCESS path only.
//
// A failed reload keeps the snapshot the pool had (pool.go) — at boot that is
// the empty NewPool snapshot, which is a state nobody observed rather than a
// state anybody configured. Running the two steps on it anyway told the
// operator "pool is empty" about a pool that was merely unreadable, and — the
// expensive half — handed the reconcile the EMPTY coupled set, a perfectly
// legitimate fingerprint value: mismatch against the stand on record, full
// context_embed_cache flush, and the empty set stamped as the new truth. The
// next healthy boot then mismatches against THAT stamp and flushes a second
// time. Two cold-cache spikes out of a read that never happened.
//
// The listener guards exactly this (listener.go: a failed reload leaves
// coupledPrev untouched, so no diff is taken); the boot path now does too. The
// failure stays non-fatal and unstamped, so the next boot — or the next
// coupled NOTIFY — retries against an un-advanced stand.
//
// reload and reconcile are parameters so the seam is testable without a
// database: the interesting states are "reload failed" and "reload succeeded",
// and both are unreachable through a live pgxpool in a unit test.
func bootLoadBackendPool(ctx context.Context, p *backends.Pool, reload, reconcile func(context.Context) error) {
	if err := reload(ctx); err != nil {
		slog.Error("backends: initial reload failed — pool starts empty", "error", err)
		return
	}
	// A02-W4 (design/02 §4.1c): name the empty pool in the boot log.
	advisePoolEmptyBoot(p)
	if err := reconcile(ctx); err != nil {
		slog.Error("backends: embed-cache coupled fingerprint reconcile failed — stale vectors may serve until the next boot or coupled write", "error", err)
	}
}

// Deprecation labels of the A06-A1 boot sweep, in the vocabulary A02-W5 opened
// at the dying seed path (`deprecation=env_backend_seed`): one attribute names
// WHICH deprecated surface a line is about, so an operator can grep the whole
// deprecation window out of a JSON boot log with a single key.
const deprecationRetiredRow = "retired_settings_row"

// TODO(beta13-tombstone): the ENV half of the boot sweep is MISSING between β8 and
// β13, deliberately and visibly.
//
// warnRetiredEnvVarsBoot and its `deprecation=retired_env` label lived here
// until β8. The sweep read its per-key wording from c.sources (env var vs.
// shadowing settings row), so it could only ever speak about keys the loader
// still knew — and β8 cut the last of the 29 out of the registry. design/06 §4
// Phase A #1 wrote the hand-over into the plan before the first key moved: "im
// Schnitt wird er durch den Tombstone 3.5 ersetzt, weil c.sources die Keys dann
// nicht mehr kennt." Its test file went with it in the same commit; the β3
// partition pin that guarded the shrinking sweep fataled by design once the
// live half emptied ("this sweep is spent, β13's tombstone owns the statement
// now").
//
// What is NOT covered right now: a deployment that still has a non-empty
// CTX_CHAT_HOST (or any of the other 28) in its environment boots SILENTLY. The
// loader ignores the var — fromSources is registry-driven — and nothing says so.
// That is exactly the silent-ignore fail-open design/06 §5.1 names as the first
// break path of the cut.
//
// β13 owes the replacement (design/01 §4 W9 / design/06 §3.5): a STATIC name
// list — config.RetiredEnvNames() is the source, this file the consumer — swept
// with os.LookupEnv, with (a) a value filter (set-but-empty is not set: compose
// materializes all 29 as `${VAR:-}`), (b) the list cut to value-bearing keys
// (hosts, api_keys, models — not protocols/timeouts/num_ctx/think/parallelism,
// whose compose scaffold defaults are hard-wired non-empty), (c) the known
// scaffold-default VALUES of CTX_RERANK_HOST/CTX_RERANK_MODEL exempted, and
// (d) name-only lines, never values (six of the names are api_key vars).
//
// The ROW half below survives unchanged: it keys off config.RetiredKeyNames(),
// which the cut does not touch, so it keeps reporting leftover context_settings
// rows in every scope. The reminder pin lives in retiredsources_test.go
// (TestRetiredEnvSweepIsOwedToBeta13) — it keeps the ingredient list from rotting
// while it has no consumer.

// retiredMajor is the release the 29 backend tuple keys disappear in (E1:
// v5.0.0). Spelled once — it is the anchor the whole runbook is written around
// ("before v5 / from v5 on", design/06 §8 E1d), and a boot line that named a
// different version than the docs would send operators looking for a release
// that does not exist.
const retiredMajor = "v5.0.0"

// warnRetiredSettingRowsBoot names every context_settings row that still sits
// on one of the 29 retired keys, in EVERY scope (A06-A1, design/06 §3.4 #2).
//
// These rows are the half of the problem an env sweep cannot see. They live in
// the database, they survive an .env cleanup, and their runtime effect is
// either dead already (the coupled model keys are not admitted, build.go) or
// merely a duplicate of what the backend pool serves — so nothing in normal
// operation makes them visible. What makes them urgent is the CLOSING window:
// DELETE /api/settings/<key> answers today and answers 404 after the cut, in
// every scope, which locks tenant admins out of their own api_key rows
// (design/06 §3.3 step 2a, §5.3). The line is therefore an expiring
// instruction, not a status report.
//
// The embed-cache hint rode along on the coupled:embed-cache keys until β7,
// because the recommended DELETE had a price: it changed the effective value
// and flushed the process-wide, scope-less embed cache for ALL tenants. That
// price is gone with the settings-side flush — a row on one of the five cut
// embed keys reaches no config field any more, so its DELETE changes nothing
// and costs nothing. Keeping the hint would have made this WARN quote a cost
// that no longer exists, which is the same failure as hiding one.
//
// Never fatal, like every other boot advisory: a failed sweep degrades to one
// line saying the sweep failed. A deprecation notice must not be the thing that
// stops a daemon that served queries yesterday.
func warnRetiredSettingRowsBoot(ctx context.Context, pool *pgxpool.Pool) {
	refs, err := store.SettingRowsForKeys(ctx, pool, config.RetiredKeyNames())
	if err != nil {
		slog.Warn("settings: retired-key row sweep failed — retired settings rows cannot be reported this boot",
			"deprecation", deprecationRetiredRow, "error", err)
		return
	}
	for _, ref := range refs {
		msg := "settings: a settings row still holds retired key " + ref.Key + " — retired in " + retiredMajor +
			"; remove it with DELETE /api/settings/" + ref.Key + " while this version still answers (from " +
			retiredMajor + " on the key answers 404 in every scope)"
		slog.Warn(msg,
			"deprecation", deprecationRetiredRow,
			"key", ref.Key, "scope", ref.Scope)
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
	// Health check mode: /ctx -health probes the local server with serving
	// semantics (E9) and never returns in that case — see healthmode.go.
	dispatchHealthMode()

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

	// W03-1 (Achse 03 Contract/Observability, migration 108): stamp the
	// _migrations checksum column for rows still NULL (pre-108 applies and
	// M031+ self-record rows). Non-fatal in the boot sequence's established
	// idiom (see normalizeInterruptedSyncsBoot/bootstrapAdminKeyBoot below) —
	// a missing checksum degrades a future audit, not today's serving.
	if err := store.BackfillChecksums(ctx, pool); err != nil {
		slog.Error("migration checksum backfill failed", "error", err)
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

	// A06-A1 (design/06 §3.4): the deprecation window's own channel. Named
	// here, right after the effective generation exists, because the env sweep
	// needs Source(key) to tell a live env var from one a settings row already
	// shadows — and before the backend bootstrap below, so the operator reads
	// "these sources are retired" before he reads what the dying seed path did
	// with them. Both halves are advisory only: nothing here changes a value,
	// and a boot with every retired source set behaves exactly as it did
	// yesterday.
	warnRetiredSettingRowsBoot(ctx, pool)

	// Evokoa-Clean-Room Achse 03 (design/03 §4.5, wave W03-3): the
	// schema-contract check. AFTER settings.Bootstrap — the effective
	// contract.mode's DB layer must exist for the §4.4 special precedence
	// to resolve correctly — and BEFORE the overlay/scheduler/router: an
	// enforce+breaking boot must exit before the process accepts a request
	// or starts a background arm on a schema it does not trust. Also
	// starts the periodic re-check ticker (never enforces; stop-semantics
	// stay boot-exclusive).
	schemaContractBoot(ctx, pool, cfgStore)

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

	// F3-P1: backend pool. context_backends is the ONLY source of backend
	// topology — there is no boot-time seed any more (A02-W6/β1, design/02
	// §4.2): a fresh install boots with an empty pool and the W4 advisory
	// below, and gets its rows from `ctx backends seed`, the `ctx init`
	// wizard or the web UI. Loading stays non-fatal — a degraded pool must
	// not block a boot that served queries yesterday.
	backendPool := backends.NewPool(pool, settings.BackendSecretResolver(pool))
	// A04-W4 (design/04 §3.2b): the reconcile below is the boot half of the
	// embed-cache coupled diff. The listener's diff rides the NOTIFY funnel and
	// therefore only sees edits made while this process runs; a pool row or
	// disable profile edited by psql with ctxd stopped leaves no notification
	// behind, and the listener baseline is then taken FROM that edited pool. It
	// compares the loaded pool against the fingerprint on record (migration 132)
	// and flushes context_embed_cache if they differ. Runs here — after the
	// reload, before the scheduler builds the listener — so stamp and in-memory
	// baseline describe one topology. Non-fatal like the load above;
	// nothing is stamped on a failing path, so the next boot retries.
	bootLoadBackendPool(ctx, backendPool, backendPool.Reload, func(ctx context.Context) error {
		return events.ReconcileCoupledFingerprint(ctx, pool, backendPool)
	})

	// Vorhaben E (wave MW2, carrying the W1 construction leftover — design/01
	// §7 W1: "Konstruktion in cmd/ctxd/main.go, Reload-Anbindung"): the ONE
	// process-wide dispatch admission layer (I-D1). Settings map from the
	// effective snapshot, the per-origin policy derives from the backend pool;
	// both stay hot via the NOTIFY reload owner (events.SettingsWriteHandler).
	// Since MW3 every non-stream chat call site acquires through it (llm
	// chain walk, daily synthesis, backend chat probe); stream/embed/rerank
	// follow with MW5. Empty policy = pass-through (behavior-neutral until
	// the deliberate data activation MW13).
	dispatcher := dispatch.New(nil, events.DispatchSettings(cfgStore.Snapshot().Dispatch)) //nolint:forbidigo // MT 06 BLIND: boot-time read; dispatch.* keys are global-only (design/01 §3.1), no tenant dimension exists for them.
	dispatcher.UpdatePolicy(dispatch.DerivePolicy(events.DispatchBackendRows(backendPool.Snapshot()), nil))
	defer dispatcher.Close()

	// MW4 (design/03 §4.1.1): class authorization is ctx-bound — the
	// dispatcher derives the admission principal exclusively from the request
	// context via this hook; Request carries no principal parameter. Wired
	// HERE, not in buildRouter next to the request-scope hooks: Acquire also
	// runs on the scheduler's detached contexts, which spawn below — the
	// unsynchronized write must ride the boot happens-before ahead of them.
	dispatch.SetPrincipalHook(handler.RequestPrincipal)

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
	// MW2/A5-W0: the dispatcher is the demand-herald target AND the signal the
	// scheduler's arms read (interactiveDemand → dispatcher.InteractiveDemand).
	// Installed before Run (the listener's SettingsWriteHandler reads it) and
	// before the HTTP server serves (the WithScheduler mounts inject the
	// dispatcher directly — InteractiveArrived per request).
	scheduler.SetDispatcher(dispatcher)
	go scheduler.Run(ctx)

	// HTTP server. ListenAddr is restart-only, read once from the effective
	// snapshot (== env value; the settings overlay rejects restart-only keys).
	listenAddr := cfgStore.Snapshot().Server.ListenAddr //nolint:forbidigo // MT 06 BLIND: restart-only server.listen_addr is a process-global env value (the overlay rejects restart-only keys), read once at boot.
	router := NewRouter(ctx, pool, cfgStore, scheduler, backendPool, blocktypeReg, projectHub, dispatcher)
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
