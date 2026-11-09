"""Layer G (green) of the runtime part of cluster A: peer facts for ``export --for`` (V28), the bundle exporter as
the import's peer (V13), one participant pair per job (V24), route #9 naming a source without leases (V17), the
staging quota margin (R28, §1a), the settings of the listener (V22) and the success sweep over ``jobs``, ``ledger``,
``ops`` and ``participant``: a step counts as done only on an explicitly positive value."""

from __future__ import annotations

import dataclasses
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import jobs
from ctx_handover.engine.errors import LeaseLost, NotFound
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import from_wire, to_wire
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import (
    AuthContext,
    AuthScheme,
    EnvFacts,
    Mode,
    OpState,
    OpStatus,
    Options,
    Peer,
    Right,
    Role,
    SessionInfo,
    SessionRef,
)
from ctx_handover.engine.ops import OpRunner
from ctx_handover.engine.participant import ParticipantRegistry
from ctx_handover.tests.fakes.apis_fake import FakePlanning, World, make_facts
from ctx_handover.tests.unit.A.harness import FAST


def _dial(name: str = "server", peer_id: str | None = None, pubkey: str | None = None) -> Peer:
    return Peer(
        name=name,
        peer_id=peer_id or ulid(),
        direction="dial",
        url=f"https://{name}.example",
        pubkey=pubkey or f"pk-{name}",
        rights=frozenset({Right.PUSH, Right.PULL}),
    )


@pytest.fixture
def led(tmp_path, clock):
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    yield ledger
    ledger.close()


# ---------------------------------------------------------------------------------------------------------
# V28: the ledger keeps the facts last received from a peer
# ---------------------------------------------------------------------------------------------------------


def test_peer_facts_are_kept_per_peer_replaced_and_dropped_with_the_peer(tmp_path, clock):
    """★ ``peer_facts_set``/``peer_facts_get`` (contract V28): the newest facts per peer, durable across ledger
    objects (another process), a fresh dict per read; removing the dial peer drops them; a missing peer id or facts
    that are no mapping are refused.

    Mutant: ``remove_peer`` keeps the ``peer_facts`` row → a re-paired peer with the same id inherits old facts."""
    path = tmp_path / "ledger.sqlite3"
    led = Ledger(path, clock=clock)
    peer = _dial()
    led.upsert_peer(peer)
    first = to_wire(make_facts("server"))
    led.peer_facts_set(peer.peer_id, first)
    got = led.peer_facts_get(peer.peer_id)
    assert got == first
    got["instance_name"] = "changed by the reader"
    assert led.peer_facts_get(peer.peer_id) == first, "a read hands out a copy"
    newer = {**first, "workspace_root": "/srv/handover"}
    led.peer_facts_set(peer.peer_id, newer)
    other = Ledger(path, clock=clock)
    assert other.peer_facts_get(peer.peer_id) == newer
    assert other.peer_facts_get(ulid()) is None
    other.close()
    with pytest.raises(TypeError):
        led.peer_facts_set("", first)
    with pytest.raises(TypeError):
        led.peer_facts_set(peer.peer_id, [first])  # type: ignore[arg-type]
    assert led.remove_peer(peer.name) is True
    assert led.peer_facts_get(peer.peer_id) is None
    assert led.remove_peer(peer.name) is False
    led.close()


def _transport(log: list) -> SimpleNamespace:
    def bundle_target(path, identity, **kw):
        log.append(kw)
        return SimpleNamespace(kind="bundle_target")

    return SimpleNamespace(
        bundle=SimpleNamespace(BundleTarget=bundle_target, BundleSource=None),
        remote=None,
        wire=None,
    )


def _export(led: Ledger, peer: Peer | None, log: list) -> dict:
    src = SimpleNamespace(instance_name="laptop", host=None)
    jobs.apis_for(
        Mode.EXPORT,
        peer,
        local_source=src,
        local_target=None,
        identity=object(),
        bundle_path=Path("/out/x.ctxh.tar"),
        options=Options(),
        ledger=led,
        transport=_transport(log),
    )
    return log[-1]


def test_export_for_a_peer_hands_its_last_known_facts_to_the_bundle(led):
    """★ ``export --for <peer>`` (V28): ``apis_for`` gives ``BundleTarget`` the facts stored for that peer as its
    declared target facts; without a peer, without stored facts or with facts that no longer parse the bundle keeps
    the generic facts of an unknown importer (``None``).

    Mutant: ``declared_facts=None`` in ``apis_for`` → the stored facts never reach the export."""
    log: list = []
    peer = _dial()
    led.upsert_peer(peer)
    assert _export(led, peer, log)["declared_facts"] is None  # nothing received yet
    facts = make_facts("server")
    led.peer_facts_set(peer.peer_id, to_wire(facts))
    declared = _export(led, peer, log)["declared_facts"]
    assert isinstance(declared, EnvFacts) and declared == from_wire(EnvFacts, to_wire(facts))
    assert _export(led, None, log)["declared_facts"] is None
    led.peer_facts_set(peer.peer_id, {"instance_name": "half"})
    assert _export(led, peer, log)["declared_facts"] is None


# ---------------------------------------------------------------------------------------------------------
# V13: a verified bundle binds its exporter as the import's peer
# ---------------------------------------------------------------------------------------------------------


def _import_row(job_id: str) -> dict:
    return {
        "job_id": job_id,
        "mode": str(Mode.IMPORT),
        "peer_id": None,
        "params": {"options": to_wire(Options()), "bundle_path": "/in/b.ctxh.tar"},
    }


@pytest.mark.parametrize(
    ("verified", "signer", "bound"),
    [
        (True, "server", True),
        ("yes", "server", False),  # a truthy non-bool is no verification
        (True, "trusted key", False),  # this profile's own key: no peer
        (True, "nobody", False),
        (True, "revoked", False),
        (False, None, False),
    ],
)
def test_import_binds_only_the_verified_paired_exporter(runtime_env, monkeypatch, verified, signer, bound):
    """★ V13 + sweep: the composition root binds the exporter of an imported bundle as the job's source peer (its
    workspace-map rows, the return trip) only when ``BundleSource.verified is True`` and the signer is a stored,
    non-revoked peer; ``peer_verified`` and the journaled origin follow the same explicit ``True``.

    Mutants: ``_exporter`` without the revocation check (row ``revoked`` binds); ``peer_verified =
    bool(verified)`` (row ``"yes"`` binds)."""
    rt = jobs.ensure_started(None)
    server = _dial("server")
    rt.ledger.upsert_peer(server)
    gone = _dial("revoked")
    rt.ledger.upsert_peer(gone)
    rt.ledger.revoke_peer(gone.name)
    target = SimpleNamespace(peer_verified=None, source_peer_id=None, peer_name=None)
    bundle_source = SimpleNamespace(verified=verified, signer=signer)
    monkeypatch.setattr(jobs, "make_local_apis", lambda *a, **kw: (object(), target))
    monkeypatch.setattr(jobs, "apis_for", lambda *a, **kw: (bundle_source, target))
    runner = jobs.JobRunner(rt.ledger, None, keepalive=False)
    job_id = ulid()
    rt.ledger.create_job(job_id, Role.CONDUCTOR, Mode.IMPORT, None, _import_row(job_id)["params"])
    runner._claim(job_id)
    runner._default_apis(_import_row(job_id))
    assert target.peer_verified is (verified is True)
    want = (server.peer_id, server.name) if bound else (None, None)
    assert (target.source_peer_id, target.peer_name) == want
    (origin,) = [e for e in rt.ledger.events(job_id, Role.CONDUCTOR) if e["kind"] == "origin"]
    assert origin["data"]["origin"].startswith("verified" if verified is True else "unverified")


# ---------------------------------------------------------------------------------------------------------
# V24: plan_sync and the run drive one participant pair
# ---------------------------------------------------------------------------------------------------------


def test_plan_sync_and_the_run_use_one_participant_pair(tmp_path, clock):
    """★ V24 (A part): ``plan_sync`` and ``run_foreground`` of a job compose its participants once — the instance
    that negotiated (a ``BundleTarget`` holding its FinalPlan) is the one that commits; the pair is dropped when the
    run ends, so a later ``resume`` composes afresh.

    Mutant: ``_participants`` calls the factory on every ``_conductor`` → two compositions per job."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    world = World(clock)
    built: list[str] = []

    def factory(row):
        built.append(row["job_id"])
        return world.source, world.target

    runner = jobs.JobRunner(
        led,
        None,
        apis_factory=factory,
        planning=FakePlanning(),
        clock=clock,
        sleep=world.sleep,
        poll=FAST,
        keepalive=False,
    )
    job_id, final = runner.plan_sync(
        Mode.PUSH, None, Options(), session_ref=SessionRef(id="tip"), calling_session=None, session_key=None
    )
    assert final is not None
    assert runner.run_foreground(job_id) == jobs.EXIT_OK
    assert built == [job_id]
    assert job_id not in runner._pairs
    led.close()


# ---------------------------------------------------------------------------------------------------------
# V17: route #9 names the state of a source that gave up its leases
# ---------------------------------------------------------------------------------------------------------


class _SourceApi:
    def __init__(self, ledger: Ledger, calls: list) -> None:
        self.ledger, self.calls = ledger, calls
        self.lose_lease = False

    def heartbeat(self, job) -> None:
        self.calls.append(job.job_id)
        state = self.ledger.source_state(job.job_id)["state"]
        if self.lose_lease or state in ("reclaimed", "released", "suspended"):
            raise LeaseLost(f"the source is {state}")


def _pull_source(led: Ledger, reg: ParticipantRegistry, peer: Peer) -> str:
    job_id = ulid()
    auth = AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=peer.rights, epoch=1)
    reg.open(auth, {"job_id": job_id, "epoch": 1, "role": "source", "mode": "pull", "options": {}})
    info = SessionInfo("tip", "root", ("root", "tip"), None, None, None, None, None, None, 1, (1,))
    led.source_lock(job_id, info, "holder", peer.name)
    led.source_set(job_id, "locked")
    return job_id


@pytest.mark.parametrize("lost", ["reclaimed", "released", "suspended", "releasing"])
def test_heartbeat_names_the_state_of_a_source_without_leases(led, clock, lost):
    """★ Route #9 answers ``{state}`` of the participant (DESIGN §4.7): a source that holds its leases answers its
    handover state (``locked``) after the keepalive; one that gave them up answers that state (``reclaimed`` …)
    without a keepalive, so the dialer's ``RemoteSource`` raises ``LeaseLost(state=…)``; a lost turn lease while the
    source still counts as locked stays an error.

    Mutant: ``heartbeat`` answers the job row's state → ``open`` for every source, the dialer never learns it."""
    calls: list = []
    api = _SourceApi(led, calls)
    reg = ParticipantRegistry(led, lambda role, peer: api, clock=clock)
    peer = Peer(name="laptop", peer_id=ulid(), direction="inbound", url=None, pubkey="pk", rights=frozenset(Right))
    led.upsert_peer(peer)
    job_id = _pull_source(led, reg, peer)
    auth = AuthContext(scheme=AuthScheme.PEER_SIGNATURE, peer=peer, rights=peer.rights, epoch=1)
    assert reg.heartbeat(auth, job_id, {"epoch": 1}) == {"state": "locked"}
    assert calls == [job_id]
    api.lose_lease = True
    with pytest.raises(LeaseLost):
        reg.heartbeat(auth, job_id, {"epoch": 1})
    api.lose_lease = False
    led.source_set(job_id, lost)
    calls.clear()
    assert reg.heartbeat(auth, job_id, {"epoch": 1}) == {"state": lost}
    assert calls == []


# ---------------------------------------------------------------------------------------------------------
# R28 §1a: the staging quota of a job is the negotiated need plus 10 %
# ---------------------------------------------------------------------------------------------------------


def test_negotiation_sets_the_staging_quota_to_the_need_plus_ten_percent(tmp_path):
    """★ R28 / DESIGN §4.7 (contract §1a): ``LocalTarget.negotiate`` registers the job's staging quota as
    ``need_bytes`` of the confirmed plan plus ``QUOTA_MARGIN`` (10 %), nothing charged yet — a peer can store at most
    that much for the job.

    Mutant: ``QUOTA_MARGIN = 1.0`` in ``local_target`` → the quota equals the bare need."""
    from ctx_handover.engine import local_target
    from ctx_handover.engine.contracts import JobContext
    from ctx_handover.engine.model import Include, ItemKind, ItemRole, Origin, PlanItem, RefineReport, ResourcePlan
    from ctx_handover.tests.conftest import require_upstream
    from ctx_handover.tests.fakes.apis_fake import RecordingReporter
    from ctx_handover.tests.integration.harness import Home

    require_upstream()
    jobs._reset_for_tests()
    try:
        home = Home(tmp_path, "server")
        with home.active():
            _source, target = jobs.make_local_apis(None, calling_session=None, session_key=None)
            job = JobContext(
                job_id=ulid(),
                mode=Mode.PUSH,
                peer=None,
                options=Options(),
                epoch=1,
                reporter=RecordingReporter(),
                cancelled=lambda: False,
                created_at=0.0,
            )
            item = PlanItem(
                item_id="w0",
                kind=ItemKind.FILE_TREE,
                source_path="/laptop/home/proj",
                role=ItemRole.PRIMARY,
                origin=Origin.CWD,
                include=Include.YES,
                reasons=("cwd",),
                size_estimate=10_000_000,
            )
            plan = ResourcePlan(
                job_id=job.job_id,
                revision=(1,),
                items=(item,),
                not_transferred=(),
                processes=(),
                delegations=(),
                cron=(),
                refine=RefineReport(used="off"),
                tools_needed=(),
                env_vars_needed=(),
                notes_for_target="",
                approx_tokens=100,
            )
            target.open_job(job)
            target.facts(job, make_facts("laptop"))
            final = target.negotiate(job, plan)
            quota = home.ledger().quota_get(job.job_id, Role.TARGET)
    finally:
        jobs._reset_for_tests()
    assert pytest.approx(1.1) == local_target.QUOTA_MARGIN
    assert final.need_bytes > 0
    assert quota == (int(final.need_bytes * 1.1), 0)


# ---------------------------------------------------------------------------------------------------------
# V22: the listener's settings
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def test_listener_settings_take_schema_keys_normalised_and_keep_their_own_knobs(runtime_env):
    """★ V22: ``server_settings`` = the one normaliser for every schema key (an unknown ``inbound`` word is ``off``,
    fail-closed — integrator decision, gp-k0) plus the listener's own knobs outside the schema as given (``name``,
    ``operator_api``, rate limits — the server checks them); job caps outside the schema are positive integers or the
    default. An empty ``allow_roots`` stays empty in ``Options`` (fail-closed, as the surfaces show it).

    Mutant: ``server_settings`` builds only from ``plugin_settings`` → ``operator_api`` and ``name`` vanish."""
    runtime_env.config.setdefault("plugins", {}).setdefault("entries", {})["ctx_handover"] = {
        "settings": {
            "inbound": "nonsense",
            "name": "gw-b",
            "operator_api": True,
            "rate_per_s": 5,
            "max_jobs_per_peer": 0,
            "max_jobs_total": 7,
            "max_request_mb": -3,
        }
    }
    s = jobs.server_settings(runtime_env.config)
    assert s["inbound"] == "off" and s["max_request_mb"] == 8
    assert (s["name"], s["operator_api"], s["rate_per_s"]) == ("gw-b", True, 5)
    assert (s["max_jobs_per_peer"], s["max_jobs_total"]) == (2, 7)
    assert jobs.local_options({"allow_roots": []}).allow_roots == ()
    assert jobs.local_options({}).allow_roots == ("~/hermes-handover",)


# ---------------------------------------------------------------------------------------------------------
# success sweep: only an explicitly positive value counts as done
# ---------------------------------------------------------------------------------------------------------


def test_job_update_of_a_missing_job_is_not_found(led):
    """★ ``Ledger.job_update`` without an epoch on a job row that does not exist raises ``NotFound`` instead of
    reporting an error or result as recorded (the UPDATE touched no row).

    Mutant: no ``rowcount`` check → the call returns silently."""
    with pytest.raises(NotFound):
        led.job_update(ulid(), Role.TARGET, last_error={"code": "import_failed"})
    job_id = ulid()
    led.create_job(job_id, Role.TARGET, Mode.PUSH, None, {})
    led.job_update(job_id, Role.TARGET, last_error={"code": "import_failed"})
    assert led.job(job_id, Role.TARGET)["last_error"] == {"code": "import_failed"}


def test_a_failed_operation_restarts_only_on_an_explicit_retryable_true(led):
    """★ ``OpRunner.start`` runs a failed operation again only when its error says ``retryable: true`` (a bool);
    a string ``"false"`` (from a peer or an older row) is no permission to repeat the steps.

    Mutant: ``bool(error.get("retryable"))`` → the string ``"false"`` restarts the operation."""
    runner = OpRunner(led, inline=True)
    ran: list[str] = []
    job_id = ulid()

    def fn(report):
        ran.append("x")
        return {"ok": 1}

    for digest, retryable in (("d-str", "false"), ("d-true", True)):
        led.op_put(job_id, "export", digest, ulid(), OpState.FAILED, error={"code": "x", "retryable": retryable})
    assert runner.start(job_id, "export", "d-str", fn).state is OpState.FAILED and ran == []
    assert runner.start(job_id, "export", "d-true", fn).state is OpState.DONE and ran == ["x"]


def test_an_operation_without_a_result_mapping_fails(led):
    """★ An operation's function returns a mapping (``OpFn``); ``None`` is an empty result, anything else is no
    confirmed outcome — the operation fails instead of being recorded as done.

    Mutant: ``dict(result or {})`` without the type check → the list is turned into a dict (or raises inside the
    worker and leaves the row ``running``)."""
    runner = OpRunner(led, inline=True)
    job_id = ulid()
    bad = runner.start(job_id, "pack", "d1", lambda report: [("pack_id", "p1")])
    assert bad.state is OpState.FAILED and "not a result mapping" in bad.error["message"]
    assert runner.start(job_id, "pack", "d2", lambda report: None).result == {}


def test_cancel_of_a_finished_job_says_that_nothing_was_requested(led):
    """★ ``JobRunner.cancel`` says "Cancel requested" only when the ledger holds the request; ``set_cancel`` keeps
    none for a job that already ended, so the answer names its state.

    Mutant: the old unconditional "Cancel requested for job …" → the user is told a finished job was cancelled."""
    runner = jobs.JobRunner(led, None, keepalive=False)
    active, done = ulid(), ulid()
    for job_id in (active, done):
        led.create_job(job_id, Role.CONDUCTOR, Mode.PUSH, None, {})
    led.transition(done, Role.CONDUCTOR, "*", "done", {}, epoch=0)
    assert runner.cancel(active).startswith("Cancel requested") and led.cancel_requested(active) is True
    answer = runner.cancel(done)
    assert "already done" in answer and "requested" not in answer
    assert led.cancel_requested(done) is False


@pytest.mark.parametrize(("op_state", "want"), [(OpState.DONE, "released"), (OpState.RUNNING, "release_started")])
def test_recover_reports_released_only_for_a_finished_release(led, clock, op_state, want):
    """★ The janitor's recovery of a dead participant source reports ``released`` only when the release operation
    finished; a release still running is ``release_started`` (it is not confirmed yet).

    Mutant: ``recover`` returns ``"released"`` after starting the operation, whatever its state."""

    class Api:
        def start_release(self, job, reason):
            return OpStatus(op_id=ulid(), state=op_state)

    reg = ParticipantRegistry(led, lambda role, peer: Api(), clock=clock)
    job_id = ulid()
    led.create_job(job_id, Role.SOURCE, Mode.PULL, None, {"role": "source"})
    info = SessionInfo("tip", "root", ("root", "tip"), None, None, None, None, None, None, 1, (1,))
    led.source_lock(job_id, info, "holder", "laptop")
    led.source_set(job_id, "locked")
    assert reg.recover(led.job(job_id, Role.SOURCE)) == want


def test_known_facts_are_rebuilt_as_env_facts():
    """``jobs.known_facts`` returns ``EnvFacts`` (what ``BundleTarget`` compares and negotiates with)."""
    facts = make_facts("server")
    stub = SimpleNamespace(peer_facts_get=lambda peer_id: to_wire(facts))
    got = jobs.known_facts(stub, _dial())  # type: ignore[arg-type]
    assert dataclasses.is_dataclass(got) and got.instance_name == facts.instance_name


def test_status_names_what_stayed_behind_and_what_the_source_withheld(led):
    """★ V34/V29/V36 in ``status``: the conductor's ``left_behind`` (pack exclusions, dropped suspected secrets)
    joins ``exclusions``; secrets go to ``excluded_secrets``, whole stash entries to ``withheld_stash``; tracked files
    on the secrets list (``denied_tracked`` of an included repository) are named with their absolute path;
    ``withheld`` lists the paths the source did not release; ``source_kept`` marks an export that left the source
    active (a copy). Each path once.

    Mutant: ``_summary`` without ``_left_behind`` → ``.env`` and the stash entry are named nowhere in the status."""
    runner = jobs.JobRunner(led, None, keepalive=False)
    job_id = ulid()
    led.create_job(job_id, Role.CONDUCTOR, Mode.EXPORT, None, {})
    led.transition(job_id, Role.CONDUCTOR, "*", "done", {}, epoch=0)
    plan = {
        "plan": {
            "items": [
                {
                    "item_id": "w0",
                    "source_path": "/home/u/proj",
                    "reasons": [],
                    "git": {"denied_tracked": [".env.local"]},
                }
            ],
            "not_transferred": [{"path": "/home/u/proj/id_ed25519", "why": "secret"}],
        },
        "decisions": [],
    }
    left = [
        {"path": "/home/u/proj/.env", "why": "secret", "item_id": "w0"},
        {"path": "/home/u/proj/settings.txt", "why": "suspected_secret", "item_id": "w0"},
        {"path": "stash@{0} (wip): .env", "why": "stash_secret", "item_id": "w0"},
        {"path": "/home/u/proj/.env", "why": "secret", "item_id": "w0"},
        {"why": "secret"},
    ]
    result = {"outcome": "done", "plan": plan, "left_behind": left, "withheld": ["/srv/private/keep.txt", 3]}
    led.job_update(job_id, Role.CONDUCTOR, result={**result, "source_kept": True})
    job = runner.status(job_id)["job"]
    paths = [e["path"] for e in job["exclusions"]]
    assert paths == [
        "/home/u/proj/id_ed25519",
        "/home/u/proj/.env.local",
        "/home/u/proj/.env",
        "/home/u/proj/settings.txt",
        "stash@{0} (wip): .env",
    ]
    assert job["excluded_secrets"] == ["/home/u/proj/id_ed25519", "/home/u/proj/.env", "/home/u/proj/settings.txt"]
    assert job["withheld_stash"] == ["stash@{0} (wip): .env"]
    assert job["withheld"] == ["/srv/private/keep.txt"]
    assert job["source_kept"] is True
    led.job_update(job_id, Role.CONDUCTOR, result={**result, "source_kept": "yes"})
    assert runner.status(job_id)["job"]["source_kept"] is False
