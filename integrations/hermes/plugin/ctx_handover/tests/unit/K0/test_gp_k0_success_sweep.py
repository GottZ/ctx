"""K0 round G′ success sweep (``reports/L2/pkg/Gp-common.md``): a check passes only on an explicitly positive value.

``policy.check_tree_entry`` read a missing ``size`` of a peer's file entry as ``0`` and let the entry through; the
target then failed with a bare ``KeyError`` in ``filetree._write_file`` (``int(entry["size"])``) instead of skipping
the entry as ``special``. Directories and symlinks carry no content, their ``size`` may stay absent.
"""

from __future__ import annotations

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.policy import Verdict


@pytest.mark.parametrize("size", ["missing", None, "12", 1.0, True, -1])
def test_file_entry_needs_an_explicit_size(size):
    """★ A file entry without a whole, non-negative ``size`` is ``special``.

    Red before the fix: ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='special')``
    for the entry without ``size``."""
    entry = {"type": "file", "mode": "0644", "path": "a.txt", "object": "f" * 64}
    if size != "missing":
        entry["size"] = size
    assert policy.check_tree_entry(entry) == Verdict(False, "special")


def test_size_stays_optional_where_there_is_no_content():
    """Directories and symlinks pass without ``size``; a file with ``size`` 0 passes."""
    assert policy.check_tree_entry({"type": "dir", "mode": "0755", "path": "d"}).allowed
    assert policy.check_tree_entry({"type": "symlink", "path": "l", "target": "d"}).allowed
    assert policy.check_tree_entry({"type": "file", "mode": "0644", "size": 0, "path": "e"}).allowed
