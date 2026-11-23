#!/usr/bin/env bash
# The nightly integration run on the production database image (ci.yml job
# integration-production-image, issue #514). Every check of that job lives
# here, so each step runs locally exactly as on the runner:
#
#   bash .github/scripts/production-image.sh matrix
#       the shard matrix as JSON (one line, for $GITHUB_OUTPUT)
#   bash .github/scripts/production-image.sh packages <shard>
#       the packages of a shard, space-separated
#   bash .github/scripts/production-image.sh record <events.log>
#       starts a background `docker events` recorder of the containers
#       Testcontainers creates (the reaper included, marked reaper=true)
#   bash .github/scripts/production-image.sh probe
#       starts the image once, logs version, JIT, planner input and collation,
#       and fails when the server has no JIT
#   bash .github/scripts/production-image.sh containers <events.log>
#       fails unless every container the tests started (the reaper aside) came
#       from $CTX_TEST_PG_IMAGE, and unless at least one did
#   bash .github/scripts/production-image.sh jit-tests <shard> <go-test.log>
#       fails when a test of the shard skipped for want of JIT, and in the
#       shard of internal/store when one of its JIT tests did not run
#
# Inputs (env): CTX_TEST_PG_IMAGE — the image internal/testdb starts and the
# one `probe` and `containers` hold the run to; GITHUB_STEP_SUMMARY (optional).
set -euo pipefail

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}

# The shards. One package per explicit shard, each a package whose suite takes
# minutes on the production image; `rest` is every other package whose
# integration tests start a database container, computed at run time, so a new
# such package always runs somewhere. A package named here that starts no
# container (renamed, moved, its tests gone) fails `packages` instead of
# silently dropping out. Timeouts per shard: the package budget (go test
# -timeout) and the job cap above it, as in the per-PR integration-tests job;
# the measurements behind them are in the job's comment in ci.yml.
SHARD_NAMES=(store handler oai events guard rest)
declare -A SHARD_PACKAGES=(
  [store]="internal/store"
  [handler]="internal/handler"
  [oai]="internal/oai"
  [events]="internal/events"
  [guard]="internal/guard internal/rrf"
)
declare -A SHARD_PKG_TIMEOUT=(
  [store]=30m [handler]=20m [oai]=20m [events]=20m [guard]=20m [rest]=20m
)
declare -A SHARD_JOB_TIMEOUT=(
  [store]=40 [handler]=30 [oai]=30 [events]=30 [guard]=35 [rest]=40
)

# The JIT tests: they need pg_jit_available(), which the CI image does not
# have, and skip there with the message below. The production image has JIT,
# so here a skip is a failure, and so is a test that does not run at all (a
# rename has to update this list). JIT_PACKAGE must be a package of exactly one
# explicit shard (check_table): the shard that holds it checks the list.
JIT_PACKAGE=internal/store
JIT_TESTS=(
  TestMigration177_JitOfAFirstCallDoesNotStick
  TestTransitionTablePlansDoNotKeepJit
  TestMigration180ReplansABackendThatKeptJit
)
JIT_SKIP_MESSAGE='pg_jit_available() is false'

# fail writes to stderr: a step that captures the output of `packages` or
# `matrix` must not swallow the reason.
fail() {
  echo "::error::production-image: $*" >&2
  exit 1
}

need_image() {
  [ -n "${CTX_TEST_PG_IMAGE:-}" ] || fail "CTX_TEST_PG_IMAGE is not set"
}

# container_packages lists, relative to the module, every package whose test
# binary under -tags=integration links internal/testdb or the Testcontainers
# postgres module, i.e. every package whose tests can start a database. One
# line per dependency of each test variant: a dependency recompiled for the
# test reads `<path> [<pkg>.test]`, so the path is the second field and the
# suffix a third.
container_packages() {
  (
    cd "$ROOT/go"
    mod=$(go list -m)
    go list -tags=integration -test \
      -f '{{if .ForTest}}{{range .Deps}}{{$.ForTest}} {{.}}{{"\n"}}{{end}}{{end}}' ./... |
      awk -v pfx="$mod/" -v testdb="$mod/internal/testdb" -v pg=github.com/testcontainers/testcontainers-go/modules/postgres \
        '$2 == testdb || $2 == pg { sub("^" pfx, "", $1); print $1 }' | sort -u
  )
}

# check_table fails when the shard table contradicts itself: a shard with
# packages that SHARD_NAMES (and so the matrix) leaves out would take its
# packages out of `rest` and run them nowhere, and a JIT_PACKAGE in no explicit
# shard would leave JIT_TESTS checked nowhere.
check_table() {
  local s jit=0
  for s in "${!SHARD_PACKAGES[@]}"; do
    [[ " ${SHARD_NAMES[*]} " == *" $s "* ]] || fail "shard $s has packages but is not in SHARD_NAMES"
    [[ " ${SHARD_PACKAGES[$s]} " != *" $JIT_PACKAGE "* ]] || jit=$((jit + 1))
  done
  [ "$jit" = 1 ] || fail "JIT_PACKAGE $JIT_PACKAGE is in $jit explicit shards, not in exactly one"
  for s in "${SHARD_NAMES[@]}"; do
    [ "$s" = rest ] || [ -n "${SHARD_PACKAGES[$s]:-}" ] || fail "shard $s names no package"
    [ -n "${SHARD_PKG_TIMEOUT[$s]:-}" ] && [ -n "${SHARD_JOB_TIMEOUT[$s]:-}" ] || fail "shard $s has no timeouts"
  done
  [[ " ${SHARD_NAMES[*]} " == *" rest "* ]] || fail "SHARD_NAMES has no rest shard"
}

cmd_matrix() {
  local out="" s
  check_table
  for s in "${SHARD_NAMES[@]}"; do
    out+="${out:+,}{\"shard\":\"$s\",\"pkg_timeout\":\"${SHARD_PKG_TIMEOUT[$s]}\",\"job_timeout\":${SHARD_JOB_TIMEOUT[$s]}}"
  done
  echo "{\"include\":[$out]}"
}

cmd_packages() {
  local shard=${1:?shard}
  local all explicit="" s p
  check_table
  all=$(container_packages)
  [ -n "$all" ] || fail "go list found no package with database tests"
  for s in "${!SHARD_PACKAGES[@]}"; do
    for p in ${SHARD_PACKAGES[$s]}; do
      grep -qxF "$p" <<<"$all" || fail "shard $s names $p, which has no tests that start a database"
      ! grep -qxF "$p" <<<"$explicit" || fail "$p is in two shards"
      explicit+="$p"$'\n'
    done
  done
  if [ "$shard" = rest ]; then
    grep -vxF -f <(printf '%s' "$explicit") <<<"$all" | sed 's#^#./#' | tr '\n' ' ' | sed 's/ $//'
    echo
  elif [ -n "${SHARD_PACKAGES[$shard]:-}" ]; then
    for p in ${SHARD_PACKAGES[$shard]}; do printf './%s ' "$p"; done | sed 's/ $//'
    echo
  else
    fail "unknown shard $shard (known: ${SHARD_NAMES[*]})"
  fi
}

cmd_record() {
  local log=${1:?events log}
  # Only the lifecycle events: an exec event carries its command, line breaks
  # included, and would spill continuation lines into the log.
  nohup docker events \
    --filter type=container \
    --filter label=org.testcontainers=true \
    --filter event=create --filter event=start --filter event=die \
    --filter event=kill --filter event=oom \
    --format '{{.Time}} {{.Action}} {{.Actor.Attributes.name}} image={{.Actor.Attributes.image}} reaper={{index .Actor.Attributes "org.testcontainers.reaper"}} exitCode={{.Actor.Attributes.exitCode}} id={{.Actor.ID}}' \
    >"$log" 2>&1 &
  echo "recording container events to $log (pid $!)"
}

# The probe starts the image with what internal/testdb passes (runPostgres:
# the two flags, random_page_cost being testdb.DeploymentRandomPageCost, and
# the timescaledb-tune sizing, which only an image that runs timescaledb-tune
# reads) and reads what a test session sees. --pull never: it probes the image
# this job built, never a namesake from a registry.
cmd_probe() {
  need_image
  docker image inspect "$CTX_TEST_PG_IMAGE" --format "image $CTX_TEST_PG_IMAGE: id={{.Id}} size={{.Size}}" ||
    fail "image $CTX_TEST_PG_IMAGE does not exist locally"
  local cid i values jit
  cid=$(docker run -d --pull never \
    -e POSTGRES_USER=probe -e POSTGRES_PASSWORD=probe -e POSTGRES_DB=probe \
    -e TS_TUNE_MEMORY=2GB -e TS_TUNE_NUM_CPUS=2 \
    "$CTX_TEST_PG_IMAGE" -c timescaledb.max_background_workers=0 -c random_page_cost=4)
  # shellcheck disable=SC2064 # the id is fixed now
  trap "docker rm -f $cid >/dev/null 2>&1 || true" EXIT
  # TCP readiness: the entrypoint's first server (initdb) listens on no TCP
  # address, so this waits for the final server.
  for i in $(seq 1 120); do
    if docker exec "$cid" pg_isready -q -h 127.0.0.1 -U probe -d probe; then
      break
    fi
    [ "$i" -lt 120 ] || { docker logs --tail 40 "$cid" 2>&1 || true; fail "server of $CTX_TEST_PG_IMAGE not ready after 120 s"; }
    sleep 1
  done
  values=$(docker exec -e PGPASSWORD=probe "$cid" psql -X -A -t -F '=' -v ON_ERROR_STOP=1 -h 127.0.0.1 -U probe -d probe -c "
    SELECT 'server_version', current_setting('server_version')
    UNION ALL SELECT 'timescaledb', coalesce((SELECT default_version FROM pg_available_extensions WHERE name = 'timescaledb'), 'absent')
    UNION ALL SELECT 'vector', coalesce((SELECT default_version FROM pg_available_extensions WHERE name = 'vector'), 'absent')
    UNION ALL SELECT 'pg_jit_available', pg_jit_available()::text
    UNION ALL SELECT 'jit', current_setting('jit')
    UNION ALL SELECT 'random_page_cost', current_setting('random_page_cost')
    UNION ALL SELECT 'work_mem', current_setting('work_mem')
    UNION ALL SELECT 'effective_cache_size', current_setting('effective_cache_size')
    UNION ALL SELECT 'collation', datcollate FROM pg_database WHERE datname = current_database()")
  echo "$values"
  {
    echo "## Production database image"
    echo ""
    echo "\`$CTX_TEST_PG_IMAGE\`, as a test session sees it:"
    echo ""
    echo '```'
    echo "$values"
    echo '```'
  } >>"$SUMMARY"
  jit=$(awk -F= '$1 == "pg_jit_available" { print $2 }' <<<"$values")
  [ "$jit" = true ] || fail "$CTX_TEST_PG_IMAGE has no JIT (pg_jit_available() = '${jit}'): this is not the production image, and the run would test the CI image's settings"
  echo "ok: $CTX_TEST_PG_IMAGE has JIT"
}

cmd_containers() {
  need_image
  local log=${1:?events log} starts other
  [ -f "$log" ] || fail "no events log at $log"
  starts=$(awk '$2 == "start" && $5 != "reaper=true"' "$log")
  echo "containers the tests started (the reaper aside), by image:"
  awk '{ print "  " $4 }' <<<"$starts" | sort | uniq -c
  [ -n "$starts" ] || fail "the tests started no container (events log $log)"
  other=$(awk -v want="image=$CTX_TEST_PG_IMAGE" '$4 != want { print $4 }' <<<"$starts" | sort -u | tr '\n' ' ')
  [ -z "$other" ] || fail "containers from another image than $CTX_TEST_PG_IMAGE: ${other% }"
  echo "ok: every container the tests started came from $CTX_TEST_PG_IMAGE"
}

# Every shard: no test skipped for want of JIT. The shard that holds
# JIT_PACKAGE (read from the table; check_table keeps it in exactly one): each
# of JIT_TESTS passed or failed.
cmd_jit_tests() {
  local shard=${1:?shard} log=${2:?go test log} t bad=0
  [ -f "$log" ] || fail "no test log at $log"
  check_table
  [[ " ${SHARD_NAMES[*]} " == *" $shard "* ]] || fail "unknown shard $shard (known: ${SHARD_NAMES[*]})"
  if grep -qF "$JIT_SKIP_MESSAGE" "$log"; then
    echo "::error::a test skipped for want of JIT:"
    awk -v m="$JIT_SKIP_MESSAGE" 'index($0, m) && n++ < 20' "$log"
    bad=1
  fi
  if [[ " ${SHARD_PACKAGES[$shard]:-} " == *" $JIT_PACKAGE "* ]]; then
    for t in "${JIT_TESTS[@]}"; do
      if grep -qE "^[[:space:]]*--- SKIP: $t " "$log"; then
        echo "::error::$t skipped"
        bad=1
      elif grep -qE "^[[:space:]]*--- (PASS|FAIL): $t " "$log"; then
        echo "ran: $t"
      else
        echo "::error::$t did not run (renamed or removed? update JIT_TESTS in $0)"
        bad=1
      fi
    done
  else
    echo "shard $shard does not run $JIT_PACKAGE: its ${#JIT_TESTS[@]} JIT tests are checked in the shard that does"
  fi
  [ "$bad" = 0 ] || fail "JIT tests skipped or missing on ${CTX_TEST_PG_IMAGE:-the image under test}"
  echo "ok: no test skipped for want of JIT"
}

case "${1:-}" in
  matrix) cmd_matrix ;;
  packages) cmd_packages "${2:-}" ;;
  record) cmd_record "${2:-}" ;;
  probe) cmd_probe ;;
  containers) cmd_containers "${2:-}" ;;
  jit-tests) cmd_jit_tests "${2:-}" "${3:-}" ;;
  *)
    echo "usage: $0 matrix | packages <shard> | record <log> | probe | containers <log> | jit-tests <shard> <log>" >&2
    exit 2
    ;;
esac
