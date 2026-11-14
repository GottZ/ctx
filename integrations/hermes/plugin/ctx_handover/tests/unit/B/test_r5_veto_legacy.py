"""The text revision renamed "handover bundle" to "handover file" in the veto replies. Reclaim finds the replies of
a job by exact text, so a session locked by plugin 0.1.0 and taken back after an update must still match the old
wording."""

from __future__ import annotations

from ctx_handover.engine import lock
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.model import SourceState


def test_r5_reclaim_still_knows_the_0_1_0_bundle_texts():
    job = ulid()
    entry = {"job_id": job, "peer": "", "state": SourceState.EXPORTED.value, "prepared_at": 1.7e9, "ack": None}
    texts = lock._all_texts(entry)
    short = lock.short(job)
    when = lock._fmt_time(1.7e9)
    old = {
        f"This session was exported to a handover bundle on {when}. Run /handover reclaim to continue here.",
        f"This session was exported to a handover bundle on {when} (job {short}); it continues where the bundle is "
        "imported. Your message was not sent. To continue here instead, run /handover reclaim.",
        f"An export of this session to a handover bundle is being completed (job {short}). Your message was not "
        "sent. /handover status shows progress; /handover reclaim --force takes the session back if it is stuck.",
    }
    assert old <= texts, sorted(texts)
    assert any("handover file" in t for t in texts)
