"""K0: ``ResourcePlan.context_tokens`` (R32, L3-F17).

The source's estimate of the session's active context (``plan.active_context_tokens``) travels in the plan document
so the conductor can compare it with the target model's window. The field is additive (contract K-3): declared with
``model.added_field(0)``, left out of the wire form while it is 0, so every plan written before it — with or without
the K-3 fields — keeps its bytes and its plan id. The ids pinned here were computed with the model of commit
``7c746df5`` (before this field) by a red-proof script (output ``pinned-before.txt``).
"""

from __future__ import annotations

import dataclasses
from dataclasses import replace

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine import plan as dplan
from ctx_handover.engine.errors import BadRequest
from ctx_handover.engine.model import WIRE_OMIT_DEFAULT, FinalPlan, ResourcePlan
from ctx_handover.tests.unit.K0.test_model_added_fields import (
    PLAN_ID_BEFORE_K3,
    RESOURCE_PLAN_ID_BEFORE_K3,
    _with_added,
    sample_final_plan,
)

PLAN_ID_K3_WITH_WARNINGS = "b134f2bfed69fd3ce80ae39f083accae40f1c1a8c082f5abf63c404dd077545a"
RESOURCE_PLAN_ID_K3_WITH_WARNINGS = "5f474cf8a1e1ea5d3d30ccbf61bdbaa9521a1317036811549435f97e8f6491ec"


def _with_tokens(final: FinalPlan, tokens: int) -> FinalPlan:
    return replace(final, plan=replace(final.plan, context_tokens=tokens))


def test_g4_context_tokens_is_an_added_field_after_warnings():
    """The field follows the K-3 field ``warnings``, defaults to 0 and carries the omit marker."""
    fields = dataclasses.fields(ResourcePlan)
    names = [f.name for f in fields]
    assert names[-2:] == ["warnings", "context_tokens"]
    field = fields[-1]
    assert field.default == 0 and field.metadata.get(WIRE_OMIT_DEFAULT) is True
    assert sample_final_plan().plan.context_tokens == 0


def test_g4_plan_ids_of_older_plans_are_unchanged():
    """★ A plan without an estimate hashes to the id it had before the field existed — the v1 form (pinned before
    K-3) and a K-3 plan with ``warnings`` and the item fields (pinned on ``7c746df5``) — so stored plan ids, bundle
    manifests and a resumed job's ``final_plan_id`` stay valid.

    Mutant: ``context_tokens: int = 0`` (a plain field without ``added_field``)."""
    old = sample_final_plan()
    assert dplan.plan_id(dplan.final_doc(old)) == PLAN_ID_BEFORE_K3
    assert jsonutil.doc_id(jsonutil.to_wire(old.plan)) == RESOURCE_PLAN_ID_BEFORE_K3
    k3 = _with_added(old)
    assert dplan.plan_id(dplan.final_doc(k3)) == PLAN_ID_K3_WITH_WARNINGS
    assert jsonutil.doc_id(jsonutil.to_wire(k3.plan)) == RESOURCE_PLAN_ID_K3_WITH_WARNINGS
    assert "context_tokens" not in jsonutil.to_wire(k3)["plan"]


def test_g4_context_tokens_round_trip_over_the_wire():
    """A set estimate is in the wire form, decodes to the same plan and changes the plan id; a document without
    the key (an older peer, a stored plan) decodes with 0."""
    final = _with_tokens(_with_added(sample_final_plan()), 12_345)
    wire = jsonutil.to_wire(final)
    assert wire["plan"]["context_tokens"] == 12_345
    back = jsonutil.from_wire(FinalPlan, wire)
    assert back == final and back.plan.context_tokens == 12_345
    assert jsonutil.canonical_bytes(back) == jsonutil.canonical_bytes(final)
    assert jsonutil.from_wire(ResourcePlan, jsonutil.to_wire(final.plan)).context_tokens == 12_345
    assert dplan.plan_id(dplan.final_doc(final)) != PLAN_ID_K3_WITH_WARNINGS
    older = jsonutil.to_wire(_with_added(sample_final_plan()))
    assert jsonutil.from_wire(FinalPlan, older).plan.context_tokens == 0


@pytest.mark.parametrize("value", ["12", True, 1.5, None, [1]])
def test_g4_context_tokens_is_type_checked(value):
    wire = jsonutil.to_wire(sample_final_plan())
    wire["plan"]["context_tokens"] = value
    with pytest.raises(BadRequest) as exc:
        jsonutil.from_wire(FinalPlan, wire)
    assert exc.value.details["field"] == "plan.context_tokens"


def test_g4_plan_uses_the_estimate_and_falls_back_to_the_lineage_total():
    """D reads the field (``plan.context_tokens_of``): the estimate when set, else ``approx_tokens``."""
    final = sample_final_plan()
    assert dplan.context_tokens_of(final) == final.plan.approx_tokens == 4321
    assert dplan.context_tokens_of(_with_tokens(final, 99)) == 99
