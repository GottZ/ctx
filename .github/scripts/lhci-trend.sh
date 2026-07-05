#!/usr/bin/env bash
# LHCI timing trend line (overnight plan C, wave C-W4, nightly).
#
# Emits ONE JSON line per nightly run — the lab timing metrics of the
# REPRESENTATIVE (median) Lighthouse run from the web-lhci job — as its own
# dated artifact (same one-line-per-run design as e2e-trend.sh: Actions
# artifacts are immutable per run, cross-run append is not a first-class
# operation; download the retained set to reconstruct the trend).
#
# NEVER a gate (C-E2 / 06-E6): annotation and trend only. Lab timings on
# shared CI runners carry a ±20-40% noise floor — a single line is never
# signal, only jumps beyond ~40% across the trend are (review C1-M2). If
# timing budgets are ever introduced, calibrate them from the MEDIAN of
# several nightly lines, never a single run (review C1-M3).
#
# Standalone / locally provable:
#   LHCI_DIR=go/web/e2e/perf/.lhci TREND_DIR=/tmp/lhci-trend \
#     bash .github/scripts/lhci-trend.sh
#
# Inputs (env):
#   LHCI_DIR   dir with LHCI filesystem-target output — manifest.json +
#              *.report.json (default go/web/e2e/perf/.lhci)
#   TREND_DIR  output dir for the trend line (default $LHCI_DIR/trend)
set -euo pipefail

LHCI_DIR=${LHCI_DIR:-go/web/e2e/perf/.lhci}
TREND_DIR=${TREND_DIR:-$LHCI_DIR/trend}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
STAMP=$(date -u +%Y-%m-%dT%H:%M:%SZ)
DATE=$(date -u +%Y-%m-%d)

MANIFEST="$LHCI_DIR/manifest.json"
if [ ! -f "$MANIFEST" ]; then
  echo "::warning::lhci-trend: no $MANIFEST — the LHCI run produced no manifest; trend line skipped"
  {
    echo "## LHCI timing trend (wave C-W4)"
    echo "- no manifest.json — trend line skipped this run"
  } >>"$SUMMARY"
  exit 0
fi

# The representative run is LHCI's median pick across numberOfRuns — the
# in-run noise damping layer (C1-M3); the cross-night median is the reader's.
REPORT=$(jq -r '[.[] | select(.isRepresentativeRun == true)][0].jsonPath // empty' "$MANIFEST")
# manifest jsonPath is absolute for the machine that ran LHCI (the toolchain
# container mounts the repo at /work) — fall back to basename resolution.
if [ ! -f "$REPORT" ]; then
  REPORT="$LHCI_DIR/$(basename "$REPORT")"
fi
if [ ! -f "$REPORT" ]; then
  echo "::warning::lhci-trend: representative report not found under $LHCI_DIR; trend line skipped"
  exit 0
fi

mkdir -p "$TREND_DIR"
OUT="$TREND_DIR/lhci-trend-${DATE}.json"

LINE=$(jq -c --arg ts "$STAMP" --arg sha "${GITHUB_SHA:-local}" '{
  ts: $ts,
  sha: $sha,
  lighthouse: .lighthouseVersion,
  url: .finalDisplayedUrl,
  perf_score: (.categories.performance.score // null),
  fcp_ms: (.audits["first-contentful-paint"].numericValue // null | if . == null then null else floor end),
  lcp_ms: (.audits["largest-contentful-paint"].numericValue // null | if . == null then null else floor end),
  tbt_ms: (.audits["total-blocking-time"].numericValue // null | if . == null then null else floor end),
  si_ms:  (.audits["speed-index"].numericValue // null | if . == null then null else floor end),
  cls:    (.audits["cumulative-layout-shift"].numericValue // null)
}' "$REPORT")

printf '%s\n' "$LINE" >"$OUT"

{
  echo "## LHCI timing trend line (wave C-W4, nightly)"
  echo ""
  echo '```json'
  echo "$LINE"
  echo '```'
  echo ""
  echo "Median run of 3 (LHCI representative run). Uploaded as \`web-lhci-trend\`"
  echo "(retention 90d) — one line per night; noise floor ±20-40%, only jumps"
  echo "beyond ~40% across the trend are signal. Never a gate."
} >>"$SUMMARY"

echo "lhci-trend: wrote $OUT"
echo "$LINE"
