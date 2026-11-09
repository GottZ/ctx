"""Display history after a rewind of a carried copy (finding of layer 2, DESIGN §6.8 step 4, §6.9).

The core hides every copy of a message whose NEWEST copy was rewound (``hermes_state_messages.py:1146-1149``: "a
rewind in a compression child never brings back the parent's copy"). The capsule leaves rewind rows out (DESIGN
§6.8 step 4), so the target no longer holds the rewound newest copy and shows the parent's copy again. The three
hashes cannot see it: H_all skips rewind rows by definition, H_rows/H_model cover only the tip's live rows. Fixing
it changes the capsule contract (rewind rows that hide an older copy would have to travel and path B′ would need a
way to store them), which is a design decision, not a cluster-B code change — this test records the gap and fails
loudly (strict) once it is closed.
"""

from __future__ import annotations

import pytest

from ctx_handover.tests.unit.B.conftest import norm_conv, roundtrip


@pytest.mark.xfail(strict=True, reason="rewind rows are not transferred (DESIGN §6.8 step 4); open design point")
def test_rewound_carried_copy_keeps_parent_copy_hidden(
    make_session, session_db, target_db, store, source_paths, target_cwd
):
    b = make_session
    root = b.create()
    b.turns(root, 3)
    child = b.rotate(root, keep_last=2)
    b.turns(child, 1)
    carried = next(
        r for r in session_db.get_messages(child) if r["role"] == "user" and not r.get("_compressed_summary")
    )
    b.rewind(child, int(carried["id"]))
    _info, cap, _capsule, _p, prepared = roundtrip(session_db, target_db, store, child, source_paths, target_cwd)
    assert (prepared.h_all, prepared.h_rows, prepared.h_model) == (cap.h_all, cap.h_rows, cap.h_model)
    source_display = norm_conv(session_db.get_resume_conversations(child)[1])
    target_display = norm_conv(target_db.get_resume_conversations(prepared.tip_id)[1])
    assert target_display == source_display
