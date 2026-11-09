"""r4-f — README limits the orchestrator asked for in review round 4 (``REVIEW-3.md`` §4), each sentence bound to the
code behind it (L28: no free-standing substrings).

* **M19 / L14** (g4 round 3: "Grenze nicht ehrlich"): the secrets list counts wherever a name lies — a folder named
  like a secret file (``credentials/``) and credential folder names (``.docker``, ``.kube``) inside a project. What it
  means for a user: without git the folder stays here and is named only afterwards; in a git workspace the secret
  file name does not apply to the folder, the tracked files below a credential folder name are listed beforehand;
  ``--include-secret`` sends it.
* **R2-L2** (g3: not in the README): redeeming a pairing code proves no possession of the private key — a valid code
  pairs a foreign public key; that pairing gets no access, but it unpairs the key's owner here and aborts its jobs.

Red before r4-f: none of these sentences was in the README (``test_*_bullet`` fail on the first ``in`` assertion).
"""

from __future__ import annotations

import inspect
import time
from pathlib import Path
from types import SimpleNamespace

import pytest

from ctx_handover.engine import (
    bundle,
    crypto,
    filetree,
    gitops,
    jobs,
    materialize,
    note,
    pairing,
    plan,
    policy,
    server,
    surfaces,
)
from ctx_handover.engine.ids import ulid
from ctx_handover.engine.ignore import IgnoreMatcher
from ctx_handover.engine.ledger import Ledger
from ctx_handover.engine.model import Mode, Right, Role
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.C.conftest import new_identity
from ctx_handover.tests.unit.E._support import make_cfg

PLUGIN_ROOT = Path(policy.__file__).resolve().parents[1]
FLAT = " ".join((PLUGIN_ROOT / "README.md").read_text(encoding="utf-8").split())


def _bullet(start: str) -> str:
    limits = FLAT.split("## Limits — read this first", 1)[1].split("## What the lock", 1)[0]
    return next(p for p in limits.split(" - ") if p.startswith(start))


# ---------------------------------------------------------------------------------------------------------
# M19 / L14: secret names inside a project
# ---------------------------------------------------------------------------------------------------------

FILES = {
    "credentials/creds.go": "package credentials\n",
    ".kube/deploy.yaml": "kind: Deployment\n",
    ".docker/nginx/default.conf": "server {}\n",
    "src/main.go": "package main\n",
}


def _lay(root: Path) -> None:
    for rel, text in FILES.items():
        path = root / rel
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(text, encoding="utf-8")


def _scan(src: Path, cfg) -> tuple[list[str], list[tuple[str, str]]]:
    entries, left = filetree.scan(src, IgnoreMatcher.for_tree(src, ()), cfg)
    return sorted(e.path for e in entries), sorted((n.path, n.why) for n in left)


def _secret_bullet() -> str:
    return _bullet("**Secret names are matched broadly, on purpose.**")


def test_secret_names_bullet_names_what_counts():
    """★ The names the bullet lists are the policy's: ``credentials`` is a secret file name, the folder names are
    ``SOURCE_DENY_DIRS`` (matched anywhere in a path, ``policy.is_denied_name``)."""
    bullet = _secret_bullet()
    assert (
        "The secrets list counts wherever a name lies, also inside a project: a folder named like a secret file "
        "(`credentials`, for instance a Go package of that name) and every credential folder name (`.ssh`, `.aws`, "
        "`.gnupg`, `.kube`, `.docker`, `.azure` and the others of `policy.SOURCE_DENY_DIRS`) with everything below "
        "it." in bullet
    ), bullet
    assert "credentials" in policy.SOURCE_DENY_NAMES
    for name in (".ssh", ".aws", ".gnupg", ".kube", ".docker", ".azure"):
        assert name in policy.SOURCE_DENY_DIRS, name
        assert policy.is_denied_name(f"proj/deep/{name}/x.yaml", case_insensitive=False), name
    assert policy.is_denied_name("proj/credentials", case_insensitive=False)
    assert not policy.is_denied_name("proj/credentials/creds.go", case_insensitive=False)


def test_secret_names_bullet_file_tree_stays_here_and_is_named_afterwards(user_home, hermes_home):
    """★ Without git the folders stay here as a whole (the scan's ``secret``); the summary before the handover counts
    the tree by its ignore rules only (``plan._tree_count``: the folder's bytes are in the size, nothing is listed);
    afterwards ``secret`` is left behind (``materialize._LEFT_BEHIND``), recorded as ``excluded_secrets``
    (``jobs``), shown as "Secrets left out" and in the note as "secrets excluded"."""
    bullet = _secret_bullet()
    assert (
        "In a workspace without git such a folder stays here as a whole, and you learn it from the final message "
        '("Secrets left out") and the note, not from the summary before the handover.' in bullet
    ), bullet
    src = user_home / "proj"
    _lay(src)
    entries, left = _scan(src, make_cfg(user_home, hermes_home))
    assert entries == ["src", "src/main.go"], entries
    assert left == [(".docker", "secret"), (".kube", "secret"), ("credentials", "secret")], left
    size, over = plan._tree_count(src, IgnoreMatcher.for_tree(src, ()), 1 << 30, 1000)
    assert over is None and size == sum(len(t) for t in FILES.values())
    assert "secret" in materialize._LEFT_BEHIND
    assert 'why in ("secret", "suspected_secret")' in inspect.getsource(jobs)
    text = surfaces.render_final(Mode.PUSH, None, "done", "server", None, excluded_secrets=["proj/credentials"])
    assert "Secrets left out:" in text and "proj/credentials" in text, text
    assert 'f"secrets excluded: ' in inspect.getsource(note)


def test_secret_names_bullet_git_workspace(user_home, hermes_home):
    """★ In a git workspace the secret file name does not apply to a folder (``credentials/creds.go`` is no tracked
    secret file); the tracked files below ``.docker``/``.kube`` are the summary's "Tracked files on the secrets
    list" (``GitInfo.denied_tracked``)."""
    bullet = _secret_bullet()
    assert (
        "In a git workspace a folder named like a secret file travels like any other, and the tracked files below a "
        'credential folder name are listed before the handover ("Tracked files on the secrets list", see '
        "**Unredacted**)." in bullet
    ), bullet
    repo = RepoBuilder.init(user_home / "repo")
    for rel, text in FILES.items():
        repo.write(rel, text)
    repo.commit_all("seed")
    info = gitops.GitProbe(gitops.Git()).info(str(repo.root), make_cfg(user_home, hermes_home))
    assert info.denied_tracked == (".docker/nginx/default.conf", ".kube/deploy.yaml"), info.denied_tracked
    shown = surfaces.render_plan({"denied_tracked": list(info.denied_tracked)})
    assert "Tracked files on the secrets list" in shown and ".kube/deploy.yaml" in shown, shown


def test_secret_names_bullet_include_secret(user_home, hermes_home):
    """★ ``--include-secret`` (the policy's ``include_secret``, exact paths) sends the folder; below a credential
    folder name every file needs its own path as well."""
    bullet = _secret_bullet()
    assert (
        "To send such a folder anyway, name it with `--include-secret <path>` — below a credential folder name each "
        "file as well." in bullet
    ), bullet
    src = user_home / "proj"
    _lay(src)

    def scan(*lifted: str):
        return _scan(src, make_cfg(user_home, hermes_home, include_secret=tuple(str(src / p) for p in lifted)))

    entries, _ = scan("credentials")
    assert "credentials/creds.go" in entries, entries
    entries, left = scan(".kube")
    assert ".kube/deploy.yaml" not in entries and (".kube/deploy.yaml", "secret") in left, (entries, left)
    entries, _ = scan(".kube", ".kube/deploy.yaml")
    assert ".kube/deploy.yaml" in entries, entries


# ---------------------------------------------------------------------------------------------------------
# R2-L2: a pairing code pairs any public key
# ---------------------------------------------------------------------------------------------------------


@pytest.fixture
def listener(tmp_path, clock):
    led = Ledger(tmp_path / "listener.sqlite3", clock=clock)
    yield SimpleNamespace(ledger=led, identity=new_identity(led))
    led.close()


def _code(listener, clock) -> dict:
    code = pairing.create_code(listener.ledger, rights=frozenset({Right.PUSH}), name_hint="", ttl_s=600, clock=clock)
    text = pairing.pair_string(
        code, url="https://server.example", fingerprint=listener.identity.fingerprint(), name="server", profile=None
    )
    return pairing.parse_pair_string(text)


def _redeem(listener, clock, dialer, name: str) -> str:
    body = pairing.redeem_body(
        dialer,
        _code(listener, clock),
        listener_instance_id=listener.identity.instance_id,
        self_name=name,
        plugin_version="0.1.0",
    )
    return pairing.redeem_listener(listener.ledger, listener.identity, body, None, clock=clock)["peer_id"]


def test_pairing_bullet_a_code_pairs_a_foreign_key(tmp_path, listener, clock):
    """★ A valid code redeemed with somebody else's public key (no private key behind it): accepted; the earlier
    pairing of that key is revoked and its open job goes ``aborting``; the new row grants nothing, because a request
    signed with any other key does not verify against it (the server checks ``ctx.peer.pubkey``)."""
    bullet = _bullet("**A pairing code is all a pairing needs.**")
    for sentence in (
        "Redeeming a code does not prove that the device holds the private key of the public key it registers: "
        "whoever has a valid, unused pairing code can pair a public key that is not theirs (public keys are no "
        "secret — every bundle names its exporter's).",
        "That pairing gets no access, because every later request must be signed with the private key of that key; "
        "but it replaces the earlier pairing of that key here: that device is unpaired here (it pairs again) and its "
        "open handovers here are aborted.",
        "Treat a pairing code like a password until it is used or expires (10 minutes by default).",
    ):
        assert sentence in bullet, (sentence, bullet)
    victim_ledger = Ledger(tmp_path / "victim.sqlite3", clock=clock)
    attacker_ledger = Ledger(tmp_path / "attacker.sqlite3", clock=clock)
    try:
        victim = new_identity(victim_ledger)
        attacker = new_identity(attacker_ledger)
        victim_row = _redeem(listener, clock, victim, "laptop")
        job = ulid()
        listener.ledger.create_job(job, Role.TARGET, Mode.PUSH, victim_row, {"role": "target"})

        forged = SimpleNamespace(instance_id=ulid(), public_b64=victim.public_b64)  # only the public key
        new_row = _redeem(listener, clock, forged, "evil")

        assert listener.ledger.inbound_peer(victim_row) is None
        assert listener.ledger.job(job, Role.TARGET)["state"] == "aborting"
        peer = listener.ledger.inbound_peer(new_row)
        assert peer is not None and peer.pubkey == victim.public_b64()
        msg = b"POST /api/plugins/ctx_handover/v1/target/open " + str(time.time()).encode()
        assert crypto.verify(peer.pubkey, msg, attacker.sign(msg)) is False
        assert crypto.verify(peer.pubkey, msg, victim.sign(msg)) is True  # only the key's owner could use it
        assert "verify(ctx.peer.pubkey" in inspect.getsource(server)
        assert '"pubkey": self._identity.public_b64()' in inspect.getsource(bundle)
        assert pairing.DEFAULT_TTL_S == 600
    finally:
        victim_ledger.close()
        attacker_ledger.close()


# ---------------------------------------------------------------------------------------------------------
# R3-L1 (text from r4-a through the integrator): a first ledger read that fails
# ---------------------------------------------------------------------------------------------------------


def test_lock_paragraph_names_the_unread_window():
    """★ The lock paragraph names the window of R3-L1: the ledger's lock wait (``busy_timeout`` 5000 ms), turns not
    checked until a read succeeds (the refresher keeps trying, ``VetoIndex.poll_s``), and the two log lines r4-a added
    (``VetoIndex.start`` for the failed first read, ``refresh_now`` for its end). The behaviour of those lines is
    r4-a's test (``tests/unit/A/test_r4_a_l1_l3.py``).

    Red before r4-f: the sentence was not in the README."""
    from ctx_handover.engine import ledger, lock

    lock_part = FLAT.split("## What the lock on the source guarantees", 1)[1].split("What is **not** guaranteed", 1)[0]
    assert (
        "If the ledger cannot be read when a Hermes process starts (another process holds its lock for more than 5 s), "
        "turns in that process are not checked against the source lock until a read succeeds (the plugin keeps "
        "trying); the plugin logs the start and the end of that window." in lock_part
    ), lock_part
    assert "PRAGMA busy_timeout=5000" in inspect.getsource(ledger)
    assert inspect.signature(lock.VetoIndex).parameters["poll_s"].default > 0
    start, refresh = inspect.getsource(lock.VetoIndex.start), inspect.getsource(lock.VetoIndex.refresh_now)
    assert "log.warning(" in start and "the handover ledger could not be read yet" in start
    assert "log.warning(" in refresh and "the handover ledger is read now" in refresh


# ---------------------------------------------------------------------------------------------------------
# R3-M3 and R3-M2 (r4-ke's fixes; texts from r4-ke's report §7 through the integrator)
# ---------------------------------------------------------------------------------------------------------


def _test_names(rel: str) -> set[str]:
    source = (PLUGIN_ROOT / rel).read_text(encoding="utf-8")
    return {line.split("(", 1)[0][4:] for line in source.splitlines() if line.startswith("def test_")}


def test_lfs_sentence_names_what_the_checks_count():
    """★ R3-M3: the LFS content counts against the size limit and the free-space check before anything is written —
    the objects once, once more per pointer file when git-lfs is there (``gitapply._lfs_bytes``; behaviour in r4-ke's
    ``tests/unit/E/test_r4_ke_lfs_room.py``).

    Red before r4-f: the README said nothing about LFS content and the checks (REVIEW-3 §4 "nicht im README")."""
    from ctx_handover.engine import gitapply

    use = FLAT.split("## Use", 1)[1].split("## Settings", 1)[0]
    assert (
        "without git-lfs there they stay pointers and the final message says so. The size limit (`max_total_gb`, per "
        "handover) and the free-space check at the destination count that content before anything is written: each "
        "shipped object once and, when git-lfs is installed there, once more for every file that points to it." in use
    ), use[:400]
    bytes_src = inspect.getsource(gitapply._lfs_bytes)
    assert 'shutil.which("git-lfs")' in bytes_src and "_lfs_sizes(" in bytes_src
    names = _test_names("tests/unit/E/test_r4_ke_lfs_room.py")
    for name in (
        "test_r4_ke_m3_fresh_cap_counts_the_lfs_checkout",
        "test_r4_ke_m3_fresh_free_space_counts_the_lfs_checkout",
        "test_r4_ke_m3_job_cap_counts_the_lfs_bytes_of_a_later_item",
        "test_r4_ke_m3_without_git_lfs_only_the_placed_objects_count",
    ):
        assert name in names, (name, sorted(names))


def test_unredacted_names_the_bound_of_the_hard_link_check():
    """★ R3-M2: the bound of the hard-link check (``policy.LINKED_SCAN_MAX``), its order (fixed files first, browser
    profiles last) and what happens beyond it (a linked file it could not compare is left out — ``secret`` once
    Hermes' places were read, ``hermes`` otherwise); behaviour in r4-ke's ``test_r4_ke_hardlink_budget.py``.

    Red before r4-f: "or beyond the first 4096 entries the check reads … is caught only by the name and content
    checks" — wrong after R3-M2."""
    unredacted = FLAT.split("**Unredacted:**", 1)[1].split("**Not moved:**", 1)[0]
    assert "beyond the first" not in unredacted, unredacted
    assert (
        f"The check reads at most {policy.LINKED_SCAN_MAX} entries of the credential places per scan: first the fixed "
        "files (Hermes' credential stores, the names at the top of your home and of `~/.config`), then the folders "
        "level by level, browser profiles last. When it cannot read them all (a large `~/.kube/cache` or browser "
        "profile), every file with more than one link it could not compare is left out as well — as a secret "
        "(`--include-secret` on its path releases it) once all of Hermes' places were read, otherwise like Hermes' "
        "own files (nothing releases it)." in unredacted
    ), unredacted
    walk = inspect.getsource(policy._credential_identities)
    assert (
        walk.index("HERMES_CREDENTIAL_PATHS") < walk.index("queue.extend(stores)") < walk.rindex("_BROWSER_DENY_DIRS")
    )
    assert 'unknown = "hermes" if "hermes" in w.unread else ("secret" if w.unread else None)' in walk
    names = _test_names("tests/unit/K0/test_r4_ke_hardlink_budget.py")
    for name in (
        "test_r4_ke_a_large_kube_cache_does_not_hide_the_credential_files",
        "test_r4_ke_exhausted_bound_refuses_an_unknown_linked_file",
        "test_r4_ke_exhausted_bound_in_a_hermes_place_is_never_released",
        "test_r4_ke_a_flat_store_does_not_starve_the_names_and_the_hermes_places",
    ):
        assert name in names, (name, sorted(names))
