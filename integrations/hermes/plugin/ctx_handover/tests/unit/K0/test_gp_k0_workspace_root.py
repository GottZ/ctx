"""K0 round G′, V40 / R29 (lab finding L3-F01): the default write root when ``HOME`` lies in the Hermes home.

The official Docker image sets ``HOME=/opt/data`` and ``HERMES_HOME=/opt/data``: ``~/hermes-handover`` then lies in the
Hermes home, which the target denylist refuses, so a Docker Hermes accepted no handover at all. R29: the default root
is then ``<HERMES_HOME>/handover-workspaces`` — the one exception from the target denylist for the Hermes home; every
other Hermes path, the neighbour ``handover-workspaces-x`` and symlink escapes stay denied. Real directories in tmp.
"""

from __future__ import annotations

import os
from dataclasses import dataclass
from pathlib import Path

import pytest

from ctx_handover.engine import paths, policy
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig, Verdict

OK = Verdict(True, None)
DENIED = Verdict(False, "target_denied")


@dataclass
class Host:
    home: Path
    hermes_home: Path
    hermes_root: Path

    @property
    def plugin_dir(self) -> Path:
        return self.hermes_home / "plugin-data" / "ctx_handover"

    @property
    def reserved(self) -> Path:
        return self.hermes_home / "handover-workspaces"

    def cfg(self, options: Options | None = None, **over) -> PolicyConfig:
        kwargs = {
            "home": self.home,
            "hermes_home": self.hermes_home,
            "hermes_root": self.hermes_root,
            "plugin_dir": self.plugin_dir,
            "case_insensitive": False,
            "target_os": "linux",
        }
        kwargs.update(over)
        return PolicyConfig.from_options(options or Options(), **kwargs)

    def check(self, dest: Path, cfg: PolicyConfig | None = None, **kw) -> Verdict:
        kw.setdefault("recorded", False)
        kw.setdefault("local_choice", False)
        return policy.check_target_dest(dest, cfg or self.cfg(), **kw)


def _mk(*dirs: Path) -> None:
    for d in dirs:
        d.mkdir(parents=True, exist_ok=True)


def make_docker(tmp_path: Path) -> Host:
    """The official image: HOME = HERMES_HOME = Hermes root (``/opt/data``)."""
    data = tmp_path / "opt" / "data"
    host = Host(home=data, hermes_home=data, hermes_root=data)
    _mk(data, host.plugin_dir, data / "profiles" / "coder", data / "skills")
    (data / "config.yaml").write_text("model: x\n", encoding="utf-8")
    (data / ".env").write_text("K=v\n", encoding="utf-8")
    return host


def make_lab(tmp_path: Path) -> Host:
    """The L3 lab: ``HOME=lab/b/home`` inside ``HERMES_HOME=lab/b``."""
    hh = tmp_path / "lab" / "b"
    host = Host(home=hh / "home", hermes_home=hh, hermes_root=hh)
    _mk(host.home, host.plugin_dir)
    return host


def make_docker_profile(tmp_path: Path) -> Host:
    """Docker with an active profile: HOME = Hermes root ``/opt/data``, active home ``/opt/data/profiles/coder``."""
    data = tmp_path / "opt" / "data"
    host = Host(home=data, hermes_home=data / "profiles" / "coder", hermes_root=data)
    _mk(data, host.hermes_home, host.plugin_dir)
    return host


@pytest.fixture
def docker(tmp_path) -> Host:
    return make_docker(tmp_path)


@pytest.fixture
def lab(tmp_path) -> Host:
    return make_lab(tmp_path)


@pytest.fixture
def docker_profile(tmp_path) -> Host:
    return make_docker_profile(tmp_path)


@pytest.fixture
def classic(tmp_path) -> Host:
    """A desktop: ``~/.hermes`` inside an ordinary home."""
    home = tmp_path / "u"
    host = Host(home=home, hermes_home=home / ".hermes", hermes_root=home / ".hermes")
    _mk(home, host.plugin_dir)
    return host


# --------------------------------------------------------------------------------------------------------
# paths.default_workspace_root / expand_handover_root
# --------------------------------------------------------------------------------------------------------


def test_default_root_outside_the_hermes_home_is_home_hermes_handover(classic, tmp_path):
    """★ An ordinary home keeps ``~/hermes-handover``; ``hermes_root`` changes nothing there.

    Red before the fix: ``AttributeError: module 'ctx_handover.engine.paths' has no attribute
    'default_workspace_root'``."""
    assert paths.default_workspace_root(classic.home, classic.hermes_home) == classic.home / "hermes-handover"
    got = paths.default_workspace_root(classic.home, classic.hermes_home, hermes_root=classic.hermes_root)
    assert got == classic.home / "hermes-handover"
    assert paths.default_workspace_root(str(classic.home) + "/", str(classic.hermes_home)) == got


def test_default_root_in_the_hermes_home_is_the_reserved_directory(docker, lab):
    """★ HOME = HERMES_HOME (Docker) and HOME inside HERMES_HOME (lab) ⇒ ``<HERMES_HOME>/handover-workspaces``."""
    assert paths.default_workspace_root(docker.home, docker.hermes_home) == docker.reserved
    assert paths.default_workspace_root(lab.home, lab.hermes_home) == lab.reserved
    assert paths.HERMES_WORKSPACES_DIR == "handover-workspaces"
    assert paths.DEFAULT_WORKSPACE_SPEC == "~/hermes-handover"


def test_default_root_with_a_profile_counts_the_hermes_root(docker_profile):
    """★ Docker with a profile: ``~/hermes-handover`` lies in the Hermes root (denied too) ⇒ the reserved directory
    of the active home; without ``hermes_root`` only the active home is known."""
    h = docker_profile
    assert paths.default_workspace_root(h.home, h.hermes_home, hermes_root=h.hermes_root) == h.reserved
    assert paths.default_workspace_root(h.home, h.hermes_home) == h.home / "hermes-handover"


def test_default_root_sees_a_home_that_resolves_into_the_hermes_home(tmp_path):
    """★ A home that is a symlink into the Hermes home counts as inside (the denylist compares realpaths too)."""
    data = tmp_path / "data"
    _mk(data)
    link = tmp_path / "home-link"
    os.symlink(data, link)
    assert paths.default_workspace_root(link, data) == data / "handover-workspaces"


def test_default_root_case_insensitive_and_relative(tmp_path):
    """On a case-insensitive host a differently cased Hermes home still contains the home; relative paths are
    refused."""
    home = tmp_path / "Data"
    got = paths.default_workspace_root(home, tmp_path / "data", case_insensitive=True)
    assert got == tmp_path / "data" / "handover-workspaces"
    assert paths.default_workspace_root(home, tmp_path / "data") == home / "hermes-handover"
    for args in ((Path("rel"), tmp_path), (tmp_path, Path("rel"))):
        with pytest.raises(PolicyDenied):
            paths.default_workspace_root(*args)


def test_expand_handover_root_maps_only_the_default_spelling(docker, classic):
    """★ Every spelling that expands to ``~/hermes-handover`` is the default root; any other root is
    ``expand_root`` unchanged (an explicit root in the Hermes home stays what it is — and denied)."""
    for spec in ("~/hermes-handover", "~/hermes-handover/", "hermes-handover", str(docker.home / "hermes-handover")):
        assert paths.expand_handover_root(spec, docker.home, docker.hermes_home) == docker.reserved, spec
    assert paths.expand_handover_root("~/work", docker.home, docker.hermes_home) == docker.home / "work"
    assert paths.expand_handover_root("/srv/ws", docker.home, docker.hermes_home) == Path("/srv/ws")
    assert (
        paths.expand_handover_root("~/hermes-handover", classic.home, classic.hermes_home)
        == classic.home / "hermes-handover"
    )
    with pytest.raises(PolicyDenied):
        paths.expand_handover_root("~other/x", docker.home, docker.hermes_home)


# --------------------------------------------------------------------------------------------------------
# PolicyConfig.from_options and check_target_dest
# --------------------------------------------------------------------------------------------------------


def test_docker_default_allow_roots_resolve_to_the_reserved_directory(docker, docker_profile, classic):
    """★ Integrator point (2): default ``allow_roots`` ``~/hermes-handover`` in a Docker home is the reserved
    directory, so a counterpart's destination under it is not ``not_allowed_root``.

    Red before the fix: ``assert (PosixPath('…/opt/data/hermes-handover'),) == (PosixPath('…/opt/data/
    handover-workspaces'),)``."""
    assert docker.cfg().allow_roots == (docker.reserved,)
    assert docker_profile.cfg().allow_roots == (docker_profile.reserved,)
    assert classic.cfg().allow_roots == (classic.home / "hermes-handover",)
    cfg = docker.cfg(Options(allow_roots=("~/hermes-handover", "~/elsewhere")))
    assert cfg.allow_roots == (docker.reserved, docker.home / "elsewhere")


@pytest.mark.parametrize("layout", ["docker", "lab", "docker_profile"])
def test_reserved_directory_accepts_writes_with_default_settings(layout, request):
    """★ L3-F01: with default settings a received workspace lands under ``<HERMES_HOME>/handover-workspaces`` —
    the directory itself and anything below it, driven by a counterpart (not recorded, not typed locally).

    Red before the fix: ``assert Verdict(allowed=False, reason='target_denied') == Verdict(allowed=True,
    reason=None)``."""
    h: Host = request.getfixturevalue(layout)
    for dest in (h.reserved, h.reserved / "proj", h.reserved / "_handover" / "01J" / "a" / "b.txt"):
        assert h.check(dest) == OK, dest
    root = paths.default_workspace_root(h.home, h.hermes_home, hermes_root=h.hermes_root)
    assert h.check(root / "proj") == OK


@pytest.mark.parametrize("layout", ["docker", "lab", "docker_profile"])
def test_every_other_hermes_path_stays_denied(layout, request):
    """★ The exception is exactly the reserved directory: the neighbour ``handover-workspaces-x``, a prefix
    spelling, the Hermes home itself, its config, ``.env``, plugin data, skills, other profiles and the
    reserved directory of another profile stay ``target_denied`` — also when recorded or typed locally."""
    h: Host = request.getfixturevalue(layout)
    cfg = h.cfg(Options(allow_roots=(str(h.hermes_root),)))
    hh, root = h.hermes_home, h.hermes_root
    denied = [
        hh / "handover-workspaces-x" / "proj",
        hh / "handover-workspaces-x",
        hh / "handover-workspace" / "proj",
        hh / "handover",
        hh,
        hh / "config.yaml",
        hh / ".env",
        hh / "skills" / "x",
        h.plugin_dir / "blocks" / "x",
        hh / "handover-workspaces" / ".." / "config.yaml",
        root / "profiles" / "other" / "handover-workspaces" / "proj",
        root,
    ]
    if root != hh:
        denied.append(root / "handover-workspaces" / "proj")
    for dest in denied:
        for recorded, local in ((False, False), (True, False), (False, True)):
            assert h.check(dest, cfg, recorded=recorded, local_choice=local) == DENIED, (dest, recorded, local)


def test_symlink_escapes_from_the_reserved_directory_stay_denied(docker):
    """★ A link inside the reserved directory that resolves to the Hermes home, and a reserved directory that is
    itself a link into the Hermes home, are judged by their realpath: denied."""
    h = docker
    _mk(h.reserved)
    os.symlink(h.hermes_home, h.reserved / "up")
    assert h.check(h.reserved / "up" / "config.yaml") == DENIED
    assert h.check(h.reserved / "up" / "new-dir" / "x") == DENIED
    os.rename(h.reserved, h.hermes_home / "ws-old")
    os.symlink(h.hermes_home / "skills", h.reserved)
    assert h.check(h.reserved / "proj") == DENIED
    assert h.check(h.reserved / "proj", recorded=True, local_choice=True) == DENIED


def test_case_folding_of_the_exception_follows_the_host(docker):
    """On a case-sensitive host ``Handover-Workspaces`` is another directory in the Hermes home (denied); on a
    case-insensitive one it is the reserved directory."""
    h = docker
    other = h.hermes_home / "Handover-Workspaces" / "proj"
    assert h.check(other, recorded=True) == DENIED
    assert h.check(other, h.cfg(case_insensitive=True)) == OK


def test_reserved_directory_outside_docker_still_needs_its_dot_entry_opened(classic):
    """A desktop's reserved directory lies under ``~/.hermes`` — the dot-entry rule of the home still refuses it
    unless an allow-root opens ``~/.hermes``; the default write root stays ``~/hermes-handover``."""
    h = classic
    assert h.check(h.reserved / "proj", local_choice=True) == DENIED
    assert h.check(h.home / "hermes-handover" / "proj") == OK


def test_reserved_workspace_roots_helper(docker, tmp_path):
    """``policy.reserved_workspace_roots(cfg)`` names the reserved directory as given and resolved."""
    assert policy.reserved_workspace_roots(docker.cfg()) == (docker.reserved,)
    real = tmp_path / "real-hh"
    _mk(real)
    link = tmp_path / "hh-link"
    os.symlink(real, link)
    h = Host(home=tmp_path / "x", hermes_home=link, hermes_root=link)
    assert policy.reserved_workspace_roots(h.cfg()) == (link / "handover-workspaces", real / "handover-workspaces")
    assert h.check(real / "handover-workspaces" / "p", recorded=True) == OK
    assert h.check(real / "other" / "p", recorded=True) == DENIED


def test_exception_never_lifts_the_plugin_data_dir(docker):
    """★ The exception lifts only the Hermes home and root: plugin data placed inside the reserved directory (an
    unusual layout) stays denied."""
    h = docker
    cfg = h.cfg(plugin_dir=h.reserved / "plugin-data")
    assert h.check(h.reserved / "proj", cfg) == OK
    assert h.check(h.reserved / "plugin-data" / "blocks" / "x", cfg, recorded=True) == DENIED


def test_home_of_a_foreign_os_falls_back_to_expand_root(tmp_path):
    """★ Integrator decision: a ``home``/``hermes_home`` this host cannot read as absolute (a Windows home on a
    POSIX host, as the D collector tests build) is no error — ``expand_handover_root`` is ``expand_root`` then, and
    R29 applies only where both are absolute here.

    Red before the fix: ``PolicyDenied: unsafe path (root_not_absolute): 'C:\\\\Users\\\\x'`` from
    ``PolicyConfig.from_options`` (D: ``test_collector_msys_paths_on_windows_source``)."""
    win = Path("C:\\Users\\x")
    if win.is_absolute():  # pragma: no cover - a Windows host reads it as absolute
        pytest.skip("Windows host")
    hh = tmp_path / "hh"
    want = paths.expand_root("~/hermes-handover", win)
    assert paths.expand_handover_root("~/hermes-handover", win, hh) == want
    assert paths.expand_handover_root("~/hermes-handover", hh, win) == paths.expand_root("~/hermes-handover", hh)
    assert paths.expand_handover_root("~/hermes-handover", hh, hh, hermes_root=win) == paths.expand_root(
        "~/hermes-handover", hh
    )
    cfg = PolicyConfig.from_options(
        Options(),
        home=win,
        hermes_home=hh,
        hermes_root=hh,
        plugin_dir=hh / "plugin-data" / "ctx_handover",
        case_insensitive=True,
        target_os="windows",
    )
    assert cfg.allow_roots == (want,)
    with pytest.raises(PolicyDenied):
        paths.default_workspace_root(win, hh)
