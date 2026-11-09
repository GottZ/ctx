"""Package i-f23 (L3-F23): the README sentence on the return trip's fresh folder, bound to the rule behind it.

The row "Back to the server after landing" says where a returning repository lands when its original folder cannot
take it: ``<folder>-handover-<time>`` beside it under ``allow_roots``, else ``<workspace_root>/<name>-handover-<time>``.
The test reads that sentence and asks the workspace transaction's one fallback rule (``WorkspaceTxn._fallback``,
which every conflict fallback of PREPARE goes through) for both cases, so a change on either side turns it red."""

from __future__ import annotations

import time

from ctx_handover.engine import materialize
from ctx_handover.tests.fakes.git_fake import DirBlockStore, RecordingJournal
from ctx_handover.tests.unit.E._support import AT, make_cfg

from ._l2r_helpers import FLAT

SENTENCE = (
    "repositories return into their original folders when nothing changed there meanwhile; otherwise (also when a "
    "file git ignores there is in the way) they land in a fresh folder — `<folder>-handover-<time>` beside it when "
    "that lies under `allow_roots`, else `<workspace_root>/<name>-handover-<time>` — which the final message names"
)


def test_i_f23_readme_names_where_a_return_lands_when_the_origin_cannot_take_it(tmp_path, user_home, hermes_home):
    """★ README row + rule: an origin outside ``allow_roots`` (``~/proj``) falls back under ``workspace_root``, one
    under it keeps the sibling (mutant: the old sibling rule ⇒ ``~/proj-handover-<time>``)."""
    assert SENTENCE in FLAT
    cfg = make_cfg(user_home, hermes_home)
    txn = materialize.WorkspaceTxn(
        "01JOB00000000000000000F23RD",
        tmp_path / "txn",
        None,
        DirBlockStore(tmp_path / "s"),
        RecordingJournal(),
        cfg=cfg,
        now=lambda: AT,
    )
    stamp = time.strftime("%Y%m%d-%H%M%S", time.localtime(AT))
    root = user_home / "hermes-handover"
    assert txn._fallback(user_home / "proj") == root / f"proj-handover-{stamp}"
    assert txn._fallback(root / "team" / "proj") == root / "team" / f"proj-handover-{stamp}"
