"""Fixtures of the layer-3 lab suite (BUILD-ORDER §5, TESTPLAN §1/§4/§6).

``lab`` (session): the paired two-gateway stand, built once per run from **this worktree's** plugin tree:

1. ``labctl.py reset --no-probe`` — fresh homes, Hermes' managed runtime kept (``CTXH_E2E_NO_RESET=1``: only
   ``labctl.py stop``; then the stand is rebuilt on the existing homes and peer names may collide);
2. ``labctl.py install-handover <plugin dir>`` — a plain copy into ``a/plugins`` and ``b/plugins`` (no symlink)
   plus ``hermes plugins enable ctx_handover``; the fixture checks that the worktree's product files are the
   production state ``abcbd8a7`` (``CTXH_E2E_EXPECT_HEAD``) and that each copy is byte-equal to the worktree
   (sha256 per file) — no lab patch such as L3-P01;
3. settings per home: ``listen_url`` (A ``http://127.0.0.1:47111``, B ``http://127.0.0.1:47112``);
   ``workspace_root`` = the only ``allow_roots`` entry = ``lab/recv-e2e/<x>`` (the default ``~/hermes-handover`` lies
   inside HERMES_HOME in this lab and is refused, lab finding L3-F01); the gate secret ``CTX_HANDOVER_PEER_TOKEN`` in
   the ``.env`` of the dialing home (B holds A's token, A holds B's); ``max_jobs_per_peer`` 8 and ``max_jobs_total``
   16 (listener caps above the defaults 2/4, so a scenario that leaves a stuck job — L3-F07 — does not refuse the
   following ones with ``rate_limited``). Session workspaces live in ``lab/ws-e2e/<x>``
   (a cwd inside ``lab/<x>`` is never sent, L3-F02); both folders are emptied at the start;
4. ``labctl.py start`` of ``llm gw-a gw-b serve-a serve-b`` with fixed tokens (A ``lab-tok-a``, B ``lab-tok-b``,
   each for the gateway and ``hermes serve`` of the same home);
5. pairing in both directions by paste string (``hermes handover pair``): on A, B is the inbound peer ``laptop``
   and the dial peer ``laptopgw``; on B, A is the dial peer ``server`` and the inbound peer ``serverin``.

Teardown: ``labctl.py stop`` and ``labctl.py status`` — the run fails in teardown unless the status shows
``lab-tagged processes: 0``. Bootstrap and teardown output: ``lab/evidence/e2e-<date>/_session/``.

``ev`` (autouse, per test): ``lab/evidence/e2e-<date>/<test>/`` (date from ``CTXH_E2E_EVIDENCE`` if set); at the end
of each test the fake-model requests of the test (compact form, no full bodies) and the new part of every lab
process log are written there.

``outside`` (per test): a fresh folder outside every git work tree (``_lab.Lab.outside``: ``CTXH_E2E_OUTSIDE``, default
``~/.cache/ctx-handover-e2e/<run id>-<uid>``, mode 0700), removed in teardown — for sources whose mentioned files
must not lie in a git work tree that encloses ``lab/`` (L3-F26).

``e2e_timeout`` marker: hard upper bound per test (SIGALRM in the main thread raises ``E2ETimeout``; the test's own
``finally`` blocks still run). Default 900 s.
"""

from __future__ import annotations

import json
import os
import re
import shutil
import signal
import subprocess
import time
from pathlib import Path

import pytest

from . import _lab

DEFAULT_TIMEOUT_S = 900


class E2ETimeout(Exception):
    """A lab test exceeded its ``e2e_timeout``."""


def pytest_configure(config):
    config.addinivalue_line("markers", "e2e_timeout(seconds): hard upper bound of one lab test (tests/e2e)")


@pytest.fixture(autouse=True)
def _e2e_watchdog(request):
    if not _lab.enabled():
        yield
        return
    marker = request.node.get_closest_marker("e2e_timeout")
    limit = int(marker.args[0]) if marker and marker.args else DEFAULT_TIMEOUT_S

    def expired(signum, frame):
        raise E2ETimeout(f"{request.node.name} exceeded its upper bound of {limit} s")

    previous = signal.signal(signal.SIGALRM, expired)
    signal.alarm(limit)
    try:
        yield
    finally:
        signal.alarm(0)
        signal.signal(signal.SIGALRM, previous)


def _safe_name(node_name: str) -> str:
    return re.sub(r"[^A-Za-z0-9._-]+", "_", node_name)[:120]


_LOGS = ("llm", "gw-a", "gw-b", "serve-a", "serve-b")


def _read_records(path: Path, mark: int) -> list[dict]:
    records: list[dict] = []
    if not path.exists():
        return records
    with open(path, encoding="utf-8") as fh:
        for i, line in enumerate(fh):
            if i < mark or not line.strip():
                continue
            try:
                records.append(json.loads(line))
            except ValueError:
                continue
    return records


def _line_count(path: Path) -> int:
    if not path.exists():
        return 0
    with open(path, "rb") as fh:
        return sum(1 for _ in fh)


@pytest.fixture(autouse=True)
def ev(request):
    """Evidence directory of this test (see module docstring)."""
    if not _lab.enabled():
        yield None
        return
    evidence = _lab.Evidence(_lab.evidence_root() / _safe_name(request.node.name))
    run = _lab.LAB / "run"
    offsets = {name: (run / f"{name}.log").stat().st_size if (run / f"{name}.log").exists() else 0 for name in _LOGS}
    llm_path = _lab.LLM_DIR / "requests.jsonl"
    llm_mark = _line_count(llm_path)
    evidence.write("started.txt", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
    try:
        yield evidence
    finally:
        try:
            evidence.write("llm-requests.json", _lab.compact_records(_read_records(llm_path, llm_mark)))
            for name in _LOGS:
                path = run / f"{name}.log"
                if not path.exists():
                    continue
                size = path.stat().st_size
                start = offsets[name] if size >= offsets[name] else 0
                with open(path, "rb") as fh:
                    fh.seek(max(start, size - 250_000))
                    evidence.write(f"log-{name}.txt", fh.read())
            evidence.write("finished.txt", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))
        except Exception as exc:  # evidence must never mask the test result
            (evidence.root / "evidence-error.txt").write_text(repr(exc), encoding="utf-8")


@pytest.fixture(scope="session")
def lab():
    """The paired stand (module docstring). Fails (never skips) when a bootstrap step does not work."""
    if not _lab.enabled():
        pytest.skip(_lab.SKIP_REASON)
    session_ev = _lab.Evidence(_lab.evidence_root() / "_session")
    stand = _lab.Lab(session_ev)
    try:
        _bootstrap(stand, session_ev)
        yield stand
    finally:
        stand.labctl("stop", timeout=600)
        status = stand.status_text()
        session_ev.write("final-status.txt", status)
        left = stand.lab_tagged()
        if left or "lab-tagged processes: 0" not in status:
            pytest.fail(f"lab processes left after the run: {left}\n{status}", pytrace=False)


@pytest.fixture
def outside(lab, ev):
    """A folder outside every git work tree for this test, removed afterwards (module docstring, ``Lab.outside``)."""
    with lab.outside(ev) as root:
        yield root


EXPECTED_HEAD = os.environ.get("CTXH_E2E_EXPECT_HEAD", "abcbd8a7")
"""Production state the run must test (product files unchanged against it; tests/ may differ)."""


def _product_state() -> dict:
    root = _lab.PLUGIN_SRC
    git = shutil.which("git") or "git"
    head = subprocess.run(  # noqa: S603 - fixed git argv on the worktree, no shell
        [git, "-C", str(root), "rev-parse", "--short=8", "HEAD"], capture_output=True, text=True, check=False
    ).stdout.strip()
    status = subprocess.run(  # noqa: S603 - fixed git argv on the worktree, no shell
        [git, "-C", str(root), "status", "--porcelain", "--", ".", ":(exclude)tests"],
        capture_output=True,
        text=True,
        check=False,
    ).stdout.splitlines()
    return {"head": head, "dirty_product_files": status}


def _bootstrap(stand: _lab.Lab, ev: _lab.Evidence) -> None:
    if os.environ.get("CTXH_E2E_NO_RESET") == "1":
        stand.labctl("stop", timeout=600)
    else:
        res = stand.labctl("reset", "--no-probe", timeout=1800)
        if res.returncode != 0:
            raise _lab.LabError(f"labctl reset failed: {res.stdout[-1500:]} {res.stderr[-800:]}")
    res = stand.labctl("install-handover", str(_lab.PLUGIN_SRC), timeout=1800)
    if res.returncode != 0:
        raise _lab.LabError(f"install-handover failed: {res.stdout[-2000:]} {res.stderr[-800:]}")
    source = _lab.tree_digests(_lab.PLUGIN_SRC)
    product = _product_state()
    ev.write("source-state.json", {"product": product, "files": len(source)})
    if product["head"] != EXPECTED_HEAD or product["dirty_product_files"]:
        raise _lab.LabError(f"worktree is not the production state {EXPECTED_HEAD}: {product}")
    for inst in ("a", "b"):
        installed = stand.home(inst) / "plugins" / "ctx_handover"
        if installed.is_symlink() or not (installed / "plugin.yaml").is_file():
            raise _lab.LabError(f"ctx_handover is not installed as a copy in {installed}")
        copy = _lab.tree_digests(installed)
        diff = sorted(set(source) ^ set(copy)) + sorted(k for k in source if k in copy and source[k] != copy[k])
        ev.write(f"copy-check-{inst}.json", {"files": len(copy), "diff": diff})
        if diff:
            raise _lab.LabError(f"installed copy in {inst} differs from the worktree (lab patch applied?): {diff[:10]}")
        for folder in (stand.ws(inst), stand.handover_root(inst)):
            shutil.rmtree(folder, ignore_errors=True)
            folder.mkdir(parents=True)
        stand.setting(inst, "listen_url", _lab.SERVE_URL[inst])
        stand.setting(inst, "workspace_root", str(stand.handover_root(inst)))
        stand.setting(inst, "allow_roots", json.dumps([str(stand.handover_root(inst))]))
        stand.env_set(inst, "CTX_HANDOVER_PEER_TOKEN", stand.tokens[_lab.OTHER[inst]])
        # isolation between scenarios: fault scenarios end with unfinished jobs on purpose, and with the default
        # cap (2 per peer, engine/participant.py:36) one stuck job plus one open offer refuse every later handover
        # of that peer with rate_limited (L3-F07); the cap itself is no E-scenario
        stand.setting(inst, "max_jobs_per_peer", "8")
        stand.setting(inst, "max_jobs_total", "16")
    for inst in ("a", "b"):
        ev.write(f"config-{inst}.yaml", stand.config_path(inst).read_text(encoding="utf-8"))
    stand.start(*_lab.COMPONENTS)
    ev.write("status-after-start.txt", stand.status_text())
    made, redeemed = stand.pair("a", "b", hint=_lab.LAPTOP, name=_lab.SERVER)
    ev.cli("pair-a-code", made)
    ev.cli("pair-b-redeem", redeemed)
    made, redeemed = stand.pair("b", "a", hint=_lab.SERVER_IN, name=_lab.LAPTOP_GW)
    ev.cli("pair-b-code", made)
    ev.cli("pair-a-redeem", redeemed)
    for inst in ("a", "b"):
        peers = stand.handover(inst, "peers", timeout=120)
        ev.cli(f"peers-{inst}", peers)
    names_a = {r["name"]: r["direction"] for r in stand.ledger_rows("a", "SELECT name, direction FROM peers")}
    names_b = {r["name"]: r["direction"] for r in stand.ledger_rows("b", "SELECT name, direction FROM peers")}
    want_a = {_lab.LAPTOP: "inbound", _lab.LAPTOP_GW: "dial"}
    want_b = {_lab.SERVER: "dial", _lab.SERVER_IN: "inbound"}
    if any(names_a.get(k) != v for k, v in want_a.items()) or any(names_b.get(k) != v for k, v in want_b.items()):
        raise _lab.LabError(f"peer table after pairing: a={names_a} b={names_b}")
