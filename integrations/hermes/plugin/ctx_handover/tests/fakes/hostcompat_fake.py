"""Test double for ``engine/hostcompat.py`` and a recording PluginContext stand-in (layer K0).

``FakeHost`` has one method per public function of ``hostcompat`` with the same parameters; ``install()``
swaps them in on the real module (``monkeypatch.setattr``), so every caller that goes through
``hostcompat.<name>`` sees the fake. A K0 test keeps the two in lock-step (``test_hostcompat_fake_parity``).

State is plain attributes a test may change:

``config``            dict returned by ``load_config`` (a deep copy per call)
``secrets``           what ``read_secret`` (= ``get_secret``) resolves; then ``env_file``
``env_file``          the profile ``.env``; ``save_env`` writes here, ``read_env_fresh`` reads here first
``env_locked``        names ``env_writable`` refuses (``EnvNotWritable``)
``silently_refused``  names ``save_env`` drops without error (the core's silent refusal, R16)
``usable_providers``  names ``provider_usable`` accepts
``llm``               the callable ``plugin_llm`` returns (None = unavailable)
``feature_overrides`` fields forced into ``features()``
``facts_overrides``   fields forced into ``host_facts()``
``mcp_grant_refusal`` ``None``, or the ``details.reason`` ``grant_mcp_server`` refuses with (``managed`` …)
``calls``             ``(name, args)`` of every call, in order

``grant_mcp_server`` writes into ``config["plugins"]["entries"]["ctx_handover"]["mcp_allowlist"]``, so a later
``load_config`` sees the grant as the real one does.

Paths come from the isolated test home: ``effective_home()`` = ``user_home`` = ``HOME``, ``hermes_home`` =
``HERMES_HOME`` = ``hermes_root`` (one profile), the document cache ``<hermes_home>/cache/documents``, the plugin
data dir ``<hermes_home>/plugin-data/ctx_handover``.
``session_db()`` opens a real ``hermes_state.SessionDB`` at ``<hermes_home>/state.db`` when Hermes is importable.
``redact`` uses the core's ``redact_sensitive_text(force=True)`` when importable, else a small offline stand-in.
"""

from __future__ import annotations

import contextvars
import copy
import os
import re
from collections.abc import Callable
from dataclasses import replace
from pathlib import Path

from ctx_handover.engine import hostcompat
from ctx_handover.engine.errors import BadRequest, CapabilityMissing, EnvNotWritable
from ctx_handover.engine.hostcompat import HostFeatures
from ctx_handover.engine.model import HermesPaths, HostFacts

_OFFLINE_SECRET_RE = re.compile(
    r"\b(sk-[A-Za-z0-9_-]{12,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9-]{10,})\b"
    r"|\b([A-Z0-9_]*(?:KEY|TOKEN|SECRET|PASSWORD)[A-Z0-9_]*\s*[=:]\s*)\S+",
    re.IGNORECASE,
)


def _offline_redact(text: str) -> str:
    return _OFFLINE_SECRET_RE.sub(lambda m: (m.group(2) or "") + "***", text)


class FakeHost:
    """In-memory host (see module docstring)."""

    def __init__(self, *, user_home: Path, hermes_home: Path) -> None:
        self.user_home = Path(user_home)
        self.hermes_home = Path(hermes_home)
        self.hermes_root_path = self.hermes_home
        self.document_cache = self.hermes_home / "cache" / "documents"
        self.plugin_data = self.hermes_home / "plugin-data" / hostcompat.PLUGIN_ID
        self.config: dict = {"model": {"default": "test-model", "provider": "test-provider"}}
        self.secrets: dict[str, str] = {}
        self.env_file: dict[str, str] = {}
        self.env_locked: set[str] = set()
        self.silently_refused: set[str] = set()
        self.usable_providers: set[str] = {"test-provider"}
        self.llm: Callable[[str, list, dict, int], dict | None] | None = None
        self.feature_overrides: dict = {}
        self.facts_overrides: dict = {}
        self.mcp_grant_refusal: str | None = None
        self.calls: list[tuple[str, tuple]] = []
        self._real = {name: getattr(hostcompat, name) for name in public_functions()}

    # -- installation -----------------------------------------------------------------------------------
    def install(self, monkeypatch) -> None:
        """Replace every public function of ``engine.hostcompat`` with this fake's method."""
        for name in public_functions():
            monkeypatch.setattr(hostcompat, name, getattr(self, name))

    def _record(self, name: str, *args) -> None:
        self.calls.append((name, args))

    # -- hostcompat surface -----------------------------------------------------------------------------
    def features(self, ctx=None) -> HostFeatures:
        self._record("features", ctx)
        base = HostFeatures(
            side_question=ctx is not None and callable(getattr(ctx, "side_question", None)),
            ask_choice=ctx is not None and callable(getattr(ctx, "ask_choice", None)),
            post_message=ctx is not None and callable(getattr(ctx, "post_message", None)),
            host_isolated=False,
            session_db_ok=True,
            plugin_llm=self.llm is not None,
            missing=(),
        )
        return replace(base, **self.feature_overrides)

    def session_db(self, path: Path | None = None):
        self._record("session_db")
        from hermes_state import SessionDB

        return SessionDB(Path(path) if path is not None else self.hermes_home / "state.db")

    def state_db_path(self) -> Path:
        self._record("state_db_path")
        return self.hermes_home / "state.db"

    def effective_home(self) -> Path:
        """The isolated test ``HOME`` (``user_home``): the home ``~`` means for the profile (L3-F20)."""
        self._record("effective_home")
        return self.user_home

    def hermes_paths(self) -> HermesPaths:
        self._record("hermes_paths")
        return HermesPaths(home=str(self.hermes_home), document_cache=str(self.document_cache))

    def host_facts(self) -> HostFacts:
        self._record("host_facts")
        from ctx_handover.engine import PLUGIN_VERSION

        model = self.config.get("model") if isinstance(self.config.get("model"), dict) else {}
        facts = HostFacts(
            is_wsl=False,
            is_container=False,
            profile="default",
            multiplex=False,
            hermes_version="0.0.0",
            plugin_version=PLUGIN_VERSION,
            model=model.get("default"),
            provider=model.get("provider"),
            context_length=model.get("context_length"),
            terminal_backend="local",
            hermes_home=str(self.hermes_home),
            document_cache=str(self.document_cache),
        )
        return replace(facts, **self.facts_overrides)

    def plugin_dir(self) -> Path:
        self._record("plugin_dir")
        self.plugin_data.mkdir(parents=True, exist_ok=True)
        os.chmod(self.plugin_data, 0o700)
        return self.plugin_data

    def load_config(self) -> dict:
        self._record("load_config")
        return copy.deepcopy(self.config)

    def grant_mcp_server(self, server: str) -> None:
        self._record("grant_mcp_server", server)
        if not isinstance(server, str) or not hostcompat.MCP_SERVER_RE.fullmatch(server):
            raise BadRequest("invalid MCP server name", field="server")
        entry = self.config.setdefault("plugins", {}).setdefault("entries", {}).setdefault(hostcompat.PLUGIN_ID, {})
        listed = entry.get("mcp_allowlist")
        listed = [str(item) for item in listed] if isinstance(listed, list) else []
        if server in listed:
            return
        if self.mcp_grant_refusal is not None:
            raise CapabilityMissing(
                "the MCP allowlist was not written (fake)",
                capability="mcp_allowlist",
                reason=self.mcp_grant_refusal,
                command=f"hermes config set plugins.entries.{hostcompat.PLUGIN_ID}.mcp_allowlist '[\"{server}\"]'",
            )
        entry["mcp_allowlist"] = [*listed, server]

    def read_secret(self, name: str) -> str | None:
        self._record("read_secret", name)
        value = self.secrets.get(name, self.env_file.get(name))
        return value if value else None

    def env_writable(self, name: str) -> None:
        self._record("env_writable", name)
        if name in self.env_locked:
            raise EnvNotWritable(f"{name} is locked by the managed scope", variable=name)

    def save_env(self, name: str, value: str) -> None:
        self._record("save_env", name)
        if name in self.silently_refused:
            return
        self.env_file[name] = value

    def provider_usable(self, name: str, config: dict) -> bool:
        self._record("provider_usable", name)
        return str(name or "") in self.usable_providers

    def redact(self, text: str) -> str:
        self._record("redact")
        try:
            from agent.redact import redact_sensitive_text
        except ImportError:
            return _offline_redact(text)
        return redact_sensitive_text(text, force=True)

    def plugin_llm(self) -> Callable[[str, list, dict, int], dict | None] | None:
        self._record("plugin_llm")
        return self.llm

    def run_in_context(self, fn: Callable, *a, **kw):
        self._record("run_in_context", fn)
        return contextvars.copy_context().run(fn, *a, **kw)

    def hermes_root(self) -> Path:
        self._record("hermes_root")
        return self.hermes_root_path

    def read_env_fresh(self, name: str) -> str | None:
        self._record("read_env_fresh", name)
        value = self.env_file.get(name)
        return value if value else None

    def approvals_off(self, config: dict) -> bool:
        self._record("approvals_off")
        return self._real["approvals_off"](config)

    def hermes_version(self) -> str:
        self._record("hermes_version")
        return "0.0.0"


def public_functions() -> tuple[str, ...]:
    """Names of the public functions defined in ``engine.hostcompat`` (the surface the fake must mirror)."""
    import inspect

    return tuple(
        sorted(
            name
            for name, obj in vars(hostcompat).items()
            if inspect.isfunction(obj) and obj.__module__ == hostcompat.__name__ and not name.startswith("_")
        )
    )


class _Handle:
    """What ``ctx.post_message`` returns: ``edit(text)`` records the new text."""

    def __init__(self, ctx: FakeCtx, index: int) -> None:
        self._ctx, self._index = ctx, index

    def edit(self, text: str) -> dict:
        self._ctx.messages[self._index] = text
        self._ctx.edits.append((self._index, text))
        return {"ok": True, "delivered": True}


class _Llm:
    """``ctx.llm`` stand-in: ``complete_structured(**kw)`` records the call and returns ``FakeCtx.llm_result``."""

    def __init__(self, ctx: FakeCtx) -> None:
        self._ctx = ctx

    def complete_structured(self, **kw):
        self._ctx.llm_calls.append(kw)
        result = self._ctx.llm_result
        if isinstance(result, BaseException):
            raise result
        return result


class FakeCtx:
    """A recording stand-in for ``hermes_cli.plugins.PluginContext`` (signatures of
    ``hermes_cli/plugins_context.py`` and ``plugins_capability_ctx.py``). Cluster tests may subclass it."""

    def __init__(self, *, host: FakeHost | None = None) -> None:
        self.host = host
        self.commands: dict[str, dict] = {}
        self.cli_commands: dict[str, dict] = {}
        self.hooks: dict[str, list[Callable]] = {}
        self.tools: dict[str, dict] = {}
        self.messages: list[str] = []
        self.edits: list[tuple[int, str]] = []
        self.choice_answers: list[str | None] = []
        self.choices_asked: list[tuple[str, list]] = []
        self.capabilities: set[str] = set()
        self.side_answers: list[str | None] = []
        self.side_calls: list[tuple[str, str, float]] = []
        self.mcp_calls: list[tuple[str, str, dict | None, float]] = []
        self.mcp_result: dict = {"ok": True}
        self.injected: list[tuple[str, str, str | None]] = []
        self.inject_result = False
        self.llm_calls: list[dict] = []
        self.llm_result: object = None
        self.llm = _Llm(self)
        self.cwd = str(host.user_home) if host is not None else ""
        self.surface = "local"

    def register_command(
        self, name: str, handler: Callable, description: str = "", args_hint: str = "", argument_mode=None
    ):
        self.commands[name] = {
            "handler": handler,
            "description": description,
            "args_hint": args_hint,
            "argument_mode": argument_mode,
        }

    def register_cli_command(
        self, name: str, help: str, setup_fn: Callable, handler_fn: Callable | None = None, description: str = ""
    ):
        self.cli_commands[name] = {"help": help, "setup_fn": setup_fn, "handler_fn": handler_fn}

    def register_hook(self, hook_name: str, callback: Callable, *, hold_stream: bool = False):
        self.hooks.setdefault(hook_name, []).append(callback)

    def register_tool(self, name: str, toolset: str, schema: dict, handler: Callable, **kw):
        self.tools[name] = {"toolset": toolset, "schema": schema, "handler": handler, **kw}

    def post_message(self, text: str):
        self.messages.append(text)
        return _Handle(self, len(self.messages) - 1)

    def ask_choice(self, question: str, choices: list) -> str | None:
        self.choices_asked.append((question, list(choices)))
        return self.choice_answers.pop(0) if self.choice_answers else None

    def has_capability(self, capability: str) -> bool:
        return capability in self.capabilities

    def side_question(self, token: str, prompt: str, *, timeout: float = 15.0) -> str | None:
        self.side_calls.append((token, prompt, timeout))
        return self.side_answers.pop(0) if self.side_answers else None

    def call_mcp(self, server: str, tool: str, arguments: dict | None = None, timeout: float = 30) -> dict:
        self.mcp_calls.append((server, tool, arguments, timeout))
        return self.mcp_result

    def inject_message(self, content: str, role: str = "user", *, session_key: str | None = None, origin=None):
        self.injected.append((content, role, session_key))
        return self.inject_result

    def session_cwd(self) -> str:
        return self.cwd

    def session_surface(self) -> str:
        return self.surface
