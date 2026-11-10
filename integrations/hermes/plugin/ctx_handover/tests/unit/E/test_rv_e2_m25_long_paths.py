"""First review round, finding M25 (gitapply part, wish to the integrator): a Windows target checks git paths with
long-path support — git runs there with ``core.longpaths=true`` (DESIGN §9.2, §9.4) — so a path over 260 characters
no longer sends the item to ``NO_CHECKOUT``/the fallback; names NTFS cannot hold still do."""

from __future__ import annotations

from ctx_handover.engine import gitapply
from ctx_handover.engine.paths import rules_for


def test_rv_e2_m25_long_git_path_is_no_issue_on_windows():
    """★ M25 (mutant: ``path_issues`` with the plain Windows rules ⇒ the long path is an issue)."""
    win = rules_for("windows", True)
    long_path = "/".join(["d" * 60] * 5) + "/file.txt"
    assert len(long_path) > 260
    assert gitapply.path_issues([long_path, "ok.txt"], win) == []
    assert [p for p, _ in gitapply.path_issues(["con.txt", "a:b", long_path], win)] == ["con.txt", "a:b"]
