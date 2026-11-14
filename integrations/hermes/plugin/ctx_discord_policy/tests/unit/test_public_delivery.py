"""What a member's turn may put into Discord (dsp.delivery.public): text filter and upload guard."""

from __future__ import annotations

import re
from types import SimpleNamespace

import pytest

ASKER = "900000000000000500"
OTHER = "900000000000000501"

# What any extractor of Hermes' kind would look for, on the text as it leaves the plugin: a directive keyword
# followed by a colon (ASCII or a width variant), or an absolute path anchor followed by a path.
_DIRECTIVE_LEFT = re.compile(r"media\s*[:：﹕]", re.IGNORECASE)
_PATH_LEFT = re.compile(r"(?<![\w./\\])(?:~[\w.-]*[/\\／＼]|[/／][\w.-]+[/\\／＼]|[A-Za-z][:：][/\\／＼]|\\\\\w|file[:：])")


# Hermes' ``extract_local_files`` pattern (raw text, Unicode ``\w``), for the extensions used here.
_HERMES_BARE = re.compile(r"(?<![/:\w.])(?:~/|/|[A-Za-z]:[/\\])(?:[\w.\-]+[/\\])*[\w.\-]+\.(?:txt|md|pdf|png)\b",
                          re.IGNORECASE)


@pytest.fixture
def public(anchor):
    return anchor.mod("delivery.public")


def route(lane="public", author=ASKER):
    return SimpleNamespace(author_id=author, lane=lane, is_operator=lane == "operator")


@pytest.mark.parametrize("text", [
    "MEDIA:/srv/project/notes.txt",
    "Here: MEDIA: /srv/project/notes.txt",
    "**MEDIA:/srv/project/notes.txt**",
    "_MEDIA:/srv/project/notes.txt_",
    "`MEDIA:/srv/project/notes.txt`",
    "MEDIA:`/srv/project/notes.txt`",
    'MEDIA:"/srv/my docs/notes.txt"',
    "media:/srv/project/notes.txt",
    "ＭＥＤＩＡ：/srv/project/notes.txt",
    "MEDİA:/srv/project/notes.txt",
    "ME\u200bDIA:/srv/project/notes.txt",
    "MEDIA:notes.txt",
    "MEDIA:./notes.txt",
    "MEDIA:../../etc/passwd.txt",
    "MEDIA:/Caddyfile",
    "MEDIA:/a.png MEDIA:/b.png\nMEDIA:/srv/c.pdf",
    "MEDIA:/a.pngMEDIA:/b.png",
    "The file is /srv/project/notes.txt.",
    "See **/srv/project/notes.txt** now",
    "See `/srv/project/notes.txt` now",
    "```\ncat /srv/project/notes.txt\n```",
    "~/docs/id.txt",
    "~root/.ssh/id_rsa",
    "C:\\Users\\op\\notes.txt",
    "C:/Users/op/notes.txt",
    "\\\\fileserver\\share\\notes.txt",
    "file:///srv/project/notes.txt",
    "FILE://localhost/srv/notes.txt",
    "／srv／project／notes.txt",
    "[link](/srv/project/notes.txt)",
    "> quoted from a linked message: MEDIA:/srv/project/notes.txt\n> and /srv/x/y.md",
    "https://example.com/?p=/etc/passwd.txt",
    "MEDIA:\n/srv/project/notes.txt",
    *(f"see {pre}/srv/project/notes.txt" for pre in ("\\", "\uff0e", "\uff0f", "\uff3c", "\u24d0", "\u00a1", "\u20ac",
                                                      "\u2215", "\u00b7", "\u3002", "\u200b", "\u00ad")),
])
def test_no_directive_or_host_path_leaves_a_members_answer(public, text):
    clean = public.reply(text, route())
    assert not _DIRECTIVE_LEFT.search(clean), clean
    assert not _PATH_LEFT.search(clean), clean
    assert not _HERMES_BARE.search(clean), clean              # Hermes' own bare-path pattern finds nothing
    assert public.reply(clean, route()) == clean                     # idempotent


@pytest.mark.parametrize("text", [
    "Social media: it depends.",
    "See https://example.com/docs/guide.html for details.",
    "Use /policy and then Status.",
    "Open hours 24/7, and/or weekends; 1/2 of them.",
    "Relative paths like docs/notes.txt stay readable.",
    "Plain text without anything special.",
])
def test_ordinary_text_stays_as_it_is(public, text):
    assert public.reply(text, route()) == text


def test_paths_stay_readable(public):
    assert public.reply("Edit /etc/nginx/nginx.conf", route()) == "Edit \u2215etc\u2215nginx\u2215nginx.conf"
    assert public.reply("MEDIA:/srv/x.txt", route()) == "MEDIA\u2236\u2215srv\u2215x.txt"


def test_mentions_are_defused_except_the_askers(public):
    text = f"<@{ASKER}> asked; <@{OTHER}> and <@!{OTHER}> and <@&900000000000000777>, @everyone, @here."
    clean = public.reply(text, route())
    assert f"<@{ASKER}>" in clean
    assert f"<@{OTHER}>" not in clean and f"<@!{OTHER}>" not in clean
    assert "<@&900000000000000777>" not in clean
    assert "@everyone" not in clean and "@here" not in clean
    assert public.reply(clean, route()) == clean


def test_runner_results_keep_their_type(public):
    class Ephemeral(str):
        def __new__(cls, text, ttl_seconds=None):
            obj = super().__new__(cls, text)
            obj.ttl_seconds = ttl_seconds
            return obj

    out = public.reply_result(Ephemeral("MEDIA:/srv/x.txt", 30), route())
    assert isinstance(out, Ephemeral) and out.ttl_seconds == 30 and "MEDIA:" not in out
    assert public.reply_result(None, route()) is None
    assert public.reply_result({"final_response": "x"}, route()) == {"final_response": "x"}


# ── upload guard (A11) ─────────────────────────────────────────────────────────────────────────────


@pytest.fixture
def scene(anchor, tmp_path, monkeypatch):
    from unit.delivery_fakes import GUILD_A, Scene

    s = Scene(anchor)
    a = s.guild(GUILD_A)

    async def deliver_media(self, event, media_files, local_files, **kwargs):   # Hermes' event-level seams
        for path, _voice in media_files:
            await self.send_document(str(event.source.chat_id), path)

    async def image_batch(self, event, images, metadata, human_delay, record_delivery):
        record_delivery(await self.send_multiple_images(str(event.source.chat_id), images, metadata))

    s.cls._deliver_media_attachments, s.cls._send_image_batch = deliver_media, image_batch
    interventions = anchor.mod("adapt.interventions")
    result = interventions.install_upload_guard(s.cls)
    assert result.outcome.value == "wrapped", result
    trusted = tmp_path / "trusted"
    trusted.mkdir()
    (trusted / "chart.png").write_bytes(b"png")
    host = tmp_path / "host"
    host.mkdir()
    (host / "notes.txt").write_text("notes")
    monkeypatch.setattr(anchor.mod("hermes"), "media_trusted_roots", lambda: [trusted])
    s.a, s.trusted, s.host = a, trusted, host
    return s


async def test_a_public_turn_uploads_only_from_trusted_roots(scene, anchor):
    msg = scene.world.post(scene.a.general, "question", "900000000000000500", "Member")
    r = scene.route(msg, lane="public")
    channel = str(scene.a.general.id)
    with scene.in_turn(r):
        refused = await scene.adapter.send_document(channel, str(scene.host / "notes.txt"))
        allowed = await scene.adapter.send_document(channel, str(scene.trusted / "chart.png"))
        images = await scene.adapter.send_multiple_images(channel, [(f"file://{scene.host}/notes.txt", "")])
        remote = await scene.adapter.send_multiple_images(channel, [("https://example.com/a.png", "")])
    assert not refused.success and not images.success
    assert allowed.success and remote.success


async def test_an_operator_turn_keeps_its_uploads(scene):
    msg = scene.world.post(scene.a.general, "question", "900000000000000400", "Ada")
    r = scene.route(msg, lane="operator")
    with scene.in_turn(r):
        result = await scene.adapter.send_document(str(scene.a.general.id), str(scene.host / "notes.txt"))
    assert result.success


async def test_the_guard_fails_closed(scene, anchor, monkeypatch):
    msg = scene.world.post(scene.a.general, "question", "900000000000000500", "Member")
    r = scene.route(msg, lane="public")
    channel = str(scene.a.general.id)
    monkeypatch.setattr(anchor.mod("hermes"), "media_trusted_roots", lambda: None)   # Hermes without the list
    with scene.in_turn(r):
        assert not (await scene.adapter.send_document(channel, str(scene.trusted / "chart.png"))).success
    monkeypatch.setattr(anchor.mod("delivery.worklog"), "candidate_routes",
                        lambda *a, **k: (_ for _ in ()).throw(RuntimeError("lookup broke")))
    assert not (await scene.adapter.send_document(channel, str(scene.trusted / "chart.png"))).success


async def test_k3_sends_of_a_public_route_carry_no_host_path(scene, anchor):
    msg = scene.world.post(scene.a.general, "question", "900000000000000500", "Member")
    r = scene.route(msg, lane="public")
    with scene.in_turn(r):
        await scene.adapter.send(str(scene.a.general.id), "see MEDIA:/srv/x.txt and <@900000000000000501>")
    sent = scene.world.sent_to(scene.a.general.id)
    assert any("\u2215srv\u2215x.txt" in t for t in sent) and not any("/srv/x.txt" in t for t in sent), sent
    assert not any("<@900000000000000501>" in t for t in sent), sent
