#!/usr/bin/env bash
# shellcheck disable=SC2086,SC2046,SC2015,SC2329 # package lists are word-split on purpose (set -f below); step functions are called through eval
#
# ctx — impact-tiered test gates (FOLGE-T)
# Usage: bash go/gates.sh <command> [--base <ref>] [--final] [--keep-going]
#
#   quick           build + vet -tags=integration over the module, golangci-lint on
#                   the touched packages, go test -short on the touched packages and
#                   on every package whose test binary depends on one of them
#                   (reverse dependents from `go list -test -deps`, transitive),
#                   plus the source-census tests (they read source files of other
#                   packages, no import edge leads to them)
#   wave [--final]  quick + go test -short -race on the touched packages and their
#                   direct importers + integration (-tags=integration -p 1, no
#                   -short) of the touched packages and of the trigger rows + the
#                   extra gates of the trigger table; --final picks the long forms
#                   (fuzzer 300 s, the full heap-gate matrix) and deadcode
#   full            the complete local list of FOLGE-P hardening 7
#                   (briefings/dispatch/dispatch-folge-p-haertung7.md, "Prüf-Regime"),
#                   united with whatever wave selects for this diff, so every stage
#                   is a subset of full
#   explain         changed files -> packages -> dependents -> chosen gates with the
#                   reason; runs nothing but git and go list (--stage quick|wave|full)
#   mutant <overlay.json> <test-regex> [pkg] [--race]
#                   one mutant (go test -overlay) against exactly the tests that must
#                   catch it: exit 0 = killed (red), 4 = survived, 5 = no red proof
#                   (build error, or the regex matched no test)
#
# Base: --base <ref> (default origin/root); the merge base with HEAD is compared
# with the working tree, untracked files count.
# Exit code: 0 green; 1/2/3 = the first red tier (quick/wave/full) — a red tier
# stops the run unless --keep-going; 4/5 mutant; 64 usage.
#
# The categories stay where they are (build tags, -short, env-gated measurement
# tests, packages); this script only selects. CI stays the authority: ci.yml runs
# the full matrix on every PR and the nightly with -race -covermode=atomic, and
# nothing here touches it. Steps marked "measurement" run locally only — CI never
# runs them (python conformance, fuzzer, heap gates, CPU gates -count=20).
#
# Shared build cache (convention, FOLGE-T decision 5): GOCACHE defaults to
# <main checkout>/.gotmp/gocache-shared when unset (content-addressed, safe to
# share); GOLANGCI_LINT_CACHE defaults to <worktree>/.gotmp/glci-gates — a shared
# lint cache gets poisoned by parallel worktrees (phantom issues pointing at
# deleted trees, 11 cases documented in .hooks/pre-commit); GOFLAGS gets -buildvcs=false and -trimpath (without -trimpath the
# compile action ID contains the package directory, so a second worktree
# recompiles every package of the module — measured in FOLGE-T: 35 % of the cold
# time instead of 4 %); CTX_GATES_TRIMPATH=0 leaves -trimpath out. GOTMPDIR and
# TMPDIR stay per wave and default to <worktree>/.gotmp/gates/ when unset.
# Logs: ${CTX_GATES_LOG:-$TMPDIR/gates}/<stage>-<UTC time>/.
#
# ctx — Your AI's save game. By GottZ (github.com/GottZ/ctx/graphs/contributors)

set -u
set -f # no globbing: package lists are split on purpose, paths carry no glob characters

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "$SCRIPT_DIR/.." && pwd)"
GO_DIR="$REPO/go"
MODULE="github.com/GottZ/ctx"

# ── trigger table: path glob (repo-relative, applies to non-test files) → extra gates
# Gate words: differential · embeddiff · chatheap · chatcpu · embedheap · streamheap · python · web ·
# integ:<pkg> (integration of that package) · short:<pkg> (-short of that package).
# A glob starting with ^ matches files in the repository root only. One row per
# line, comment = why.
TRIGGERS=(
  "go/internal/oai/parse*.go|differential"            # the chat parse: differential against the old parse + its fuzzer
  "go/internal/oai/jsonkeys*.go|differential embeddiff" # key scanner both parses are built on
  "go/internal/oai/embeddings_parse*.go|embeddiff"   # the embeddings parse: its own differential + fuzzer (W02-09) when present
  "go/internal/oai/chat*.go|chatheap chatcpu"         # chat route: heap over the route per form, CPU gates -count=20
  "go/internal/oai/budget.go|chatheap chatcpu streamheap" # the in-flight reservation the heap gates measure against
  "go/internal/oai/embed*.go|embedheap"               # embeddings route: its heap gate (W02-09) when present
  "go/internal/oai/chat_stream*.go|streamheap"        # the stream relay: work per frame and heap of streams at the frame cap (FOLGE-S)
  "go/internal/oai/rewrite*.go|streamheap"            # the chunk and answer rewrite (FOLGE-S, non-stream at the cap included)
  "go/internal/oai/relay*.go|streamheap"              # the relay buffer between rewrite and client
  "go/internal/oai/sse*.go|streamheap"                # the stream writer
  "go/internal/llm/stream*.go|streamheap"             # the relay reader (line buffer, chunk metadata)
  "go/internal/llm/passthrough*.go|streamheap"        # ChatRaw / ChatRawStream, the wire sites the gates measure
  "go/internal/oai/*.go|python integ:./cmd/ctxd/"     # wire behaviour the Python SDK sees; cmd/ctxd mounts /v1
  "go/cmd/ctxd/*.go|python"                           # the mount of /v1
  "docs/api.md|python"                                # the documented wire contract the harness checks
  "go/conformance/*|python short:./internal/oai/"     # the Python half of the harness; its JSON excerpts feed the Go half too
  "go/web/src/lib/api/__fixtures__/*|short:./internal/cli/" # cli/kanban_test.go reads the board fixture
  "^*.sh|short:./internal/config/"                    # retireddocs_test.go globs the scripts in the repository root
  ".env.example|short:./internal/config/"             # the env template the config tests pin
  "go/internal/handler/*|integ:./internal/handler/ integ:./internal/store/" # handlers write through the store
  "go/internal/store/*|integ:./internal/handler/ integ:./internal/store/"   # the store and its callers
  "go/migrations/*|integ:./internal/handler/ integ:./internal/store/"       # schema: store + handler suites
  "go/internal/dispatch/*|integ:./internal/dispatch/" # the dispatcher's own integration tests (CI shard rest)
  "go/internal/config/*|integ:./internal/config/ integ:./cmd/ctxd/"         # settings and the daemon that loads them
  "go/web/*|web"                                      # admin SPA: svelte-check + vitest (e2e stays in CI's container)
)
# Changed test files select a gate when they define it (a measurement test that is
# skipped in -short would otherwise never run): see test_file_gates below.

# The integration packages of the FOLGE-P hardening 7 list — part of full.
FULL_INTEG=(./internal/oai/ ./internal/config/ ./cmd/ctxd/ ./internal/handler/)
CPU_GATES='^(TestParseCPUAgainstTheBase|TestParseCPUIsLinearInASharedPrefix|TestRefusalOfASharedInvalidPrefixIsLinearOverTheRoute)$'
CHAT_HEAP_FILE="$GO_DIR/internal/oai/chat_parse_mem_test.go"
EMBED_HEAP_FILE="$GO_DIR/internal/oai/embed_scan_mem_test.go"
EMBED_DIFF_FILE="$GO_DIR/internal/oai/embed_parse_differential_test.go"
EMBED_HEAP_FORMS="text lt assign" # embed_scan_mem_test.go (W02-09): the unit map of embedScanForm
STREAM_HEAP_FILE="$GO_DIR/internal/oai/folge_s_frame_test.go"
# folge_s_frame_test.go (FOLGE-S): S1 work per relayed frame, S4 the non-stream
# answer at its cap, S3 the house metric with 1/4-MiB frames, S2 the live heap
# of n concurrent streams — the heap gates run one case per child process
# under GOMEMLIMIT=200MiB themselves
STREAM_HEAP_GATES='^(TestStreamFrameWorkPerFrame|TestNonStreamWorkAtTheCap|TestStreamHeapGateLargeFrames|TestStreamLiveHeapConcurrent)$'

usage() { sed -n '3,30p' "${BASH_SOURCE[0]}" | sed 's/^# \{0,1\}//' >&2; exit 64; }
die() { echo "gates: $*" >&2; exit 64; }
now() { date -u +%FT%TZ; }
load() { uptime | sed 's/.*load average: *//'; }

# ── arguments
CMD="${1:-}"; [ -n "$CMD" ] || usage; shift
BASE_REF="origin/root"; FINAL=0; KEEP=0; STAGE=""; RACE_MUT=0; POS=()
while [ $# -gt 0 ]; do
  case "$1" in
    --base) [ $# -ge 2 ] || usage; BASE_REF="$2"; shift 2 ;;
    --final) FINAL=1; shift ;;
    --keep-going) KEEP=1; shift ;;
    --stage) [ $# -ge 2 ] || usage; STAGE="$2"; shift 2 ;;
    --race) RACE_MUT=1; shift ;;
    -h|--help) usage ;;
    *) POS+=("$1"); shift ;;
  esac
done
case "$CMD" in
  quick|wave|full) [ ${#POS[@]} -eq 0 ] || usage; STAGE="$CMD" ;;
  explain) [ ${#POS[@]} -eq 0 ] || usage; STAGE="${STAGE:-wave}" ;;
  mutant) [ ${#POS[@]} -ge 2 ] && [ ${#POS[@]} -le 3 ] || usage ;;
  *) usage ;;
esac
case "$STAGE" in ""|quick|wave|full) ;; *) die "unknown stage '$STAGE'" ;; esac

# ── environment (shared cache by convention, per-wave temp dirs)
COMMON="$(git -C "$REPO" rev-parse --path-format=absolute --git-common-dir)" || die "not a git checkout: $REPO"
SHARED="$(dirname "$COMMON")/.gotmp"
export GOCACHE="${GOCACHE:-$SHARED/gocache-shared}"
export GOLANGCI_LINT_CACHE="${GOLANGCI_LINT_CACHE:-$REPO/.gotmp/glci-gates}"
export GOTMPDIR="${GOTMPDIR:-$REPO/.gotmp/gates/gotmp}"
export TMPDIR="${TMPDIR:-$REPO/.gotmp/gates/tmp}"
GOFLAGS="${GOFLAGS:-}"
case " $GOFLAGS " in *" -buildvcs="*) ;; *) GOFLAGS="$GOFLAGS -buildvcs=false" ;; esac
if [ "${CTX_GATES_TRIMPATH:-1}" != 0 ]; then
  case " $GOFLAGS " in *" -trimpath "*) ;; *) GOFLAGS="$GOFLAGS -trimpath" ;; esac
fi
export GOFLAGS="${GOFLAGS# }"
mkdir -p "$GOCACHE" "$GOLANGCI_LINT_CACHE" "$GOTMPDIR" "$TMPDIR"
LOG_ROOT="${CTX_GATES_LOG:-$TMPDIR/gates}"
RUN_DIR="$LOG_ROOT/$CMD-$(date -u +%Y%m%dT%H%M%SZ)"
mkdir -p "$RUN_DIR"
echo "gates: env GOCACHE=$GOCACHE GOLANGCI_LINT_CACHE=$GOLANGCI_LINT_CACHE GOTMPDIR=$GOTMPDIR TMPDIR=$TMPDIR GOFLAGS='$GOFLAGS' log=$RUN_DIR"

# ── mutant: no diff analysis, one go test with an overlay
if [ "$CMD" = mutant ]; then
  OV="${POS[0]}"; RX="${POS[1]}"; PKG="${POS[2]:-./internal/oai/}"
  [ -f "$OV" ] || die "overlay not found: $OV"
  OV="$(cd "$(dirname "$OV")" && pwd)/$(basename "$OV")"
  jq -e '.Replace | type == "object" and length > 0' "$OV" >/dev/null 2>&1 || die "overlay is not go's {\"Replace\":{...}} JSON: $OV"
  flags=(-short -count=1 -v -overlay "$OV" -run "$RX")
  [ "$RACE_MUT" = 1 ] && flags+=(-race)
  echo "gates: mutant start $(now) load $(load) overlay=$OV run=$RX pkg=$PKG race=$RACE_MUT"
  s=$(date +%s)
  (cd "$GO_DIR" && go test "${flags[@]}" "$PKG") > "$RUN_DIR/mutant.log" 2>&1; rc=$?
  d=$(( $(date +%s) - s ))
  fails=$(grep -c -- '^--- FAIL' "$RUN_DIR/mutant.log")
  passes=$(grep -c -- '^--- PASS' "$RUN_DIR/mutant.log")
  if grep -q '\[build failed\]\|\[setup failed\]' "$RUN_DIR/mutant.log"; then
    echo "gates: stufe=mutant result=NO-RED-PROOF reason='build failed — a build error is not a red proof' seconds=$d log=$RUN_DIR/mutant.log"; exit 5
  fi
  if [ "$rc" -eq 0 ] && [ "$passes" -eq 0 ]; then
    echo "gates: stufe=mutant result=NO-RED-PROOF reason='the regex matched no test' seconds=$d log=$RUN_DIR/mutant.log"; exit 5
  fi
  if [ "$rc" -ne 0 ] && [ "$fails" -eq 0 ] && ! grep -q 'panic: test timed out' "$RUN_DIR/mutant.log"; then
    echo "gates: stufe=mutant result=NO-RED-PROOF reason='go test failed without a failing test (bad regex, init panic, vet) — not a red proof' seconds=$d log=$RUN_DIR/mutant.log"; exit 5
  fi
  if [ "$rc" -eq 0 ]; then
    echo "gates: stufe=mutant result=SURVIVED passed=$passes seconds=$d log=$RUN_DIR/mutant.log"; exit 4
  fi
  echo "gates: stufe=mutant result=KILLED failed_tests=$fails passed=$passes seconds=$d end $(now) load $(load) log=$RUN_DIR/mutant.log"
  grep -- '^--- FAIL' "$RUN_DIR/mutant.log" | sed 's/^/  /' | head -20
  exit 0
fi

# ── changed files
MB="$(git -C "$REPO" merge-base "$BASE_REF" HEAD)" || die "no merge base with $BASE_REF"
mapfile -t CHANGED < <({ git -C "$REPO" diff --name-only --no-renames "$MB" --; git -C "$REPO" ls-files --others --exclude-standard; } | sort -u)

# ── package graph (go list, cached per tree state in LOG_ROOT)
graph_key="$({ git -C "$REPO" rev-parse HEAD; git -C "$REPO" diff HEAD -- '*.go' go/go.mod go/go.sum;
  git -C "$REPO" ls-files -z --others --exclude-standard -- '*.go' | xargs -0 -r cat; } | sha256sum | cut -c1-16)"
GRAPH="$LOG_ROOT/graph-$graph_key"
if [ ! -s "$GRAPH/deps" ] || [ ! -s "$GRAPH/plain" ] || [ ! -s "$GRAPH/udeps" ]; then
  mkdir -p "$GRAPH"
  if ! { (cd "$GO_DIR" && go list -tags=integration -f '{{.Dir}}|{{.ImportPath}}' ./...) > "$GRAPH/dirs.tmp" &&
    (cd "$GO_DIR" && go list -f '{{.ImportPath}}' ./...) > "$GRAPH/plain.tmp" &&
    (cd "$GO_DIR" && go list -tags=integration -f '{{.ImportPath}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./...) > "$GRAPH/imports.tmp" &&
    (cd "$GO_DIR" && go list -test -tags=integration -f '{{.ImportPath}}|{{join .Deps ","}}' ./...) > "$GRAPH/deps.tmp" &&
    (cd "$GO_DIR" && go list -f '{{.ImportPath}}|{{join .Imports " "}} {{join .TestImports " "}} {{join .XTestImports " "}}' ./...) > "$GRAPH/uimports.tmp" &&
    (cd "$GO_DIR" && go list -test -f '{{.ImportPath}}|{{join .Deps ","}}' ./...) > "$GRAPH/udeps.tmp"; }; then
    die "go list failed — the reverse dependencies cannot be determined (stop finding, see $GRAPH)"
  fi
  mv "$GRAPH/uimports.tmp" "$GRAPH/uimports"; mv "$GRAPH/udeps.tmp" "$GRAPH/udeps"; mv "$GRAPH/plain.tmp" "$GRAPH/plain"; mv "$GRAPH/dirs.tmp" "$GRAPH/dirs"; mv "$GRAPH/imports.tmp" "$GRAPH/imports"; mv "$GRAPH/deps.tmp" "$GRAPH/deps"
fi
declare -A DIR2PKG=()
while IFS='|' read -r d p; do DIR2PKG["$d"]="$p"; done < "$GRAPH/dirs"
ALL_PKGS=$(cut -d'|' -f2 "$GRAPH/dirs" | sort -u)
rel() { local p="$1"; [ "$p" = "$MODULE" ] && { echo "."; return; }; echo "./${p#"$MODULE"/}/"; }

# reverse dependents (transitive): every package whose test binary depends on one of
# $2… — $1 is the graph: deps (-tags=integration) or udeps (untagged)
rdeps_transitive() {
  local g="$GRAPH/$1"; shift
  awk -F'|' -v targets="$*" -v mod="$MODULE" '
    BEGIN { n = split(targets, t, " "); for (i = 1; i <= n; i++) want[t[i]] = 1 }
    {
      ip = $1; owner = ip
      if (ip ~ / \[/) { owner = ip; sub(/^.* \[/, "", owner); sub(/\]$/, "", owner) }
      sub(/\.test$/, "", owner); sub(/_test$/, "", owner)
      base = ip; sub(/ \[.*$/, "", base); sub(/\.test$/, "", base); sub(/_test$/, "", base)
      hit = (base in want)
      m = split($2, d, ",")
      for (i = 1; i <= m && !hit; i++) { x = d[i]; sub(/ \[.*$/, "", x); if (x in want) hit = 1 }
      if (hit && index(owner, mod) == 1) print owner
    }' "$g" | sort -u
}
# direct importers (production, test or external-test import) of $2…, plus $2… —
# $1 is the graph: imports (-tags=integration) or uimports (untagged)
rdeps_direct() {
  local g="$GRAPH/$1"; shift
  awk -F'|' -v targets="$*" '
    BEGIN { n = split(targets, t, " "); for (i = 1; i <= n; i++) { want[t[i]] = 1; print t[i] } }
    { m = split($2, d, " "); for (i = 1; i <= m; i++) if (d[i] in want) { print $1; break } }' "$g" | sort -u
}

file_tag() { [ -f "$REPO/$1" ] && sed -n '1,5{/^\/\/go:build/p}' "$REPO/$1" | head -1; }

# ── classification
declare -A SHORT_EXTRA=() RACE_TEST=() PKG_PROD=() PKG_TEST=() INTEG=() INTEG_WHY=() EXTRA=() EXTRA_WHY=() READERS=() READERS_INTEG=()
GO_CHANGED=0; NONGO_TEXT=(); UNMAPPED=(); LINT_ALL=0; DEADCODE_HIT=0; SELF_HIT=0; MODALL=0
add_extra() { EXTRA["$1"]=1; EXTRA_WHY["$1"]="${EXTRA_WHY[$1]:-}${EXTRA_WHY[$1]:+, }$2"; }
add_integ() { INTEG["$1"]=1; INTEG_WHY["$1"]="${INTEG_WHY[$1]:-}${INTEG_WHY[$1]:+, }$2"; }
test_file_gates() { # a changed test file selects the gate it defines
  local f="$1" tag; tag="$(file_tag "$f")"
  [ -f "$REPO/$f" ] || return 0
  case "$tag" in *pyconformance*) add_extra python "$f (pyconformance harness)" ;; esac
  case "$f" in *conformance*_test.go) add_extra python "$f (conformance harness)" ;; esac
  grep -q 'CTX_FOLGEP_FORM' "$REPO/$f" && add_extra chatheap "$f (defines the chat heap gate)"
  grep -q 'CTX_W0209_SCAN_FORM' "$REPO/$f" && add_extra embedheap "$f (defines the embeddings heap gate)"
  grep -q 'CTX_FOLGE_S_CHILD' "$REPO/$f" && add_extra streamheap "$f (defines the stream heap gates)"
  grep -q 'func TestParseCPU\|func TestRefusalOfASharedInvalidPrefixIsLinearOverTheRoute' "$REPO/$f" && add_extra chatcpu "$f (defines a chat CPU gate)"
  grep -q '^func FuzzChatParseDifferential\|^func TestChatParseDifferential' "$REPO/$f" && add_extra differential "$f (defines the differential)"
  grep -q '^func FuzzEmbedParseDifferential\|^func TestEmbedParseDifferential' "$REPO/$f" && add_extra embeddiff "$f (defines the embeddings differential)"
  return 0
}
for f in "${CHANGED[@]}"; do
  case "$f" in
    go/go.mod|go/go.sum) MODALL=1; GO_CHANGED=1; LINT_ALL=1 ;;
    go/.golangci.yml|.golangci.yml) LINT_ALL=1 ;;
    deadcode.sh|go/deadcode-allow.txt|go/deadcode-testonly-allow.txt) DEADCODE_HIT=1 ;;
    go/gates.sh) SELF_HIT=1 ;;
  esac
  if [[ "$f" == go/*.go ]]; then
    GO_CHANGED=1
    d="$REPO/$(dirname "$f")"; p="${DIR2PKG[$d]:-}"
    if [ -z "$p" ]; then UNMAPPED+=("$f"); continue; fi
    if [[ "$f" == *_test.go ]]; then
      PKG_TEST["$p"]=1; test_file_gates "$f"
      tag="$(file_tag "$f")"
      if [[ "$tag" == *race* ]] && [[ "$tag" != *'!race'* ]]; then RACE_TEST["$p"]="$f is race-tagged"; fi
      if [[ "$tag" == *integration* ]]; then add_integ "$(rel "$p")" "$f is integration-tagged"
      elif [ -f "$REPO/$f" ] && grep -q 'testing.Short()' "$REPO/$f"; then add_integ "$(rel "$p")" "$f skips parts under -short"; fi
    else
      PKG_PROD["$p"]=1
    fi
  elif [[ "$f" != *.go ]]; then
    case "$f" in *.png|*.svg|*.jpg|*.ico|*.woff*|.project) ;; *) NONGO_TEXT+=("$f") ;; esac
    # a non-Go file inside a package directory (testdata, //go:embed assets,
    # migrations) belongs to the nearest enclosing package: testdata → its tests,
    # anything else → the package itself (it may be embedded)
    if [[ "$f" == go/* ]] && [[ "$f" != go/web/src/* ]]; then
      d="$REPO/$(dirname "$f")"; p=""
      while [ "$d" != "$GO_DIR" ] && [ "$d" != "$REPO" ] && [ "$d" != / ]; do
        p="${DIR2PKG[$d]:-}"; [ -n "$p" ] && break; d="$(dirname "$d")"
      done
      if [ -n "$p" ]; then
        GO_CHANGED=1
        if [[ "$f" == */testdata/* ]]; then PKG_TEST["$p"]=1; else PKG_PROD["$p"]=1; fi
      fi
    fi
  fi
  if [[ "$f" != *_test.go ]]; then
    for row in "${TRIGGERS[@]}"; do
      glob="${row%%|*}"; gates="${row#*|}"
      if [[ "$glob" == ^* ]]; then glob="${glob#^}"; [[ "$f" == */* ]] && continue; fi
      # shellcheck disable=SC2053 # the glob is a pattern on purpose
      if [[ "$f" == $glob ]]; then
        for g in $gates; do
          case "$g" in
            integ:*) add_integ "${g#integ:}" "$f ~ $glob" ;;
            short:*) SHORT_EXTRA["${g#short:}"]="$f ~ $glob" ;;
            *) add_extra "$g" "$f ~ $glob" ;;
          esac
        done
      fi
    done
  fi
done
# readers: tests that open a changed non-Go file by an exact path literal
for f in "${NONGO_TEXT[@]}"; do
  b="$(basename "$f")"; r="${f#go/}"
  while IFS= read -r tf; do
    [ -n "$tf" ] || continue
    p="${DIR2PKG[$(dirname "$tf")]:-}"; [ -n "$p" ] || continue
    if [[ "$(sed -n '1,5{/^\/\/go:build/p}' "$tf")" == *integration* ]]; then READERS_INTEG["$p"]="${READERS_INTEG[$p]:-}${READERS_INTEG[$p]:+ }$f"
    else READERS["$p"]="${READERS[$p]:-}${READERS[$p]:+ }$f"; fi
  done < <(grep -rlE --include='*_test.go' "\"(\\.\\./)*(go/)?${r//./\\.}\"|\"${b//./\\.}\"" "$GO_DIR" 2>/dev/null)
done
for p in "${!READERS_INTEG[@]}"; do add_integ "$(rel "$p")" "its integration test reads ${READERS_INTEG[$p]}"; done

# ── package sets
PROD=$(printf '%s\n' "${!PKG_PROD[@]}" | sed '/^$/d' | sort -u)
TESTONLY=$(printf '%s\n' "${!PKG_TEST[@]}" | sed '/^$/d' | sort -u)
if [ "$MODALL" = 1 ] || [ ${#UNMAPPED[@]} -gt 0 ]; then SHORT_SET="$ALL_PKGS"; RACE_SET="$ALL_PKGS"
else
  # -short/-race from the untagged graph (what those runs compile), the
  # integration-only side from the tagged one
  SHORT_SET=$( { [ -n "$PROD" ] && rdeps_transitive udeps $PROD; printf '%s\n' $TESTONLY; printf '%s\n' "${!READERS[@]}"; } | sed '/^$/d' | sort -u)
  RACE_SET=$( { [ -n "$PROD" ] && rdeps_direct uimports $PROD; printf '%s\n' "${!RACE_TEST[@]}"; } | sed '/^$/d' | sort -u)
  # a touched package that exists only under -tags=integration (internal/testdb,
  # v1door) is exercised by its importers' integration tests; integration-only
  # importers of a touched package run in the integration step as well
  for p in $( [ -n "$PROD" ] && rdeps_transitive deps $PROD); do
    if ! grep -qFx "$p" "$GRAPH/plain"; then add_integ "$(rel "$p")" "integration-only reverse dependent"; fi
  done
  for t in $PROD; do
    grep -qFx "$t" "$GRAPH/plain" && continue
    for p in $(rdeps_direct imports "$t"); do [ "$p" = "$t" ] || add_integ "$(rel "$p")" "imports the integration-only package $(rel "$t")"; done
  done
fi
for p in $PROD; do add_integ "$(rel "$p")" "touched package"; done
for p in "${!SHORT_EXTRA[@]}"; do SHORT_SET=$(printf '%s\n' $SHORT_SET "$MODULE/${p#./}" | sed 's|/$||' | sort -u); done
# packages that exist only under -tags=integration cannot run untagged
TAGONLY=$(printf '%s\n' $SHORT_SET $RACE_SET | sed '/^$/d' | sort -u | grep -vFx -f "$GRAPH/plain")
for p in $TAGONLY; do add_integ "$(rel "$p")" "integration-only package among the affected"; done
SHORT_SET=$(printf '%s\n' $SHORT_SET | sed '/^$/d' | grep -Fx -f "$GRAPH/plain")
RACE_SET=$(printf '%s\n' $RACE_SET | sed '/^$/d' | grep -Fx -f "$GRAPH/plain")
# source-census tests: untagged tests that walk or parse source files of other
# packages without importing them (wire-site, marker, call-site, env-name fences)
# — no import edge leads to them, so they run on every Go change (census step)
CENSUS_FILES=$(grep -rlE --include='*_test.go' 'parser\.Parse(File|Dir)|filepath\.WalkDir|filepath\.Walk\(|os\.ReadDir|filepath\.Glob|go/build|packages\.Load|os\.ReadFile\([^)]*\.go"' "$GO_DIR" 2>/dev/null |
  while IFS= read -r cf; do sed -n '1,5{/^\/\/go:build/p}' "$cf" | grep -q integration || echo "$cf"; done)
CENSUS_PKGS=$(for cf in $CENSUS_FILES; do echo "${DIR2PKG[$(dirname "$cf")]:-}"; done | sed '/^$/d' | sort -u | grep -vFx -f <(printf '%s\n' $SHORT_SET ""))
CENSUS_RX=$(for cf in $CENSUS_FILES; do grep -ho '^func Test[A-Za-z0-9_]*' "$cf"; done | sed 's/^func //' | sort -u | paste -sd'|')
TOUCHED=$(printf '%s\n' $PROD $TESTONLY | sed '/^$/d' | sort -u)
count() { printf '%s\n' $1 | sed '/^$/d' | wc -l; }
relset() { for p in $1; do rel "$p"; done; }

# ── steps
STEP_TIER=(); STEP_NAME=(); STEP_WHY=(); STEP_CMD=(); SKIPPED=()
step() { local tier=$1 name=$2 why=$3; shift 3; STEP_TIER+=("$tier"); STEP_NAME+=("$name"); STEP_WHY+=("$why"); STEP_CMD+=("$(printf '%q ' "$@")"); }
skip() { SKIPPED+=("$1: $2"); }

heap_runs() { # heap_runs <binary> <testname> <envvar> <matrix: wave|final> <forms...>
  local bin=$1 tn=$2 ev=$3 mx=$4; shift 4
  local f n over=0 runs=0 bad=0 out
  out="$RUN_DIR/heap-$tn.runs"; : > "$out"
  # shellcheck disable=SC2329 # called below, inside the subshell
  one() { # ml n bytes bodylog form
    local envs=("$ev=$5" "CTX_FOLGEP_N=$2" "CTX_W0209_N=$2")
    [ -n "$3" ] && envs+=("CTX_FOLGEP_BYTES=$3")
    [ -n "$4" ] && envs+=("CTX_FOLGEP_BODYLOG=$4")
    if [ "$1" = off ]; then env -u GOMEMLIMIT "${envs[@]}" "$bin" -test.run "^$tn\$" -test.count=1 > "$RUN_DIR/heap.tmp" 2>&1
    else env GOMEMLIMIT="$1" "${envs[@]}" "$bin" -test.run "^$tn\$" -test.count=1 > "$RUN_DIR/heap.tmp" 2>&1; fi
    local r=$?; runs=$((runs + 1)); [ $r -ne 0 ] && bad=$((bad + 1))
    grep -q 'verdict=OVER' "$RUN_DIR/heap.tmp" && over=$((over + 1))
    grep -q -- '--- SKIP' "$RUN_DIR/heap.tmp" && { echo "skip form=$5"; bad=$((bad + 1)); }
    echo "rc=$r ml=$1 n=$2 bytes=${3:-cap} bodylog=${4:-none} form=$5 $(grep -h '^FOLGEP form\|^W0209' "$RUN_DIR/heap.tmp" | head -1)" >> "$out"
  }
  (cd "$(dirname "$CHAT_HEAP_FILE")" || exit 9
   # wave: n 1/10 under GOMEMLIMIT=200MiB. final (memall.sh of FOLGE-P, every
   # form): also without a limit, with body logging, 80 × 1 MiB with and without
   # the limit, and the small sizes of hardening 6 with and without body logging
   for f in "$@"; do for n in 1 10; do
     one 200MiB "$n" "" "" "$f"
     if [ "$mx" = final ]; then
       one off "$n" "" "" "$f"
       if [ "$ev" = CTX_FOLGEP_FORM ]; then
         one 200MiB "$n" "" full "$f"
         for by in 20100 65600 262144; do one 200MiB "$n" "$by" "" "$f"; one 200MiB "$n" "$by" full "$f"; done
       fi
     fi
   done
   if [ "$mx" = final ] && [ "$ev" = CTX_FOLGEP_FORM ]; then one 200MiB 80 1048576 "" "$f"; one off 80 1048576 "" "$f"; fi
   done
   echo "heap $tn: runs=$runs over=$over rcfail=$bad matrix=$mx"
   [ "$bad" -eq 0 ] && [ "$over" -eq 0 ])
}
# every label of memForm's switch (several per case line), each also with _pub:
# the public alias runs the sensitivity scan (S-H6-1), the plain one does not
chat_forms() {
  grep -E '^[[:space:]]*case "f[0-9a-z_]+"' "$CHAT_HEAP_FILE" | grep -oE '"f[0-9a-z_]+"' | tr -d '"' | sort -u |
    while IFS= read -r f; do printf '%s %s_pub ' "$f" "$f"; done
}
build_bin() { (cd "$GO_DIR" && go test -c -o "$RUN_DIR/bin/$1.test" "$2"); }
step_chatheap() { build_bin oai ./internal/oai/ && heap_runs "$RUN_DIR/bin/oai.test" TestChatParseHeapGate CTX_FOLGEP_FORM "$1" ${CTX_GATES_HEAP_FORMS:-$(chat_forms)}; }
step_embedheap() { build_bin oai ./internal/oai/ && heap_runs "$RUN_DIR/bin/oai.test" TestEmbedScanHeapGate CTX_W0209_SCAN_FORM "$1" $EMBED_HEAP_FORMS; }
step_streamheap() { # wave: once; final: three runs (heap gates by run, the medians inside each)
  build_bin oai ./internal/oai/ || return 1
  local n=1 i r bad=0; [ "$1" = final ] && n=3
  for i in $(seq "$n"); do
    (cd "$GO_DIR/internal/oai" && "$RUN_DIR/bin/oai.test" -test.count=1 -test.v -test.run "$STREAM_HEAP_GATES") > "$RUN_DIR/streamheap.log" 2>&1; r=$?
    [ $r -ne 0 ] && bad=$((bad + 1))
    echo "stream heap run $i: rc=$r pass=$(grep -c '^--- PASS' "$RUN_DIR/streamheap.log") fail=$(grep -c '^--- FAIL' "$RUN_DIR/streamheap.log") skip=$(grep -c '^--- SKIP' "$RUN_DIR/streamheap.log")"
    grep -hoE 'FOLGES \{.*\}|FOLGE-S S[14]: .*' "$RUN_DIR/streamheap.log" | sed 's/^/  /'
  done
  [ "$bad" -eq 0 ]
}
step_chatcpu() {
  build_bin oai ./internal/oai/ || return 1
  (cd "$GO_DIR/internal/oai" && "$RUN_DIR/bin/oai.test" -test.count=20 -test.v -test.run "$CPU_GATES") > "$RUN_DIR/cpu.log" 2>&1; local r=$?
  echo "cpu gates -count=20: rc=$r pass=$(grep -c '^--- PASS' "$RUN_DIR/cpu.log") fail=$(grep -c '^--- FAIL' "$RUN_DIR/cpu.log") retries=$(grep -c 'FOLGEP cpu retry' "$RUN_DIR/cpu.log")"
  return $r
}
step_census() { # numeric literals added to production Go files (A-E2 review aid, never red)
  git -C "$REPO" diff -U0 "$MB" -- 'go/internal/*.go' 'go/cmd/*.go' ':(exclude)*_test.go' |
    grep '^+[^+]' | grep -oE '(^|[^A-Za-z0-9_.])[0-9][0-9_]*(\.[0-9]+)?' | sed 's/^[^0-9]//' | sort | uniq -c | sort -rn | head -40
  echo "census: done (review aid, not a gate)"
}
step_doccheck() { # whitespace errors and conflict markers; tracked files via git, untracked by grep
  local f bad=0
  git -C "$REPO" diff --check "$MB" -- "$@" || bad=1
  for f in "$@"; do
    git -C "$REPO" ls-files --error-unmatch -- "$f" >/dev/null 2>&1 && continue
    [ -f "$REPO/$f" ] || continue
    if grep -InE '[[:blank:]]+$|^(<{7}|>{7})( |$)' "$REPO/$f"; then echo "untracked $f: trailing whitespace or conflict marker"; bad=1; fi
  done
  return $bad
}
step_web() { command -v bun >/dev/null 2>&1 || { echo "bun not on PATH"; return 1; }; (cd "$GO_DIR/web" && bun run check && bun run test); }
step_self() { bash -n "$GO_DIR/gates.sh" || return 1; if command -v shellcheck >/dev/null 2>&1; then shellcheck "$GO_DIR/gates.sh"; else echo "shellcheck not installed — bash -n only"; fi; }
step_lint_precommit() { local c="$REPO/.gotmp/glci-precommit"; mkdir -p "$c"; GOLANGCI_LINT_CACHE="$c" golangci-lint run --timeout=2m; }

plan() {
  local st=$1 short race integ python fuzz heapmx
  # quick tier
  if [ "$GO_CHANGED" = 1 ] || [ "$st" = full ]; then
    step 1 build "Go files changed" go build ./...
    step 1 vet "Go files changed (pre-commit form)" go vet -tags=integration ./...
  else skip build "no Go file changed"; skip vet "no Go file changed"; fi
  if [ "$st" = full ]; then step 1 lint "full: CI form over the tree" golangci-lint run --timeout=5m
  elif [ "$LINT_ALL" = 1 ]; then step 1 lint "$( [ "$MODALL" = 1 ] && echo "go.mod/go.sum changed" || echo ".golangci.yml changed"): the whole tree" golangci-lint run --timeout=5m
  elif [ -n "$TOUCHED" ]; then step 1 lint "touched packages" golangci-lint run --timeout=5m $(relset "$TOUCHED")
  else skip lint "no Go package touched"; fi
  if [ ${#NONGO_TEXT[@]} -gt 0 ]; then step 1 doccheck "non-Go text changed: whitespace and conflict markers" step_doccheck "${NONGO_TEXT[@]}"; fi
  [ "$SELF_HIT" = 1 ] && step 1 selfcheck "go/gates.sh changed" step_self
  if [ "$DEADCODE_HIT" = 1 ] && [ "$st" != full ]; then step 1 deadcode "deadcode files changed" bash "$REPO/deadcode.sh"; step 1 deadcode-testonly "deadcode files changed" bash "$REPO/deadcode.sh" testonly; fi
  if [ "$st" = full ]; then short="$ALL_PKGS"; else short="$SHORT_SET"; fi
  # wave: a package that also runs in this wave's integration step (-tags=integration
  # without -short) runs every one of its tests there, nackt — the tree has no
  # !integration file and no test that runs only under -short — so its -short run
  # would repeat them (oai: 77 s). quick and full keep it.
  INTEG_DEDUP=""
  if [ "$st" = wave ]; then
    local p ip keep=""
    for p in $short; do
      ip="$(rel "$p")"
      if [ -n "${INTEG[$ip]:-}" ]; then INTEG_DEDUP="$INTEG_DEDUP $ip"; else keep="$keep $p"; fi
    done
    short="$keep"
  fi
  if [ -n "$short" ]; then step 1 short "$( [ "$st" = full ] && echo "full: every package" || echo "touched + reverse dependents ($(count "$short") packages${INTEG_DEDUP:+; without$INTEG_DEDUP, they run all their tests in the integration step})")" go test -short -count=1 $( [ "$st" = full ] && echo ./... || relset "$short")
  else skip short "no Go package affected"; fi
  if [ "$st" != full ] && [ "$GO_CHANGED" = 1 ] && [ -n "$CENSUS_PKGS" ] && [ -n "$CENSUS_RX" ]; then
    step 1 census-tests "source-census tests outside the -short set ($(count "$CENSUS_PKGS") packages; they read source files, no import edge)" go test -short -count=1 -run "^($CENSUS_RX)\$" $(relset "$CENSUS_PKGS")
  fi
  [ "$st" = quick ] && return 0
  # wave tier
  if [ "$st" = full ]; then race="$ALL_PKGS"; else race="$RACE_SET"; fi
  if [ -n "$race" ]; then step 2 race "$( [ "$st" = full ] && echo "full: every package" || echo "touched + direct importers ($(count "$race") packages)")" go test -short -race -count=1 -timeout 60m $( [ "$st" = full ] && echo ./... || relset "$race")
  elif [ -n "$TESTONLY" ]; then skip race "only test files changed (decision 3: quick without race; race-tagged test files excepted)"
  else skip race "no Go package touched"; fi
  integ=$(printf '%s\n' "${!INTEG[@]}" | sed '/^$/d' | sort -u)
  [ "$st" = full ] && integ=$(printf '%s\n' $integ "${FULL_INTEG[@]}" | sort -u)
  if [ -n "$integ" ]; then step 2 integration "$( [ "$st" = full ] && echo 'hardening-7 list + ')$(for p in $integ; do printf '%s[%s] ' "$p" "${INTEG_WHY[$p]:-hardening-7 list}"; done)" go test -tags=integration -count=1 -p 1 -timeout 60m $integ
  else skip integration "no package with a reason"; fi
  python=0; [ -n "${EXTRA[python]:-}" ] && python=1; [ "$st" = full ] && python=1
  if [ "$python" = 1 ]; then
    if [ "$st" = full ]; then step 2 python "measurement (CI never runs it): full form" go test -tags=pyconformance -count=1 -v ./internal/oai/
    else step 2 python "measurement (CI never runs it): ${EXTRA_WHY[python]}; the rest of the package runs in its integration step" go test -tags=pyconformance -count=1 -v -run '^TestConformancePython' ./internal/oai/; fi
  else skip python "no wire-facing file changed"; fi
  if [ -n "${EXTRA[differential]:-}" ] || [ "$st" = full ]; then
    fuzz=60s; { [ "$FINAL" = 1 ] || [ "$st" = full ]; } && fuzz=300s
    step 2 differential "${EXTRA_WHY[differential]:-full}" go test -count=1 -run '^TestChatParseDifferential$' ./internal/oai/
    step 2 fuzz "measurement (CI never runs it): fuzzer $fuzz" go test -run '^$' -fuzz '^FuzzChatParseDifferential$' -fuzztime "$fuzz" -parallel 4 ./internal/oai/
  else skip differential "parse files untouched"; skip fuzz "parse files untouched"; fi
  if [ -n "${EXTRA[embeddiff]:-}" ] || { [ "$st" = full ] && [ -f "$EMBED_DIFF_FILE" ]; }; then
    if [ -f "$EMBED_DIFF_FILE" ]; then
      fuzz=60s; { [ "$FINAL" = 1 ] || [ "$st" = full ]; } && fuzz=300s
      step 2 embeddiff "${EXTRA_WHY[embeddiff]:-full}" go test -count=1 -run '^TestEmbedParseDifferential$' ./internal/oai/
      step 2 embedfuzz "measurement (CI never runs it): fuzzer $fuzz" go test -run '^$' -fuzz '^FuzzEmbedParseDifferential$' -fuzztime "$fuzz" -parallel 4 ./internal/oai/
    else skip embeddiff "no embeddings differential in this tree (W02-09 delivers $EMBED_DIFF_FILE)"; fi
  else skip embeddiff "embeddings parse untouched"; fi
  heapmx=wave; { [ "$FINAL" = 1 ] || [ "$st" = full ]; } && heapmx=final
  if [ -n "${EXTRA[chatheap]:-}" ] || [ "$st" = full ]; then step 2 chatheap "measurement (CI never runs it): ${EXTRA_WHY[chatheap]:-full}, matrix $heapmx" step_chatheap "$heapmx"
  else skip chatheap "chat route untouched"; fi
  if [ -n "${EXTRA[chatcpu]:-}" ] || [ "$st" = full ]; then step 2 chatcpu "measurement (CI never runs it): ${EXTRA_WHY[chatcpu]:-full}" step_chatcpu
  else skip chatcpu "chat route untouched"; fi
  if [ -n "${EXTRA[embedheap]:-}" ] || { [ "$st" = full ] && [ -f "$EMBED_HEAP_FILE" ]; }; then
    if [ -f "$EMBED_HEAP_FILE" ]; then step 2 embedheap "measurement (CI never runs it): ${EXTRA_WHY[embedheap]:-full}" step_embedheap "$heapmx"
    else skip embedheap "no embeddings heap gate in this tree (W02-09 delivers $EMBED_HEAP_FILE)"; fi
  else skip embedheap "embeddings route untouched"; fi
  if [ -n "${EXTRA[streamheap]:-}" ] || { [ "$st" = full ] && [ -f "$STREAM_HEAP_FILE" ]; }; then
    if [ -f "$STREAM_HEAP_FILE" ]; then step 2 streamheap "measurement (CI runs it in the integration job): ${EXTRA_WHY[streamheap]:-full}, matrix $heapmx" step_streamheap "$heapmx"
    else skip streamheap "no stream heap gates in this tree (FOLGE-S delivers $STREAM_HEAP_FILE)"; fi
  else skip streamheap "stream path untouched"; fi
  if [ -n "${EXTRA[web]:-}" ]; then step 2 web "${EXTRA_WHY[web]}" step_web; skip web-e2e "Playwright runs in CI's pinned container (web job)"
  else skip web "go/web untouched"; fi
  if [ "$st" = full ]; then
    step 3 deadcode full bash "$REPO/deadcode.sh"
    step 3 deadcode-testonly full bash "$REPO/deadcode.sh" testonly
  elif [ "$FINAL" = 1 ] && [ -n "$TOUCHED" ] && [ "$DEADCODE_HIT" = 0 ]; then
    step 2 deadcode --final bash "$REPO/deadcode.sh"
    step 2 deadcode-testonly --final bash "$REPO/deadcode.sh" testonly
  fi
  if [ "$st" = full ]; then
    step 3 lint-precommit "full: pre-commit form (own cache); the --build-tags=integration form equals the CI form (NZ-1, docs/development.md)" step_lint_precommit
    step 3 census "full: numeric literals added to production files" step_census
  fi
  return 0
}

explain() {
  echo "gates: explain stage=$STAGE base=$BASE_REF merge-base=${MB:0:12} final=$FINAL"
  echo "changed files (${#CHANGED[@]}):"; printf '  %s\n' "${CHANGED[@]}"
  echo "touched packages: production=[$(relset "$PROD" | tr '\n' ' ')] test-only=[$(relset "$TESTONLY" | tr '\n' ' ')]"
  [ ${#UNMAPPED[@]} -gt 0 ] && echo "unmapped (no package in go list → every package selected): ${UNMAPPED[*]}"
  [ "$MODALL" = 1 ] && echo "go.mod/go.sum changed → every package selected"
  for p in "${!READERS[@]}"; do echo "reader: $(rel "$p") tests open ${READERS[$p]}"; done
  echo "-short set (touched + transitive reverse dependents): $(count "$SHORT_SET") [$(relset "$SHORT_SET" | tr '\n' ' ')]"
  echo "-race set (touched + direct importers): $(count "$RACE_SET") [$(relset "$RACE_SET" | tr '\n' ' ')]"
  for p in "${!SHORT_EXTRA[@]}"; do echo "short extra $p ← ${SHORT_EXTRA[$p]}"; done
  for p in "${!RACE_TEST[@]}"; do echo "race: $(rel "$p") ← ${RACE_TEST[$p]}"; done
  [ -n "$CENSUS_PKGS" ] && echo "census packages outside the -short set: $(count "$CENSUS_PKGS") [$(relset "$CENSUS_PKGS" | tr '\n' ' ')]"
  for g in "${!EXTRA[@]}"; do echo "extra $g ← ${EXTRA_WHY[$g]}"; done
  local i
  for i in "${!STEP_NAME[@]}"; do echo "step ${STEP_TIER[$i]}/${STEP_NAME[$i]}: ${STEP_WHY[$i]}"; echo "    \$ ${STEP_CMD[$i]}"; done
}

summary() {
  local names; names=$(printf '%s,' "${STEP_NAME[@]}"); names="${names%,}"
  local sk=""; [ ${#SKIPPED[@]} -gt 0 ] && sk=$(printf '%s; ' "${SKIPPED[@]}") && sk="${sk%; }"
  [ "$MODALL" = 1 ] && [ "$STAGE" != full ] && echo "gates: note: go.mod/go.sum changed — every package is affected; run full here, CI covers every integration shard"
  echo "gates: stufe=$STAGE$( [ "$FINAL" = 1 ] && echo ' --final') pakete=$( [ "$STAGE" = full ] && grep -c "" "$GRAPH/plain" || count "$SHORT_SET") gates=[$names] ausgelassen=[$sk]${1:+ $1}"
}

plan "$STAGE"
if [ "$CMD" = explain ]; then explain; summary; exit 0; fi

echo "gates: $STAGE start $(now) load $(load) base=$BASE_REF merge-base=${MB:0:12} changed=${#CHANGED[@]}"
RED=0; RESULTS=(); cur_tier=1
for i in "${!STEP_NAME[@]}"; do
  t=${STEP_TIER[$i]}
  if [ "$t" != "$cur_tier" ]; then
    if [ "$RED" != 0 ] && [ "$KEEP" = 0 ]; then echo "gates: tier $RED red — stopping (use --keep-going to run the rest)"; break; fi
    cur_tier=$t
  fi
  name=${STEP_NAME[$i]}; lf="$RUN_DIR/$(printf '%02d' "$i")-$name.log"
  s=$(date +%s)
  echo "== $name start $(now) load $(load) :: ${STEP_CMD[$i]}" > "$lf"
  (cd "$GO_DIR" && eval "${STEP_CMD[$i]}") >> "$lf" 2>&1; rc=$?
  d=$(( $(date +%s) - s ))
  echo "== $name rc=$rc seconds=$d end $(now) load $(load)" >> "$lf"
  fails=$(grep -c '^--- FAIL\|^FAIL' "$lf"); races=$(grep -c 'WARNING: DATA RACE' "$lf")
  last=$(grep -E '^(heap |cpu gates|census:|ok  |FAIL|PASS$)' "$lf" | tail -1 | cut -c1-120)
  echo "gates: step $t/$name rc=$rc ${d}s fails=$fails races=$races ${last:+| $last}"
  if [ "$name" = race ]; then # CI's unit job runs -race with -timeout=5m per test binary (ci.yml)
    awk -F'\t' '/^ok/ { t = $3; sub(/s$/, "", t); if (t + 0 > 300) printf "gates: warning: %s took %ss under -race here; CI allows 5m per binary (ci.yml unit job)\n", $2, t }' "$lf"
  fi
  RESULTS+=("$name:$rc:${d}s")
  if [ "$rc" != 0 ] && [ "$RED" = 0 ]; then RED=$t; fi
done
echo "gates: $STAGE end $(now) load $(load)"
summary "rot=$RED log=$RUN_DIR"
exit "$RED"
