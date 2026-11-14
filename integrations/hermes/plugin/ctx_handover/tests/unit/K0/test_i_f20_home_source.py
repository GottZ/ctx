"""K0 · L3-F20: the home the plugin uses is the one Hermes uses for the profile — the same under
``plugins.isolation: in_process`` and ``host`` (DECISIONS R12, R33).

Cause (read at main ``0f0e96fc8e``; identical at the floor v0.22.6): Hermes applies its subprocess HOME contract
(``hermes_constants.apply_subprocess_home_env``: a launch ``HOME`` that is the profile home ``<HERMES_HOME>/home`` is
repaired to the real home on a host; ``terminal.home_mode: profile`` and containers use the profile home) to every
child it starts — the plugin host included (``hermes_cli/plugin_host.py:127-136`` → ``served_profile_child_env`` →
``_finalize_child_env`` → ``_apply_profile_home``) — but not to its own process. ``Path.home()`` therefore answers
differently in the gateway (in-process plugins) and in the plugin host. Hermes' own file tools expand ``~`` with
``get_subprocess_home()`` (``tools/file_tools_paths.py:32-49``); ``hostcompat.effective_home`` takes that source.

Without a running Hermes: each isolation mode is a subprocess with the environment that mode gives the plugin — the
gateway's launch environment (the lab layout is ``lab/bin/labctl.py:74-75``: ``HOME=<HERMES_HOME>/home``), and the
plugin host's environment as Hermes builds it (``served_profile_child_env(target_home=…, inherit_credentials=True)``
run under the launch environment, plus the host marker and ``cwd = HERMES_HOME``, ``plugin_host.py:127-149``).
"""

from __future__ import annotations

import getpass
import json
import os
import pwd
import subprocess
import sys
from dataclasses import dataclass
from pathlib import Path

import pytest

from ctx_handover.engine import hostcompat
from ctx_handover.engine.errors import CapabilityMissing
from ctx_handover.tests.conftest import PLUGIN_ROOT, require_upstream
from ctx_handover.tests.fakes.hostcompat_fake import FakeHost

LAYOUTS = ("lab", "account", "profile_mode")
"""``lab``: launch ``HOME`` is the profile home (the lab gateways, ``labctl.py:74-75``); ``account``: the same with
the operator override ``HERMES_REAL_HOME``; ``profile_mode``: a normal ``HOME`` with ``TERMINAL_HOME_MODE=profile``
(``terminal.home_mode`` bridged by the gateway, ``gateway/run.py:1988-2030``)."""

_DERIVE_HOST_ENV = r"""
import json, os
from tools.environments.local import served_profile_child_env
print(json.dumps(served_profile_child_env(target_home=os.environ["HERMES_HOME"], inherit_credentials=True)))
"""

_PROBE_PROCESS = r"""
import json, os
from pathlib import Path
from hermes_constants import is_container
print(json.dumps({"process_home": str(Path.home()), "cwd": os.getcwd(), "container": bool(is_container())}))
"""

_PROBE_HOME = r"""
import json, os
from pathlib import Path
from hermes_constants import is_container
from tools.file_tools_paths import _expand_tilde
from ctx_handover.engine import hostcompat
print(json.dumps({
    "process_home": str(Path.home()),
    "plugin_home": str(hostcompat.effective_home()),
    "hermes_tilde": _expand_tilde("~"),
    "container": bool(is_container()),
    "cwd": os.getcwd(),
}))
"""


@dataclass(frozen=True)
class ModeEnvs:
    """The two environments of one layout: ``launch`` (in-process plugin) and ``host`` (plugin host)."""

    launch: dict[str, str]
    host: dict[str, str]
    launch_cwd: Path
    hermes_home: Path
    account: Path


def _pythonpath() -> str:
    upstream = require_upstream()
    return os.pathsep.join([str(upstream), str(PLUGIN_ROOT.parent)])


def run_json(code: str, env: dict[str, str], cwd: Path) -> dict:
    """Run ``code`` with this interpreter in ``env``/``cwd``; its last stdout line is a JSON object."""
    out = subprocess.run(  # noqa: S603 - this interpreter, a fixed script
        [sys.executable, "-c", code], env=env, cwd=cwd, capture_output=True, text=True, timeout=120, check=False
    )
    assert out.returncode == 0, out.stderr[-3000:]
    return json.loads(out.stdout.strip().splitlines()[-1])


def mode_envs(tmp_path: Path, layout: str) -> ModeEnvs:
    """Launch and plugin-host environments of ``layout`` (module docstring). Nothing of the test process leaks in
    except ``PATH``; ``LANG``/``TZ``/``SHELL``/``LOGNAME`` are fixed so other facts can be compared too."""
    hh = tmp_path / "hh"
    (hh / "home").mkdir(parents=True)
    account = tmp_path / "account"
    account.mkdir()
    launch_cwd = tmp_path / "launch-cwd"
    launch_cwd.mkdir()
    user = getpass.getuser()
    env = {
        "PATH": os.environ.get("PATH", os.defpath),
        "HERMES_HOME": str(hh),
        "PYTHONPATH": _pythonpath(),
        "PYTHONDONTWRITEBYTECODE": "1",
        "LANG": "C.UTF-8",
        "TZ": "UTC",
        "SHELL": "/bin/sh",
        "LOGNAME": user,
        "USER": user,
    }
    if layout == "lab":
        env["HOME"] = str(hh / "home")
    elif layout == "account":
        env.update(HOME=str(hh / "home"), HERMES_REAL_HOME=str(account))
    elif layout == "profile_mode":
        env.update(HOME=str(account), TERMINAL_HOME_MODE="profile")
    else:  # pragma: no cover - parametrisation guard
        raise ValueError(layout)
    host = run_json(_DERIVE_HOST_ENV, env, launch_cwd)
    host["PYTHONPATH"] = env["PYTHONPATH"]  # plugin_host.py:133-134 puts its own tree first; ours is the plugin's
    host["PYTHONDONTWRITEBYTECODE"] = "1"
    host[hostcompat.HOST_PROCESS_ENV] = "1"
    return ModeEnvs(launch=env, host=host, launch_cwd=launch_cwd, hermes_home=hh, account=account)


def expected_home(envs: ModeEnvs, layout: str, container: bool) -> str:
    """Hermes' answer per layout (``hermes_constants.get_subprocess_home``, auto mode on a host vs. a container)."""
    if layout == "profile_mode" or container:
        return str(envs.hermes_home / "home")
    if layout == "account":
        return str(envs.account)
    return pwd.getpwuid(os.getuid()).pw_dir


@pytest.fixture(autouse=True)
def _upstream():
    require_upstream()


@pytest.mark.parametrize("layout", LAYOUTS)
def test_i_f20_effective_home_is_the_same_in_both_isolation_modes(tmp_path, layout):
    """★ L3-F20: the plugin's home is the same in the gateway process and in the plugin host, and it is the home
    Hermes' own file tools expand ``~`` to in each of them (mutant M1: ``effective_home`` returns ``Path.home()``,
    the source before the fix ⇒ the two modes disagree)."""
    envs = mode_envs(tmp_path, layout)
    launch = run_json(_PROBE_HOME, envs.launch, envs.launch_cwd)
    host = run_json(_PROBE_HOME, envs.host, envs.hermes_home)
    want = expected_home(envs, layout, launch["container"])
    assert (launch["plugin_home"], host["plugin_home"]) == (want, want)
    assert (launch["hermes_tilde"], host["hermes_tilde"]) == (want, want)


def test_i_f20_process_home_differs_between_isolation_modes(tmp_path):
    """The cause, pinned: ``Path.home()`` (what the plugin read before) and the working directory differ between
    the two modes, because Hermes rewrites ``HOME`` for the plugin host and starts it in ``HERMES_HOME``."""
    envs = mode_envs(tmp_path, "profile_mode")
    launch = run_json(_PROBE_PROCESS, envs.launch, envs.launch_cwd)
    host = run_json(_PROBE_PROCESS, envs.host, envs.hermes_home)
    assert launch["process_home"] == str(envs.account)
    assert host["process_home"] == str(envs.hermes_home / "home")
    assert (launch["cwd"], host["cwd"]) == (str(envs.launch_cwd), str(envs.hermes_home))
    lab = mode_envs(tmp_path / "lab", "lab")
    launch = run_json(_PROBE_PROCESS, lab.launch, lab.launch_cwd)
    host = run_json(_PROBE_PROCESS, lab.host, lab.hermes_home)
    if not launch["container"]:  # the lab finding itself: the gateway says <hh>/home, the plugin host the real home
        assert launch["process_home"] == str(lab.hermes_home / "home")
        assert host["process_home"] == pwd.getpwuid(os.getuid()).pw_dir


def _profile_home(hermes_home: Path) -> Path:
    home = hermes_home / "home"
    home.mkdir()
    return home


def test_i_f20_effective_home_reads_home_mode_from_the_profile_config(hermes_home, user_home, monkeypatch):
    """★ R12: a process that did not bridge ``terminal.*`` into the environment (a CLI subcommand, the dashboard)
    still follows ``terminal.home_mode`` of the profile, as the gateway does after its bridge; a set
    ``TERMINAL_HOME_MODE`` wins (mutant M3: no config fallback ⇒ the user's home instead of the profile home)."""
    profile_home = _profile_home(hermes_home)
    monkeypatch.delenv("TERMINAL_HOME_MODE", raising=False)
    (hermes_home / "config.yaml").write_text("terminal:\n  home_mode: profile\n", encoding="utf-8")
    assert hostcompat.effective_home() == profile_home
    monkeypatch.setenv("TERMINAL_HOME_MODE", "real")
    assert hostcompat.effective_home() == user_home


def test_i_f20_effective_home_without_hermes_is_capability_missing(monkeypatch):
    monkeypatch.setitem(sys.modules, "hermes_constants", None)
    with pytest.raises(CapabilityMissing) as info:
        hostcompat.effective_home()
    assert info.value.details["capability"] == "hermes_constants.get_subprocess_home"


def test_i_f20_fake_effective_home_is_the_test_home(fake_host, user_home):
    assert isinstance(fake_host, FakeHost)
    assert hostcompat.effective_home() == user_home
    assert ("effective_home", ()) in fake_host.calls
