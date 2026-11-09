"""Shared fixtures for every ctx_handover test suite (layer K0, owned by the integrator).

Isolation: every test runs with its own ``HOME`` (``<tmp>/home``) and ``HERMES_HOME``
(``<tmp>/hermes-home``), the XDG base directories under that home, and without inherited ``HERMES_*``,
``GIT_*`` or ``CTX_HANDOVER_*`` variables — no test reads or writes the real home, the real Hermes profile or
a git identity of the host.

``HERMES_HOME`` deliberately lies outside ``<tmp>/home/.hermes``: Hermes' live-system guard treats
``expanduser("~")/.hermes`` as the production root (``hermes_state_guard.py:26-39``) and refuses to open a
``state.db`` there from a test. With ``HOME`` redirected, that tripwire now sits on ``<tmp>/home/.hermes`` and
fires if code resolves the platform default instead of ``HERMES_HOME``.

Hermes itself comes from ``sys.path`` (``PYTHONPATH=<hermes-agent checkout>:…``, see BUILD-ORDER §0); no
path is hard-wired. Fixtures that need Hermes fail with a clear message when it is not importable
instead of skipping, so a missing upstream never turns a gate green.

Fixtures
--------
``user_home``     the isolated ``$HOME`` (Path)
``hermes_home``   the isolated ``HERMES_HOME`` = ``<tmp>/hermes-home`` (Path, exists)
``session_db``    a real ``hermes_state.SessionDB`` at ``hermes_home / "state.db"``
``make_session``  ``tests.fakes.sessiondb.SessionBuilder`` over ``session_db`` — lineages are built
                  through the core's own paths only (no SQL, no import flags); ``large_segment`` on it copies a
                  session-wide template per shape (``segment_templates``, contract K-6)
``segment_templates`` the session-wide ``SegmentTemplates`` (below) under the pytest base temp; None with
                  ``--no-segment-templates`` (every ``large_segment`` builds directly)
``clock``         ``tests.fakes.FakeClock``
``fake_host``     ``tests.fakes.hostcompat_fake.FakeHost`` installed over ``engine.hostcompat``
``fake_ctx``      ``tests.fakes.hostcompat_fake.FakeCtx`` (a recording PluginContext stand-in)
"""

from __future__ import annotations

import importlib.util
import json
import os
import sqlite3
import sys
import threading
import uuid
from collections.abc import Callable, Mapping
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import pytest

PLUGIN_ROOT = Path(__file__).resolve().parents[1]
if str(PLUGIN_ROOT.parent) not in sys.path:
    sys.path.insert(0, str(PLUGIN_ROOT.parent))

_SCRUBBED_PREFIXES = ("HERMES_", "GIT_", "CTX_HANDOVER_")


def upstream_root() -> Path | None:
    """Directory of the importable hermes-agent checkout, or None."""
    spec = importlib.util.find_spec("hermes_state")
    if spec is None or not spec.origin:
        return None
    return Path(spec.origin).resolve().parent


def require_upstream() -> Path:
    """The hermes-agent checkout; fails the test (never skips) when Hermes is not importable."""
    root = upstream_root()
    if root is None:
        pytest.fail(
            "hermes-agent is not importable: run pytest with PYTHONPATH=<hermes-agent checkout>:<plugin parent>",
            pytrace=False,
        )
    return root


@pytest.fixture(autouse=True)
def _isolated_env(tmp_path, monkeypatch):
    for key in list(os.environ):
        if key.startswith(_SCRUBBED_PREFIXES):
            monkeypatch.delenv(key, raising=False)
    home = tmp_path / "home"
    home.mkdir()
    hermes = tmp_path / "hermes-home"
    hermes.mkdir()
    monkeypatch.setenv("HOME", str(home))
    monkeypatch.setenv("USERPROFILE", str(home))
    monkeypatch.setenv("HERMES_HOME", str(hermes))
    for name, rel in (
        ("XDG_CONFIG_HOME", ".config"),
        ("XDG_DATA_HOME", ".local/share"),
        ("XDG_CACHE_HOME", ".cache"),
        ("XDG_STATE_HOME", ".local/state"),
    ):
        monkeypatch.setenv(name, str(home / rel))
    return home


@pytest.fixture
def user_home(_isolated_env) -> Path:
    return _isolated_env


@pytest.fixture
def hermes_home(user_home) -> Path:
    return Path(os.environ["HERMES_HOME"])


@pytest.fixture
def clock():
    from ctx_handover.tests.fakes import FakeClock

    return FakeClock()


@pytest.fixture
def session_db(hermes_home):
    require_upstream()
    from hermes_state import SessionDB

    db = SessionDB(hermes_home / "state.db")
    try:
        yield db
    finally:
        db.close()


# ── session-wide segment templates (contract K-6, W001) ───────────────────────────────────────────────

#: A copy keeps the target's own ``state_meta`` rows (``key TEXT PRIMARY KEY, value TEXT``,
#: ``hermes_state_common.py:646-649``): they carry the per-file identity ``store_instance_id``,
#: ``store_created_at_utc`` and ``db_file_generation`` (``hermes_state.py:1184-1223``).
_IDENTITY_SELECT = "SELECT key, value FROM state_meta"
_IDENTITY_UPSERT = "INSERT OR REPLACE INTO state_meta (key, value) VALUES (?, ?)"


@dataclass(frozen=True)
class _Template:
    path: Path
    sid: str
    counters: tuple[float | None, int]


def _table_counts(conn: sqlite3.Connection) -> dict[str, int]:
    counts = {}
    for (name,) in conn.execute("SELECT name FROM sqlite_master WHERE type = 'table' ORDER BY name").fetchall():
        quoted = '"' + name.replace('"', '""') + '"'
        counts[name] = conn.execute(f"SELECT count(*) FROM {quoted}").fetchone()[0]  # noqa: S608 - sqlite_master names
    return counts


def _schema(conn: sqlite3.Connection) -> list[tuple]:
    return conn.execute("SELECT type, name, tbl_name, sql FROM sqlite_master ORDER BY type, name").fetchall()


class SegmentTemplates:
    """Session-wide template databases for ``tests.fakes.sessiondb.large_segment`` (contract K-6, W001).

    The first eligible call of a shape builds it once — by the requesting builder's ``twin`` over a fresh
    ``SessionDB`` under ``root``, i.e. through the same core paths as a direct build — and later calls of the same
    shape only copy. A copy is ``sqlite3.Connection.backup`` into the requesting test's database, which keeps its
    own file identity: the header ``application_id`` (``SessionDB`` compares it on every write and stops with
    ``StateDbReplacedError`` otherwise, ``hermes_state.py:1240-1250``) and its ``state_meta`` identity rows. The
    open ``SessionDB`` then keeps working as if it had written the rows itself, and the builder adopts the
    counters the direct build would have left.

    Eligible (else ``copy_into`` returns None and the caller builds directly):

    * the builder's clock is a ``tests.fakes.FakeClock`` (the builder never moves it) and ``create_kw`` is JSON;
      the template key is the shape, ``create_kw``, the clock time, the counters and the tick;
    * ``builder.db`` is a writable ``SessionDB`` with a ``db_path`` whose schema equals a fresh ``SessionDB``'s and
      whose every table holds exactly the rows of a fresh one — nothing a test wrote is ever overwritten.

    ``builds``/``copies`` count what happened (tests and timing reports read them).
    """

    def __init__(self, root: Path) -> None:
        self.root = Path(root)
        self._lock = threading.Lock()
        self._entries: dict[tuple, _Template] = {}
        self._pristine: tuple[dict[str, int], list[tuple]] | None = None
        self.builds = 0
        self.copies = 0

    def _key(self, builder: Any, shape: tuple, create_kw: Mapping[str, Any]) -> tuple | None:
        from ctx_handover.tests.fakes import FakeClock

        db = builder.db
        if not isinstance(builder.clock, FakeClock) or getattr(db, "read_only", False):
            return None
        if not isinstance(getattr(db, "db_path", None), (str, os.PathLike)):
            return None
        try:
            kw = json.dumps(dict(create_kw), sort_keys=True)
        except (TypeError, ValueError):
            return None
        return (*shape, kw, builder.now(), *builder.counters(), builder.tick)

    def _fresh_db(self):
        from hermes_state import SessionDB

        folder = self.root / f"t{len(self._entries):03d}-{uuid.uuid4().hex[:8]}"
        folder.mkdir(parents=True)
        return SessionDB(folder / "state.db")

    def _pristine_state(self) -> tuple[dict[str, int], list[tuple]]:
        if self._pristine is None:
            db = self._fresh_db()
            path = Path(db.db_path)
            db.close()
            conn = sqlite3.connect(path)
            try:
                self._pristine = (_table_counts(conn), _schema(conn))
            finally:
                conn.close()
        return self._pristine

    def _eligible_target(self, path: Path) -> bool:
        counts, schema = self._pristine_state()
        conn = sqlite3.connect(path, timeout=30)
        try:
            return _schema(conn) == schema and _table_counts(conn) == counts
        finally:
            conn.close()

    def _build(self, builder: Any, build: Callable[[Any], str]) -> _Template:
        db = self._fresh_db()
        try:
            twin = builder.twin(db)
            sid = build(twin)
            entry = _Template(Path(db.db_path), sid, twin.counters())
        finally:
            db.close()
        self.builds += 1
        return entry

    def copy_into(
        self, builder: Any, shape: tuple, create_kw: Mapping[str, Any], build: Callable[[Any], str]
    ) -> str | None:
        """Copy the template of ``shape`` (built with ``build(twin)`` on first use) into ``builder.db`` and return
        the segment id, or None when the call is not eligible (class docstring)."""
        key = self._key(builder, shape, create_kw)
        if key is None:
            return None
        target = Path(os.fspath(builder.db.db_path))
        with self._lock:
            if not self._eligible_target(target):
                return None
            entry = self._entries.get(key)
            if entry is None:
                entry = self._entries[key] = self._build(builder, build)
            _restore(entry.path, target)
            self.copies += 1
        builder.adopt_counters(*entry.counters)
        return entry.sid


def _restore(template: Path, target: Path) -> None:
    """``backup`` the template into ``target`` keeping the target's ``application_id`` and identity rows."""
    image = sqlite3.connect(":memory:")
    dst = sqlite3.connect(target, timeout=30, isolation_level=None)
    try:
        src = sqlite3.connect(template, timeout=30)
        try:
            src.backup(image)
        finally:
            src.close()
        app_id = int(dst.execute("PRAGMA application_id").fetchone()[0])
        identity = dst.execute(_IDENTITY_SELECT).fetchall()
        image.execute(f"PRAGMA application_id = {app_id}")
        image.executemany(_IDENTITY_UPSERT, identity)
        image.commit()
        image.backup(dst)
    finally:
        dst.close()
        image.close()


def pytest_addoption(parser):
    parser.addoption(
        "--no-segment-templates",
        action="store_true",
        default=False,
        help="ctx_handover: build every large_segment directly (contract K-6 off, for timing comparisons)",
    )


@pytest.fixture(scope="session")
def segment_templates(request, tmp_path_factory) -> SegmentTemplates | None:
    if request.config.getoption("--no-segment-templates"):
        return None
    return SegmentTemplates(tmp_path_factory.mktemp("segment-templates"))


@pytest.fixture
def make_session(session_db, clock, segment_templates):
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    return SessionBuilder(session_db, clock=clock, templates=segment_templates)


@pytest.fixture
def fake_host(monkeypatch, user_home, hermes_home):
    from ctx_handover.tests.fakes.hostcompat_fake import FakeHost

    host = FakeHost(user_home=user_home, hermes_home=hermes_home)
    host.install(monkeypatch)
    return host


@pytest.fixture
def fake_ctx(fake_host):
    from ctx_handover.tests.fakes.hostcompat_fake import FakeCtx

    return FakeCtx(host=fake_host)
