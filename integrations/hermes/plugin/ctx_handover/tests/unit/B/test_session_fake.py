"""The cluster-B fake for cluster A has the real modules' function and parameter names, and works end to end."""

from __future__ import annotations

import inspect

import pytest

from ctx_handover.engine import lock, session_export, session_import
from ctx_handover.engine.errors import VerifyFailed
from ctx_handover.engine.model import PrepareOutcome, SessionRef
from ctx_handover.tests.fakes.session_fake import FakeSession
from ctx_handover.tests.unit.B.support import MemoryBlockStore

PAIRS = (("session_export", session_export), ("session_import", session_import), ("lock", lock))


@pytest.mark.parametrize(("name", "module"), PAIRS)
def test_fake_signatures_match_real_modules(name, module):
    fake = FakeSession()
    namespace = getattr(fake, name)
    for fn_name, fake_fn in vars(namespace).items():
        real_fn = getattr(module, fn_name)
        real = inspect.signature(real_fn).parameters
        mine = inspect.signature(fake_fn).parameters
        required = [p for p, v in real.items() if v.default is inspect.Parameter.empty and v.kind != v.VAR_KEYWORD]
        missing = [p for p in required if p not in mine]
        assert not missing, (name, fn_name, missing)
        unknown = [p for p, v in mine.items() if p not in real and v.kind not in (v.VAR_KEYWORD, v.VAR_POSITIONAL)]
        assert not unknown, (name, fn_name, unknown)


def test_fake_roundtrip_and_faults():
    fake = FakeSession()
    store = MemoryBlockStore()
    tip = fake.add_session(rows=4, title="Work")
    info = fake.session_export.describe(None, SessionRef(id=tip), calling_session=None)
    cap = fake.session_export.export_capsule(None, info, job_id="01JOB", store=store, source_meta={}, hermes=None)
    capsule = fake.session_export.read_capsule(store, cap.capsule_id, max_json_bytes=1 << 20)
    plan = fake.session_import.plan_import(
        None,
        capsule,
        cwd_for=lambda p: "/t",
        model_policy="auto",
        model_override=None,
        target_config={},
        provider_usable=lambda n, c: True,
        max_session_bytes=1 << 30,
    )
    prepared = fake.session_import.import_hidden(None, store, cap.capsule_id, plan)
    assert fake.sessions[prepared.tip_id]["hidden"] is True
    fake.append(tip, "late")
    outcome, rows = fake.session_export.check_revision(None, info, cap)
    assert outcome is PrepareOutcome.DELTA and rows[-1]["content"] == "late"
    delta = fake.session_export.export_delta(None, info, cap, rows, store=store)
    prepared = fake.session_import.apply_delta(None, store, delta, prepared)
    fake.session_import.unhide(None, prepared)
    assert fake.sessions[prepared.tip_id]["hidden"] is False
    holder = fake.lock.acquire(None, None, info, "01JOB", ttl_s=60, wait_s=0, cancelled=lambda: False)
    fake.lock.prepare(None, None, info, "01JOB", "laptop", None)
    assert fake.sessions[tip]["archived"] and fake.sessions[tip]["title"].startswith("Work (handed over")
    fake.lock.retire(None, None, info, holder, "01JOB", None, [])
    assert fake.lock.reclaim(None, None, tip, force=False)["state"] == "reclaimed"
    assert fake.sessions[tip]["title"] == "Work"
    fake.fail("import_hidden", VerifyFailed("tampered"))
    with pytest.raises(VerifyFailed):
        fake.session_import.import_hidden(None, store, cap.capsule_id, plan)
    fake.script("check_revision", PrepareOutcome.STALE_SESSION)
    assert fake.session_export.check_revision(None, info, cap)[0] is PrepareOutcome.STALE_SESSION
    assert [c[0] for c in fake.calls][:3] == ["describe", "export_capsule", "read_capsule"]
