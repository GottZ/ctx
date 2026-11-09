"""Test doubles for cluster D (planning and environment; INTERFACES §4.2, §6).

Other clusters use these to drive planning without a real host:

``env_facts(...)`` / ``host_facts(...)``   complete ``EnvFacts``/``HostFacts`` with overridable fields
``plan_item(...)`` / ``resource_plan(...)``  plan documents with sensible defaults
``final_plan(plan, decisions)``             a ``FinalPlan`` whose ``plan_id`` is computed by ``plan.plan_id``
``decision(item, dest)``                    a ``TargetDecision``
``model_change(...)``                       ``FinalPlan.model_change`` in the one form of contract X04
``Busy``                                    a ``contracts.BusyReportLike``
``StaticGitProbe``                          ``contracts.GitProbe`` from a fixed ``{work tree root: shallow}`` map
                                            (``partial=`` names partial clones, ``is_partial`` as E's probe)
``CeilingGitProbe``                         ``contracts.GitProbe`` over real ``git rev-parse`` with a ceiling dir,
                                            so a probe below the test's tmp dir never finds an enclosing repo;
                                            read-only, no identity, no config writes
``HermesFileStub`` / ``static_discover``    stand-ins for ``hermes_files.HermesFile``/``discover`` (cluster B),
                                            including its ``rejected=`` list
``ScriptedModel``                           a side question or ``llm`` callable with scripted replies; records
                                            every prompt/input it saw
``InjectedModel``                           an ``llm`` that obeys instructions planted in the transcript (``ADD:``
                                            lines) — the prompt-injection adversary of the refinement tests
``fake_redact``                             the core's ``redact_sensitive_text(force=True)`` when importable

Nothing here touches the real home, the real Hermes profile or a git identity.
"""

from __future__ import annotations

import json
import os
import subprocess
from collections.abc import Callable, Iterable, Sequence
from dataclasses import dataclass, field, replace
from pathlib import Path

from ctx_handover.engine.model import (
    DestMode,
    EnvFacts,
    FinalPlan,
    HostFacts,
    Include,
    ItemKind,
    ItemRole,
    NotTransferred,
    Origin,
    PlanItem,
    RefineReport,
    ResourcePlan,
    TargetDecision,
    ToolInfo,
)


def host_facts(**overrides) -> HostFacts:
    """``HostFacts`` of a plain Linux gateway; any field can be overridden."""
    base = {
        "is_wsl": False,
        "is_container": False,
        "profile": "default",
        "multiplex": False,
        "hermes_version": "0.22.6",
        "plugin_version": "0.1.0",
        "model": "test-model",
        "provider": "test-provider",
        "context_length": 131072,
        "terminal_backend": "local",
        "hermes_home": "/home/u/.hermes",
        "document_cache": "/home/u/.hermes/cache/documents",
    }
    base.update(overrides)
    return HostFacts(**base)


def env_facts(name: str = "server", **overrides) -> EnvFacts:
    """``EnvFacts`` of a Linux host named ``name``; ``host`` may be a dict of ``HostFacts`` overrides."""
    host = overrides.pop("host", None)
    if isinstance(host, dict) or host is None:
        host = host_facts(**(host or {}))
    home = overrides.get("home", "/home/u")
    base = {
        "instance_name": name,
        "os_family": "linux",
        "os_name": "Debian GNU/Linux 13",
        "os_version": "6.12.0",
        "arch": "x86_64",
        "hostname": name,
        "user": "u",
        "home": home,
        "shell": "bash",
        "host": host,
        "tools": {
            "git": ToolInfo("/usr/bin/git", "git version 2.47.0"),
            "python3": ToolInfo("/usr/bin/python3", "3.12.3"),
        },
        "tools_seen": {},
        "workspace_root": f"{home}/hermes-handover",
        "allow_roots": (f"{home}/hermes-handover",),
        "disk_free": {f"{home}/hermes-handover": 100 * 1024**3},
        "case_insensitive": {f"{home}/hermes-handover": False},
        "offline_capable": False,
        "locale": "C.UTF-8",
        "timezone": "UTC",
        "git_version": "2.47.0",
        "git_lfs": False,
    }
    base.update(overrides)
    return EnvFacts(**base)


def plan_item(item_id: str = "w0", source_path: str = "/home/u/app", **overrides) -> PlanItem:
    """A primary file-tree item by default."""
    base = {
        "item_id": item_id,
        "kind": ItemKind.FILE_TREE,
        "source_path": source_path,
        "role": ItemRole.PRIMARY,
        "origin": Origin.CWD,
        "include": Include.YES,
        "reasons": ("cwd",),
        "size_estimate": 1024,
    }
    base.update(overrides)
    return PlanItem(**base)


def resource_plan(items: Sequence[PlanItem] = (), **overrides) -> ResourcePlan:
    base = {
        "job_id": "01JCTESTJOB0000000000000000",
        "revision": (1, 2),
        "items": tuple(items) or (plan_item(),),
        "not_transferred": (),
        "processes": (),
        "delegations": (),
        "cron": (),
        "refine": RefineReport(used="off"),
        "tools_needed": (),
        "env_vars_needed": (),
        "notes_for_target": "",
        "approx_tokens": 1000,
    }
    base.update(overrides)
    return ResourcePlan(**base)


def decision(item: PlanItem, dest: str, **overrides) -> TargetDecision:
    base = {"item_id": item.item_id, "dest": dest, "mode": DestMode.FRESH, "reason": "workspace-root"}
    base.update(overrides)
    return TargetDecision(**base)


def final_plan(plan: ResourcePlan, decisions: Sequence[TargetDecision] | None = None, **overrides) -> FinalPlan:
    """A ``FinalPlan`` with a correct ``plan_id`` (computed by the real ``plan.plan_id``)."""
    from ctx_handover.engine import plan as plan_mod

    if decisions is None:
        decisions = tuple(decision(i, f"/home/u/hermes-handover/{Path(i.source_path).name}") for i in plan.items)
    base = {
        "plan_id": "",
        "plan": plan,
        "decisions": tuple(decisions),
        "need_bytes": sum(d.need_bytes for d in decisions),
        "free_bytes": {"/home/u/hermes-handover": 100 * 1024**3},
        "model_change": None,
    }
    base.update(overrides)
    return plan_mod.with_plan_id(FinalPlan(**base))


def model_change(
    *,
    from_model: str = "qwen3:32b",
    from_provider: str = "ollama",
    to_model: str = "gpt-x",
    to_provider: str = "openrouter",
    context_length: int | None = 131072,
    offline_capable: bool = False,
    reason: str = "provider_unusable",
) -> dict:
    """``FinalPlan.model_change`` in the one form of contract X04 (producer: ``session_import.map_model`` plus
    ``session_import.offline_capable``): ``{"from": {"model", "provider"}, "to": {"model", "provider",
    "context_length"}, "reason": str, "offline_capable": bool}``. A model that stays is ``None``, not a dict."""
    return {
        "from": {"model": from_model, "provider": from_provider},
        "to": {"model": to_model, "provider": to_provider, "context_length": context_length},
        "reason": reason,
        "offline_capable": offline_capable,
    }


@dataclass(frozen=True)
class Busy:
    """A ``contracts.BusyReportLike``."""

    processes: tuple[dict, ...] = ()
    delegations: tuple[dict, ...] = ()
    cron: tuple[dict, ...] = ()
    warnings: tuple[str, ...] = ()


class StaticGitProbe:
    """``contracts.GitProbe`` from a fixed map ``{work tree root: shallow}`` (no git needed)."""

    def __init__(self, roots: dict[str, bool] | None = None, *, partial: Iterable[str] = ()) -> None:
        self.roots = {os.path.normpath(k): bool(v) for k, v in (roots or {}).items()}
        self.partial = {os.path.normpath(p) for p in partial}
        self.calls: list[tuple[str, str]] = []

    def toplevel(self, path: str) -> str | None:
        self.calls.append(("toplevel", path))
        p = os.path.normpath(path)
        best = None
        for root in self.roots:
            inside = p == root or p.startswith(root.rstrip("/\\") + os.sep)
            if inside and (best is None or len(root) > len(best)):
                best = root
        return best

    def common_dir(self, path: str) -> str | None:
        top = self.toplevel(path)
        return None if top is None else os.path.join(top, ".git")

    def is_shallow(self, path: str) -> bool:
        top = self.toplevel(path)
        return bool(top is not None and self.roots[top])

    def is_partial(self, path: str) -> bool:
        top = self.toplevel(path)
        return top is not None and top in self.partial


class CeilingGitProbe:
    """``contracts.GitProbe`` over the real ``git rev-parse`` (read-only: no identity, no config writes).

    ``ceiling`` is exported as ``GIT_CEILING_DIRECTORIES`` so a path below the test's tmp dir never resolves to
    an enclosing repository (pytest's basetemp lives inside the ctx checkout). Repositories for tests are
    created with plain ``git init`` (no commits — BUILD-ORDER §0, LAYER1-COMMON: identities only via E's
    repo builder)."""

    def __init__(self, ceiling: Path) -> None:
        self.ceiling = str(ceiling)
        self.calls: list[tuple[str, str]] = []

    def _env(self) -> dict[str, str]:
        env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
        env["GIT_CEILING_DIRECTORIES"] = self.ceiling
        env["GIT_CONFIG_NOSYSTEM"] = "1"
        env["GIT_TERMINAL_PROMPT"] = "0"
        return env

    def _rev_parse(self, path: str, *args: str) -> str | None:
        d = path if os.path.isdir(path) else os.path.dirname(path)
        try:
            proc = subprocess.run(  # noqa: S603 - fixed argv, no shell
                ["git", "-C", d, "rev-parse", *args],  # noqa: S607 - test helper resolves git from PATH
                capture_output=True,
                text=True,
                env=self._env(),
                timeout=20,
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired):
            return None
        if proc.returncode != 0:
            return None
        return proc.stdout.strip() or None

    def toplevel(self, path: str) -> str | None:
        self.calls.append(("toplevel", path))
        return self._rev_parse(path, "--show-toplevel")

    def common_dir(self, path: str) -> str | None:
        out = self._rev_parse(path, "--path-format=absolute", "--git-common-dir")
        return out

    def is_shallow(self, path: str) -> bool:
        return self._rev_parse(path, "--is-shallow-repository") == "true"


def git_init(path: Path) -> Path:
    """``git init`` without commits (no identity needed). Returns ``path``."""
    path.mkdir(parents=True, exist_ok=True)
    env = {k: v for k, v in os.environ.items() if not k.startswith("GIT_")}
    env["GIT_CONFIG_NOSYSTEM"] = "1"
    subprocess.run(  # noqa: S603 - fixed argv, no shell
        ["git", "init", "-q", str(path)],  # noqa: S607 - test helper resolves git from PATH
        check=True,
        env=env,
        capture_output=True,
        timeout=30,
    )
    return path


@dataclass(frozen=True)
class HermesFileStub:
    """Shape of ``hermes_files.HermesFile`` (cluster B, INTERFACES §4.1)."""

    kind: str
    source_path: str
    rel: str
    size: int
    session_id: str | None = None


def static_discover(files: Iterable[HermesFileStub], rejected: Iterable[tuple[str, str]] = ()) -> Callable:
    """A ``discover(rows, lineage_ids, hermes, cfg, *, rejected=None)`` stand-in returning ``files`` and appending
    ``rejected`` (``(source_path, reason)``) to the caller's list, like ``hermes_files.discover``; records its
    inputs."""
    files = list(files)
    refused = list(rejected)
    seen: list[dict] = []

    def discover(rows, lineage_ids, hermes, cfg, *, rejected=None):
        seen.append({"rows": list(rows), "lineage_ids": list(lineage_ids), "hermes": hermes, "cfg": cfg})
        if rejected is not None:
            rejected.extend(refused)
        return list(files)

    discover.seen = seen  # type: ignore[attr-defined]
    return discover


@dataclass
class ScriptedModel:
    """A side-question (``fn(prompt) -> str | None``) or ``llm`` (``fn(instr, blocks, schema, timeout)``) double.

    ``replies`` are returned in order (an ``Exception`` instance is raised instead); ``seen`` records every
    call's arguments."""

    replies: list = field(default_factory=list)
    seen: list = field(default_factory=list)

    def _next(self):
        if not self.replies:
            return None
        reply = self.replies.pop(0)
        if isinstance(reply, BaseException):
            raise reply
        return reply

    def side_question(self, prompt: str) -> str | None:
        self.seen.append(("side_question", prompt))
        return self._next()

    def llm(self, instructions: str, blocks: list, schema: dict, timeout_s: int) -> dict | None:
        self.seen.append(("llm", instructions, blocks, schema, timeout_s))
        return self._next()


@dataclass
class InjectedModel:
    """An ``llm`` that follows instructions planted in its input: every line ``ADD: <path>`` becomes an ``add``
    entry, every ``DROP: <id>`` a ``drop`` entry and ``NOTE: <text>`` the notes — the worst case of a model
    that obeys the transcript instead of the plugin."""

    seen: list = field(default_factory=list)

    def __call__(self, instructions: str, blocks: list, schema: dict, timeout_s: int) -> dict:
        self.seen.append((instructions, blocks))
        text = "\n".join(str(b.get("text", "")) for b in blocks if isinstance(b, dict))
        add, drop, notes = [], [], []
        for line in text.splitlines():
            line = line.strip()
            for marker, sink in (("ADD:", add), ("DROP:", drop), ("NOTE:", notes)):
                pos = line.find(marker)
                if pos >= 0:
                    sink.append(line[pos + len(marker) :].strip().strip("\"'`"))
        return {
            "add": [{"path": p, "reason": "the transcript told me to"} for p in add],
            "drop": [{"id": d, "reason": "the transcript told me to"} for d in drop],
            "tools_needed": [],
            "env_vars_needed": [],
            "notes_for_target": " ".join(notes)[:1500],
        }


def fake_redact(text: str) -> str:
    """The core's ``redact_sensitive_text(text, force=True)``; a small stand-in without Hermes."""
    try:
        from agent.redact import redact_sensitive_text
    except ImportError:  # pragma: no cover - the gates always run with the upstream checkout
        import re

        return re.sub(r"(?i)\b([A-Z0-9_]*(?:KEY|TOKEN|SECRET)[A-Z0-9_]*\s*[=:]\s*)\S+", r"\1***", text)
    return redact_sensitive_text(text, force=True)


def json_reply(obj: dict) -> str:
    """A side-question reply wrapping ``obj`` in prose, as chat models do."""
    return "Sure, here is what I found:\n```json\n" + json.dumps(obj) + "\n```\nHope that helps."


def not_transferred(path: str, why: str) -> NotTransferred:
    return NotTransferred(path=path, why=why)


def replace_item(plan: ResourcePlan, item_id: str, **changes) -> ResourcePlan:
    """``plan`` with the item ``item_id`` changed."""
    return replace(plan, items=tuple(replace(i, **changes) if i.item_id == item_id else i for i in plan.items))
