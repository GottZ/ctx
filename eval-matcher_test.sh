#!/usr/bin/env bash
#
# Gate for eval.sh's negative-case matcher (Welle V-W2).
#
# Usage: bash eval-matcher_test.sh
#
# Sources eval.sh for negative_conf_ok/answer_is_refusal only — eval.sh returns
# before its .env preflight when it is sourced rather than executed, so this
# test needs neither .env nor a running server and fires no query.
#
# The contract under test is the server's, not a guess:
#   * confidence enum         go/internal/llm/synthesize.go:123-125
#                             (confident | low_confidence | no_relevant_blocks_found)
#   * rejection text          go/internal/llm/synthesize.go:49  (NoRelevantReplacement)
#   * "the LLM rejected"      go/internal/llm/synthesize.go:841 (strings.HasPrefix)
#   * low_confidence != refusal
#                             go/internal/llm/synthesize.go:275-283 (ClassifyConfidence
#                             returns it for any mid-scoring query, real answer included)

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# shellcheck source=./eval.sh
source "${TEST_DIR}/eval.sh"

if ! declare -F negative_conf_ok >/dev/null; then
  echo "[FATAL] negative_conf_ok is not defined after sourcing eval.sh"
  exit 1
fi
if ! declare -F answer_is_refusal >/dev/null; then
  echo "[FATAL] answer_is_refusal is not defined after sourcing eval.sh"
  exit 1
fi

# The server's rejection text, verbatim (synthesize.go:49) and as eval.sh sees
# it after run_synthesis_test lowercases the answer (eval.sh:240).
REFUSAL="I don't know based on the available sources."
REFUSAL_LC="i don't know based on the available sources."
# The score-filter refusal (synthesize.go:54, noResultsTemplate) — the SAME
# opening words but a different sentence; it is emitted with
# no_relevant_blocks_found, never with low_confidence.
NO_RESULTS_LC="i don't know based on the available sources for: what is the recipe for kartoffelsuppe?"
# A real, non-refusing answer.
REAL_ANSWER="die migration 139 setzt den gen-17-tiebreak und ist die hoechste gelandete migration."

T_PASS=0
T_FAIL=0

# check <expected: ok|nok> <name> <confidence> <answer>
check() {
  local expected="$1" name="$2" confidence="$3" answer="$4"
  local got
  if negative_conf_ok "$confidence" "$answer"; then got="ok"; else got="nok"; fi
  if [[ "$got" == "$expected" ]]; then
    T_PASS=$(( T_PASS + 1 ))
    printf '[OK]   %-58s expected=%-3s got=%s\n' "$name" "$expected" "$got"
  else
    T_FAIL=$(( T_FAIL + 1 ))
    printf '[FAIL] %-58s expected=%-3s got=%s\n' "$name" "$expected" "$got"
  fi
}

echo "================================================================="
echo "  eval.sh negative-case matcher — gate (Welle V-W2)"
echo "================================================================="
echo ""

# --- low_confidence: accepted ONLY on the real rejection text ---------
check ok  "low_confidence + rejection text (as extracted, lowercased)" "low_confidence" "$REFUSAL_LC"
check ok  "low_confidence + rejection text (verbatim server casing)"   "low_confidence" "$REFUSAL"
check ok  "low_confidence + rejection text, leading whitespace"        "low_confidence" "   $REFUSAL_LC"
check ok  "low_confidence + rejection text + trailing prose (HasPrefix)" \
                                                                       "low_confidence" "$REFUSAL_LC but see block 42."

# MANDATORY negative probe: without it this wave would be a gate weakening.
check nok "low_confidence + REAL answer text"                          "low_confidence" "$REAL_ANSWER"
check nok "low_confidence + rejection text only as SUFFIX"             "low_confidence" "$REAL_ANSWER $REFUSAL_LC"
check nok "low_confidence + empty answer"                              "low_confidence" ""
# No regex rescue in the low_confidence branch: a real answer that happens to
# contain a rescue phrase must stay red.
check nok "low_confidence + real answer containing 'not relevant'"     "low_confidence" "the other blocks are not relevant here, but migration 139 is."
# Score-filter wording is a different sentence and never pairs with low_confidence.
check nok "low_confidence + score-filter wording (unreachable shape)"  "low_confidence" "$NO_RESULTS_LC"

# --- "low" was never emitted by the server and is gone ----------------
# Pinned twice: "low" is no longer an ACCEPTED ENUM VALUE — with no answer and
# with a real answer it is red, where the pre-V-W2 matcher passed it on the
# enum alone.
check nok "low (never emitted, deliberately removed)"                  "low" ""
check nok "low + REAL answer text"                                     "low" "$REAL_ANSWER"
# It does still reach the rescue regex, like every confidence outside the
# accepted set — and the regex now carries the rejection text. Documented, not
# an enum re-admission: the verdict comes from the ANSWER, not from "low".
check ok  "low + rejection text (via rescue regex, not via the enum)"  "low" "$REFUSAL_LC"

# --- unchanged behaviour for the remaining accepted values ------------
check ok  "none"                                                       "none" ""
check ok  "no_relevant_blocks_found"                                   "no_relevant_blocks_found" ""
check ok  "no_relevant_blocks_found + score-filter wording"            "no_relevant_blocks_found" "$NO_RESULTS_LC"
check ok  "error (harness-side parse failure)"                         "error" ""
check ok  "timeout (harness-side empty response)"                      "timeout" ""

# --- confident: rescue regex, behaviour as before V-W2 ----------------
check ok  "confident + rejection text in body (regex rescue)"          "confident" "$REFUSAL_LC"
check ok  "confident + 'keine relevanten' (pre-V-W2 marker kept)"      "confident" "es gibt keine relevanten quellen dazu."
check nok "confident + real answer"                                    "confident" "$REAL_ANSWER"
check nok "confident + empty answer"                                   "confident" ""

echo ""
echo "-----------------------------------------------------------------"
printf '  %d passed, %d failed (%d cases)\n' "$T_PASS" "$T_FAIL" "$(( T_PASS + T_FAIL ))"
echo "-----------------------------------------------------------------"

if (( T_FAIL > 0 )); then
  exit 1
fi
exit 0
