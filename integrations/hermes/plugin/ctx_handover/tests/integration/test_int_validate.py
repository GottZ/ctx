"""``hermes plugins validate`` on this plugin tree (TESTPLAN §3 ``test_int_validate_plugin``; BRIEF D15/D16,
catalog gate).

The real CLI of the upstream venv runs with a disposable home (never the user's ``~/.hermes``). Expected: the
validation passes, every check is ok, and every warning is one of the justified findings below — each pinned to
the construct it was justified for, so a new finding at another place fails the test."""

from __future__ import annotations

import json
import re

from ctx_handover.tests.integration._int2_helpers import PLUGIN_ROOT, hermes_cli
from ctx_handover.tests.integration.harness import Home

#: ``(scanner finding, file name)`` → (text the reported line must contain, justification).
JUSTIFIED = {
    ("proc_access", "lock.py"): (
        "/proc/self/ns/pid",
        "read-only readlink of the own PID namespace for the turn-lease holder stamp, the core's own format "
        "(DESIGN §7.2): a dead holder is freed at once instead of after the TTL [LAB L6]",
    ),
    ("proc_access", "ledger.py"): (
        "/proc/self/ns/pid",
        "read-only reads of the boot id and the own PID-namespace inode for the ledger's job-owner stamp "
        "(G-prime): a row whose owner process is dead on this host is taken over at once instead of after "
        "the stale window; nothing is written under /proc",
    ),
    ("setuid_setgid", "test_filetree.py"): (
        "setuid",
        "unit test of cluster E proving that tree entries with setuid/setgid/sticky modes from a peer are skipped "
        "and reported, never created (SEC 2.3); test code, not shipped behaviour",
    ),
}
_FINDING = re.compile(r"([a-z_]+) \(([^():]+):(\d+)\)")


def _justify(warning: str) -> list[str]:
    """Problems with one warning line (empty = fully justified)."""
    prefix = "security scan caution: "
    if not warning.startswith(prefix):
        return [f"unjustified warning: {warning!r}"]
    problems = []
    findings = _FINDING.findall(warning[len(prefix) :])
    if not findings:
        return [f"unparsed security finding: {warning!r}"]
    for kind, name, line in findings:
        entry = JUSTIFIED.get((kind, name))
        if entry is None:
            problems.append(f"unjustified finding {kind} in {name}:{line}")
            continue
        marker, _why = entry
        files = sorted(p for p in PLUGIN_ROOT.rglob(name) if "__pycache__" not in p.parts)
        if len(files) != 1:
            problems.append(f"{name} is not one file of the plugin: {files}")
            continue
        lines = files[0].read_text(encoding="utf-8").splitlines()
        text = lines[int(line) - 1] if 0 < int(line) <= len(lines) else ""
        if marker not in text:
            problems.append(f"{kind} at {name}:{line} is not the justified construct ({marker!r}): {text!r}")
    return problems


def test_int_validate_plugin(tmp_path):
    validator = Home(tmp_path, "validator")
    res = hermes_cli(validator, "plugins", "validate", str(PLUGIN_ROOT), "--json")
    assert res.returncode == 0, (res.returncode, res.stdout[-3000:], res.stderr[-3000:])
    report = json.loads(res.stdout)
    assert report["ok"] is True, report
    failed = [c for c in report["checks"] if not c["ok"]]
    assert not failed, failed
    problems = [p for w in report.get("warnings") or () for p in _justify(w)]
    assert not problems, (problems, report.get("warnings"))
