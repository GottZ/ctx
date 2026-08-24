#!/usr/bin/env bash
# Install the ctx_checkpoint memory provider into a Hermes home directory.
#
# Copies the user plugin to $HERMES_HOME/plugins/ctx_checkpoint/ and reports
# which operating mode the host supports. Never edits config.yaml and never
# restarts anything — config changes and process lifecycle stay with the
# operator.
#
# Usage:
#   install.sh [--hermes-home PATH] [--hermes-src PATH] [--dry-run]
#
#   --hermes-home PATH  Hermes home (default: $HERMES_HOME, then /opt/data if
#                       it holds a config.yaml, then ~/.hermes)
#   --hermes-src PATH   Hermes source checkout/installation to probe for the
#                       fail-closed checkpoint contract (optional)
#   --dry-run           Show what would happen without writing anything

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_SRC="${SCRIPT_DIR}/../plugin/ctx_checkpoint"
HERMES_HOME_ARG=""
HERMES_SRC=""
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --hermes-home) HERMES_HOME_ARG="$2"; shift 2 ;;
        --hermes-src)  HERMES_SRC="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help)     sed -n '2,17p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

[ -f "${PLUGIN_SRC}/__init__.py" ] || {
    echo "plugin source not found at ${PLUGIN_SRC}" >&2; exit 1;
}

resolve_home() {
    if [ -n "$HERMES_HOME_ARG" ]; then echo "$HERMES_HOME_ARG"; return; fi
    if [ -n "${HERMES_HOME:-}" ]; then echo "$HERMES_HOME"; return; fi
    if [ -f /opt/data/config.yaml ]; then echo /opt/data; return; fi
    echo "${HOME}/.hermes"
}

HOME_DIR="$(resolve_home)"
DEST="${HOME_DIR}/plugins/ctx_checkpoint"

echo "Hermes home:    ${HOME_DIR}"
echo "Plugin source:  ${PLUGIN_SRC}"
echo "Plugin dest:    ${DEST}"

if [ ! -d "$HOME_DIR" ]; then
    echo "Hermes home ${HOME_DIR} does not exist — pass --hermes-home." >&2
    exit 1
fi

if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would install plugin version $(cat "${PLUGIN_SRC}/VERSION")"
else
    if [ -d "$DEST" ]; then
        BACKUP="${DEST}.bak-$(date -u +%Y%m%dT%H%M%SZ)"
        echo "existing install found — backing up to ${BACKUP}"
        mv "$DEST" "$BACKUP"
    fi
    mkdir -p "${HOME_DIR}/plugins"
    cp -R "$PLUGIN_SRC" "$DEST"
    rm -rf "${DEST}/__pycache__" "${DEST}/tests/__pycache__"
    echo "installed plugin version $(cat "${DEST}/VERSION")"
fi

# ---------------------------------------------------------------------------
# Host capability probe: does this Hermes know the fail-closed contract?
# ---------------------------------------------------------------------------
MODE="unknown (no --hermes-src given)"
if [ -n "$HERMES_SRC" ]; then
    if grep -q "PRE_COMPRESS_CHECKPOINT_API_VERSION" \
        "${HERMES_SRC}/agent/memory_provider.py" 2>/dev/null; then
        MODE="fail-closed capable (checkpoint contract present)"
    else
        MODE="stock host (best-effort only)"
    fi
fi
echo "Host mode:      ${MODE}"

cat <<'NEXT'

Next steps (manual, operator-owned):

1. Make sure the ctx MCP server is configured in config.yaml:
     mcp_servers:
       ctx:
         # your ctx server entry (see https://github.com/GottZ/ctx)

2. Activate the provider in config.yaml:
     memory:
       provider: ctx_checkpoint
       ctx_checkpoint:
         mcp_server: ctx                  # MCP server name (default: ctx)
         category: compaction-checkpoints # ctx category for checkpoint blocks
         chunk_chars: 36000               # source part size (1k..40k)
         sensitivity: internal            # ctx sensitivity for stored blocks

3. Fail-closed gate — ONLY on a patched host (see ../patches/):
     compression:
       checkpoint_required: true
   WARNING: on a stock (unpatched) Hermes this key is silently ignored.
   Compaction then keeps running WITHOUT a guaranteed checkpoint even
   though the config suggests otherwise. Enable it only when the host
   probe above reports "fail-closed capable".

4. Restart Hermes yourself when you are ready.
NEXT
