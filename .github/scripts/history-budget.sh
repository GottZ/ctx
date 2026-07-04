#!/usr/bin/env bash
# Screenshot-history blob-volume budget (design 06 §3.1/§6.3, wave PV11, nightly).
#
# The relevant size is the history RATE, not the working tree: every [baseline]
# full refresh adds ~9-15 MB of NON-delta-compressible PNG blobs permanently to
# the git history (§3.1). This step measures the cumulative __screenshots__ blob
# volume across ALL of history and annotates:
#
#   >= 60 MB   ⇒ ::warning (watch — batching/sequencing §5.5 should be holding)
#   >= 150 MB  ⇒ ::warning + the documented escalation path is DUE
#
# Deliberately NEVER a hard fail (design 06 §6.3: "ab 150 MB ist der
# Eskalationspfad fällig" — a decision to be taken WITH measured data, not an
# auto-fail of the nightly verification). The escalation (baseline orphan branch
# with shallow reference, or Git-LFS) moves history OUT of the main line and is
# the User's call; auto-failing the nightly run would neither fix it nor add
# information beyond this annotation.
#
# Standalone / locally provable:
#   REF=HEAD bash .github/scripts/history-budget.sh
#
# Inputs (env):
#   REF   git ref to walk (default root — nightly runs on root, so HEAD==root;
#         HEAD is the safe fallback for worktrees where the `root` branch ref
#         may be absent)
#   SCREENSHOT_PATH   history path (default go/web/e2e/__screenshots__)
set -euo pipefail

SCREENSHOT_PATH=${SCREENSHOT_PATH:-go/web/e2e/__screenshots__}
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}
WARN_MB=60
ESCALATE_MB=150

# Prefer the design's `root`; fall back to HEAD if that ref is not present
# (git worktrees / shallow checkouts). fetch-depth: 0 in the nightly job makes
# full history available.
REF=${REF:-}
if [ -z "$REF" ]; then
  if git rev-parse --verify --quiet root >/dev/null; then REF=root; else REF=HEAD; fi
fi

# Sum the raw (uncompressed) size of every blob version ever committed under the
# screenshot path. `git rev-list --objects` yields "<sha> <path>" lines; keep the
# sha only, then default `git cat-file --batch-check` prints "<sha> <type> <size>".
BYTES=$(git rev-list --objects "$REF" -- "$SCREENSHOT_PATH" \
  | awk '{print $1}' \
  | git cat-file --batch-check 2>/dev/null \
  | awk '$2=="blob"{n++; sum+=$3} END{print (sum+0)}')
BLOBS=$(git rev-list --objects "$REF" -- "$SCREENSHOT_PATH" \
  | awk '{print $1}' \
  | git cat-file --batch-check 2>/dev/null \
  | awk '$2=="blob"{n++} END{print (n+0)}')

MB=$(awk -v b="$BYTES" 'BEGIN{printf "%.2f", b/1048576}')
MB_INT=$(awk -v b="$BYTES" 'BEGIN{printf "%d", b/1048576}')

{
  echo "## Screenshot history budget (design 06 §6.3, nightly)"
  echo ""
  echo "- ref: \`$REF\`  path: \`$SCREENSHOT_PATH\`"
  echo "- cumulative blob volume: **${MB} MB** across ${BLOBS} committed blob versions"
  echo "- thresholds: warn ${WARN_MB} MB · escalation ${ESCALATE_MB} MB (never an auto-fail)"
} >>"$SUMMARY"

if [ "$MB_INT" -ge "$ESCALATE_MB" ]; then
  echo "::warning::history-budget: __screenshots__ history is ${MB} MB (>= ${ESCALATE_MB} MB) — the documented escalation path is DUE: move baseline history to an orphan branch (shallow reference) or Git-LFS. Decision is the User's, WITH this measurement (design 06 §6.3); the nightly run is NOT auto-failed."
  echo "- **escalation DUE**: orphan-branch / Git-LFS decision (design 06 §6.3)" >>"$SUMMARY"
elif [ "$MB_INT" -ge "$WARN_MB" ]; then
  echo "::warning::history-budget: __screenshots__ history is ${MB} MB (>= ${WARN_MB} MB) — watch the refresh rate; sequencing + [baseline]-squash batching (§5.5) should be holding it."
  echo "- **watch**: above the ${WARN_MB} MB warn line" >>"$SUMMARY"
else
  echo "history-budget: ${MB} MB (${BLOBS} blobs) — below ${WARN_MB} MB warn line"
  echo "- below the ${WARN_MB} MB warn line" >>"$SUMMARY"
fi
