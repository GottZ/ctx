"""Layer 3 — session transfer edge cases (TESTPLAN §4 E6, E7, E13).

Sessions that need a shape the fake model cannot produce in reasonable time (10 000 rows, a foreign provider
route) are built on A's ``state.db`` with ``tests.fakes.sessiondb.SessionBuilder`` — the core's own write paths
(``create_session``, ``update_session_model``, ``append_messages_batch``, in-place compaction), never SQL —
while the gateways run (a second writer, as LAB L3 did). All transfers are pulls from B (``hermes handover take``).

Runtime estimate: 4–7 min (E6 2–4 min: building ≈ 10 800 rows / 6.5 MB and moving them; E7, E13 ≈ 1 min each).
"""

from __future__ import annotations

import json

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)


def _own_dir(lab, name: str):
    """A workspace folder of its own (a session whose cwd were ``lab/ws-e2e/a`` itself would carry every other
    scenario's workspace along)."""
    path = lab.ws("a") / f"{name}{lab.run_id}"
    path.mkdir(parents=True, exist_ok=True)
    return path


def _builder(db):
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    return SessionBuilder(db)


def _pull(lab, ev, sid: str, *extra: str, name: str = "take") -> tuple[_lab.CliResult, dict, str]:
    res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", *extra, timeout=900)
    ev.cli(name, res)
    assert res.rc == 0 and res.job_id, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write(f"{name}-job.json", status)
    job = status["job"]
    assert job.get("state") == "done", job
    return res, job, str((job.get("ack") or {}).get("session_id") or "")


@pytest.mark.e2e_timeout(1200)
def test_e06_session_over_the_import_limits(lab, ev):
    """E6 (SK H1, R2): a single segment of 10 800 rows (10 500 archived by in-place compaction + 300 live) and
    ≈ 6.5 MB exceeds ``import_sessions``' limits (10 000 rows / 5 MiB); the target uses path B′ and the result is
    lossless: H_all, H_rows, H_model and the display history are equal."""
    from ctx_handover.tests.fakes.sessiondb import large_segment

    cwd = lab.ws("a") / "e06"
    cwd.mkdir(parents=True, exist_ok=True)
    title = f"e06 large {lab.run_id}"
    with lab.db("a", write=True) as db:
        sid = large_segment(
            _builder(db), live=300, archived=10_500, size=600, cwd=str(cwd), title=title, model="fake-model"
        )
    hashes_a = lab.hashes("a", sid)
    ev.write("source-hashes.json", hashes_a)
    assert hashes_a["rows"] > 10_000, hashes_a
    res, job, sid_b = _pull(lab, ev, sid)
    # the target reports the import path B′ as a job warning ("… was over Hermes' import limit and was imported another
    # way …")
    said = " ".join(str(w) for w in job.get("warnings") or []) + "\n" + res.text
    ev.write("import-path.txt", said)
    assert "over Hermes' import limit and was imported another way" in said, said[-1500:]
    hashes_b = lab.hashes("b", sid_b)
    ev.write("target-hashes.json", hashes_b)
    for key in ("h_all", "h_rows", "h_model", "model_norm", "display_norm", "rows"):
        assert hashes_b[key] == hashes_a[key], (key, hashes_a[key], hashes_b[key])


@pytest.mark.e2e_timeout(420)
def test_e07_title_collision(lab, ev):
    """E7: B already has an unrelated session titled "T"; the pulled session arrives as "T #2" without an error and
    the unrelated session keeps its title."""
    title = f"E07 shared title {lab.run_id}"
    with lab.db("b", write=True) as db:
        b = _builder(db)
        other = b.create(title=title, cwd=str(lab.work("b")), model="fake-model")
        b.turns(other, 1)
    src = lab.make_source("a", "e07proj", repo=False, title=title, turns=("collide",))
    _res, _job, sid_b = _pull(lab, ev, src.sid)
    assert lab.row("b", sid_b).get("title") == f"{title} #2", lab.row("b", sid_b)
    assert lab.row("b", other).get("title") == title


@pytest.mark.e2e_timeout(420)
def test_e13_model_mapping(lab, ev):
    """E13 (R11, R20, L18): A's session runs on provider "x" (route ``http://route-of-a.invalid/v1``) that B does not
    have: B's row gets B's default model and no peer route (no ``base_url``/billing route of A anywhere in the row),
    the final message and the note name the change, and the session resumes on B through its gateway without a
    "select a model" stop — the fake model receives B's default model name."""
    marker = "route-of-a.invalid"
    with lab.db("a", write=True) as db:
        b = _builder(db)
        sid = b.create(
            title=f"e13 foreign model {lab.run_id}",
            model="x-model",
            provider="x",
            base_url=f"http://{marker}/v1",
            api_mode="chat_completions",
            cwd=str(_own_dir(lab, "e13")),
            model_config={"gateway_runtime": {"provider": "x", "base_url": f"http://{marker}/v1"}},
        )
        b.turns(sid, 2)
    ev.write("source-row.json", lab.row("a", sid))
    res, _job, sid_b = _pull(lab, ev, sid)
    row_b = lab.row("b", sid_b)
    ev.write("target-row.json", row_b)
    assert row_b.get("model") == "fake-model", row_b.get("model")
    assert marker not in json.dumps(row_b, default=str), "a route of A was kept in B's row"
    assert "fake-model" in res.text and "x-model" in res.text, res.text
    arrival = lab.arrival("b", sid_b) or {}
    assert "x-model" in str(arrival.get("note")) and "fake-model" in str(arrival.get("note")), arrival.get("note")
    mark = lab.llm_mark()
    turn = lab.turn("b", sid_b, f"E13 continue {_lab.uid()}")
    ev.write("turn-b.json", turn.brief())
    assert turn.ok and "select a model" not in turn.text.lower(), turn.brief()
    mains = [r for r in lab.llm_since(mark) if r.get("kind") == "main"]
    assert mains and all(r.get("model") == "fake-model" for r in mains), [r.get("model") for r in mains]
