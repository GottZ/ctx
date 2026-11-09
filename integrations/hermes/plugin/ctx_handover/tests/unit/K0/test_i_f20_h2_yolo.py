"""K0 · review H2 (``review-code/REVIEW.md`` §2): a gateway launched with ``--yolo`` / ``HERMES_YOLO_MODE=1`` runs
tools without asking, so ``hostcompat.approvals_off`` must say so and the continuation must not start (R22,
catalog rule 12).

Upstream (main ``0f0e96fc8e``): ``tools/approval.py:48`` freezes ``HERMES_YOLO_MODE`` (``utils.is_truthy_value``)
at import and bypasses every approval with it (``:350-353``, ``:539-545``); ``--yolo`` writes the variable into the
process environment before any import (``hermes_cli/main.py:2762-2768``, ``:3010-3015``, ``:3420-3425``); the plugin
host inherits it (``served_profile_child_env`` keeps it, checked in
``test_i_f20_h2_yolo_reaches_the_plugin_host``). ``hermes chat --resume … -q`` is a gateway client — the turn runs in
the gateway (``hermes_cli/gateway_agent.py:1-6``), so scrubbing the variable from the child (``continuation.py``)
does not help.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import hostcompat, jobs
from ctx_handover.engine.errors import ContinueRefused
from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.integration.harness import Home
from ctx_handover.tests.unit.K0.test_i_f20_home_source import _DERIVE_HOST_ENV, mode_envs, run_json

SID = "20261109_141003_a1b2c3"
JOB = "01JC0000000000000000000ABC"
YES = ("1", "true", "TRUE", " yes ", "On")
NO = ("0", "false", "", "no", "off", "2")


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()


@pytest.mark.parametrize("value", YES + NO)
def test_i_f20_h2_yolo_env_means_approvals_off(value, monkeypatch):
    """★ ``HERMES_YOLO_MODE`` counts exactly as Hermes counts it (``utils.is_truthy_value``) — with the default
    approval configuration (mutant M4: no process check ⇒ every truthy value reads as "approvals apply")."""
    from utils import is_truthy_value

    monkeypatch.setenv("HERMES_YOLO_MODE", value)
    assert hostcompat.approvals_off({}) is is_truthy_value(value) is (value in YES)
    assert hostcompat.approvals_off({"approvals": {"mode": "manual", "single_query_mode": "deny"}}) is (value in YES)


def test_i_f20_h2_continuation_refused_on_a_yolo_gateway(tmp_path):
    """★ The reviewer's scenario through the composition root: target gateway with ``HERMES_YOLO_MODE=1``, default
    ``config.yaml`` approvals, ``allow_remote_continue: true``, a paired device asks for ``--continue`` ⇒
    ``continue_refused`` (``approvals_off``), nothing is started (mutants M4, and M5 = the reviewer's
    ``jobs.py`` wiring ``approvals_off=lambda: False`` ⇒ the recording ``popen`` is reached)."""
    jobs._reset_for_tests()
    started: list = []

    def popen(*args, **kwargs):
        started.append(args)
        raise AssertionError("must not start")

    home = Home(tmp_path, "server", settings={"allow_remote_continue": True})
    home.extra_env["HERMES_YOLO_MODE"] = "1"
    try:
        with home.active():
            _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
            spawner = target.continuation
            spawner._which = lambda _name: "/usr/bin/hermes"  # never reached when the refusal holds
            spawner._popen = popen
            with pytest.raises(ContinueRefused) as info:
                spawner.spawn(SID, "go on", JOB, requested_by="laptop")
            assert info.value.details.get("reason") == "approvals_off"
            home.extra_env.pop("HERMES_YOLO_MODE")
        # the same gateway without YOLO starts the turn (the guard is not a blanket refusal)
        with home.active(), pytest.raises(AssertionError, match="must not start"):
            spawner.spawn(SID, "go on", JOB, requested_by="laptop")
    finally:
        jobs._reset_for_tests()
    assert len(started) == 1


_PROBE_YOLO = r"""
import json
from ctx_handover.engine import hostcompat
print(json.dumps({"approvals_off": hostcompat.approvals_off(hostcompat.load_config())}))
"""


def test_i_f20_h2_yolo_reaches_the_plugin_host(tmp_path):
    """Under ``plugins.isolation: host`` the plugin evaluates the same: the host's environment (built by Hermes)
    keeps ``HERMES_YOLO_MODE``, so both modes refuse; without it neither does."""
    plain = mode_envs(tmp_path / "plain", "account")
    yolo = mode_envs(tmp_path / "yolo", "account")
    yolo.launch["HERMES_YOLO_MODE"] = "1"
    host = run_json(_DERIVE_HOST_ENV, yolo.launch, yolo.launch_cwd)
    assert host.get("HERMES_YOLO_MODE") == "1"
    host.update(PYTHONPATH=yolo.launch["PYTHONPATH"], PYTHONDONTWRITEBYTECODE="1", HERMES_PLUGIN_HOST_PROCESS="1")
    assert run_json(_PROBE_YOLO, yolo.launch, yolo.launch_cwd) == {"approvals_off": True}
    assert run_json(_PROBE_YOLO, host, yolo.hermes_home) == {"approvals_off": True}
    assert run_json(_PROBE_YOLO, plain.launch, plain.launch_cwd) == {"approvals_off": False}
    assert run_json(_PROBE_YOLO, plain.host, plain.hermes_home) == {"approvals_off": False}


def test_i_f20_h2_truthy_copy_matches_the_core():
    """``hostcompat._TRUTHY`` is a copy of ``utils.TRUTHY_STRINGS`` (``utils`` is not public in ``plugin_api.json``)."""
    from utils import TRUTHY_STRINGS

    assert frozenset(TRUTHY_STRINGS) == hostcompat._TRUTHY
