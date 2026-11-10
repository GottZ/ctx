"""A · L3-F20, wish W1 (its acceptance test, built in for ``engine/jobs.py``): the composition
root builds the policy home — and with it the facts, the path map, the denylist and the collector — from
``hostcompat.effective_home``, so it is the same under ``plugins.isolation: in_process`` and ``host`` (red while
``jobs._policy`` reads ``Path.home()``)."""

from __future__ import annotations

import pytest

from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.unit.K0.test_i_f20_home_source import LAYOUTS, expected_home, mode_envs, run_json

_PROBE_POLICY = r"""
import json
from pathlib import Path
from types import SimpleNamespace
import hermes_constants
from ctx_handover.engine import hostcompat, jobs
from ctx_handover.engine.model import Options
hermes = hostcompat.hermes_paths()
policy = jobs._policy(Options(), SimpleNamespace(data_dir=Path(hermes.home) / "plugin-data" / "ctx_handover"), hermes)
print(json.dumps({"home": str(policy.home), "allow_roots": [str(r) for r in policy.allow_roots],
                  "container": bool(hermes_constants.is_container())}))
"""


@pytest.mark.parametrize("layout", LAYOUTS)
def test_i_f20_w1_policy_home_is_the_same_in_both_isolation_modes(tmp_path, layout):
    """★ mutant: ``_policy(home=Path.home())`` (the state before W1) ⇒ the two modes disagree."""
    require_upstream()
    envs = mode_envs(tmp_path, layout)
    launch = run_json(_PROBE_POLICY, envs.launch, envs.launch_cwd)
    host = run_json(_PROBE_POLICY, envs.host, envs.hermes_home)
    assert launch["home"] == expected_home(envs, layout, launch["container"])
    assert (launch["home"], launch["allow_roots"]) == (host["home"], host["allow_roots"])
