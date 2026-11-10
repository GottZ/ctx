"""Cluster F — progress reporting (DESIGN §14.1 "Progress", §2.1 consequence 5; TESTPLAN §2.7)."""

from __future__ import annotations

import io
import json
import threading

from ctx_handover.engine.contracts import Reporter
from ctx_handover.engine.model import Progress
from ctx_handover.engine.progress import NullReporter, PostMessageReporter, ReporterBroker, TtyReporter
from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx


class Clock:
    def __init__(self) -> None:
        self.t = 100.0

    def __call__(self) -> float:
        return self.t


def test_progress_reporter_throttles_edits():
    """One sticky message created by ``start``; edits at most every 2 s; terminal steps and ``finish`` are never
    throttled; the ETA comes from the byte rate."""
    ctx = FakeCtx()
    clock = Clock()
    rep = PostMessageReporter(ctx, throttle_s=2.0, clock=clock, peer="laptop", direction="out")
    assert isinstance(rep, Reporter)
    rep.start("Handover → laptop: 3 items, 1.0 GiB; 0 secrets left out.")
    assert len(ctx.messages) == 1
    for i in range(10):  # ten updates within one second → no edit yet
        clock.t += 0.1
        rep.progress(Progress(step="packing workspace", bytes_done=i * 1024**2, bytes_total=1024**3))
    assert ctx.edits == []
    clock.t += 2.0
    rep.progress(Progress(step="packing workspace", bytes_done=512 * 1024**2, bytes_total=1024**3))
    assert len(ctx.edits) == 1
    assert "packing workspace · 512/1024 MiB (50 %)" in ctx.messages[0]
    assert "min left" in ctx.messages[0]
    rep.warn("2 files over max_file_mb were left out")
    clock.t += 2.1
    rep.progress(Progress(step="sending", bytes_done=600 * 1024**2, bytes_total=1024**3))
    assert "2 files over max_file_mb" in ctx.messages[0]
    edits = len(ctx.edits)
    rep.finish("Handed over to laptop.")
    assert len(ctx.edits) == edits + 1 and ctx.messages[0].startswith("Handed over to laptop.")
    assert len(ctx.messages) == 1


def test_progress_terminal_step_renders_final_text():
    ctx = FakeCtx()
    clock = Clock()
    rep = PostMessageReporter(ctx, clock=clock, peer="server", direction="in", on_done=lambda: "FINAL TEXT")
    rep.start("first")
    rep.progress(Progress(step="importing"))
    rep.progress(Progress(step="done"))
    assert ctx.messages[0] == "FINAL TEXT"


def test_progress_without_post_message():
    """No ``post_message`` (plugin host, serve, CLI without gateway): reporting is silent and never raises;
    ``/handover status`` stays authoritative (DESIGN §2.1 consequence 5)."""
    rep = PostMessageReporter(object(), peer="x")
    rep.start("first")
    rep.progress(Progress(step="packing"))
    rep.info("i")
    rep.warn("w")
    rep.finish("done")

    class Exploding(FakeCtx):
        def post_message(self, text):
            raise RuntimeError("no conversation bound")

    rep = PostMessageReporter(Exploding(), peer="x")
    rep.start("first")
    rep.progress(Progress(step="done"))
    rep.finish("done")

    class BadHandle(FakeCtx):
        def post_message(self, text):
            class H:
                def edit(self, text):
                    raise OSError("socket closed")

            return H()

    rep = PostMessageReporter(BadHandle(), peer="x", clock=Clock())
    rep.start("first")
    rep.finish("done")
    NullReporter().progress(Progress(step="x"))


def test_tty_reporter_lines_and_json():
    out = io.StringIO()
    clock = Clock()
    rep = TtyReporter(stream=out, clock=clock, peer="server", direction="in", throttle_s=0.5)
    rep.progress(Progress(step="importing", items_done=1, items_total=4))
    clock.t += 0.1
    rep.progress(Progress(step="importing", items_done=2, items_total=4))  # throttled
    rep.warn("careful")
    text = out.getvalue()
    assert "Handover ← server · importing · 1/4" in text and "2/4" not in text and "warning: careful" in text

    out = io.StringIO()
    rep = TtyReporter(stream=out, json_mode=True, clock=clock, peer="server", direction="in")
    rep.progress(Progress(step="importing", items_done=1, items_total=4))
    rep.info("hello")
    events = [json.loads(line) for line in out.getvalue().splitlines()]
    assert events[0]["event"] == "progress" and events[0]["step"] == "importing"
    assert events[1] == {"event": "info", "text": "hello"}


def test_progress_shows_states_in_words_and_json_keeps_the_names():
    """The progress line names the conductor's state in words (no ``in_doubt`` or ``target_preparing`` for a
    person); ``--json`` events keep the state name; a step without words is shown as it is. The words are the ones
    ``/handover status`` uses (``surfaces.STATE_TEXT``).

    Mutant: ``format_progress`` shows ``p.step`` again ⇒ ``'unpacking on the receiving machine'`` missing."""
    from ctx_handover.engine import surfaces
    from ctx_handover.engine.progress import STEP_TEXT, format_progress

    line = format_progress(Progress(step="target_preparing"), "laptop", "out")
    assert line == "Handover → laptop · unpacking on the receiving machine", line
    doubt = format_progress(Progress(step="in_doubt"), None, "out")
    assert doubt == "Handover → handover file · unclear whether the other machine finished", doubt
    assert "in_doubt" not in doubt
    out = io.StringIO()
    TtyReporter(stream=out, json_mode=True, clock=Clock(), peer="laptop").progress(Progress(step="in_doubt"))
    assert json.loads(out.getvalue())["step"] == "in_doubt"
    for step, words in STEP_TEXT.items():
        assert surfaces.STATE_TEXT[step] == words, step


def test_reporter_broker_binds_reporter_to_job():
    """The runner's ``reporter_factory(job_id)`` is called in the handler thread during ``plan_sync``; the broker
    hands it the reporter the handler bound, and the same one again for later calls (worker thread)."""
    broker = ReporterBroker()
    bound = NullReporter()
    with broker.bind(bound):
        assert broker.factory("01JOB1") is bound
    seen = []
    t = threading.Thread(target=lambda: seen.append(broker.factory("01JOB1")))
    t.start()
    t.join()
    assert seen == [bound]
    assert isinstance(broker.factory("01OTHER"), NullReporter)
    broker.release("01JOB1")
    assert broker.factory("01JOB1") is not bound
