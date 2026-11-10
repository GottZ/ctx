"""A · wish W5 (acceptance test, built in for ``engine/jobs.py``): the composition root pins
the git runner to ``hostcompat.effective_home`` — every git process of the plugin gets the ``HOME`` the agent's
terminal gets, so the source profile reads the same ``~/.gitconfig`` under ``plugins.isolation: in_process`` and
``host`` (red while ``jobs._git`` builds ``gitops.Git()`` on the process home)."""

from __future__ import annotations

import pytest

from ctx_handover.tests.conftest import require_upstream
from ctx_handover.tests.unit.K0.test_i_f20_home_source import LAYOUTS, expected_home, mode_envs, run_json

_PROBE_GIT = r"""
import json
import threading
from types import SimpleNamespace
import hermes_constants
from ctx_handover.engine import gitops, jobs
git = jobs._git(SimpleNamespace(lock=threading.Lock(), git=None))
env = git.env(gitops.SOURCE)
print(json.dumps({"home": env.get("HOME"), "container": bool(hermes_constants.is_container())}))
"""


@pytest.mark.parametrize("layout", LAYOUTS)
def test_rv_e2_w5_git_home_is_the_same_in_both_isolation_modes(tmp_path, layout):
    """★ mutant: ``jobs._git`` builds ``gitops.Git()`` (the process home) ⇒ the two modes disagree."""
    require_upstream()
    envs = mode_envs(tmp_path, layout)
    launch = run_json(_PROBE_GIT, envs.launch, envs.launch_cwd)
    host = run_json(_PROBE_GIT, envs.host, envs.hermes_home)
    assert launch["home"] == expected_home(envs, layout, launch["container"])
    assert launch["home"] == host["home"]
