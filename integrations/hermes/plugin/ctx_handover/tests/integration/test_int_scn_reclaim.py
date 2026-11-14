"""Mandatory scenario 10 (layer 2, R1, R14, DESIGN §6.6, §7.4–§7.7, contract X08, TESTPLAN E15): reclaim at the
source after a successful handover.

After the push the laptop's session carries the primary lock (ended, archived, hidden, renamed ``T (handed over to
server …)``) and the veto: a real agent turn on it — through the hook callbacks the plugin hands to Hermes — never
reaches the model and leaves a placeholder pair. ``hermes handover reclaim <id>`` gives the session back: the marks
and the original title are restored, the veto rows are gone and the hook lets turns through again, the placeholder
pair is rewound (the model history equals the one before the handover), and the server learns that the source was
reclaimed (route #8, W047)."""

from __future__ import annotations

from ctx_handover.engine.model import Mode, Options, PlanHints, SessionRef
from ctx_handover.tests.integration._int3_helpers import (
    agent_turn,
    bounded,
    fake_provider,
    histories,
    hook_dispatcher,
    run_job,
    surface,
    title_of,
)
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import lineage_hashes, session_marks, wait_for

TITLE = "Reclaim me"


def test_int_scn_reclaim_after_handover(online):
    """★ Scenario 10.

    Mutant (red proof): ``lock._rewind_placeholders`` returns without rewinding (placeholders stay)."""
    server, laptop, _listener, peer = online
    bounded(laptop)
    b = builder(laptop)
    sid = b.create(title=TITLE, cwd=str(laptop.home))
    b.user(sid, "summarise the meeting")
    b.assistant(sid, "here is the summary")
    model_before, _ = histories(laptop.db(), sid)
    hashes_before = lineage_hashes(laptop.db(), sid)

    opts = Options(refine="off", yes=True, hints=PlanHints(no_workspace=True))
    res = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=opts)
    assert res.rc == 0, res.why()
    marks = session_marks(laptop.db(), sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks
    assert marks["title"] != TITLE and marks["title"].startswith(f"{TITLE} (handed over to server"), marks["title"]

    _, plugin = surface(laptop, "status --json")  # the plugin of this profile: EngineBackend + hooks over the runtime
    with laptop.active(), fake_provider() as (url, chat_requests):
        db = laptop.db()
        assert wait_for(lambda: plugin.hooks.pre_llm_call(session_id=sid) is not None, timeout=10), "no veto"
        db.reopen_session(sid)  # what a resume of the archived session does (cli_agent_setup_mixin.py:577-580)
        agent_turn(db, sid, url, hook_dispatcher(plugin), "typed while the session is on the server")
        assert chat_requests == [], "a vetoed turn reached the model"
        assert len(db.get_messages(sid)) > len(model_before), "the vetoed turn left no placeholder pair"

    call, _ = surface(laptop, f"reclaim {sid} --json", plugin=plugin)
    assert (call.data.get("reclaim") or {}).get("state") == "reclaimed", (call.exit_code, call.out)

    db = laptop.db()
    marks = session_marks(db, sid)
    assert not (marks["ended"] or marks["archived"] or marks["hidden"]), marks
    assert title_of(laptop, sid) == TITLE, "the original title was not restored"
    assert [r for r in laptop.ledger().veto_rows() if r["job_id"] == res.job_id] == [], "veto rows remain"
    with laptop.active():
        assert wait_for(lambda: plugin.hooks.pre_llm_call(session_id=sid) is None, timeout=10), "veto still active"
    model_after, _ = histories(db, sid)
    assert model_after == model_before, "the placeholder pair was not rewound"
    assert lineage_hashes(db, sid)[1:] == hashes_before[1:]

    with laptop.active(), fake_provider(answer=True) as (url, chat_requests):
        agent_turn(laptop.db(), sid, url, hook_dispatcher(plugin), "back on the laptop")
        assert chat_requests, "a turn after the reclaim did not reach the model"

    assert (call.data.get("reclaim") or {}).get("target_notified") is True, call.data
    rows = [r for r in server.ledger().arrival_rows() if r["job_id"] == res.job_id]
    assert rows and rows[0].get("source_reclaimed_at"), "the server did not learn that the source was reclaimed"
