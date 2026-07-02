#!/usr/bin/env bash
# CI baseline marker gate (design 06 §5.5 layer 2, wave PV8).
#
# Enforcement that survives --no-verify and dead local hooks: if the change
# range touches go/web/e2e/__screenshots__/** or GROWS the a11y debt ledger
# (go/web/e2e/a11y-baseline.json entry count), every commit in the range that
# touches those paths must carry a [baseline] marker in its message.
# Accepted alternative: the PR label `baseline-update` (design 06 §5.5).
# Shrinking the debt (ratchet) and screenshot-free ranges pass freely.
#
# Standalone script (not inline YAML) so the gate is locally provable:
#
#   RANGE_BASE=origin/root bash .github/scripts/baseline-marker-gate.sh
#
# Inputs (env):
#   RANGE_BASE   base ref/sha of the range (PR base ref or push `before`) — required
#   RANGE_HEAD   head of the range (default: HEAD)
#   PR_LABELS    space-separated PR label names (optional)
set -euo pipefail

SHOTS_RE='^go/web/e2e/__screenshots__/'
DEBT_FILE='go/web/e2e/a11y-baseline.json'
MARKER='[baseline]'
LABEL='baseline-update'

RANGE_HEAD=${RANGE_HEAD:-HEAD}
if [ -z "${RANGE_BASE:-}" ]; then
  echo "::error::baseline-marker-gate: RANGE_BASE not set"
  exit 2
fi
if ! git rev-parse -q --verify "$RANGE_BASE^{commit}" >/dev/null; then
  echo "::error::baseline-marker-gate: cannot resolve RANGE_BASE '$RANGE_BASE' (shallow checkout?)"
  exit 2
fi

# Merge-base semantics = the three-dot PR range from design 06 §5.5: only
# changes introduced on the head side count, not drift on the base branch.
BASE=$(git merge-base "$RANGE_BASE" "$RANGE_HEAD")
CHANGED=$(git diff --name-only "$BASE" "$RANGE_HEAD")

TRIGGER_PATHS=()

if printf '%s\n' "$CHANGED" | grep -qE "$SHOTS_RE"; then
  TRIGGER_PATHS+=("go/web/e2e/__screenshots__/")
fi

# Debt growth = MORE entries at head than at merge-base. Entries are objects
# with a "page" field (a11y-baseline.json schema, design 06 §3.3); the count
# comparison is the same mechanic as the local commit-msg hook (layer 1) —
# robust against reformatting, removals (ratchet) pass freely.
if printf '%s\n' "$CHANGED" | grep -qFx "$DEBT_FILE"; then
  DEBT_BASE=$(git show "$BASE:$DEBT_FILE" 2>/dev/null | grep -cE '"page"[[:space:]]*:' || true)
  DEBT_HEAD=$(git show "$RANGE_HEAD:$DEBT_FILE" 2>/dev/null | grep -cE '"page"[[:space:]]*:' || true)
  if [ "${DEBT_HEAD:-0}" -gt "${DEBT_BASE:-0}" ]; then
    TRIGGER_PATHS+=("$DEBT_FILE")
  fi
fi

if [ ${#TRIGGER_PATHS[@]} -eq 0 ]; then
  echo "baseline-marker-gate: no baseline-relevant changes in range — pass"
  exit 0
fi

for label in ${PR_LABELS:-}; do
  if [ "$label" = "$LABEL" ]; then
    echo "baseline-marker-gate: PR label '$LABEL' present — accepted alternative to per-commit markers"
    exit 0
  fi
done

VIOLATIONS=()
for commit in $(git rev-list "$BASE".."$RANGE_HEAD" -- "${TRIGGER_PATHS[@]}"); do
  if ! git log -1 --format=%B "$commit" | grep -qF "$MARKER"; then
    VIOLATIONS+=("$commit $(git log -1 --format=%s "$commit")")
  fi
done

if [ ${#VIOLATIONS[@]} -gt 0 ]; then
  echo "::error::baseline-marker-gate: baseline-touching commits without a [baseline] marker"
  echo ""
  echo "Range touches: ${TRIGGER_PATHS[*]}"
  echo "Visual baselines are the frozen 'objectively good' reference; a11y"
  echo "debt entries are frozen accessibility exceptions. Changing either is"
  echo "a visible decision, not a side effect (design 06 §5.5). Commits:"
  for v in "${VIOLATIONS[@]}"; do
    echo "  - $v"
  done
  echo ""
  echo "Fix: reword those commits with a [baseline] marker (+ one line WHY),"
  echo "or apply the PR label '$LABEL'."
  exit 1
fi

echo "baseline-marker-gate: all baseline-touching commits carry $MARKER — pass"
