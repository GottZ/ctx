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
                  through the core's own paths only (no SQL, no import flags)
``clock``         ``tests.fakes.FakeClock``
``fake_host``     ``tests.fakes.hostcompat_fake.FakeHost`` installed over ``engine.hostcompat``
``fake_ctx``      ``tests.fakes.hostcompat_fake.FakeCtx`` (a recording PluginContext stand-in)
"""

from __future__ import annotations

import importlib.util
import os
import sys
from pathlib import Path

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


@pytest.fixture
def make_session(session_db, clock):
    from ctx_handover.tests.fakes.sessiondb import SessionBuilder

    return SessionBuilder(session_db, clock=clock)


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
