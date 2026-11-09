"""Child-process entry points for the cluster C multi-process tests (never imported by runtime code).

Usage: ``python -m ctx_handover.tests.unit.C.proc_helpers <mode> <args…>``; every mode waits until the barrier
file ``go`` exists (poll 0.5 ms) so the children hit the critical section together, then prints one JSON line.

``identity <ledger> <go>``      ``Identity.create`` through the real Hermes ``.env`` functions of ``hostcompat``
                                 (``HERMES_HOME`` comes from the environment) → ``{"pubkey": …}``
``redeem <ledger> <go> <body>`` ``pairing.redeem_listener`` with the JSON body file → ``{"ok": …, "code": …}``
"""

from __future__ import annotations

import json
import os
import sys
import time
from pathlib import Path


def _barrier(go: str) -> None:
    deadline = time.monotonic() + 60
    while not os.path.exists(go):
        if time.monotonic() > deadline:
            raise SystemExit("barrier timeout")
        time.sleep(0.0005)


def main(argv: list[str]) -> int:
    from ctx_handover.engine import hostcompat
    from ctx_handover.engine.crypto import Identity
    from ctx_handover.engine.errors import HandoverError
    from ctx_handover.tests.unit.C.ledger_stub import make_ledger

    mode, ledger_path, go = argv[0], Path(argv[1]), argv[2]
    ledger = make_ledger(ledger_path)
    if mode == "identity":
        _barrier(go)
        ident = Identity.create(
            ledger,
            env_writable=hostcompat.env_writable,
            save_env=hostcompat.save_env,
            read_env_fresh=hostcompat.read_env_fresh,
        )
        print(json.dumps({"pubkey": ident.public_b64()}), flush=True)
        return 0
    if mode == "redeem":
        from ctx_handover.engine import pairing

        body = json.loads(Path(argv[3]).read_text(encoding="utf-8"))
        env = json.loads(Path(argv[4]).read_text(encoding="utf-8"))
        ident = Identity.load(ledger, read_secret=env.get, read_env_fresh=env.get)
        _barrier(go)
        try:
            resp = pairing.redeem_listener(ledger, ident, body, "127.0.0.1")
        except HandoverError as exc:
            print(json.dumps({"ok": False, "code": exc.code}), flush=True)
            return 0
        print(json.dumps({"ok": True, "peer_id": resp["peer_id"]}), flush=True)
        return 0
    raise SystemExit(f"unknown mode {mode}")


if __name__ == "__main__":
    sys.exit(main(sys.argv[1:]))
