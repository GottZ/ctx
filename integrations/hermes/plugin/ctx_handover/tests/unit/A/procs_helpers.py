"""Worker functions for the two-process ledger tests (imported by ``multiprocessing`` spawn children).

Every worker opens its own ``Ledger`` on the shared file — two real processes, two SQLite connections, the
same coordination path the gateway, ``hermes serve`` and the CLI use (DESIGN I10).
"""

from __future__ import annotations

from pathlib import Path


def redeem_worker(path: str, tag: str, codes: list[str], now: float, barrier, out) -> None:
    from ctx_handover.engine.ledger import Ledger
    from ctx_handover.engine.model import Peer, Right

    lg = Ledger(Path(path))
    results = []
    for i, code in enumerate(codes):
        peer = Peer(
            name=f"{tag}-{i}",
            peer_id=f"{tag}-{i}",
            direction="inbound",
            url=None,
            pubkey=f"pk-{tag}-{i}",
            rights=frozenset({Right.PUSH}),
        )
        barrier.wait(timeout=30)
        results.append(lg.redeem_pair_code(code, now, peer))
    lg.close()
    out.put((tag, results))


def fail_worker(path: str, tag: str, code: str, attempts: int, barrier, out) -> None:
    from ctx_handover.engine.ledger import Ledger

    lg = Ledger(Path(path))
    barrier.wait(timeout=30)
    values = [lg.fail_pair_code(code) for _ in range(attempts)]
    lg.close()
    out.put((tag, values))


def stalled_owner(path: str, job_id: str, ready, go, out) -> None:
    """Process A: owns the job, stalls until B adopted it, then tries to keep writing."""
    from ctx_handover.engine.errors import InvalidState
    from ctx_handover.engine.ledger import Fenced, Ledger
    from ctx_handover.engine.model import Role

    lg = Ledger(Path(path))
    epoch = lg.claim_owner(job_id, Role.CONDUCTOR, "proc-A", 60)
    lg.transition(job_id, Role.CONDUCTOR, "created", "hello", {"by": "A"}, epoch=epoch)
    ready.set()
    go.wait(timeout=30)
    report: dict = {"epoch": epoch, "heartbeat": lg.heartbeat(job_id, Role.CONDUCTOR, "proc-A", epoch)}
    for name, call in (
        ("transition", lambda: lg.transition(job_id, Role.CONDUCTOR, "hello", "facts", {"by": "A"}, epoch=epoch)),
        ("event", lambda: lg.event(job_id, Role.CONDUCTOR, "intent", {"step": "lock", "by": "A"}, epoch=epoch)),
        ("intent_commit", lambda: lg.intent_commit(job_id, epoch)),
    ):
        try:
            call()
            report[name] = "written"
        except Fenced:
            report[name] = "fenced"
        except InvalidState as exc:
            report[name] = "invalid_state:" + str(exc.details.get("reason"))
    lg.close()
    out.put(report)


def hung_op_owner(path: str, lock_dir: str, job_id: str, marker: str, started, release, out) -> None:
    """Runs an exclusive op whose heartbeat never comes (a hung but alive process) until ``release`` is set."""
    from ctx_handover.engine.ledger import Ledger
    from ctx_handover.engine.ops import OpRunner

    lg = Ledger(Path(path))
    runner = OpRunner(lg, None, heartbeat_s=3600, stale_s=0.2, lock_dir=Path(lock_dir))

    def fn(report):
        with open(marker, "a", encoding="utf-8") as fh:
            fh.write("H-start\n")
        started.set()
        release.wait(30)
        with open(marker, "a", encoding="utf-8") as fh:
            fh.write("H-end\n")
        return {"by": "H"}

    st = runner.start(job_id, "commit", "d", fn, exclusive=True)
    out.put(st.op_id)
    runner.wait(job_id, st.op_id, timeout=60)
    lg.close()
