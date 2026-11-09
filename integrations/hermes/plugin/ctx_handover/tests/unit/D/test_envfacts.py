"""D: environment facts per platform, without a shell (DESIGN §11.1, §11.2, BX M11, BX P7).

Linux runs against this host with real subprocesses (fake tools in a temp ``PATH``); macOS and Windows run the same
code through an injected ``SystemOps`` — the only platform seam — so their branches are exercised on Linux too.
"""

from __future__ import annotations

import os
import subprocess
import sys
import time
from dataclasses import replace
from pathlib import Path

import pytest

from ctx_handover.engine import envfacts
from ctx_handover.engine.model import Options, ToolInfo
from ctx_handover.tests.fakes.planning_fake import env_facts, host_facts

ALLOWED_CHILD_ENV = {
    "PATH", "HOME", "LANG", "LC_ALL", "SYSTEMROOT", "PATHEXT", "WINDIR", "TEMP", "TMP", "USERPROFILE", "APPDATA",
    "LOCALAPPDATA", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME", "XDG_STATE_HOME",
    "GRADLE_USER_HOME", "JAVA_TOOL_OPTIONS",
}  # fmt: skip
"""Fixed names only: ``PATH`` (and the Windows system variables) from the parent, every home, config, cache and temp
location pointing into the probe's scratch root (R26, ``test_envfacts_traces.py``)."""


def _bin(tmp_path: Path, scripts: dict[str, str]) -> Path:
    d = tmp_path / "bin"
    d.mkdir()
    for name, body in scripts.items():
        p = d / name
        p.write_text("#!/bin/sh\n" + body + "\n", encoding="utf-8")
        p.chmod(0o755)
    return d


def _host(tmp_path: Path, **over):
    return host_facts(
        hermes_home=str(tmp_path / "hermes-home"), document_cache=str(tmp_path / "hermes-home/cache/documents"), **over
    )


def test_envfacts_no_shell_and_timeouts(tmp_path, monkeypatch, user_home):
    """Tool versions come from ``<tool> --version`` with ``shell=False``, a 3 s timeout and a minimal environment;
    transcript tool names are only looked up (``which``), never executed."""
    marker = tmp_path / "executed"
    bindir = _bin(
        tmp_path,
        {
            "slowtool": "sleep 20",
            "fasttool": 'echo "fasttool 1.2.3 ${CANARY_SECRET:-scrubbed}"',
            "seentool": f"touch {marker}",
        },
    )
    monkeypatch.setenv("PATH", f"{bindir}:/usr/bin:/bin")
    monkeypatch.setenv("CANARY_SECRET", "leaked")
    start = time.monotonic()
    facts = envfacts.probe(
        instance_name="server",
        options=Options(tool_probe=("slowtool", "fasttool", "bad name")),
        host=_host(tmp_path),
        config={},
        tools_seen=["seentool", "ghosttool", "rm;reboot"],
    )
    assert time.monotonic() - start < 10
    assert facts.tools["slowtool"] == ToolInfo(str(bindir / "slowtool"), None)
    assert facts.tools["fasttool"].version == "fasttool 1.2.3 scrubbed"
    assert "bad name" not in facts.tools
    assert facts.tools_seen == {"seentool": str(bindir / "seentool"), "ghosttool": None}
    assert not marker.exists(), "a transcript tool name must never be executed"

    calls, timeouts = [], []
    real_popen = subprocess.Popen

    class Recorder(real_popen):
        def __init__(self, *args, **kwargs):
            calls.append((args, kwargs))
            super().__init__(*args, **kwargs)

        def communicate(self, input=None, timeout=None):
            timeouts.append(timeout)
            return super().communicate(input, timeout)

    monkeypatch.setattr(envfacts.subprocess, "Popen", Recorder)
    envfacts.probe(instance_name="server", options=Options(tool_probe=("fasttool",)), host=_host(tmp_path), config={})
    assert calls and timeouts
    for args, kwargs in calls:
        assert isinstance(args[0], list) and all(isinstance(a, str) for a in args[0])
        assert kwargs.get("shell", False) is False
        assert set(kwargs["env"]) <= ALLOWED_CHILD_ENV
        assert kwargs.get("stdin") is subprocess.DEVNULL
        assert kwargs.get("start_new_session") is True
    assert all(t is not None and 0 < t <= envfacts.PROBE_TIMEOUT_S <= 3 for t in timeouts[: len(calls)])


def test_envfacts_host_facts_as_parameters(tmp_path, monkeypatch, user_home):
    """BX M11: ``envfacts`` never imports Hermes; Hermes facts arrive as ``HostFacts``."""
    for name in ("hermes_constants", "hermes_cli", "hermes_cli.config", "agent", "agent.secret_scope", "hermes_state"):
        monkeypatch.setitem(sys.modules, name, None)
    host = _host(tmp_path, is_wsl=True, model="qwen3", provider="llamacpp")
    facts = envfacts.probe(
        instance_name="laptop", options=Options(), host=host, config={}, capabilities=frozenset({"x"})
    )
    assert facts.host is host
    assert facts.capabilities == frozenset({"x"})
    assert facts.os_family == "linux" and "(WSL)" in facts.os_name
    assert facts.offline_capable is True
    assert facts.home == str(user_home)
    assert facts.workspace_root == str(user_home / "hermes-handover")
    assert facts.allow_roots == (str(user_home / "hermes-handover"),)
    assert set(facts.disk_free) >= {str(user_home / "hermes-handover")}
    assert facts.case_insensitive == {str(user_home / "hermes-handover"): False}


def test_envfacts_case_probe_is_read_only(tmp_path, user_home, monkeypatch):
    """The case probe only stats existing paths (a swapped-case spelling of one component): nothing is created.
    (``PATH`` is empty here; real tools run in a scratch home, R26 — ``test_envfacts_traces.py``.)"""
    empty = tmp_path / "empty-bin"
    empty.mkdir()
    monkeypatch.setenv("PATH", str(empty))
    before = sorted(os.listdir(user_home))
    facts = envfacts.probe(instance_name="x", options=Options(), host=_host(tmp_path), config={})
    assert sorted(os.listdir(user_home)) == before
    assert facts.case_insensitive[str(user_home / "hermes-handover")] is False
    mixed = tmp_path / "Mixed"
    mixed.mkdir()
    real = envfacts.default_system()
    folding = replace(real, stat=lambda p: os.stat(str(p).replace("mIXED", "Mixed")))
    assert envfacts.case_insensitive_at(str(mixed / "sub"), folding, default=False) is True
    assert envfacts.case_insensitive_at(str(mixed / "sub"), real, default=False) is False


def _sys(**over) -> tuple[envfacts.SystemOps, list]:
    calls = []

    def run(argv, timeout, env):
        calls.append((argv, timeout, env))
        return (0, f"{Path(argv[0]).name} 9.9.9\n", "")

    base = replace(
        envfacts.default_system(),
        which=lambda name: f"/opt/bin/{name}" if name in {"git", "bash", "node"} else None,
        run=run,
        environ={"SHELL": "/bin/zsh", "PATH": "/opt/bin"},
        node=lambda: "box",
        user=lambda: "u",
        home=lambda: "/Users/u",
        disk_free=lambda p: 1,
        stat=lambda p: os.stat("/"),
    )
    return replace(base, **over), calls


def test_envfacts_platforms_without_shell(tmp_path):
    mac, calls = _sys(
        system=lambda: "Darwin",
        release=lambda: "24.1.0",
        mac_ver=lambda: ("15.1", ("", "", ""), "arm64"),
        machine=lambda: "arm64",
    )
    facts = envfacts.probe(instance_name="laptop", options=Options(), host=_host(tmp_path), config={}, system=mac)
    assert (facts.os_family, facts.os_name, facts.os_version, facts.arch, facts.shell) == (
        "darwin",
        "macOS",
        "15.1",
        "arm64",
        "zsh",
    )
    assert facts.tools["git"].version == "git 9.9.9" and facts.git_version == "9.9.9"
    assert all(isinstance(argv, list) for argv, _t, _e in calls)

    win, calls = _sys(
        system=lambda: "Windows",
        release=lambda: "11",
        version=lambda: "10.0.26100",
        win32_ver=lambda: ("11", "10.0.26100", "SP0", "Multiprocessor Free"),
        machine=lambda: "AMD64",
        environ={"PATH": "C:\\bin", "SYSTEMROOT": "C:\\Windows", "PATHEXT": ".EXE", "SECRET_TOKEN": "x"},
        home=lambda: "C:\\Users\\u",
    )
    facts = envfacts.probe(instance_name="pc", options=Options(), host=_host(tmp_path), config={}, system=win)
    assert (facts.os_family, facts.os_name, facts.os_version, facts.shell) == (
        "windows",
        "Windows",
        "10.0.26100",
        "bash",
    )
    for _argv, _timeout, env in calls:
        assert "SECRET_TOKEN" not in env and env.get("SYSTEMROOT") == "C:\\Windows"

    linux, _ = _sys(
        system=lambda: "Linux",
        release=lambda: "6.12.1",
        os_release=lambda: {"PRETTY_NAME": "Arch Linux"},
        machine=lambda: "x86_64",
    )
    facts = envfacts.probe(
        instance_name="srv", options=Options(), host=_host(tmp_path, is_container=True), config={}, system=linux
    )
    assert facts.os_name == "Arch Linux (container)" and facts.os_version == "6.12.1"


@pytest.mark.parametrize(
    ("provider", "base_url", "expected"),
    [
        ("llamacpp", None, True),
        ("llama.cpp", "", True),
        ("ollama", None, True),
        ("lmstudio", "", True),
        ("openrouter", None, False),
        ("custom", "http://127.0.0.1:8080/v1", True),
        ("custom", "http://localhost:1234", True),
        ("custom", "http://[::1]:8000/v1", True),
        ("custom", "http://192.168.1.5:11434", True),
        ("custom", "http://10.0.0.2", True),
        ("custom", "http://172.16.0.1", True),
        ("custom", "http://172.32.0.1", False),
        ("custom", "http://100.64.1.2", False),
        ("ollama", "https://ollama.com/v1", False),
        ("custom", "https://api.example.com/v1", False),
        ("custom", "not a url", False),
        (None, None, False),
    ],
)
def test_envfacts_offline_route(provider, base_url, expected):
    assert envfacts.is_offline_route(provider, base_url) is expected


def test_envfacts_offline_from_config(tmp_path, user_home):
    host = _host(tmp_path, provider="custom", model="m")
    config = {"model": {"default": "m", "provider": "custom", "base_url": "http://127.0.0.1:8080/v1"}}
    assert envfacts.probe(instance_name="x", options=Options(), host=host, config=config).offline_capable is True
    config = {"model": {"default": "m", "provider": "custom", "base_url": "https://api.example.com"}}
    assert envfacts.probe(instance_name="x", options=Options(), host=host, config=config).offline_capable is False


def test_envfacts_delta():
    src = env_facts(  # noqa: S604 - EnvFacts.shell is a shell name, nothing runs
        "server",
        os_name="Debian 13",
        shell="bash",
        tools={
            "git": ToolInfo("/usr/bin/git", "git version 2.47.0"),
            "docker": ToolInfo("/usr/bin/docker", "Docker version 27.1.0"),
            "kubectl": ToolInfo("/usr/bin/kubectl", "v1.31"),
            "python3": ToolInfo("/usr/bin/python3", "Python 3.12.3"),
        },
        tools_seen={"make": "/usr/bin/make", "cd": None},
        host={"model": "gpt-x", "provider": "openrouter", "terminal_backend": "local"},
        offline_capable=False,
    )
    tgt = env_facts(  # noqa: S604 - EnvFacts.shell is a shell name, nothing runs
        "laptop",
        os_family="darwin",
        os_name="macOS",
        os_version="15.1",
        arch="arm64",
        shell="zsh",
        tools={"git": ToolInfo("/usr/bin/git", "git version 2.39.5"), "python3": ToolInfo("/opt/py", "Python 3.14.0")},
        tools_seen={"make": None, "cd": None},
        host={"model": "qwen3", "provider": "llamacpp", "terminal_backend": "docker", "hermes_version": "0.23.0"},
        offline_capable=True,
    )
    d = envfacts.delta(src, tgt)
    assert d["missing_tools"] == ["docker", "kubectl", "make"]
    assert d["version_changes"] == [{"tool": "python3", "from": "3.12", "to": "3.14"}]
    assert d["os"] == {"from": "Debian 13 6.12.0 x86_64", "to": "macOS 15.1 arm64"}
    assert d["shell"] == {"from": "bash", "to": "zsh"}
    assert d["model"] == {
        "from": {"model": "gpt-x", "provider": "openrouter"},
        "to": {"model": "qwen3", "provider": "llamacpp"},
    }
    assert d["offline"] == {"from": False, "to": True}
    assert d["terminal_backend"] == {"from": "local", "to": "docker"}
    assert d["hermes"] == {"from": "0.22.6", "to": "0.23.0"}
    assert envfacts.delta(src, src) == {}


def test_tools_seen_from_rows():
    def call(cmd):
        return {
            "role": "assistant",
            "tool_calls": [
                {"id": "1", "function": {"name": "terminal", "arguments": f'{{"command": {cmd!r}}}'.replace("'", '"')}}
            ],
        }

    rows = [
        call("cd app && npm run dev | tee log; FOO=1 make build"),
        call("sudo docker ps"),
        call("./run.sh --x"),
        call("env A=1 time nohup cargo test"),
        call("$(evil) ; `rm` ; rm -rf build"),
        {
            "role": "assistant",
            "tool_calls": '[{"id": "2", "function": {"name": "terminal", "arguments": "{\\"command\\": \\"jq .\\"}"}}]',
        },
        {"role": "user", "content": "kubectl apply"},
    ]
    assert envfacts.tools_seen_from_rows(rows) == {"npm", "tee", "make", "docker", "cargo", "evil", "rm", "jq"}
