"""D · L3-F20: the home in a side's environment facts is the home of that side's policy — one home for the facts,
the path map, the denylist and the collector — and the facts agree under ``plugins.isolation: in_process`` and
``host``.

Before: ``envfacts.probe`` reported and resolved ``~/hermes-handover`` with the process home (``SystemOps.home`` =
``Path.home()``) even when the caller passed its policy (``cfg``) with another home; under ``host`` the process
home is the one Hermes rewrote for the plugin host (``tests/unit/K0/test_i_f20_home_source.py``). The composition
root builds the policy home from ``hostcompat.effective_home`` (wish W1 of package i-f20 for ``jobs._policy``);
the end-to-end test here composes the policy the same way.
"""

from __future__ import annotations

from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import envfacts
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.planning_fake import host_facts
from ctx_handover.tests.unit.K0.test_i_f20_home_source import LAYOUTS, expected_home, mode_envs, run_json


def _cfg(home: Path, hermes: Path) -> PolicyConfig:
    return PolicyConfig.from_options(
        Options(),
        home=home,
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )


def _probe(tmp_path: Path, process_home: Path, hermes: Path, cfg: PolicyConfig | None):
    system = replace(envfacts.default_system(), home=lambda: str(process_home), which=lambda _n: None)
    host = host_facts(hermes_home=str(hermes), document_cache=str(hermes / "cache" / "documents"))
    return envfacts.probe(
        instance_name="b", options=Options(), host=host, config={}, system=system, scratch_dir=tmp_path, cfg=cfg
    )


def _dirs(tmp_path: Path, *names: str) -> list[Path]:
    out = []
    for name in names:
        (tmp_path / name).mkdir(parents=True)
        out.append(tmp_path / name)
    return out


def test_i_f20_probe_reports_the_home_of_the_given_policy(tmp_path):
    """★ With ``cfg`` the facts name the policy's home and resolve the default root under it — not under the
    process home (mutant M2: ``home = s.home()`` ⇒ the process home, and the root check against the policy fails
    with ``not_allowed_root``). Without ``cfg`` the process home stays the source."""
    process_home, policy_home, hermes = _dirs(tmp_path, "process-home", "policy-home", "hh")
    facts = _probe(tmp_path, process_home, hermes, _cfg(policy_home, hermes))
    root = str(policy_home / "hermes-handover")
    assert facts.home == str(policy_home)
    assert (facts.workspace_root, facts.allow_roots) == (root, (root,))
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (True, "")
    assert root in facts.disk_free and set(facts.case_insensitive) == {root}
    plain = _probe(tmp_path, process_home, hermes, None)
    assert (plain.home, plain.workspace_root) == (str(process_home), str(process_home / "hermes-handover"))


def test_i_f20_probe_profile_home_default_root_is_the_reserved_directory(tmp_path):
    """★ R29 with the profile home as the policy home (``terminal.home_mode: profile``, a container): the default
    ``~/hermes-handover`` lies in the Hermes home, so the root is ``<HERMES_HOME>/handover-workspaces`` and usable —
    whatever the process home is (mutant M2 ⇒ ``<process home>/hermes-handover``, ``not_allowed_root``)."""
    account, hermes = _dirs(tmp_path, "account", "hh")
    (hermes / "home").mkdir()
    facts = _probe(tmp_path, account, hermes, _cfg(hermes / "home", hermes))
    reserved = str(hermes / "handover-workspaces")
    assert (facts.home, facts.workspace_root) == (str(hermes / "home"), reserved)
    assert (facts.workspace_root_ok, facts.workspace_root_problem) == (True, "")


_PROBE_FACTS = r"""
import json, shutil, tempfile
from dataclasses import replace
from pathlib import Path
import hermes_constants
hermes_constants.export_scratch_tmp_env()  # hermes_bootstrap does this at the start of every Hermes process
from ctx_handover.engine import envfacts, hostcompat
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig
hermes = hostcompat.hermes_paths()
cfg = PolicyConfig.from_options(
    Options(), home=hostcompat.effective_home(), hermes_home=Path(hermes.home), hermes_root=hostcompat.hermes_root(),
    plugin_dir=Path(hermes.home) / "plugin-data" / "ctx_handover", case_insensitive=False, target_os="linux",
    document_cache=Path(hermes.document_cache),
)
base = envfacts.default_system()
system = replace(base, which=lambda n: base.which(n) if n == "git" else None)
facts = to_wire(envfacts.probe(
    instance_name="a", options=Options(), host=hostcompat.host_facts(), config=hostcompat.load_config(),
    system=system, cfg=cfg,
))
facts["disk_free"] = sorted(facts["disk_free"])  # free bytes move with other writers; the paths must agree
facts["tools"] = {name: info["path"] for name, info in facts["tools"].items()}  # versions: timing, not source
facts.pop("git_version")
print(json.dumps({
    "facts": facts,
    "which": {name: shutil.which(name) for name in envfacts.TOOL_ALLOWLIST},
    "tempdir": tempfile.gettempdir(),
    "container": bool(hermes_constants.is_container()),
}))
"""


@pytest.mark.parametrize("layout", LAYOUTS)
def test_i_f20_env_facts_agree_in_both_isolation_modes(tmp_path, layout):
    """★ L3-F20 end to end, without Hermes running: the facts a side reports — home, user, shell, OS, locale,
    time zone, Hermes home, profile, document cache, terminal backend, write root and its checks, tool paths — are
    the same in the gateway's process and in the plugin host, the temp directory and the ``PATH`` lookup too, and
    the home is Hermes' home of the profile (mutants M1/M2 ⇒ the homes differ)."""
    envs = mode_envs(tmp_path, layout)
    launch = run_json(_PROBE_FACTS, envs.launch, envs.launch_cwd)
    host = run_json(_PROBE_FACTS, envs.host, envs.hermes_home)
    assert launch["facts"]["home"] == expected_home(envs, layout, launch["container"])
    assert launch["facts"] == host["facts"]
    assert (launch["which"], launch["tempdir"]) == (host["which"], host["tempdir"])
