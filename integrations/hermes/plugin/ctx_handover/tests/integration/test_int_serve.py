"""The listener inside a real ``hermes serve`` (TESTPLAN §3 ``test_int_http_push_loopback`` /
``test_int_http_pull_loopback`` purpose "signed HTTP with a fixed dashboard token through a real ``hermes serve``
router mount", and ``test_int_identity_visible_to_serve_process``; DESIGN §2.1, §4.1, §12.2; R7, R12, R16).

The existing loopback tests serve the plugin router from ``listener_app.py`` (a stand-in that mounts
``dashboard/plugin_api.py`` itself). Here home A runs the upstream ``hermes serve``: the plugin is installed into
A's ``HERMES_HOME/plugins`` and enabled, Hermes discovers it, imports ``dashboard/plugin_api.py`` with
``import_user_code`` and mounts the router behind its own dashboard gate (fixed ``HERMES_DASHBOARD_SESSION_TOKEN``)
and the profile secret scope. Every Hermes process runs with a disposable ``HOME``/``HERMES_HOME``."""

from __future__ import annotations

import json

import pytest

from ctx_handover.engine import hostcompat, jobs, pairing, wire
from ctx_handover.engine.model import GateAuth, GateMode, Mode, Options, Role, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder, tree_digest
from ctx_handover.tests.integration._int2_helpers import ServeListener, ack_ids, hermes_cli, outcome
from ctx_handover.tests.integration.conftest import builder
from ctx_handover.tests.integration.harness import GATE_ENV, lineage_hashes, pair, session_marks


@pytest.fixture
def served(homes):
    """``(server, laptop, serve)``: home A (with its identity) behind a real ``hermes serve``. The tests pair B
    with it themselves, so a router the serve process does not mount fails the test, not its setup."""
    server, laptop = homes
    server.identity()
    serve = ServeListener(server)
    try:
        yield server, laptop, serve
    finally:
        serve.stop()


def _dirty_repo(root) -> RepoBuilder:
    r = RepoBuilder.init(root)
    r.write("README.md", "# app\n")
    r.write("src/main.py", "print('v1')\n")
    r.add_all()
    r.commit("first")
    r.write("src/main.py", "print('v2 staged')\n")
    r.add("src/main.py")
    r.write("src/main.py", "print('v3 in the work tree')\n")
    r.write("notes/todo.txt", "untracked work\n")
    return r


def _session(home, cwd, title: str) -> str:
    b = builder(home)
    sid = b.create(title=title, cwd=str(cwd))
    b.user(sid, "please change src/main.py")
    b.assistant(sid, "done, see src/main.py")
    return sid


def _assert_arrived(src_home, sid, dst_home, job_id, conductor_home, dest, digest):
    _root, new_tip = ack_ids(conductor_home, job_id)
    assert new_tip
    src_db, dst_db = src_home.db(), dst_home.db()
    assert lineage_hashes(src_db, sid) == lineage_hashes(dst_db, new_tip)
    assert not session_marks(dst_db, new_tip)["hidden"]
    marks = session_marks(src_db, sid)
    assert marks["ended"] and marks["archived"] and marks["hidden"], marks
    assert tree_digest(dest) == digest


def test_int_http_push_loopback_hermes_serve(served):
    server, laptop, serve = served
    peer = pair(server, laptop, serve)
    repo = _dirty_repo(laptop.home / "proj")
    digest = tree_digest(repo.root)
    sid = _session(laptop, repo.root, "Push through serve")
    job_id, rc = laptop.run(Mode.PUSH, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    assert rc == 0, (outcome(laptop, job_id), serve.log_tail())
    _assert_arrived(laptop, sid, server, job_id, laptop, server.home / "hermes-handover" / "proj", digest)
    assert serve.mounted()
    assert serve.ran_job(job_id, Role.TARGET), "the target participant did not run in the hermes serve process"


def test_int_http_pull_loopback_hermes_serve(served):
    server, laptop, serve = served
    peer = pair(server, laptop, serve)
    repo = _dirty_repo(server.home / "proj")
    digest = tree_digest(repo.root)
    sid = _session(server, repo.root, "Pull through serve")
    job_id, rc = laptop.run(Mode.PULL, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    assert rc == 0, (outcome(laptop, job_id), serve.log_tail())
    _assert_arrived(server, sid, laptop, job_id, laptop, laptop.home / "hermes-handover" / "proj", digest)
    assert serve.mounted()
    assert serve.ran_job(job_id, Role.SOURCE), "the source participant did not run in the hermes serve process"
    pulled = server.ledger().pull_log(5)
    assert pulled and pulled[0]["job_id"] == job_id


def test_int_identity_visible_to_serve_process(homes):
    """[R16] The identity key is created by an operator verb in a CLI process (``hermes handover pair``) while
    ``hermes serve`` of the same profile is already running; that serve process reads it on its next request
    (``read_env_fresh``) and completes the pairing with it."""
    server, laptop = homes
    serve = ServeListener(server)
    try:
        status, body = serve.get("hello")
        assert status == 404 and json.loads(body) == {"detail": "Plugin not found"}, (status, body)  # no key yet
        assert server.ledger().meta_get("identity_pubkey") is None

        res = hermes_cli(server, "handover", "pair", "--json")
        assert res.returncode == 0, (res.stdout[-2000:], res.stderr[-2000:])
        out = json.loads(res.stdout)
        pair_string = next(w for w in out["text"].split() if w.startswith("ctxh-pair:"))
        pubkey = server.ledger().meta_get("identity_pubkey")
        assert pubkey, "the CLI did not store the identity"

        status, body = serve.get("hello")
        assert status == 200, (status, body, serve.log_tail())
        hello = json.loads(body)
        assert hello["listener_pubkey"] == pubkey
        assert hello["pairing_open"] is True

        # The running serve process signs the pairing with the key the CLI created.
        ident_b = laptop.identity()
        laptop.extra_env[GATE_ENV] = serve.token
        with laptop.active():
            peer = pairing.redeem_dialer(
                lambda p: wire.WireClient(p, ident_b, read_secret=hostcompat.read_secret),
                ident_b,
                pairing.parse_pair_string(pair_string),
                name="server",
                gate=GateAuth(mode=GateMode.SESSION_TOKEN, token_env=GATE_ENV),
                allow_insecure=True,
                ledger=jobs._profile().ledger,
            )
        assert peer.pubkey == pubkey and peer.url == serve.url
    finally:
        serve.stop()
