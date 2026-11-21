#!/usr/bin/env bash
# Test matrix for .hooks/pre-commit Gate 3 (schema-contract manifest names
# the staged migration chain, issue #397).
#
#   bash .hooks/pre-commit.test.sh [path/to/pre-commit]
#
# Every case runs in its own throwaway repository: an empty `git init` that
# borrows the objects of the repository this script lives in (alternates),
# points HEAD at that repository's HEAD commit and loads its tree into the
# index — no working tree, no commit, no identity needed. The case then
# stages blobs with `git update-index` and runs the hook against that index,
# the way `git commit` would, reading only its exit code and output.
#
# "Regenerating" the manifest is modelled, not run (the generator needs
# Docker): the model takes HEAD's manifest and rewrites what the generator
# derives from the chain alone — migration_max, migration_count and, when
# the manifest carries it, migration_chain_sha256. For the edits below (a
# new SELECT, a comment, a new file without schema objects) that is what a
# real regeneration changes; schema content is the job of the integration
# test TestCheck_FreshDB_NoDrift, not of the hook.
#
# The fingerprint here is computed one file at a time from the index blobs —
# deliberately not the way the hook computes it — so a case also checks the
# hook's arithmetic against the definition.
set -uo pipefail
unset GIT_DIR GIT_INDEX_FILE GIT_WORK_TREE GIT_PREFIX

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
HOOK=${1:-$HERE/pre-commit}
HOOK=$(cd "$(dirname "$HOOK")" && pwd)/$(basename "$HOOK")
SRC=$(git -C "$HERE" rev-parse --show-toplevel) || exit 2
OBJECTS=$(git -C "$SRC" rev-parse --path-format=absolute --git-common-dir)/objects
BASE=$(git -C "$SRC" rev-parse HEAD) || exit 2
MANIFEST=go/internal/schemacontract/manifest.json
CHAIN=':(glob)go/migrations/*.sql'

WORK=$(mktemp -d) || exit 2
trap 'rm -rf "$WORK"' EXIT

sha256_hex() {
    if command -v sha256sum >/dev/null 2>&1; then sha256sum | cut -d' ' -f1
    else shasum -a 256 | cut -d' ' -f1; fi
}

new_repo() {
    local d
    d=$(mktemp -d "$WORK/case.XXXXXX") || return 1
    git init -q "$d" || return 1
    echo "$OBJECTS" > "$d/.git/objects/info/alternates"
    git -C "$d" update-ref HEAD "$BASE" && git -C "$d" read-tree HEAD || return 1
    echo "$d"
}

# stage <repo> <path> <file with the new content>
stage() {
    local blob
    blob=$(git -C "$1" hash-object -w "$3") || return 1
    git -C "$1" update-index --add --cacheinfo "100644,$blob,$2"
}

# stage_append <repo> <path> <text>: stage the indexed file plus one line.
stage_append() {
    local f="$WORK/blob.$$"
    { git -C "$1" show ":$2"; printf '%s\n' "$3"; } > "$f" && stage "$1" "$2" "$f"
}

# chain_sha256 <repo>: sha256 over "<sha256 of the file>  <name>" lines of
# every go/migrations/*.sql in the index, in byte order of the names.
chain_sha256() {
    git -C "$1" ls-files -s -- "$CHAIN" | while read -r _mode blob _stage path; do
        printf '%s  %s\n' "$(git -C "$1" cat-file blob "$blob" | sha256_hex)" "${path##*/}"
    done | sha256_hex
}

# regen_manifest <repo> <out>: the generator's chain-derived fields over
# HEAD's manifest (see the header).
regen_manifest() {
    local names count max fp
    names=$(git -C "$1" ls-files -- "$CHAIN" | sed 's|.*/||')
    count=$(printf '%s\n' "$names" | grep -c .)
    max=$(printf '%s\n' "$names" | sed 's/_.*//' | sed 's/^0*//' | sort -n | tail -1)
    fp=$(chain_sha256 "$1")
    git -C "$1" show "HEAD:$MANIFEST" | sed \
        -e "s/^\([[:space:]]*\"migration_max\":[[:space:]]*\)[0-9]*/\1$max/" \
        -e "s/^\([[:space:]]*\"migration_count\":[[:space:]]*\)[0-9]*/\1$count/" \
        -e "s/^\([[:space:]]*\"migration_chain_sha256\":[[:space:]]*\"\)[0-9a-f]*/\1$fp/" > "$2"
}

stage_regen_manifest() {
    regen_manifest "$1" "$WORK/manifest.$$" && stage "$1" "$MANIFEST" "$WORK/manifest.$$"
}

# The newest migration of the chain at HEAD: the file a branch edits.
newest() {
    git -C "$1" ls-files -- "$CHAIN" | tail -1
}

PASSED=0
FAILED=0
# run_case <name> <want: accept|reject> <setup function>
run_case() {
    local name=$1 want=$2 setup=$3 d out rc got
    d=$(new_repo) || { echo "FAIL $name: setup of the throwaway repo"; FAILED=$((FAILED + 1)); return; }
    if ! "$setup" "$d"; then
        echo "FAIL $name: case setup"; FAILED=$((FAILED + 1)); return
    fi
    out=$(cd "$d" && sh "$HOOK" 2>&1)
    rc=$?
    if [ "$rc" = 0 ]; then got=accept; else got=reject; fi
    if [ "$got" = reject ] && ! printf '%s\n' "$out" | grep -q 'REJECTED'; then
        got="error(rc=$rc)"
    fi
    if [ "$got" = "$want" ]; then
        echo "ok   $name: $got"; PASSED=$((PASSED + 1))
    else
        echo "FAIL $name: want $want, got $got"
        printf '%s\n' "$out" | sed 's/^/       | /'
        FAILED=$((FAILED + 1))
    fi
}

case_ordinary() { stage_append "$1" README.md 'pre-commit test matrix'; }

case_new_migration_with_manifest() {
    printf 'SELECT 1;\n' > "$WORK/new.sql"
    stage "$1" go/migrations/999_precommit_test_matrix.sql "$WORK/new.sql" && stage_regen_manifest "$1"
}

case_new_migration_without_manifest() {
    printf 'SELECT 1;\n' > "$WORK/new.sql"
    stage "$1" go/migrations/999_precommit_test_matrix.sql "$WORK/new.sql"
}

# A logic edit that leaves the schema as it was: the issue's case.
case_neutral_edit_regenerated() {
    stage_append "$1" "$(newest "$1")" 'SELECT 1;' && stage_regen_manifest "$1"
}

case_neutral_edit_not_regenerated() {
    stage_append "$1" "$(newest "$1")" 'SELECT 1;'
}

case_comment_edit_not_regenerated() {
    stage_append "$1" "$(newest "$1")" '-- pre-commit test matrix'
}

case_comment_edit_regenerated() {
    stage_append "$1" "$(newest "$1")" '-- pre-commit test matrix' && stage_regen_manifest "$1"
}

# The manifest was generated from another edit of the same file than the
# one staged.
case_manifest_of_other_edit() {
    local m
    m=$(newest "$1")
    stage_append "$1" "$m" 'SELECT 2;' && stage_regen_manifest "$1" || return 1
    { git -C "$1" show "HEAD:$m"; printf 'SELECT 1;\n'; } > "$WORK/other.sql" && stage "$1" "$m" "$WORK/other.sql"
}

# The manifest was generated from a working tree whose migration edit is not
# staged: only the manifest is.
case_manifest_alone_from_unstaged_edit() {
    local m
    m=$(newest "$1")
    stage_append "$1" "$m" 'SELECT 1;' && stage_regen_manifest "$1" || return 1
    git -C "$1" update-index --cacheinfo "100644,$(git -C "$1" rev-parse "HEAD:$m"),$m"
}

case_deleted_migration_not_regenerated() {
    git -C "$1" update-index --force-remove "$(newest "$1")"
}

# An .sql file below go/migrations/ is not part of the chain: the embedded
# FS (go:embed *.sql) holds the top level only.
case_nested_sql_not_in_chain() {
    printf 'SELECT 1;\n' > "$WORK/nested.sql"
    stage "$1" go/migrations/testdata/precommit_test_matrix.sql "$WORK/nested.sql"
}

echo "pre-commit Gate 3 matrix — hook: $HOOK, base: $BASE"
run_case ordinary_commit                    accept case_ordinary
run_case new_migration_with_manifest        accept case_new_migration_with_manifest
run_case new_migration_without_manifest     reject case_new_migration_without_manifest
run_case neutral_edit_regenerated           accept case_neutral_edit_regenerated
run_case neutral_edit_not_regenerated       reject case_neutral_edit_not_regenerated
run_case comment_edit_not_regenerated       reject case_comment_edit_not_regenerated
run_case comment_edit_regenerated           accept case_comment_edit_regenerated
run_case manifest_of_other_edit             reject case_manifest_of_other_edit
run_case manifest_alone_from_unstaged_edit  reject case_manifest_alone_from_unstaged_edit
run_case deleted_migration_not_regenerated  reject case_deleted_migration_not_regenerated
run_case nested_sql_not_in_chain            accept case_nested_sql_not_in_chain

# The checked-in manifest names the checked-in chain: the Go generator and
# the definition above agree on the real files.
d=$(new_repo)
have=$(git -C "$d" show "HEAD:$MANIFEST" | sed -n 's/^[[:space:]]*"migration_chain_sha256":[[:space:]]*"\([0-9a-f]*\)".*/\1/p')
want=$(chain_sha256 "$d")
if [ -n "$have" ] && [ "$have" = "$want" ]; then
    echo "ok   head_manifest_names_head_chain: $want"; PASSED=$((PASSED + 1))
else
    echo "FAIL head_manifest_names_head_chain: manifest ${have:-(none)}, chain $want"; FAILED=$((FAILED + 1))
fi

echo "$PASSED passed, $FAILED failed"
[ "$FAILED" = 0 ]
