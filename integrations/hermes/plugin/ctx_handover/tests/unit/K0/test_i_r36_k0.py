"""R36 / L3-F19 (package i-r36), K0 part: two additive contract members.

* ``model.PlanItem.cwd_bytes`` (``added_field(None)``): the size of the session's working folder when the plan item's
  root is not that folder (``rel_cwd`` set, e.g. the repository around it). The source measures it; the summary before
  sending names it next to the workspace's own size — also on the conductor of a pull, which only has the wire plan.
  Documents without it keep their bytes and ids.
* ``contracts.GitProbe.is_ignored(self, path) -> bool``: is the directory ignored in the work tree around it
  (``git check-ignore``)? The collector asks it before it takes an enclosing repository as the workspace."""

from __future__ import annotations

import dataclasses
import inspect
import typing
from dataclasses import replace

import pytest

from ctx_handover.engine import contracts, gitops, jsonutil
from ctx_handover.engine import plan as dplan
from ctx_handover.engine.errors import BadRequest
from ctx_handover.engine.model import WIRE_OMIT_DEFAULT, FinalPlan, PlanItem
from ctx_handover.tests.fakes.git_fake import FakeGitProbe
from ctx_handover.tests.fakes.planning_fake import CeilingGitProbe, StaticGitProbe

from .test_model_added_fields import PLAN_ID_BEFORE_K3, sample_final_plan


def _with_cwd_bytes(final: FinalPlan, value) -> FinalPlan:
    items = list(final.plan.items)
    items[0] = replace(items[0], cwd_bytes=value)
    return replace(final, plan=replace(final.plan, items=tuple(items)))


def test_plan_item_cwd_bytes_is_an_added_field():
    """★ ``cwd_bytes`` sits after the earlier fields with default None and the omit marker.

    Red on ``ae0749a5``: no field ``cwd_bytes``."""
    by_name = {f.name: f for f in dataclasses.fields(PlanItem)}
    f = by_name["cwd_bytes"]
    assert f.default is None and f.metadata.get(WIRE_OMIT_DEFAULT) is True
    assert [x.name for x in dataclasses.fields(PlanItem)][-1] == "cwd_bytes"


def test_cwd_bytes_keeps_old_ids_and_round_trips():
    """★ A plan without ``cwd_bytes`` hashes to the id from before K-3 (bundles and stored plan ids stay valid); one
    with it round-trips through the wire form and gets a new id; a document written before the field decodes with
    None.

    Mutant: ``cwd_bytes`` declared without ``added_field`` (always serialised) — the old id changes."""
    final = sample_final_plan()
    assert dplan.plan_id(dplan.final_doc(final)) == PLAN_ID_BEFORE_K3
    assert all("cwd_bytes" not in item for item in jsonutil.to_wire(final)["plan"]["items"])
    measured = _with_cwd_bytes(final, 4096)
    wire = jsonutil.to_wire(measured)
    assert wire["plan"]["items"][0]["cwd_bytes"] == 4096
    assert jsonutil.from_wire(FinalPlan, wire) == measured
    assert dplan.plan_id(dplan.final_doc(measured)) != PLAN_ID_BEFORE_K3
    assert jsonutil.from_wire(FinalPlan, jsonutil.to_wire(final)).plan.items[0].cwd_bytes is None


@pytest.mark.parametrize("value", ["4096", 1.5, [1]])
def test_cwd_bytes_is_type_checked(value):
    wire = jsonutil.to_wire(_with_cwd_bytes(sample_final_plan(), 1))
    wire["plan"]["items"][0]["cwd_bytes"] = value
    with pytest.raises(BadRequest) as exc:
        jsonutil.from_wire(FinalPlan, wire)
    assert exc.value.details["field"] == "plan.items[0].cwd_bytes"


def _members(proto) -> set[str]:
    getter = getattr(typing, "get_protocol_members", None)
    if getter is not None:
        return set(getter(proto))
    return set(getattr(proto, "__protocol_attrs__", ()))


def test_git_probe_protocol_declares_is_ignored():
    """★ ``is_ignored(self, path: str) -> bool`` is a member of ``contracts.GitProbe``; E's probe, D's two fakes and the
    scripted E fake implement it with the same parameters.

    Red on ``ae0749a5``: ``'is_ignored' not in`` the protocol members."""
    assert "is_ignored" in _members(contracts.GitProbe)
    sig = inspect.signature(contracts.GitProbe.is_ignored)
    assert list(sig.parameters) == ["self", "path"]
    assert sig.return_annotation in (bool, "bool")
    for impl in (gitops.GitProbe, StaticGitProbe, CeilingGitProbe, FakeGitProbe):
        assert list(inspect.signature(impl.is_ignored).parameters) == ["self", "path"], impl
    assert isinstance(StaticGitProbe(), contracts.GitProbe)
    assert isinstance(FakeGitProbe(), contracts.GitProbe)
