"""Environment facts of one side and their delta (layer L4, stdlib only; DESIGN §11.1, §11.2, BX M11, BX P7).

Deterministic and without a shell on every platform: values come from ``platform``, ``socket``, ``getpass``,
``locale``, ``shutil.which``, ``shutil.disk_usage`` and ``os.stat``; tool versions from ``<tool> --version`` run as
an argument list (``shell=False``, ``stdin`` closed, a 3 s timeout, a minimal environment that carries no secret of
the parent) in parallel. Names the transcript or the counterpart supplies (``tools_seen``) are only looked up with
``which`` — never executed (a peer could name any binary). Hermes facts (WSL/container, profile, versions,
configured model) arrive as ``HostFacts`` from ``hostcompat.host_facts`` — this module imports no Hermes code.

All platform calls go through ``SystemOps`` (``default_system()``), the one seam tests use to run the macOS and
Windows branches on any host. The case-sensitivity probe is read-only: it stats a swapped-case spelling of an
existing path component instead of creating files.
"""

from __future__ import annotations

import concurrent.futures
import getpass
import ipaddress
import locale
import os
import platform
import re
import shutil
import signal
import socket
import subprocess
import time
import unicodedata
import urllib.parse
from collections.abc import Callable, Iterable, Mapping
from dataclasses import dataclass
from pathlib import Path

from .errors import BadRequest
from .jsonutil import loads_bounded
from .model import EnvFacts, HostFacts, Options, ToolInfo
from .paths import expand_root

PROBE_TIMEOUT_S = 3.0
PROBE_WORKERS = 16
MAX_TOOLS_SEEN = 64
MAX_VERSION_CHARS = 128

TOOL_ALLOWLIST: tuple[str, ...] = (
    "git", "python3", "python", "node", "npm", "pnpm", "yarn", "bun", "deno", "go", "cargo", "rustc", "java",
    "javac", "mvn", "gradle", "docker", "podman", "kubectl", "helm", "terraform", "make", "cmake", "gcc", "clang",
    "uv", "pip", "gh", "rg", "jq",
)  # fmt: skip
"""Tools probed on every side (DESIGN §11.1); the ``tool_probe`` setting adds more."""

VERSION_ARGS: dict[str, tuple[str, ...]] = {
    "go": ("version",),
    "kubectl": ("version", "--client"),
    "helm": ("version", "--short"),
}
"""Tools whose version flag is not ``--version``."""

LOCAL_PROVIDERS = frozenset({"llamacpp", "llama.cpp", "llama-cpp", "ollama", "lmstudio"})
"""Providers that run on this machine when no ``base_url`` says otherwise (DESIGN §6.7 step 4)."""

SHELL_WORDS = frozenset(
    {
        "cd", "export", "source", ".", "echo", "set", "unset", "alias", "exit", "true", "false", "test", "[", "[[",
        "exec", "eval", "pushd", "popd", "read", "printf", "pwd", "type", "wait", "trap", "ulimit", "umask", "shift",
        "local", "return", "for", "while", "until", "if", "then", "else", "elif", "do", "done", "fi", "case", "esac",
        "function", "{", "}", "!",
    }
)  # fmt: skip
"""Shell built-ins and keywords — never reported as tools."""

PREFIX_WORDS = frozenset({"sudo", "env", "time", "nohup", "nice", "command", "builtin", "xargs", "exec", "doas"})
"""Words that run the next word; the tool is the word after them."""

_TOOL_RE = re.compile(r"^[A-Za-z0-9._+-]{1,64}$")
_ASSIGN_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*=")
_SEPARATORS = re.compile(r"&&|\|\||[;|&\n(){}`]|\$\(")
_CHILD_ENV_POSIX = ("PATH",)
_CHILD_ENV_WINDOWS = ("PATH", "SYSTEMROOT", "PATHEXT", "WINDIR", "TEMP", "TMP", "USERPROFILE")


def _os_release() -> dict[str, str]:
    out: dict[str, str] = {}
    for path in ("/etc/os-release", "/usr/lib/os-release"):
        try:
            text = Path(path).read_text(encoding="utf-8", errors="replace")[:65536]
        except OSError:
            continue
        for line in text.splitlines():
            key, sep, value = line.partition("=")
            if sep and key.strip():
                out[key.strip()] = value.strip().strip('"').strip("'")
        break
    return out


def _run(argv: list[str], timeout: float, env: dict[str, str]) -> tuple[int, str, str] | None:
    """Run ``argv`` without a shell in its own process group; on timeout the whole group is killed (a tool that
    spawns helpers — gradle, a shell wrapper — leaves nothing behind) and None is returned."""
    posix = os.name == "posix"
    flags = 0 if posix else getattr(subprocess, "CREATE_NEW_PROCESS_GROUP", 0)
    try:
        proc = subprocess.Popen(  # noqa: S603 - fixed argv from an allowlist or the operator's tool_probe, no shell
            argv,
            shell=False,
            stdin=subprocess.DEVNULL,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            env=env,
            start_new_session=posix,
            creationflags=flags,
        )
    except (OSError, ValueError):
        return None
    try:
        out, err = proc.communicate(timeout=timeout)
    except subprocess.TimeoutExpired:
        try:
            if posix:
                os.killpg(proc.pid, signal.SIGKILL)
            else:
                proc.kill()
        except OSError:
            pass
        try:
            proc.communicate(timeout=1)
        except (subprocess.SubprocessError, OSError):
            pass
        return None
    return proc.returncode, out.decode("utf-8", "replace")[:65536], err.decode("utf-8", "replace")[:65536]


def _disk_free(path: str) -> int | None:
    try:
        return int(shutil.disk_usage(path).free)
    except (OSError, ValueError):
        return None


def _user() -> str:
    try:
        return getpass.getuser()
    except Exception:  # no passwd entry, no LOGNAME — the fact is then unknown
        return ""


def _locale() -> str:
    try:
        lang, enc = locale.getlocale()
    except (ValueError, TypeError):
        return ""
    return ".".join(p for p in (lang, enc) if p)


def _timezone() -> str:
    try:
        name = time.strftime("%Z")
        offset = time.strftime("%z")
    except (ValueError, OSError):
        return ""
    return f"{name} ({offset})" if name and offset else name or offset


@dataclass(frozen=True)
class SystemOps:
    """Every platform call ``probe`` makes (see module docstring)."""

    system: Callable[[], str]
    release: Callable[[], str]
    version: Callable[[], str]
    machine: Callable[[], str]
    mac_ver: Callable[[], tuple]
    win32_ver: Callable[[], tuple]
    os_release: Callable[[], dict[str, str]]
    node: Callable[[], str]
    user: Callable[[], str]
    home: Callable[[], str]
    environ: Mapping[str, str]
    which: Callable[[str], str | None]
    run: Callable[[list[str], float, dict[str, str]], tuple[int, str, str] | None]
    disk_free: Callable[[str], int | None]
    stat: Callable[[str], os.stat_result]
    locale: Callable[[], str]
    timezone: Callable[[], str]


def default_system() -> SystemOps:
    """``SystemOps`` of this host (read at call time: ``environ`` is a snapshot)."""
    return SystemOps(
        system=platform.system,
        release=platform.release,
        version=platform.version,
        machine=platform.machine,
        mac_ver=platform.mac_ver,
        win32_ver=platform.win32_ver,
        os_release=_os_release,
        node=socket.gethostname,
        user=_user,
        home=lambda: str(Path.home()),
        environ=dict(os.environ),
        which=shutil.which,
        run=_run,
        disk_free=_disk_free,
        stat=os.stat,
        locale=_locale,
        timezone=_timezone,
    )


# ---------------------------------------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------------------------------------


def _one_line(text: object, cap: int = MAX_VERSION_CHARS) -> str:
    out = "".join(ch for ch in str(text or "") if unicodedata.category(ch) not in {"Cc", "Cf", "Cs"})
    return " ".join(out.split())[:cap]


def _family(system: str) -> str:
    name = (system or "").strip().lower()
    if name.startswith(("cygwin", "msys", "mingw")):
        return "windows"
    return name or "unknown"


def _child_env(sys_ops: SystemOps, family: str, home: str) -> dict[str, str]:
    keys = _CHILD_ENV_WINDOWS if family == "windows" else _CHILD_ENV_POSIX
    env = {k: sys_ops.environ[k] for k in keys if isinstance(sys_ops.environ.get(k), str)}
    env.setdefault("PATH", os.defpath)
    if family != "windows":
        env["HOME"] = home
        env["LANG"] = "C"
        env["LC_ALL"] = "C"
    return env


def _version(sys_ops: SystemOps, name: str, path: str, env: dict[str, str]) -> str | None:
    argv = [path, *VERSION_ARGS.get(name, ("--version",))]
    result = sys_ops.run(argv, PROBE_TIMEOUT_S, env)
    if result is None:
        return None
    _rc, out, err = result
    for text in (out, err):
        for line in str(text).splitlines():
            line = _one_line(line)
            if line:
                return line
    return None


def _nearest_existing(path: str, sys_ops: SystemOps) -> str | None:
    current = path
    for _ in range(256):
        try:
            sys_ops.stat(current)
            return current
        except FileNotFoundError:
            parent = os.path.dirname(current)
            if parent == current:
                return None
            current = parent
        except (OSError, ValueError):
            return None
    return None


def case_insensitive_at(path: str, sys_ops: SystemOps, *, default: bool) -> bool:
    """Does the filesystem holding ``path`` compare names case-insensitively? Read-only: the nearest existing
    ancestor's last component that has a case is spelled with swapped case and ``stat``-ed — the same inode means
    insensitive, "not found" means sensitive. Without such a component the OS default is returned."""
    existing = _nearest_existing(path, sys_ops)
    if existing is None:
        return default
    current = existing
    for _ in range(256):
        parent, name = os.path.split(current)
        if not name:
            return default
        swapped = name.swapcase()
        if swapped != name:
            try:
                a = sys_ops.stat(current)
                b = sys_ops.stat(os.path.join(parent, swapped))
            except FileNotFoundError:
                return False
            except (OSError, ValueError):
                return default
            return (a.st_dev, a.st_ino) == (b.st_dev, b.st_ino)
        current = parent
    return default


def _host_url(base_url: str) -> str | None:
    try:
        parsed = urllib.parse.urlsplit(base_url.strip())
    except ValueError:
        return None
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        return None
    return parsed.hostname


def is_offline_route(provider: str | None, base_url: str | None) -> bool:
    """Is the model route local (DESIGN §6.7 step 4, BX P7)? A ``base_url`` decides when present: loopback or a
    private (RFC 1918 / ULA / link-local) address, or ``localhost``. Without one, the provider name decides
    (``llamacpp``, ``ollama``, ``lmstudio``). Tailnet (100.64/10) and public addresses are not offline."""
    if isinstance(base_url, str) and base_url.strip():
        host = _host_url(base_url)
        if host is None:
            return False
        if host.lower() == "localhost":
            return True
        try:
            addr = ipaddress.ip_address(host)
        except ValueError:
            return False
        if addr.is_loopback or addr.is_link_local:
            return True
        if isinstance(addr, ipaddress.IPv4Address):
            return any(addr in ipaddress.ip_network(n) for n in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))
        return addr in ipaddress.ip_network("fc00::/7")
    return isinstance(provider, str) and provider.strip().lower() in LOCAL_PROVIDERS


def main_route(config: dict) -> tuple[str | None, str | None]:
    """``(provider, base_url)`` of the main model in a Hermes config dict (``model.provider``/``model.base_url``)."""
    model = config.get("model") if isinstance(config, dict) else None
    if not isinstance(model, dict):
        return None, None
    provider = model.get("provider") if isinstance(model.get("provider"), str) else None
    base_url = model.get("base_url") if isinstance(model.get("base_url"), str) else None
    return provider, base_url


def _tool_names(names: Iterable[object]) -> list[str]:
    out: list[str] = []
    for n in names:
        if isinstance(n, str) and _TOOL_RE.fullmatch(n) and n not in out:
            out.append(n)
    return out


# ---------------------------------------------------------------------------------------------------------
# Probe
# ---------------------------------------------------------------------------------------------------------


def probe(
    *,
    instance_name: str,
    options: Options,
    host: HostFacts,
    config: dict,
    tools_seen: Iterable[str] = (),
    capabilities: frozenset[str] = frozenset(),
    system: SystemOps | None = None,
) -> EnvFacts:
    """Deterministic facts of this host (DESIGN §11.1). ``tools_seen`` are names to look up (the source's transcript
    tools, or on the target the names the source reported) — never executed. ``system`` (additive) replaces the
    platform calls."""
    s = system or default_system()
    family = _family(s.system())
    home = s.home()
    windows = family == "windows"
    if windows:
        ver = s.win32_ver()
        os_name, os_version = "Windows", _one_line(ver[1] if len(ver) > 1 and ver[1] else s.version(), 64)
        shell_path = s.which("bash")
        shell = "bash" if shell_path else ""
    elif family == "darwin":
        ver = s.mac_ver()
        os_name, os_version = "macOS", _one_line(ver[0] if ver and ver[0] else s.release(), 64)
        shell = os.path.basename(str(s.environ.get("SHELL") or ""))
    else:
        release = s.os_release() if family == "linux" else {}
        os_name = _one_line(release.get("PRETTY_NAME") or platform_name(family), 64)
        os_version = _one_line(s.release(), 64)
        shell = os.path.basename(str(s.environ.get("SHELL") or ""))
    flags = [label for flag, label in ((host.is_wsl, "WSL"), (host.is_container, "container")) if flag]
    if flags:
        os_name = f"{os_name} ({', '.join(flags)})"

    env = _child_env(s, family, home)
    names = _tool_names([*TOOL_ALLOWLIST, *options.tool_probe])
    paths = {n: s.which(n) for n in names}
    tools: dict[str, ToolInfo] = {}
    with concurrent.futures.ThreadPoolExecutor(max_workers=PROBE_WORKERS, thread_name_prefix="ctxh-probe") as pool:
        futures = {n: pool.submit(_version, s, n, p, env) for n, p in paths.items() if p}
        for n in names:
            path = paths[n]
            version = None
            if path is not None:
                try:
                    version = futures[n].result(timeout=PROBE_TIMEOUT_S + 2)
                except Exception:  # a hung or crashing probe leaves the version unknown
                    version = None
            tools[n] = ToolInfo(path=path, version=version)
    seen = {n: s.which(n) for n in _tool_names(tools_seen)[:MAX_TOOLS_SEEN]}

    base = Path(home)
    workspace_root = str(expand_root(options.workspace_root, base))
    allow_roots = tuple(str(expand_root(r, base)) for r in options.allow_roots)
    plugin_dir = str(Path(host.hermes_home) / "plugin-data" / "ctx_handover")
    disk: dict[str, int] = {}
    for p in dict.fromkeys((workspace_root, *allow_roots, plugin_dir)):
        existing = _nearest_existing(p, s)
        free = s.disk_free(existing) if existing else None
        if free is not None:
            disk[p] = free
    default_ci = family in {"darwin", "windows"}
    case = {p: case_insensitive_at(p, s, default=default_ci) for p in dict.fromkeys((workspace_root, *allow_roots))}
    provider, base_url = main_route(config)
    git_info = tools.get("git")
    git_version = None
    if git_info and git_info.version:
        match = re.search(r"(\d+\.\d+(?:\.\d+)?)", git_info.version)
        git_version = match.group(1) if match else None
    return EnvFacts(
        instance_name=_one_line(instance_name, 64),
        os_family=family,
        os_name=os_name,
        os_version=os_version,
        arch=_one_line(s.machine(), 32),
        hostname=_one_line(s.node(), 128),
        user=_one_line(s.user(), 64),
        home=home,
        shell=_one_line(shell, 32),
        host=host,
        tools=tools,
        tools_seen=seen,
        workspace_root=workspace_root,
        allow_roots=allow_roots,
        disk_free=disk,
        case_insensitive=case,
        offline_capable=is_offline_route(provider or host.provider, base_url),
        locale=_one_line(s.locale(), 64),
        timezone=_one_line(s.timezone(), 64),
        git_version=git_version,
        git_lfs=s.which("git-lfs") is not None,
        capabilities=frozenset(capabilities),
    )


def platform_name(family: str) -> str:
    """Display name of an OS family without a release file."""
    return {"linux": "Linux", "darwin": "macOS", "windows": "Windows", "freebsd": "FreeBSD"}.get(family, family or "?")


# ---------------------------------------------------------------------------------------------------------
# Delta
# ---------------------------------------------------------------------------------------------------------


def _major(version: str | None) -> str | None:
    """``"3.12"`` from ``"Python 3.12.3"`` (major.minor), ``"27"`` from ``"Docker version 27.1.0"``."""
    if not version:
        return None
    match = re.search(r"(\d+)(?:\.(\d+))?", version)
    if not match:
        return None
    return match.group(1) + (f".{match.group(2)}" if match.group(2) is not None else "")


def _describe_os(f: EnvFacts) -> str:
    parts = [f.os_name]
    if f.os_version and f.os_version not in f.os_name:
        parts.append(f.os_version)
    parts.append(f.arch)
    return " ".join(p for p in parts if p)


def delta(source: EnvFacts, target: EnvFacts) -> dict:
    """What changes for the continued work (DESIGN §11.2): OS/arch, shell, tools present on the source but missing
    on the target (allowlist and transcript tools), major.minor version changes, model/provider, offline
    capability, terminal backend, Hermes version. Empty when nothing differs; path map and "not transferred" are
    added by the note from plan and decisions."""
    out: dict = {}
    src_os, tgt_os = _describe_os(source), _describe_os(target)
    if src_os != tgt_os:
        out["os"] = {"from": src_os, "to": tgt_os}
    if source.shell != target.shell:
        out["shell"] = {"from": source.shell, "to": target.shell}
    missing = set()
    for name, info in source.tools.items():
        if info.path and not (target.tools.get(name) and target.tools[name].path):
            missing.add(name)
    for name, path in source.tools_seen.items():
        if path and not target.tools_seen.get(name) and not (target.tools.get(name) and target.tools[name].path):
            missing.add(name)
    names = sorted(n for n in missing if _TOOL_RE.fullmatch(n))
    if names:
        out["missing_tools"] = names
    changes = []
    for name in sorted(set(source.tools) & set(target.tools)):
        a, b = _major(source.tools[name].version), _major(target.tools[name].version)
        if a and b and a != b and name != "git":
            changes.append({"tool": name, "from": a, "to": b})
    if changes:
        out["version_changes"] = changes
    src_model = {"model": source.host.model, "provider": source.host.provider}
    tgt_model = {"model": target.host.model, "provider": target.host.provider}
    if src_model != tgt_model:
        out["model"] = {"from": src_model, "to": tgt_model}
    if source.offline_capable != target.offline_capable:
        out["offline"] = {"from": source.offline_capable, "to": target.offline_capable}
    if source.host.terminal_backend != target.host.terminal_backend:
        out["terminal_backend"] = {"from": source.host.terminal_backend, "to": target.host.terminal_backend}
    if source.host.hermes_version != target.host.hermes_version:
        out["hermes"] = {"from": source.host.hermes_version, "to": target.host.hermes_version}
    return out


# ---------------------------------------------------------------------------------------------------------
# Transcript tool names
# ---------------------------------------------------------------------------------------------------------


def _terminal_commands(raw: object) -> list[str]:
    if isinstance(raw, str):
        try:
            raw = loads_bounded(raw.encode("utf-8", "replace"), max_bytes=4 * 1024 * 1024)
        except BadRequest:
            return []
    out = []
    for call in raw if isinstance(raw, list) else []:
        if not isinstance(call, dict):
            continue
        fn = call.get("function") if isinstance(call.get("function"), dict) else {}
        if (fn.get("name") or call.get("name")) != "terminal":
            continue
        args = fn.get("arguments", call.get("arguments"))
        if isinstance(args, str):
            try:
                args = loads_bounded(args.encode("utf-8", "replace"), max_bytes=1024 * 1024)
            except BadRequest:
                continue
        if isinstance(args, dict) and isinstance(args.get("command"), str):
            out.append(args["command"])
    return out


def _first_word(segment: str) -> str | None:
    words = segment.split()
    i = 0
    while i < len(words):
        word = words[i].strip("\"'")
        if _ASSIGN_RE.match(word) or word in PREFIX_WORDS or (word.startswith("-") and i > 0):
            i += 1
            continue
        if word in SHELL_WORDS:
            return None
        return word if _TOOL_RE.fullmatch(word) and "/" not in word else None
    return None


def tools_seen_from_rows(rows: Iterable[dict]) -> set[str]:
    """First words of the transcript's terminal commands (after ``&&``, ``||``, ``;``, ``|``; env assignments and
    ``sudo``/``env``/``time``/``nohup`` skipped; built-ins and paths ignored) — names to look up, never to run."""
    names: set[str] = set()
    for row in rows:
        if not isinstance(row, dict) or row.get("role") != "assistant":
            continue
        for command in _terminal_commands(row.get("tool_calls")):
            for segment in _SEPARATORS.split(command[:65536]):
                word = _first_word(segment)
                if word:
                    names.add(word)
            if len(names) >= MAX_TOOLS_SEEN:
                return names
    return names
