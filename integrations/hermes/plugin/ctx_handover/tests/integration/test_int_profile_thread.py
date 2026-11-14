"""Worker threads keep the profile of the request that started them (TESTPLAN §3 ``test_int_profile_scoped_thread``;
DESIGN §2.1 consequence 6, §15.4; [F4], [F7], OPEN A14).

A multiplexing gateway serves several profiles in one process: the profile of a turn or request is a context
variable (``hermes_constants.set_hermes_home_override``, ``agent.secret_scope.set_secret_scope``), not
``HERMES_HOME``. Here the process's own profile is the test's launch home; the laptop's profile is served through
that scope, exactly as the gateway binds it, and its secrets (identity key, gate token) live only in the laptop's
``.env``. The handler starts a push and returns; the job continues on a worker thread
(``JobRunner.continue_async``). Expected: the push completes from the laptop's profile, and nothing of the plugin
appears in the launch profile — no plugin data directory, no session store."""

from __future__ import annotations

import contextvars
import os
from pathlib import Path

from ctx_handover.engine import hostcompat, jobs
from ctx_handover.engine.model import Mode, Options, PlanHints, Role, SessionRef
from ctx_handover.tests.integration._int2_helpers import ack_ids, outcome
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import GATE_ENV, lineage_hashes


def test_int_profile_scoped_thread(online):
    server, laptop, listener, peer = online
    launch_home = Path(os.environ["HERMES_HOME"])
    assert launch_home not in (laptop.hermes_home, server.hermes_home)
    with laptop.active():
        hostcompat.save_env(GATE_ENV, listener.token)  # a gateway reads profile secrets from the profile .env
    b = builder(laptop)
    sid = b.create(title="Scoped profile", cwd=str(laptop.home))
    b.turns(sid, 4)
    jobs._reset_for_tests()  # nothing of the setup stays cached in this process

    from agent.secret_scope import build_profile_secret_scope, set_multiplex_context, set_secret_scope
    from hermes_constants import set_hermes_home_override

    def handler():
        """A slash/route handler of the laptop profile inside a multiplexing gateway."""
        set_multiplex_context(True)
        set_hermes_home_override(laptop.hermes_home)
        set_secret_scope(build_profile_secret_scope(laptop.hermes_home), profile_home=str(laptop.hermes_home))
        runner = jobs.runner_for(None)
        opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True), stall_timeout_s=60)
        job_id, final = runner.plan_sync(
            Mode.PUSH, peer, opts, session_ref=SessionRef(id=sid), calling_session=None, session_key=None
        )
        assert final is not None, outcome(laptop, job_id)
        runner.continue_async(job_id)  # the handler returns; the job goes on in a worker thread
        return runner, job_id

    runner, job_id = contextvars.copy_context().run(handler)
    runner.join(120)
    row = laptop.ledger().job(job_id, Role.CONDUCTOR)
    assert jobs.exit_code(row) == 0, outcome(laptop, job_id)
    _root, new_tip = ack_ids(laptop, job_id)
    assert lineage_hashes(laptop.db(), sid) == lineage_hashes(server.db(), new_tip)
    assert not (launch_home / "plugin-data" / "ctx_handover").exists(), sorted(launch_home.rglob("*"))[:20]
    assert not (launch_home / "state.db").exists()
