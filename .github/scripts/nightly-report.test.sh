#!/usr/bin/env bash
# Checks the decisions of nightly-report.sh against a stubbed gh that keeps
# the repository's open issues in a file, so consecutive runs see what the
# earlier ones wrote. Runs in the lint job of ci.yml and locally:
#
#   bash .github/scripts/nightly-report.test.sh
#
# Prints one line per case and "nightly-report.test: N failed"; exits 1 when
# a case failed.
set -uo pipefail

SCRIPT=${1:-$(cd "$(dirname "$0")" && pwd)/nightly-report.sh}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
SECRET=ghs_nightly-report-test-sentinel

# The stub: every call appends one line (its arguments) to $STUB_LOG.
# `issue list` prints $STUB_STATE (a JSON array of open issues) or fails when
# $STUB_LIST_FAIL is set; `issue create` adds an issue to the state, `issue
# close` removes it; the body of create and comment goes to $STUB_LOG too.
cat >"$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >>"$STUB_LOG"
case "$1 $2" in
  "issue list")
    [ -z "${STUB_LIST_FAIL:-}" ] || { echo "HTTP 502" >&2; exit 1; }
    cat "$STUB_STATE"
    ;;
  "issue create")
    title="" body=""
    while [ $# -gt 0 ]; do
      case "$1" in
        --title) title=$2; shift ;;
        --body-file) body=$2; shift ;;
      esac
      shift
    done
    n=$(jq '[.[].number] | (max // 100) + 1' "$STUB_STATE")
    jq --argjson n "$n" --arg t "$title" '. + [{number: $n, title: $t}]' "$STUB_STATE" >"$STUB_STATE.new"
    mv "$STUB_STATE.new" "$STUB_STATE"
    { echo "body>>"; cat "$body"; echo "<<body"; } >>"$STUB_LOG"
    echo "https://github.com/o/r/issues/$n"
    ;;
  "issue comment")
    body=""
    while [ $# -gt 0 ]; do [ "$1" = --body-file ] && body=$2; shift; done
    { echo "body>>"; cat "$body"; echo "<<body"; } >>"$STUB_LOG"
    ;;
  "issue close")
    jq --argjson n "$3" 'map(select(.number != $n))' "$STUB_STATE" >"$STUB_STATE.new"
    mv "$STUB_STATE.new" "$STUB_STATE"
    ;;
  "label create") ;;
  *) echo "stub gh: unexpected call: $*" >&2; exit 64 ;;
esac
STUB
chmod +x "$WORK/bin/gh"

RED='{"lint":{"result":"success","outputs":{}},"integration-production-image":{"result":"failure","outputs":{}},"web-lhci":{"result":"skipped","outputs":{}}}'
TIMEOUT='{"lint":{"result":"success","outputs":{}},"integration-tests":{"result":"cancelled","outputs":{}}}'
GREEN='{"lint":{"result":"success","outputs":{}},"integration-production-image":{"result":"success","outputs":{}},"web-lhci":{"result":"skipped","outputs":{}}}'
OURS='Nightly CI run is red'

failed=0
case_no=0

# night <state-json-or-"keep"> <needs-json> [mode]: one run of the script on a
# fresh log; the state file persists across nights unless replaced. Sets $rc,
# $out (stdout+stderr), $log (the stub's calls), $summary.
night() {
  local state=$1 needs=$2 mode=${3:-write}
  [ "$state" = keep ] || printf '%s' "$state" >"$WORK/state.json"
  : >"$WORK/log"
  : >"$WORK/summary"
  out=$(env PATH="$WORK/bin:$PATH" STUB_LOG="$WORK/log" STUB_STATE="$WORK/state.json" \
    NEEDS_JSON="$needs" REPO=o/r RUN_URL=https://github.com/o/r/actions/runs/42 MODE="$mode" \
    ASSIGNEES=maintainer-a,maintainer-b GH_TOKEN="$SECRET" GITHUB_STEP_SUMMARY="$WORK/summary" \
    bash "$SCRIPT" 2>&1)
  rc=$?
  log=$(cat "$WORK/log")
  summary=$(cat "$WORK/summary")
}

calls() { grep -c -- "^gh $1" <<<"$log" || true; }

check() { # name function
  local name=$1
  shift
  case_no=$((case_no + 1))
  if "$@"; then
    echo "ok   $case_no $name"
  else
    echo "FAIL $case_no $name"
    echo "     rc=$rc out: $(tail -3 <<<"$out" | tr '\n' ' ')"
    echo "     calls: $(grep '^gh ' <<<"$log" | tr '\n' ';')"
    failed=$((failed + 1))
  fi
}

# Red, no open issue: the label, then one issue with label and assignees.
night '[]' "$RED"
case1() {
  [ $rc = 0 ] && [ "$(calls "label create nightly-red")" = 1 ] && [ "$(calls "issue create")" = 1 ] &&
   grep -q -- "--label nightly-red --assignee maintainer-a,maintainer-b --body-file" <<<"$log" &&
   grep -qF "| \`integration-production-image\` | failure |" <<<"$log" && grep -qF "/actions/runs/42" <<<"$log" &&
   ! grep -qF "web-lhci" <<<"$log" && [ "$(calls "issue comment")" = 0 ]
}
check "red without an issue opens one, labelled and assigned" case1

# The next red night finds that issue and comments; the issue count stays 1.
night keep "$RED"
case2() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "issue comment 101")" = 1 ] &&
   [ "$(jq length "$WORK/state.json")" = 1 ]
}
check "the next red night comments on that issue instead of opening a second" case2

# A third red night: still one issue.
night keep "$RED"
case3() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "issue comment 101")" = 1 ] &&
   [ "$(jq length "$WORK/state.json")" = 1 ]
}
check "a red streak of three nights is one issue with comments" case3

# Green: the issue is closed with the run link.
night keep "$GREEN"
case4() {
  [ $rc = 0 ] && grep -q -- "^gh issue close 101 --repo o/r --comment .*green again: https://github.com/o/r/actions/runs/42" <<<"$log" &&
   [ "$(jq length "$WORK/state.json")" = 0 ] && [ "$(calls "issue create")" = 0 ]
}
check "the first green night closes the issue with the run link" case4

# Green again: nothing to write.
night keep "$GREEN"
case5() {
  [ $rc = 0 ] && [ "$(grep -c "^gh " <<<"$log")" = 1 ] && [ "$(calls "issue list")" = 1 ]
}
check "a green night without an open issue writes nothing" case5

# A cancelled job (a timeout) is red.
night '[]' "$TIMEOUT"
case6() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 1 ] && grep -qF "| \`integration-tests\` | cancelled |" <<<"$log"
}
check "a cancelled job (timeout) is red" case6

# Several open issues with the title (opened by hand): the oldest gets the
# comment; one with the label but another title is not ours.
night "[{\"number\":12,\"title\":\"$OURS\"},{\"number\":9,\"title\":\"$OURS\"},{\"number\":3,\"title\":\"something else\"}]" "$RED"
case7() {
  [ $rc = 0 ] && [ "$(calls "issue comment 9 ")" = 1 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "issue comment")" = 1 ]
}
check "with several open issues the oldest of ours gets the comment" case7
night '[{"number":3,"title":"something else"}]' "$RED"
case8() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 1 ] && [ "$(calls "issue comment")" = 0 ]
}
check "an issue with the label but another title is not the report's" case8

# Dry run: reads, writes nothing, says what it would do.
night '[]' "$RED" dry-run
case9() {
  [ $rc = 0 ] && [ "$(grep -c "^gh " <<<"$log")" = 1 ] && grep -q "action: open an issue" <<<"$out" &&
   grep -qF "Action: open an issue" <<<"$summary"
}
check "a dry run on red writes nothing and names the action" case9
night "[{\"number\":7,\"title\":\"$OURS\"}]" "$GREEN" dry-run
case10() {
  [ $rc = 0 ] && [ "$(grep -c "^gh " <<<"$log")" = 1 ] && grep -q "action: close #7" <<<"$out"
}
check "a dry run on green with an open issue writes nothing" case10

# Fail closed: no listing, no write (a failed lookup must not open a duplicate).
STUB_LIST_FAIL=1 night '[]' "$RED"
case11() {
  [ $rc != 0 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "label create")" = 0 ]
}
check "a failed lookup writes nothing and fails" case11
export -n STUB_LIST_FAIL 2>/dev/null || true
unset STUB_LIST_FAIL

# Inputs that are not a run's job results fail before any call.
night '[]' '{}'
case12() {
  [ $rc != 0 ] && [ -z "$log" ]
}
check "no job in NEEDS_JSON fails without a call" case12
night '[]' '{"lint":{"result":"neutral"}}'
case13() {
  [ $rc != 0 ] && [ -z "$log" ]
}
check "an unknown result fails without a call" case13
night '[]' 'not json'
case14() {
  [ $rc != 0 ] && [ -z "$log" ]
}
check "NEEDS_JSON that is not JSON fails without a call" case14
night '[]' "$RED" publish
case15() {
  [ $rc != 0 ] && [ -z "$log" ]
}
check "an unknown MODE fails without a call" case15

# The token never reaches the output, the summary or a gh argument.
night '[]' "$RED"
case16() {
  ! grep -qF "$SECRET" <<<"$out$summary$log"
}
check "the token is printed nowhere" case16

# The job waits for every job a schedule runs: its needs are every job of
# ci.yml but itself and the jobs that never run on a schedule (docker is
# gated off schedule, docker-attest follows docker, dependency-graph runs on
# a push only). A job added to ci.yml without either list fails here.
CI_YML=${CI_YML:-$(cd "$(dirname "$0")/../workflows" && pwd)/ci.yml}
NOT_ON_SCHEDULE=(docker docker-attest dependency-graph)
needs_cover_the_schedule() { # ci.yml
  local all needs want
  all=$(awk '/^jobs:$/ { j = 1; next } j && /^[^ #]/ { j = 0 }
    j && /^  [a-z0-9-]+:$/ { sub(/^  /, ""); sub(/:$/, ""); print }' "$1" | sort)
  needs=$(awk '/^  nightly-report:$/ { r = 1; next } r && /^  [a-z0-9-]+:$/ { r = 0 }
    r && /^    needs:$/ { n = 1; next } n && /^      - / { sub(/^      - /, ""); print; next } { n = 0 }' "$1" | sort)
  want=$(grep -vxF -e nightly-report "${NOT_ON_SCHEDULE[@]/#/-e}" <<<"$all")
  [ -n "$needs" ] && [ "$needs" = "$want" ] && return 0
  echo "     nightly-report needs: $(tr '\n' ' ' <<<"$needs")" >&2
  echo "     jobs a schedule runs: $(tr '\n' ' ' <<<"$want")" >&2
  return 1
}
rc=0 out="" log=""
case17() { needs_cover_the_schedule "$CI_YML"; }
check "nightly-report waits for every job a schedule runs" case17
sed 's/^  nightly-report:$/  a-new-job:\n    runs-on: ubuntu-latest\n\n  nightly-report:/' "$CI_YML" >"$WORK/ci-new-job.yml"
case18() { ! needs_cover_the_schedule "$WORK/ci-new-job.yml" 2>/dev/null; }
check "a job added to ci.yml but not to the needs fails the check" case18
awk '!(/^      - build$/ && !d++)' "$CI_YML" >"$WORK/ci-dropped.yml"
case19() { ! cmp -s "$CI_YML" "$WORK/ci-dropped.yml" && ! needs_cover_the_schedule "$WORK/ci-dropped.yml" 2>/dev/null; }
check "a job dropped from the needs fails the check" case19

echo "nightly-report.test: $failed failed"
[ "$failed" = 0 ]
