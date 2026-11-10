"""K0: additive policy surface of layer 2 (contract K-4; W074, W093, W096) — real files in tmp.

``check_source_file(..., follow_link=…)`` decouples "follow a symlink" from "lift the size limit";
``sniff_bytes`` is the in-memory form of the content sniff; ``exclude_pathspecs`` stays, deprecated.
"""

from __future__ import annotations

import os

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import Verdict
from ctx_handover.tests.unit.K0.test_policy import OPENSSH_KEY, Layout, _write

OK = Verdict(True, None)
LIMIT = 100
EC_KEY_HEAD = "-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n"


@pytest.fixture
def lay(tmp_path) -> Layout:
    home = tmp_path / "u"
    root = home / ".hermes"
    profile = root / "profiles" / "work"
    project = home / "proj"
    plugin_data = profile / "plugin-data" / "ctx_handover"
    for d in (root, profile, project, plugin_data):
        d.mkdir(parents=True, exist_ok=True)
    return Layout(home, root, profile, project, plugin_data)


def _cfg(lay: Layout, **over):
    return lay.cfg(options=Options(max_file_bytes=LIMIT), **over)


def _link(lay: Layout, name: str, target) -> os.PathLike:
    link = lay.project / name
    os.symlink(target, link)
    return link


def test_follow_link_none_keeps_the_explicit_coupling(lay):
    target = _write(lay.home / "docs" / "small.txt", "small\n")
    link = _link(lay, "small-link", target)
    cfg = _cfg(lay)
    assert policy.check_source_file(link, cfg, explicit=False) == Verdict(False, "symlink")
    assert policy.check_source_file(link, cfg, explicit=True) == OK
    assert policy.check_source_file(link, cfg, explicit=False, follow_link=None) == Verdict(False, "symlink")


def test_follow_link_true_follows_without_lifting_the_size_limit(lay):
    """★ ``follow_link=True`` resolves a link for a non-explicit path, and the size limit still applies; only
    ``explicit`` lifts it (W074: the two meanings of ``explicit`` are separate).

    Mutants: ``follow = explicit`` (``follow_link`` ignored) and ``… and not (explicit or follow)`` in the size
    check (following lifts the limit).
    """
    small = _link(lay, "small-link", _write(lay.home / "docs" / "small.txt", "small\n"))
    big = _link(lay, "big-link", _write(lay.home / "docs" / "big.txt", "x" * (LIMIT + 1)))
    cfg = _cfg(lay)
    assert policy.check_source_file(small, cfg, explicit=False, follow_link=True) == OK
    assert policy.check_source_file(big, cfg, explicit=False, follow_link=True) == Verdict(False, "size")
    assert policy.check_source_file(big, cfg, explicit=True, follow_link=True) == OK
    assert policy.check_source_file(big, cfg, explicit=True) == OK


def test_follow_link_false_refuses_a_link_even_when_explicit(lay):
    target = _write(lay.home / "docs" / "small.txt", "small\n")
    link = _link(lay, "small-link", target)
    cfg = _cfg(lay)
    assert policy.check_source_file(link, cfg, explicit=True, follow_link=False) == Verdict(False, "symlink")
    assert policy.check_source_file(target, cfg, explicit=True, follow_link=False) == OK
    big = _write(lay.project / "big.txt", "x" * (LIMIT + 1))
    assert policy.check_source_file(big, cfg, explicit=True, follow_link=False) == OK


def test_followed_link_keeps_denylist_location_and_sniff(lay, tmp_path):
    cfg = _cfg(lay)
    ssh = _link(lay, "cfg", _write(lay.home / ".ssh" / "config", "Host x\n"))
    assert policy.check_source_file(ssh, cfg, explicit=False, follow_link=True) == Verdict(False, "secret")
    outside = _link(lay, "outside", _write(tmp_path / "elsewhere" / "f.txt", "x\n"))
    assert policy.check_source_file(outside, cfg, explicit=False, follow_link=True) == Verdict(False, "outside_home")
    inside_root = policy.check_source_file(outside, cfg, explicit=False, follow_link=True, item_root=lay.project)
    assert inside_root == Verdict(False, "symlink")
    key = _link(lay, "key", _write(lay.home / "docs" / "notes.txt", EC_KEY_HEAD))
    assert policy.check_source_file(key, cfg, explicit=False, follow_link=True) == Verdict(False, "secret")
    assert policy.check_source_file(key, cfg, explicit=True, follow_link=True) == Verdict(False, "secret")


def test_check_extra_passes_follow_link_through(lay):
    link = _link(lay, "extra-link", _write(lay.home / "docs" / "small.txt", "small\n"))
    cfg = _cfg(lay)
    assert policy.check_extra(link, cfg, explicit=False, redact=lambda t: t) == Verdict(False, "symlink")
    assert policy.check_extra(link, cfg, explicit=False, redact=lambda t: t, follow_link=True) == OK
    assert policy.check_extra(link, cfg, explicit=True, redact=lambda t: t, follow_link=False) == Verdict(
        False, "symlink"
    )
    token = _link(lay, "token-link", _write(lay.home / "docs" / "t.txt", "API_TOKEN=abc123\n"))
    redact = lambda t: t.replace("abc123", "***")  # noqa: E731 - a stand-in redactor
    assert policy.check_extra(token, cfg, explicit=False, redact=redact, follow_link=True) == Verdict(
        True, "suspected_secret"
    )


HEADS = [
    OPENSSH_KEY.encode(),
    b"header\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n",
    b"PuTTY-User-Key-File-3: ssh-ed25519\n",
    b"# created\nAGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ\n",
    b"[Interface]\nPrivateKey = aGVsbG8gd29ybGQgdGhpcyBpcyBub3QgYSBrZXk=\n",
    b"just text\n",
    b"",
    b"\x00\x01binary\xff",
    b"x" * (policy.SNIFF_BYTES + 10) + b"\n" + OPENSSH_KEY.encode(),
    b"x" * (policy.SNIFF_BYTES - 12) + OPENSSH_KEY.encode(),
    b"x" * (policy.SNIFF_BYTES - 60) + b"\n" + OPENSSH_KEY.encode(),
]


@pytest.mark.parametrize("head", HEADS, ids=range(len(HEADS)))
def test_sniff_bytes_agrees_with_the_file_sniff(head, lay):
    """The in-memory sniff and the file sniff give the same verdict for the same bytes, including markers that
    start outside or straddle the 8 KiB window (W093: E sniffs git blobs without a scratch file)."""
    path = lay.project / "blob.bin"
    path.write_bytes(head)
    expected = policy.sniff_secret(path)
    assert policy.sniff_bytes(head) is expected
    assert policy.sniff_bytes(bytearray(head)) is expected
    assert policy.sniff_bytes(memoryview(head)) is expected


def test_sniff_bytes_window_is_the_file_window():
    inside = b"x" * (policy.SNIFF_BYTES - 200) + b"\n" + OPENSSH_KEY.encode()
    beyond = b"x" * policy.SNIFF_BYTES + b"\n" + OPENSSH_KEY.encode()
    assert policy.sniff_bytes(inside) is True
    assert policy.sniff_bytes(beyond) is False


def test_exclude_pathspecs_kept_unchanged():
    assert policy.exclude_pathspecs(["b", "a", "a"]) == (":(exclude,literal)a", ":(exclude,literal)b")
    assert "Deprecated" in (policy.exclude_pathspecs.__doc__ or "")
