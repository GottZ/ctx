"""Review round 3 — L3-F24, the conductor's part (integrator's request; D form ``plan.include_refused`` /
``presend_summary(…, include=…)`` from cluster D, the texts from F): the conductor hands the job's local ``--include``
and ``--include-secret`` paths to both pre-send summaries — the one the confirmation shows (``_confirm``) and the one
it journals as ctx ``presend`` (``_negotiated``) — so a path the plan refused is named before anything is sent.

Red before the fix: ``include_refused == []`` in both summaries (the conductor passed no ``include``)."""

from __future__ import annotations

import dataclasses

from ctx_handover.engine import plan as planning
from ctx_handover.engine.jsonutil import canonical_bytes, sha256_hex
from ctx_handover.engine.model import NotTransferred, Options, PlanHints, Role
from ctx_handover.tests.fakes import apis_fake
from ctx_handover.tests.fakes.apis_fake import FakePlanning
from ctx_handover.tests.unit.A.harness import Harness

OUTSIDE = "/opt/elsewhere/tree"
SECRET = "/opt/elsewhere/.env.prod"
WHY = planning.WHY_OUTSIDE_HOME


class _AlwaysAsk(FakePlanning):
    """A reason to ask, so the confirmation (and its summary) is shown."""

    def confirmation_reasons(self, final, options, source_offline, **kw):
        return [*super().confirmation_reasons(final, options, source_offline, **kw), planning.CONFIRM_PROPOSED]


def _refusing_plan(monkeypatch, *paths: str) -> None:
    """The source's plan refuses ``paths`` as this machine's policy does for a folder outside ``$HOME`` and
    ``export_roots`` (``not_transferred`` with ``outside home``)."""
    real = apis_fake.ResourcePlan

    def plan(**kw):
        return real(**{**kw, "not_transferred": tuple(NotTransferred(p, WHY) for p in paths)})

    monkeypatch.setattr(apis_fake, "ResourcePlan", plan)


def _with_include_secret(h: Harness, paths: tuple[str, ...]) -> None:
    """``start_job(include_secret=…)`` keeps the paths in the conductor row's params (X07, never in ``Options``)."""
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    params = {**(row["params"] or {}), "include_secret": list(paths)}
    encoded = canonical_bytes(params)
    with h.ledger.tx() as c:
        c.execute(
            "UPDATE jobs SET params=?, params_digest=? WHERE job_id=? AND role=?",
            (encoded.decode("utf-8"), sha256_hex(encoded), h.job_id, str(Role.CONDUCTOR)),
        )


def _presend(h: Harness) -> dict:
    out: dict = {}
    for e in h.ledger.events(h.job_id, Role.CONDUCTOR):
        data = e["data"] or {}
        if isinstance(data.get("ctx"), dict) and "presend" in data["ctx"]:
            out = data["ctx"]["presend"]
    return out


def test_r3_a_f24_both_summaries_name_a_refused_local_include(tmp_path, clock, monkeypatch):
    """★ ``--include <folder outside $HOME>`` and ``--include-secret <file outside>``: the confirmation's summary and
    the journaled ``presend`` name both with their reason.

    Mutant (``Conductor._includes``): return ``()`` ⇒ ``include_refused == []``."""
    _refusing_plan(monkeypatch, OUTSIDE, SECRET)
    h = Harness(tmp_path, clock, options=Options(hints=PlanHints(include=(OUTSIDE,))))
    h.planning = _AlwaysAsk()
    _with_include_secret(h, (SECRET,))
    asked: list[dict] = []
    c = h.conductor(confirm=lambda info: asked.append(info) or "yes")
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done", h.states()
    want = [{"path": OUTSIDE, "why": WHY}, {"path": SECRET, "why": WHY}]
    assert len(asked) == 1
    assert asked[0]["summary"]["include_refused"] == want, asked[0]["summary"]
    assert _presend(h)["include_refused"] == want
    assert _presend(h)["include_refused_more"] == 0


def test_r3_a_f24_without_include_nothing_is_named(tmp_path, clock, monkeypatch):
    """A path the plan left behind on its own (not named with ``--include``) is no refused include."""
    _refusing_plan(monkeypatch, OUTSIDE)
    h = Harness(tmp_path, clock, options=Options())
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done", h.states()
    assert _presend(h)["include_refused"] == [] and _presend(h)["include_refused_more"] == 0


def test_r3_a_f24_an_include_the_plan_took_is_not_named(tmp_path, clock, monkeypatch):
    """The include became an item (the world's ``/srv/app`` workspace): nothing refused."""
    _refusing_plan(monkeypatch)
    h = Harness(tmp_path, clock, options=Options(hints=PlanHints(include=("/srv/app",))))
    item = h.world.source.items[0]
    h.world.source.items[0] = dataclasses.replace(item, source_path="/srv/app")
    c = h.conductor()
    c.run_until_negotiated()
    c.run()
    assert h.state() == "done", h.states()
    assert _presend(h)["include_refused"] == []
