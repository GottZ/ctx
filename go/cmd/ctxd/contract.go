package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/GottZ/ctx/internal/config"
	"github.com/GottZ/ctx/internal/schemacontract"
)

// defaultContractRecheckPollWhileOff is the polling cadence
// startContractRecheckTicker uses while contract.recheck_interval resolves
// to 0 (off, design/03 §4.5): the loop keeps waking on this fixed cadence
// so a later hot flip to a positive interval is picked up without a
// restart, without running a check while genuinely off. Matches the
// registry default (contract.recheck_interval's own default is 60s) so
// "off, then flipped back to the default" behaves identically to "always
// on at the default".
const defaultContractRecheckPollWhileOff = 60 * time.Second

// schemaContractBoot runs the boot-time schema-contract check
// (Evokoa-Clean-Room design/03 §4.5), wired after settings.Bootstrap /
// cfgStore construction (main.go, right after the "config: effective" log)
// and before cfgStore.SetOverlay / the scheduler / the router start:
//
//   - AFTER settings.Bootstrap: the effective contract.mode (subject to the
//     §4.4 special precedence) needs the DB override layer to exist —
//     though note the resolution itself does NOT read cfgStore (see below).
//   - BEFORE the scheduler/router: an enforce+breaking boot must exit
//     before the process accepts a request or starts a background arm on a
//     schema it does not trust.
//
// contract.mode is deliberately NOT read from cfgStore.Snapshot().Contract.Mode
// — internal/config's ContractConfig.Mode doc explains why in full: that
// field exists only so the key is a registered, visible/writable settings
// entry, and using its registry-merged (DB>env>default) value here would
// silently reinstate the exact precedence bug design/03 §4.4 documents.
// schemacontract.RunCheckSingleFlight resolves the real, env-dominant
// effective mode itself, independent of cfgStore. RecheckInterval has no
// such exception — startContractRecheckTicker reads it from cfgStore.
func schemaContractBoot(ctx context.Context, pool *pgxpool.Pool, cfgStore *config.Store) {
	report, err := schemacontract.RunCheckSingleFlight(ctx, pool)
	if err != nil {
		slog.Error("contract: check failed on boot", "error", err)
	}
	logInvalidContractModeEnv()
	slog.Info("contract mode=" + report.Mode + " source=" + report.ModeSource)

	breaking := breakingDrifts(report)
	switch report.Mode {
	case schemacontract.ModeEnforce:
		if report.Status == schemacontract.StatusUnchecked || len(breaking) > 0 {
			logContractBreakingDrifts(breaking, report.Status)
			slog.Error("contract: enforce mode — refusing to start",
				"escape_hatch_env", schemacontract.EnvContractMode+"=off",
				"escape_hatch_kill_switch", "CTX_SETTINGS_DISABLE=1")
			os.Exit(1)
		}
	case schemacontract.ModeWarn:
		logContractBreakingDrifts(breaking, report.Status)
		logContractParamDrifts(report)
	case schemacontract.ModeOff:
		// The check ran and is stored (visible later via /api/contract,
		// W03-4) — off only disables boot-abort and health degradation
		// (design/03 §4.4/§4.6), never the check itself or its visibility.
	}

	startContractRecheckTicker(ctx, pool, cfgStore)
}

// breakingDrifts filters report.Drifts down to SeverityBreaking entries —
// the ones an enforce boot must act on (design/03 §4.4).
func breakingDrifts(report schemacontract.Report) []schemacontract.Drift {
	var out []schemacontract.Drift
	for _, d := range report.Drifts {
		if d.Severity == schemacontract.SeverityBreaking {
			out = append(out, d)
		}
	}
	return out
}

// logContractBreakingDrifts logs one ERROR line per breaking drift, plus a
// distinct line when the check itself could not run (design/03 §4.4's last
// row: "Check selbst nicht ausführbar" is its OWN enforce/warn trigger,
// independent of any Drift entry existing).
func logContractBreakingDrifts(breaking []schemacontract.Drift, status string) {
	if status == schemacontract.StatusUnchecked {
		slog.Error("contract: check itself could not run — fail-closed, not implying ok")
	}
	for _, d := range breaking {
		slog.Error("contract: breaking drift", "class", d.Class, "object", d.Object, "detail", d.Detail)
	}
}

// logContractParamDrifts logs one WARN line per param-severity drift
// (design/03 §4.4: param drifts are loud in every mode, never boot-fatal).
func logContractParamDrifts(report schemacontract.Report) {
	for _, d := range report.Drifts {
		if d.Severity == schemacontract.SeverityParam {
			slog.Warn("contract: param drift", "class", d.Class, "object", d.Object, "detail", d.Detail)
		}
	}
}

// logInvalidContractModeEnv logs the ONE diagnostic ResolveMode's pure
// signature has no room for: a SET but unrecognized CTX_CONTRACT_MODE value
// is worth its own ERROR line (design/03 §4.4), distinct from "unset" —
// silently falling back to warn/default would hide a broken break-glass
// attempt from the operator who just typed it.
func logInvalidContractModeEnv() {
	v := os.Getenv(schemacontract.EnvContractMode)
	if v == "" || schemacontract.ValidModeValue(v) {
		return
	}
	slog.Error("contract: invalid "+schemacontract.EnvContractMode+" value — falling back to warn/default",
		"value", v)
}

// startContractRecheckTicker runs the periodic re-check goroutine
// (design/03 §4.5): it re-reads contract.recheck_interval from the
// effective config EVERY cycle (a hot settings flip takes effect on the
// NEXT tick, no restart needed) and calls the SAME single-flight entry
// point boot used. It NEVER enforces — no os.Exit runs on this path, ever;
// stop-semantics are boot-exclusive (§4.5's last bullet). Follows the
// scheduler's own hot-interval ticker idiom (internal/events/scheduler.go's
// GraphOverview.RebuildInterval loop: read Snapshot() fresh, select on
// ctx.Done()/time.After(interval)) rather than time.NewTicker, because a
// plain Ticker cannot change its period without being stopped and
// recreated — the read-fresh-then-sleep loop is how this codebase already
// solves "hot cadence, no restart" (design/03 §4.5 asks to look at exactly
// this neighbor).
func startContractRecheckTicker(ctx context.Context, pool *pgxpool.Pool, cfgStore *config.Store) {
	go func() {
		for {
			interval := cfgStore.Snapshot().Contract.RecheckInterval //nolint:forbidigo // MT 06 BLIND: the contract re-check cadence is server-global process telemetry (one shared schema, no tenant dimension), not tenant-scoped.
			wait := interval
			if wait <= 0 {
				// 0 = off (design/03 §4.5): no re-check runs, but keep
				// polling on the default cadence so a later hot flip to a
				// positive interval is picked up without a restart.
				wait = defaultContractRecheckPollWhileOff
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(wait):
			}
			// Re-read AFTER waking: the interval may have changed hot
			// during the sleep (including a flip TO off), so the decision
			// to actually run reflects the current setting, not the one
			// that was in effect when the sleep started.
			if cfgStore.Snapshot().Contract.RecheckInterval <= 0 { //nolint:forbidigo // MT 06 BLIND: see above.
				continue
			}
			if _, err := schemacontract.RunCheckSingleFlight(ctx, pool); err != nil {
				slog.Error("contract: re-check failed", "error", err)
			}
		}
	}()
}
