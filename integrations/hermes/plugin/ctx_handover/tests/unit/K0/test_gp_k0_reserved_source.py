"""K0 round G′, integrator decision on the open point (return trip R4 in Docker, HOME = HERMES_HOME): the reserved
directory ``<HERMES_HOME>/handover-workspaces`` (exactly it and below) is also a permitted SOURCE — same rule as on the
target side: no neighbour path, no symlink escape; the plugin data dir, every other Hermes path and the Hermes
credential stores stay protected. Real files in tmp; every check also with one shared ``SourceCheckCache``."""

from __future__ import annotations

import os
from pathlib import Path

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.policy import SourceCheckCache, Verdict
from ctx_handover.tests.unit.K0.test_gp_k0_workspace_root import Host, make_docker, make_docker_profile, make_lab

OK = Verdict(True, None)
HERMES = Verdict(False, "hermes")


@pytest.fixture
def docker(tmp_path) -> Host:
    return make_docker(tmp_path)


@pytest.fixture
def lab(tmp_path) -> Host:
    return make_lab(tmp_path)


@pytest.fixture
def docker_profile(tmp_path) -> Host:
    return make_docker_profile(tmp_path)


def _file(path: Path, text: str = "hello\n") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


def _source(path: Path, cfg, *, item_root: Path | None, explicit: bool = False) -> list[Verdict]:
    """The verdict without and with a shared per-scan cache (both must agree)."""
    cache = SourceCheckCache()
    return [
        policy.check_source_file(path, cfg, explicit=explicit, item_root=item_root),
        policy.check_source_file(path, cfg, explicit=explicit, item_root=item_root, cache=cache),
    ]


@pytest.mark.parametrize("layout", ["docker", "lab", "docker_profile"])
def test_a_received_workspace_can_be_sent_on(layout, request):
    """★ R4 return trip from a Docker host: a workspace under the reserved directory is a source — its files, its
    root directory and ``is_hermes_protected``.

    Red before the fix: ``assert [Verdict(allowed=False, reason='hermes'), …] == [Verdict(allowed=True,
    reason=None), …]``."""
    h: Host = request.getfixturevalue(layout)
    cfg = h.cfg()
    proj = h.reserved / "proj"
    f = _file(proj / "src" / "a.txt")
    assert _source(f, cfg, item_root=proj) == [OK, OK]
    assert _source(proj, cfg, item_root=proj) == [OK, OK]
    assert policy.is_hermes_protected(f, cfg) is False
    assert policy.is_hermes_protected(f, cfg, cache=SourceCheckCache()) is False


@pytest.mark.parametrize("layout", ["docker", "lab", "docker_profile"])
def test_every_other_hermes_path_stays_protected_as_source(layout, request):
    """★ The neighbour ``handover-workspaces-x``, the Hermes home's own files, skills, other profiles, the
    reserved directory of the Hermes root or another profile stay ``hermes`` — with the item root set to the
    file's own directory, so only the Hermes rule can refuse."""
    h: Host = request.getfixturevalue(layout)
    cfg = h.cfg()
    hh, root = h.hermes_home, h.hermes_root
    refused = [
        hh / "handover-workspaces-x" / "proj" / "a.txt",
        hh / "handover-workspace" / "proj" / "a.txt",
        hh / "skills" / "s" / "SKILL.md",
        hh / "memories" / "MEMORY.md",
        root / "profiles" / "other" / "handover-workspaces" / "proj" / "a.txt",
    ]
    if root != hh:
        refused.append(root / "handover-workspaces" / "proj" / "a.txt")
    for path in refused:
        _file(path)
        assert _source(path, cfg, item_root=path.parent) == [HERMES, HERMES], path
        assert policy.is_hermes_protected(path, cfg) is True, path


def test_plugin_data_inside_the_reserved_directory_stays_protected(docker):
    """★ The exception lifts only the Hermes home and root, never the plugin data dir."""
    h = docker
    cfg = h.cfg(plugin_dir=h.reserved / "plugin-data")
    f = _file(h.reserved / "plugin-data" / "blocks" / "x")
    assert _source(f, cfg, item_root=f.parent) == [HERMES, HERMES]
    g = _file(h.reserved / "proj" / "a.txt")
    assert _source(g, cfg, item_root=g.parent) == [OK, OK]


def test_symlink_escapes_from_the_reserved_directory_stay_protected(docker):
    """★ Judged by the realpath: a followed link to the Hermes config or ``.env``, a file reached through a
    directory link to the Hermes home, and a reserved directory that is itself a link into the Hermes home."""
    h = docker
    proj = h.reserved / "proj"
    proj.mkdir(parents=True)
    cfg = h.cfg()
    os.symlink(h.hermes_home / "config.yaml", proj / "cfg-link")
    os.symlink(h.hermes_home / ".env", proj / "env-link")
    for link in (proj / "cfg-link", proj / "env-link"):
        assert _source(link, cfg, item_root=proj, explicit=True)[0] == HERMES, link
    os.symlink(h.hermes_home, h.reserved / "up")
    assert _source(h.reserved / "up" / "config.yaml", cfg, item_root=h.reserved) == [HERMES, HERMES]
    os.rename(h.reserved, h.hermes_home / "ws-old")
    _file(h.hermes_home / "skills" / "proj" / "a.txt")
    os.symlink(h.hermes_home / "skills", h.reserved)
    f = h.reserved / "proj" / "a.txt"
    assert _source(f, cfg, item_root=h.reserved / "proj") == [HERMES, HERMES]
    assert policy.is_hermes_protected(f, cfg) is True


def test_hermes_credentials_are_never_a_source(docker_profile):
    """★ Credential stores of the active home and the Hermes root stay protected; a secret-named file inside the
    reserved directory is still a secret (the name denylist is unchanged)."""
    h = docker_profile
    cfg = h.cfg()
    for path in (h.hermes_home / "auth.json", h.hermes_home / ".env", h.hermes_root / "auth.json"):
        _file(path, "{}\n")
        assert policy.is_hermes_protected(path, cfg) is True, path
    proj = h.reserved / "proj"
    secret = _file(proj / ".env", "TOKEN=x\n")
    assert _source(secret, cfg, item_root=proj) == [Verdict(False, "secret")] * 2
    os.symlink(h.hermes_home / "auth.json", proj / "auth-link")
    assert _source(proj / "auth-link", cfg, item_root=proj, explicit=True)[0] == HERMES
