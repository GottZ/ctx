"""K0: model fields added in layer 2 (contract K-3; W003, W072, W073, W075).

``PlanItem.hermes_kind``, ``PlanItem.instruction_files``, ``ResourcePlan.warnings`` and
``EnvFacts.python_version`` are declared with ``model.added_field``: the wire form omits them at their default,
so documents and ids that do not use them keep the bytes they had before the fields existed. The pinned ids
below were computed with the model of commit ``c32a862d`` (before K-3) by
``lab/L2-redproof/wp-k0/pin_ids.py`` (output ``pinned-before.txt``) from exactly these documents.
"""

from __future__ import annotations

import dataclasses
from dataclasses import replace

import pytest

from ctx_handover.engine import jsonutil
from ctx_handover.engine import plan as dplan
from ctx_handover.engine.errors import BadRequest
from ctx_handover.engine.model import (
    WIRE_OMIT_DEFAULT,
    Conflict,
    DestMode,
    EnvFacts,
    FinalPlan,
    GitInfo,
    HostFacts,
    Include,
    ItemKind,
    ItemRole,
    Origin,
    PlanItem,
    RefineReport,
    ResourcePlan,
    TargetDecision,
    ToolInfo,
)

PLAN_ID_BEFORE_K3 = "68728d89c933aed95052c6ed26cc313ded43440e7d9c94d9370d23ad5728c00b"
RESOURCE_PLAN_ID_BEFORE_K3 = "2b73fd3063dd5b329f2efe0fbdc439760c61501a086fbbab5d51f69917c5ccad"
ENV_FACTS_ID_BEFORE_K3 = "4bff44ad83d661b681b2deb83af53a7396acb9ac398b34b2d7bd6fc5157fb494"

ADDED = {
    PlanItem: {"hermes_kind": None, "instruction_files": (), "cwd_bytes": None, "cwd_tracked": None},
    ResourcePlan: {"warnings": (), "context_tokens": 0},
    EnvFacts: {"python_version": "", "workspace_root_ok": None, "workspace_root_problem": ""},
}


def sample_final_plan() -> FinalPlan:
    git = GitInfo(
        head="a" * 40,
        branch="main",
        object_format="sha1",
        shallow=False,
        dirty=True,
        in_progress=None,
        lfs=False,
        submodules=(),
        remotes=(("origin", "https://example.invalid/r.git"),),
        root_commits=("b" * 40,),
        untracked_bytes=12,
        untracked_count=1,
        denied_tracked=(".env",),
    )
    items = (
        PlanItem(
            "i1",
            ItemKind.GIT_REPO,
            "/home/u/src/proj",
            ItemRole.PRIMARY,
            Origin.CWD,
            Include.YES,
            ("cwd",),
            1234,
            git=git,
            suggested_dest="~/src/proj",
            rel_cwd="sub",
        ),
        PlanItem(
            "i2",
            ItemKind.EXTRA_FILE,
            "/home/u/notes.md",
            ItemRole.EXTRA,
            Origin.TRANSCRIPT,
            Include.PROPOSED,
            ("proposed", "outside_workspace"),
            99,
        ),
        PlanItem(
            "i3", ItemKind.HERMES_FILE, "/hh/cache/spillover/x.txt", ItemRole.HERMES, Origin.CWD, Include.YES, (), 7
        ),
    )
    plan = ResourcePlan(
        job_id="01JC0000000000000000000ABC",
        revision=(3, "r1", (1, 2)),
        items=items,
        not_transferred=(),
        processes=({"pid": 1, "cmd": "x"},),
        delegations=(),
        cron=(),
        refine=RefineReport(used="off"),
        tools_needed=("git",),
        env_vars_needed=("API_KEY",),
        notes_for_target="n",
        approx_tokens=4321,
    )
    decisions = (
        TargetDecision("i1", "/home/t/src/proj", DestMode.FRESH, "free", need_bytes=1234),
        TargetDecision(
            "i2",
            "/home/t/notes.md",
            DestMode.UPDATE,
            "user",
            conflict=Conflict("dirty", "local edits", "/home/t/notes-1.md"),
            warnings=("w",),
        ),
        TargetDecision("i3", "/th/cache/spillover/x.txt", DestMode.SKIP, "no"),
    )
    return FinalPlan(
        plan_id="",
        plan=plan,
        decisions=decisions,
        need_bytes=1333,
        free_bytes={"/home/t": 10**9},
        model_change=None,
    )


def sample_env_facts() -> EnvFacts:
    host = HostFacts(
        is_wsl=False,
        is_container=True,
        profile="default",
        multiplex=False,
        hermes_version="0.22.6",
        plugin_version="0.1.0",
        model="m",
        provider="p",
        context_length=8192,
        terminal_backend="local",
        hermes_home="/hh",
        document_cache="/hh/cache/documents",
    )
    return EnvFacts(  # noqa: S604 - EnvFacts.shell is a fact, not a subprocess option
        instance_name="lap",
        os_family="linux",
        os_name="Linux",
        os_version="6.1",
        arch="x86_64",
        hostname="h",
        user="u",
        home="/home/u",
        shell="/bin/zsh",
        host=host,
        tools={"git": ToolInfo("/usr/bin/git", "2.45.0")},
        tools_seen={"git": "2.45.0", "rg": None},
        workspace_root="/home/u/src",
        allow_roots=("/home/u",),
        disk_free={"/home/u": 5},
        case_insensitive={"/home/u": False},
        offline_capable=True,
        locale="C.UTF-8",
        timezone="UTC",
        git_version="2.45.0",
        git_lfs=False,
        capabilities=frozenset({"b", "a"}),
    )


def _with_added(final: FinalPlan) -> FinalPlan:
    items = list(final.plan.items)
    items[2] = replace(items[2], hermes_kind="spillover")
    items[0] = replace(items[0], instruction_files=(("AGENTS.md", "c" * 64), ("sub/CLAUDE.md", "d" * 64)))
    return replace(final, plan=replace(final.plan, items=tuple(items), warnings=("resume_guard",)))


def test_ids_of_documents_without_added_fields_are_unchanged():
    """★ A plan, resource plan and env-facts document that do not set the added fields hash to exactly the ids
    computed before K-3 — ``plan.plan_id`` (D, X02) included — so stored plan ids and bundle manifests stay valid.

    Mutant: drop the ``WIRE_OMIT_DEFAULT`` skip in ``jsonutil._encode`` (added fields always serialised).
    """
    final = sample_final_plan()
    assert dplan.plan_id(dplan.final_doc(final)) == PLAN_ID_BEFORE_K3
    doc = jsonutil.to_wire(final)
    doc.pop("plan_id")
    assert jsonutil.doc_id(doc) == PLAN_ID_BEFORE_K3
    assert jsonutil.doc_id(jsonutil.to_wire(final.plan)) == RESOURCE_PLAN_ID_BEFORE_K3
    assert jsonutil.doc_id(jsonutil.to_wire(sample_env_facts())) == ENV_FACTS_ID_BEFORE_K3
    assert jsonutil.doc_id(sample_env_facts()) == ENV_FACTS_ID_BEFORE_K3
    wire = jsonutil.to_wire(final)
    assert all(not {"hermes_kind", "instruction_files"} & set(item) for item in wire["plan"]["items"])
    assert "warnings" not in wire["plan"]
    assert "python_version" not in jsonutil.to_wire(sample_env_facts())


def test_added_fields_round_trip_and_change_the_id_when_set():
    final = _with_added(sample_final_plan())
    wire = jsonutil.to_wire(final)
    assert wire["plan"]["items"][2]["hermes_kind"] == "spillover"
    assert wire["plan"]["items"][0]["instruction_files"] == [["AGENTS.md", "c" * 64], ["sub/CLAUDE.md", "d" * 64]]
    assert "hermes_kind" not in wire["plan"]["items"][0]
    assert wire["plan"]["warnings"] == ["resume_guard"]
    back = jsonutil.from_wire(FinalPlan, wire)
    assert back == final
    assert jsonutil.canonical_bytes(back) == jsonutil.canonical_bytes(final)
    assert dplan.plan_id(dplan.final_doc(final)) != PLAN_ID_BEFORE_K3
    facts = replace(sample_env_facts(), python_version="Python 3.14.0")
    assert jsonutil.from_wire(EnvFacts, jsonutil.to_wire(facts)) == facts
    assert jsonutil.doc_id(facts) != ENV_FACTS_ID_BEFORE_K3


def test_documents_written_before_k3_decode_with_defaults():
    old = jsonutil.to_wire(sample_final_plan())
    back = jsonutil.from_wire(FinalPlan, old)
    assert back == sample_final_plan()
    assert all(i.hermes_kind is None and i.instruction_files == () for i in back.plan.items)
    assert back.plan.warnings == ()
    assert jsonutil.from_wire(EnvFacts, jsonutil.to_wire(sample_env_facts())).python_version == ""


@pytest.mark.parametrize(
    ("patch", "field"),
    [
        (lambda w: w["plan"]["items"][0].__setitem__("hermes_kind", 5), "plan.items[0].hermes_kind"),
        (
            lambda w: w["plan"]["items"][0].__setitem__("instruction_files", [["a"]]),
            "plan.items[0].instruction_files[0]",
        ),
        (
            lambda w: w["plan"]["items"][0].__setitem__("instruction_files", [["a", 1]]),
            "plan.items[0].instruction_files[0][1]",
        ),
        (lambda w: w["plan"].__setitem__("warnings", "busy"), "plan.warnings"),
    ],
)
def test_added_fields_are_type_checked(patch, field):
    wire = jsonutil.to_wire(sample_final_plan())
    patch(wire)
    with pytest.raises(BadRequest) as exc:
        jsonutil.from_wire(FinalPlan, wire)
    assert exc.value.details["field"] == field


def test_added_fields_are_declared_additively():
    """Each K-3 field sits after the v1 fields, has its contract default and carries the omit marker; no field
    without the marker ever disappears from the wire form."""
    for cls, wanted in ADDED.items():
        fields = dataclasses.fields(cls)
        names = [f.name for f in fields]
        assert names[-len(wanted) :] == list(wanted)
        for f in fields:
            if f.name in wanted:
                assert f.default == wanted[f.name] and f.metadata.get(WIRE_OMIT_DEFAULT) is True
            else:
                assert not f.metadata.get(WIRE_OMIT_DEFAULT)
