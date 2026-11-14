"""Layer I (L3-F18, DECISIONS R35): ``reclaim`` after a finished handover makes two independent copies. The other side
is told when it can be reached; when it cannot, or when it pulled the session, the output says explicitly that the
copy there stays active and does not learn of the reclaim."""

from __future__ import annotations

import argparse
import json
import re

import pytest

from ctx_handover.engine import commands
from ctx_handover.engine.commands import Plugin

from .test_commands import SID, _plugin

#: The lab's acceptance wording (E15, ``tests/e2e/test_e2e_flows.py`` on the lab branch).
_UNAWARE = re.compile(
    r"(?:not|never) (?:be )?(?:told|notified|informed)|(?:does not|doesn't|will not|won't) (?:learn|know|hear)"
    r"|(?:is|stays) (?:not|un)aware|(?:learns|knows|hears) nothing"
)
_DONE_WARNING = "the copy on laptop stays active; the two copies are now independent"


def _cli(plugin: Plugin, *argv: str) -> int:
    return plugin.cli(argparse.Namespace(verb=argv[0], rest=list(argv[1:])))


def _text(capsys) -> str:
    return str(json.loads(capsys.readouterr().out.strip().splitlines()[-1])["text"])


@pytest.mark.parametrize("notified", [None, False])
def test_reclaim_after_done_says_the_other_copy_is_not_told(capsys, notified):
    """★ R35/L3-F18: target not reachable or a pull (``target_notified`` not True) ⇒ the output names the copy on the
    peer as active and says that it does not learn of the reclaim.

    Red (on 1539274c): only "the copy on laptop stays active; the two copies are now independent" — no sentence that
    the copy is not told."""
    plugin, backend, _ = _plugin()
    result = {
        "ok": True,
        "state": "reclaimed",
        "message": "Reclaimed: this session is active here again.",
        "warnings": [_DONE_WARNING],
    }
    if notified is not None:
        result["target_notified"] = notified
    backend.reclaim_result = result
    assert _cli(plugin, "reclaim", SID, "--json") == commands.EXIT_OK
    text = _text(capsys)
    assert any("laptop" in line and "stays active" in line for line in text.lower().splitlines()), text
    assert _UNAWARE.search(text.lower()), text


def test_reclaim_after_done_with_a_notified_target_says_it_was_told(capsys):
    """R35: a reachable target that took the notice ⇒ the output says it was told and does not claim the opposite."""
    plugin, backend, _ = _plugin()
    backend.reclaim_result = {
        "ok": True,
        "state": "reclaimed",
        "message": "Reclaimed: this session is active here again.",
        "warnings": [_DONE_WARNING],
        "target_notified": True,
    }
    assert _cli(plugin, "reclaim", SID, "--json") == commands.EXIT_OK
    text = _text(capsys)
    assert "was told" in text.lower(), text
    assert not _UNAWARE.search(text.lower()), text


def test_reclaim_of_an_unfinished_handover_adds_no_copy_notice(capsys):
    """No finished handover (no independent copy) ⇒ no sentence about a copy elsewhere."""
    plugin, backend, _ = _plugin()
    backend.reclaim_result = {
        "ok": True,
        "state": "released",
        "message": "Reclaimed: this session is active here again.",
        "warnings": [],
    }
    assert _cli(plugin, "reclaim", SID, "--json") == commands.EXIT_OK
    assert not _UNAWARE.search(_text(capsys).lower())
