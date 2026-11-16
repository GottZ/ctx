"""What Hermes does with a ctx_checkpoint folder its account cannot read (GottZ/ctx#290).

Seen on a Windows install (2026-11-15): the folder came from a shell with administrator
rights, Hermes Desktop could not read it, skipped the plugin, and with
``compression.checkpoint_required`` every compaction failed with a message that does not
name the folder. The plugin's code never runs then, so what it can contribute is a known
host behaviour (pinned here through Hermes' own loader, ``plugins/memory/__init__.py``), an
installer that leaves a readable copy (``integrations/hermes/scripts/install.sh``) and the
Troubleshooting section of the README.

The unreadable cases need permissions that bind this process: a test run with
``CAP_DAC_OVERRIDE`` (root) reads a mode-000 file anyway and skips them.
"""

from __future__ import annotations

import logging
import os
import shutil
import stat
import subprocess
from pathlib import Path

import pytest

PLUGIN_SRC = Path(__file__).resolve().parents[1]
INSTALL_SH = PLUGIN_SRC.parents[1] / "scripts" / "install.sh"
NAME = "ctx_checkpoint"


def _permissions_bind(tmp_path: Path) -> bool:
    probe = tmp_path / "permission-probe"
    probe.write_text("x", encoding="utf-8")
    probe.chmod(0)
    try:
        probe.read_text(encoding="utf-8")
    except PermissionError:
        return True
    finally:
        probe.chmod(stat.S_IRUSR | stat.S_IWUSR)
    return False


@pytest.fixture
def installed(_isolated_hermes_home):
    dest = _isolated_hermes_home / "plugins" / NAME
    shutil.copytree(PLUGIN_SRC, dest, ignore=shutil.ignore_patterns("__pycache__", ".ruff_cache"))
    yield dest
    dest.chmod(stat.S_IRWXU)  # let tmp_path cleanup remove it again
    for path in dest.rglob("*"):
        path.chmod(stat.S_IRWXU)


@pytest.fixture
def binding_permissions(tmp_path):
    if not _permissions_bind(tmp_path):
        pytest.skip("file permissions do not bind this process (root or CAP_DAC_OVERRIDE)")


def test_hermes_finds_a_readable_install(installed):
    from plugins.memory import find_provider_dir

    assert find_provider_dir(NAME) == installed


def test_an_unreadable_module_skips_the_plugin_with_a_warning_naming_the_folder(installed, binding_permissions, caplog):
    from plugins.memory import find_provider_dir, load_memory_provider

    (installed / "__init__.py").chmod(0)
    with caplog.at_level(logging.DEBUG, logger="plugins.memory"):
        assert find_provider_dir(NAME) is None
        assert load_memory_provider(NAME, register_skills=False) is None
    warnings = [r.getMessage() for r in caplog.records if r.levelno == logging.WARNING]
    assert any("Skipping unreadable plugin directory" in m and str(installed) in m for m in warnings), warnings


def test_an_untraversable_folder_skips_the_plugin(installed, binding_permissions, caplog):
    """Mode 000 on the folder itself: Hermes does not find the plugin. Whether it says so at
    WARNING depends on Python: ``Path.exists`` raises the permission error up to 3.13 (Hermes
    logs "Skipping unreadable plugin directory") and reports "missing" from 3.14 on (no line;
    only the later "is not installed" and "Compress will fail closed" notices remain)."""
    import sys

    from plugins.memory import find_provider_dir

    installed.chmod(0)
    with caplog.at_level(logging.DEBUG, logger="plugins.memory"):
        assert find_provider_dir(NAME) is None
    warnings = [r.getMessage() for r in caplog.records if r.levelno >= logging.WARNING]
    if sys.version_info >= (3, 14):
        assert warnings == []
    else:
        assert any(str(installed) in m for m in warnings), warnings


@pytest.mark.parametrize("source", ["checkout", "private_checkout"])
def test_install_sh_leaves_a_copy_every_account_can_read(_isolated_hermes_home, tmp_path, source):
    """install.sh run with a restrictive umask (an elevated or sudo shell often has 077), from
    this checkout and from one made under such a umask (every file readable by its owner only)."""
    home = _isolated_hermes_home
    script = INSTALL_SH
    if source == "private_checkout":
        tree = tmp_path / "private-checkout"
        shutil.copytree(PLUGIN_SRC, tree / "plugin" / NAME, ignore=shutil.ignore_patterns("__pycache__"))
        (tree / "scripts").mkdir()
        script = Path(shutil.copy2(INSTALL_SH, tree / "scripts" / "install.sh"))
        for path in (tree / "plugin" / NAME).rglob("*"):
            path.chmod(stat.S_IRWXU if path.is_dir() else stat.S_IRUSR | stat.S_IWUSR)
    env = {k: v for k, v in os.environ.items() if k != "HERMES_HOME"}
    bash = shutil.which("bash")
    assert bash, "install.sh needs bash"
    result = subprocess.run(  # noqa: S603 - fixed argv: the repo's install.sh and a temporary home
        [bash, "-c", 'umask 077 && exec bash "$0" --hermes-home "$1"', str(script), str(home)],
        capture_output=True,
        text=True,
        env=env,
        timeout=60,
        check=False,
    )
    assert result.returncode == 0, result.stderr
    dest = home / "plugins" / NAME
    assert (dest / "__init__.py").is_file() and not dest.is_symlink()
    # plugins/ did not exist before: the installer made it, and the account must reach the copy.
    unreadable = [
        str(path.relative_to(home))
        for path in [dest.parent, dest, *dest.rglob("*")]
        if not path.stat().st_mode & stat.S_IROTH or (path.is_dir() and not path.stat().st_mode & stat.S_IXOTH)
    ]
    assert unreadable == [], f"not readable for other accounts: {unreadable[:5]}"


def _run_install(home: Path, *, env_extra: dict[str, str] | None = None, path_prefix: str | None = None):
    env = {k: v for k, v in os.environ.items() if k != "HERMES_HOME"}
    env.update(env_extra or {})
    if path_prefix:
        env["PATH"] = f"{path_prefix}{os.pathsep}{env.get('PATH', '')}"
    bash = shutil.which("bash")
    assert bash, "install.sh needs bash"
    return subprocess.run(  # noqa: S603 - fixed argv: the repo's install.sh and a temporary home
        [bash, str(INSTALL_SH), "--hermes-home", str(home)],
        capture_output=True,
        text=True,
        env=env,
        timeout=60,
        check=False,
    )


def _running_install(home: Path) -> Path:
    """An installed plugin of an earlier version, as the next upgrade finds it."""
    dest = home / "plugins" / NAME
    dest.mkdir(parents=True)
    (dest / "VERSION").write_text("0.0.1-running\n", encoding="utf-8")
    (dest / "__init__.py").write_text("# the running install\n", encoding="utf-8")
    return dest


def _assert_running_install_kept(home: Path, dest: Path) -> None:
    assert (dest / "VERSION").read_text(encoding="utf-8") == "0.0.1-running\n", "the running install is gone"
    assert not list((home / "plugins").glob(f".{NAME}.bak-*")), "a backup stayed behind next to the install"


def test_install_sh_keeps_the_running_install_when_staging_fails(_isolated_hermes_home, tmp_path):
    """No staging directory (TMPDIR names a file: mktemp fails for root as well) — the upgrade
    stops before it touches the running install."""
    home = _isolated_hermes_home
    dest = _running_install(home)
    not_a_dir = tmp_path / "tmpdir-is-a-file"
    not_a_dir.write_text("x", encoding="utf-8")
    result = _run_install(home, env_extra={"TMPDIR": str(not_a_dir)})
    assert result.returncode != 0, result.stdout
    _assert_running_install_kept(home, dest)


def test_install_sh_restores_the_running_install_when_the_move_fails(_isolated_hermes_home, tmp_path):
    """The staged copy cannot be moved into place (a mv that refuses the staged tree) — the
    running install is moved back, never left in the backup."""
    home = _isolated_hermes_home
    dest = _running_install(home)
    stage_root = tmp_path / "stage-root"
    stage_root.mkdir()
    real_mv = shutil.which("mv")
    assert real_mv
    shim = tmp_path / "shim"
    shim.mkdir()
    (shim / "mv").write_text(
        "#!/bin/sh\n"
        f'for a in "$@"; do case "$a" in "{stage_root}"/*) echo "mv: refused by the test" >&2; exit 1;; esac; done\n'
        f'exec "{real_mv}" "$@"\n',
        encoding="utf-8",
    )
    (shim / "mv").chmod(stat.S_IRWXU)
    result = _run_install(home, env_extra={"TMPDIR": str(stage_root)}, path_prefix=str(shim))
    assert result.returncode != 0, result.stdout
    _assert_running_install_kept(home, dest)
