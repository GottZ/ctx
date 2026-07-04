#!/usr/bin/env bash
# Shared runner for the pinned e2e toolchain container (design 06 §4.4, PV8).
#
#   bash e2e-container.sh <command> [args...]
#
# Parses e2e/toolchain.lock (the SINGLE digest source, decision E4), builds
# the toolchain image from e2e/Dockerfile with those pins, and executes the
# given command inside the container with the repo dir mounted at /work.
#
# Consumers: e2e-visual.sh (local baseline runs) and the ci.yml `web` job
# (every bun step) — ONE parse/build implementation, no second copy of the
# lock mechanics anywhere (design 06 §4.4: ci.yml and the script read the
# same file; wave PV8 extracted this shared runner instead of duplicating).
#
# CTX_E2E_CONTAINER=1 is set unconditionally: this IS the pinned container,
# the env var is truthful here by construction — @visual pixel asserts run
# (playwright.config.ts grepInvert gating). CI is passed through so the
# CI-specific config knobs (workers: 4, retries: 1, forbidOnly) apply inside.
# CTX_E2E_QUARANTINE is passed through so the nightly web-job step can include
# the @quarantine specs (design 06 §5.4); empty on PRs ⇒ they stay excluded.
set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

LOCK="e2e/toolchain.lock"
[ -f "$LOCK" ] || { echo "e2e-container.sh: $LOCK missing" >&2; exit 1; }

lock_var() {
  local val
  val=$(grep -E "^$1=" "$LOCK" | head -n1 | cut -d= -f2-)
  if [ -z "$val" ]; then
    echo "e2e-container.sh: $1 missing from $LOCK" >&2
    exit 1
  fi
  if ! printf '%s' "$val" | grep -qE '@sha256:[0-9a-f]{64}$'; then
    echo "e2e-container.sh: $1 in $LOCK is not digest-pinned (need <ref>@sha256:<64 hex>): $val" >&2
    exit 1
  fi
  printf '%s' "$val"
}

PLAYWRIGHT_IMAGE=$(lock_var PLAYWRIGHT_IMAGE)
BUN_IMAGE=$(lock_var BUN_IMAGE)

if [ $# -eq 0 ]; then
  echo "usage: e2e-container.sh <command> [args...]" >&2
  exit 2
fi

# Local image tag derives from the lock content: a digest bump busts the tag,
# so a stale local toolchain image can never serve a new pin.
TAG="ctx-e2e-toolchain:$(sha256sum "$LOCK" | cut -c1-12)"

echo "e2e-container.sh: building $TAG from pinned digests ($LOCK)" >&2
docker build -f e2e/Dockerfile \
  --build-arg PLAYWRIGHT_IMAGE="$PLAYWRIGHT_IMAGE" \
  --build-arg BUN_IMAGE="$BUN_IMAGE" \
  -t "$TAG" e2e

# --ipc=host + --init per Playwright container guidance (design 06 §4.4).
exec docker run --rm --init --ipc=host \
  -e CTX_E2E_CONTAINER=1 \
  -e CTX_E2E_QUARANTINE="${CTX_E2E_QUARANTINE:-}" \
  -e CI="${CI:-}" \
  -v "$PWD:/work" -w /work \
  "$TAG" \
  "$@"
