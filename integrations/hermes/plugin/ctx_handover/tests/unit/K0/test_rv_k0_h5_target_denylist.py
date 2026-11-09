"""rv-k0, review finding H5 (``review-code/REVIEW.md`` §2, g4 H-3): the target denylist of R19 holds beyond the
process's own ``$HOME``.

R19: the denylist (Hermes root, autostart locations, SSH directory, shell and git configuration) applies always,
"also when the operator widens ``allow_roots``". The reviewer's probes (``g4-policy/probes/p1_target.py``,
``p7_chain.py``) showed three gaps, all at ``policy.check_target_dest`` — the gate behind a peer's
``suggested_dest`` (``pathmap.choose_dest``), ``materialize.assess`` and the workspace-root check of ``envfacts``:

1. Windows, ``allow_roots: ["~"]``: ``Documents\\WindowsPowerShell`` (the PowerShell profile folder, run at every
   PowerShell start) was writable;
2. Hermes as root with ``allow_roots`` ⊇ ``/home``: another account's ``.ssh`` (missing or empty ⇒ FRESH);
3. ``allow_roots: ["/"]``: ``/etc/cron.d``, ``/etc/systemd/system``, ``/var/spool/cron/crontabs``.

The tests build the policy the way production does (``PolicyConfig.from_options``) and only resolve paths; nothing
is written outside tmp.
"""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import pathmap, policy
from ctx_handover.engine.model import ItemRole, Options, Origin
from ctx_handover.engine.policy import PolicyConfig, Verdict
from ctx_handover.tests.fakes.planning_fake import env_facts, plan_item

OK = Verdict(True, None)
DENIED = Verdict(False, "target_denied")


def _cfg(home: Path, allow: tuple[str, ...], *, target_os: str = "linux", ci: bool = False) -> PolicyConfig:
    hermes = Path(home) / ".hermes"
    return PolicyConfig.from_options(
        replace(Options(), allow_roots=allow),
        home=Path(home),
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=ci,
        target_os=target_os,
    )


def _dest(path: Path | str, cfg: PolicyConfig) -> Verdict:
    return policy.check_target_dest(Path(path), cfg, recorded=False, local_choice=False)


@pytest.fixture
def home(tmp_path) -> Path:
    h = tmp_path / "u"
    (h / ".hermes").mkdir(parents=True)
    return h


# --------------------------------------------------------------------------------------------------------------
# 1. PowerShell profile folders on Windows targets
# --------------------------------------------------------------------------------------------------------------

POWERSHELL_DENIED = [
    "Documents/WindowsPowerShell",
    "Documents/WindowsPowerShell/Microsoft.PowerShell_profile.ps1",
    "Documents/PowerShell",
    "Documents/PowerShell/Modules/evil",
    "documents/powershell/profile.ps1",
    "OneDrive/Documents/PowerShell",
    "OneDrive - Contoso/Documents/WindowsPowerShell",
    "AppData/Local/Microsoft/WindowsApps/python.exe",
    # ancestors: a tree written there lands its entries in a profile folder
    "Documents",
    "OneDrive",
    "OneDrive - Contoso/Documents",
]


@pytest.mark.parametrize("rel", POWERSHELL_DENIED)
def test_powershell_profile_folders_denied_under_a_widened_allow_root(rel, home):
    """★ Reviewer case 1: Windows target, ``allow_roots: ["~"]``; a peer suggests the PowerShell profile folder.
    Before the fix: ``Verdict(True, None)``."""
    cfg = _cfg(home, ("~",), target_os="windows", ci=True)
    assert _dest(home / rel, cfg) == DENIED, rel


def test_powershell_rule_keeps_ordinary_documents_paths(home):
    cfg = _cfg(home, ("~",), target_os="windows", ci=True)
    for rel in ("Documents/proj", "Documents/PowerShellNotes", "OneDrive/Projects/app", "proj"):
        assert _dest(home / rel, cfg) == OK, rel


def test_powershell_rule_is_scoped_to_windows_targets(home):
    """PowerShell on Linux/macOS reads ``~/.config/powershell`` (a dot directory, denied anyway); a Linux target keeps
    ``~/Documents`` as a destination."""
    cfg = _cfg(home, ("~",))
    for rel in ("Documents", "Documents/WindowsPowerShell", "OneDrive/Documents/PowerShell"):
        assert _dest(home / rel, cfg) == OK, rel
    assert _dest(home / ".config" / "powershell", cfg) == DENIED


# --------------------------------------------------------------------------------------------------------------
# 2. Other accounts' homes
# --------------------------------------------------------------------------------------------------------------

FOREIGN_DENIED = [
    "/home/alice/.ssh",
    "/home/alice/.ssh/authorized_keys",
    "/home/alice/.bashrc",
    "/home/alice/.profile",
    "/home/alice/.config/autostart/x.desktop",
    "/home/alice/.config/systemd/user/x.service",
    "/home/alice/.local/bin/x",
    "/home/alice/bin",
    "/home/alice/.gitconfig",
    "/home/alice/.anything",
    "/home/alice",
    "/home",
]


@pytest.mark.parametrize("dest", FOREIGN_DENIED)
def test_other_users_home_keeps_the_home_denylist(dest):
    """★ Reviewer case 2: Hermes runs as root (``$HOME=/root``), ``allow_roots: ["/home"]``; a peer suggests
    ``/home/alice/.ssh``. Before the fix: ``Verdict(True, None)``."""
    cfg = _cfg(Path("/root"), ("/home",))
    assert _dest(dest, cfg) == DENIED, dest


def test_other_users_home_keeps_ordinary_project_paths():
    cfg = _cfg(Path("/root"), ("/home",))
    for dest in ("/home/alice/proj", "/home/alice/work/app", "/home/alice/proj/.venv"):
        assert _dest(dest, cfg) == OK, dest


def test_users_folders_of_macos_and_windows_are_homes(home):
    cfg = _cfg(home, ("/",), target_os="windows", ci=True)
    for dest in (
        "/Users/bob/.ssh",
        "/Users/bob/Documents/PowerShell",
        "/users/Bob/AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/x.lnk",
        "/Users",
    ):
        assert _dest(dest, cfg) == DENIED, dest
    assert _dest("/Users/bob/src/app", cfg) == OK


def test_account_homes_outside_the_usual_parents(home, tmp_path, monkeypatch):
    """★ A service account's home from the password database (``/var/lib/postgresql``, ``/srv/ftp`` …) gets the same
    rules. ``policy._account_homes`` is the discovery seam; before the fix it did not exist and the dot directory was
    writable."""
    svc = tmp_path / "srv" / "svc"
    svc.mkdir(parents=True)
    monkeypatch.setattr(policy, "_account_homes", lambda: (svc,), raising=False)
    cfg = _cfg(home, (str(tmp_path / "srv"),))
    for dest in (svc / ".ssh", svc / ".bashrc", svc / "bin" / "x", svc):
        assert _dest(dest, cfg) == DENIED, dest
    assert _dest(svc / "work", cfg) == OK


@pytest.mark.skipif(os.name != "posix", reason="password database")
def test_account_homes_come_from_the_password_database():
    import pwd

    own = Path(os.path.normpath(pwd.getpwuid(os.getuid()).pw_dir))
    homes = policy._account_homes()
    assert own in homes or own == Path("/")
    assert Path("/") not in homes
    assert all(h.is_absolute() for h in homes)


def test_peer_suggestion_into_another_users_ssh_is_not_chosen():
    """★ The reviewer's chain (``p7_chain.py``): ``pathmap.choose_dest`` took the peer's ``suggested_dest``
    ``/home/alice/.ssh`` (reason ``suggested``) because it lay under ``allow_roots``. Now the gate refuses it."""
    o = replace(Options(), allow_roots=("/home",))
    cfg = _cfg(Path("/root"), ("/home",))
    target = env_facts("laptop", home="/root", allow_roots=("/home",), workspace_root="/home")
    source = env_facts("server", home="/home/src")
    item = plan_item(
        "w1", "/home/src/ps", role=ItemRole.SECONDARY, origin=Origin.TRANSCRIPT, suggested_dest="/home/alice/.ssh"
    )
    dest, reason = pathmap.choose_dest(
        item,
        source_facts=source,
        target_facts=target,
        lookup=lambda p, d: None,
        options=o,
        local_choice=False,
        job_id="01JCJOB0000000000000000000",
        cfg=cfg,
    )
    assert dest != "/home/alice/.ssh"
    assert reason != pathmap.SUGGESTED


# --------------------------------------------------------------------------------------------------------------
# 3. System autostart and configuration places
# --------------------------------------------------------------------------------------------------------------

SYSTEM_DENIED_POSIX = [
    "/etc/cron.d/x",
    "/etc/cron.daily/x",
    "/etc/crontab",
    "/etc/systemd/system/x.service",
    "/etc/profile.d/x.sh",
    "/etc/init.d/x",
    "/etc/xdg/autostart/x.desktop",
    "/etc/ld.so.preload",
    "/etc",
    "/var/spool/cron/crontabs/root",
    "/var/at/tabs/root",
    "/usr/local/bin/x",
    "/usr/lib/systemd/system/x.service",
    "/lib/systemd/system/x.service",
    "/run/systemd/system/x.service",
    "/bin/x",
    "/sbin/x",
    "/boot/x",
    "/Library/LaunchDaemons/x.plist",
    "/Library/LaunchAgents/x.plist",
    "/System/Library/x",
    "/private/etc/periodic/daily/x",
    "/opt/homebrew/bin/x",
    "/var/root/.ssh",
    "/root/.ssh",
    "/var",
    "/",
]


@pytest.mark.parametrize("dest", SYSTEM_DENIED_POSIX)
def test_system_autostart_places_denied_for_a_root_allow_root(dest):
    """★ Reviewer case 3: Hermes as root with ``allow_roots: ["/"]``. Before the fix ``/etc/cron.d/x``,
    ``/etc/systemd/system/x.service`` and ``/var/spool/cron/crontabs/root`` were ``Verdict(True, None)``."""
    cfg = _cfg(Path("/root"), ("/",))
    assert _dest(dest, cfg) == DENIED, dest


def test_system_rule_keeps_data_places():
    cfg = _cfg(Path("/root"), ("/",))
    for dest in ("/srv/work/proj", "/opt/data/projects/app", "/data/x", "/mnt/disk/proj", "/var/www/site"):
        assert _dest(dest, cfg) == OK, dest


SYSTEM_DENIED_WINDOWS = [
    "/ProgramData/Microsoft/Windows/Start Menu/Programs/StartUp/x.lnk",
    "/ProgramData/chocolatey/bin/x.exe",
    "/Windows/System32/x.dll",
    "/windows/system32/Tasks/x",
    "/Program Files/x",
    "/Program Files (x86)/x",
]


@pytest.mark.parametrize("dest", SYSTEM_DENIED_WINDOWS)
def test_windows_system_places_denied(dest, home):
    """Windows host with a drive as allow root; the components after the drive are the same as these after ``/``."""
    cfg = _cfg(home, ("/",), target_os="windows", ci=True)
    assert _dest(dest, cfg) == DENIED, dest


def test_windows_rule_does_not_take_posix_names(home):
    """``C:\\dev`` and ``C:\\bin`` are common workspace roots on Windows; the POSIX system list is not applied."""
    cfg = _cfg(home, ("/",), target_os="windows", ci=True)
    for dest in ("/dev/projects/app", "/bin/tools", "/usr/x", "/etc/x"):
        assert _dest(dest, cfg) == OK, dest


def test_local_choice_and_recorded_paths_keep_the_denylist():
    """R19 holds for a destination the local user typed and for a recorded return path too."""
    cfg = _cfg(Path("/root"), ("/root/hermes-handover",))
    for dest in ("/etc/cron.d/x", "/home/alice/.ssh", "/usr/local/bin"):
        assert policy.check_target_dest(Path(dest), cfg, recorded=True, local_choice=True) == DENIED, dest
