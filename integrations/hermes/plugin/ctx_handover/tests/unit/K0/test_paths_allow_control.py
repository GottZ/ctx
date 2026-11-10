"""K0: ``paths.check_rel(..., allow_control=True)`` (contract K-4, W095).

The flag admits what git and a file tree reproduce byte for byte on POSIX targets — C0 controls except NUL, DEL,
``surrogateescape`` bytes — and nothing else; cluster E masked those characters before calling ``check_rel``
(``filetree.safe_rel``, ``gitapply.path_issues``). The tests pin the flag to E's verdicts.
"""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import paths
from ctx_handover.engine.errors import PolicyDenied

POSIX = paths.rules_for("linux", False)
MAC = paths.rules_for("darwin", True)
WIN = paths.rules_for("windows", True)

LEGAL = [
    "a\x01b",
    "dir/x\x1f/y",
    "tab\there",
    "nl\nname",
    "del\x7f",
    os.fsdecode(b"caf\xe9.txt"),
    os.fsdecode(b"d\xff\xfe/f"),
    "plain/ok.txt",
]
REFUSED = [
    ("nul\x00x", "control_character"),
    ("c1\x85x", "control_character"),
    ("c1\x9b[31m", "control_character"),
    ("lone\ud800x", "control_character"),
    ("lone" + chr(0xDC7F) + "x", "control_character"),
    ("a/../b", "dot_or_empty_component"),
    (".git/x\x01", "dotgit"),
    (".GIT\x01/../x", "dot_or_empty_component"),
    ("/abs\x01", "absolute"),
    ("", "empty"),
]


@pytest.mark.parametrize("rel", LEGAL)
def test_allow_control_accepts_reproducible_posix_names_unchanged(rel):
    assert paths.check_rel(rel, POSIX, allow_control=True) is rel
    assert paths.check_rel(rel, MAC, allow_control=True) is rel
    if rel != "plain/ok.txt":
        with pytest.raises(PolicyDenied):
            paths.check_rel(rel, POSIX)


@pytest.mark.parametrize(("rel", "reason"), REFUSED)
def test_allow_control_still_refuses(rel, reason):
    with pytest.raises(PolicyDenied) as exc:
        paths.check_rel(rel, POSIX, allow_control=True)
    assert exc.value.details["reason"] == reason


@pytest.mark.parametrize("rel", LEGAL[:-1])
def test_allow_control_has_no_effect_on_windows(rel):
    with pytest.raises(PolicyDenied):
        paths.check_rel(rel, WIN, allow_control=True)


def test_allow_control_lengths_count_escaped_bytes_once():
    one = os.fsdecode(b"\xff")
    assert paths.check_rel(one * 255, POSIX, allow_control=True)
    with pytest.raises(PolicyDenied) as exc:
        paths.check_rel(one * 256, POSIX, allow_control=True)
    assert exc.value.details["reason"] == "component_too_long"
    long = "/".join([one * 200] * 21)
    with pytest.raises(PolicyDenied) as exc:
        paths.check_rel(long, POSIX, allow_control=True)
    assert exc.value.details["reason"] == "too_long"


CORPUS = LEGAL + [r for r, _ in REFUSED if "\x00" not in r and r] + ["x\x01/.git", "ok\x02/y", "a\udcffb/c\x1b"]


@pytest.mark.parametrize("rules", [POSIX, MAC, WIN], ids=["linux", "darwin", "windows"])
def test_allow_control_matches_cluster_e_masking(rules):
    """For every corpus name, ``check_rel(…, allow_control=True)`` accepts exactly what E's file-tree rule
    (``filetree.safe_rel``) and E's git rule (``gitapply.path_issues``) accept today."""
    from ctx_handover.engine import filetree, gitapply

    for rel in CORPUS:
        try:
            paths.check_rel(rel, rules, allow_control=True)
            mine = True
        except PolicyDenied:
            mine = False
        try:
            filetree.safe_rel(os.fsencode(rel), rules)
            tree = True
        except (PolicyDenied, UnicodeEncodeError):
            tree = False
        git = not gitapply.path_issues([rel], rules)
        assert mine == tree == git, rel
    with pytest.raises(PolicyDenied):
        paths.check_rel("nul\x00x", rules, allow_control=True)
    with pytest.raises(PolicyDenied):
        filetree.safe_rel(b"nul\x00x", rules)
