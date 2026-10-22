#!/usr/bin/env bash
# break-glass.sh — host-side secret extraction + settings factory reset.
#
# Extraction runs through the ctxd binary's -secret-decrypt mode (openssl
# enc cannot do AES-GCM — "AEAD ciphers not supported", OpenSSL 3.6.2) and
# needs ONLY env + stdin: it works via `docker run` even when the ctx
# container itself crash-loops. Master key comes from .env
# (CTX_SECRETS_KEY, optionally CTX_SECRETS_KEY_PREV during rotation).
#
# Usage:  ./break-glass.sh secret <name> [scope]
#         ./break-glass.sh reset-settings [key]
#
# Every resolution stage fails closed with exit 2. The script hands the DB
# password to `docker exec` and the master key to `docker run` — a guessed
# container or image name would hand them to whatever answers to that name
# on this host.
#   DB container: CTX_DB_CONTAINER → running compose service `db`     → exit 2
#   ctx image:    CTX_IMAGE → image of the compose service `ctx` container
#                 (any state: a crash-looping or exited one counts)   → exit 2
# Both secrets travel by name (`-e VAR`), never as a value on the docker
# command line, where every local user can read them from the process list.
# Arguments reach SQL only as psql variables (:'name'), never spliced in.
set -euo pipefail
cd "$(dirname "$0")"; set -a; source .env; set +a

usage() { echo "usage: $0 secret <name> [scope] | reset-settings [key]" >&2; exit 1; }
die()   { echo "break-glass: $*" >&2; exit 2; }

# First line of a command's stdout, or empty when it fails or prints
# nothing. No `| head -1`: under pipefail an early-closing head can SIGPIPE
# the producer and turn a hit into a failure.
first_line() {
  local out
  out="$("$@" 2>/dev/null)" || out=""
  printf '%s' "${out%%$'\n'*}"
}

resolve_db_container() {
  DB_CONTAINER="${CTX_DB_CONTAINER:-}"
  if [[ -z "$DB_CONTAINER" ]]; then
    DB_CONTAINER="$(first_line docker compose -f docker-compose.yml ps -q db)"
  fi
  [[ -n "$DB_CONTAINER" ]] || die "no running compose service 'db' found." \
    "Set CTX_DB_CONTAINER to the database container — no name is guessed," \
    "the DB password would go to whichever container answers to it."
}

resolve_image() {
  IMAGE="${CTX_IMAGE:-}"
  if [[ -z "$IMAGE" ]]; then
    local cid
    cid="$(first_line docker compose -f docker-compose.yml ps -a -q ctx)"
    if [[ -n "$cid" ]]; then
      IMAGE="$(first_line docker inspect --format '{{.Image}}' "$cid")"
    fi
  fi
  [[ -n "$IMAGE" ]] || die "no container of compose service 'ctx' found." \
    "Set CTX_IMAGE to the ctx image (with a build: service compose names it" \
    "<project>-ctx) — no name is guessed, the master key would go to" \
    "whichever image answers to it."
}

[[ $# -ge 1 ]] || usage
case "$1" in
  secret)
    [[ -n "${2:-}" ]] || usage
    resolve_db_container
    resolve_image
    # replace(…, E'\n','') strips the MIME line wraps from encode(): PG
    # wraps base64 every 76 chars (RFC 2045) — without replace, every
    # realistic provider key arrives multi-line. The decrypt mode reads
    # stdin to EOF and strips CR/LF anyway (belt and braces), but the
    # record should leave psql in one piece. psql interpolates :'name'
    # only in script input, not in -c — hence the heredoc on stdin.
    PGPASSWORD="$CONTEXT_DB_PASSWORD" docker exec -i -e PGPASSWORD "$DB_CONTAINER" \
      psql -X -At -v ON_ERROR_STOP=1 -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
        -v name="$2" -v scope="${3:-_global}" <<'SQL' \
    | docker run --rm -i -e CTX_SECRETS_KEY -e CTX_SECRETS_KEY_PREV "$IMAGE" -secret-decrypt
SELECT replace(encode(nonce,'base64'), E'\n', '')
       ||':'|| replace(encode(ciphertext,'base64'), E'\n', '')
       ||':'|| name ||':'|| scope
  FROM context_secrets WHERE name = :'name' AND scope = :'scope';
SQL
    ;;
  reset-settings)
    resolve_db_container
    # Delete override(s) — propagates via NOTIFY immediately, or at next
    # boot. Audit rows come from the 051 DB trigger (api_key_id NULL,
    # metadata.via='sql') — even the most drastic intervention leaves
    # history, not just NOTIFYs. An empty key means all _global overrides.
    PGPASSWORD="$CONTEXT_DB_PASSWORD" docker exec -i -e PGPASSWORD "$DB_CONTAINER" \
      psql -X -v ON_ERROR_STOP=1 -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" \
        -v key="${2:-}" <<'SQL'
DELETE FROM context_settings
 WHERE scope = '_global' AND (:'key' = '' OR key = :'key');
SQL
    ;;
  *)
    usage
    ;;
esac
