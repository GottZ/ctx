"""r3-kd, rest of review finding H5 (``review-code/REVIEW-2.md`` §2, R2-M6): the target denylist of R19 beyond the
homes the policy already knew.

Three gaps the second review left open (g4, read by the lead):

1. **WSL**: a Linux target (WSL) with ``allow_roots: ["/mnt/c/Users/u"]`` — the Windows rules applied only to Windows
   targets, so the Startup folder, ``Documents/WindowsPowerShell`` and ``.ssh`` of that Windows home were writable;
   ``/mnt/c/Windows`` and ``/mnt/c/ProgramData`` too.
2. **Homes outside the known places**: a home neither in the enumerated password database nor below ``/home``,
   ``/Users``, ``/var/home`` (``/srv/users/alice``, an LDAP account) kept its ``.ssh`` writable. The owner of the
   directories on the way is now asked for its home (NSS by uid, which LDAP answers without enumeration).
3. **Localised OneDrive**: the PowerShell profile folder in ``OneDrive/Dokumente`` (German, and the other display
   languages OneDrive names its documents folder in) was writable; ``README.md`` said "under OneDrive" in general.

``policy.check_target_dest`` is the gate behind every destination (``pathmap``, ``materialize``, ``envfacts``); the
tests build the policy the way production does and resolve paths only — nothing is written outside tmp.
"""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig, Verdict

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


# --------------------------------------------------------------------------------------------------------------
# 1. WSL: Windows homes and system folders through the drive mounts of a Linux target
# --------------------------------------------------------------------------------------------------------------

WSL_HOME_DENIED = [
    ".ssh",
    ".ssh/authorized_keys",
    "AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/evil.lnk",
    "Documents/WindowsPowerShell",
    "Documents/WindowsPowerShell/Microsoft.PowerShell_profile.ps1",
    "Documents/PowerShell/Modules/evil",
    "OneDrive/Documents/PowerShell",
    "OneDrive - Contoso/Documents/WindowsPowerShell",
    "AppData/Local/Microsoft/WindowsApps/python.exe",
    "documents/powershell/profile.ps1",
    ".SSH/config",
    "appdata/roaming/microsoft/windows/start menu/programs/startup/x.lnk",
    ".gitconfig",
    "Documents",
    "",
]


@pytest.mark.parametrize("rel", WSL_HOME_DENIED)
def test_wsl_windows_home_keeps_the_windows_denylist(rel):
    """★ The reviewer's case (R2-M6): Linux target in WSL, ``allow_roots: ["/mnt/c/Users/u"]``. Before the fix the
    Startup folder, the PowerShell profile folders and ``.ssh`` were ``Verdict(True, None)``."""
    cfg = _cfg(Path("/home/u"), ("/mnt/c/Users/u",))
    dest = "/mnt/c/Users/u" + (f"/{rel}" if rel else "")
    assert _dest(dest, cfg) == DENIED, dest


@pytest.mark.parametrize(
    "dest",
    [
        "/mnt/c/Users/bob/.ssh",
        "/mnt/d/Users/bob/.bashrc",
        "/mnt/c/users/Bob/Documents/PowerShell",
        "/c/Users/u/.ssh",
        "/mnt/c/Users",
        "/mnt/c/Windows/System32/drivers/etc/hosts",
        "/mnt/c/windows/system32",
        "/mnt/c/Program Files/app/x.dll",
        "/mnt/c/Program Files (x86)/app",
        "/mnt/c/ProgramData/Microsoft/Windows/Start Menu/Programs/StartUp/x.lnk",
        "/c/Windows",
    ],
)
def test_wsl_other_homes_and_windows_system_folders_are_denied(dest):
    """★ Every Windows home below a drive mount and the Windows system folders, however wide ``allow_roots`` is.
    Before the fix: allowed."""
    cfg = _cfg(Path("/home/u"), ("/",))
    assert _dest(dest, cfg) == DENIED, dest


@pytest.mark.parametrize(
    "dest",
    [
        "/mnt/c/Users/u/projects/app",
        "/mnt/c/Users/u/Documents/proj",
        "/mnt/c/Users/u/OneDrive/Projects/app",
        "/mnt/c/work/app",
        "/mnt/c/dev/bin",
        "/mnt/data/Users/x/.ssh",
        "/mnt/cdrive/Users/x/.ssh",
    ],
)
def test_wsl_rule_keeps_ordinary_destinations(dest):
    """No false alarm: projects in a Windows home, other folders of the drive, mounts that are no drive letter."""
    cfg = _cfg(Path("/home/u"), ("/mnt",))
    assert _dest(dest, cfg) == OK, dest


def test_wsl_home_is_home_like_for_the_source_rule():
    """The source's home rule reads the same list (``policy.homes_of``): a Windows home through WSL is a home."""
    cfg = _cfg(Path("/home/u"), ())
    homes = policy.homes_of(Path("/mnt/c/Users/bob/proj"), cfg, windows=False)
    assert Path("/mnt/c/Users/bob") in homes


# --------------------------------------------------------------------------------------------------------------
# 2. Homes outside the known places: the owners' homes (NSS by uid)
# --------------------------------------------------------------------------------------------------------------


def test_owner_home_outside_the_known_places_is_a_home(tmp_path, monkeypatch):
    """★ ``/srv/users/alice`` of an account the password database does not enumerate (LDAP): the directory's owner is
    asked for its home (``policy._uid_home``, the NSS seam). Before the fix ``.ssh`` there was writable."""
    alice = tmp_path / "srv" / "users" / "alice"
    alice.mkdir(parents=True)
    monkeypatch.setattr(policy, "_account_homes", lambda: ())
    monkeypatch.setattr(
        policy, "_uid_home", lambda uid: alice if uid == os.lstat(alice).st_uid else None, raising=False
    )
    cfg = _cfg(tmp_path / "me", (str(tmp_path / "srv"),))
    for dest in (alice / ".ssh", alice / ".ssh" / "authorized_keys", alice / ".bashrc", alice / "bin" / "x", alice):
        assert _dest(dest, cfg) == DENIED, dest
    assert _dest(alice / "work" / "app", cfg) == OK


@pytest.mark.skipif(os.name != "posix", reason="password database")
def test_uid_home_answers_from_nss():
    import pwd

    own = Path(os.path.normpath(pwd.getpwuid(os.getuid()).pw_dir))
    got = policy._uid_home(os.getuid())
    assert got == (own if len(own.parts) >= 2 else None)
    assert policy._uid_home(2**31 - 7) is None


def test_owner_home_of_slash_is_ignored(tmp_path, monkeypatch):
    """An account whose home is ``/`` would make every path home-relative: never a home."""
    monkeypatch.setattr(policy, "_uid_home", lambda uid: None, raising=False)
    monkeypatch.setattr(policy, "_account_homes", lambda: ())
    cfg = _cfg(tmp_path / "me", (str(tmp_path),))
    assert _dest(tmp_path / "work" / ".venv", cfg) == OK


# --------------------------------------------------------------------------------------------------------------
# 3. Localised OneDrive documents folders
# --------------------------------------------------------------------------------------------------------------

ONEDRIVE_DENIED = [
    "OneDrive/Dokumente/PowerShell",
    "OneDrive/Dokumente/WindowsPowerShell/Microsoft.PowerShell_profile.ps1",
    "OneDrive - Contoso/Documentos/PowerShell",
    "OneDrive/Documenti/WindowsPowerShell",
    "OneDrive/Documenten/PowerShell",
    "OneDrive/Dokumenty/PowerShell",
    "OneDrive/Документы/PowerShell",
    "OneDrive/ドキュメント/PowerShell",
    "onedrive/dokumente/powershell/profile.ps1",
    "OneDrive/Dokumente",
]


@pytest.mark.parametrize("rel", ONEDRIVE_DENIED)
def test_localised_onedrive_documents_hold_the_powershell_profile(rel, tmp_path):
    """★ OneDrive's known-folder move names the documents folder in the display language on disk; the PowerShell
    profile folder there runs at every start. Before the fix: allowed."""
    home = tmp_path / "u"
    cfg = _cfg(home, ("~",), target_os="windows", ci=True)
    assert _dest(home / rel, cfg) == DENIED, rel


def test_localised_onedrive_rule_keeps_other_onedrive_folders(tmp_path):
    """No false alarm: only the listed documents names (never a glob on that level, which would deny every folder of
    OneDrive as an ancestor)."""
    home = tmp_path / "u"
    cfg = _cfg(home, ("~",), target_os="windows", ci=True)
    for rel in ("OneDrive/Projects/app", "OneDrive/Dokumente/proj", "OneDrive/Bilder", "OneDrive/Desktop/x"):
        assert _dest(home / rel, cfg) == OK, rel


def test_onedrive_names_are_listed_with_their_powershell_folders():
    """The constant names every pair (PowerShell 7 and 5.1) for each documents name."""
    for name in policy.ONEDRIVE_DOCUMENTS_NAMES:
        assert f"OneDrive*/{name}/PowerShell" in policy.TARGET_DENY_WINDOWS
        assert f"OneDrive*/{name}/WindowsPowerShell" in policy.TARGET_DENY_WINDOWS
    assert "Dokumente" in policy.ONEDRIVE_DOCUMENTS_NAMES
