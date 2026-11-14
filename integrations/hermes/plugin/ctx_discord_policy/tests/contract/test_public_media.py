"""A member's turn never makes Hermes upload a host file, and its answer names none (H1-A).

Hermes extracts attachments from a turn's answer after the turn (``extract_media``: ``MEDIA:`` directives,
``extract_local_files``: bare local paths) and validates them with ``validate_media_delivery_path``, which by
default accepts every existing file outside a short denylist. Real here: Hermes' admission and handler, the A7
runner wrapper, Hermes' extractors and validators, Hermes' post-stream rescan
(``GatewayRunner._deliver_media_from_response``) and the Discord adapter's file senders down to
``channel.send``. The files exist in a directory Hermes does not deny.
"""

from __future__ import annotations

import os
from types import SimpleNamespace

import pytest
from contract.policy_world import CHANNEL, MEMBER, OPERATOR, World


@pytest.fixture
def world(hermes, tmp_path, monkeypatch):
    monkeypatch.delenv("HERMES_MEDIA_DELIVERY_STRICT", raising=False)
    monkeypatch.delenv("HERMES_MEDIA_ALLOW_DIRS", raising=False)
    w = World(hermes, tmp_path, monkeypatch)
    project = tmp_path / "project"
    project.mkdir()
    (project / "notes.txt").write_text("operator notes\n", encoding="utf-8")
    (project / "plan.md").write_text("plan\n", encoding="utf-8")
    (project / "Caddyfile").write_text("example.com\n", encoding="utf-8")
    home = tmp_path / "userhome"
    (home / "docs").mkdir(parents=True)
    (home / "docs" / "id.txt").write_text("secret\n", encoding="utf-8")
    monkeypatch.setenv("HOME", str(home))
    w.files = SimpleNamespace(notes=str(project / "notes.txt"), plan=str(project / "plan.md"),
                              caddy=str(project / "Caddyfile"), home=str(home / "docs" / "id.txt"))
    try:
        yield w
    finally:
        w.close()


def _replies(f):
    return {
        "media": f"Here you go.\nMEDIA:{f.notes}\n",
        "media_spaced": f"Here: MEDIA: {f.notes}",
        "media_emphasis": f"**MEDIA:{f.notes}**",
        "media_inline_code": f"`MEDIA:{f.notes}`",
        "media_quoted": f"MEDIA:`{f.notes}`",
        "media_lowercase": f"media:{f.notes}",
        "media_extensionless": f"MEDIA:{f.caddy}",
        "bare_path": f"The file is {f.notes} as you asked.",
        "bare_path_emphasis": f"See **{f.notes}**.",
        "home_path": "Look at ~/docs/id.txt please.",
        "several": f"MEDIA:{f.notes}\nMEDIA:{f.plan}\nand {f.plan}",
    }


def _hermes_attachments(text):
    """Every file Hermes' own extraction and validation would upload from ``text`` (both extractors)."""
    from gateway.platforms.base import BasePlatformAdapter as Base

    media, cleaned = Base.extract_media(text)
    media = Base.filter_media_delivery_paths(media)
    local, _ = Base.extract_local_files(cleaned)
    local = Base.filter_local_delivery_paths(local)
    return [p for p, _ in media] + list(local)


async def _member_turn(w, text, mid=7101, author=MEMBER):
    delivered = await w.deliver(w.message(author, mid, "what is on the host?"))
    assert delivered["event"] is not None, delivered
    return delivered["event"], await w.turn(delivered["event"], reply=text)


@pytest.mark.parametrize("kind", sorted(_replies(SimpleNamespace(notes="n", plan="p", caddy="c", home="h"))))
async def test_a_members_answer_names_nothing_hermes_would_attach(world, kind):
    text = _replies(world.files)[kind]
    assert _hermes_attachments(text), f"{kind}: Hermes would not attach anything from this text anyway"
    event, out = await _member_turn(world, text)
    assert out["lane"] == "public" and isinstance(out["result"], str), out
    assert _hermes_attachments(out["result"]) == [], (kind, out["result"])


async def test_an_operators_answer_keeps_its_attachment(world):
    event, out = await _member_turn(world, f"Here you go.\nMEDIA:{world.files.notes}\n", mid=7102, author=OPERATOR)
    assert out["lane"] == "operator"
    assert _hermes_attachments(out["result"]) == [os.path.realpath(world.files.notes)]


class _Channel:
    def __init__(self):
        self.id = int(CHANNEL)
        self.type = 0
        self.guild = SimpleNamespace(id=900000000000000100, filesize_limit=25 * 1024 * 1024)
        self.sent = []

    async def send(self, content=None, **kwargs):
        files = kwargs.get("files") or ([kwargs["file"]] if kwargs.get("file") else [])
        self.sent.append({"content": content, "files": [getattr(f, "filename", None) for f in files]})
        return SimpleNamespace(id=4400 + len(self.sent), attachments=list(files))


async def _streamed_rescan(w, event, route, text):
    """Hermes' rescan of an already streamed answer, inside the turn (the route is the turn's context)."""
    channel = _Channel()
    w.adapter._client = SimpleNamespace(user=w.adapter._client.user, application_id=w.adapter._client.application_id,
                                        get_channel=lambda cid: channel if cid == channel.id else None)
    w.runner._thread_metadata_for_source = lambda source, anchor=None: None
    w.runner._reply_anchor_for_event = lambda ev: None
    token = w.runtime.route.set(route)
    try:
        await w.GatewayRunner._deliver_media_from_response(w.runner, text, event, w.adapter)
    finally:
        w.runtime.route.reset(token)
    return [name for entry in channel.sent for name in entry["files"]]


async def test_a_streamed_members_answer_uploads_no_host_file(world):
    delivered = await world.deliver(world.message(MEMBER, 7103, "hi"))
    event = delivered["event"]
    route = event.raw_message._dsp_route
    assert not route.is_operator
    uploaded = await _streamed_rescan(world, event, route, f"Done.\nMEDIA:{world.files.notes}\n")
    assert uploaded == [], uploaded


async def test_a_streamed_operator_answer_keeps_its_upload(world):
    delivered = await world.deliver(world.message(OPERATOR, 7104, "hi"))
    event = delivered["event"]
    route = event.raw_message._dsp_route
    assert route.is_operator
    uploaded = await _streamed_rescan(world, event, route, f"Done.\nMEDIA:{world.files.notes}\n")
    assert uploaded == ["notes.txt"], uploaded


async def test_a_member_may_get_a_file_from_hermes_trusted_roots(world, tmp_path, monkeypatch):
    allowed = tmp_path / "allowed"
    allowed.mkdir()
    (allowed / "chart.txt").write_text("chart\n", encoding="utf-8")
    monkeypatch.setenv("HERMES_MEDIA_ALLOW_DIRS", str(allowed))     # the operator's own allowlist
    delivered = await world.deliver(world.message(MEMBER, 7105, "hi"))
    route = delivered["event"].raw_message._dsp_route
    result = await world.adapter.send_document(chat_id=CHANNEL, file_path=str(allowed / "chart.txt"))
    token = world.runtime.route.set(route)
    try:
        refused = await world.adapter.send_document(chat_id=CHANNEL, file_path=world.files.notes)
        trusted = await world.adapter.send_document(chat_id=CHANNEL, file_path=str(allowed / "chart.txt"))
    finally:
        world.runtime.route.reset(token)
    assert result is not None
    assert not refused.success and "host" in (refused.error or ""), refused
    assert "ctx_discord_policy" not in (trusted.error or ""), trusted      # not refused by the guard
