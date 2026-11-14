"""Shared harness for the conductor, fault-matrix and runner tests (cluster A).

A real ``Ledger`` on disk, a ``World`` of fake participants (``tests/fakes/apis_fake.py``), a fake clock whose
``sleep`` only advances time, and helpers to read the journal.
"""

from __future__ import annotations

from dataclasses import dataclass, field

from ctx_handover.engine.conductor import Conductor, PollPolicy
from ctx_handover.engine.contracts import JobContext
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.jsonutil import to_wire
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Options, Role
from ctx_handover.tests.fakes.apis_fake import FakePlanning, RecordingReporter, World

FAST = PollPolicy(
    op_first_s=1.0,
    op_max_s=5.0,
    in_doubt_first_s=1.0,
    in_doubt_max_s=300.0,
    retry_attempts=4,
    retry_first_s=0.5,
    retry_max_s=60.0,
    retire_attempts=5,
)


@dataclass
class Harness:
    tmp_path: object
    clock: object
    mode: Mode = Mode.PUSH
    options: Options = field(default_factory=Options)
    continue_prompt: str | None = None
    owner_seq: int = 0

    def __post_init__(self) -> None:
        self.ledger = Ledger(self.tmp_path / f"ledger-{ulid()}.sqlite3", clock=self.clock)
        self.world = World(self.clock)
        self.planning = FakePlanning()
        self.reporter = RecordingReporter()
        self.job_id = ulid()
        params = {
            "mode": str(self.mode),
            "session_ref": {"id": "tip", "latest": False, "offer_id": None},
            "options": to_wire(self.options),
            "continue_prompt": self.continue_prompt,
        }
        self.ledger.create_job(self.job_id, Role.CONDUCTOR, self.mode, None, params)
        if self.mode is Mode.PULL:
            self.world.source.hosted = False

    def conductor(self, *, confirm=None, owner: str | None = None, stale_after_s: float = 60.0) -> Conductor:
        self.owner_seq += 1
        owner = owner or f"owner-{self.owner_seq}"
        epoch = self.ledger.claim_owner(self.job_id, Role.CONDUCTOR, owner, stale_after_s)
        assert epoch is not None, "job still owned by a live runner"
        job = JobContext(
            job_id=self.job_id,
            mode=self.mode,
            peer=None,
            options=self.options,
            epoch=epoch,
            reporter=self.reporter,
            cancelled=lambda: self.ledger.cancel_requested(self.job_id),
            created_at=self.clock(),
        )
        return Conductor(
            self.ledger,
            job,
            self.world.source,
            self.world.target,
            confirm=confirm,
            continue_prompt=self.continue_prompt,
            planning=self.planning,
            owner=owner,
            clock=self.clock,
            sleep=self.world.sleep,
            poll=FAST,
            keepalive=False,
        )

    # journal helpers -------------------------------------------------------------------------------------------
    def state(self) -> str:
        return self.ledger.job(self.job_id, Role.CONDUCTOR)["state"]

    def states(self) -> list[str]:
        evs = self.ledger.events(self.job_id, Role.CONDUCTOR)
        return [e["state"] for e in evs if e["kind"] in ("created", "transition")]

    def events(self, kind: str | None = None) -> list[dict]:
        evs = self.ledger.events(self.job_id, Role.CONDUCTOR)
        return [e for e in evs if kind is None or e["kind"] == kind]

    def stale(self) -> None:
        """Let the current owner go stale (the runner process died)."""
        self.clock.advance(61)

    def calls(self, side: str) -> list[str]:
        return [m for s, m in self.world.log if s == side]
