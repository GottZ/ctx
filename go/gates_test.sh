#!/usr/bin/env bash
# shellcheck disable=SC2086,SC2329,SC2317,SC2015,SC1090 # value lists are split on purpose; the fakes are called by the sourced script; shellcheck follows gates.sh to its final exit, sourced it returns before; a && ok || bad is the assertion idiom; the settings cases source gates.sh by its variable path
#
# Self-test of the integration room check in go/gates.sh (#337): the slot
# arithmetic per bound, the start order, the host-wide slot ledger and the
# integration step's scheduler — without a real test run. Every host value is
# passed in or faked (room_measure, room_docker_fs and a `go` on PATH that only
# sleeps and prints a result line).
#
# Usage: bash go/gates_test.sh   (gates.sh runs it in its selfcheck step whenever
#                                 go/gates.sh or this file changes)
#
# Sources gates.sh for its room functions only: gates.sh returns right after
# them when it is sourced rather than executed.

set -uo pipefail

TEST_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GATES="$TEST_DIR/gates.sh"
FUNCS="room_check room_plan room_rank room_mem_need ledger_sum room_take proc_start step_integ"

# the starting values are under test: no CTX_GATES_* of the caller may move them
for v in $(compgen -v CTX_GATES_); do unset "$v"; done

if ! ( source "$GATES" && declare -F $FUNCS ) >/dev/null 2>&1; then
  echo "[FATAL] sourcing $GATES does not define the room functions ($FUNCS)"
  exit 1
fi
# shellcheck source=./gates.sh
source "$GATES"
set +e

T="$(mkdir -p "$REPO/.gotmp" && mktemp -d "$REPO/.gotmp/gates-test.XXXXXX")" || { echo "[FATAL] no temp dir"; exit 1; }
SLEEPER=""
cleanup() { [ -n "$SLEEPER" ] && kill "$SLEEPER" 2>/dev/null; rm -rf "$T"; }
trap cleanup EXIT

PASS=0; FAIL=0
ok() { PASS=$((PASS + 1)); echo "ok   $1"; }
bad() { FAIL=$((FAIL + 1)); echo "FAIL $1"; shift; printf '       %s\n' "$@"; }
# expect <name> <text> <fixed string>… — every string occurs in the text
expect() {
  local name=$1 text=$2 s; shift 2
  for s in "$@"; do
    case "$text" in *"$s"*) ;; *) bad "$name" "missing: $s" "got:" "$text"; return ;; esac
  done
  ok "$name"
}
# reject <name> <text> <fixed string>… — no string occurs in the text
reject() {
  local name=$1 text=$2 s; shift 2
  for s in "$@"; do
    case "$text" in *"$s"*) bad "$name" "unexpected: $s" "got:" "$text"; return ;; esac
  done
  ok "$name"
}
same() { if [ "$2" = "$3" ]; then ok "$1"; else bad "$1" "want: $3" "got:  $2"; fi; }

H=./internal/handler/; E=./internal/events/; S=./internal/store/
R=8192 # the memory reserve, MB

echo "== starting values"
same "slot memory of a measured package (MB)" "$(room_mem_need "$H")" 3072
same "slot memory of an unmeasured package (MB)" "$(room_mem_need ./internal/guard/)" 5120
same "memory reserve (MB)" "$ROOM_MEM_RESERVE_MB" "$R"
same "disk per slot / reserve (GB)" "$ROOM_DISK_GB/$ROOM_DISK_RESERVE_GB" "3/120"
same "cores per slot" "$ROOM_CPU" 2
same "serial switch off" "$ROOM_SERIAL" 0

echo "== start order: longest first, unknown packages after the ranked ones in their given order"
same "rank" "$(room_rank ./internal/zeta/ "$S" ./internal/alpha/ "$E" ./cmd/ctxd/ "$H")" "$H $E $S ./cmd/ctxd/ ./internal/zeta/ ./internal/alpha/"

echo "== one bound decides (room_plan <MemAvailable MB> <free GB> <load1> <cores> <reserved mem MB> <reserved GB> <reserved cores> <pkg…>)"
out=$(room_plan 40000 300 1.00 12 0 0 0 "$S" "$E" "$H")
expect "all fit: three at once, handler first" "$out" "plan: 3 of 3 packages at once now" "start $H" "start $E" "start $S"
same "all fit: start order" "$(grep -o 'start ./internal/[a-z]*/' <<<"$out" | tr '\n' ' ')" "start $H start $E start $S "

out=$(room_plan $((R + 2 * 3072 + 100)) 300 1.00 12 0 0 0 "$H" "$E" "$S")
expect "memory decides: two of three" "$out" "plan: 2 of 3 packages at once now" "bound=mem" "queue $S"

out=$(room_plan 40000 $((120 + 2 * 3 + 1)) 1.00 12 0 0 0 "$H" "$E" "$S")
expect "disk decides: two of three" "$out" "plan: 2 of 3 packages at once now" "bound=disk" "queue $S"

out=$(room_plan 40000 300 6.50 12 0 0 0 "$H" "$E" "$S")
expect "load decides: (12 - 6.5) / 2 -> two of three" "$out" "plan: 2 of 3 packages at once now" "bound=cpu" "queue $S"

out=$(room_plan $((R + 3072 + 500)) 300 1.00 12 0 0 0 "$H" "$E" "$S")
expect "room for one slot: serial, memory named with its values" "$out" "plan: 1 of 3 packages at once now" "bound=mem" "serial" "mem: $((R + 3072 + 500)) avail - $R reserve - 3072 reserved = 500 MB"

out=$(room_plan 40000 300 18.11 12 0 0 0 "$H" "$E" "$S")
expect "load above the cores: the first package starts anyway, the rest waits (serial)" "$out" "plan: 1 of 3 packages at once now" "bound=cpu" "serial" "start $H"

out=$(ROOM_SERIAL=1 room_plan 40000 300 1.00 12 0 0 0 "$H" "$E" "$S")
expect "CTX_GATES_SERIAL=1: one at a time although there is room" "$out" "plan: 1 of 3 packages at once now" "bound=serial"

out=$(room_plan $((R - 384)) 300 1.00 12 0 0 0 "$H" "$E" "$S")
expect "below the memory reserve: no room for even one" "$out" "plan: 0 of 3 packages at once now" "bound=mem" "no room"

out=$(room_plan 40000 119 1.00 12 0 0 0 "$H" "$E" "$S")
expect "below the disk reserve: no room for even one" "$out" "plan: 0 of 3 packages at once now" "bound=disk" "no room"

out=$(room_plan $((R + 4000)) 300 1.00 12 0 0 0 ./internal/guard/)
expect "an unmeasured package needs 5120 MB" "$out" "plan: 0 of 1 packages at once now" "bound=mem"
out=$(room_plan $((R + 4000)) 300 1.00 12 0 0 0 "$H")
expect "a measured package fits into 4000 MB" "$out" "plan: 1 of 1 packages at once now"

echo "== room_check verdicts"
same "own package running, memory short: wait (not 'none')" "$(room_check $((R + 100)) 300 1.00 12 0 0 0 1 3072 | cut -d' ' -f1-2)" "wait mem"
same "nothing running, memory short: none" "$(room_check $((R + 100)) 300 1.00 12 0 0 0 0 3072 | cut -d' ' -f1-2)" "none mem"
same "memory and disk short: both named" "$(room_check $((R + 100)) 100 1.00 12 0 0 0 0 3072 | cut -d' ' -f1-2)" "none mem+disk"
same "load short with an own package running: wait cpu" "$(room_check 40000 300 11.00 12 0 0 0 1 3072 | cut -d' ' -f1-2)" "wait cpu"
same "memory and load short: both named" "$(room_check $((R + 100)) 300 11.00 12 0 0 0 1 3072 | cut -d' ' -f1-2)" "wait mem+cpu"
same "load short, nothing of this run running: the first starts, marked" "$(room_check 40000 300 11.00 12 0 0 0 0 3072 | cut -d' ' -f1-2)" "start cpu-first"

echo "== the host-wide ledger: other runs' reservations, dead entries, the CPU a slot has not shown yet"
L="$T/ledger"; mkdir -p "$L"; ROOM_LEDGER="$L"
sleep 300 & SLEEPER=$!
sleep 0.2
ls_start=$(proc_start "$SLEEPER")
now=$(date +%s)
printf '%s %s 6144 6 2 %s ./internal/handler/ /other/run\n' "$SLEEPER" "$ls_start" "$now" > "$L/slot-1-1"
ls_sum=$(ledger_sum 0) # the cores decay with the entry's age (e^(-age/60 s)): a second may pass between the entry and the sum
same "a live foreign slot counts with its whole memory and disk budget" "${ls_sum% *}" "1 6144 6"
case "${ls_sum##* }" in 2.00|1.9[0-9]) ok "a fresh foreign slot counts with (nearly) all of its cores" ;; *) bad "a fresh foreign slot counts with (nearly) all of its cores" "want: 1.90–2.00" "got:  ${ls_sum##* }" ;; esac
out=$(room_plan 16808 300 1.00 12 6144 6 2.00 "$H" "$E" "$S")
expect "foreign reservation decides: 0 instead of 2 slots" "$out" "plan: 0 of 3 packages at once now" "bound=mem" "- 6144 reserved"
out=$(room_plan 16808 300 1.00 12 0 0 0 "$H" "$E" "$S")
expect "the same host without it: 2 slots" "$out" "plan: 2 of 3 packages at once now"
out=$(room_plan 40000 300 6.50 12 0 0 2.00 "$H" "$E" "$S")
expect "a young foreign slot's CPU counts too: 1 instead of 2" "$out" "plan: 1 of 3 packages at once now" "bound=cpu"

printf '%s %s 6144 6 2 %s ./internal/handler/ /other/run\n' "$SLEEPER" "$ls_start" "$((now - 600))" > "$L/slot-1-1"
same "a slot 600 s old: its CPU is in load1 by now, memory and disk stay reserved" "$(ledger_sum 0)" "1 6144 6 0.00"

true & dead=$!; wait "$dead"
printf '%s 12345 3072 3 2 %s ./internal/store/ /dead/run\n' "$dead" "$now" > "$L/slot-2-1"
printf '%s 1 3072 3 2 %s ./internal/events/ /reused/pid\n' "$SLEEPER" "$now" > "$L/slot-3-1"
same "a dead entry and a reused pid do not count" "$(ledger_sum 0)" "1 6144 6 0.00"
[ -f "$L/slot-2-1" ] && [ -f "$L/slot-3-1" ] && ok "read without the lock (explain) removes nothing" || bad "read without the lock (explain) removes nothing" "$(ls "$L")"
ledger_sum 1 >/dev/null
[ ! -f "$L/slot-2-1" ] && [ ! -f "$L/slot-3-1" ] && [ -f "$L/slot-1-1" ] && ok "under the lock the dead entries are removed, the live one stays" || bad "under the lock the dead entries are removed, the live one stays" "$(ls "$L")"
printf '%s 0 3072 3 2 %s ./internal/events/ /no/start/time\n' "$SLEEPER" "$now" > "$L/slot-4-1"
same "an entry whose process had ended before it was followed (start time 0) does not count" "$(ledger_sum 0)" "1 6144 6 0.00"
printf '%s 12345 3072 3 2 %s ./internal/store/ /dead/run\n' "$dead" "$now" > "$L/slot-2-1"
room_measure() { echo "40000 300 1.00 12"; }
RUN_DIR="$T/clean"
room_take "$SLEEPER" slot-9-1 "$H" 3072 0 >/dev/null
[ ! -f "$L/slot-2-1" ] && [ ! -f "$L/slot-4-1" ] && [ -f "$L/slot-9-1" ] && ok "a start decision removes dead entries and writes its own" || bad "a start decision removes dead entries and writes its own" "$(ls "$L")"
same "the new entry belongs to the deciding process, budget 3072 MB 3 GB 2 cores" "$(cut -d' ' -f1,3-5,7 "$L/slot-9-1")" "$SLEEPER 3072 3 2 $H"

echo "== the ledger directory: only one nobody else can write to"
mkdir -p "$T/lok" "$T/lbad"; chmod 755 "$T/lok"; chmod 777 "$T/lbad"; ln -s "$T/lok" "$T/llink"
room_ledger_ok "$T/lok" && ok "own directory, 755: used" || bad "own directory, 755: used"
room_ledger_ok "$T/lbad" && bad "writable by others (777): not used" || ok "writable by others (777): not used"
room_ledger_ok "$T/llink" && bad "a symlink: not used" || ok "a symlink: not used"

echo "== two runs decide at the same moment: the lock lets only one of them take the last slot"
find "$L" -maxdepth 1 -name 'slot-*' -delete
RUN_DIR="$T/race"
room_measure() { sleep 1; echo "$((R + 3072 + 500)) 300 1.00 12"; }
room_take "$SLEEPER" slot-a-1 "$H" 3072 0 > "$T/take-a" &
pa=$!
room_take "$SLEEPER" slot-b-1 "$E" 3072 0 > "$T/take-b" &
pb=$!
wait "$pa" "$pb"
starts=$(cat "$T/take-a" "$T/take-b" | grep -c '^start ')
same "exactly one of two simultaneous decisions starts" "$starts" 1
expect "the other one sees the first one's reservation" "$(cat "$T/take-a" "$T/take-b")" "none mem" "3072 reserved"
same "one ledger entry" "$(find "$L" -maxdepth 1 -name 'slot-*' | wc -l | tr -d ' ')" 1
hit=0
for i in 1 2 3; do
  find "$L" -maxdepth 1 -name 'slot-*' -delete
  room_measure() { : > "$T/in-measure"; sleep 0.5; echo "40000 300 1.00 12"; }
  room_take "$SLEEPER" "slot-e-$i" "$H" 3072 0 > /dev/null &
  pe=$!
  until [ -f "$T/in-measure" ]; do sleep 0.02; done; rm -f "$T/in-measure"
  flock -x "$L/.lock" -c "[ -f '$L/slot-e-$i' ]" && hit=$((hit + 1))
  wait "$pe"
done
same "the entry is on disk before the decision lets the lock go (3 of 3)" "$hit" 3

echo "== a ledger lock that cannot be opened holds the start back (fail-closed)"
mkdir -p "$T/badledger/.lock"
same "nothing of this run running: none, with the reason" "$(ROOM_LEDGER="$T/badledger" room_take "$SLEEPER" slot-c-1 "$S" 3072 0 | cut -d' ' -f1-2)" "none lock"
same "an own package running: wait" "$(ROOM_LEDGER="$T/badledger" room_take "$SLEEPER" slot-c-2 "$S" 3072 1 | cut -d' ' -f1-2)" "wait lock"
same "no entry written" "$(find "$T/badledger" -maxdepth 1 -name 'slot-*' | wc -l | tr -d ' ')" 0

echo "== the integration step end to end (fake go, faked host values)"
mkdir -p "$T/bin"
cat > "$T/bin/go" <<'EOF'
#!/usr/bin/env bash
pkg="${!#}"
echo "$(date +%s.%N) start $pkg $GOTMPDIR $TMPDIR ARGV=$*" >> "$FAKE_TRACE"
sleep "${FAKE_SLEEP:-1}"
f=no; grep -qs "^$$ " "$FAKE_LEDGER"/slot-* && f=yes
echo "$(date +%s.%N) end $pkg followed=$f" >> "$FAKE_TRACE"
case "$pkg" in
  *red*) printf -- '--- FAIL: TestRed (0.00s)\nFAIL\tgithub.com/GottZ/ctx/%s\t1.000s\n' "${pkg#./}"; exit 1 ;;
esac
printf 'ok  \tgithub.com/GottZ/ctx/%s\t1.000s\n' "${pkg#./}"
EOF
chmod +x "$T/bin/go"
export PATH="$T/bin:$PATH" FAKE_LEDGER="$L"
room_docker_fs() { echo /; }
ROOM_POLL=0.2
ROOM_WAIT=10 # a broken scheduler fails within seconds instead of waiting the default 1800 s
# most packages running at once, from the trace
maxconc() { sort -n "$1" | awk '$2 == "start" { n++; if (n > m) m = n } $2 == "end" { n-- } END { print m + 0 }'; }
run_step() { # run_step <name> <pkg…>: step_integ as the main loop runs it; rc in STEP_RC
  RUN_DIR="$T/$1"; mkdir -p "$RUN_DIR"; shift
  export FAKE_TRACE="$RUN_DIR/trace"; : > "$FAKE_TRACE"
  GOTMPDIR="$RUN_DIR/gotmp"; TMPDIR="$RUN_DIR/tmp"; mkdir -p "$GOTMPDIR" "$TMPDIR"
  (cd "$T" && step_integ "$@") > "$RUN_DIR/step.log" 2>&1 3> "$RUN_DIR/console"
  STEP_RC=$?
}

find "$L" -maxdepth 1 -name 'slot-*' -delete
room_measure() { echo "40000 300 1.00 12"; }
run_step green "$S" "$E" "$H"
same "all green: rc 0" "$STEP_RC" 0
same "all green: three at once" "$(maxconc "$T/green/trace")" 3
log=$(cat "$T/green/step.log")
expect "the step log lists every package and its result" "$log" \
  "integration: $H rc=0 ok" "integration: $E rc=0 ok" "integration: $S rc=0 ok" \
  "ok  	github.com/GottZ/ctx/internal/handler/" "ok  	github.com/GottZ/ctx/internal/store/" \
  "integration: 3 packages: 3 ok, 0 failed, 0 not started"
expect "the step log names each package's own log" "$log" "log=$T/green/integ/internal_handler.log" "log=$T/green/integ/internal_store.log"
[ -s "$T/green/integ/internal_events.log" ] && ok "each package has its own log" || bad "each package has its own log" "$(ls "$T/green/integ")"
same "each package has its own GOTMPDIR and TMPDIR" "$(awk '$2 == "start" { print $4; print $5 }' "$T/green/trace" | sort -u | wc -l | tr -d ' ')" 6
expect "the console shows the starts and the summary" "$(cat "$T/green/console")" "gates: integration: start $H" "gates: integration: 3 packages: 3 ok"
same "no ledger entry is left behind" "$(find "$L" -maxdepth 1 -name 'slot-*' | wc -l | tr -d ' ')" 0
same "go test gets the integration flags" "$(grep -o 'ARGV=.*' "$T/green/trace" | sort -u | head -1)" "ARGV=test -tags=integration -count=1 -p 1 -timeout 60m $E"
same "while it runs, the ledger entry lives on the go test process (3 of 3)" "$(grep -c 'followed=yes' "$T/green/trace")" 3

run_step red "$H" ./internal/red/
same "a red package: rc 1" "$STEP_RC" 1
expect "the summary names the red package with rc, time and log" "$(cat "$T/red/step.log")" \
  "integration: ./internal/red/ rc=1 FAIL" "log=$T/red/integ/internal_red.log" "--- FAIL: TestRed" \
  "integration: 2 packages: 1 ok, 1 failed, 0 not started"

room_measure() { echo "$((R + 3072 + 10)) 300 1.00 12"; }
run_step serial "$S" "$E" "$H"
same "room for one: rc 0" "$STEP_RC" 0
same "room for one: one at a time" "$(maxconc "$T/serial/trace")" 1
expect "room for one: the step says it runs serially and why" "$(cat "$T/serial/console")" "serial" "bound=mem"
same "the step starts longest first" "$(awk '$2 == "start" { print $3 }' "$T/serial/trace" | tr '\n' ' ')" "$H $E $S "

room_measure() { echo "40000 300 1.00 12"; }
ROOM_SERIAL=1 run_step serialswitch "$S" "$E" "$H"
same "CTX_GATES_SERIAL=1 in the step: one at a time" "$(maxconc "$T/serialswitch/trace")" 1
expect "CTX_GATES_SERIAL=1 in the step: named" "$(cat "$T/serialswitch/console")" "bound=serial"

room_measure() { echo "40000 300 11.00 12"; }
run_step cpu "$S" "$E" "$H"
same "load 11 of 12 in the step: one at a time, rc 0" "$(maxconc "$T/cpu/trace") $STEP_RC" "1 0"
expect "load 11 of 12 in the step: named" "$(cat "$T/cpu/console")" "bound=cpu"

room_measure() { echo "$((R + 4000)) 300 1.00 12"; }
ROOM_WAIT=1
run_step unmeasured "$H" ./internal/guard/
same "the step books 5120 MB for an unmeasured package: it does not fit into 4000" "$STEP_RC $(cat "$T/unmeasured/integ/not-started")" "6 ./internal/guard/"
ROOM_WAIT=10

room_measure() { echo "$((R + 3072 + 100)) 300 1.00 12"; }
sleep 3 & F=$!
printf '%s %s 3072 3 2 %s ./internal/x/ /other/run\n' "$F" "$(proc_start "$F")" "$(date +%s)" > "$L/slot-foreign-1"
ROOM_WAIT=20
run_step foreign "$H"
same "a foreign slot holds the room; when it ends, the waiting package starts" "$STEP_RC $(grep -c ' start ' "$T/foreign/trace")" "0 1"
expect "the wait names the foreign reservation" "$(cat "$T/foreign/console")" "gates: integration: waiting" "3072 reserved"
ROOM_WAIT=10
wait "$F" 2>/dev/null

room_measure() { echo "1000 300 1.00 12"; }
ROOM_WAIT=2
s0=$(date +%s)
run_step noroom "$H" "$E"
same "no room: rc 6, neither green nor red" "$STEP_RC" 6
[ $(( $(date +%s) - s0 )) -ge 2 ] && ok "no room: it waited CTX_GATES_ROOM_WAIT first" || bad "no room: it waited CTX_GATES_ROOM_WAIT first" "returned after $(( $(date +%s) - s0 )) s"
same "no room: nothing ran" "$(wc -l < "$T/noroom/trace" | tr -d ' ')" 0
expect "no room: the summary says not started, with the bound" "$(cat "$T/noroom/step.log")" \
  "integration: $H not started (no room)" "integration: 2 packages: 0 ok, 0 failed, 2 not started" "bound=mem"
expect "no room: the waiting is announced on the console" "$(cat "$T/noroom/console")" "gates: integration: waiting"
same "no room: the not-started list for the main loop" "$(cat "$T/noroom/integ/not-started")" "$H $E"
reject "no room: no go test result line" "$(cat "$T/noroom/step.log")" "rc=0 ok" "rc=1 FAIL"
ROOM_WAIT=10

( room_check() { echo "wait cpu values"; }; ROOM_WAIT=1; run_step waitguard "$H"; exit "$STEP_RC" ) & wp=$!
for _ in $(seq 100); do kill -0 "$wp" 2>/dev/null || break; sleep 0.1; done
if kill -0 "$wp" 2>/dev/null; then kill "$wp"; wait "$wp" 2>/dev/null; r="still waiting after 10 s"; else wait "$wp"; r=$?; fi
same "a 'wait' while nothing of this run runs cannot resolve: the deadline applies (rc 6)" "$r" 6

echo "== the main loop's reading of a step's rc and the run's exit code"
step_is_red integration 6 && bad "integration rc 6 (no room) is not red" || ok "integration rc 6 (no room) is not red"
step_is_red integration 1 && ok "integration rc 1 is red" || bad "integration rc 1 is red"
step_is_red race 6 && ok "another step's rc 6 is red" || bad "another step's rc 6 is red"
step_is_red build 0 && bad "rc 0 is not red" || ok "rc 0 is not red"
same "the not-started list comes from the step" "$(room_noroom integration 6 "$T/noroom")" "$H $E"
same "rc 6 without the list still counts as no room" "$(room_noroom integration 6 "$T/green")" "(see the integration step log)"
same "a green integration step: no room-list" "$(room_noroom integration 0 "$T/green")" ""
same "exit: green" "$(gates_exit 0 "")" 0
same "exit: no room only -> 6" "$(gates_exit 0 "$H")" 6
same "exit: a red tier wins over no room" "$(gates_exit 2 "$H")" 2
same "exit: red tier" "$(gates_exit 1 "")" 1

echo "== settings are whole numbers without leading zeros"
for bad in abc 0900 -1 ""; do
  ( CTX_GATES_ROOM_WAIT="$bad"; source "$GATES" ) >/dev/null 2>&1; r=$?
  if [ -z "$bad" ]; then same "CTX_GATES_ROOM_WAIT empty: the default" "$r" 0; else same "CTX_GATES_ROOM_WAIT=$bad: usage error 64" "$r" 64; fi
done
( CTX_GATES_ROOM_WAIT=0; source "$GATES" ) >/dev/null 2>&1; same "CTX_GATES_ROOM_WAIT=0: accepted" "$?" 0
( CTX_GATES_SLOT_CPU=0; source "$GATES" ) >/dev/null 2>&1; same "CTX_GATES_SLOT_CPU=0: usage error 64" "$?" 64

echo
echo "gates_test: $PASS passed, $FAIL failed"
[ "$FAIL" -eq 0 ]
