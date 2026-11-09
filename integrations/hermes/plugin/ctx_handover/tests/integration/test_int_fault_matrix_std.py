"""Fault matrix against the real modules — standard selection (wp-int1; layer-2 counterpart of the fake matrix
``tests/unit/A/test_fault_matrix.py``).

Real ``SessionDB`` in two homes, real git, real block stores, the listener as a real HTTP process on loopback with
signed routes, the conductor in its own process (``harness.FaultWorld``). Faults are injected at real seams
(``harness.rules_for``): lost connection before/after effect in the HTTP transport, the conductor process SIGKILLed
before/after a call, the listener process SIGKILLed after a request took effect or inside a running operation (and
restarted on the same port), error envelopes of the participant, the user's cancel verb from another process, a link
that stays down. Then the test drives the job like the product (resume, janitor adoption after the 60 s stale window,
retries) until DONE or ABORTED and checks the three promises (``harness.assert_case``).

Selection: every fault kind at least once in every phase of the job, push and pull — the call of a phase is the first
of its candidates that can carry the fault (link faults and participant restarts need the remote participant, DESIGN
§3.5). A pull commits locally and a push retires locally: those phases have no link fault. The full cross matrix is
``test_int_fault_matrix_full.py`` (marker ``slow``).
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.model import Mode
from ctx_handover.tests.integration.harness import FAULT_KINDS, applicable, assert_case

PHASES = {
    "planning": (("target", "negotiate"), ("source", "start_plan")),
    "transfer": (("target_blocks", "write_blocks"), ("source_blocks", "read_blocks")),
    "prepare": (("target", "start_prepare"), ("source", "start_prepare")),
    "commit": (("target", "start_commit"), ("target", "complete:commit")),
    "retire": (("source", "start_retire"), ("source", "complete:retire")),
}


def _selection() -> list[tuple[Mode, str, str, str, str]]:
    out = []
    for mode in (Mode.PUSH, Mode.PULL):
        for phase, candidates in PHASES.items():
            for kind in FAULT_KINDS:
                pick = next(((s, m) for s, m in candidates if applicable(mode, s, kind)), None)
                if pick is not None:
                    out.append((mode, phase, pick[0], pick[1], kind))
    return out


SELECTION = _selection()


@pytest.mark.parametrize(
    ("mode", "phase", "side", "method", "kind"),
    SELECTION,
    ids=[f"{m}-{p}-{s}.{c}-{k}" for m, p, s, c, k in SELECTION],
)
def test_int_fault_matrix_standard(fault_world, mode, phase, side, method, kind):
    res = fault_world.run_case(mode, side, method, kind)
    assert res.fault_fired, f"the fault was never reached in phase {phase}: {res.describe()}"
    assert_case(res)
