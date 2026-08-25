#!/usr/bin/env bash
# ARCHIVED — the checkpoint contract is upstream since 2026-08-25 (merged as
# NousResearch/hermes-agent#94639, tip `1ee524f77d`). A host at or after that
# commit needs no patches; do not run this against such a source tree. Kept
# only to reconstruct or audit hosts pinned to v2026.8.19 or earlier. See
# README.md next to this file.
#
# Build a Hermes image with the fail-closed pre-compress checkpoint contract.
#
# Reproducible: clones the pinned upstream tag, verifies the baseline SHA-256
# manifest before patching (never patch blind over an unknown source state),
# applies the patch series, verifies the patched manifest, and builds the
# image with the upstream Dockerfile. Never deploys anything.
#
# Usage:
#   build-image.sh --upstream-tag v2026.8.19 [options]
#
#   --upstream-tag TAG  Upstream tag matching a patches/<TAG>/ directory (required)
#   --repo URL          Upstream repo (default: https://github.com/NousResearch/hermes-agent.git)
#   --src PATH          Existing checkout to use instead of cloning (must be clean, at TAG)
#   --workdir DIR       Where to clone (default: mktemp under /var/tmp)
#   --image NAME        Image tag (default: hermes-agent:ctx-checkpoint-<TAG>)
#   --skip-build        Stop after patching + verification (patch-only mode)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PATCH_ROOT="${SCRIPT_DIR}"
REPO="https://github.com/NousResearch/hermes-agent.git"
TAG=""
SRC=""
WORKDIR=""
IMAGE=""
SKIP_BUILD=0

while [ $# -gt 0 ]; do
    case "$1" in
        --upstream-tag) TAG="$2"; shift 2 ;;
        --repo)         REPO="$2"; shift 2 ;;
        --src)          SRC="$2"; shift 2 ;;
        --workdir)      WORKDIR="$2"; shift 2 ;;
        --image)        IMAGE="$2"; shift 2 ;;
        --skip-build)   SKIP_BUILD=1; shift ;;
        -h|--help)      sed -n '2,24p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

[ -n "$TAG" ] || { echo "--upstream-tag is required" >&2; exit 2; }
PATCH_DIR="${PATCH_ROOT}/${TAG}"
[ -d "$PATCH_DIR" ] || {
    echo "no patch series for ${TAG} — available:" >&2
    ls "$PATCH_ROOT" >&2
    exit 1
}
IMAGE="${IMAGE:-hermes-agent:ctx-checkpoint-${TAG}}"

if [ -n "$SRC" ]; then
    CHECKOUT="$SRC"
    echo "using existing checkout: ${CHECKOUT}"
    git -C "$CHECKOUT" diff --quiet || {
        echo "checkout is dirty — refusing to patch" >&2; exit 1;
    }
else
    WORKDIR="${WORKDIR:-$(mktemp -d /var/tmp/hermes-ctx-build.XXXXXX)}"
    CHECKOUT="${WORKDIR}/hermes-agent"
    echo "cloning ${REPO} @ ${TAG} into ${CHECKOUT}"
    git clone --depth 1 --branch "$TAG" "$REPO" "$CHECKOUT"
fi

cd "$CHECKOUT"

echo "verifying pristine baseline against ${PATCH_DIR}/baseline.sha256"
sha256sum -c "${PATCH_DIR}/baseline.sha256" || {
    echo "BASELINE MISMATCH: this source state differs from the state the" >&2
    echo "patch series was built against. Refusing to patch blind." >&2
    exit 1
}

echo "applying patch series"
if git rev-parse --git-dir >/dev/null 2>&1; then
    git -c commit.gpgsign=false -c user.name="ctx-checkpoint-build" \
        -c user.email="build@invalid" am "${PATCH_DIR}"/*.patch
else
    for p in "${PATCH_DIR}"/*.patch; do patch -p1 --fuzz=0 < "$p"; done
fi

echo "verifying patched tree against ${PATCH_DIR}/patched.sha256"
sha256sum -c "${PATCH_DIR}/patched.sha256"

if [ "$SKIP_BUILD" -eq 1 ]; then
    echo "patch-only mode: patched tree ready at ${CHECKOUT}"
    exit 0
fi

echo "building ${IMAGE}"
docker build -t "$IMAGE" "$CHECKOUT"

cat <<DONE

Built: ${IMAGE}

Before switching any live container to this image, verify in a throwaway
container against a COPY of your data mount:
  docker run --rm --network none --entrypoint python "${IMAGE}" - <<'PY'
from agent.memory_provider import PRE_COMPRESS_CHECKPOINT_API_VERSION
print("checkpoint contract v", PRE_COMPRESS_CHECKPOINT_API_VERSION)
PY

Deploying (recreate) is a separate, deliberate step — this script never
touches running containers.
DONE
