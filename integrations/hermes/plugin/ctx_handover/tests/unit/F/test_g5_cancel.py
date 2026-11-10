"""L3-F22: ``cancel`` of a job that cannot be cancelled any more.

Lab (lab finding L3-F22): ``hermes handover cancel 5FT9KS
--json`` on a pull that had ended ``done`` answered exit 0, ``ok: true``, "Too late to cancel: the target is
committing …" — the job was finished, not committing, and nothing was cancelled. DESIGN §14.2: a refusal exits ≠ 0
and carries ``error.code`` in ``--json`` (README §Use, L3-F14).

The CLI runs through the real ``Plugin.cli`` → ``EngineBackend.cancel`` → ``jobs.JobRunner`` over a real ``Ledger``."""

from __future__ import annotations

import json
import threading
import types

import pytest

from ctx_handover.engine import commands, jobs
from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.errors import InvalidState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer

from .test_commands import _plugin
from .test_g3_f import _cli


def _engine_plugin(fake_host, tmp_path) -> tuple[Plugin, Ledger]:
    ledger = Ledger(tmp_path / "ledger.sqlite3")
    ctx = FakeCtx(host=fake_host)
    holder: dict = {}

    def factory(c, p):
        if "eb" not in holder:
            eb = EngineBackend(c, p)
            runtime = types.SimpleNamespace(
                ledger=ledger, store=None, veto=None, identity=None, identity_error=None, lock=threading.RLock()
            )
            eb._parts.update({"runtime": runtime, "runner": jobs.JobRunner(ledger, None, keepalive=False)})
            holder["eb"] = eb
        return holder["eb"]

    return Plugin(ctx, backend_factory=factory), ledger


def _job(ledger: Ledger, state: str, *, commit: bool) -> str:
    job_id = ulid()
    ledger.create_job(job_id, Role.CONDUCTOR, Mode.PULL, None, {})
    if commit:
        assert ledger.intent_commit(job_id, 0)
    if state != "created":
        ledger.transition(job_id, Role.CONDUCTOR, "*", state, {}, epoch=0)
    return job_id


def _payload(capsys) -> dict:
    return json.loads(capsys.readouterr().out.strip().splitlines()[-1])


@pytest.mark.parametrize(
    ("state", "commit", "says"),
    [
        ("done", True, "has already finished"),  # the lab's case: every finished job journaled ``intent: commit``
        ("aborted", False, "has already ended (aborted)"),
        ("committing", True, "can no longer be undone (committing)"),
        ("retiring", True, "can no longer be undone (retiring)"),
    ],
)
def test_g5_cancel_that_cancels_nothing_exits_non_zero_and_names_the_state(
    capsys, fake_host, tmp_path, state, commit, says
):
    """★ L3-F22: ``hermes handover cancel <short id> --json`` of a job that is finished, aborted or past its commit
    point exits 3 (``invalid_state``, DESIGN §14.2 busy/conflict), ``ok: false``, and the text names the job's real
    state; "committing" only for a job that is committing. Nothing is recorded as a cancel request.

    Red on ``08cfe0f0`` (``done``): exit 0, ``ok: true``, "Too late to cancel: the target is committing. …"; the other
    rows exit 0 as well (``aborted``: "Job … is already aborted; nothing to cancel.").

    Mutants: ``EngineBackend.cancel`` returns the text without raising (every row red); ``cancel_answer`` answers a
    refused ``set_cancel`` before it checks for a finished job (``done`` names "committing")."""
    plugin, ledger = _engine_plugin(fake_host, tmp_path)
    job_id = _job(ledger, state, commit=commit)
    code = _cli(plugin, "cancel", job_id[-6:], "--json")
    payload = _payload(capsys)
    assert code == commands.EXIT_BUSY and payload["ok"] is False, payload
    assert payload["error"]["code"] == "invalid_state", payload
    text = payload["text"]
    assert says in text and job_id[-6:] in text, text
    if state != "committing":
        assert "committing." not in text and "is committing" not in text, text
    assert ledger.cancel_requested(job_id) is False
    assert ledger.job(job_id, Role.CONDUCTOR)["state"] == state


def test_g5_cancel_of_a_running_job_still_answers_ok(capsys, fake_host, tmp_path):
    """The positive case stays: a job before the commit point gets the request (exit 0, ``ok: true``)."""
    plugin, ledger = _engine_plugin(fake_host, tmp_path)
    job_id = _job(ledger, "packing", commit=False)
    assert _cli(plugin, "cancel", job_id, "--json") == commands.EXIT_OK
    payload = _payload(capsys)
    assert payload["ok"] is True and payload["text"].startswith("Cancel requested"), payload
    assert ledger.cancel_requested(job_id) is True


class _LateCtrlC(FakeBackend):
    """Ctrl+C during the foreground run while the job is already committing: the cancel is refused, the run goes on."""

    def __init__(self, **kw) -> None:
        super().__init__(**kw)
        self.runs = 0

    def run_foreground(self, job_id: str) -> int:
        self._rec("run_foreground", job_id=job_id)
        self.runs += 1
        if self.runs == 1:
            raise KeyboardInterrupt
        return 0

    def cancel(self, job_id: str) -> str:
        self._rec("cancel", job_id=job_id)
        raise InvalidState(
            "Too late to cancel handover JOB01: it is past its commit point (committing).",
            reason="committing",
            state="committing",
        )


def test_g5_ctrl_c_after_the_commit_point_lets_the_run_finish(capsys):
    """★ F22 follow-up: with ``EngineBackend.cancel`` now raising for a job that cannot be cancelled, the first Ctrl+C
    of a CLI ``take``/``to`` must not turn into an error: the refusal is shown and the foreground run continues to the
    job's end (the commit is not left without its runner).

    Mutant: the Ctrl+C branch calls ``backend.cancel`` without catching the refusal → exit 3, the second
    ``run_foreground`` never runs."""
    backend = _LateCtrlC(peers=[make_peer("server")])
    job = "01JC00000000000000000JOB01"
    backend.statuses[job] = {"job_id": job, "mode": "pull", "state": "done"}
    plugin, _b, _ = _plugin(backend)
    code = _cli(plugin, "take", "server", "--latest", "--yes", "--json")
    payload = _payload(capsys)
    assert backend.runs == 2 and "cancel" in backend.names(), backend.names()
    assert code == commands.EXIT_OK and payload["ok"] is True, payload
    assert "Too late to cancel" in payload["text"], payload
