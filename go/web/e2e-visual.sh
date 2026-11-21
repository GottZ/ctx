#!/usr/bin/env bash
# Container runner for the @visual baseline suite (design 06 §4.4, wave PV3).
#
#   bash e2e-visual.sh                          # compare against committed baselines
#   bash e2e-visual.sh --update                 # refresh changed baselines
#   bash e2e-visual.sh -- -g name               # pass extra playwright args after --
#   bash e2e-visual.sh --update -- e2e/x.spec.ts  # refresh the baselines of one spec
#
# --update passes the mode explicitly, as one argument: --update-snapshots=changed.
# Playwright reads an optional mode after a bare --update-snapshots, so the
# next argument (a spec file) would be taken as the mode and rejected (#456).
# 'changed' rewrites a baseline only when it fails the comparison (and writes a
# missing one), so an unchanged shot keeps its bytes; 'all' would rewrite
# every shot whose bytes differ inside the comparison threshold — byte diffs
# without an image change.
#
# Thin wrapper over e2e-container.sh (wave PV8 extracted the shared lock
# parse + image build so the ci.yml `web` job runs the SAME implementation —
# e2e/toolchain.lock stays the single digest source, no logic duplication).
# The suite runs with CTX_E2E_CONTAINER=1 (set by e2e-container.sh): @visual
# tests execute ONLY inside the pinned container (playwright.config.ts
# grepInvert gating) — baselines never come from an unpinned host render.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

PW_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --update) PW_ARGS+=(--update-snapshots=changed); shift ;;
    --) shift; PW_ARGS+=("$@"); break ;;
    *)
      echo "e2e-visual.sh: unknown arg '$1' (use --update or -- <playwright args>)" >&2
      exit 1
      ;;
  esac
done

# bun install --frozen-lockfile inside is the same install path as the
# release build (bun.lock = single dependency truth).
exec bash e2e-container.sh \
  bash -c 'bun install --frozen-lockfile && bun run test:e2e "$@"' bash "${PW_ARGS[@]}"
