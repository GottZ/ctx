"""Layer-2 composition roots against the real modules (contract X19, X20, X21, X23, W055/W056, W110, R28):
one runtime per profile and process, the runner's reporter factory, settings normalisation, verified peers and
bundles, unbuildable jobs, the janitor's block-store GC, ``gc_job`` order and the listener's identity."""

from __future__ import annotations

import os
import time
from dataclasses import replace
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import blocks, crypto, jobs
from ctx_handover.engine import ledger as ledger_mod
from ctx_handover.engine.errors import Unauthorized
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Options, Peer, Right, Role, SessionRef


@pytest.fixture
def runtime_env(fake_host):
    jobs._reset_for_tests()
    yield fake_host
    jobs._reset_for_tests()


def _settings(host, **values) -> None:
    host.config.setdefault("plugins", {}).setdefault("entries", {})["ctx_handover"] = {"settings": dict(values)}


# ---------------------------------------------------------------------------------------------------------
# X19: one runtime
# ---------------------------------------------------------------------------------------------------------


def test_runtime_is_one_per_profile_and_process(runtime_env, fake_ctx, monkeypatch):
    """★ X19/W109: ``ensure_started`` returns the profile's one ``Runtime``; ``runner_for`` hands out its runner;
    the veto index reads the runtime's own ledger — one ``Ledger`` object in the process, however often and from
    wherever the runtime is reached.

    Mutant: ``ensure_started`` builds ``lock.VetoIndex(path)`` without ``ledger_factory`` (the layer-1 wiring) →
    the index opens a second ``Ledger`` on the same file."""
    made: list[Path] = []
    real_init = ledger_mod.Ledger.__init__

    def counting(self, path, **kw):
        made.append(Path(path))
        real_init(self, path, **kw)

    monkeypatch.setattr(ledger_mod.Ledger, "__init__", counting)
    rt = jobs.ensure_started(fake_ctx)
    assert isinstance(rt, jobs.Runtime)
    assert jobs.ensure_started(None) is rt and jobs.ensure_started(fake_ctx) is rt
    assert rt.runner is not None and jobs.runner_for(fake_ctx) is rt.runner and jobs.runner_for() is rt.runner
    assert rt.store is not None and rt.veto is not None and rt.ctx is fake_ctx
    assert rt.veto.refresh_now(force=True)  # the index works over the runtime's ledger
    deadline = time.time() + 5
    while rt.veto.refreshed_at is None and time.time() < deadline:
        time.sleep(0.05)
    assert made == [rt.ledger.path], f"ledger objects in this process: {made}"


def test_runner_started_without_a_factory_takes_the_surfaces_one(runtime_env, fake_ctx):
    """★ (finding L2-F) A route, the CLI or the janitor may create the profile's runner before the surfaces: the
    surfaces' reporter factory is bound afterwards (the first real factory stays), so their progress arrives.

    Mutant: ``runner_for`` ignores the factory when the runner exists (the layer-1 code) → the runner keeps the
    null factory."""
    rt = jobs.ensure_started(None)
    runner = rt.runner
    seen: list[str] = []

    def factory(job_id):
        seen.append(job_id)
        return jobs._NullReporter()

    assert jobs.runner_for(fake_ctx, factory) is runner
    runner._reporter_factory("J1")
    assert seen == ["J1"]
    assert runner.bind_reporter_factory(lambda job_id: jobs._NullReporter()) is False
    runner._reporter_factory("J2")
    assert seen == ["J1", "J2"]


def test_janitor_runs_once_per_process_start_and_before_jobs(runtime_env, monkeypatch):
    """DESIGN §3.6: the janitor runs on the first use in a process (not again on later ``ensure_started``) and
    before every new job (``JobRunner(before_job=Runtime.janitor)``)."""
    calls: list[dict] = []
    monkeypatch.setattr(jobs, "janitor", lambda *a, **kw: calls.append(kw) or {})
    rt = jobs.ensure_started(None)
    jobs.ensure_started(None)
    assert len(calls) == 1 and calls[0]["store"] is rt.store
    rt.runner._before_job()
    assert len(calls) == 2


# ---------------------------------------------------------------------------------------------------------
# settings
# ---------------------------------------------------------------------------------------------------------


def test_settings_yaml_words_and_flags(runtime_env, fake_ctx):
    """A bare YAML ``off`` arrives as ``False``: ``refine: off`` must stay off (no LLM call the user disabled),
    ``ctx_persist: off`` must stop the ctx outbox; quoted words in boolean settings are not truthy strings
    (``allow_remote_continue: "false"`` stays off)."""
    raw = {
        "refine": False,
        "extra_paths": False,
        "ctx_persist": False,
        "inbound": False,
        "model_mapping": True,
        "allow_remote_continue": "false",
        "include_hermes_files": "no",
        "refine_auto_include": "yes",
        "allow_agent_start": "maybe",
    }
    s = jobs.plugin_settings({"plugins": {"entries": {"ctx_handover": {"settings": raw}}}})
    assert s["refine"] == "off" and s["extra_paths"] == "off" and s["ctx_persist"] == "off" and s["inbound"] == "off"
    assert s["allow_remote_continue"] is False and s["include_hermes_files"] is False
    assert s["refine_auto_include"] is True and "allow_agent_start" not in s
    o = jobs.local_options(s)
    assert o.refine == "off" and o.extra_paths == "off" and o.include_hermes_files is False
    assert o.model_mapping == "auto"
    assert (
        jobs.local_options(
            jobs.plugin_settings({"plugins": {"entries": {"ctx_handover": {"settings": {"refine": True}}}}})
        ).refine
        == "auto"
    )
    runtime_env.config["memory"] = {"provider": "ctx_checkpoint"}
    _settings(runtime_env, ctx_persist="auto")
    assert jobs._flush(fake_ctx) is not None
    _settings(runtime_env, ctx_persist=False)
    assert jobs._flush(fake_ctx) is None


def test_listener_settings_and_registry_caps(runtime_env):
    """W055/X18: the listener's settings carry ``hosted``, versions, name and the job caps; the registry (the one
    owner of the caps) gets the same values."""
    _settings(runtime_env, max_jobs_per_peer=3, max_jobs_total=0)
    s = jobs.server_settings(runtime_env.config)
    assert s["max_jobs_per_peer"] == 3 and s["max_jobs_total"] == 4  # 0 is no cap: the default applies
    assert {"inbound", "name", "max_request_mb", "hosted", "plugin_version", "hermes_version"} <= set(s)
    rt = jobs.ensure_started(None)
    reg = jobs._registry(rt)
    assert (reg._max_per_peer, reg._max_jobs) == (3, 4)


# ---------------------------------------------------------------------------------------------------------
# X21: verified peers and bundles
# ---------------------------------------------------------------------------------------------------------


def test_peer_verified_only_for_signature_checked_peers():
    """X21: verified = paired, not revoked, with a stored key; no peer (bundle, local) is not verified."""
    p = Peer(name="s", peer_id="P", direction="dial", url="https://s", pubkey="pk", rights=frozenset({Right.PULL}))
    assert jobs._peer_verified(p) is True
    assert jobs._peer_verified(replace(p, direction="inbound", url=None)) is True
    assert jobs._peer_verified(replace(p, revoked_at=1.0)) is False
    assert jobs._peer_verified(replace(p, pubkey="")) is False
    assert jobs._peer_verified(None) is False


def test_import_is_verified_only_by_the_bundle_signature(runtime_env, monkeypatch, tmp_path):
    """★ X21: an import's target is ``peer_verified`` exactly when the bundle carries a signature of a stored,
    non-revoked key (``BundleSource.verified``) — it decides whether a continuation may start; the origin the
    status shows comes from the same check.

    Mutant: ``_default_apis`` leaves ``peer_verified`` as composed (``peer is not None`` / no peer ⇒ False) → a
    signed bundle is never verified."""

    class FakeBundleSource:
        def __init__(self, path, *, trusted_pubkeys, trust_unsigned, ledger, again=False):
            self.verified = Path(path).name == "signed"
            self.signer = "server" if self.verified else None

    transport = SimpleNamespace(bundle=SimpleNamespace(BundleSource=FakeBundleSource))
    monkeypatch.setattr(jobs, "_default_transport", lambda: transport)
    rt = jobs.ensure_started(None)
    runner = rt.runner
    for name, verified, origin in (("signed", True, "verified bundle (server)"), ("plain", False, "unverified bundle")):
        job_id = ulid()
        params = {"options": to_wire(Options(trust_unsigned=True)), "bundle_path": str(tmp_path / name)}
        rt.ledger.create_job(job_id, Role.CONDUCTOR, Mode.IMPORT, None, params)
        runner._claim(job_id)
        src, tgt = runner._default_apis(rt.ledger.job(job_id, Role.CONDUCTOR))
        assert isinstance(src, FakeBundleSource)
        assert tgt.peer_verified is verified
        assert runner._origin(rt.ledger.job(job_id, Role.CONDUCTOR)) == origin
        runner._release(job_id)


def test_job_whose_participants_cannot_be_composed_ends_aborted(tmp_path, clock):
    """★ A refused bundle (or a removed peer, a missing identity) fails while the job's participants are composed:
    the job ends ``aborted`` with the error and releases its owner — it does not stay ``created`` and active
    forever (the janitor would retry it on every run, ``status`` would list it).

    Mutant: ``plan_sync`` without ``_abort_unbuilt`` → the job stays ``created``."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)

    def refuse(row):
        raise Unauthorized("the bundle is not signed by a paired, non-revoked peer")

    runner = jobs.JobRunner(led, None, apis_factory=refuse, keepalive=False)
    with pytest.raises(Unauthorized):
        runner.plan_sync(
            Mode.IMPORT,
            None,
            Options(),
            session_ref=SessionRef(),
            calling_session=None,
            session_key=None,
            bundle_path=tmp_path / "x.ctxh.tar",
        )
    (row,) = led.jobs(role=Role.CONDUCTOR, limit=5)
    assert row["state"] == "aborted" and row["owner"] is None
    assert row["last_error"]["code"] == "unauthorized" and jobs.exit_code(row) == jobs.EXIT_AUTH
    assert led.jobs(role=Role.CONDUCTOR, active=True) == []
    led.close()


def test_recover_of_an_uncomposable_stale_job_aborts_it(tmp_path, clock):
    """The janitor's recovery of a stale, unlocked job whose participants can no longer be composed ends it."""
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    job_id = ulid()
    led.create_job(job_id, Role.CONDUCTOR, Mode.PUSH, "gone", {"options": to_wire(Options())})

    def refuse(row):
        raise Unauthorized("the peer of this job was removed")

    runner = jobs.JobRunner(led, None, apis_factory=refuse, keepalive=False)
    runner.recover(job_id, background=False)
    row = led.job(job_id, Role.CONDUCTOR)
    assert row["state"] == "aborted" and row["owner"] is None
    led.close()


# ---------------------------------------------------------------------------------------------------------
# X20: status form
# ---------------------------------------------------------------------------------------------------------


def test_status_summary_has_the_contract_keys(tmp_path, clock):
    """X20: ``status()`` → ``{"jobs", "pull_log", "outbox_pending"}``, ``status(id)`` → ``{"job"}``; the summary
    carries every key of the contract (``None``/empty when unknown) next to the existing ones."""
    from ctx_handover.tests.unit.A.test_jobs import RunnerEnv

    e = RunnerEnv(tmp_path, clock)
    job_id = e.start()
    e.runner.run_foreground(job_id)
    overview = e.runner.status(None)
    assert set(overview) == {"jobs", "pull_log", "outbox_pending"}
    job = e.runner.status(job_id)["job"]
    contract = {
        "job_id",
        "mode",
        "role",
        "state",
        "active",
        "peer",
        "calling_session",
        "progress",
        "note",
        "origin",
        "exclusions",
        "instruction_files",
        "ack",
        "error",
    }
    assert contract <= set(job)
    assert job["role"] == "conductor" and job["error"] == job["last_error"] and isinstance(job["exclusions"], list)
    assert isinstance(job["ack"], dict) and overview["jobs"][0]["job_id"] == job_id
    e.ledger.close()


# ---------------------------------------------------------------------------------------------------------
# R28: janitor GC and gc_job over the public BlockStore API
# ---------------------------------------------------------------------------------------------------------


def _finished_job_with_object(led: Ledger, store, job_id: str, state: str = "committed"):
    led.create_job(job_id, Role.TARGET, Mode.PUSH, None, {})
    m = store.put_bytes(b"x" * 4096 + job_id.encode(), "application/octet-stream")
    led.job_object(job_id, m.object_id)
    epoch = led.claim_owner(job_id, Role.TARGET, "test:0", 0.0)
    led.transition(job_id, Role.TARGET, "*", state, {"kind": "test"}, epoch=epoch)
    led.release_owner(job_id, Role.TARGET, "test:0", epoch)
    return m


def _age(store, m, seconds: float) -> None:
    """Backdate an object's block and manifest files (stored long before the GC runs)."""
    at = time.time() - seconds
    for path in (store.manifest_path(m.object_id), *(store.block_path(b) for b in m.blocks)):
        os.utime(path, (at, at))


def test_janitor_collects_blocks_and_manifests_of_finished_jobs(tmp_path):
    """★ DESIGN §5.1/R28: the janitor runs the block-store GC — blocks beyond the cache cap and the manifests of
    objects no non-terminal job references (committed and aborted jobs) go; a running job's object stays. At most
    once per interval for all processes of the profile.

    Mutant: ``janitor`` without the ``store`` GC step (the layer-1 janitor) → the aborted job's manifest stays."""
    pd = tmp_path / "pd"
    led = Ledger(pd / "ledger.sqlite3")
    store = blocks.BlockStore(pd, led, cache_days=3, cache_bytes=0)
    done = _finished_job_with_object(led, store, ulid(), "committed")
    aborted = _finished_job_with_object(led, store, ulid(), "aborted")
    for m in (done, aborted):
        _age(store, m, 3600)
    live_job = ulid()
    led.create_job(live_job, Role.TARGET, Mode.PUSH, None, {})
    live = store.put_bytes(b"live" * 1000, "application/octet-stream")
    led.job_object(live_job, live.object_id)
    did = jobs.janitor(led, None, None, flush_outbox=None, store=store)
    assert did["gc"] == 2
    for m in (done, aborted):
        assert not store.manifest_path(m.object_id).exists() and led.object_get(m.object_id) is None
        assert not any(store.has(b) for b in m.blocks)
    assert store.manifest_path(live.object_id).is_file() and all(store.has(b) for b in live.blocks)
    assert jobs.janitor(led, None, None, flush_outbox=None, store=store)["gc"] is None  # within the interval
    assert jobs.janitor(led, None, None, flush_outbox=None, store=store, gc_interval_s=0.0)["gc"] == 0
    led.close()


def test_janitor_gc_failure_never_breaks_the_janitor(tmp_path, clock):
    led = Ledger(tmp_path / "l.sqlite3", clock=clock)

    class Broken:
        cache_days, cache_bytes = 3, 1

        def gc(self, *, keep_days, max_bytes):
            raise RuntimeError("disk gone")

    did = jobs.janitor(led, None, None, flush_outbox=None, store=Broken())
    assert did["gc"] == "error:RuntimeError" and did["codes"] == 0
    led.close()


def test_gc_job_frees_the_jobs_blocks_and_manifests_before_purging(tmp_path, clock):
    """★ ``gc --job`` (SEC 6.3, R28) over the real block store: the job's blocks, manifests and index rows go, the
    journal too; ``release_job`` runs before ``purge_job`` (it finds the objects through ``job_objects``).

    Mutant: ``gc_job`` purges the job rows before ``store.release_job`` → the job's objects are no longer known and
    its blocks and manifests stay."""
    pd = tmp_path / "pd"
    led = Ledger(pd / "ledger.sqlite3", clock=clock)
    later = time.time() + 2 * blocks.MANIFEST_GRACE_S
    store = blocks.BlockStore(pd, led, clock=lambda: later)
    job_id = ulid()
    m = _finished_job_with_object(led, store, job_id, "aborted")
    other = _finished_job_with_object(led, store, ulid(), "committed")
    (pd / "staging" / job_id / "quarantine").mkdir(parents=True)
    leftovers = jobs._continuation_files(pd / "logs", job_id)
    for path in leftovers:
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text("x")
    jobs.gc_job(led, job_id, data_dir=pd, store=store)
    assert not any(p.exists() for p in leftovers)
    assert not store.manifest_path(m.object_id).exists() and led.object_get(m.object_id) is None
    assert not any(store.has(b) for b in m.blocks)
    assert led.job(job_id, Role.TARGET) is None and led.job_objects(job_id) == set()
    assert not (pd / "staging" / job_id).exists()
    assert store.manifest_path(other.object_id).is_file()  # another job's object is not this gc's business
    led.close()


# ---------------------------------------------------------------------------------------------------------
# W055/W056: the listener and the identity
# ---------------------------------------------------------------------------------------------------------


def test_listener_follows_pairing_and_rotation_in_other_processes(runtime_env):
    """W056: without an identity the listener is ``None`` (every route answers like a missing plugin); once
    another process of the profile created one it serves; after a rotation elsewhere it is rebuilt with the new
    key instead of signing with the old one until a restart.

    Mutant: ``listener_server`` returns the cached server without comparing the identity → the old key stays."""
    host = runtime_env
    rt = jobs.ensure_started(None)
    assert jobs.listener_server() is None
    other = Ledger(rt.ledger.path)  # the CLI process of the same profile
    crypto.Identity.create(
        other, env_writable=host.env_writable, save_env=host.save_env, read_env_fresh=host.read_env_fresh
    )
    srv = jobs.listener_server()
    assert srv is not None and jobs.listener_server() is srv
    rotated = crypto.Identity.create(
        other,
        env_writable=host.env_writable,
        save_env=host.save_env,
        read_env_fresh=host.read_env_fresh,
        rotate=True,
    )
    other.close()
    srv2 = jobs.listener_server()
    assert srv2 is not None and srv2 is not srv
    assert jobs._identity(rt).fingerprint() == rotated.fingerprint()


# ---------------------------------------------------------------------------------------------------------
# X23: bundle ways
# ---------------------------------------------------------------------------------------------------------


def test_export_composes_the_planning_negotiation_and_keep(tmp_path, clock):
    """X23/W062/O3: an export gets the planning cluster's negotiation bound to the bundle's directory and
    ``retire=False`` with ``--keep``; without ``plan.bundle_negotiate`` the bundle target decides itself."""
    from ctx_handover.engine import plan as planning

    led = Ledger(tmp_path / "l.sqlite3", clock=clock)
    seen: list[dict] = []

    class BundleTarget:
        def __init__(self, out, identity, **kw):
            seen.append({"out": out, **kw})

    transport = SimpleNamespace(bundle=SimpleNamespace(BundleTarget=BundleTarget))
    src = SimpleNamespace(host=SimpleNamespace(plugin_version="0.1.0", hermes_version="0.22.6"), instance_name="srv")
    out = tmp_path / "exports" / "work-20261109-120000.ctxh.tar"
    jobs.apis_for(
        Mode.EXPORT,
        None,
        local_source=src,
        local_target=None,
        identity=object(),
        bundle_path=out,
        options=Options(keep_source=True),
        ledger=led,
        transport=transport,
    )
    (kw,) = seen
    assert kw["retire"] is False and kw["out"] == out
    if hasattr(planning, "bundle_negotiate"):
        assert kw["negotiate"].func is planning.bundle_negotiate
        assert kw["negotiate"].keywords == {"out_dir": out.parent}
    else:  # pragma: no cover - planning cluster without the export negotiation
        assert kw["negotiate"] is None
    led.close()


def test_export_without_path_names_the_default_bundle_file(tmp_path):
    """X23/W127: export without a path writes ``<workspace_root>/<title-slug>-<YYYYmmdd-HHMMSS>.ctxh.tar``
    (DESIGN §5.4, §14.4 ``workspace_root`` = "fresh workspaces and bundles"); ``--dir`` without the suffix."""
    at = time.mktime((2026, 11, 9, 12, 30, 5, 0, 0, -1))
    opts = Options(workspace_root=str(tmp_path / "ho"))
    assert jobs.default_bundle_path("My Work: v2!", opts, at) == tmp_path / "ho" / "my-work-v2-20261109-123005.ctxh.tar"
    assert jobs.default_bundle_path(None, replace(opts, as_dir=True), at) == tmp_path / "ho" / "session-20261109-123005"
