"""``cancel --force`` on the surfaces (review round 4, finding R4-M1): the slash command and ``hermes handover`` accept
the flag, pass it to the engine only when it is given, and map the answer to the exit codes of DESIGN §14.2 — 0 when
the stuck job ended here, 3 (``invalid_state``) for every refusal; the text names what happened, never with a code
suffix.

The CLI runs through the real ``Plugin.cli`` → ``EngineBackend.cancel`` → ``jobs.JobRunner`` over a real ``Ledger``
and the fault-injection world of cluster A (a pull whose source on the peer no longer knows the job).

Red at ``ad5a5cfa``: "Unknown option --force for cancel" (exit 2) and a plain ``cancel`` of the stuck job exits 0 with
"Cancel requested for job …"."""

from __future__ import annotations

import json
import threading
import types

import pytest

from ctx_handover.engine import commands, surfaces
from ctx_handover.engine.commands import EngineBackend, Plugin
from ctx_handover.engine.errors import ImportFailed, NotFound
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Mode, Role
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend, make_peer
from ctx_handover.tests.unit.A.test_jobs import RunnerEnv

from .test_commands import _plugin, _slash
from .test_g3_f import _cli


def _engine_plugin(fake_host, env: RunnerEnv) -> Plugin:
    ctx = FakeCtx(host=fake_host)
    holder: dict = {}

    def factory(c, p):
        if "eb" not in holder:
            eb = EngineBackend(c, p)
            runtime = types.SimpleNamespace(
                ledger=env.ledger, store=None, veto=None, identity=None, identity_error=None, lock=threading.RLock()
            )
            eb._parts.update({"runtime": runtime, "runner": env.runner})
            holder["eb"] = eb
        return holder["eb"]

    return Plugin(ctx, backend_factory=factory)


def _payload(capsys) -> dict:
    return json.loads(capsys.readouterr().out.strip().splitlines()[-1])


def _stuck_pull(tmp_path, clock) -> tuple[RunnerEnv, str]:
    e = RunnerEnv(tmp_path, clock, mode=Mode.PULL)
    e.world.inject("target", "start_prepare", "error", error=ImportFailed("target prepare failed"))
    e.world.inject("source", "start_release", "error", error=NotFound("job not found"), times=10_000)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    assert e.state(job_id) == "aborting"
    return e, job_id


def test_r5_the_grammar_takes_force_for_cancel_only_as_a_flag():
    """``cancel [<job>] --force`` parses (also without a job); the usage line names it; the value before it is the
    job."""
    parsed = surfaces.parse("cancel 01JCJOB --force")
    assert parsed.errors == () and parsed.args["force"] is True and parsed.args["job"] == "01JCJOB", parsed
    bare = surfaces.parse("cancel --force")
    assert bare.errors == () and bare.args["force"] is True and bare.args["job"] is None, bare
    assert surfaces.parse("cancel 01JCJOB").args["force"] is False
    assert "--force" in surfaces.USAGE["cancel"]


def test_r5_cli_cancel_force_ends_a_stuck_job_and_exits_0(capsys, fake_host, tmp_path, clock):
    """★ R4-M1 through ``hermes handover``: a plain ``cancel --json`` of the stuck pull exits 3 (``invalid_state``,
    nothing new requested) and names ``--force``; ``cancel <job> --force --json`` exits 0, ``ok: true``, the job is
    ``aborted`` and the text names what may remain on the peer; a second ``--force`` exits 3 ("already aborted").

    Mutant ``R5-cli-no-flag`` (``_verb_cancel`` ignores ``force``): the forced call exits 3 with the plain answer."""
    e, job_id = _stuck_pull(tmp_path, clock)
    plugin = _engine_plugin(fake_host, e)
    sid = job_id[-6:]

    assert _cli(plugin, "cancel", sid, "--json") == commands.EXIT_BUSY
    plain = _payload(capsys)
    assert plain["ok"] is False and plain["error"]["code"] == "invalid_state", plain
    assert "--force" in plain["text"] and "(invalid_state)" not in plain["text"], plain

    assert _cli(plugin, "cancel", sid, "--force", "--json") == commands.EXIT_OK
    forced = _payload(capsys)
    assert forced["ok"] is True, forced
    assert "cancelled with --force" in forced["text"] and "/handover reclaim" in forced["text"], forced
    assert e.ledger.job(job_id, Role.CONDUCTOR)["state"] == "aborted"

    assert _cli(plugin, "cancel", sid, "--force", "--json") == commands.EXIT_BUSY
    again = _payload(capsys)
    assert "already aborted" in again["text"], again


@pytest.mark.parametrize("state", ["committed", "retired"])
def test_r5_cancel_force_of_a_committed_job_is_a_clear_refusal(capsys, fake_host, tmp_path, clock, state):
    """★ Never two active copies: ``--force`` on a committed or retired job exits 3 with a text that names the state,
    ``--force`` and ``/handover reclaim``; the job keeps its state (slash and CLI give the same answer)."""
    e = RunnerEnv(tmp_path, clock, mode=Mode.PUSH)
    job_id = ulid()
    e.ledger.create_job(job_id, Role.CONDUCTOR, Mode.PUSH, None, {"session_ref": {"id": "tip"}})
    assert e.ledger.intent_commit(job_id, 0)
    e.ledger.transition(job_id, Role.CONDUCTOR, "*", state, {}, epoch=0)
    plugin = _engine_plugin(fake_host, e)

    assert _cli(plugin, "cancel", job_id[-6:], "--force", "--json") == commands.EXIT_BUSY
    payload = _payload(capsys)
    assert payload["ok"] is False and payload["error"]["code"] == "invalid_state", payload
    text = payload["text"]
    assert f"past its commit point ({state})" in text and "also with --force" in text, text
    assert "/handover reclaim" in text and "(invalid_state)" not in text, text
    assert _slash(plugin, f"cancel {job_id[-6:]} --force") == text
    assert e.ledger.job(job_id, Role.CONDUCTOR)["state"] == state


class _Recording(FakeBackend):
    def cancel(self, job_id: str, **kw) -> str:
        self._rec("cancel", job_id=job_id, **kw)
        return f"Cancel requested for {job_id[-6:]}." if not kw else f"Handover {job_id[-6:]} cancelled with --force."


def test_r5_the_slash_passes_force_only_when_it_is_given():
    """The backend gets ``force=True`` only from ``--force``; without the flag the call is the old one-argument form
    (backends written before the flag keep working). On a messaging platform ``cancel --force`` is refused like
    ``cancel``."""
    backend = _Recording(peers=[make_peer("server")])
    plugin, _b, _ = _plugin(backend)
    job = "01JC00000000000000000JOB01"
    backend.statuses[job] = {"job_id": job, "mode": "pull", "state": "aborting", "active": True}
    assert "cancelled with --force" in _slash(plugin, f"cancel {job} --force")
    assert "Cancel requested" in _slash(plugin, f"cancel {job}")
    calls = [c for c in backend.calls if c[0] == "cancel"]
    assert [c[1] for c in calls] == [{"job_id": job, "force": True}, {"job_id": job}], calls
    assert _slash(plugin, f"cancel {job} --force", platform="telegram") == commands.REFUSAL
