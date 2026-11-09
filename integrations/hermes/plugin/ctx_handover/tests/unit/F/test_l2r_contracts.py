"""Layer R (red first) — open contract points of cluster F (``reports/L2-contracts.md`` V02, V10, V27, V31 and the
F clean-up of §2), checked against the real neighbour modules (D ``plan``, C ``wire``, A ``Ledger``)."""

from __future__ import annotations

import types

import httpx
import pytest

from ctx_handover.engine import commands, progress, surfaces, wire
from ctx_handover.engine import plan as planning
from ctx_handover.engine.continuation import Spawner
from ctx_handover.engine.errors import HandoverError
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import Progress
from ctx_handover.engine.progress import PostMessageReporter
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx
from ctx_handover.tests.fakes.surfaces_fake import sample_final_plan
from ctx_handover.tests.unit.F._l2r_helpers import IDENTITY_SENTINEL, backend_with_runtime, dial_peer

SESSION = "20261109_093000_a1b2c3"

# ---------------------------------------------------------------------------------------------------------
# V02 — the continuation takes ULID job ids only
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    "bad",
    ["job1", "Z" * 26, "ABCDEFGHJKMNPQRSTVWXYZ0123", "LOWER", "8" + ulid()[1:], ulid() + "0", "x" * 64],
    ids=["short", "Z26", "first-char-A", "word", "first-char-8", "27-chars", "64-chars"],
)
def test_l2r_continuation_job_ids_are_ulids(tmp_path, bad):
    """V02 (K-2): ``continuation.Spawner`` checks job ids with the one rule ``ids.ULID_RE`` (``continuation.py:62``
    has its own ``[A-Za-z0-9]{1,64}``): ``spawn`` refuses anything else as an invalid ``job_id`` before a file is
    written or a process starts, and ``result`` reports it as never started; a lower-cased ULID is not canonical
    either. A ULID passes."""
    started: list[list[str]] = []

    def popen(argv, **kwargs):
        started.append(list(argv))
        return types.SimpleNamespace(pid=4242)

    spawner = Spawner(
        log_dir=tmp_path / "logs",
        profile=None,
        approvals_off=lambda: False,
        which=lambda name: "/usr/bin/hermes",
        popen=popen,
    )
    for job_id in (bad, ulid().lower()):
        with pytest.raises(HandoverError) as exc:
            spawner.spawn(SESSION, None, job_id, requested_by=None)
        assert exc.value.details.get("field") == "job_id", job_id
        assert spawner.result(job_id) == {"started": False}
    assert started == []
    assert not (tmp_path / "logs").exists() or not any((tmp_path / "logs").iterdir())
    good = ulid()
    assert spawner.spawn(SESSION, None, good, requested_by=None).started is True
    assert spawner.result(good)["started"] is True


# ---------------------------------------------------------------------------------------------------------
# V10 — F reads the W125 keys of ``presend_summary`` directly
# ---------------------------------------------------------------------------------------------------------

W125 = ("items_count", "total_bytes", "excluded_secrets", "denied_tracked", "withheld_stash", "outside_workspace")


def _canonical_presend(final) -> dict:
    """``plan.presend_summary`` reduced to its W125 keys (``plan.PRESEND_KEYS``) — the layer-1 aliases ``items``,
    ``extras_outside_workspace`` (and the extra ``item_list``) are what F must no longer need."""
    pre = planning.presend_summary(
        final,
        withheld_stash=["stash@{0}: .env.production"],
        instructions=[{"item_id": "w0", "rel": "AGENTS.md", "sha256": "ab" * 32, "status": "new"}],
    )
    return {key: pre[key] for key in planning.PRESEND_KEYS}


def test_l2r_plan_summary_reads_the_w125_keys(tmp_path):
    """V10: the conductor hands ``presend_summary`` over structured; ``surfaces.plan_summary`` (what the first
    progress message, the confirmation and ``render_plan`` show) takes ``items_count``, ``total_bytes``,
    ``excluded_secrets``, ``denied_tracked``, ``withheld_stash``, ``outside_workspace``, ``instruction_files``,
    ``model`` and ``offline`` from it as they are — not from the aliases, and not recomputed from the FinalPlan."""
    final = sample_final_plan(extras=2, secrets=1)
    pre = _canonical_presend(final)
    assert pre["items_count"] == 3 and len(pre["outside_workspace"]) == 2  # the fixture really has these
    summary = surfaces.plan_summary(final, pre)
    for key in W125:
        assert summary[key] == pre[key], key
    assert summary["model"] == pre["model"] and summary["offline"] is pre["offline"]
    assert any("AGENTS.md" in line for line in summary["instruction_files"]), summary["instruction_files"]

    alone = surfaces.plan_summary(None, pre)  # what a surface that only has the conductor's summary shows
    for key in (*W125, "model", "offline"):
        assert alone[key] == pre[key], key
    text = surfaces.render_presend(alone, "laptop", "push")
    assert text.startswith(f"Handover → laptop: {pre['items_count']} items")
    assert "2 items outside the project" in text


# ---------------------------------------------------------------------------------------------------------
# V27 — doctor uses cluster C's probe summary and C's measurement sizes
# ---------------------------------------------------------------------------------------------------------


def _rejecting_client(peer):
    """The real ``wire.WireClient`` of ``peer`` whose dashboard gate answers 401 (no network)."""
    return wire.WireClient(
        peer,
        None,
        read_secret=lambda name: "stale-dashboard-token",
        transport=httpx.MockTransport(lambda request: httpx.Response(401)),
        max_attempts=1,
        sleep=lambda s: None,
    )


def test_l2r_doctor_peer_line_is_the_probe_summary_of_wire(fake_host, tmp_path):
    """V27 (X24, wp-c W-5): the doctor line of a paired gateway is cluster C's ``probe_gate`` summary — its
    ``ok``, ``detail`` and ``remedy`` — and F adds only the clock skew; no second classification of the report
    beside it (``commands.peer_check`` derives its own texts today)."""
    backend, runtime = backend_with_runtime(fake_host, tmp_path)
    peer = dial_peer("server")
    runtime.ledger.upsert_peer(peer)
    probe = _rejecting_client(peer)
    try:
        report = probe.probe_gate(measure_limit=False)
    finally:
        probe.close()
    assert report["gate"] == "rejected" and report["ok"] is False and report["remedy"]
    backend._client = _rejecting_client
    checks = {c["name"]: c for c in backend.doctor_checks()}
    line = checks["peer server"]
    assert line["ok"] is report["ok"]
    assert report["detail"] in line["detail"], (line["detail"], report["detail"])
    assert report["remedy"] in (line.get("remedy") or ""), (line.get("remedy"), report["remedy"])


def test_l2r_doctor_measure_sizes_come_from_wire():
    """V27: the sizes ``doctor --measure`` uploads are cluster C's constant ``wire.PROBE_SIZES_MIB``; F keeps no
    copy of its own (``commands.MEASURE_SIZES`` is gone or the same object) and its texts name their sum."""
    sizes = wire.PROBE_SIZES_MIB
    own = getattr(commands, "MEASURE_SIZES", sizes)
    assert own is sizes, f"commands.MEASURE_SIZES is a copy: {own!r}"
    text = getattr(commands, "MEASURE_TEXT", f"{sum(sizes)} MiB")
    assert f"{sum(sizes)} MiB" in text


# ---------------------------------------------------------------------------------------------------------
# V31 — every parked run gets its closing text
# ---------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize("step", ["in_doubt", "retiring", "aborting"])
def test_l2r_parked_run_gets_its_closing_text(step):
    """V31 (W111): a run may park in ``in_doubt``, ``retiring`` or ``aborting`` (``conductor.py`` "parked for the
    janitor"); ``progress.PARKING_STEPS`` names all three, and the progress notice shows the final text for each
    at once (unthrottled)."""
    assert step in progress.PARKING_STEPS
    ctx = FakeCtx()
    rep = PostMessageReporter(
        ctx, throttle_s=3600.0, clock=lambda: 100.0, peer="laptop", on_done=lambda: f"FINAL after {step}"
    )
    rep.start("Handover → laptop: 1 items, 3.0 MiB; 0 secrets left out.")
    rep.progress(Progress(step="transferring", bytes_done=1, bytes_total=10))
    rep.progress(Progress(step=step))
    assert ctx.messages[-1] == f"FINAL after {step}"


# ---------------------------------------------------------------------------------------------------------
# §2 F — ``rotate_identity`` leaves the runtime's identity cache to cluster A
# ---------------------------------------------------------------------------------------------------------


def test_l2r_rotate_identity_does_not_set_the_runtime_identity(fake_host, tmp_path):
    """L2-contracts §2 F: ``EngineBackend.rotate_identity`` writes the new key (``Identity.create(…,
    rotate=True)``) and nothing else — the profile runtime reloads its identity itself when the ledger names
    another key (``jobs._identity``), so F does not set ``rt.identity``/``rt.identity_error``."""
    backend, runtime = backend_with_runtime(fake_host, tmp_path)
    runtime.identity_error = "untouched"
    first = backend.rotate_identity()
    second = backend.rotate_identity()
    assert first["fingerprint"] != second["fingerprint"]
    assert runtime.ledger.meta_get("identity_pubkey")
    assert runtime.identity is IDENTITY_SENTINEL
    assert runtime.identity_error == "untouched"
