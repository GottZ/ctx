#!/usr/bin/env bash
# Container runner for the @visual baseline suite (design 06 §4.4, wave PV3).
#
#   bash e2e-visual.sh              # compare against committed baselines
#   bash e2e-visual.sh --update     # refresh baselines (--update-snapshots)
#   bash e2e-visual.sh -- -g name   # pass extra playwright args after --
#
# The toolchain image is built from e2e/Dockerfile with the digest pins read
# from e2e/toolchain.lock — the SINGLE source (the PV8 ci.yml web job reads
# the same file). A wrong/drifted digest fails the build loudly; there is no
# fallback to a mutable tag. The suite runs with CTX_E2E_CONTAINER=1: @visual
# tests execute ONLY inside this pinned container (playwright.config.ts
# grepInvert gating) — baselines never come from an unpinned host render.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

LOCK="e2e/toolchain.lock"
[ -f "$LOCK" ] || { echo "e2e-visual.sh: $LOCK missing" >&2; exit 1; }

lock_var() {
  local val
  val=$(grep -E "^$1=" "$LOCK" | head -n1 | cut -d= -f2-)
  if [ -z "$val" ]; then
    echo "e2e-visual.sh: $1 missing from $LOCK" >&2
    exit 1
  fi
  if ! printf '%s' "$val" | grep -qE '@sha256:[0-9a-f]{64}$'; then
    echo "e2e-visual.sh: $1 in $LOCK is not digest-pinned (need <ref>@sha256:<64 hex>): $val" >&2
    exit 1
  fi
  printf '%s' "$val"
}

PLAYWRIGHT_IMAGE=$(lock_var PLAYWRIGHT_IMAGE)
BUN_IMAGE=$(lock_var BUN_IMAGE)

PW_ARGS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --update) PW_ARGS+=(--update-snapshots); shift ;;
    --) shift; PW_ARGS+=("$@"); break ;;
    *)
      echo "e2e-visual.sh: unknown arg '$1' (use --update or -- <playwright args>)" >&2
      exit 1
      ;;
  esac
done

# Local image tag derives from the lock content: a digest bump busts the tag,
# so a stale local toolchain image can never serve a new pin.
TAG="ctx-e2e-toolchain:$(sha256sum "$LOCK" | cut -c1-12)"

echo "e2e-visual.sh: building $TAG from pinned digests ($LOCK)" >&2
docker build -f e2e/Dockerfile \
  --build-arg PLAYWRIGHT_IMAGE="$PLAYWRIGHT_IMAGE" \
  --build-arg BUN_IMAGE="$BUN_IMAGE" \
  -t "$TAG" e2e

# --ipc=host + --init per Playwright container guidance (design 06 §4.4).
# The repo dir is mounted; bun install --frozen-lockfile inside is the same
# install path as the release build (bun.lock = single dependency truth).
exec docker run --rm --init --ipc=host \
  -e CTX_E2E_CONTAINER=1 \
  -v "$PWD:/work" -w /work \
  "$TAG" \
  bash -c 'bun install --frozen-lockfile && bun run test:e2e "$@"' bash "${PW_ARGS[@]}"
