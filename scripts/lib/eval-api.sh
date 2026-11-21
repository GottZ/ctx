# shellcheck shell=bash
# eval-api.sh — one request of an eval harness, and whether it got an answer.
#
# Sourced by eval.sh and eval-temporal.sh (#472). A request without an answer
# is no measurement: the harness counts it by its class and never scores it —
# a pass rate, a recall or a baseline from it would read a broken server as a
# retrieval result.
#
#   eval_api_call URL BODY TIMEOUT_S KEY [KEY_VAR]
#     POSTs BODY as JSON with X-Context-Key: KEY (curl --max-time TIMEOUT_S; a
#     redirect is not followed, so the key goes to URL alone; -q keeps a
#     ~/.curlrc from changing that — `location` would follow one, `fail` would
#     turn every status into a curl error; the key reaches curl on its stdin,
#     not on a command line other users can read) and sets, in the calling
#     shell — call it directly, never inside $(…), where the variables would
#     stay in the subshell:
#       API_BODY       the response body ('' without one)
#       API_STATUS     the HTTP status, 000 without a response
#       API_NO_ANSWER  '' when the request got an answer, else its class
#       API_DETAIL     the class in words, for a log line
#     With KEY_VAR, HTTP 401 stops the script (exit 1) naming KEY_VAR: every
#     later request would get the same answer. The key itself is never printed.
#
#   eval_no_answer_class CURL_RC HTTP_STATUS BODY
#     Prints the class of a request without an answer, nothing for an answer.
#     A request got an answer when curl ended with exit 0, the status is 2xx
#     and the body is a JSON object with success:true (every answer of
#     /api/query, /api/search and /api/manage carries it). The classes:
#       timeout       curl ran out of time (exit 28) — also after a 200 status
#                     line, which /api/query commits before its keepalive spaces
#       connection    any other curl failure: refused, a name that does not
#                     resolve, TLS, a connection closed without an answer
#       http_401_403  the key was refused (401) or lacks the right (403)
#       http_5xx      a server error
#       http_other    any other status outside 2xx (a redirect among them)
#       bad_body      a 2xx whose body is not a JSON object with success:true —
#                     success:false in a 200 is a failure after the keepalive
#                     committed the status, e.g. the synthesis
#
#   eval_class_counts CLASS...
#     Prints the classes counted, in the order they first occur: "2 timeout, 1 http_5xx".

EVAL_API_ANSWER_PY=$(cat <<'PY'
import json, sys
try:
    doc = json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
sys.exit(0 if isinstance(doc, dict) and doc.get("success") is True else 1)
PY
)

eval_no_answer_class() {
  local rc="$1" status="$2" body="$3"
  if [[ "$rc" == 28 ]]; then
    echo timeout
    return 0
  fi
  if [[ "$rc" != 0 ]]; then
    echo connection
    return 0
  fi
  case "$status" in
    401|403) echo http_401_403; return 0 ;;
    5??) echo http_5xx; return 0 ;;
    2??) ;;
    *) echo http_other; return 0 ;;
  esac
  if ! printf '%s' "$body" | python3 -c "$EVAL_API_ANSWER_PY" 2>/dev/null; then
    echo bad_body
  fi
  return 0
}

# shellcheck disable=SC2034 # the API_* variables are this function's output, read by the caller
eval_api_call() {
  local url="$1" body="$2" timeout="$3" key="$4" key_var="${5:-}" out rc
  # The status rides behind the body on a line of its own; curl writes it
  # (000) also when the request got no response at all.
  out=$(curl -q -s --max-time "$timeout" -w '\n%{http_code}' -X POST "$url" \
    -H "Content-Type: application/json" \
    -H @- \
    -d "$body" 2>/dev/null <<< "X-Context-Key: $key")
  rc=$?
  API_STATUS=000
  API_BODY=""
  if [[ "$out" == *$'\n'* ]]; then
    API_STATUS="${out##*$'\n'}"
    API_BODY="${out%$'\n'*}"
  fi
  [[ "$API_STATUS" =~ ^[0-9]{3}$ ]] || API_STATUS=000
  API_NO_ANSWER=$(eval_no_answer_class "$rc" "$API_STATUS" "$API_BODY")
  case "$API_NO_ANSWER" in
    "") API_DETAIL="" ;;
    timeout) API_DETAIL="timeout after ${timeout}s" ;;
    connection) API_DETAIL="no connection (curl exit $rc)" ;;
    bad_body) API_DETAIL="HTTP $API_STATUS, the body is not a JSON object with success:true" ;;
    *) API_DETAIL="HTTP $API_STATUS" ;;
  esac
  if [[ -n "$key_var" && "$API_NO_ANSWER" == http_401_403 && "$API_STATUS" == 401 ]]; then
    echo "[FATAL] $url answered HTTP 401: $key_var is not a key of that server; nothing was written" >&2
    exit 1
  fi
  return 0
}

eval_class_counts() {
  local -A n=()
  local order=() c out=""
  for c in "$@"; do
    [[ -n "${n[$c]:-}" ]] || order+=("$c")
    n[$c]=$(( ${n[$c]:-0} + 1 ))
  done
  for c in "${order[@]}"; do
    out+="${out:+, }${n[$c]} $c"
  done
  printf '%s\n' "$out"
}
