"""JobRunner, janitor, gc and composition-root wiring (cluster A; TESTPLAN §2.2, DESIGN §3.6, BUILD-ORDER §6
item 21)."""

from __future__ import annotations

import contextvars
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.errors import BadRequest, InvalidState
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import (
    GateAuth,
    GateMode,
    Mode,
    Options,
    Peer,
    Right,
    Role,
    SessionInfo,
    SessionRef,
)
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.fakes.apis_fake import FakePlanning, SimulatedCrash, World
from ctx_handover.tests.unit.A.harness import FAST


class RunnerEnv:
    def __init__(self, tmp_path, clock, mode=Mode.PUSH):
        self.clock = clock
        self.ledger = Ledger(tmp_path / f"l-{ulid()}.sqlite3", clock=clock)
        self.world = World(clock)
        self.runner = jobs.JobRunner(
            self.ledger,
            None,
            apis_factory=lambda row: (self.world.source, self.world.target),
            planning=FakePlanning(),
            clock=clock,
            sleep=self.world.sleep,
            poll=FAST,
            keepalive=False,
        )
        self.mode = mode

    def start(self) -> str:
        job_id, final = self.runner.plan_sync(
            self.mode, None, Options(), session_ref=SessionRef(id="tip"), calling_session=None, session_key=None
        )
        assert final is not None
        return job_id

    def state(self, job_id) -> str:
        return self.ledger.job(job_id, Role.CONDUCTOR)["state"]

    def run_crashing(self, job_id) -> None:
        with pytest.raises(SimulatedCrash):
            self.runner.run_foreground(job_id)

    def janitor(self):
        self.world.faults.clear()
        self.clock.advance(61)
        return jobs.janitor(self.ledger, self.runner, None, flush_outbox=None, background=False)


@pytest.mark.parametrize(
    ("label", "side", "method", "kind", "expected", "source_after"),
    [
        ("<=negotiated", "target", "negotiate", "crash", "aborted", (None, "released")),
        ("locking..target_prepared", "source", "start_export", "crash", "suspended", ("locked",)),
        ("source_preparing", "source", "start_prepare", "crash", "aborted", ("released",)),
        ("committing", "target", "start_commit", "crash_after", "done", ("retired",)),
        ("retiring", "source", "start_retire", "crash", "done", ("retired",)),
    ],
)
def test_janitor_actions_table(tmp_path, clock, label, side, method, kind, expected, source_after):
    """★ Each conductor row of DESIGN §3.6: a job whose runner died is aborted (nothing locked or commit not
    sent), suspended (transfer phase) or driven forward (commit sent); the outcome promises hold.

    Mutant: ``Conductor.recover`` suspends jobs in SOURCE_PREPARING instead of aborting them."""
    e = RunnerEnv(tmp_path, clock)
    e.world.inject(side, method, kind)
    if label == "<=negotiated":
        job_id = None
        with pytest.raises(SimulatedCrash):
            e.runner.plan_sync(
                e.mode, None, Options(), session_ref=SessionRef(id="tip"), calling_session=None, session_key=None
            )
        job_id = e.ledger.jobs(role=Role.CONDUCTOR)[0]["job_id"]
    else:
        job_id = e.start()
        e.run_crashing(job_id)
    before = e.state(job_id)
    did = e.janitor()
    assert (job_id, before) in did["conductor"]
    assert e.state(job_id) == expected, (label, before, e.state(job_id))
    assert e.world.source.state in source_after
    assert e.world.violations == []
    if expected in ("done", "aborted"):
        e.world.assert_outcome(expected)
    e.ledger.close()


def test_janitor_never_releases_prepared_source(tmp_path, clock):
    """★ A stale participant source in ``prepared`` stays locked (fail-closed); a stale ``locked`` one is
    released (DESIGN §3.6).

    Mutant: ``ParticipantRegistry.recover`` releases ``prepared`` sources too."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    calls = []

    class Api:
        def start_release(self, job, reason):
            calls.append(("release", job.job_id))

        def start_retire(self, job, ack):
            calls.append(("retire", job.job_id))

    reg = ParticipantRegistry(led, lambda role, peer: Api(), clock=clock)
    for job_id, state, root in (("SP", "prepared", "r1"), ("SL", "locked", "r2"), ("SR", "retiring", "r3")):
        led.create_job(job_id, Role.SOURCE, Mode.PULL, "P1", {"role": "source"})
        info = SessionInfo("t" + root, root, (root, "t" + root), None, None, None, None, None, None, 1, (1,))
        led.source_lock(job_id, info, "h", "laptop")
        led.source_set(
            job_id,
            state,
            ack={
                "session_id": "n",
                "root_id": "n",
                "title": None,
                "dests": [],
                "path_map": [],
                "model": None,
                "offline_capable": True,
                "warnings": [],
                "committed_at": 1.0,
            },
        )
        led.transition(job_id, Role.SOURCE, "*", state, {}, epoch=0)
        assert led.claim_owner(job_id, Role.SOURCE, "dead-process", 60) is not None
    clock.advance(61)
    did = jobs.janitor(led, None, reg, flush_outbox=None)
    outcome = {j: what for j, _role, what in did["participant"]}
    assert outcome == {"SP": "kept", "SL": "released", "SR": "retire_continued"}
    assert ("release", "SP") not in calls and ("release", "SL") in calls and ("retire", "SR") in calls
    assert led.source_state("SP")["state"] == "prepared"
    led.close()


def test_janitor_purges_expired_codes_and_compacts_journal(tmp_path, clock):
    """Codes deleted on every janitor run, journals of terminal jobs > 30 days compacted (SEC 6.2, SK §5)."""
    e = RunnerEnv(tmp_path, clock)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    assert e.state(job_id) == "done"
    n_events = len(e.ledger.events(job_id, Role.CONDUCTOR))
    e.ledger.put_pair_code("OLD000", "s", frozenset({Right.PUSH}), "", clock() + 10)
    clock.advance(31 * 86400)
    did = jobs.janitor(e.ledger, e.runner, None, flush_outbox=None, background=False)
    assert did["codes"] == 1 and did["compacted"] >= n_events - 1
    assert len(e.ledger.events(job_id, Role.CONDUCTOR)) == 1
    assert e.ledger.pairing_pending(clock()) is False
    e.ledger.close()


def test_janitor_retries_retire_on_every_start(tmp_path, clock):
    """Pull with an unreachable source after the commit: the next janitor run (not only a stale one) retires."""
    e = RunnerEnv(tmp_path, clock, mode=Mode.PULL)
    e.world.inject("source", "start_retire", "disconnect", times=50)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    assert e.state(job_id) == "retiring"
    assert e.ledger.job(job_id, Role.CONDUCTOR)["owner"] is None
    e.world.faults.clear()
    jobs.janitor(e.ledger, e.runner, None, flush_outbox=None, background=False)
    assert e.state(job_id) == "done"
    e.world.assert_outcome("done")
    e.ledger.close()


def test_gc_purges_job_artifacts(tmp_path, clock):
    """``gc --job`` removes blocks, staging (with quarantine), report, log and journal; active jobs refuse
    (SEC 6.3)."""
    e = RunnerEnv(tmp_path, clock)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    data = tmp_path / "pd"
    (data / "staging" / job_id / "quarantine" / "w0").mkdir(parents=True)
    (data / "reports").mkdir(parents=True)
    (data / "reports" / f"{job_id}.md").write_text("r")
    (data / "logs").mkdir()
    (data / "logs" / f"continue-{job_id}.log").write_text("l")
    released = []
    store = SimpleNamespace(release_job=lambda jid, keep: released.append((jid, keep)))
    jobs.gc_job(e.ledger, job_id, data_dir=data, store=store)
    assert released == [(job_id, False)]
    assert not (data / "staging" / job_id).exists()
    assert not (data / "reports" / f"{job_id}.md").exists() and not (data / "logs" / f"continue-{job_id}.log").exists()
    assert e.ledger.job(job_id, Role.CONDUCTOR) is None and e.ledger.events(job_id, Role.CONDUCTOR) == []
    other = e.start()
    with pytest.raises(InvalidState):
        jobs.gc_job(e.ledger, other, data_dir=data, store=store)
    e.ledger.close()


def test_janitor_lazy_not_in_register(fake_host, fake_ctx, hermes_home):
    """``register()`` and importing the engine open nothing: no ledger, no data dir, no thread (BX L2, R12)."""
    import threading

    import ctx_handover
    from ctx_handover.engine import jobs as jobs_mod

    jobs_mod._reset_for_tests()
    threads = {t.name for t in threading.enumerate()}
    ctx_handover.register(fake_ctx)
    assert jobs_mod._PROFILES == {}
    assert not (hermes_home / "plugin-data").exists()
    assert {t.name for t in threading.enumerate()} == threads
    assert not any(name for name, _ in fake_host.calls if name in ("plugin_dir", "session_db", "load_config"))


def test_runner_threads_inherit_context_and_cancel(tmp_path, clock):
    e = RunnerEnv(tmp_path, clock)
    var: contextvars.ContextVar[str] = contextvars.ContextVar("profile", default="none")
    seen = []
    real = e.world.source.start_export

    def export(job):
        seen.append(var.get())
        return real(job)

    e.world.source.start_export = export
    job_id = e.start()
    token = var.set("work")
    try:
        e.runner.continue_async(job_id)
    finally:
        var.reset(token)
    e.runner.join(30)
    assert seen and seen[0] == "work"
    assert e.state(job_id) == "done"
    assert "Too late to cancel" in e.runner.cancel(job_id)
    st = e.runner.status(job_id)["job"]
    assert st["outcome"] == "done" and st["exit_code"] == 0 and st["ack"]["session_id"] == "new-tip"
    assert st["active"] is False and st["calling_session"] is None
    overview = e.runner.status(None)
    assert overview["jobs"][0]["job_id"] == job_id
    e.ledger.close()


def test_runner_plan_sync_end_without_lock(tmp_path, clock):
    from ctx_handover.engine.model import Include
    from ctx_handover.tests.fakes.apis_fake import make_item

    e = RunnerEnv(tmp_path, clock)
    e.world.source.items.append(make_item("x2", "/srv/x", include=Include.PROPOSED))
    job_id, final = e.runner.plan_sync(
        Mode.PUSH,
        None,
        Options(),
        session_ref=SessionRef(id="tip"),
        calling_session=None,
        session_key=None,
        confirm=lambda info: "cancel",
    )
    assert final is None and e.state(job_id) == "aborted"
    assert jobs.exit_code(e.ledger.job(job_id, Role.CONDUCTOR)) == jobs.EXIT_FAILED
    assert e.ledger.job(job_id, Role.CONDUCTOR)["owner"] is None
    e.ledger.close()


def test_exit_codes():
    def row(state, code=None):
        return {"state": state, "last_error": {"code": code} if code else None}

    assert jobs.exit_code(row("done")) == 0
    assert jobs.exit_code(row("aborted", "verify_failed")) == 1
    assert jobs.exit_code(row("aborted", "session_busy")) == 3
    assert jobs.exit_code(row("aborted", "gate_rejected")) == 4
    assert jobs.exit_code(row("suspended")) == 5
    assert jobs.exit_code(row("retiring")) == 5
    assert jobs.exit_code(None) == 2


def _transport(log):
    class Client:
        def __init__(self, peer, identity, *, read_secret):
            log.append(("client", peer.name))

    class RemoteSource:
        def __init__(self, client):
            self.client = client

    class RemoteTarget(RemoteSource):
        pass

    class BundleTarget:
        def __init__(self, out, identity, *, as_dir, declared_facts, ledger, retire=True, **kw):
            log.append(("bundle_target", str(out), as_dir, retire))

    class BundleSource:
        def __init__(self, path, *, trusted_pubkeys, trust_unsigned, ledger, again=False):
            log.append(("bundle_source", sorted(trusted_pubkeys), trust_unsigned, again))

    return SimpleNamespace(
        wire=SimpleNamespace(WireClient=Client),
        remote=SimpleNamespace(RemoteSource=RemoteSource, RemoteTarget=RemoteTarget),
        bundle=SimpleNamespace(BundleTarget=BundleTarget, BundleSource=BundleSource),
    )


def test_apis_for_wiring(tmp_path, clock, fake_host):
    """Mode → participant pair (DESIGN §2.2); bundle trust only from stored, non-revoked keys (SEC 1.6)."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    log: list = []
    t = _transport(log)
    src, tgt = object(), object()
    dial = Peer(
        name="server",
        peer_id="D1",
        direction="dial",
        url="https://srv",
        pubkey="pkD",
        rights=frozenset({Right.PULL, Right.PUSH}),
        gate=GateAuth(mode=GateMode.SESSION_TOKEN),
    )
    s, tt = jobs.apis_for(
        Mode.PUSH,
        dial,
        local_source=src,
        local_target=tgt,
        identity=object(),
        bundle_path=None,
        options=Options(),
        ledger=led,
        transport=t,
    )
    assert s is src and type(tt).__name__ == "RemoteTarget"
    s, tt = jobs.apis_for(
        Mode.PULL,
        dial,
        local_source=src,
        local_target=tgt,
        identity=object(),
        bundle_path=None,
        options=Options(),
        ledger=led,
        transport=t,
    )
    assert type(s).__name__ == "RemoteSource" and tt is tgt
    jobs.apis_for(
        Mode.EXPORT,
        None,
        local_source=src,
        local_target=tgt,
        identity=object(),
        bundle_path=Path("/b.ctxh.tar"),
        options=Options(as_dir=True),
        ledger=led,
        transport=t,
    )
    led.upsert_peer(dial)
    led.upsert_peer(
        Peer(name="lap", peer_id="I1", direction="inbound", url=None, pubkey="pkI", rights=frozenset({Right.PUSH}))
    )
    led.upsert_peer(
        Peer(name="old", peer_id="I2", direction="inbound", url=None, pubkey="pkOld", rights=frozenset({Right.PUSH}))
    )
    led.revoke_peer("old")
    jobs.apis_for(
        Mode.IMPORT,
        None,
        local_source=src,
        local_target=tgt,
        identity=None,
        bundle_path=Path("/b"),
        options=Options(),
        ledger=led,
        transport=t,
    )
    assert ("bundle_target", "/b.ctxh.tar", True, True) in log  # retire unless --keep (O3)
    assert ("bundle_source", ["pkD", "pkI"], False, False) in log
    with pytest.raises(BadRequest):
        jobs.apis_for(
            Mode.OFFER,
            dial,
            local_source=src,
            local_target=tgt,
            identity=object(),
            bundle_path=None,
            options=Options(),
            ledger=led,
            transport=t,
        )
    inbound = led.peer("lap")
    with pytest.raises(BadRequest):
        jobs.apis_for(
            Mode.PUSH,
            inbound,
            local_source=src,
            local_target=tgt,
            identity=object(),
            bundle_path=None,
            options=Options(),
            ledger=led,
            transport=t,
        )
    led.close()


def test_local_options_from_settings():
    o = jobs.local_options(
        {
            "allow_roots": ["~/w"],
            "export_roots": ["~/share"],
            "max_session_mb": 1,
            "staging_ttl_hours": 2,
            "participant_idle_timeout_min": 1,
            "refine": "off",
            "max_total_gb": True,
        }
    )
    assert o.allow_roots == ("~/w",) and o.export_roots == ("~/share",)
    assert o.max_session_bytes == 1024**2 and o.staging_ttl_s == 7200 and o.participant_idle_timeout_s == 60
    assert o.refine == "off" and o.max_total_bytes == 10 * 1024**3  # a bool is no number
    assert jobs.plugin_settings({"plugins": {"entries": {"ctx_handover": {"settings": {"a": 1}}}}}) == {"a": 1}
    assert jobs.plugin_settings({}) == {}
