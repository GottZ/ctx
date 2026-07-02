#!/usr/bin/env bash
# e2e/job runtime budget check (design 06 §6.3, wave PV8).
#
# Two separate budgets:
#   - e2e part budget:  playwright wall time, measured from report.json
#     (stats.duration — excludes container build/install by design)
#   - job budget:       whole `web` job, measured from CTX_JOB_STARTED_AT
# Over budget => annotation (three consecutive runs over the e2e budget
# trigger PV11 sharding); job > job_fail_seconds => red. While the budget
# file says calibrated=false, budget comparisons only annotate — the first
# real CI run sets the anchors via a calibration commit (§6.3); the 10-min
# job fail is a design constant and stays active either way.
#
# Standalone script so the step is locally provable:
#   CTX_JOB_STARTED_AT=$(date +%s) bash .github/scripts/e2e-budget.sh
#
# Inputs (env):
#   REPORT              path to report.json (default go/web/e2e/.results/report.json)
#   BUDGET_FILE         path to budget json (default .github/e2e-budget.json)
#   CTX_JOB_STARTED_AT  job start epoch seconds (optional; job budget skipped if unset)
set -euo pipefail

REPORT=${REPORT:-go/web/e2e/.results/report.json}
BUDGET_FILE=${BUDGET_FILE:-.github/e2e-budget.json}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}

[ -f "$BUDGET_FILE" ] || { echo "::error::e2e-budget: $BUDGET_FILE missing"; exit 2; }

CALIBRATED=$(jq -r '.calibrated' "$BUDGET_FILE")
E2E_BUDGET=$(jq -r '.e2e_budget_seconds' "$BUDGET_FILE")
JOB_BUDGET=$(jq -r '.job_budget_seconds' "$BUDGET_FILE")
JOB_FAIL=$(jq -r '.job_fail_seconds' "$BUDGET_FILE")

{
  echo "## e2e runtime budget (design 06 §6.3)"
  echo ""
} >>"$SUMMARY"

E2E_S="n/a"
if [ -f "$REPORT" ]; then
  E2E_MS=$(jq -r '.stats.duration // empty' "$REPORT")
  if [ -n "$E2E_MS" ]; then
    E2E_S=$(( ${E2E_MS%.*} / 1000 ))
  fi
fi
if [ "$E2E_S" = "n/a" ]; then
  # No report = the e2e step itself already failed red before writing one;
  # the budget step must not mask that with a second failure.
  echo "::warning::e2e-budget: no usable $REPORT — e2e duration not measurable this run"
  echo "- e2e wall time: not measurable (no report.json)" >>"$SUMMARY"
else
  echo "- e2e wall time: ${E2E_S}s (budget: ${E2E_BUDGET}s)" >>"$SUMMARY"
fi

JOB_S="n/a"
if [ -n "${CTX_JOB_STARTED_AT:-}" ]; then
  JOB_S=$(( $(date +%s) - CTX_JOB_STARTED_AT ))
  echo "- job elapsed: ${JOB_S}s (budget: ${JOB_BUDGET}s, fail: ${JOB_FAIL}s)" >>"$SUMMARY"
fi

if [ "$CALIBRATED" != "true" ]; then
  {
    echo "- **calibration pending**: budgets above are design estimates."
    echo "  This run's measured values are the calibration input — set them in"
    echo "  \`$BUDGET_FILE\` (calibrated=true) via the calibration commit (§6.3)."
  } >>"$SUMMARY"
  echo "::notice::e2e-budget: uncalibrated (design estimates). Measured: e2e=${E2E_S}s job=${JOB_S}s — first real CI run sets the anchors via calibration commit."
else
  if [ "$E2E_S" != "n/a" ] && [ "$E2E_S" -gt "$E2E_BUDGET" ]; then
    echo "::warning::e2e-budget: e2e wall ${E2E_S}s > budget ${E2E_BUDGET}s — three consecutive runs over budget trigger PV11 sharding (design 06 §6.3)"
  fi
  if [ "$JOB_S" != "n/a" ] && [ "$JOB_S" -gt "$JOB_BUDGET" ]; then
    echo "::warning::e2e-budget: job elapsed ${JOB_S}s > budget ${JOB_BUDGET}s"
  fi
fi

# Hard fail threshold — design constant, active regardless of calibration.
if [ "$JOB_S" != "n/a" ] && [ "$JOB_S" -gt "$JOB_FAIL" ]; then
  echo "::error::e2e-budget: job elapsed ${JOB_S}s > hard limit ${JOB_FAIL}s (design 06 §6.3)"
  exit 1
fi

echo "e2e-budget: pass (e2e=${E2E_S}s job=${JOB_S}s calibrated=${CALIBRATED})"
