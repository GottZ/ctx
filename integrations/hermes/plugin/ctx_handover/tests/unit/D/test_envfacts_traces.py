"""D: version probes leave no trace in the user's home (R26, DESIGN §11.1).

Every ``<tool> --version`` runs with a throw-away scratch root: ``HOME`` (also the working directory), the XDG base
directories, ``TMPDIR``, the JVM user home and ``GRADLE_USER_HOME`` point into it, and it is removed after the
probe. Tools that create state when they start (rustup, Go telemetry, gradle, npm …) write there, never into the
user's home. Without a usable scratch root nothing is executed.
"""

from __future__ import annotations

import os
from dataclasses import replace
from pathlib import Path

from ctx_handover.engine import envfacts
from ctx_handover.engine.model import Options, ToolInfo
from ctx_handover.tests.fakes.planning_fake import host_facts
from ctx_handover.tests.unit.D.conftest import tree_diff, tree_state, write

XDG_DIRS = {
    "XDG_CONFIG_HOME": ".config",
    "XDG_CACHE_HOME": ".cache",
    "XDG_DATA_HOME": ".local/share",
    "XDG_STATE_HOME": ".local/state",
}
SCRATCH_ENV_POSIX = {"PATH", "HOME", "LANG", "LC_ALL", "TMPDIR", "GRADLE_USER_HOME", "JAVA_TOOL_OPTIONS", *XDG_DIRS}

# A tool that creates state when it starts: a history file in $HOME, a directory in every XDG base directory and a
# lock file in its working directory — then it prints its version.
FAKECLI = """\
mkdir -p "$HOME/.fakecli" && echo run >> "$HOME/.fakecli/history"
for d in "${XDG_CONFIG_HOME:-$HOME/.config}" "${XDG_CACHE_HOME:-$HOME/.cache}" \\
         "${XDG_DATA_HOME:-$HOME/.local/share}" "${XDG_STATE_HOME:-$HOME/.local/state}"; do
  mkdir -p "$d/fakecli" && echo 1 > "$d/fakecli/state"
done
echo 1 > .fakecli.lock
echo "fakecli 4.5.6"
"""


def _bin(tmp_path: Path, scripts: dict[str, str]) -> Path:
    d = tmp_path / "bin"
    d.mkdir()
    for name, body in scripts.items():
        p = d / name
        p.write_text("#!/bin/sh\n" + body + "\n", encoding="utf-8")
        p.chmod(0o755)
    return d


def _host(tmp_path: Path):
    return host_facts(hermes_home=str(tmp_path / "hermes-home"), document_cache=str(tmp_path / "hermes-home/cache"))


def _unchanged(before: dict, after: dict, what: str) -> None:
    diff = tree_diff(before, after)
    assert diff == {"added": [], "removed": [], "changed": []}, f"{what} changed by the version probe: {diff}"


def test_envfacts_probe_leaves_home_untouched(tmp_path, monkeypatch, user_home, scratch_tmp):
    """★ R26: a real probe (this host's ``SystemOps``, the standard tool list on ``PATH`` plus ``tool_probe``) leaves
    the user's home and XDG directories exactly as they were (paths, types, sizes, mtimes), even though ``fakecli``
    writes into ``$HOME``, every XDG directory and its working directory when it starts. The tool still runs (its
    version is reported) and the scratch root is gone afterwards.

    Mutant: ``_child_env`` hands the user's real ``HOME`` to the child."""
    bindir = _bin(tmp_path, {"fakecli": FAKECLI})
    monkeypatch.setenv("PATH", f"{bindir}{os.pathsep}{os.environ.get('PATH', os.defpath)}")
    xdg = tmp_path / "xdg"
    for name in XDG_DIRS:
        (xdg / name.lower()).mkdir(parents=True)
        monkeypatch.setenv(name, str(xdg / name.lower()))
    write(user_home / ".profile", "export EDITOR=vi\n")
    project = user_home / "proj"
    write(project / "main.py", "print('hi')\n")
    monkeypatch.chdir(project)
    home_before, xdg_before = tree_state(user_home), tree_state(xdg)

    facts = envfacts.probe(
        instance_name="laptop", options=Options(tool_probe=("fakecli",)), host=_host(tmp_path), config={}
    )

    assert facts.tools["fakecli"] == ToolInfo(str(bindir / "fakecli"), "fakecli 4.5.6"), "fakecli must run"
    _unchanged(home_before, tree_state(user_home), "home")
    _unchanged(xdg_before, tree_state(xdg), "XDG directories")
    assert os.listdir(scratch_tmp) == [], "the probe's scratch root must be removed"


def test_envfacts_probe_child_env_is_scratch(tmp_path, monkeypatch, user_home, scratch_tmp):
    """R26: all children of one probe share one scratch root under the temp directory: ``HOME`` (= working
    directory), the XDG directories, ``TMPDIR``, ``GRADLE_USER_HOME`` and the JVM ``user.home`` (a JVM reads the
    passwd entry, not ``$HOME``) point into it; nothing of the parent's home or XDG settings is passed on. The
    JVM's "Picked up JAVA_TOOL_OPTIONS" notice is not a version, and a failing ``--version`` (a rustup proxy
    without its toolchain home) reports no version instead of its error text.

    Mutant: ``_version`` reads the first output line regardless of the exit code."""
    bindir = _bin(
        tmp_path,
        {
            "pwdcli": "pwd",
            "failcli": 'echo "error: rustup could not choose a version of failcli to run" >&2; exit 1',
            "jvmcli": 'echo "Picked up JAVA_TOOL_OPTIONS: $JAVA_TOOL_OPTIONS" >&2; echo "jvmcli 21.0.4" >&2',
        },
    )
    monkeypatch.setenv("PATH", str(bindir))
    real = envfacts.default_system()
    recorded: list[tuple[str, dict]] = []

    def run(argv, timeout, env):
        recorded.append((Path(argv[0]).name, dict(env)))
        return real.run(argv, timeout, env)

    facts = envfacts.probe(
        instance_name="laptop",
        options=Options(tool_probe=("pwdcli", "failcli", "jvmcli")),
        host=_host(tmp_path),
        config={},
        system=replace(real, run=run),
    )

    assert sorted(name for name, _env in recorded) == ["failcli", "jvmcli", "pwdcli"]
    home = recorded[0][1]["HOME"]
    root = Path(home).parent
    assert root.parent == scratch_tmp and root.name.startswith("ctxh-probe-")
    for _name, env in recorded:
        assert set(env) <= SCRATCH_ENV_POSIX
        assert env["HOME"] == home
        assert {k: env[k] for k in XDG_DIRS} == {k: str(Path(home) / rel) for k, rel in XDG_DIRS.items()}
        assert Path(env["TMPDIR"]).parent == root
        assert env.get("GRADLE_USER_HOME") == str(Path(home) / ".gradle")
        assert env.get("JAVA_TOOL_OPTIONS") == f"-Duser.home={home}"
        assert not [v for v in env.values() if str(user_home) in v], "the user's home must not reach the child"
    assert facts.tools["pwdcli"].version == os.path.realpath(home)[: envfacts.MAX_VERSION_CHARS]
    assert facts.tools["failcli"] == ToolInfo(str(bindir / "failcli"), None)
    assert facts.tools["jvmcli"].version == "jvmcli 21.0.4"
    assert os.listdir(scratch_tmp) == []


def _fake_system(**over) -> tuple[envfacts.SystemOps, list]:
    calls: list = []

    def run(argv, timeout, env):
        calls.append((argv, timeout, dict(env)))
        return (0, f"{Path(argv[0]).name} 9.9.9\n", "")

    base = replace(
        envfacts.default_system(),
        system=lambda: "Linux",
        os_release=lambda: {"PRETTY_NAME": "Debian GNU/Linux 13"},
        which=lambda name: f"/opt/bin/{name}" if name in {"git", "bash", "node"} else None,
        run=run,
        environ={"SHELL": "/bin/bash", "PATH": "/opt/bin"},
        node=lambda: "box",
        user=lambda: "u",
        home=lambda: "/home/u",
        disk_free=lambda p: 1,
        stat=lambda p: os.stat("/"),
    )
    return replace(base, **over), calls


def test_envfacts_probe_scratch_windows_and_unavailable(tmp_path, scratch_tmp):
    """R26 on the Windows branch (simulated ``SystemOps``): ``USERPROFILE``, ``HOME``, ``APPDATA``,
    ``LOCALAPPDATA``, ``TEMP`` and ``TMP`` point into the scratch root, the parent's values are not passed on.
    Without a usable scratch root nothing is executed: tools are reported by path with an unknown version.

    Mutant: without a scratch root the probe runs the tools with the user's home."""
    parent = {
        "PATH": "C:\\bin",
        "SYSTEMROOT": "C:\\Windows",
        "PATHEXT": ".EXE",
        "USERPROFILE": "C:\\Users\\u",
        "APPDATA": "C:\\Users\\u\\AppData\\Roaming",
        "LOCALAPPDATA": "C:\\Users\\u\\AppData\\Local",
        "TEMP": "C:\\Users\\u\\AppData\\Local\\Temp",
        "TMP": "C:\\Users\\u\\AppData\\Local\\Temp",
    }
    win, calls = _fake_system(
        system=lambda: "Windows",
        win32_ver=lambda: ("11", "10.0.26100", "SP0", "Multiprocessor Free"),
        environ=parent,
        home=lambda: "C:\\Users\\u",
    )
    facts = envfacts.probe(instance_name="pc", options=Options(), host=_host(tmp_path), config={}, system=win)
    assert facts.tools["git"].version == "git 9.9.9" and calls
    for _argv, _timeout, env in calls:
        home = env["USERPROFILE"]
        root = os.path.dirname(home)
        assert os.path.dirname(root) == str(scratch_tmp)
        assert env["HOME"] == home and env["SYSTEMROOT"] == "C:\\Windows"
        assert env["APPDATA"].startswith(home + os.sep) and env["LOCALAPPDATA"].startswith(home + os.sep)
        assert os.path.dirname(env["TEMP"]) == root and env["TMP"] == env["TEMP"]
        assert not [v for v in env.values() if "C:\\Users\\u" in v]
    assert os.listdir(scratch_tmp) == []

    blocker = write(tmp_path / "not-a-directory", "x")
    linux, calls = _fake_system()
    facts = envfacts.probe(
        instance_name="srv", options=Options(), host=_host(tmp_path), config={}, system=linux, scratch_dir=blocker
    )
    assert calls == [], "no tool may run without a scratch root"
    assert facts.tools["git"] == ToolInfo("/opt/bin/git", None)
    assert facts.tools["node"] == ToolInfo("/opt/bin/node", None)
    assert facts.tools["cargo"] == ToolInfo(None, None)
