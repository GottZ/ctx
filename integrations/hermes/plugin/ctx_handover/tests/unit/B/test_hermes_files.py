"""Session files under HERMES_HOME: discovery on the source, placement on the target (DESIGN §6.11, §10.6)."""

from __future__ import annotations

import hashlib
import os
from pathlib import Path

import pytest

from ctx_handover.engine import hermes_files as hf
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.model import HermesPaths, Options
from ctx_handover.engine.policy import HERMES_FILE_KINDS, PolicyConfig, check_target_dest
from ctx_handover.tests.unit.B.support import spill_terminal_output


def _cfg(user_home: Path, hermes_home: Path, **kw) -> PolicyConfig:
    return PolicyConfig.from_options(
        Options(**kw),
        home=user_home,
        hermes_home=hermes_home,
        hermes_root=hermes_home,
        plugin_dir=hermes_home / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
        document_cache=hermes_home / "cache" / "documents",
    )


@pytest.fixture
def produced(make_session, hermes_home, user_home, monkeypatch):
    """A session whose transcript names one session file of every kind, each written by the core's own writer
    (``terminal_output``: the terminal tool's full-output spill, L3-F10)."""
    from gateway.session_ingress_media import capture_native_media
    from tools.hook_output_spill import spill_if_oversized
    from tools.tool_result_storage import maybe_persist_tool_result

    b = make_session
    sid = b.create(cwd=str(user_home))
    spilled = maybe_persist_tool_result("A" * 5000, "read_file", "call_spill", threshold=100)
    hooked = spill_if_oversized(
        "H" * 5000, session_id=sid, source="pre_tool_call", config={"enabled": True, "max_chars": 100}
    )
    image = user_home / "photo.png"
    image.write_bytes(b"\x89PNG\r\n\x1a\n" + bytes(range(200)))
    (ref,) = capture_native_media([str(image)])
    b.user(sid, f"look at this\n[Image attached at: {ref['path']}]")
    call = b.tool_call("read_file", {"path": "/x"}, call_id="call_spill")
    b.assistant(sid, "", tool_calls=[call])
    b.tool(sid, "call_spill", spilled, name="read_file")
    b.assistant(sid, hooked)
    terminal = spill_terminal_output(monkeypatch, f"produced-{sid}")
    b.assistant(sid, "", tool_calls=[b.tool_call("terminal", {"command": "python3"}, call_id="call_term")])
    b.tool(sid, "call_term", terminal, name="terminal")
    rows = b.db.get_messages(sid, include_inactive=True)
    return {"sid": sid, "rows": rows, "native": ref}


def test_hermes_files_discover_markers(produced, hermes_home, user_home):
    paths = HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents"))
    refs = hf.references(produced["rows"], [produced["sid"]], paths)
    kinds = sorted(r["kind"] for r in refs)
    assert kinds == sorted(hf.SESSION_FILE_KINDS)
    by_kind = {r["kind"]: r for r in refs}
    assert by_kind["spillover"]["rel"] == Path(by_kind["spillover"]["source_path"]).name
    assert by_kind["hook_outputs"]["rel"].startswith(produced["sid"] + "/")
    assert by_kind["hook_outputs"]["session_id"] == produced["sid"]
    assert by_kind["native_inputs"]["rel"] == f"{produced['native']['sha256']}/photo.png"
    assert by_kind["terminal_output"]["rel"] == Path(by_kind["terminal_output"]["source_path"]).name
    found = hf.discover(produced["rows"], [produced["sid"]], paths, _cfg(user_home, hermes_home))
    assert sorted(f.kind for f in found) == sorted(HERMES_FILE_KINDS)
    assert all(f.size > 0 for f in found)


def test_hermes_files_discover_policy(produced, hermes_home, user_home):
    """Oversize, symlinked and out-of-area references are not carried; the reasons are reported."""
    paths = HermesPaths(home=str(hermes_home), document_cache=str(hermes_home / "cache" / "documents"))
    rejected: list[tuple[str, str]] = []
    small = _cfg(user_home, hermes_home, max_hermes_file_bytes=10)
    assert hf.discover(produced["rows"], [produced["sid"]], paths, small, rejected=rejected) == []
    assert {reason for _, reason in rejected} == {"size"}
    spill_dir = hermes_home / "cache" / "spillover"
    secret = user_home / "secret.txt"
    secret.write_text("s")
    os.symlink(secret, spill_dir / "link.txt")
    rows = [
        {"role": "tool", "content": f"Full output saved to: {spill_dir / 'link.txt'}"},
        {"role": "tool", "content": "Full output saved to: /etc/passwd"},
        {"role": "tool", "content": f"Full output saved to: {spill_dir / 'missing.txt'}"},
    ]
    rejected = []
    assert hf.discover(rows, [], paths, _cfg(user_home, hermes_home), rejected=rejected) == []
    assert sorted(reason for _, reason in rejected) == ["missing", "symlink"]


def test_hermes_files_target_placement_ignores_peer_rel(tmp_path, user_home):
    """★ A peer ``rel`` like ``../../.bashrc`` cannot choose the directory: placement uses kind + basename only,
    and the result passes the target policy for that kind [SEC 2.3].

    Mutant: ``placement`` joins the peer's ``rel`` under the area root instead of its basename.
    """
    target_home = tmp_path / "t-home"
    target = HermesPaths(home=str(target_home), document_cache=str(target_home / "cache" / "documents"))
    cfg = _cfg(user_home, target_home)
    sha = hashlib.sha256(b"img").hexdigest()
    cases = [
        ({"kind": "spillover", "rel": "../../.bashrc"}, target_home / "cache" / "spillover" / ".bashrc"),
        ({"kind": "hook_outputs", "rel": "../../../etc/cron.d/x"}, target_home / "hook_outputs" / "NEWSID" / "x"),
        (
            {"kind": "native_inputs", "rel": "../../../../.ssh/authorized_keys"},
            target_home / "cache" / "documents" / "native-inputs" / sha / "authorized_keys",
        ),
        ({"kind": "spillover", "rel": "..\\..\\evil.txt"}, target_home / "cache" / "spillover" / "evil.txt"),
    ]
    for ref, expected in cases:
        placed = hf.placement(ref, sha, target, "NEWSID")
        assert placed == expected, ref
        assert ".." not in placed.parts
        assert check_target_dest(placed, cfg, recorded=False, local_choice=False, hermes_file_kind=ref["kind"]).allowed
    for bad in (
        {"kind": "spillover", "rel": ".."},
        {"kind": "spillover", "rel": ""},
        {"kind": "config", "rel": "config.yaml"},
    ):
        with pytest.raises(PolicyDenied):
            hf.placement(bad, sha, target, "NEWSID")


def test_hermes_files_native_input_dir_is_content_hash(tmp_path):
    home = tmp_path / "h"
    target = HermesPaths(home=str(home), document_cache=str(home / "cache" / "documents"))
    data = b"received bytes"
    sha = hashlib.sha256(data).hexdigest()
    placed = hf.target_path("native_inputs", "pic.png", sha, target, "S")
    assert placed == home / "cache" / "documents" / "native-inputs" / sha / "pic.png"
    with pytest.raises(PolicyDenied):
        hf.target_path("native_inputs", "pic.png", "not-a-hash", target, "S")
    with pytest.raises(PolicyDenied):
        hf.target_path("native_inputs", "pic.png", sha.upper(), target, "S")
