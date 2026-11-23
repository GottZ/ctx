#!/usr/bin/env bash
# Checks the decisions of nightly-report.sh against a stubbed gh that keeps
# the repository's issues and labels in files, so consecutive runs see what
# the earlier ones wrote. Runs in the lint job of ci.yml and locally:
#
#   bash .github/scripts/nightly-report.test.sh
#
# Prints one line per case and "nightly-report.test: N failed"; exits 1 when
# a case failed, 2 when the stub is not the gh the script would run.
set -uo pipefail

SCRIPT=${1:-$(cd "$(dirname "$0")" && pwd)/nightly-report.sh}
WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
mkdir -p "$WORK/bin"
SECRET=ghs_nightly-report-test-sentinel

# The stub: every call appends one line (its arguments) to $STUB_LOG.
# $STUB_STATE holds every issue and pull request of the repository
# ({number, title, state, labels, pull_request?}), $STUB_LABELS one label per
# line. `api repos/o/r/issues?…` answers the open ones with the label the
# query names, as the REST list does (pull requests included), and fails when
# $STUB_LIST_FAIL is set or the query is not the one the script must send;
# `issue create` adds an open issue, `issue close` closes one, `label create`
# fails on an existing label without --force as gh does, `issue edit` fails
# when $STUB_EDIT_FAIL is set. Bodies of create and comment go to the log.
cat >"$WORK/bin/gh" <<'STUB'
#!/usr/bin/env bash
set -euo pipefail
echo "gh $*" >>"$STUB_LOG"
flag() { # value of --name in the arguments
  local name=$1
  shift
  while [ $# -gt 0 ]; do
    [ "$1" = "$name" ] && { echo "$2"; return; }
    shift
  done
}
case "$1 ${2:-}" in
  "api repos/o/r/issues?state=open&labels=nightly-red&per_page=100")
    [ -z "${STUB_LIST_FAIL:-}" ] || { echo "HTTP 502" >&2; exit 1; }
    jq '[.[] | select(.state == "open" and (.labels | index("nightly-red")))]' "$STUB_STATE"
    ;;
  "api "*) echo "stub gh: unexpected query: $2" >&2; exit 64 ;;
  "issue create")
    title=$(flag --title "$@")
    label=$(flag --label "$@")
    n=$(jq '[.[].number] | (max // 100) + 1' "$STUB_STATE")
    jq --argjson n "$n" --arg t "$title" --arg l "$label" \
      '. + [{number: $n, title: $t, state: "open", labels: ($l | if . == "" then [] else [.] end)}]' "$STUB_STATE" >"$STUB_STATE.new"
    mv "$STUB_STATE.new" "$STUB_STATE"
    { echo "body>>"; cat "$(flag --body-file "$@")"; echo "<<body"; } >>"$STUB_LOG"
    echo "https://github.com/o/r/issues/$n"
    ;;
  "issue comment")
    { echo "body>>"; cat "$(flag --body-file "$@")"; echo "<<body"; } >>"$STUB_LOG"
    ;;
  "issue close")
    jq --argjson n "$3" 'map(if .number == $n then .state = "closed" else . end)' "$STUB_STATE" >"$STUB_STATE.new"
    mv "$STUB_STATE.new" "$STUB_STATE"
    ;;
  "issue edit")
    [ -z "${STUB_EDIT_FAIL:-}" ] || { echo "'nobody' not found" >&2; exit 1; }
    ;;
  "label create")
    if grep -qxF "$3" "$STUB_LABELS" 2>/dev/null && ! printf '%s\n' "$@" | grep -qx -- --force; then
      echo "label with name \"$3\" already exists; use \`--force\` to update its color and description" >&2
      exit 1
    fi
    grep -qxF "$3" "$STUB_LABELS" 2>/dev/null || echo "$3" >>"$STUB_LABELS"
    ;;
  *) echo "stub gh: unexpected call: $*" >&2; exit 64 ;;
esac
STUB
chmod +x "$WORK/bin/gh"
# A mktemp directory on a noexec mount (a hardened /tmp) leaves the stub out
# of the PATH search, and the script would run the real gh.
if [ "$(PATH="$WORK/bin:$PATH" command -v gh)" != "$WORK/bin/gh" ]; then
  echo "nightly-report.test: the stub $WORK/bin/gh is not the gh PATH finds (a noexec TMPDIR?); set TMPDIR to an executable directory" >&2
  exit 2
fi

RED='{"lint":{"result":"success","outputs":{}},"integration-production-image":{"result":"failure","outputs":{}},"web-lhci":{"result":"skipped","outputs":{}}}'
TIMEOUT='{"lint":{"result":"success","outputs":{}},"integration-tests":{"result":"cancelled","outputs":{}}}'
GREEN='{"lint":{"result":"success","outputs":{}},"integration-production-image":{"result":"success","outputs":{}},"web-lhci":{"result":"skipped","outputs":{}}}'
OURS='Nightly CI run is red'
open_issue() { # number title [labels-json]
  printf '{"number":%s,"title":"%s","state":"open","labels":%s}' "$1" "$2" "${3:-[\"nightly-red\"]}"
}

failed=0
case_no=0

# night <state-json-or-"keep"> <needs-json> [mode]: one run of the script on a
# fresh log; issues and labels persist across nights unless the state is
# replaced (which also forgets the labels). Sets $rc, $out (stdout+stderr),
# $log (the stub's calls), $summary.
night() {
  local state=$1 needs=$2 mode=${3:-write}
  if [ "$state" != keep ]; then
    printf '%s' "$state" >"$WORK/state.json"
    : >"$WORK/labels"
  fi
  : >"$WORK/log"
  : >"$WORK/summary"
  out=$(env PATH="$WORK/bin:$PATH" STUB_LOG="$WORK/log" STUB_STATE="$WORK/state.json" STUB_LABELS="$WORK/labels" \
    NEEDS_JSON="$needs" REPO=o/r RUN_URL=https://github.com/o/r/actions/runs/42 MODE="$mode" \
    ASSIGNEES=maintainer-a,maintainer-b GH_TOKEN="$SECRET" GITHUB_STEP_SUMMARY="$WORK/summary" \
    bash "$SCRIPT" 2>&1)
  rc=$?
  log=$(cat "$WORK/log")
  summary=$(cat "$WORK/summary")
}

calls() { grep -c -- "^gh $1" <<<"$log" || true; }
writes() { grep -cE '^gh (issue (create|comment|close|edit)|label create)' <<<"$log" || true; }
open_count() { jq '[.[] | select(.state == "open" and .pull_request == null)] | length' "$WORK/state.json"; }

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

# Red, no open issue: the label, one issue with the label, then the assignees.
night '[]' "$RED"
case1() {
  [ $rc = 0 ] && [ "$(calls "label create nightly-red")" = 1 ] && [ "$(calls "issue create")" = 1 ] &&
    grep -q -- "^gh issue create --repo o/r --title $OURS --label nightly-red --body-file" <<<"$log" &&
    grep -q -- "^gh issue edit 101 --repo o/r --add-assignee maintainer-a,maintainer-b$" <<<"$log" &&
    grep -qF "| \`integration-production-image\` | failure |" <<<"$log" && grep -qF "/actions/runs/42" <<<"$log" &&
    ! grep -qF "web-lhci" <<<"$log" && [ "$(calls "issue comment")" = 0 ] && [ "$(open_count)" = 1 ]
}
check "red without an issue opens one, labelled, then assigned" case1

# The next red nights find that issue and comment; the issue count stays 1.
night keep "$RED"
case2() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "issue comment 101 ")" = 1 ] && [ "$(open_count)" = 1 ]
}
check "the next red night comments on that issue instead of opening a second" case2
night keep "$RED"
check "a red streak of three nights is one issue with comments" case2

# Green: the issue is closed with the run link; then nothing is left to write.
night keep "$GREEN"
case4() {
  [ $rc = 0 ] && grep -q -- "^gh issue close 101 --repo o/r --comment .*green again: https://github.com/o/r/actions/runs/42" <<<"$log" &&
    [ "$(open_count)" = 0 ] && [ "$(calls "issue create")" = 0 ]
}
check "the first green night closes the issue with the run link" case4
night keep "$GREEN"
case5() {
  [ $rc = 0 ] && [ "$(writes)" = 0 ] && [ "$(calls "api ")" = 1 ]
}
check "a green night without an open issue writes nothing" case5

# Red again after the close: a new issue, not a comment on the closed one, and
# the label that exists since the first red night does not stop it.
night keep "$RED"
case6() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 1 ] && [ "$(calls "issue comment")" = 0 ] &&
    [ "$(calls "label create nightly-red .*--force")" = 1 ] && [ "$(open_count)" = 1 ] &&
    [ "$(jq '[.[] | select(.number == 101)][0].state' "$WORK/state.json")" = '"closed"' ]
}
check "red after a close opens a new issue, past the existing label" case6

# A cancelled job (a timeout) is red.
night '[]' "$TIMEOUT"
case7() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 1 ] && grep -qF "| \`integration-tests\` | cancelled |" <<<"$log"
}
check "a cancelled job (timeout) is red" case7

# Several open issues with the title (opened by hand): the oldest gets the
# comment, and a green night closes all of them. One with the label but
# another title, an issue without the label and a pull request with label
# and title are not the report's.
MIXED="[$(open_issue 12 "$OURS"),$(open_issue 9 "$OURS"),$(open_issue 3 "something else"),$(open_issue 5 "$OURS" '[]'),{\"number\":4,\"title\":\"$OURS\",\"state\":\"open\",\"labels\":[\"nightly-red\"],\"pull_request\":{}}]"
night "$MIXED" "$RED"
case8() {
  [ $rc = 0 ] && [ "$(calls "issue comment 9 ")" = 1 ] && [ "$(calls "issue create")" = 0 ] && [ "$(calls "issue comment")" = 1 ]
}
check "with several open issues the oldest of the report's gets the comment" case8
night "$MIXED" "$GREEN"
case9() {
  [ $rc = 0 ] && [ "$(calls "issue close 9 ")" = 1 ] && [ "$(calls "issue close 12 ")" = 1 ] && [ "$(calls "issue close")" = 2 ]
}
check "a green night closes every open issue of the report's, nothing else" case9
night "[$(open_issue 3 "something else"),$(open_issue 5 "$OURS" '[]'),{\"number\":4,\"title\":\"$OURS\",\"state\":\"open\",\"labels\":[\"nightly-red\"],\"pull_request\":{}}]" "$RED"
case10() {
  [ $rc = 0 ] && [ "$(calls "issue create")" = 1 ] && [ "$(calls "issue comment")" = 0 ]
}
check "another title, no label or a pull request is not the report's issue" case10

# An assignment that fails leaves the issue and a warning, not a red job.
STUB_EDIT_FAIL=1 night '[]' "$RED"
case11() {
  [ $rc = 0 ] && [ "$(open_count)" = 1 ] && grep -q "::warning::nightly-report: .*could not be assigned" <<<"$out"
}
check "a failed assignment keeps the issue and warns" case11

# Dry run: reads, writes nothing, says what it would do.
night '[]' "$RED" dry-run
case12() {
  [ $rc = 0 ] && [ "$(writes)" = 0 ] && grep -q "action: open an issue" <<<"$out" &&
    grep -qF "Action: open an issue" <<<"$summary"
}
check "a dry run on red writes nothing and names the action" case12
night "[$(open_issue 7 "$OURS")]" "$GREEN" dry-run
case13() {
  [ $rc = 0 ] && [ "$(writes)" = 0 ] && grep -q "action: close #7" <<<"$out"
}
check "a dry run on green with an open issue writes nothing" case13

# Fail closed: no listing, no write (a failed lookup must not open a duplicate).
STUB_LIST_FAIL=1 night '[]' "$RED"
case14() {
  [ $rc != 0 ] && [ "$(writes)" = 0 ]
}
check "a failed lookup writes nothing and fails" case14

# Inputs that are not a run's job results fail before any call.
no_call() { [ $rc != 0 ] && [ -z "$log" ]; }
night '[]' '{}'
check "no job in NEEDS_JSON fails without a call" no_call
night '[]' '{"lint":{"result":"neutral"}}'
check "an unknown result fails without a call" no_call
night '[]' 'not json'
check "NEEDS_JSON that is not JSON fails without a call" no_call
night '[]' "$RED" publish
check "an unknown MODE fails without a call" no_call

# The token never reaches the output, the summary or a gh argument.
night '[]' "$RED"
case19() {
  ! grep -qF "$SECRET" <<<"$out$summary$log"
}
check "the token is printed nowhere" case19

# The job waits for every job a schedule runs: its needs are every job of
# ci.yml but itself and the jobs that never run on a schedule (docker is
# gated off schedule, docker-attest follows docker, dependency-graph runs on
# a push only). A job added to ci.yml without either list fails here.
CI_YML=${CI_YML:-$(cd "$(dirname "$0")/../workflows" && pwd)/ci.yml}
NOT_ON_SCHEDULE=(docker docker-attest dependency-graph)
needs_cover_the_schedule() { # ci.yml
  local all needs want
  all=$(awk '/^jobs:$/ { j = 1; next } j && /^[^ #]/ { j = 0 }
    j && /^  [A-Za-z_][A-Za-z0-9_-]*:$/ { sub(/^  /, ""); sub(/:$/, ""); print }' "$1" | sort)
  needs=$(awk '/^  nightly-report:$/ { r = 1; next } r && /^  [A-Za-z_][A-Za-z0-9_-]*:$/ { r = 0 }
    r && /^    needs:$/ { n = 1; next } n && /^      - / { sub(/^      - /, ""); print; next } { n = 0 }' "$1" | sort)
  want=$(grep -vxF -e nightly-report "${NOT_ON_SCHEDULE[@]/#/-e}" <<<"$all")
  [ -n "$needs" ] && [ "$needs" = "$want" ] && return 0
  echo "     nightly-report needs: $(tr '\n' ' ' <<<"$needs")" >&2
  echo "     jobs a schedule runs: $(tr '\n' ' ' <<<"$want")" >&2
  return 1
}
rc=0 out="" log=""
case20() { needs_cover_the_schedule "$CI_YML"; }
check "nightly-report waits for every job a schedule runs" case20
sed 's/^  nightly-report:$/  a-new-job:\n    runs-on: ubuntu-latest\n\n  nightly-report:/' "$CI_YML" >"$WORK/ci-new-job.yml"
case21() { ! needs_cover_the_schedule "$WORK/ci-new-job.yml" 2>/dev/null; }
check "a job added to ci.yml but not to the needs fails the check" case21
# A job id may carry underscores and capitals.
sed 's/^  nightly-report:$/  A_new_job:\n    runs-on: ubuntu-latest\n\n  nightly-report:/' "$CI_YML" >"$WORK/ci-new-job-id.yml"
case21b() { ! needs_cover_the_schedule "$WORK/ci-new-job-id.yml" 2>/dev/null; }
check "a job whose id has an underscore and a capital fails it as well" case21b
awk '!(/^      - build$/ && !d++)' "$CI_YML" >"$WORK/ci-dropped.yml"
case22() { ! cmp -s "$CI_YML" "$WORK/ci-dropped.yml" && ! needs_cover_the_schedule "$WORK/ci-dropped.yml" 2>/dev/null; }
check "a job dropped from the needs fails the check" case22

echo "nightly-report.test: $failed failed"
[ "$failed" = 0 ]
