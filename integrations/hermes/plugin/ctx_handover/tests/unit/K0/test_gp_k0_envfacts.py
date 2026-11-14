"""K0 round G′, V40 / R29 (integrator point (1), wish): ``EnvFacts`` carries the target's own check of
its workspace root — ``workspace_root_ok`` (``None`` = not checked) and ``workspace_root_problem`` — as fields added
after the v1 wire form (contract K-3): facts that do not set them keep their id, set fields survive the wire."""

from __future__ import annotations

import dataclasses
from dataclasses import replace

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine.errors import BadRequest
from ctx_handover.engine.model import WIRE_OMIT_DEFAULT, EnvFacts
from ctx_handover.tests.unit.K0.test_model_added_fields import ENV_FACTS_ID_BEFORE_K3, sample_env_facts


def test_env_facts_declare_the_workspace_root_check_additively():
    """★ Both fields follow the v1 fields with their defaults and the omit marker.

    Red before the fix: ``KeyError: 'workspace_root_ok'`` (the fields did not exist)."""
    fields = {f.name: f for f in dataclasses.fields(EnvFacts)}
    names = list(fields)
    assert names[-2:] == ["workspace_root_ok", "workspace_root_problem"]
    assert fields["workspace_root_ok"].default is None
    assert fields["workspace_root_problem"].default == ""
    for name in ("workspace_root_ok", "workspace_root_problem"):
        assert fields[name].metadata.get(WIRE_OMIT_DEFAULT) is True, name
    assert names.index("workspace_root_ok") > names.index("python_version")


def test_facts_without_the_check_keep_their_id_and_old_documents_decode():
    """★ Facts that leave both fields at their default hash to the id from before K-3 (stored plan ids stay
    valid); a document written before decodes with ``None``/``""``."""
    facts = sample_env_facts()
    assert jsonutil.doc_id(facts) == ENV_FACTS_ID_BEFORE_K3
    wire = jsonutil.to_wire(facts)
    assert "workspace_root_ok" not in wire and "workspace_root_problem" not in wire
    back = jsonutil.from_wire(EnvFacts, wire)
    assert back.workspace_root_ok is None and back.workspace_root_problem == ""


@pytest.mark.parametrize(("ok", "problem"), [(True, ""), (False, "denied"), (False, "not_writable")])
def test_workspace_root_check_round_trips_and_changes_the_id(ok, problem):
    """★ A checked root (usable or not) crosses the wire unchanged and is part of the facts' id."""
    facts = replace(sample_env_facts(), workspace_root_ok=ok, workspace_root_problem=problem)
    wire = jsonutil.to_wire(facts)
    assert wire["workspace_root_ok"] is ok
    back = jsonutil.from_wire(EnvFacts, wire)
    assert back == facts
    assert jsonutil.canonical_bytes(back) == jsonutil.canonical_bytes(facts)
    assert jsonutil.doc_id(facts) != ENV_FACTS_ID_BEFORE_K3


@pytest.mark.parametrize(
    ("field", "value"), [("workspace_root_ok", "yes"), ("workspace_root_ok", 1), ("workspace_root_problem", 5)]
)
def test_workspace_root_check_is_type_checked(field, value):
    """A peer's facts with a wrong type in either field are a ``BadRequest`` naming the field."""
    wire = jsonutil.to_wire(sample_env_facts())
    wire[field] = value
    with pytest.raises(BadRequest) as exc:
        jsonutil.from_wire(EnvFacts, wire)
    assert exc.value.details["field"] == field
