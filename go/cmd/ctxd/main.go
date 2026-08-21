package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
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
const (
	deprecationRetiredEnv = "retired_env"
	deprecationRetiredRow = "retired_settings_row"
)

// retiredEnvTripwireSuffixes selects the VALUE-BEARING half of the 29 retired
// env names: the 6 `_HOST`, the 6 `_API_KEY` and the 5 `_MODEL` vars (design/01
// §4 W9, "Liste geschnitten auf wert-tragende Keys").
//
// The other twelve — protocols, the fallback timeout, num_ctx, think — carry no
// topology. Their compose scaffold defaults are hard-wired non-empty
// (`${CTX_CHAT_PROTOCOL:-ollama}` and friends), so a sweep over them would warn
// about every unmodified v4 compose file on the installed base while telling
// nobody anything: "your protocol is still ollama" is not a lost configuration.
// Alert fatigue on twelve certain false positives would bury the seventeen that
// can actually mean a dead host or an ignored provider key.
//
// Suffix matching rather than a second transcript of seventeen names: the names
// come from config.RetiredEnvNames() and the suffix is what the CLASS is made
// of, so a key added to or dropped from the retirement moves this list with it.
// retiredsources_test.go pins the resulting partition (6/6/5 = 17 of 29) so a
// suffix that ever started matching something else shows up as a count.
var retiredEnvTripwireSuffixes = []string{"_HOST", "_API_KEY", "_MODEL"}

// retiredEnvScaffoldDefaults are the two values the tripwire must stay silent
// about (design/01 §4 W9, "Scaffold-Default-Ausnahmen").
//
// These two vars were the only members of the value-bearing seventeen whose
// compose scaffold shipped a non-empty default (`${CTX_RERANK_HOST:-http://ctx-rerank:8082}`),
// so on an unmodified v4 compose file they arrive set, non-empty, and identical
// on every installation that never touched rerank. Warning about them would be
// a guaranteed false positive on exactly the cohort this sweep exists for. A
// DIFFERENT value is a real operator choice and still warns — the exemption is
// value-scoped, not name-scoped.
var retiredEnvScaffoldDefaults = map[string]string{
	"CTX_RERANK_HOST":  "http://ctx-rerank:8082",
	"CTX_RERANK_MODEL": "bge-reranker-v2-m3",
}

// retiredEnvTripwireNames returns the env names the boot tripwire sweeps: the
// value-bearing subset of config.RetiredEnvNames(), in that list's sorted order.
func retiredEnvTripwireNames() []string {
	all := config.RetiredEnvNames()
	out := make([]string, 0, len(all))
	for _, name := range all {
		for _, suffix := range retiredEnvTripwireSuffixes {
			if strings.HasSuffix(name, suffix) {
				out = append(out, name)
				break
			}
		}
	}
	return out
}

// warnRetiredEnvVarsBoot is the K4 tombstone tripwire: the ENV half of the boot
// sweep, rebuilt on a static name list after the registry cut took the old one
// away (design/01 §4 W9, design/06 §3.5; TODO(beta13-tombstone) discharged).
//
// The gap it closes: fromSources is registry-driven, and from this release on
// the registry has never heard of chat.host. A deployment that still exports
// CTX_CHAT_HOST therefore boots in perfect silence — the loader does not ignore
// the var with a complaint, it does not see it at all. design/06 §5.1 names that
// silent-ignore as the first break path of the cut, and a boot log is the only
// one of the project's three communication channels that reaches a CONCRETE
// deployment without anybody reading anything first (§6.1).
//
// Its predecessor read each key's wording from c.sources (live env var vs.
// shadowing settings row) and could only speak about keys the loader still knew;
// it died with its subject in β8. This one asks the environment directly, which
// is the X1 carve-out the design grants twice (§3.4, §3.5): the read feeds the
// WARNING and never a config value, so "no raw env as a configuration source"
// stands. The emptiness test mirrors FromEnv byte for byte (load.go:296, empty
// env == unset) — a TrimSpace here would describe a different installation than
// the loader sees.
//
// Three filters, each of them the difference between a signal and noise:
//
//   - VALUE FILTER. A v4-era compose file materialises all 29 as `${VAR:-}`, so
//     every one of them is SET and empty on the whole compose cohort. An
//     existence scan would have printed up to 29 lines per boot there.
//   - VALUE-BEARING SUBSET (retiredEnvTripwireSuffixes).
//   - SCAFFOLD DEFAULTS (retiredEnvScaffoldDefaults).
//
// NAME-ONLY, never values: six of the seventeen names are api_key vars, and a
// boot log travels into log aggregators and support bundles. The line carries
// the var NAME and the way out; the value stays where it is. retiredsources_test.go
// probes that negatively with a needle.
//
// The text does not say "remove it now" on purpose. Runbook order is: boot v5,
// verify the pool, THEN strip .env and the compose declarations (design/05 §9
// sentence 4, design/06 §3.3 steps 3+5) — the vars are the safety net of a
// binary rollback until v5 is verified, and this is also why the WARN is the
// runbook's built-in progress signal: expected in step 3, gone after step 5.
func warnRetiredEnvVarsBoot() {
	for _, name := range retiredEnvTripwireNames() {
		val, set := os.LookupEnv(name)
		if !set || val == "" {
			continue
		}
		if def, exempt := retiredEnvScaffoldDefaults[name]; exempt && val == def {
			continue
		}
		slog.Warn("settings: retired env var "+name+" is set — ignored since "+retiredMajor+
			"; the backend pool owns this value now (inspect it with 'ctx backends list'), and the"+
			" upgrade runbook removes the var after v5 is verified (docs/operations.md)",
			"deprecation", deprecationRetiredEnv, "env", name)
	}
}

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
// operation makes them visible.
//
// The instruction it carries is SQL, not an API call. This binary IS the cut
// (E13): the 29 keys left the registry with it, so DELETE /api/settings/<key>
// answers 404 in every scope — including for the tenant admins whose own
// api_key rows these are (design/06 §3.3 step 2a, §5.3). Migration 133 sweeps
// the rows that existed at upgrade time; a row this sweep still finds was
// written around the API afterwards, and the same way is the only way out of
// it. Naming the closed route instead would send an operator to a 404 and
// leave the row where it is — an advisory whose remedy does not execute is
// worse than none, because it also reads as a remedy already attempted.
// The precedent is the migration-132 embed-cache advisory
// (internal/events/coupled_fingerprint.go), which names its DELETE the same
// way.
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
		// name-only, like the whole sweep: key and scope are identifiers the
		// operator needs to act, the row's VALUE never appears (six of the 29
		// are api_key keys, and a boot log travels into aggregators).
		msg := "settings: a settings row still holds retired key " + ref.Key + " — retired in " + retiredMajor +
			"; the settings API answers 404 for it in every scope, so remove the row in the database: " +
			"DELETE FROM context_settings WHERE key = '" + ref.Key + "' AND scope = '" + ref.Scope + "' " +
			"(docs/operations.md, Migration 133)"
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

	// A06-A1 / K4 (design/06 §3.4 + §3.5): the retirement's own boot channel,
	// both halves side by side — env vars the loader no longer sees, and
	// settings rows on keys the registry no longer knows.
	//
	// The site is inherited rather than chosen: the pair lived here through the
	// deprecation window because the env half then read Source(key) and needed
	// the effective generation to exist. The tombstone rebuild dropped that
	// dependency (static list, os.LookupEnv), so the env sweep could now run at
	// any point of the boot — it stays next to its sibling because an operator
	// greps `deprecation=` for ONE block, and two halves of one message split
	// across the log would read as two unrelated problems.
	//
	// Both halves are advisory only: nothing here changes a value, and a boot
	// with every retired source set behaves exactly as one with none.
	warnRetiredEnvVarsBoot()
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
