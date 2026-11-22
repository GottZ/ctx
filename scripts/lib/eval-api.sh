# shellcheck shell=bash
# eval-api.sh — one request of an eval harness, and whether it got an answer.
#
# Sourced by eval.sh and eval-temporal.sh (#472) and by test.sh (#478). A
# request without an answer is no measurement: the harness counts it by its
# class and never scores it — a pass rate, a recall, a baseline or a passed
# test from it would read a broken server as a result.
#
#   eval_api_request METHOD URL BODY TIMEOUT_S KEY_HEADER [ANSWER]
#     The general form behind eval_api_call (#478). METHOD is GET, POST, PUT or
#     DELETE; BODY '' sends no body. KEY_HEADER is the whole credential header
#     line ("X-Context-Key: …" or "Authorization: Bearer …"); it reaches curl
#     on its stdin, like the key of eval_api_call. ANSWER names the body that
#     counts as an answer (eval_no_answer_class): success (the default), ok or
#     jsonrpc — with jsonrpc the request also carries the Accept header the MCP
#     transport requires. Sets API_BODY, API_STATUS, API_NO_ANSWER and
#     API_DETAIL like eval_api_call (call it directly, never inside $(…)) and
#     never exits.
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
#   eval_no_answer_class CURL_RC HTTP_STATUS BODY [ANSWER]
#     Prints the class of a request without an answer, nothing for an answer.
#     A request got an answer when curl ended with exit 0, the status is 2xx
#     and the body is the answer ANSWER names:
#       success  (the default) a JSON object with success:true — every answer
#                of /api/query, /api/search, /api/store, /api/manage,
#                /api/settings and /api/graph carries it
#       ok       a JSON object with ok:true — /api/synthesize/daily
#       jsonrpc  a JSON-RPC response with a result object, no error, and a
#                result whose isError is not true — /mcp (a tool that failed
#                answers 200 with isError:true)
#     Any other ANSWER is never met: every body is bad_body. The classes:
#       timeout       curl ran out of time (exit 28) — also after a 200 status
#                     line, which /api/query commits before its keepalive spaces
#       connection    any other curl failure: refused, a name that does not
#                     resolve, TLS, a connection closed without an answer
#       http_401_403  the key was refused (401) or lacks the right (403)
#       http_5xx      a server error
#       http_other    any other status outside 2xx (a redirect among them)
#       bad_body      a 2xx whose body is not the answer ANSWER names —
#                     success:false in a 200 is a failure after the keepalive
#                     committed the status, e.g. the synthesis
#
#   eval_class_counts CLASS...
#     Prints the classes counted, in the order they first occur: "2 timeout, 1 http_5xx".

EVAL_API_ANSWER_PY=$(cat <<'PY'
import json, sys
shape = sys.argv[1] if len(sys.argv) > 1 else "success"
try:
    doc = json.loads(sys.stdin.read())
except Exception:
    sys.exit(1)
if not isinstance(doc, dict):
    sys.exit(1)
if shape == "success":
    sys.exit(0 if doc.get("success") is True else 1)
if shape == "ok":
    sys.exit(0 if doc.get("ok") is True else 1)
if shape == "jsonrpc":
    result = doc.get("result")
    sys.exit(0 if "error" not in doc and isinstance(result, dict) and result.get("isError") is not True else 1)
sys.exit(1)
PY
)

eval_no_answer_class() {
  local rc="$1" status="$2" body="$3" answer="${4:-success}"
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
  if ! printf '%s' "$body" | python3 -c "$EVAL_API_ANSWER_PY" "$answer" 2>/dev/null; then
    echo bad_body
  fi
  return 0
}

# shellcheck disable=SC2034 # the API_* variables are this function's output, read by the caller
eval_api_request() {
  local method="$1" url="$2" body="$3" timeout="$4" key_header="$5" answer="${6:-success}" out rc
  local args=(-q -s --max-time "$timeout" -w '\n%{http_code}' -X "$method" "$url" -H @-)
  if [[ -n "$body" ]]; then
    args+=(-H "Content-Type: application/json" -d "$body")
  fi
  if [[ "$answer" == jsonrpc ]]; then
    args+=(-H "Accept: application/json, text/event-stream")
  fi
  # The status rides behind the body on a line of its own; curl writes it
  # (000) also when the request got no response at all.
  out=$(curl "${args[@]}" 2>/dev/null <<< "$key_header")
  rc=$?
  API_STATUS=000
  API_BODY=""
  if [[ "$out" == *$'\n'* ]]; then
    API_STATUS="${out##*$'\n'}"
    API_BODY="${out%$'\n'*}"
  fi
  [[ "$API_STATUS" =~ ^[0-9]{3}$ ]] || API_STATUS=000
  API_NO_ANSWER=$(eval_no_answer_class "$rc" "$API_STATUS" "$API_BODY" "$answer")
  case "$API_NO_ANSWER" in
    "") API_DETAIL="" ;;
    timeout) API_DETAIL="timeout after ${timeout}s" ;;
    connection) API_DETAIL="no connection (curl exit $rc)" ;;
    bad_body)
      case "$answer" in
        success) API_DETAIL="HTTP $API_STATUS, the body is not a JSON object with success:true" ;;
        ok) API_DETAIL="HTTP $API_STATUS, the body is not a JSON object with ok:true" ;;
        jsonrpc) API_DETAIL="HTTP $API_STATUS, the body is not a JSON-RPC result without isError" ;;
        *) API_DETAIL="HTTP $API_STATUS, no answer shape '$answer'" ;;
      esac
      ;;
    *) API_DETAIL="HTTP $API_STATUS" ;;
  esac
  return 0
}

eval_api_call() {
  local url="$1" body="$2" timeout="$3" key="$4" key_var="${5:-}"
  eval_api_request POST "$url" "$body" "$timeout" "X-Context-Key: $key"
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
