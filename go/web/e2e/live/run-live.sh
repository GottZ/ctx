#!/usr/bin/env bash
# Live-tier orchestrator (design 06 §4.7, wave PV10). ONE code path local + CI:
#
#   1. Generate a per-RUN random bootstrap key + run id + DB password (secrets
#      never touch disk beyond the process env + the gitignored .results state).
#   2. Bring up the THROWAWAY compose stack (fresh PG + the real ctx image, built
#      from go/Dockerfile so the embedded SPA is the release SPA). ctxd's PV10a
#      bootstrap mints the first server-admin key from the per-run key BECAUSE
#      the DB starts empty.
#   3. Wait for /health, then run seed.ts (fail-closed target gate → production-
#      path seeds) + the @live specs inside the PINNED toolchain image
#      (e2e/toolchain.lock, built via e2e-container.sh — ONE lock source) with
#      --network host so it reaches the published ctx port. The toolchain image
#      is reused (not the CTX_E2E_CONTAINER=1 path) — the live tier makes NO
#      pixel assertions (§4.7), so render-pinning is irrelevant here; the image
#      is reused only for the pinned bun+browser toolchain.
#   4. Always tear the stack down (trap) — the DB (and every key) dies with it.
#
# Usage: bash e2e/live/run-live.sh   (from anywhere; paths are BASH_SOURCE-based)
set -euo pipefail

LIVE_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"   # go/web/e2e/live
WEB_DIR="$(cd "$LIVE_DIR/../.." && pwd)"                    # go/web
COMPOSE_FILE="$LIVE_DIR/docker-compose.e2e.yml"
PROJECT="ctx-e2e-live"
HOST_PORT=18099
BASE_URL="http://localhost:${HOST_PORT}"
STATE_FILE="$LIVE_DIR/.results/seed-state.json"

randhex() { openssl rand -hex 32 2>/dev/null || head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'; }

export CTX_BOOTSTRAP_ADMIN_KEY="$(randhex)"
export CTX_BOOTSTRAP_RUN_ID="$(date +%s)-$(randhex | cut -c1-8)"
export CTX_E2E_DB_PASSWORD="$(randhex)"

cleanup() {
  echo "run-live: tearing down $PROJECT (stack + volumes)"
  docker compose -p "$PROJECT" -f "$COMPOSE_FILE" down -v --remove-orphans >/dev/null 2>&1 || true
}
trap cleanup EXIT

echo "run-live: building + starting throwaway stack ($PROJECT)"
docker compose -p "$PROJECT" -f "$COMPOSE_FILE" up --build -d

# Readiness = the HTTP server responds at all. ctxd starts serving only AFTER
# migrations + the PV10a bootstrap (main.go order), so ANY HTTP status means
# fully booted. We do NOT wait for /health==200: /health pings the LLM backends
# (embed/synthesis), which the e2e stack deliberately has none of — it tests
# UI/API/enforcement/SSE, not inference, so this stack's /health answers 503 by
# design and always will. (That state is exactly what β1/E9 taught `/ctx
# -health` to accept as serving; this poll predates it and stays because it
# needs no image at all.) /api/whoami without a key returns 401 once the router
# serves, which is the readiness signal.
echo "run-live: waiting for ctxd to serve at $BASE_URL"
for i in $(seq 1 90); do
  code="$(curl -s -o /dev/null -w '%{http_code}' "$BASE_URL/api/whoami" 2>/dev/null || echo 000)"
  if [ "$code" != "000" ]; then echo "run-live: ctxd serving (HTTP $code) after ${i}s"; break; fi
  if [ "$i" -eq 90 ]; then
    echo "run-live: ctxd never started serving — dumping logs" >&2
    docker compose -p "$PROJECT" -f "$COMPOSE_FILE" logs ctx | tail -40 >&2
    exit 1
  fi
  sleep 1
done

# Live env for seed + specs (the admin key is the SAME per-run key ctxd booted
# with; the seed's handshake proves the target holds exactly it).
LIVE_ENV=(
  -e CTX_E2E_LIVE=1
  -e "CTX_E2E_ADMIN_KEY=$CTX_BOOTSTRAP_ADMIN_KEY"
  -e "CTX_E2E_RUN_ID=$CTX_BOOTSTRAP_RUN_ID"
  -e "CTX_E2E_BASE_URL=$BASE_URL"
  -e "CTX_E2E_STATE_FILE=/work/e2e/live/.results/seed-state.json"
  -e "CTX_E2E_LEAK_INJECT=${CTX_E2E_LEAK_INJECT:-}"
  -e "CI=${CI:-}"
)

# Ensure the pinned toolchain image exists + deps are installed (reuses
# e2e-container.sh — the single toolchain.lock consumer, no duplicated lock
# mechanics).
echo "run-live: preparing pinned toolchain + deps"
bash "$WEB_DIR/e2e-container.sh" bun install --frozen-lockfile
TAG="ctx-e2e-toolchain:$(sha256sum "$WEB_DIR/e2e/toolchain.lock" | cut -c1-12)"

echo "run-live: running @live specs (seed globalSetup + specs)"
docker run --rm --init --network host \
  "${LIVE_ENV[@]}" \
  -v "$WEB_DIR:/work" -w /work \
  "$TAG" \
  bun x playwright test -c e2e/live/playwright.live.config.ts

echo "run-live: PASS"
