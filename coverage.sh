#!/usr/bin/env bash
#
# ctx — Go test coverage, merged across every suite that exercises the code
# Usage: ./coverage.sh [--short] [--system] [--html]
#
#   (default)  unit + integration tests (go test -tags=integration), with
#              -coverpkg=./... so a test counts every package it runs through.
#              Needs Go and Docker (testcontainers). ~30 min.
#   --short    unit tests only (go test -short), no Docker. A few minutes.
#   --system   additionally run test.sh and eval.sh against a ctxd and ctx
#              built with -cover. Needs the compose stack (db + ctx containers,
#              .env): the live database is cloned TimescaleDB-aware into a
#              throwaway database, an instrumented ctxd serves the clone on
#              127.0.0.1:$COV_PORT, the scripts run against it, and the clone,
#              the container and every copied .env are removed on exit —
#              including on failure. The live instance is never touched; the
#              run checks that afterwards and fails if it finds test traces.
#   --html     also write coverage.html (go tool cover -html).
#
# Reports statement coverage for the whole Go module and for the code that
# ships in the ctxd and ctx binaries (go list -deps), per source and merged:
# a block counts as covered when any source executed it. The web UI
# (Svelte/TypeScript) is not part of these figures.
#
# Output: $COV_OUT (default .gotmp/coverage/) — coverage.txt, the merged
# profile merged.out, per-source profiles and logs.
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GO_DIR="$SCRIPT_DIR/go"
OUT="${COV_OUT:-$SCRIPT_DIR/.gotmp/coverage}"

SHORT=false
SYSTEM=false
HTML=false
for arg in "$@"; do
  case "$arg" in
    --short) SHORT=true ;;
    --system) SYSTEM=true ;;
    --html) HTML=true ;;
    -h|--help) sed -n '2,27p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown argument: $arg (see --help)" >&2; exit 2 ;;
  esac
done

mkdir -p "$OUT"
# /tmp may be mounted noexec; go test builds its binaries under GOTMPDIR.
export GOTMPDIR="${GOTMPDIR:-$SCRIPT_DIR/.gotmp}"
mkdir -p "$GOTMPDIR"

log() { printf '[coverage] %s\n' "$*" >&2; }

# ---------------------------------------------------------------------------
# 1. go test (unit, or unit + integration)
# ---------------------------------------------------------------------------
GO_RC=0
if $SHORT; then
  log "go test -short (unit tests)"
  (cd "$GO_DIR" && go test -short -count=1 -coverpkg=./... \
    -coverprofile="$OUT/gotest.out" ./... >"$OUT/gotest.log" 2>&1) || GO_RC=$?
else
  log "go test -tags=integration (unit + integration, ~30 min)"
  (cd "$GO_DIR" && go test -tags=integration -count=1 -p 1 -timeout=25m -coverpkg=./... \
    -coverprofile="$OUT/gotest.out" ./... >"$OUT/gotest.log" 2>&1) || GO_RC=$?
fi
if [[ $GO_RC -ne 0 ]]; then
  log "go test failed (rc=$GO_RC) — coverage below is from a failing run, see $OUT/gotest.log"
  grep -E '^(FAIL|--- FAIL|panic:)' "$OUT/gotest.log" | head -20 >&2 || true
fi
PROFILES=("go test=$OUT/gotest.out")

# ---------------------------------------------------------------------------
# 2. --system: test.sh + eval.sh against an instrumented ctxd on a DB clone
# ---------------------------------------------------------------------------
SYS_RC=0
if $SYSTEM; then
  ENV_FILE="$SCRIPT_DIR/.env"
  [[ -f "$ENV_FILE" ]] || { log ".env not found — --system needs the compose stack"; exit 1; }
  set -a
  # shellcheck source=/dev/null
  source "$ENV_FILE"
  set +a

  DB_CONTAINER="${CTX_DB_CONTAINER:-n8n-db-1}"
  CTX_CONTAINER="${COV_CTX_CONTAINER:-ctx}"
  CLONE="${COV_DB:-ctx_cov}"
  NAME="${COV_CONTAINER:-ctx-cov}"
  PORT="${COV_PORT:-18080}"
  SOURCE_DB="${CONTEXT_DB:?CONTEXT_DB not set in .env}"
  DB_ADMIN="${POSTGRES_USER:?POSTGRES_USER not set in .env}"
  RUN_DIR="$OUT/system-run"
  COV_ENV="$OUT/ctx-cov.env"
  BIN_DIR="$OUT/bin"
  CTXD_COVER="$OUT/cover-ctxd"
  CLI_COVER="$OUT/cover-cli"
  # Every path removed below lives under $OUT; refuse to run with an empty one.
  [[ -n "$OUT" && "$OUT" != / ]] || { log "COV_OUT must not be empty or /"; exit 1; }

  psql_admin() { # psql_admin <db> <sql>
    docker exec -e PGOPTIONS="${PGOPTIONS:-}" "$DB_CONTAINER" \
      psql -U "$DB_ADMIN" -d "$1" -v ON_ERROR_STOP=1 -t -A -c "$2"
  }

  [[ "$CLONE" != "$SOURCE_DB" ]] || { log "COV_DB must differ from the live database"; exit 1; }
  if [[ "$(psql_admin postgres "SELECT count(*) FROM pg_database WHERE datname = '$CLONE'")" != 0 ]]; then
    log "database $CLONE already exists — a previous run did not clean up; drop it first"; exit 1
  fi
  if docker inspect "$NAME" >/dev/null 2>&1; then
    log "container $NAME already exists — remove it first"; exit 1
  fi

  CREATED_DB=false
  cleanup_system() {
    docker rm -f "$NAME" >/dev/null 2>&1 || true
    if $CREATED_DB; then
      psql_admin postgres "DROP DATABASE IF EXISTS $CLONE" >/dev/null 2>&1 \
        || log "WARNING: could not drop $CLONE — it holds a copy of live data, drop it by hand"
    fi
    rm -rf "$RUN_DIR" "$COV_ENV" "$BIN_DIR"
  }
  trap cleanup_system EXIT

  log "building ctxd and ctx with -cover"
  rm -rf "$BIN_DIR" "$CTXD_COVER" "$CLI_COVER"
  mkdir -p "$BIN_DIR" "$CTXD_COVER" "$CLI_COVER"
  chmod 777 "$CTXD_COVER" # ctxd runs as the image's non-root user
  (cd "$GO_DIR" && CGO_ENABLED=0 go build -cover -coverpkg=./... -o "$OUT/bin/ctxd" ./cmd/ctxd \
    && CGO_ENABLED=0 go build -cover -coverpkg=./... -o "$OUT/bin/ctx" ./cmd/ctx)

  # TimescaleDB-aware copy (pre/post restore, otherwise hypertables silently
  # de-convert — issue #43). No parallel maintenance workers: the HNSW index
  # build otherwise asks for more shared memory than the container's /dev/shm.
  log "cloning $SOURCE_DB into $CLONE"
  psql_admin postgres "CREATE DATABASE $CLONE OWNER ${CONTEXT_DB_USER:-context_user}" >/dev/null
  CREATED_DB=true
  psql_admin "$CLONE" "CREATE EXTENSION IF NOT EXISTS timescaledb" >/dev/null
  psql_admin "$CLONE" "SELECT timescaledb_pre_restore()" >/dev/null
  docker exec -e PGOPTIONS='-c max_parallel_maintenance_workers=0 -c maintenance_work_mem=1GB' \
    -e SRC="$SOURCE_DB" -e DST="$CLONE" -e U="$DB_ADMIN" "$DB_CONTAINER" \
    sh -c 'pg_dump -U "$U" -Fc "$SRC" 2>/dev/null | pg_restore -U "$U" -d "$DST" --exit-on-error'
  psql_admin "$CLONE" "SELECT timescaledb_post_restore()" >/dev/null
  n_src=$(psql_admin "$SOURCE_DB" "SELECT count(*) FROM context_blocks")
  n_dst=$(psql_admin "$CLONE" "SELECT count(*) FROM context_blocks")
  [[ "$n_src" == "$n_dst" ]] || { log "clone incomplete: $n_dst of $n_src blocks"; exit 1; }

  # Background loops stay off in the clone: they would execute code no test
  # asked for (and spend LLM time). distill is switched by a settings row,
  # which outranks the environment — so it is turned off in the clone's table.
  psql_admin "$CLONE" "UPDATE context_settings SET value = 'false' WHERE key IN ('distill.enabled', 'distill.ctx_enabled')" >/dev/null

  # The live container's own environment, pointed at the clone. Holds secrets:
  # owner-only, removed on exit.
  ( umask 077
    docker inspect "$CTX_CONTAINER" --format '{{range .Config.Env}}{{println .}}{{end}}' \
      | grep -v -E '^(CONTEXT_DB|CTX_DREAM_ENABLED|CTX_DISTILL_ENABLED|CTX_RECALL_CHECK_ENABLED|CTX_GRAPH_OVERVIEW_LABEL_ENABLED|GOCOVERDIR)=' \
      > "$COV_ENV"
    printf '%s\n' "CONTEXT_DB=$CLONE" CTX_DREAM_ENABLED=false CTX_DISTILL_ENABLED=false \
      CTX_RECALL_CHECK_ENABLED=false CTX_GRAPH_OVERVIEW_LABEL_ENABLED=false GOCOVERDIR=/cover >> "$COV_ENV" )
  IMAGE=$(docker inspect "$CTX_CONTAINER" --format '{{.Config.Image}}')
  NETWORK=$(docker inspect "$CTX_CONTAINER" --format '{{.HostConfig.NetworkMode}}')

  log "starting $NAME ($IMAGE, instrumented ctxd) on 127.0.0.1:$PORT"
  docker run -d --name "$NAME" --network "$NETWORK" -p "127.0.0.1:$PORT:8080" \
    --env-file "$COV_ENV" -v "$OUT/bin/ctxd:/ctx:ro" -v "$OUT/cover-ctxd:/cover" "$IMAGE" >/dev/null
  for _ in $(seq 1 60); do
    curl -fs -o /dev/null "http://127.0.0.1:$PORT/health" && break
    sleep 2
  done
  curl -fs -o /dev/null "http://127.0.0.1:$PORT/health" || { log "$NAME did not become healthy"; docker logs --tail 20 "$NAME" >&2; exit 1; }
  docker logs "$NAME" 2>&1 | grep -q "\"db\":\"$CLONE\"" || { log "$NAME is not serving $CLONE — aborting"; exit 1; }

  # test.sh / eval.sh source .env AFTER the environment, so overrides go into
  # a copy of .env in a run directory of their own.
  mkdir -p "$RUN_DIR"
  cp "$SCRIPT_DIR/test.sh" "$SCRIPT_DIR/eval.sh" "$RUN_DIR/"
  if [[ -f "$SCRIPT_DIR/.eval-baseline.json" ]]; then cp "$SCRIPT_DIR/.eval-baseline.json" "$RUN_DIR/"; fi
  for l in go .gocache backups; do
    if [[ -e "$SCRIPT_DIR/$l" ]]; then ln -sfn "$SCRIPT_DIR/$l" "$RUN_DIR/$l"; fi
  done
  ( umask 077
    cp "$ENV_FILE" "$RUN_DIR/.env"
    printf '\n# coverage.sh: point everything at the clone\n%s\n' \
      "WEBHOOK_BASE_URL=http://127.0.0.1:$PORT" "CTX_BASE_URL=http://127.0.0.1:$PORT" \
      "CONTEXT_DB=$CLONE" "CTX_DB_CONTAINER=$DB_CONTAINER" "GOCOVERDIR=$OUT/cover-cli" >> "$RUN_DIR/.env" )

  MARK=$(date -u +%Y-%m-%dT%H:%M:%SZ)
  log "test.sh --with-ollama"
  (cd "$RUN_DIR" && PATH="$OUT/bin:$PATH" bash test.sh --with-ollama) >"$OUT/test-sh.log" 2>&1 || SYS_RC=$?
  log "$(grep -E '=== Results' "$OUT/test-sh.log" || echo 'test.sh: no result line')"
  log "eval.sh"
  (cd "$RUN_DIR" && PATH="$OUT/bin:$PATH" bash eval.sh) >"$OUT/eval-sh.log" 2>&1 || SYS_RC=$?
  log "$(grep -E 'VERDICT' "$OUT/eval-sh.log" || echo 'eval.sh: no verdict line')"

  # The live database must show no trace of the run.
  leaked=$(psql_admin "$SOURCE_DB" "SELECT count(*) FROM context_blocks WHERE title LIKE '\_\_benchmark\_test\_%' AND created_at > '$MARK'")
  if [[ "$leaked" != 0 ]]; then
    log "LIVE CONTAMINATION: $leaked test block(s) in $SOURCE_DB since $MARK — investigate"; exit 1
  fi

  # Counters are written when ctxd exits normally.
  docker stop -t 60 "$NAME" >/dev/null
  [[ "$(docker inspect "$NAME" --format '{{.State.ExitCode}}')" == 0 ]] || log "WARNING: ctxd did not exit cleanly — counters may be missing"
  (cd "$GO_DIR" && go tool covdata textfmt -i="$OUT/cover-ctxd,$OUT/cover-cli" -o "$OUT/system.out")
  PROFILES+=("system=$OUT/system.out")
fi

# ---------------------------------------------------------------------------
# 3. merge and report
# ---------------------------------------------------------------------------
(cd "$GO_DIR" && go list -deps ./cmd/ctxd ./cmd/ctx) | grep '^github.com/GottZ/ctx' | sort -u > "$OUT/shipped.txt"

python3 - "$OUT/shipped.txt" "$OUT/merged.out" "${PROFILES[@]}" <<'PY' | tee "$OUT/coverage.txt"
import sys
from collections import defaultdict

MOD = "github.com/GottZ/ctx"
shipped = {l.strip() for l in open(sys.argv[1]) if l.strip()}
merged_path = sys.argv[2]

def load(path):
    blocks = {}
    with open(path) as f:
        for line in f:
            if line.startswith("mode:"):
                continue
            loc, n, c = line.rsplit(" ", 2)
            n, c = int(n), int(c)
            old = blocks.get(loc)
            blocks[loc] = (n, max(c, old[1]) if old else c)
    return blocks

profiles = []
for arg in sys.argv[3:]:
    name, path = arg.split("=", 1)
    profiles.append((name, load(path)))

universe = {}
for _, b in profiles:
    for loc, (n, _) in b.items():
        universe[loc] = n
union = {loc: 0 for loc in universe}
for _, b in profiles:
    for loc, (_, c) in b.items():
        if c > 0:
            union[loc] = 1

with open(merged_path, "w") as f:
    f.write("mode: set\n")
    for loc in sorted(universe):
        f.write(f"{loc} {universe[loc]} {union[loc]}\n")

pkg = lambda loc: loc.split(":", 1)[0].rsplit("/", 1)[0]

def pct(hit, keep):
    tot = cov = 0
    for loc, n in universe.items():
        if keep(pkg(loc)):
            tot += n
            cov += n if hit(loc) else 0
    return cov, tot

rows = [(name, (lambda b: lambda loc: b.get(loc, (0, 0))[1] > 0)(b)) for name, b in profiles]
if len(profiles) > 1:
    rows.append(("merged", lambda loc: union[loc] > 0))
print(f"{'source':<10} {'whole Go module':>24} {'shipped (ctxd + ctx)':>26}")
for name, hit in rows:
    c1, t1 = pct(hit, lambda p: p.startswith(MOD))
    c2, t2 = pct(hit, lambda p: p in shipped)
    print(f"{name:<10} {100*c1/t1:6.1f} % ({c1:>6}/{t1:<6}) {100*c2/t2:8.1f} % ({c2:>6}/{t2:<6})")

per = defaultdict(lambda: [0, 0])
for loc, n in universe.items():
    if pkg(loc) in shipped:
        per[pkg(loc)][1] += n
        per[pkg(loc)][0] += n if union[loc] else 0
print("\nshipped packages with the most uncovered statements:")
for p, (c, t) in sorted(per.items(), key=lambda kv: kv[1][0] - kv[1][1])[:10]:
    print(f"  {p[len(MOD)+1:]:<34} {100*c/t:5.1f} %  {t-c:>5} uncovered")
PY

if $HTML; then
  (cd "$GO_DIR" && go tool cover -html="$OUT/merged.out" -o "$OUT/coverage.html")
  log "HTML report: $OUT/coverage.html"
fi
log "report: $OUT/coverage.txt"

[[ $GO_RC -eq 0 && $SYS_RC -eq 0 ]]
