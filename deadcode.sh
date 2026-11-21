#!/usr/bin/env bash
#
# ctx — deadcode allowlist gate
# Usage: ./deadcode.sh [with-tests|testonly]
#
# Compares the reachability analysis of golang.org/x/tools/cmd/deadcode against
# an allowlist. Two failure directions, both blocking, in every mode:
#
#   * a symbol that is unreachable but NOT in the allowlist  -> cut it, or write
#     a line with a reason (the reason column is mandatory, so "keeping it" is a
#     visible decision in the diff rather than a matter of discipline);
#   * an allowlist line whose symbol became reachable again  -> delete the line.
#     Without this direction an allowlist only ever grows and turns into a lid.
#
# Two modes, two questions, two policy files:
#
#   with-tests (default)  deadcode -test  vs go/deadcode-allow.txt
#                         "is anything dead even when the tests are counted?"
#   testonly              deadcode        vs go/deadcode-testonly-allow.txt
#                         "which production symbols are alive ONLY because a
#                          test calls them?" (class T)
#
# The testonly run subtracts the with-tests allowlist from its own findings, so
# a symbol is never carried in both files: everything dead WITH the tests is by
# definition also dead without them, and that class already has its line and its
# reason in the first list. Subtracting the FILE (not a second deadcode run) is
# exact because the with-tests gate blocks in both directions — whenever it is
# green, its allowlist and its findings are the same set, and when it is red the
# push/CI stops at that gate anyway.
#
# deadcode is structurally blind to constants, vars, types and struct fields; it
# reports functions and methods only (measured in T02-13: a package-level var, a
# const and a type, each used solely by a _test.go, do not show up — the same
# file's func and method do). The `unused` linter in .golangci.yml does not close
# that gap for the test-only class either: it counts a use from a _test.go as a
# use and stays silent on exported identifiers. So class T is enforced here for
# functions and methods, and NOT enforced anywhere for vars, consts and types —
# a deliberate, documented limit rather than an assumed cover.
#
# ctx — Your AI's save game. By GottZ and Damien Moon (github.com/GottZ/ctx/graphs/contributors)

set -euo pipefail
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR/go"

# The package set is hard-wired, never taken from the caller: deadcode reports
# per-package reachability, so `deadcode ./internal/armsweep/` alone flags nine
# symbols whose callers live in cmd/ — a fehlalarm generator. Modes are a closed
# list for the same reason: a mode is a flag set plus a policy file, chosen here,
# it does not open the script up to arguments.
MODE="${1:-with-tests}"
case "$MODE" in
    with-tests)
        LABEL="deadcode-gate"
        DEADCODE_FLAGS=(-test -tags=integration)
        ALLOW_SRC="deadcode-allow.txt"
        SUBTRACT_SRC=""
        ;;
    testonly)
        LABEL="deadcode-testonly-gate"
        DEADCODE_FLAGS=(-tags=integration)
        ALLOW_SRC="deadcode-testonly-allow.txt"
        SUBTRACT_SRC="deadcode-allow.txt"
        ;;
    *)
        echo "deadcode-gate: unknown mode '$MODE' (known: with-tests, testonly)" >&2
        echo "  the package set is not configurable — see the comment above." >&2
        exit 2
        ;;
esac

PKGS=(./cmd/... ./internal/... ./migrations/...)

if ! command -v deadcode >/dev/null 2>&1; then
    echo "$LABEL: deadcode not found in PATH."
    echo "  install: GOTOOLCHAIN=\$(go -C go env GOVERSION) go install golang.org/x/tools/cmd/deadcode@v0.52.0"
    echo "  (GOTOOLCHAIN: deadcode must be built with the toolchain of go/go.mod, else every package fails with 'requires newer Go version')"
    if [[ -n "${CI:-}" ]]; then
        # In CI a missing tool must not pass as a green gate — CI is the
        # authority (the local hook is only the early warning).
        echo "$LABEL: CI is set — refusing to report ok without running." >&2
        exit 1
    fi
    exit 0
fi

RAW="$(mktemp)"; IST="$(mktemp)"; ALLOW="$(mktemp)"; SUB="$(mktemp)"; KEEP="$(mktemp)"
trap 'rm -f "$RAW" "$IST" "$ALLOW" "$SUB" "$KEEP"' EXIT

# deadcode runs on its OWN line with a redirection, never inside a pipe: under
# `set -o pipefail` a clean tree makes the downstream grep exit 1 and the gate
# goes red although everything is fine; without pipefail a deadcode type error
# (rc != 0 on stderr) gets masked and the gate goes green on a broken run. The
# redirection lets `set -e` see the real exit code either way.
deadcode "${DEADCODE_FLAGS[@]}" "${PKGS[@]}" > "$RAW"

# Both `grep -v` filters carry `|| true` because an EMPTY result is the success
# case here (nothing unreachable / an allowlist made of comments only), and grep
# signals "no lines" with rc 1 — which would kill the script under `set -e`
# exactly when it should report ok. This is not the masking described above:
# the deadcode run itself is already checked, on its own line.
# The node_modules filter is a second line of defence only: the explicit package
# set above cannot reach go/web/** in the first place.
{ grep -v 'web/node_modules/' "$RAW" || true; } \
    | sed 's/^\([^:]*\):[0-9]*:[0-9]*: unreachable func: /\1\t/' \
    | LC_ALL=C sort -u > "$IST"

# testonly mode only: subtract the with-tests allowlist from the findings, so no
# symbol is carried in two policy files (see the header for why the file, not a
# second run, is the exact subtrahend). Both sides are LC_ALL=C-sorted keys of
# two fields, which is what comm needs. `|| true` for the same reason as below:
# an allowlist of comments only is a legal state, not a script death.
if [[ -n "$SUBTRACT_SRC" ]]; then
    { grep -v '^[[:space:]]*\(#\|$\)' "$SUBTRACT_SRC" || true; } \
        | cut -f1,2 | LC_ALL=C sort -u > "$SUB"
    comm -23 "$IST" "$SUB" > "$KEEP"
    cat "$KEEP" > "$IST"
fi

# Comparison key is field 1 + field 2 (path + symbol); the reason column is not
# part of it, and no line number is either — otherwise every shift inside a file
# would redden the gate.
{ grep -v '^[[:space:]]*\(#\|$\)' "$ALLOW_SRC" || true; } \
    | cut -f1,2 | LC_ALL=C sort -u > "$ALLOW"

NEW="$(comm -23 "$IST" "$ALLOW")"
GONE="$(comm -13 "$IST" "$ALLOW")"

RC=0

if [[ -n "$NEW" ]]; then
    echo "$LABEL: NEW unreachable symbol(s) not in the allowlist:"
    printf '%s\n' "$NEW" | sed 's/^/  /'
    echo ""
    echo "  Either cut the symbol, or add a line to go/$ALLOW_SRC:"
    echo "    <path-relative-to-go><TAB><symbol><TAB># <reason>"
    RC=1
fi

if [[ -n "$GONE" ]]; then
    echo "$LABEL: allowlist entr(ies) no longer unreachable — delete the line:"
    printf '%s\n' "$GONE" | sed 's/^/  /'
    RC=1
fi

if [[ $RC -eq 0 ]]; then
    echo "$LABEL: ok ($(wc -l < "$ALLOW" | tr -d ' ') entries, all allowlisted)"
fi

exit $RC
