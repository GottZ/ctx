"""Mandatory scenario 12 (layer 2): the tests the cluster reports left for layer 2 (``reports/L2-wishes-draft.md``
§3, rows with layer "2"), as far as wp-int1/wp-int2 do not cover them.

* D §10 — the collector with the real ``hermes_files.discover`` (``discover=None``) and the real ``gitops.GitProbe``
  on real repositories and real session files of a Hermes home.
* A §9 — commit step c3 with all three session-file kinds: the target places spillover, hook output and native input
  from kind and basename only (DESIGN §6.11, §10.6), byte-identical.
* A §10.5 — lease renewal from the conductor's keepalive thread against the real Hermes turn lease while the main
  thread is blocked in a long block upload: the lease never lapses and is extended.
* F §9 — ``EngineBackend`` against the real modules through the surfaces: ``peers``, ``to`` (``start_job`` kwargs),
  ``status``, ``doctor`` (gate check of the paired peer), ``gc --job`` and ``remote_choices``.
"""

from __future__ import annotations

import hashlib
import os
import threading
import time
from pathlib import Path

from ctx_handover.engine import collector, hostcompat, jobs, lock, remote
from ctx_handover.engine.model import ItemKind, Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.integration._int3_helpers import STALL_S, arrival, bounded, surface
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes

PNG = b"\x89PNG\r\n\x1a\n" + b"int3-native-input" * 8


def _session_files(home, cwd: Path, *, other_repo: Path | None = None, missing: bool = True) -> tuple[str, dict]:
    """A session in ``home`` (cwd ``cwd``) whose transcript references one file of each session-file kind, plus a
    missing spillover file and (optionally) a second repository. Returns ``(sid, {kind: (path, bytes)})``."""
    with home.active():
        hermes = hostcompat.hermes_paths()
    b = builder(home)
    sid = b.create(title="Session files", cwd=str(cwd))
    spill = Path(hermes.home) / "cache" / "spillover" / "out-1.txt"
    hook = Path(hermes.home) / "hook_outputs" / sid / "hook-1.txt"
    sha = hashlib.sha256(PNG).hexdigest()
    native = Path(hermes.document_cache) / "native-inputs" / sha / "shot.png"
    files = {
        "spillover": (spill, b"line\n" * 4096),
        "hook_outputs": (hook, b"hook said a lot\n" * 512),
        "native_inputs": (native, PNG),
    }
    for path, data in files.values():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(data)
    b.user(sid, f"look at this\n[Image attached at: {native}]")
    calls = [b.tool_call("terminal", {"command": "make test"}, call_id="t1")]
    if other_repo is not None:
        calls.append(b.tool_call("read_file", {"path": str(other_repo / "lib.py")}, call_id="r1"))
    b.assistant(sid, "running", tool_calls=calls)
    gone = Path(hermes.home) / "cache" / "spillover" / "gone.txt"
    text = f"output truncated\nFull output saved to: {spill}\n"
    if missing:
        text += f"older run\nFull output saved to: {gone}\n"
    b.tool(sid, "t1", text, name="terminal")
    if other_repo is not None:
        b.tool(sid, "r1", "LIB = 1", name="read_file")
    b.assistant(sid, f"the hook wrote more, full content saved to {hook}]")
    return sid, files


def test_int_scn_collector_with_real_discover_and_gitprobe(homes):
    """D §10: ``collector.collect(discover=None)`` with ``hermes_files.discover`` and ``gitops.GitProbe``."""
    _server, laptop = homes
    repo = RepoBuilder.init(laptop.home / "app")
    repo.write("main.py", "print(1)\n")
    repo.commit_all("first")
    other = RepoBuilder.init(laptop.home / "lib")
    other.write("lib.py", "LIB = 1\n")
    other.commit_all("first")
    sid, files = _session_files(laptop, repo.root / ".", other_repo=other.root)
    with laptop.active():
        source, _target = jobs.make_local_apis(None, calling_session=None, session_key=None)
        db = laptop.db()
        doc = db.export_session_lineage(sid, include_inactive=True) or {}
        rows = [r for r in doc.get("messages") or () if isinstance(r, dict)]
        from ctx_handover.engine import gitops

        cands = collector.collect(
            rows,
            db.get_session(sid) or {},
            source.hermes,
            source.policy,
            gitops.GitProbe(source.git),
            source_os=os.name,
        )
    by_path = {c.path: c for c in cands}
    primary = by_path.get(str(repo.root))
    assert primary is not None and primary.kind is ItemKind.GIT_REPO and "cwd" in primary.reasons, cands
    second = by_path.get(str(other.root))
    assert second is not None and second.kind is ItemKind.GIT_REPO, cands
    for kind, (path, _data) in files.items():
        c = by_path.get(str(path))
        assert c is not None and c.kind is ItemKind.HERMES_FILE, (kind, cands)
        assert any(r.startswith(f"{kind}: ") for r in c.reasons), (kind, c.reasons)
    gone = [c for c in cands if c.path.endswith("gone.txt")]
    assert gone and gone[0].reasons == ("rejected: missing",), gone


def test_int_scn_commit_places_all_three_session_file_kinds(online):
    """A §9 (W003, X17): push of a session with spillover, hook output and native input; at the server each file lies
    where DESIGN §6.11/§10.6 put it — from its kind and basename (hook output under the NEW session id, native input
    under the sha256 of the received bytes) — with identical bytes."""
    server, laptop, _listener, peer = online
    work = laptop.home / "work"
    work.mkdir()
    (work / "todo.md").write_text("- ship it\n", encoding="utf-8")
    sid, files = _session_files(laptop, work, missing=False)
    res_opts = Options(refine="off", yes=True, stall_timeout_s=STALL_S)
    with laptop.active():
        runner = jobs.runner_for(None)
        job_id, final = runner.plan_sync(
            Mode.PUSH, peer, res_opts, session_ref=SessionRef(id=sid), calling_session=None, session_key=None
        )
        assert final is not None, "the job ended during planning"
        rc = runner.run_foreground(job_id)
        row = runner.ledger.job(job_id, Role.CONDUCTOR)
    assert rc == 0, (row["state"], row["last_error"])
    tip = arrival(server, job_id)["session_id"]
    with server.active():
        hermes = hostcompat.hermes_paths()
    expected = {
        "spillover": Path(hermes.home) / "cache" / "spillover" / "out-1.txt",
        "hook_outputs": Path(hermes.home) / "hook_outputs" / tip / "hook-1.txt",
        "native_inputs": Path(hermes.document_cache) / "native-inputs" / hashlib.sha256(PNG).hexdigest() / "shot.png",
    }
    for kind, path in expected.items():
        assert path.is_file(), f"{kind} not placed at {path}"
        assert path.read_bytes() == files[kind][1], f"{kind} bytes differ"
    assert lineage_hashes(server.db(), tip) == lineage_hashes(laptop.db(), sid)


def test_int_scn_keepalive_renews_the_real_turn_lease(online, monkeypatch):
    """A §10.5: the first block batch of a push takes 40 s (longer than the 30 s lease TTL) while the conductor's
    main thread waits for it; the keepalive thread refreshes the Hermes turn lease, which never lapses."""
    _server, laptop, _listener, peer = online
    repo = RepoBuilder.init(laptop.home / "data")
    repo.write("blob.bin", os.urandom(6 * 1024 * 1024))
    repo.commit_all("data")
    b = builder(laptop)
    sid = b.create(title="Long upload", cwd=str(repo.root))
    b.user(sid, "keep the data")
    b.assistant(sid, "kept")

    write = remote.RemoteBlocks.write_blocks
    first = {"done": False, "window": None}

    def slow_first(self, frames):
        frames = list(frames)
        if not first["done"] and len(frames) > 1:
            first["done"] = True
            t0 = time.time()
            time.sleep(40)
            first["window"] = (t0, time.time())
        return write(self, iter(frames))

    monkeypatch.setattr(remote.RemoteBlocks, "write_blocks", slow_first)
    samples: list[tuple[float, float | None]] = []
    stop = threading.Event()
    opts = Options(refine="off", yes=True, lease_ttl_s=30, stall_timeout_s=STALL_S)
    with laptop.active():
        runner = jobs.runner_for(None)
        job_id, final = runner.plan_sync(
            Mode.PUSH, peer, opts, session_ref=SessionRef(id=sid), calling_session=None, session_key=None
        )
        assert final is not None, "the job ended during planning"
        holder = lock.holder_string(job_id)
        db = laptop.db()

        def sample() -> None:
            while not stop.is_set():
                samples.append((time.time(), db.session_turn_lease_expires_at(sid, holder)))
                time.sleep(0.5)

        monitor = threading.Thread(target=sample, daemon=True)
        monitor.start()
        try:
            rc = runner.run_foreground(job_id)
        finally:
            stop.set()
            monitor.join(5)
        row = runner.ledger.job(job_id, Role.CONDUCTOR)
    assert rc == 0, (row["state"], row["last_error"])
    assert first["window"] is not None, "no multi-block batch was uploaded"
    t0, t1 = first["window"]
    inside = [(t, e) for t, e in samples if t0 + 1 <= t <= t1 - 1]
    assert inside, "no lease samples during the long batch"
    assert all(e is not None and e > t for t, e in inside), "the turn lease lapsed while the upload was running"
    expiries = [e for _, e in inside]
    assert max(expiries) - min(expiries) >= 5, "the keepalive thread did not extend the lease during the upload"


def test_int_scn_engine_backend_against_real_modules(online):
    """F §9: the surfaces' ``EngineBackend`` over the real engine of the laptop and the real listener of the server."""
    server, laptop, _listener, peer = online
    bounded(laptop)
    sb = builder(server)
    remote_sid = sb.create(title="On the server", cwd=str(server.home))
    sb.user(remote_sid, "hello server")
    lb = builder(laptop)
    sid = lb.create(title="Via the surfaces", cwd=str(laptop.home))
    lb.user(sid, "hello")
    lb.assistant(sid, "hi")

    peers, plugin = surface(laptop, "peers --json")
    assert peers.exit_code == 0 and "server" in "\n".join(peers.out), peers.out

    to, _ = surface(laptop, f"to server --session {sid} --no-workspace --yes --json", plugin=plugin)
    job_id = to.data.get("job_id")
    assert to.exit_code == 0 and job_id, (to.exit_code, to.out)
    assert arrival(server, job_id) is not None

    st, _ = surface(laptop, f"status {job_id} --json", plugin=plugin)
    job = (st.data.get("status") or {}).get("job") or {}
    assert st.exit_code == 0 and job.get("state") == "done" and (job.get("ack") or {}).get("title"), st.data

    doc, _ = surface(laptop, "doctor --json", plugin=plugin)
    checks = doc.data.get("checks") or []
    peer_checks = [c for c in checks if "server" in str(c.get("name")) or "server" in str(c.get("detail"))]
    assert peer_checks and all(c.get("ok") is not False for c in peer_checks), checks

    staging = laptop.data_dir() / "staging" / job_id
    gc, _ = surface(laptop, f"gc --job {job_id} --json", plugin=plugin)
    assert gc.exit_code == 0, gc.out
    assert not staging.exists(), "gc --job left the job's staging"
    assert laptop.ledger().job(job_id, Role.CONDUCTOR) is None, "gc --job left the journal"

    with laptop.active():
        choices = plugin.backend.remote_choices(peer, limit=10)
    assert remote_sid in {s.get("id") or s.get("session_id") for s in choices["sessions"]}, choices
