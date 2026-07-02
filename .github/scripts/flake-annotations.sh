#!/usr/bin/env bash
# Flake annotation from the Playwright JSON report (design 06 §4.8/§5.4, PV8).
#
# Retries are declared infrastructure protection, not a flake blanket: every
# `flaky` outcome (test passed only on retry) becomes a visible annotation +
# job-summary row. Process rule (COVERAGE.md header): a test flaky twice in
# 14 days must be quarantined or fixed — this step is the visibility that
# rule depends on. Annotation only; never a gate (exit 0 unless the report
# is unreadable garbage).
#
# Standalone script so the step is locally provable:
#   bash .github/scripts/flake-annotations.sh
#
# Inputs (env):
#   REPORT   path to report.json (default go/web/e2e/.results/report.json)
set -euo pipefail

REPORT=${REPORT:-go/web/e2e/.results/report.json}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}

if [ ! -f "$REPORT" ]; then
  echo "::warning::flake-annotations: no $REPORT — e2e step failed before writing a report?"
  exit 0
fi

STATS=$(jq -r '.stats | "expected=\(.expected) unexpected=\(.unexpected) flaky=\(.flaky) skipped=\(.skipped)"' "$REPORT")
FLAKY=$(jq -r '.stats.flaky' "$REPORT")

{
  echo "## Playwright retry statistics (design 06 §5.4)"
  echo ""
  echo "\`$STATS\`"
  echo ""
} >>"$SUMMARY"

if [ "$FLAKY" -eq 0 ]; then
  echo "- no flaky tests this run" >>"$SUMMARY"
  echo "flake-annotations: 0 flaky"
  exit 0
fi

# A spec whose test carries status "flaky" passed only on retry. Emit one
# annotation per spec (file + title) and mirror it into the summary.
jq -r '
  [.. | objects | select(.specs? != null) | .specs[]
   | select(any(.tests[]?; .status == "flaky"))
   | "\(.file)|\(.title)"] | unique | .[]
' "$REPORT" | while IFS='|' read -r file title; do
  echo "::warning file=go/web/e2e/${file}::flaky (passed on retry): ${title}"
  echo "- flaky: \`${file}\` — ${title}" >>"$SUMMARY"
done

{
  echo ""
  echo "Rule: flaky twice within 14 days ⇒ quarantine (e2e/quarantine.json, wave PV11) or fix."
} >>"$SUMMARY"

echo "flake-annotations: $FLAKY flaky test(s) annotated"
