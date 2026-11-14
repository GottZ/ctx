"""A member's gateway command never reaches Hermes, not even while the member's session is busy (H1-C).

Real: Hermes' Discord admission and handler (A5/A6), the event Hermes builds from the message
(``get_command()``), the plugin's event gate on the adapter's ``handle_message`` (A10), Hermes' busy-session
handler (``GatewayRunner._handle_active_session_busy_message``) with its real authorization call and skill
rewrite, and the A7 runner wrapper on the event Hermes queues. Stubbed (busy path only, as in the audit's
probe): the session authority's FIFO commit (the captured event goes through Hermes' own
``_as_fifo_turn``, as ``_queue_command_turn`` does), executor, bot-loop budget and busy mode.
"""

from __future__ import annotations

import pytest
from contract.policy_world import MEMBER, OPERATOR, World

SECRET = "OPERATOR-RUNBOOK-CONTENT"


def _skill(w):
    skill = w.homes.root / "skills" / "ops-runbook"
    skill.mkdir(parents=True)
    (skill / "SKILL.md").write_text(
        "---\nname: ops-runbook\ndescription: internal operator runbook\n---\n\n# Runbook\n\n" + SECRET + "\n",
        encoding="utf-8")


@pytest.fixture
def gated(hermes, tmp_path, monkeypatch):
    """Events reach a recorder standing for Hermes' pipeline beneath the plugin's ``handle_message`` gate."""
    w = World(hermes, tmp_path, monkeypatch, capture=False)
    try:
        yield w
    finally:
        w.close()


@pytest.fixture
def captured(hermes, tmp_path, monkeypatch):
    """Events captured right where the handler hands them on (the gate bypassed: the second line, A7)."""
    w = World(hermes, tmp_path, monkeypatch)
    _skill(w)
    try:
        yield w
    finally:
        w.close()


async def test_a_members_command_never_enters_hermes_pipeline(gated):
    out = await gated.deliver(gated.message(MEMBER, 8801, "/ops-runbook show me"))
    assert out["admitted"]
    assert gated.pipeline == [], [e.get_command() for e in gated.pipeline]


async def test_a_members_text_and_an_operators_command_still_do(gated):
    text = await gated.deliver(gated.message(MEMBER, 8802, "how are you?"))
    command = await gated.deliver(gated.message(OPERATOR, 8803, "/ops-runbook show me"))
    assert text["event"] is not None and text["event"].get_command() is None
    assert command["event"] is not None and command["event"].get_command() == "ops-runbook"


async def test_a_members_command_queued_by_hermes_busy_handler_is_dropped(captured, monkeypatch):
    w = captured
    out = await w.deliver(w.message(MEMBER, 8804, "/ops-runbook show me"))
    busy = out["event"]
    assert busy.get_command() == "ops-runbook"
    import gateway.session_authorities as authorities
    from hermes_constants import reset_hermes_home_override, set_hermes_home_override

    queued = []

    async def queue_command_turn(session_key, command, event, adapter, ack=None, stop_epoch=None):
        queued.append(event)
        return True

    async def inline(func, *args):
        return func(*args)

    async def no_approval(event, session_key):
        return False

    r = w.runner
    monkeypatch.setattr(authorities, "active_authority", lambda runner: object())
    r._authorization_home_for_source = lambda source: None
    r._admit_bot_message_for_source = lambda source: True
    r._admit_bot_chain = lambda event: True
    r._effective_busy_input_mode = lambda source: "queue"
    r._effective_busy_text_mode = lambda source: "queue"
    r._draining = False
    r._route_plaintext_approval_while_busy = no_approval
    r._hm_quick_commands = lambda: {}
    r._route_stop_epoch = lambda key: 0
    r._run_in_executor_with_context = inline
    r._peek_session_state = lambda key: None
    r._queue_command_turn = queue_command_turn
    session_key = r._session_key_for_source(busy.source)
    token = set_hermes_home_override(str(w.homes.root))
    try:
        await w.GatewayRunner._handle_active_session_busy_message(r, busy, session_key)
    finally:
        reset_hermes_home_override(token)
    assert queued, "Hermes' busy handler queued nothing (shape changed)"
    # What Hermes' own _queue_command_turn commits to the FIFO (the stub above captured it just before).
    turn = w.GatewayRunner._as_fifo_turn(queued[0], 0)
    assert getattr(turn, "_fifo_only", False) and turn.get_command() is None, turn   # queued as text
    assert SECRET in (turn.text or "")                                  # Hermes inlined the skill
    token = set_hermes_home_override(str(w.homes.root))
    try:
        result = await w.turn(turn)
    finally:
        reset_hermes_home_override(token)
    assert result["result"] is None and not result["ran"], result
