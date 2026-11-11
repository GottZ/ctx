"""Review round 3: README disclosure (catalog rule 13) bound to the code.

- R2-L5 (the second review §3): reading the password database (``pwd.getpwall()``, target denylist for
  every account's home) was missing under **Reads**; the thread ``ctxh-hook-runtime`` was missing under **Background
  work**, which otherwise names threads. The test below collects every thread name the runtime code starts
  (``threading.Thread(name=…)``, ``thread_name_prefix=``) and every ``threading.Timer`` and requires each in that
  paragraph — the next thread cannot be added silently.
- M33 rest (the second review §5, g7): **Writes** said "workspace copies under ``allow_roots``" without the exceptions;
  the third ``recorded`` case of ``local_target.negotiate`` (a destination this peer delivered into here before, the
  ledger's ``in`` row) was named nowhere.
- M34 rest (the second review §5): ``Identity.load`` reads the ``.env`` also when
  ``get_secret`` returns a key that does not reproduce the ledger's public key; the README said "only when
  ``get_secret`` does not return it"."""

from __future__ import annotations

import ast
import inspect
from pathlib import Path

from ctx_handover.engine import crypto, local_target, policy
from ctx_handover.tests.unit.F._docs import docs_text

PLUGIN_ROOT = Path(policy.__file__).resolve().parents[1]


def _text() -> str:
    return " ".join(docs_text().split())


def _between(start: str, end: str) -> str:
    return _text().split(start, 1)[1].split(end, 1)[0]


def _runtime_sources() -> list[Path]:
    files = sorted((PLUGIN_ROOT / "engine").glob("*.py")) + sorted((PLUGIN_ROOT / "dashboard").glob("*.py"))
    return [*files, PLUGIN_ROOT / "__init__.py"]


def _literal_prefix(node: ast.expr) -> str | None:
    """A thread name as written: a string, or the literal start of an f-string (``ctxh-job-{…}`` → ``ctxh-job-``)."""
    if isinstance(node, ast.Constant) and isinstance(node.value, str):
        return node.value
    if isinstance(node, ast.JoinedStr) and node.values and isinstance(node.values[0], ast.Constant):
        return str(node.values[0].value)
    return None


def _threads() -> tuple[set[str], int]:
    names: set[str] = set()
    timers = 0
    for path in _runtime_sources():
        for node in ast.walk(ast.parse(path.read_text(encoding="utf-8"))):
            if not isinstance(node, ast.Call):
                continue
            func = node.func
            attr = func.attr if isinstance(func, ast.Attribute) else getattr(func, "id", "")
            if attr == "Timer":
                timers += 1
            for kw in node.keywords:
                if kw.arg in {"name", "thread_name_prefix"} and attr in {"Thread", "ThreadPoolExecutor"}:
                    name = _literal_prefix(kw.value)
                    assert name, f"{path.name}:{node.lineno}: a thread name the test cannot read"
                    names.add(name)
    return names, timers


def test_r3_f_l5_every_thread_is_named_under_background_work():
    """★ Every thread name the runtime code starts appears (as written, an f-string by its literal start) in
    **Background work**, and the git watchdog timer is mentioned.

    Red before the change: ``ctxh-hook-runtime`` (and ten other names) missing."""
    names, timers = _threads()
    assert "ctxh-hook-runtime" in names and "ctxh-janitor" in names, names
    work = _between("**Background work", "**Retention:**")
    missing = sorted(n for n in names if f"`{n}" not in work)
    assert missing == [], (missing, work)
    assert timers >= 1 and "a timer per git call" in work, work


def test_r3_f_l5_reads_name_the_password_database():
    """★ ``policy`` reads the password database (``pwd.getpwall``) for the target denylist; **Reads** says so.

    Red before the change: not under Reads (only as a deny target under Rights)."""
    assert "getpwall" in inspect.getsource(policy)
    reads = _between("**Reads:**", "**Writes:**")
    assert "the home folders of this host's accounts from the password database (`pwd`)" in reads, reads


def test_r3_f_m33_writes_name_every_destination_outside_allow_roots():
    """★ **Writes** names the three ``recorded`` cases of ``local_target.negotiate`` (return trip, earlier delivery of
    the same peer) and the destination typed here, as the Settings row does.

    Red before the change: "workspace copies under `allow_roots`" without exceptions; the earlier-delivery case
    nowhere."""
    src = inspect.getsource(local_target.LocalTarget.negotiate)
    assert "map_out(" in src and "map_in(" in src and "return-mapping" in src and "local_choice" in src
    writes = _between("**Writes:**", "**Commands:**")
    for part in (
        "workspace copies under `allow_roots` (default: `~/hermes-handover`)",
        "a repository returning to the folder this machine handed it away from",
        "a `--dest`/`--map` typed here",
        "a folder the same peer delivered into here before",
    ):
        assert part in writes, (part, writes)
    settings_row = _between("| `workspace_root`, `allow_roots` |", "| `export_roots` |")
    assert "a folder the same peer delivered into here before" in settings_row, settings_row


def test_r3_f_m34_reads_say_when_the_env_file_is_read():
    """★ ``Identity.load`` reads the ``.env`` afresh when ``get_secret`` has no key or one that does not match the
    ledger; **Reads** says exactly that.

    Red before the change: "only when `get_secret` does not return it"."""
    src = inspect.getsource(crypto.Identity.load)
    assert src.index("read_secret(IDENTITY_ENV)") < src.index("compare_digest") < src.index("read_env_fresh(")
    reads = _between("**Reads:**", "**Writes:**")
    assert "only when `get_secret` does not return it" not in reads
    assert (
        "only when `get_secret` returns no key or one that does not match the public key in the ledger (a process "
        "started before the key was created or rotated)" in reads
    ), reads
