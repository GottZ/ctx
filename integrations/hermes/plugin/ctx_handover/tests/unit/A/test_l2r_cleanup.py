"""Layer R (red) for §2 "Bereinigung" (A) of the layer-2 contract:

* ``tests/unit/A/standins.py`` follows E's step names (``c1:<item_id>``, X11/W021) and the real B/D functions the
  production code calls (``map_model``, ``offline_capable``, ``plan.final_doc``, ``plan.instruction_files``,
  segments with ``model``/``model_config``) — every port attribute ``LocalTarget``/``LocalSource`` read exists on the
  stand-in ports;
* the dead ``suspend`` fallback is gone (``LocalSource.suspend`` goes through B's ``lock.suspend`` only);
* job ids in the A tests are ULIDs (K-2 ``ids.ULID_RE``, X25).
"""

from __future__ import annotations

import ast
import re
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import ids, local_source, local_target, session_import
from ctx_handover.engine import plan as planning
from ctx_handover.tests.unit.A import standins
from ctx_handover.tests.unit.A.test_l2_source_real import mk  # noqa: F401 - fixture of the real-module source tests

HERE = Path(__file__).resolve().parent
OWN = {"_l2r_helpers.py"}


def _port_reads(module) -> set[tuple[str, str]]:
    """``(namespace, name)`` of every ``self.ports.<ns>.<name>`` read in ``module`` — also through a local alias
    (``p = self.ports``, ``rec = self.ports.ctxrecords``)."""
    src = Path(module.__file__).read_text(encoding="utf-8")
    found = set(re.findall(r"self\.ports\.(\w+)\.(\w+)", src))
    for alias in set(re.findall(r"\b(\w+) = self\.ports\b(?!\.)", src)):
        found |= set(re.findall(rf"\b{alias}\.(\w+)\.(\w+)", src))
    for alias, ns in set(re.findall(r"\b(\w+) = self\.ports\.(\w+)\b(?!\.)", src)):
        found |= {(ns, name) for name in re.findall(rf"\b{alias}\.(\w+)", src)}
    return found


@pytest.mark.parametrize(
    ("module", "ports"),
    [(local_target, standins.target_ports), (local_source, lambda: standins.source_ports(()))],
    ids=["target", "source"],
)
def test_standin_ports_offer_every_port_the_production_code_reads(module, ports):
    stand = ports()
    reads = {(ns, name) for ns, name in _port_reads(module) if hasattr(stand, ns)}
    assert reads, "no port reads found (pattern out of date?)"
    missing = sorted(f"{ns}.{name}" for ns, name in reads if not hasattr(getattr(stand, ns), name))
    assert missing == []


def test_standin_target_ports_use_the_real_b_and_d_functions():
    """The pure B/D functions are not re-implemented in the stand-ins: ``map_model``/``offline_capable``
    are B's, ``final_doc``/``instruction_files`` D's; the stand-in segments carry ``model``/``model_config``."""
    p = standins.target_ports()
    assert getattr(p.session_import, "map_model", None) is session_import.map_model
    assert getattr(p.session_import, "offline_capable", None) is session_import.offline_capable
    assert getattr(p.plan, "final_doc", None) is planning.final_doc
    assert getattr(p.plan, "instruction_files", None) is planning.instruction_files
    seg = standins._Seg("old", "new")
    assert hasattr(seg, "model") and hasattr(seg, "model_config")


def test_standin_txn_names_steps_like_e(tmp_path):
    """E's ``WorkspaceTxn.commit_steps`` names a c1 step ``c1:<item_id>`` (X11); the stand-in does the same, so
    ``LocalTarget`` journals one name whichever transaction runs (no second prefixing, W021)."""
    txn = standins.WorkspaceTxn("01JC5000000000000000000000", tmp_path, None, None, lambda *a: None)
    txn.items["w0"] = {"partial": str(tmp_path / "p"), "dest": str(tmp_path / "d"), "mode": "fresh"}
    assert [name for name, _fwd, _inv in txn.commit_steps()] == ["c1:w0"]


def test_source_suspend_has_no_fallback(mk):  # noqa: F811
    """DESIGN §7.3 only through B's ``lock.suspend``: a lock port without ``suspend`` is a programming error, not a
    silent ledger write (the removed layer-1 fallback set ``suspended`` itself)."""
    env = mk(remote_driven=True)
    job = env.job()
    plan = env.plan(job)
    env.source.lock(job, env.ref, plan.revision)
    real = env.source.ports.lock
    names = {n: getattr(real, n) for n in dir(real) if not n.startswith("_") and n != "suspend"}
    env.source._ports = SimpleNamespace(**{**vars(env.source.ports), "lock": SimpleNamespace(**names)})
    with pytest.raises(AttributeError):
        env.source.suspend(job)
    assert env.ledger.source_state(job.job_id)["state"] != "suspended"


_ID_KEYS = {"job_id", "last_job_id"}
_JOB_METHODS = {
    "create_job",
    "job",
    "job_update",
    "transition",
    "event",
    "claim_owner",
    "job_object",
    "job_objects",
    "job_objects_add",
    "source_state",
    "source_set",
}
"""Ledger methods whose first argument is a job id."""


def _job_id_literals(path: Path) -> list[tuple[int, str]]:
    """String constants used as job ids: ``job_id=``/``last_job_id=`` keywords, ``"job_id": …`` dict entries, the
    first argument of the ledger's job methods and every 26-character literal starting with ``01`` (the ULID
    look-alikes)."""
    tree = ast.parse(path.read_text(encoding="utf-8"))
    out: list[tuple[int, str]] = []
    for node in ast.walk(tree):
        if isinstance(node, ast.keyword) and node.arg in _ID_KEYS and isinstance(node.value, ast.Constant):
            if isinstance(node.value.value, str):
                out.append((node.value.lineno, node.value.value))
        elif isinstance(node, ast.Call) and isinstance(node.func, ast.Attribute) and node.func.attr in _JOB_METHODS:
            first = node.args[0] if node.args else None
            if isinstance(first, ast.Constant) and isinstance(first.value, str):
                out.append((first.lineno, first.value))
        elif isinstance(node, ast.Dict):
            for k, v in zip(node.keys, node.values, strict=True):
                if isinstance(k, ast.Constant) and k.value in _ID_KEYS and isinstance(v, ast.Constant):
                    out += [(v.lineno, v.value)] if isinstance(v.value, str) else []
        elif isinstance(node, ast.Constant) and isinstance(node.value, str) and len(node.value) == 26:
            out += [(node.lineno, node.value)] if node.value.startswith("01") else []
    return out


def test_job_ids_in_the_a_tests_are_ulids():
    bad = [
        f"{p.name}:{line}: {value!r}"
        for p in sorted(HERE.glob("*.py"))
        if p.name not in OWN
        for line, value in _job_id_literals(p)
        if not ids.is_ulid(value)
    ]
    assert bad == []
