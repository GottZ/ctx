"""Fault matrix against the real modules — full cross matrix (wp-int1, marker ``slow``; selection rules in
``conftest.py``): every call of both participants (``harness.CASES`` = the 32 call points of the fake matrix) × every
fault kind, push and pull, wherever the kind exists for that participant (link faults and participant restarts only
for the remote one — a local participant shares the conductor's process). Same driver and promises as
``test_int_fault_matrix_std.py``; a fault point the job never reaches (``target.status`` outside IN_DOUBT, release
calls on a successful run) leaves a plain run that must still keep the promises.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine.model import Mode
from ctx_handover.tests.integration.harness import CASES, FAULT_KINDS, applicable, assert_case

pytestmark = pytest.mark.slow

FULL = [
    (mode, side, method, kind)
    for mode in (Mode.PUSH, Mode.PULL)
    for side, method in CASES
    for kind in FAULT_KINDS
    if applicable(mode, side, kind)
]


@pytest.mark.parametrize(("mode", "side", "method", "kind"), FULL, ids=[f"{m}-{s}.{c}-{k}" for m, s, c, k in FULL])
def test_int_fault_matrix_full(fault_world, mode, side, method, kind):
    assert_case(fault_world.run_case(mode, side, method, kind))
