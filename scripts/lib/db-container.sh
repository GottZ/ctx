# shellcheck shell=bash
# db-container.sh — fail-closed resolution of the ctx database container.
#
# Sourced by the operator scripts (state.sh, test.sh, eval-temporal.sh,
# backup.sh, coverage.sh, scripts/archive-legacy-topic-maps.sh). They hand a
# DB password to `docker exec` — a guessed container name would hand it to
# whatever answers to that name on this host (issue #40, bug 7).
# break-glass.sh keeps its own copy of the same chain, so that it stays
# runnable on its own.
#
#   ctx_resolve_db_container COMPOSE_FILE
#     CTX_DB_CONTAINER → running compose service `db` → return 2
#
# Sets DB_CONTAINER on success; on failure prints the reason to stderr and
# returns 2. Callers end with `|| exit 2`. Passwords go to docker by name
# (`PGPASSWORD=… docker exec -e PGPASSWORD`), never as a value on the docker
# command line, where every local user can read them from the process list.
ctx_resolve_db_container() {
  local compose_file="$1" out
  DB_CONTAINER="${CTX_DB_CONTAINER:-}"
  if [[ -z "$DB_CONTAINER" ]]; then
    # No `| head -1`: under pipefail an early-closing head can SIGPIPE the
    # producer and turn a hit into a failure.
    out="$(docker compose -f "$compose_file" ps -q db 2>/dev/null)" || out=""
    DB_CONTAINER="${out%%$'\n'*}"
  fi
  if [[ -z "$DB_CONTAINER" ]]; then
    {
      echo "no running compose service 'db' found via $compose_file."
      echo "Set CTX_DB_CONTAINER to the database container — no name is guessed,"
      echo "the DB password would go to whichever container answers to it."
    } >&2
    return 2
  fi
}
