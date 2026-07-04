#!/usr/bin/env bash
# e2e duration + flaky trend line (design 06 §4.7/§6.3, wave PV11, nightly).
#
# Emits ONE JSON line per nightly run — e2e wall time (from report.json,
# stats.duration) + flaky/expected/unexpected counts — as its own dated
# artifact. Appending to the previous day's artifact is deliberately NOT done:
# GitHub Actions artifacts are immutable per run and cross-run append is not a
# first-class operation (would need a download-merge-reupload dance with race
# windows). One line per run is enough to reconstruct the trend by downloading
# the retained set (retention-days: 90); a later aggregation job can fold them.
#
# Annotation only — never a gate. The budget gate lives in e2e-budget.sh.
#
# Standalone / locally provable (stub the GitHub env):
#   REPORT=go/web/e2e/.results/report.json TREND_DIR=/tmp/trend \
#     bash .github/scripts/e2e-trend.sh
#
# Inputs (env):
#   REPORT     path to report.json (default go/web/e2e/.results/report.json)
#   TREND_DIR  output dir for the trend line (default go/web/e2e/.results/trend)
set -euo pipefail

REPORT=${REPORT:-go/web/e2e/.results/report.json}
TREND_DIR=${TREND_DIR:-go/web/e2e/.results/trend}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
STAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
DATE=$(date -u +%Y-%m-%d)

mkdir -p "$TREND_DIR"
OUT="$TREND_DIR/trend-${DATE}.json"

if [ ! -f "$REPORT" ]; then
  echo "::warning::e2e-trend: no $REPORT — the mock-tier e2e run produced no report; trend line skipped"
  echo "## e2e trend (design 06 §6.3)" >>"$SUMMARY"
  echo "- no report.json — trend line skipped this run" >>"$SUMMARY"
  exit 0
fi

# stats.duration is milliseconds wall of the playwright run (mock tier).
LINE=$(jq -c --arg ts "$STAMP" --arg sha "${GITHUB_SHA:-local}" '{
  ts: $ts,
  sha: $sha,
  e2e_duration_ms: (.stats.duration // 0),
  e2e_duration_s:  ((.stats.duration // 0) / 1000 | floor),
  expected:   (.stats.expected // 0),
  unexpected: (.stats.unexpected // 0),
  flaky:      (.stats.flaky // 0),
  skipped:    (.stats.skipped // 0)
}' "$REPORT")

printf '%s\n' "$LINE" >"$OUT"

{
  echo "## e2e trend line (design 06 §6.3, nightly)"
  echo ""
  echo "\`\`\`json"
  echo "$LINE"
  echo "\`\`\`"
  echo ""
  echo "Uploaded as \`web-trend\` artifact (retention 90d). One line per run —"
  echo "download the retained set to reconstruct the duration/flaky trend."
} >>"$SUMMARY"

echo "e2e-trend: wrote $OUT"
echo "$LINE"
