"""Cluster F, layer 2 — one runtime per process from the hooks' view (X19), ``doctor`` without a
body-limit measurement unless asked (``--measure``), ``--include-secret`` as an explicit parameter (X07), the
contract form of ``model_change`` in ctx records and summaries (X04/X22), the MCP allowlist written by
``/handover setup`` (W108) and YAML 1.1 word values in the settings (``off`` arrives as ``False``)."""

from __future__ import annotations

import dataclasses
import json
import types

import pytest

from ctx_handover.engine import commands, ctxrecords, surfaces
from ctx_handover.engine import wire as real_wire
from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.continuation import Spawner
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import make_peer, sample_final_plan

from .test_commands import SID, _plugin, _slash
from .test_engine_backend import _stub_module, c_report

MIB = 1024**2
ROTATED = "20261109_160000_bbbbbb"


# ---------------------------------------------------------------------------------------------------------
# X19 — one Ledger, one VetoIndex, one JobRunner per profile and process, also from the hooks' view
# ---------------------------------------------------------------------------------------------------------


def test_hooks_and_surfaces_share_one_runtime(fake_host, monkeypatch):
    """★ X19/W109: every hook of two ``Plugin`` objects of one process (two ``ctx``, as Hermes may hand out) and
    every verb of their backends use the one runtime of ``jobs.ensure_started``: one ``Ledger`` (the ``VetoIndex``
    reads that same one), one ``VetoIndex``, one ``JobRunner`` — none built by the surfaces, whichever of hook or
    command comes first.

    Red proof: mutant = ``EngineBackend.hook_runtime`` builds its own ``lock.VetoIndex(rt.ledger.path)`` (layer 1)
    → ``AssertionError: assert <…lock.VetoIndex object at …> is <…lock.VetoIndex object at …>``; mutant =
    ``_runner`` builds ``JobRunner(self._ledger(), …)`` → ``assert (<…JobRunner …> is <…JobRunner …>)``.
    """
    from ctx_handover.engine import jobs, ledger, lock

    counts = {"ledger": 0, "runner": 0, "veto": 0}

    def counting(cls, key: str):
        real = cls.__init__

        def init(self, *a, **kw):
            counts[key] += 1
            real(self, *a, **kw)

        monkeypatch.setattr(cls, "__init__", init)

    counting(ledger.Ledger, "ledger")
    counting(jobs.JobRunner, "runner")
    counting(lock.VetoIndex, "veto")
    jobs._reset_for_tests()
    plugins: list[Plugin] = []
    try:
        plugins = [commands.plugin_for(FakeCtx(host=fake_host)), commands.plugin_for(FakeCtx(host=fake_host))]
        assert plugins[0] is not plugins[1]
        for plugin in plugins:  # in Hermes a hook fires before any command
            hooks = plugin.hooks
            assert hooks.pre_llm_call(session_id=SID, turn_id="t1") is None
            assert hooks.pre_tool_call(session_id=SID) is None
            assert hooks.post_compaction(session_id=ROTATED, old_session_id=SID) is None
            assert hooks.post_llm_call(session_id=SID, turn_id="t1") is None
        for plugin in plugins:
            assert _slash(plugin, "status").startswith(("No handovers", "Handover", "Jobs", "No "))
            assert plugin.backend.peers() == []
        prof = jobs._profile()
        for plugin in plugins:
            backend = plugin.backend
            assert backend._ledger() is prof.ledger and backend._store() is prof.store
            assert backend._runner() is prof.runner and backend._runner() is jobs.runner_for()
            assert plugin.hooks._rt()._veto is prof.veto
        assert counts == {"ledger": 1, "runner": 1, "veto": 1}
    finally:
        prof = jobs._PROFILES and next(iter(jobs._PROFILES.values()))
        if prof and getattr(prof, "veto", None) is not None:
            prof.veto.stop()
        jobs._reset_for_tests()


# ---------------------------------------------------------------------------------------------------------
# doctor — the proxy body limit is measured only with --measure
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def doctor_plugin(fake_host, monkeypatch, tmp_path):
    """A plugin over ``EngineBackend`` with a real ``Ledger`` (one dialled peer) and cluster C's ``WireClient``
    replaced by a recorder of ``probe_gate(measure_limit=…)`` that sends nothing."""
    from ctx_handover.engine.ledger import Ledger

    led = Ledger(tmp_path / "ledger.sqlite3")
    led.upsert_peer(make_peer("server"))
    probes: list[bool] = []
    sent: list[int] = []

    class Client:
        clock_skew = 0.0

        def __init__(self, peer, ident, *, read_secret):
            self.peer = peer

        def probe_gate(self, *, measure_limit: bool = False):
            probes.append(measure_limit)
            report = {"gate": "ok", "plugin": True, "signed": True}
            if measure_limit:
                sent.extend(size * MIB for size in real_wire.PROBE_SIZES_MIB)  # what C's probe would upload
                report["body_limit"] = 2 * MIB
            return c_report(self.peer, report)

        def close(self):
            pass

    class Identity:
        @classmethod
        def load(cls, ledger, *, read_secret, read_env_fresh):
            return types.SimpleNamespace(fingerprint=lambda: "0123456789abcdef", instance_id="01I")

    _stub_module(
        monkeypatch,
        "wire",
        WireClient=Client,
        gate_warnings=lambda gate, *, env_mode: [],
        PROBE_SIZES_MIB=real_wire.PROBE_SIZES_MIB,
    )
    _stub_module(monkeypatch, "crypto", Identity=Identity)
    _stub_module(monkeypatch, "gitops", Git=lambda: object(), check_version=lambda git: (True, None))
    holder: dict = {}
    plugin = Plugin(FakeCtx(host=fake_host), backend_factory=lambda c, p: holder.setdefault("eb", EngineBackend(c, p)))
    plugin.backend._parts["runtime"] = types.SimpleNamespace(ledger=led, store=None, veto=None)
    try:
        yield plugin, led, probes, sent
    finally:
        led.close()


def test_doctor_measures_body_limit_only_on_request(doctor_plugin):
    """★ Coordinator decision (doctor): the plain ``doctor`` sends no test bodies — it shows the last measured
    limit with its date or says how to measure; ``doctor --measure`` uploads the 1+2+4+8 MiB test bodies to each
    dialled peer, keeps the result in the ledger, and is refused on messaging platforms (an operator's choice).

    Red proof: mutant = ``doctor_checks`` calls ``probe_gate(measure_limit=True)`` as in layer 1 →
    ``assert [True] == [False]``.
    """
    plugin, led, probes, sent = doctor_plugin
    out = _slash(plugin, "doctor")
    assert probes == [False] and sent == []
    assert f"request body limit not measured yet (doctor --measure sends up to {commands.measure_text()})" in out
    assert commands.measure_text().startswith("15 MiB")
    assert led.meta_get(commands.BODY_LIMIT_KEY + make_peer("server").peer_id) is None

    out = _slash(plugin, "doctor --measure")
    assert probes == [False, True] and sum(sent) == 15 * MIB
    assert "a proxy limits request bodies to 2 MiB" in out and "(measured" not in out  # C's summary (V27)
    stored = json.loads(led.meta_get(commands.BODY_LIMIT_KEY + make_peer("server").peer_id))
    assert stored["bytes"] == 2 * MIB

    out = _slash(plugin, "doctor")
    assert probes == [False, True, False] and sum(sent) == 15 * MIB
    assert "a proxy limits request bodies to 2.0 MiB (measured " in out and "doctor --measure measures again" in out

    refused = _slash(plugin, "doctor --measure", platform="telegram")
    assert refused == commands.REFUSAL and probes == [False, True, False]
    assert "--measure" in surfaces.USAGE["doctor"]


# ---------------------------------------------------------------------------------------------------------
# X07 — --include-secret travels as a parameter of its own, from the local surface only
# ---------------------------------------------------------------------------------------------------------


def test_include_secret_is_a_parameter_of_start_job_and_offer():
    """X07, R18, R24: ``/handover to``, ``take``, ``export`` and an offer pass the typed ``--include-secret`` paths
    as ``include_secret=``; the options (whose hints go to the other side) carry none."""
    plugin, backend, _ = _plugin()
    _slash(plugin, "to server --include-secret /srv/app/.env --yes")
    start = backend.last("start_job")
    assert start["include_secret"] == ("/srv/app/.env",) and start["options"].hints.include_secret == ()
    _slash(plugin, "export --include-secret a/.env --include-secret b/.env --yes")
    assert backend.last("start_job")["include_secret"] == ("a/.env", "b/.env")
    _slash(plugin, "to laptop --include-secret /srv/app/.env")  # laptop dials in: an offer it takes later
    offer = backend.last("create_offer")
    assert offer["include_secret"] == ("/srv/app/.env",) and offer["options"].hints.include_secret == ()
    _slash(plugin, "take server latest-id --yes")
    assert backend.last("start_job")["include_secret"] == ()


# ---------------------------------------------------------------------------------------------------------
# X04/X22 — model_change is read only in the contract form
# ---------------------------------------------------------------------------------------------------------

CONTRACT_CHANGE = {
    "from": {"model": "gpt-x", "provider": "cloud"},
    "to": {"model": "qwen3", "provider": "local", "context_length": 32768},
    "reason": "provider_not_configured_here",
    "offline_capable": False,
}


def test_model_change_contract_form_in_records_and_summary():
    """X22: ``ctxrecords.build_record`` (owner of the record form) and ``surfaces.plan_summary`` read
    ``model_change`` in the form of contract X04 — from/to dicts, reason, ``offline_capable`` — and nothing else
    (a flat or older form reads as unknown instead of printing a dict)."""
    data = {"job_id": "01JC0000000000000000000ABC", "source_root": "r", "target_root": "t", "peer": "laptop"}
    record = ctxrecords.build_record("departure", {**data, "model_change": CONTRACT_CHANGE}, redact=lambda s: s)
    assert (
        "- Model: gpt-x (cloud) → qwen3 (local) (provider_not_configured_here); works offline: no" in record["content"]
    )
    flat = {"from": "gpt-x", "from_provider": "cloud", "to": "qwen3", "to_provider": "local", "offline": True}
    record = ctxrecords.build_record("arrival", {**data, "model_change": flat}, redact=lambda s: s)
    assert "- Model: unknown → unknown" in record["content"] and "{" not in record["content"]
    no_change = ctxrecords.build_record("arrival", {**data, "model_change": None}, redact=lambda s: s)
    assert "- Model:" not in no_change["content"]

    final = sample_final_plan()
    summary = surfaces.plan_summary(final)  # model and offline come from D's presend_summary (V10)
    assert summary["model"] == "qwen3 (local)" and summary["offline"] is True
    flat_final = dataclasses.replace(final, model_change=flat)
    flat_summary = surfaces.plan_summary(flat_final)
    assert flat_summary["model"] is None and flat_summary["offline"] is None


# ---------------------------------------------------------------------------------------------------------
# W108 — /handover setup writes the MCP allowlist through hostcompat.grant_mcp_server
# ---------------------------------------------------------------------------------------------------------


class _ConfigCtx(FakeCtx):
    """``ctx.set_config`` as the PluginContext offers it (writes under ``plugins.entries.ctx_handover.settings``)."""

    def set_config(self, key, value):
        entry = self.host.config.setdefault("plugins", {}).setdefault("entries", {}).setdefault("ctx_handover", {})
        entry.setdefault("settings", {})[key] = value


@pytest.mark.skipif(
    not hasattr(__import__("ctx_handover.engine.hostcompat", fromlist=["x"]), "grant_mcp_server"),
    reason="hostcompat.grant_mcp_server (K-5) absent",
)
def test_setup_writes_mcp_allowlist_through_hostcompat(fake_host):
    """W108/K-5: the operator's yes in ``/handover setup`` becomes an ``mcp_allowlist`` entry written by
    ``hostcompat.grant_mcp_server``; when Hermes cannot write it (package-managed install) the setup names the
    ``hermes config set`` command instead of failing, and ctx recording stays consented (``ctx_persist: auto``)."""
    fake_host.config = {"memory": {"provider": "ctx_checkpoint"}}
    ctx = _ConfigCtx(host=fake_host)
    ctx.choice_answers = ["auto", "yes"]
    plugin = Plugin(ctx)
    out = _slash(plugin, "setup")
    entry = fake_host.config["plugins"]["entries"]["ctx_handover"]
    assert entry["mcp_allowlist"] == ["ctx"] and entry["settings"]["ctx_persist"] == "auto"
    assert "ctx: handovers are recorded through MCP server ctx." in out
    assert ("grant_mcp_server", ("ctx",)) in fake_host.calls

    fake_host.config = {"memory": {"provider": "ctx_checkpoint"}}
    fake_host.mcp_grant_refusal = "managed"
    ctx.choice_answers = ["auto", "yes"]
    out = _slash(plugin, "setup")
    assert "ctx: the MCP allowlist was not written (managed), so the plugin may not use ctx yet." in out
    assert "Run: hermes config set plugins.entries.ctx_handover.mcp_allowlist" in out
    assert "mcp_allowlist" not in fake_host.config["plugins"]["entries"]["ctx_handover"]


def test_setup_names_the_command_without_set_config(fake_host):
    """A host without ``ctx.set_config`` gets the ``hermes config set`` line for each answer instead of an error."""
    fake_host.config = {}
    ctx = FakeCtx(host=fake_host)
    ctx.choice_answers = ["off"]
    out = _slash(Plugin(ctx), "setup")
    assert (
        "inbound: not saved from here; run: hermes config set plugins.entries.ctx_handover.settings.inbound off" in out
    )


# ---------------------------------------------------------------------------------------------------------
# YAML 1.1 word values (`off` arrives as False) in F's settings readers
# ---------------------------------------------------------------------------------------------------------

YAML_SETTINGS = """
plugins:
  entries:
    ctx_handover:
      settings:
        inbound: off
        refine: off
        extra_paths: off
        accept_offers: off
        ctx_persist: off
        include_hermes_files: "off"
        allow_remote_continue: "no"
        refine_auto_include: "yes"
        allow_agent_start: maybe
"""


def test_yaml_word_values_are_read_as_meant(tmp_path):
    """The settings come from Hermes' YAML loader, which turns unquoted ``off``/``no``/``on`` into booleans and
    keeps quoted ones as text. F reads both as the word the operator wrote: choice settings get ``off``, boolean
    settings accept the words ``hermes config set`` accepts, ``ctx_persist: off`` stops ctx records also when a
    caller passes the raw settings, and only a real ``True`` lets a peer start a continuation.

    Red proof: mutant = ``BOOL_WORDS`` lookup removed from ``surfaces._coerce`` →
    ``assert True is False`` (``include_hermes_files: "off"`` fell back to the default ``True``).
    """
    import hermes_yaml

    config = hermes_yaml.safe_load(YAML_SETTINGS)
    raw = config["plugins"]["entries"]["ctx_handover"]["settings"]
    assert raw["inbound"] is False and raw["include_hermes_files"] == "off"  # what the loader really delivers
    s = surfaces.settings_from_config(config)
    assert (s["inbound"], s["refine"], s["extra_paths"], s["accept_offers"], s["ctx_persist"]) == ("off",) * 5
    assert s["include_hermes_files"] is False
    assert s["allow_remote_continue"] is False and s["refine_auto_include"] is True
    assert s["allow_agent_start"] is False  # an unknown word keeps the (closed) default
    options = surfaces.options_from(surfaces.parse("to server"), s)
    assert options.refine == "off" and options.include_hermes_files is False

    provider = {"memory": {"provider": "ctx_checkpoint"}}
    assert ctxrecords.server_for(provider, raw) is None  # raw YAML False
    assert ctxrecords.server_for(provider, {"ctx_persist": "off"}) is None
    assert ctxrecords.server_for(provider, {"ctx_persist": "auto"}) == "ctx"
    assert ctxrecords.server_for(provider, {"ctx_persist": True}) == "ctx"  # `ctx_persist: on`

    spawner = Spawner(
        log_dir=tmp_path / "logs",
        profile=None,
        approvals_off=lambda: False,
        allow_remote=lambda: "false",  # a raw text value is not a yes
        which=lambda _n: "/x/hermes",
        popen=lambda *a, **k: pytest.fail("must not start"),
    )
    with pytest.raises(ContinueRefused) as exc:
        spawner.spawn(SID, "go", "01JC0000000000000000000ABC", requested_by="laptop")
    assert exc.value.details.get("reason") == "not_allowed"


def test_readme_discloses_the_body_limit_measurement():
    """The README's Network disclosure names the ``doctor --measure`` uploads with the sizes the code uses."""
    import re

    from .test_readme import FLAT

    probe_sizes = real_wire.PROBE_SIZES_MIB  # cluster C's constant (V27)
    sizes = ", ".join(str(s) for s in probe_sizes[:-1]) + f" and {probe_sizes[-1]} MiB"
    network = FLAT.split("**Network:**", 1)[1].split("**Inbound:**", 1)[0]
    assert "`doctor` sends one signed hello per paired gateway" in network
    assert f"only `doctor --measure` (local CLI, TUI or Desktop) uploads zero-filled test bodies of {sizes}" in network
    assert re.search(rf"up to {sum(probe_sizes)} MiB per paired gateway", network)
    assert f"test uploads of {sizes} to each paired machine" in surfaces.USAGE["doctor"]  # the help text too


def test_fingerprint_is_cluster_c_rule_only():
    """X24/W120: the surfaces show peer keys only through ``crypto.fingerprint_of``; a damaged key is named as such
    instead of being hashed by a second rule."""
    from ctx_handover.engine import crypto

    key = crypto.b64e(bytes(range(32)))
    assert surfaces.fingerprint_of(key) == crypto.fingerprint_of(key)
    assert surfaces.fingerprint_of("not base64!") == "invalid key"
    from dataclasses import replace

    assert "key invalid key" in surfaces.render_peers([replace(make_peer("broken"), pubkey="AA==")])


def test_take_offers_first_then_the_20_most_recent_sessions():
    """README (Use): ``/handover take`` asks the only paired gateway for its offers and its 20 most recent sessions
    and shows the offers first; ``accept_offers: off`` hides the offers."""
    from ctx_handover.tests.fakes.surfaces_fake import FakeBackend

    plugin, backend, ctx = _plugin(FakeBackend(peers=[make_peer("server")]))
    backend.remote = {
        "offers": [{"offer_id": "01OF", "session": {"title": "Left for you"}, "expires_at": 1e18}],
        "sessions": [{"id": "S1", "title": "Recent", "last_active_at": 0, "message_count": 2, "cwd": "/x"}],
    }
    shown: list = []
    ctx.ask_choice = lambda question, choices: shown.append(choices) or None
    _slash(plugin, "take")
    assert backend.last("remote_choices")["limit"] == 20
    assert [c["value"] for c in shown[-1]] == ["offer:01OF", "session:S1"]
    backend.settings_values = {**backend.settings(), "accept_offers": "off"}
    backend.settings = lambda: backend.settings_values
    _slash(plugin, "take")
    assert [c["value"] for c in shown[-1]] == ["session:S1"]


def test_settings_words_agree_with_the_engine_reader():
    """The surfaces (``surfaces.settings_from_config``) and the engine (``jobs.plugin_settings``, cluster A) read
    every boolean and every on/off word setting the same way, so a listener and a dialer never disagree about
    ``refine: off`` or ``allow_remote_continue: "no"``. (``None`` is left out: the engine reads it as off for flags,
    the surfaces as the default.)"""
    from ctx_handover.engine import jobs

    values = [True, False, "on", "off", "yes", "no", "true", "false", "1", "0", "", 1, 0, "maybe", " Off "]
    keys = [k for k, spec in surfaces.SETTINGS_SCHEMA.items() if spec["type"] == "bool"]
    keys += ["inbound", "refine", "extra_paths", "ctx_persist", "accept_offers"]
    for key in keys:
        default = surfaces.SETTINGS_SCHEMA[key]["default"]
        for value in values:
            config = {"plugins": {"entries": {"ctx_handover": {"settings": {key: value}}}}}
            ours = surfaces.settings_from_config(config)[key]
            theirs = jobs.plugin_settings(config).get(key, default)
            choices = surfaces.SETTINGS_SCHEMA[key].get("choices")
            if choices and theirs not in choices:
                theirs = default  # the surfaces' choice check, applied to the engine's word
            assert ours == theirs, (key, value, ours, theirs)
