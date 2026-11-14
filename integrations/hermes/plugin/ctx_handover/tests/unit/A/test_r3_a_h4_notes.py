"""Review round 3 — the ledger rest of H4 (REVIEW-2 §2): the handover note of an arrival
stayed in ``arrivals.note`` for ever although its report (the same text) goes after 30 days, and ``pull_log`` (the
paths a peer pulled) had no limit at all (only ``gc --job``).

Now ``Ledger.purge_delivered`` (run by the janitor's retention step) removes arrivals whose note was delivered and
``pull_log`` rows, both older than ``JOURNAL_KEEP_S`` and only for jobs that are over on this host. A note not
delivered yet stays: it is the arrived session's handover context for its first turn.

Red before the fix: ``AttributeError: 'Ledger' object has no attribute 'purge_delivered'`` and, through the janitor,
``ARRIVAL NOTE STILL STORED`` (the reviewer's probe ``test_zz_g2_retention.py``)."""

from __future__ import annotations

from ctx_handover.engine import jobs
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ledger import JOURNAL_KEEP_S, Ledger
from ctx_handover.engine.model import Mode, Role

DAY = 86400.0


def _arrive(ledger: Ledger, session_id: str, job_id: str, note: str = "NOTE with /home/u/proj paths") -> None:
    ledger.arrival_put(
        session_ids=[session_id],
        root_id=session_id,
        job_id=job_id,
        peer="laptop",
        note=note,
        report_path="",
        path_map=[],
        continue_prompt=None,
    )


def _target(ledger: Ledger, job_id: str, state: str) -> None:
    ledger.create_job(job_id, Role.TARGET, Mode.PUSH, "peer", {"role": "target"})
    ledger.transition(job_id, Role.TARGET, "*", state, {}, epoch=0)


def test_r3_a_h4_delivered_notes_and_the_pull_log_expire(tmp_path, clock):
    """★ After ``JOURNAL_KEEP_S``: the delivered note of a finished job and the pull log of a finished job are gone;
    a pending note, a note of a job still active here, and recent rows stay.

    Mutant (``Ledger.purge_delivered``): keep every arrival ⇒ the delivered note is still stored."""
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    done, pending, active = ulid(), ulid(), ulid()
    _target(ledger, done, "committed")
    _target(ledger, pending, "committed")
    _target(ledger, active, "committing")
    _arrive(ledger, "s-done", done)
    ledger.note_delivered("s-done")
    _arrive(ledger, "s-pending", pending)
    _arrive(ledger, "s-active", active)
    ledger.note_delivered("s-active")
    pulled, running = ulid(), ulid()
    ledger.create_job(pulled, Role.SOURCE, Mode.PULL, "peer", {"role": "source"})
    ledger.transition(pulled, Role.SOURCE, "*", "released", {}, epoch=0)
    ledger.create_job(running, Role.SOURCE, Mode.PULL, "peer", {"role": "source"})
    ledger.pull_log_put(pulled, "peer", ["/home/u/proj/.env.example"])
    ledger.pull_log_put(running, "peer", ["/home/u/other"])

    clock.advance(JOURNAL_KEEP_S + DAY)
    recent = ulid()
    _target(ledger, recent, "committed")
    _arrive(ledger, "s-recent", recent)
    ledger.note_delivered("s-recent")
    ledger.pull_log_put(pulled, "peer", ["/home/u/proj/new"])

    did = ledger.purge_delivered(JOURNAL_KEEP_S)

    assert did == {"arrivals": 1, "pull_log": 1}
    assert ledger.arrival_for("s-done") is None
    assert ledger.arrival_for("s-pending")["note"].startswith("NOTE")
    assert ledger.arrival_for("s-active") is not None and ledger.arrival_for("s-recent") is not None
    assert sorted(tuple(r["items"]) for r in ledger.pull_log(10)) == [("/home/u/other",), ("/home/u/proj/new",)]
    ledger.close()


def test_r3_a_h4_the_janitor_purges_delivered_notes(tmp_path, clock):
    """The reviewer's probe through the janitor: a committed target job, its note delivered, the clock 31 days on ⇒
    the report and the note are both gone; the veto index never sees the row again."""
    ledger = Ledger(tmp_path / "ledger.sqlite3", clock=clock)
    job = ulid()
    _target(ledger, job, "committed")
    _arrive(ledger, "s1", job)
    ledger.note_delivered("s1")
    clock.advance(JOURNAL_KEEP_S + DAY)

    did = jobs.janitor(ledger, None, None, flush_outbox=None, background=False, data_dir=tmp_path / "data")

    assert did["notes"] == {"arrivals": 1, "pull_log": 0}
    assert ledger.arrival_for("s1") is None and ledger.arrival_rows() == []
    ledger.close()
