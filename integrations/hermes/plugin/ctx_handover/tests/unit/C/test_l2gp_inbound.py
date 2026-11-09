"""Round G′, cluster C — ``inbound`` is read by the one settings reader only (contract V22, integrator note of
round G′ after gp-k0): ``server.inbound_mode`` is ``settings.coerce("inbound", …)`` — the K0 normaliser reads YAML
booleans and the boolean words as ``on``/``off`` and an unrecognised value as ``off`` (fail-closed); the server keeps
no second rule, and its log line is the normaliser's own sentence.
"""

from __future__ import annotations

import logging

import pytest

from ctx_handover.engine import settings
from ctx_handover.engine.server import inbound_mode
from ctx_handover.tests.unit.C.test_server import Listener, unsigned_hello


@pytest.mark.parametrize(
    ("value", "mode", "status"),
    [
        ("yes", "on", 200),
        ("true", "on", 200),
        (1, "on", 200),
        ("no", "off", 404),
        (0, "off", 404),
        ("closed", "off", 404),
        (None, "auto", 200),
    ],
)
def test_inbound_is_the_settings_readers_word(tmp_path, value, mode, status):
    """★ The listener's inbound mode equals ``settings.coerce("inbound", value)`` for every value — boolean words
    included — and a paired listener answers ``hello`` accordingly.

    Red proof: on ``c31da4c3`` ``inbound_mode`` kept its own rule (a word must equal the mode) ⇒ ``"yes"``,
    ``"true"`` and ``1`` stayed ``off``. Mutant 12 (the server's own word rule restored).
    """
    assert settings.coerce("inbound", value) == mode
    assert inbound_mode(value) == mode
    lst = Listener(tmp_path / "lst", settings={"inbound": value})
    lst.pair(tmp_path / "lst")
    assert lst.handle(unsigned_hello()).status == status


def test_an_unrecognised_inbound_value_is_logged_with_the_readers_sentence(tmp_path, caplog):
    """The warning is the normaliser's sentence (``settings.problems``) — none for a word it accepts."""
    caplog.set_level(logging.WARNING)
    Listener(tmp_path / "a", settings={"inbound": "yes"})
    assert "inbound" not in caplog.text
    Listener(tmp_path / "b", settings={"inbound": "closed"})
    assert settings.problems({"inbound": "closed"})[0] in caplog.text
