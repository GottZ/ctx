#!/usr/bin/env bash
# Install the ctx_checkpoint memory provider into a Hermes home directory.
#
# Copies the whole plugin directory (umbrella, memory/ engine, skills/, cli.py) to
# $HERMES_HOME/plugins/ctx_checkpoint/ and reports which operating mode the host
# supports. Never edits config.yaml and never
# restarts anything — config changes and process lifecycle stay with the
# operator.
#
# Usage:
#   install.sh [--hermes-home PATH] [--hermes-src PATH] [--dry-run]
#
#   --hermes-home PATH  Hermes home (default: $HERMES_HOME, then /opt/data if
#                       it holds a config.yaml, then ~/.hermes)
#   --hermes-src PATH   Hermes source checkout/installation to probe for the
#                       fail-closed checkpoint contract (optional). Reports
#                       the host's PRE_COMPRESS_CHECKPOINT_API_VERSION; v2 or
#                       higher is fail-closed capable, v1 is the historical
#                       best-effort hook. Also reports whether the host binds
#                       plugin context engines to the gate (the pending
#                       context-engine follow-up, compacts_outside_compress).
#   --dry-run           Show what would happen without writing anything

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PLUGIN_NAME="ctx_checkpoint"
HERMES_HOME_ARG=""
HERMES_SRC=""
DRY_RUN=0

while [ $# -gt 0 ]; do
    case "$1" in
        --hermes-home) HERMES_HOME_ARG="$2"; shift 2 ;;
        --hermes-src)  HERMES_SRC="$2"; shift 2 ;;
        --dry-run)     DRY_RUN=1; shift ;;
        -h|--help)     sed -n '2,22p' "$0"; exit 0 ;;
        *) echo "unknown argument: $1" >&2; exit 2 ;;
    esac
done

PLUGIN_SRC="${SCRIPT_DIR}/../plugin/${PLUGIN_NAME}"

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
DEST="${HOME_DIR}/plugins/${PLUGIN_NAME}"

echo "Hermes home:    ${HOME_DIR}"
echo "Plugin source:  ${PLUGIN_SRC}"
echo "Plugin dest:    ${DEST}"

if [ ! -d "$HOME_DIR" ]; then
    echo "Hermes home ${HOME_DIR} does not exist — pass --hermes-home." >&2
    exit 1
fi

# Backups are dot-prefixed: Hermes skips plugin directories starting with "." or "_",
# but lists every other one under plugins/ as a memory provider of its own — an old
# <plugin>.bak-* copy showed up next to the real provider (ctx_checkpoint 0.6.1).
for legacy in "${HOME_DIR}/plugins/${PLUGIN_NAME}.bak-"*; do
    [ -d "$legacy" ] || continue
    echo "note: ${legacy} is an old-style backup that Hermes lists as a memory provider;" >&2
    echo "      delete it or rename it to start with a dot." >&2
done

# "<uid> <gid> <octal mode>" of a path, through symbolic links (GNU stat, then BSD stat).
owner_mode() {
    stat -L -c '%u %g %a' "$1" 2>/dev/null || stat -L -f '%u %g %Lp' "$1"
}

# Can the account <uid> <gid> enter and list the directory <path>? Mode bits only: the owner,
# then the group (primary group only), then the others; uid 0 passes.
reachable_for() {
    local path="$1" uid="$2" gid="$3" o_uid o_gid mode bits
    [ "$uid" -eq 0 ] && return 0
    read -r o_uid o_gid mode <<<"$(owner_mode "$path")"
    mode=$((8#$mode))
    if [ "$o_uid" -eq "$uid" ]; then bits=$(((mode >> 6) & 5))
    elif [ "$o_gid" -eq "$gid" ]; then bits=$(((mode >> 3) & 5))
    else bits=$((mode & 5)); fi
    [ "$bits" -eq 5 ]
}

if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would install plugin version $(cat "${PLUGIN_SRC}/VERSION")"
else
    PLUGINS_DIR="${HOME_DIR}/plugins"
    BACKUP=""
    STAGE=""
    # On any exit: drop the staging directory, and put a running install moved to the backup
    # back in place when the new copy did not get there — never leave the home without one.
    finish() {
        [ -n "$STAGE" ] && rm -rf "$STAGE"
        if [ -n "$BACKUP" ] && [ -d "$BACKUP" ] && [ ! -e "$DEST" ] && [ ! -L "$DEST" ]; then
            mv "$BACKUP" "$DEST" && echo "the new copy did not get into place — the running install is back at ${DEST}" >&2
        fi
    }
    trap finish EXIT
    # Readable for the account Hermes runs as, whatever the installing shell's umask or the
    # checkout's modes (a sudo or elevated shell often has 077): Hermes skips a plugin folder
    # it cannot read, and with compression.checkpoint_required every compaction then fails.
    # Everything in the plugin source directory becomes readable for every account — keep
    # nothing private there; the ctx key belongs in Hermes' .env.
    # The copy is built and its modes are set in a private staging directory first, before the
    # running install is touched; then it is moved into place. plugins/ can be writable by the
    # Hermes account, and chmod -R follows a symbolic link given as its operand, so no command
    # here changes a path below plugins/ but the two moves.
    STAGE="$(mktemp -d)"
    (umask 022 && cp -R "$PLUGIN_SRC" "${STAGE}/${PLUGIN_NAME}")
    find "${STAGE}/${PLUGIN_NAME}" -name __pycache__ -type d -prune -exec rm -rf {} + 2>/dev/null || true
    rm -rf "${STAGE}/${PLUGIN_NAME}/.ruff_cache"
    chmod -R u+rwX,go+rX "${STAGE}/${PLUGIN_NAME}"

    # The account Hermes runs as is taken to be the owner of the Hermes home. A plugins/ this
    # script did not create is not changed: an earlier elevated install can have left it
    # private, and it may belong to another account — stop and name the fix instead.
    (umask 022 && mkdir -p "$PLUGINS_DIR")
    read -r HERMES_UID HERMES_GID _ <<<"$(owner_mode "$HOME_DIR")"
    for dir in "$HOME_DIR" "$PLUGINS_DIR"; do
        if ! reachable_for "$dir" "$HERMES_UID" "$HERMES_GID"; then
            echo "${dir} cannot be entered by uid ${HERMES_UID} (the owner of ${HOME_DIR}, taken as the" >&2
            echo "account Hermes runs as), so Hermes could not load the plugin. Nothing was changed." >&2
            echo "Fix it and run this script again:  chmod go+rx '${dir}'" >&2
            exit 1
        fi
    done

    if [ -d "$DEST" ]; then
        BACKUP="${PLUGINS_DIR}/.${PLUGIN_NAME}.bak-$(date -u +%Y%m%dT%H%M%SZ)"
        echo "existing install found — backing up to ${BACKUP}"
        mv "$DEST" "$BACKUP"
    fi
    if [ -e "$DEST" ] || [ -L "$DEST" ]; then
        echo "${DEST} exists and is not a directory (or appeared just now); not replacing it." >&2
        exit 1
    fi
    if mv --version >/dev/null 2>&1; then
        mv -T "${STAGE}/${PLUGIN_NAME}" "$DEST"   # GNU: never into a directory placed at $DEST
    else
        mv "${STAGE}/${PLUGIN_NAME}" "$DEST"
    fi
    if [ ! -f "${DEST}/VERSION" ] || ! reachable_for "$DEST" "$HERMES_UID" "$HERMES_GID"; then
        echo "${DEST} is not in place or cannot be entered by uid ${HERMES_UID}; check it before restarting Hermes." >&2
        exit 1
    fi
    echo "installed plugin version $(cat "${DEST}/VERSION") (the path is open to uid ${HERMES_UID} by its mode bits)"
fi

# ---------------------------------------------------------------------------
# Host capability probe: does this Hermes know the fail-closed contract?
# ---------------------------------------------------------------------------
#
# The contract is versioned: v1 is the implicit historical best-effort hook
# every provider is already on, v2 is the opt-in fail-closed checkpoint
# contract (upstream since 2026-08-25, merged as NousResearch/hermes-agent
# #94639). Only v2 or higher can honour compression.checkpoint_required.
#
# A second probe covers the context-engine follow-up (branch
# feat/context-engine-compaction-authority, PR pending upstream). The merged
# contract binds the built-in compaction paths, micro-compaction and the codex
# app-server mode; a plugin context engine that compacts outside compress() is
# bound only once the host carries compacts_outside_compress.
MODE="unknown (no --hermes-src given)"
ENGINE_BINDING="unknown (no --hermes-src given)"
if [ -n "$HERMES_SRC" ]; then
    PROVIDER_PY="${HERMES_SRC}/agent/memory_provider.py"
    HOST_API=""
    if [ -f "$PROVIDER_PY" ]; then
        HOST_API="$(sed -n \
            's/^[[:space:]]*PRE_COMPRESS_CHECKPOINT_API_VERSION[[:space:]]*=[[:space:]]*\([0-9][0-9]*\).*/\1/p' \
            "$PROVIDER_PY")"
        HOST_API="${HOST_API%%$'\n'*}"   # first match only
    fi
    if [ -z "$HOST_API" ]; then
        MODE="stock host older than the upstream merge (best-effort only)"
    elif [ "$HOST_API" -ge 2 ]; then
        MODE="fail-closed capable (checkpoint contract v${HOST_API})"
    else
        MODE="NOT fail-closed capable (checkpoint contract v${HOST_API}, needs v2+)"
    fi

    ENGINE_PY="${HERMES_SRC}/agent/context_engine.py"
    ENGINE_HITS=0
    if [ -f "$ENGINE_PY" ]; then
        ENGINE_HITS="$(grep -c compacts_outside_compress "$ENGINE_PY" || true)"
        ENGINE_HITS="${ENGINE_HITS:-0}"
    fi
    if [ "$ENGINE_HITS" -ge 1 ]; then
        ENGINE_BINDING="present (compacts_outside_compress)"
    else
        ENGINE_BINDING="absent (host predates the context-engine follow-up — a plugin context engine can compact past the gate)"
    fi
fi
echo "Host mode:      ${MODE}"
echo "Engine binding: ${ENGINE_BINDING}"

cat <<'NEXT'

Next steps (manual, operator-owned):

Steps 1 and 2 are what `hermes memory setup ctx_checkpoint` does
interactively (plugin 0.5.0+); the manual equivalent:

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

3. Fail-closed gate — ONLY on a host carrying the upstream checkpoint
   contract (API v2; any Hermes release cut after 2026-08-25, or main
   at/after 1ee524f77d):
     compression:
       checkpoint_required: true
   WARNING: on a Hermes older than that merge this key is silently ignored.
   Compaction then keeps running WITHOUT a guaranteed checkpoint even
   though the config suggests otherwise. Enable it only when the host
   probe above reports "fail-closed capable".
   With "Engine binding: absent" the gate covers the built-in compaction
   paths only: if you run a PLUGIN CONTEXT ENGINE, compaction it performs
   outside compress() (from on_turn_complete() or its own scheduler) never
   reaches the checkpoint, so checkpoint_required is not fully fail-closed
   on that host. Hosts using the built-in compressor are unaffected.
   NOTE: an armed gate trades availability for the guarantee — if the
   checkpoint provider is unavailable, the session HALTS instead of
   compacting unarchived context.

4. Restart Hermes yourself when you are ready.
NEXT
