#!/usr/bin/env bash
# The report of a nightly run of ci.yml (job nightly-report, issue #514): one
# open issue while the nightly is red, closed by the first green run.
#
#   bash .github/scripts/nightly-report.sh
#
# Inputs (env):
#   NEEDS_JSON  ${{ toJSON(needs) }} of the job: every job of the run it waits
#               for, with its result (success, failure, cancelled, skipped)
#   REPO        owner/name
#   RUN_URL     the run's page
#   MODE        write (a schedule run) or dry-run (anything else: reads the
#               open issue, writes nothing, says what it would do)
#   ASSIGNEES   comma-separated logins a new issue is assigned to (optional)
#   GH_TOKEN    read by gh; this script never prints it
#   GITHUB_STEP_SUMMARY (optional)
#
# Red is a job that failed or was cancelled (a job that runs out of its
# timeout-minutes ends cancelled); skipped and success are green.
#
# The report's issues are the open issues with the label and exactly the
# title below, read from the REST list of the repository's issues (not from
# the search index, which `gh issue list --label` reads and which can lag
# behind an issue opened moments ago). When that read fails, the script stops
# before it writes anything.
#
# Red and no such issue: the label is created (or kept, --force), one issue
# opened, then assigned (an assignment that fails leaves the issue and a
# warning). Red and such an issue: a comment on the oldest — never a second
# issue, so a red streak stays one issue. Green: every such issue is closed
# with a comment that links the run.
set -euo pipefail

LABEL=nightly-red
TITLE="Nightly CI run is red"
SUMMARY=${GITHUB_STEP_SUMMARY:-/dev/null}

fail() {
  echo "::error::nightly-report: $*" >&2
  exit 1
}

: "${NEEDS_JSON:?NEEDS_JSON is not set}"
: "${REPO:?REPO is not set}"
: "${RUN_URL:?RUN_URL is not set}"
MODE=${MODE:-dry-run}
case "$MODE" in write | dry-run) ;; *) fail "MODE must be write or dry-run, not '$MODE'" ;; esac

# One line per job: <job> <result>. An empty object or an unknown result is
# an error, not a green run.
results=$(jq -r 'to_entries[] | "\(.key) \(.value.result)"' <<<"$NEEDS_JSON") ||
  fail "NEEDS_JSON is not a JSON object of job results"
[ -n "$results" ] || fail "NEEDS_JSON names no job"
red_jobs=()
red_rows=()
while read -r job result; do
  case "$result" in
    success | skipped) ;;
    failure | cancelled)
      red_jobs+=("$job")
      red_rows+=("| \`$job\` | $result |")
      ;;
    *) fail "job $job has result '$result'" ;;
  esac
done <<<"$results"

# The REST list holds pull requests too; they carry a pull_request key.
listed=$(gh api "repos/$REPO/issues?state=open&labels=$LABEL&per_page=100") ||
  fail "could not list the open issues with label $LABEL"
ours=$(jq -r --arg t "$TITLE" '[.[] | select(.pull_request == null and .title == $t) | .number] | sort | .[]' <<<"$listed") ||
  fail "could not read the issue list"
issue=$(head -n 1 <<<"$ours")

body=$(mktemp)
trap 'rm -f "$body"' EXIT
today=$(date -u +%F)
if [ "${#red_jobs[@]}" -gt 0 ]; then
  {
    echo "The nightly run of ci.yml on $today is red: $RUN_URL"
    echo ""
    echo "| Job | Result |"
    echo "|---|---|"
    printf '%s\n' "${red_rows[@]}"
    echo ""
    echo "The job \`nightly-report\` of every scheduled run comments here while the nightly stays red and closes this issue on the first green run (docs/development.md, \"A red nightly opens an issue\")."
  } >"$body"
  if [ -n "$issue" ]; then action="comment on #$issue"; else action="open an issue"; fi
elif [ -n "$issue" ]; then
  echo "The nightly run of ci.yml on $today is green again: $RUN_URL" >"$body"
  numbers=$(paste -sd ' ' - <<<"$ours")
  action="close #${numbers// / #}"
else
  action="nothing (green, no open issue)"
fi

echo "nightly-report ($MODE): ${#red_jobs[@]} red job(s)${red_jobs[*]:+: ${red_jobs[*]}}; action: $action"
{
  echo "## Nightly report ($MODE)"
  echo ""
  echo "Action: $action"
  if [ -s "$body" ]; then
    echo ""
    cat "$body"
  fi
} >>"$SUMMARY"

[ "$MODE" = write ] || exit 0
if [ "${#red_jobs[@]}" -gt 0 ] && [ -z "$issue" ]; then
  gh label create "$LABEL" --repo "$REPO" --force --color B60205 \
    --description "A scheduled run of ci.yml is red (job nightly-report)" >/dev/null
  url=$(gh issue create --repo "$REPO" --title "$TITLE" --label "$LABEL" --body-file "$body")
  echo "opened $url"
  if [ -n "${ASSIGNEES:-}" ] && ! gh issue edit "${url##*/}" --repo "$REPO" --add-assignee "$ASSIGNEES" >/dev/null; then
    echo "::warning::nightly-report: $url could not be assigned to $ASSIGNEES"
  fi
elif [ "${#red_jobs[@]}" -gt 0 ]; then
  gh issue comment "$issue" --repo "$REPO" --body-file "$body"
else
  while read -r n; do
    [ -z "$n" ] || gh issue close "$n" --repo "$REPO" --comment "$(cat "$body")"
  done <<<"$ours"
fi
