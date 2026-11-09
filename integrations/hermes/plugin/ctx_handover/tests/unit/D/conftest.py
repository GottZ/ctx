"""Fixtures of the cluster D suites (planning and environment).

``lay``        a real directory layout in the isolated test home: project, notes, credential dirs, Hermes home
``cfg``        ``PolicyConfig`` for ``lay`` (``cfg(**over)`` builds variants)
``session``    builds a real ``SessionDB`` transcript through the core's own write paths and returns the rows
               and the session row exactly as the export path reads them
"""

from __future__ import annotations

import json
from dataclasses import dataclass, field
from pathlib import Path

import pytest

from ctx_handover.engine.model import Options
from ctx_handover.engine.policy import PolicyConfig


@dataclass
class Layout:
    home: Path
    hermes_home: Path
    project: Path
    notes: Path
    plugin_dir: Path

    def cfg(self, **over) -> PolicyConfig:
        options = over.pop("options", Options())
        kwargs = {
            "home": self.home,
            "hermes_home": self.hermes_home,
            "hermes_root": self.hermes_home,
            "plugin_dir": self.plugin_dir,
            "case_insensitive": False,
            "target_os": "linux",
            "document_cache": self.hermes_home / "cache" / "documents",
        }
        kwargs.update(over)
        return PolicyConfig.from_options(options, **kwargs)


def write(path: Path, text: str = "hello\n") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


@pytest.fixture
def lay(user_home, hermes_home) -> Layout:
    project = user_home / "proj"
    notes = user_home / "notes"
    plugin_dir = hermes_home / "plugin-data" / "ctx_handover"
    for d in (project, notes, plugin_dir):
        d.mkdir(parents=True, exist_ok=True)
    write(project / "src" / "main.py", "print('hi')\n")
    write(notes / "todo.md", "- ship it\n")
    return Layout(user_home, hermes_home, project, notes, plugin_dir)


@dataclass
class Transcript:
    """Builds one session through ``SessionBuilder`` (core write paths) and reads it back like the export."""

    builder: object
    db: object
    sid: str
    _n: int = 0
    calls: list = field(default_factory=list)

    def _call_id(self) -> str:
        self._n += 1
        return f"call_{self._n:04d}"

    def user(self, text: str) -> None:
        self.builder.user(self.sid, text)

    def assistant(self, text: str) -> None:
        self.builder.assistant(self.sid, text)

    def tool(self, name: str, args: dict, result: dict | str | None = None) -> None:
        call_id = self._call_id()
        call = self.builder.tool_call(name, args, call_id=call_id)
        self.builder.assistant(self.sid, "", tool_calls=[call])
        content = result if isinstance(result, str) else json.dumps(result if result is not None else {"ok": True})
        self.builder.tool(self.sid, call_id, content, name=name)

    def terminal(self, command: str, *, cwd: str | None = None, workdir: str | None = None) -> None:
        args = {"command": command}
        if workdir:
            args["workdir"] = workdir
        result = {"output": "", "exit_code": 0, "error": None}
        if cwd:
            result["cwd"] = cwd
        self.tool("terminal", args, result)

    def rows(self) -> list[dict]:
        exported = self.db.export_session_lineage(self.sid, include_inactive=True)
        return list(exported["messages"])

    def session_row(self) -> dict:
        return dict(self.db.get_session(self.sid))


@pytest.fixture
def transcript(make_session, session_db):
    def build(cwd: Path | str | None, **kw) -> Transcript:
        sid = make_session.create(cwd=str(cwd) if cwd is not None else None, **kw)
        return Transcript(make_session, session_db, sid)

    return build
