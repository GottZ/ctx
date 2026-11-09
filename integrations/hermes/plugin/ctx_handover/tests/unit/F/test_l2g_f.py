"""Layer G (green) of cluster F — what the green layer added beyond the layer-R tests: the closing text of a run
parked in ``aborting`` (V31), the withheld paths of a pull in the final message (V29), the one settings rule in
the surfaces, ``ctxrecords`` and ``doctor`` (V22)."""

from __future__ import annotations

import pytest

from ctx_handover.engine import commands, ctxrecords, surfaces
from ctx_handover.engine import settings as settings_rules
from ctx_handover.engine.model import Mode, Options, Progress
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend

from .test_commands import _plugin, _slash
from .test_l2_contracts import ACK

# ---------------------------------------------------------------------------------------------------------
# V31 — a run parked in ``aborting`` gets a closing text, never a success text
# ---------------------------------------------------------------------------------------------------------


def test_aborting_closing_text_is_never_a_success_text():
    """★ V31: a run parked in ``aborting`` (before the commit point, the janitor finishes the undo) shows its own
    closing text at once — the error and that nothing was handed over — and the notice stays open for the final
    ``aborted``.

    Red proof: mutant = the ``aborting`` branch of ``Plugin._final_text`` removed (the state falls through to
    ``render_final``) ⇒ ``AssertionError: Handed over to server: "this session" continues there (session ?, model
    default; works offline: unknown). This session is now read-only here; …``.
    """
    plugin, backend, ctx = _plugin()
    _slash(plugin, "to server")
    start = backend.last("start_job")
    job_id, reporter = start["job_id"], start["reporter"]
    backend.statuses[job_id] = {"state": "aborting", "last_error": {"code": "peer_unreachable", "message": "timeout"}}
    reporter.progress(Progress(step="aborting"))
    text = ctx.messages[0]
    assert "Handed over" not in text and "read-only" not in text, text
    assert "being undone in the background; nothing was handed over" in text, text
    assert f"/handover status {job_id[-6:]}" in text
    backend.statuses[job_id] = {"state": "aborted", "last_error": {"code": "cancelled", "message": "x"}}
    reporter.progress(Progress(step="aborted"))  # a parking step leaves the notice open for the end state
    assert "Cancelled. Nothing was changed." in ctx.messages[0]


# ---------------------------------------------------------------------------------------------------------
# V29 — take --include narrows the pull; the final message lists what the source withheld
# ---------------------------------------------------------------------------------------------------------


def test_take_include_sets_the_pull_hints():
    """V29 (F part): ``take … --include P`` becomes ``Options.hints.include`` (a local hint that only narrows the
    source's plan, R18); ``--include-secret`` never travels in the hints (X07)."""
    parsed = surfaces.parse("take server --include /srv/shared/data.txt --include /srv/private/keep.txt")
    assert not parsed.errors, parsed.errors
    options = surfaces.options_from(parsed, surfaces.settings_from_config({}))
    assert options.hints.include == ("/srv/shared/data.txt", "/srv/private/keep.txt")
    assert options.hints.include_secret == () and surfaces.include_secret_from(parsed) == ()


def test_pull_final_message_lists_withheld_paths():
    """★ V29: the closing message of a pull lists ``summary["withheld"]`` of the job result (paths asked for with
    ``--include`` that the source does not release) under "Withheld by source policy"; ``/handover status <job>``
    shows them too.

    Red proof: mutant = ``Plugin._final_text`` does not pass ``withheld`` to ``render_final`` ⇒
    ``AssertionError: "My Work" is here now (session 20261109_141003_a1b2c3, /home/u/app, model qwen3; …)``.
    """
    plugin, backend, _ctx = _plugin()
    job_id = "01JCAAAAAAAAAAAAAAAAPULL01"
    withheld = ["/srv/private/keep.txt", "/srv/private/notes.md"]
    backend.statuses[job_id] = {"state": "done", "ack": ACK, "withheld": withheld}
    text = plugin._final_text(job_id, Mode.PULL, "server", Options())
    assert "Withheld by source policy" in text, text
    assert all(f"  - {p}" in text for p in withheld), text
    details = surfaces.render_job_details({"withheld": withheld})
    assert "Withheld by source policy" in details and withheld[0] in details
    backend.statuses[job_id] = {"state": "done", "ack": ACK}
    assert "Withheld" not in plugin._final_text(job_id, Mode.PULL, "server", Options())


# ---------------------------------------------------------------------------------------------------------
# V22 — one settings rule: K0's ``engine/settings.py``
# ---------------------------------------------------------------------------------------------------------

CONFIGS = [
    None,
    {},
    {"plugins": {"entries": {"ctx_handover": {"settings": {"inbound": False, "max_file_mb": -1}}}}},
    {"plugins": {"entries": {"ctx_handover": {"settings": {"include_hermes_files": None, "refine": "Off"}}}}},
    {"plugins": {"entries": {"ctx_handover": {"settings": {"allow_roots": [], "max_total_gb": float("inf")}}}}},
    {"plugins": {"entries": {"ctx_handover": "not a mapping"}}},
]


@pytest.mark.parametrize("config", CONFIGS, ids=range(len(CONFIGS)))
def test_surfaces_read_settings_through_the_one_normaliser(config):
    """V22: the surfaces keep no reading rule of their own — the table and the secret names are K0's objects and
    ``settings_from_config`` gives exactly what ``settings.from_config`` gives (``allow_roots: []`` stays empty)."""
    assert surfaces.SETTINGS_SCHEMA is settings_rules.SCHEMA
    assert surfaces.SECRET_ENVS is settings_rules.SECRET_ENVS
    assert not hasattr(surfaces, "_coerce")
    assert surfaces.settings_from_config(config) == settings_rules.from_config(config)


@pytest.mark.parametrize("value", [False, "off", "OFF", None, "no", "false", "auto", True, 0, "nonsense", ["off"]])
def test_ctx_persist_is_read_by_the_one_rule(value):
    """★ V22: ``ctxrecords.persist_off`` (and with it ``server_for``, which A calls with the raw settings) reads
    ``ctx_persist`` exactly as K0's ``settings.coerce`` does: bare YAML ``off`` (``False``) and ``"off"`` are off,
    ``None`` and an unknown word are the default ``auto``.

    Red proof: mutant = the layer-2 word table (``{"off", "no", "false"}``) back in ``persist_off`` ⇒
    ``assert True is False`` for ``"no"``.
    """
    want_off = settings_rules.coerce("ctx_persist", value) == "off"
    assert ctxrecords.persist_off({"ctx_persist": value}) is want_off
    config = {"memory": {"provider": "ctx_checkpoint"}}
    assert ctxrecords.server_for(config, {"ctx_persist": value}) == (None if want_off else "ctx")


def test_ctx_persist_off_and_missing_settings():
    """The two values that matter most: YAML ``off`` stops records; no settings at all keep the default."""
    assert ctxrecords.persist_off({"ctx_persist": False}) is True
    assert ctxrecords.persist_off(None) is False and ctxrecords.persist_off({}) is False


def test_doctor_names_settings_the_normaliser_replaced(fake_host):
    """★ V22: ``doctor`` names every setting the one normaliser replaced by its default (``settings.problems``) —
    Hermes only warns at load, so without this line a typo silently means the default.

    Red proof: mutant = the ``settings`` line removed from ``local_checks`` ⇒ ``KeyError: 'settings'``.
    """
    raw = {"max_file_mb": -1, "inbound": "maybe", "allow_remote_continue": "perhaps"}
    backend = FakeBackend(config={"plugins": {"entries": {"ctx_handover": {"settings": raw}}}})
    checks = {c["name"]: c for c in commands.local_checks(FakeCtx(host=fake_host), backend)}
    line = checks["settings"]
    assert line["ok"] is None
    for key in raw:
        assert key in line["detail"], (key, line["detail"])
    assert line["detail"] == " ".join(settings_rules.problems(raw))
    clean = FakeBackend(config={"plugins": {"entries": {"ctx_handover": {"settings": {"max_file_mb": 7}}}}})
    assert "settings" not in {c["name"] for c in commands.local_checks(FakeCtx(host=fake_host), clean)}
