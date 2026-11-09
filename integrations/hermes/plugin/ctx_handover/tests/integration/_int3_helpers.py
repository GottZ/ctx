"""Helpers of the layer-2 mandatory scenarios (wp-int3, ``test_int_scn_*``): real modules, real ``SessionDB``, real
git (repositories only through ``tests/fakes/repo_builder.py``), real block stores and the real listener process of
``harness.py``. Only the observation side lives here: what a user would look at after a handover (session history,
work-tree bytes, index, stash, source marks, the surfaces' answers).

Two seams are process seams, not mocks of the code under test:

* ``python -m ctx_handover.tests.integration._int3_helpers push-until-batch <cfg.json>`` runs a push conductor in its
  own process (the CLI process of the laptop) and parks it after a given number of block batches reached the
  listener, so the test can SIGKILL it in the middle of the block upload (scenario 5).
* ``FakeProvider`` is an OpenAI-compatible endpoint on loopback that records every chat request and answers 500:
  a real ``AIAgent`` turn on a handed-over session must be vetoed before it reaches the model (scenario 10).
"""

from __future__ import annotations

import contextlib
import json
import os
import signal
import subprocess
import sys
import threading
import time
from collections.abc import Callable, Iterator
from dataclasses import dataclass, field, replace
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
from typing import Any, ClassVar

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import PLUGIN_PARENT, UPSTREAM, Home

STALL_S = 90
"""Bound on a stuck job (``Options.stall_timeout_s``; the setting ``stall_timeout_min``): an in-doubt or stalled
conductor parks after this instead of polling for the default 30 minutes, so a defect fails the test rather than
hanging the suite."""

IGNORED_NAMES = (b"build", b"debug.log")
"""Top-level names of ``wip_repo`` that ``.gitignore`` covers: they never travel (D10: non-ignored files only)."""


# ---------------------------------------------------------------------------------------------------------------------
# repositories and their observable state
# ---------------------------------------------------------------------------------------------------------------------


def wip_repo(root: Path, *, stash: bool = True) -> RepoBuilder:
    """A repository with every kind of unsaved work of D10: a stash entry, a staged change (index patch) with a
    further change on top in the work tree, an untracked non-ignored file — plus ignored files that stay behind."""
    r = RepoBuilder.init(root)
    r.write(".gitignore", "build/\n*.log\n")
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.write("src/util.py", "VALUE = 1\n")
    r.add_all()
    r.commit("first")
    r.write("src/util.py", "VALUE = 2  # parked in the stash\n")
    if stash:
        r.stash("wip util")
    r.write("src/main.py", "print('v2 staged')\n")
    r.add("src/main.py")
    r.write("src/main.py", "print('v3 in the work tree')\n")
    r.write("notes/todo.txt", "untracked work\n")
    r.write("build/out.bin", b"\x00ignored build output\n")
    r.write("debug.log", "ignored log line\n")
    return r


def repo_view(path: Path) -> dict:
    """What a user sees of a repository: HEAD, branch, index entries (mode, object, stage, path), the stash list
    (commit and message), and the bytes of the non-ignored work tree."""
    r = RepoBuilder(Path(path))
    return {
        "head": r.head(),
        "branch": r.out("symbolic-ref", "-q", "HEAD"),
        "index": r.index_entries(),
        "stash": r.stash_list(),
        "worktree": tree_digest(Path(path), exclude=(b".git", *IGNORED_NAMES)),
    }


def present(root: Path, *rels: str) -> dict[str, bool]:
    return {rel: os.path.lexists(Path(root) / rel) for rel in rels}


# ---------------------------------------------------------------------------------------------------------------------
# sessions and jobs
# ---------------------------------------------------------------------------------------------------------------------


def session_in(home: Home, cwd: Path | str, title: str, *, turns: int = 1, extra: Callable | None = None) -> str:
    """A titled session in ``home`` whose cwd is ``cwd`` with ``turns`` user/assistant exchanges."""
    b = builder(home)
    sid = b.create(title=title, cwd=str(cwd))
    for i in range(turns):
        b.user(sid, f"please change src/main.py (step {i})")
        b.assistant(sid, f"done, see src/main.py (step {i})")
    if extra is not None:
        extra(b, sid)
    return sid


@dataclass
class JobResult:
    job_id: str
    rc: int
    row: dict
    errors: list = field(default_factory=list)
    final: Any = None

    def why(self) -> tuple:
        return (self.row.get("state"), self.row.get("last_error"), self.errors)


def run_job(
    home: Home,
    mode: Mode,
    peer,
    session_ref: SessionRef,
    *,
    options: Options | None = None,
    include_secret: tuple[str, ...] = (),
    confirm: Callable[[dict], str | None] | None = None,
    bundle_path: Path | None = None,
    side_question: Callable[[str], str | None] | None = None,
) -> JobResult:
    """``plan_sync`` + ``run_foreground`` of the profile's one runner in ``home`` (the CLI path of DESIGN §14.2) with
    the local ``--include-secret`` as the explicit parameter of contract X07."""
    options = options or Options(refine="off", yes=True)
    if options.stall_timeout_s == Options().stall_timeout_s:
        options = replace(options, stall_timeout_s=STALL_S)
    with home.active():
        runner = jobs.runner_for(None)
        job_id, final = runner.plan_sync(
            mode,
            peer,
            options,
            session_ref=session_ref,
            calling_session=None,
            session_key=None,
            confirm=confirm,
            bundle_path=bundle_path,
            include_secret=tuple(include_secret),
            side_question=side_question,
        )
        rc = runner.run_foreground(job_id) if final is not None else None
        row = runner.ledger.job(job_id, Role.CONDUCTOR) or {}
        if rc is None:
            rc = jobs.exit_code(row)
        errors = [e["data"] for e in runner.ledger.events(job_id, Role.CONDUCTOR) if e["kind"] == "error"]
    return JobResult(job_id, int(rc), dict(row), errors, final)


def bounded(home: Home, **settings) -> None:
    """The plugin settings of ``home`` for surface-driven jobs: refinement off (no model) and ``stall_timeout_min``
    = 1 (see ``STALL_S``), plus ``settings``."""
    home.write_settings({"refine": "off", "stall_timeout_min": 1, **settings})


def arrival(home: Home, job_id: str) -> dict | None:
    """The target's arrival row of a committed job (``Ledger.arrival_rows``)."""
    for row in home.ledger().arrival_rows():
        if row["job_id"] == job_id:
            return dict(row)
    return None


def title_of(home: Home, sid: str) -> str | None:
    return (home.db().get_session(sid) or {}).get("title")


def norm_conv(conv) -> list[dict]:
    """A model/display history without in-memory row markers (``_row_id`` etc.) and storage ids."""
    out = []
    for m in conv:
        d = {k: v for k, v in m.items() if not k.startswith("_") and k not in ("message_id", "id", "session_id")}
        if m.get("_compressed_summary"):
            d["_compressed_summary"] = True
        out.append(d)
    return out


def histories(db, tip: str) -> tuple[list[dict], list[dict]]:
    """``(model history, display history)`` exactly as a resume reads them (``get_resume_conversations``)."""
    model, display = db.get_resume_conversations(tip)
    return norm_conv(model), norm_conv(display)


# ---------------------------------------------------------------------------------------------------------------------
# the surfaces (cluster F) over the real engine
# ---------------------------------------------------------------------------------------------------------------------


def surface(
    home: Home,
    raw: str,
    *,
    session_id: str | None = None,
    platform: str = "cli",
    answers: list[str | None] | None = None,
    plugin=None,
):
    """Run ``raw`` (``/handover …`` text or ``hermes handover …`` tokens) through the plugin's real surfaces and
    ``EngineBackend`` in ``home``. ``answers`` feed ``ask_choice`` (None = no interactive surface). Returns
    ``(call, plugin)``."""
    from ctx_handover.engine import commands
    from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

    if plugin is None:
        plugin = commands.Plugin(FakeCtx())
    ctx = plugin.ctx
    asker = None
    if answers is not None:
        ctx.choice_answers = list(answers)
        asker = ctx.ask_choice
    call = commands.Call(platform=platform, session_id=session_id, session_key=None, asker=asker, cli=platform == "cli")
    with home.active():
        plugin.run(raw, call)
    return call, plugin


# ---------------------------------------------------------------------------------------------------------------------
# a model endpoint that must never be reached, and a real agent turn
# ---------------------------------------------------------------------------------------------------------------------


class _Provider(BaseHTTPRequestHandler):
    chat_requests: ClassVar[list] = []
    answer: ClassVar[bool] = False

    def do_POST(self):
        body = self.rfile.read(int(self.headers.get("Content-Length", 0) or 0))
        try:
            req = json.loads(body.decode() or "{}")
        except ValueError:
            req = {}
        if isinstance(req, dict) and "messages" in req:
            type(self).chat_requests.append(req)
        if not type(self).answer:
            self.send_response(500)
            self.end_headers()
            return
        message = {"role": "assistant", "content": "ok"}
        if isinstance(req, dict) and req.get("stream"):
            chunks = [
                {"index": 0, "delta": message, "finish_reason": None},
                {"index": 0, "delta": {}, "finish_reason": "stop"},
            ]
            payload = (
                "".join(
                    "data: "
                    + json.dumps(
                        {
                            "id": "c1",
                            "object": "chat.completion.chunk",
                            "created": 0,
                            "model": "test-model",
                            "choices": [c],
                        }
                    )
                    + "\n\n"
                    for c in chunks
                )
                + "data: [DONE]\n\n"
            )
            data, ctype = payload.encode(), "text/event-stream"
        else:
            doc = {
                "id": "c1",
                "object": "chat.completion",
                "created": 0,
                "model": "test-model",
                "choices": [{"index": 0, "message": message, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2},
            }
            data, ctype = json.dumps(doc).encode(), "application/json"
        self.send_response(200)
        self.send_header("Content-Type", ctype)
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *a, **kw):
        pass


@contextlib.contextmanager
def fake_provider(*, answer: bool = False) -> Iterator[tuple[str, list]]:
    """``(base_url, chat_requests)`` of a loopback model endpoint that records chat requests. It answers 500 (a turn
    that must never reach the model) or, with ``answer``, a minimal OpenAI-compatible completion ``ok``."""
    _Provider.chat_requests = []
    _Provider.answer = answer
    srv = HTTPServer(("127.0.0.1", 0), _Provider)
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    try:
        yield f"http://127.0.0.1:{srv.server_address[1]}/v1", _Provider.chat_requests
    finally:
        srv.shutdown()
        srv.server_close()


def hook_dispatcher(plugin) -> Callable[..., list]:
    """Hermes' ``invoke_hook`` over the callbacks the plugin's ``register()`` hands to Hermes."""
    callbacks = plugin.hooks.callbacks()

    def invoke(name: str, **kw):
        cb = callbacks.get(name)
        if cb is None:
            return []
        result = cb(**kw)
        return [result] if result is not None else []

    return invoke


def agent_turn(db, sid: str, provider_url: str, dispatcher: Callable[..., list], typed: str) -> dict:
    """One real ``AIAgent`` turn on ``sid`` with ``dispatcher`` as Hermes' plugin hook dispatcher."""
    from unittest.mock import patch

    from run_agent import AIAgent

    agent = AIAgent(
        api_key="test-key",
        base_url=provider_url,
        provider="openai-compat",
        model="test-model",
        max_iterations=2,
        enabled_toolsets=[],
        quiet_mode=True,
        skip_context_files=True,
        skip_memory=True,
        save_trajectories=False,
        platform="cli",
        session_db=db,
        session_id=sid,
    )
    history = db.get_messages_as_conversation(sid)
    with patch("hermes_cli.plugins.invoke_hook", side_effect=dispatcher):
        return agent.run_conversation(typed, conversation_history=history, task_id="t")


# ---------------------------------------------------------------------------------------------------------------------
# a conductor process that can be killed in the middle of the block upload (scenario 5)
# ---------------------------------------------------------------------------------------------------------------------


def child_env(home: Home) -> dict[str, str]:
    env = {k: v for k, v in os.environ.items() if not k.startswith(("HERMES_", "CTX_HANDOVER_", "GIT_"))}
    env.update(home.env())
    env["PYTHONPATH"] = os.pathsep.join(p for p in (UPSTREAM, str(PLUGIN_PARENT)) if p)
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    return env


class ParkedPush:
    """A push conductor in its own process that parks (sleeps, holding the only upload slot) once the listener
    acknowledged at least ``min_blocks`` blocks. ``sent`` = block ids acknowledged before the park, ``announced`` =
    block ids of every manifest the conductor put on the target (the job's need-set as far as it got)."""

    def __init__(self, home: Home, sid: str, peer_name: str, options: dict, *, min_blocks: int = 4) -> None:
        self.dir = home.root / "parked-push"
        self.dir.mkdir(parents=True, exist_ok=True)
        cfg = {
            "session_id": sid,
            "peer_name": peer_name,
            "options": options,
            "min_blocks": min_blocks,
            "sent_log": str(self.dir / "sent.log"),
            "announced_log": str(self.dir / "announced.log"),
        }
        path = self.dir / "cfg.json"
        path.write_text(json.dumps(cfg), encoding="utf-8")
        self._err = (self.dir / "child.log").open("ab")
        self.proc = subprocess.Popen(  # noqa: S603 - fixed argv: this interpreter and a module of the test tree
            [sys.executable, "-m", "ctx_handover.tests.integration._int3_helpers", "push-until-batch", str(path)],
            stdout=subprocess.PIPE,
            stderr=self._err,
            env=child_env(home),
            cwd=str(home.root),
        )
        self.job_id: str | None = None

    def wait_parked(self, timeout: float = 180.0) -> str:
        """Read the child's lines until it reports the park; returns the job id."""
        deadline = time.monotonic() + timeout
        while time.monotonic() < deadline:
            line = self.proc.stdout.readline().decode().strip()
            if not line:
                if self.proc.poll() is not None:
                    raise RuntimeError(f"conductor process ended early (rc {self.proc.returncode}); see {self.dir}")
                continue
            if line.startswith("JOB "):
                self.job_id = line.split()[1]
            elif line.startswith("PARKED"):
                return str(self.job_id)
        raise RuntimeError("conductor process did not park in time")

    def kill(self) -> None:
        if self.proc.poll() is None:
            os.kill(self.proc.pid, signal.SIGKILL)
            self.proc.wait(10)
        self._err.close()

    def sent(self) -> set[str]:
        return _ids(self.dir / "sent.log")

    def announced(self) -> set[str]:
        return _ids(self.dir / "announced.log")


def _ids(path: Path) -> set[str]:
    if not path.exists():
        return set()
    return {line.strip() for line in path.read_text(encoding="utf-8").splitlines() if line.strip()}


def _push_until_batch(cfg_path: str) -> int:
    from ctx_handover.engine import remote
    from ctx_handover.engine.jsonutil import from_wire

    cfg = json.loads(Path(cfg_path).read_text(encoding="utf-8"))
    original_write = remote.RemoteBlocks.write_blocks
    original_put = remote.RemoteBlocks.put_manifests
    gate = threading.Lock()
    done = {"blocks": 0}

    def put_manifests(self, manifests):
        manifests = list(manifests)
        with open(cfg["announced_log"], "a", encoding="utf-8") as fh:
            for m in manifests:
                fh.write("".join(f"{b}\n" for b in m.blocks))
        return original_put(self, manifests)

    def write_blocks(self, frames):
        frames = list(frames)
        with gate:  # one batch at a time; once parked, no further batch ever reaches the listener
            result = original_write(self, iter(frames))
            rejected = {b for b, _ in result[1]}
            with open(cfg["sent_log"], "a", encoding="utf-8") as fh:
                fh.write("".join(f"{b}\n" for b, _ in frames if b not in rejected))
            done["blocks"] += len(frames) - len(rejected)
            if done["blocks"] >= int(cfg["min_blocks"]):
                print("PARKED", flush=True)
                time.sleep(3600)
            return result

    remote.RemoteBlocks.write_blocks = write_blocks
    remote.RemoteBlocks.put_manifests = put_manifests
    runner = jobs.runner_for(None)
    peer = next(p for p in runner.ledger.peers() if p.name == cfg["peer_name"])
    job_id, final = runner.plan_sync(
        Mode.PUSH,
        peer,
        from_wire(Options, cfg["options"]),
        session_ref=SessionRef(id=cfg["session_id"]),
        calling_session=None,
        session_key=None,
    )
    print(f"JOB {job_id}", flush=True)
    if final is None:
        print("ENDED", flush=True)
        return 1
    return runner.run_foreground(job_id)


def main(argv: list[str]) -> int:
    if len(argv) == 2 and argv[0] == "push-until-batch":
        return _push_until_batch(argv[1])
    print(f"usage: {Path(__file__).name} push-until-batch <cfg.json>", file=sys.stderr)
    return 2


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))


__all__ = [
    "IGNORED_NAMES",
    "STALL_S",
    "JobResult",
    "ParkedPush",
    "agent_turn",
    "arrival",
    "bounded",
    "fake_provider",
    "histories",
    "hook_dispatcher",
    "norm_conv",
    "present",
    "repo_view",
    "run_job",
    "session_in",
    "surface",
    "title_of",
    "wip_repo",
]
