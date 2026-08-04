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
set -euo pipefail
cd "$(dirname "$0")"; set -a; source .env; set +a
# DB container resolution: env → compose → literal (issue #19); set -e-safe forms.
DB_CONTAINER="${CTX_DB_CONTAINER:-}"
if [[ -z "$DB_CONTAINER" ]]; then
  DB_CONTAINER="$(docker compose -f docker-compose.yml ps -q db 2>/dev/null | head -1)"
fi
[[ -n "$DB_CONTAINER" ]] || DB_CONTAINER="n8n-db-1"
case "${1:?usage: secret <name> [scope] | reset-settings [key]}" in
  secret)
    # replace(…, E'\n','') strips the MIME line wraps from encode(): PG
    # wraps base64 every 76 chars (RFC 2045) — without replace, every
    # realistic provider key arrives multi-line. The decrypt mode reads
    # stdin to EOF and strips CR/LF anyway (belt and braces), but the
    # record should leave psql in one piece.
    docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" "$DB_CONTAINER" \
      psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" -At -c \
      "SELECT replace(encode(nonce,'base64'), E'\n', '')
              ||':'|| replace(encode(ciphertext,'base64'), E'\n', '')
              ||':'|| name ||':'|| scope
         FROM context_secrets WHERE name='${2:?name}' AND scope='${3:-_global}'" \
    | docker run --rm -i -e CTX_SECRETS_KEY -e CTX_SECRETS_KEY_PREV n8n-ctx -secret-decrypt
    ;;
  reset-settings)
    # Delete override(s) — propagates via NOTIFY immediately, or at next
    # boot. Audit rows come from the 051 DB trigger (api_key_id NULL,
    # metadata.via='sql') — even the most drastic intervention leaves
    # history, not just NOTIFYs.
    WHERE="scope='_global'"; [ -n "${2:-}" ] && WHERE="$WHERE AND key='$2'"
    docker exec -e PGPASSWORD="$CONTEXT_DB_PASSWORD" "$DB_CONTAINER" \
      psql -U "$CONTEXT_DB_USER" -d "$CONTEXT_DB" -c "DELETE FROM context_settings WHERE $WHERE;"
    ;;
  *)
    echo "usage: $0 secret <name> [scope] | reset-settings [key]" >&2
    exit 1
    ;;
esac
