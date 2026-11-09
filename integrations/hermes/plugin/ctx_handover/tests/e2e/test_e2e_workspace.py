"""Layer 3 — workspace policy on the real stand (TESTPLAN §4 E8, E9).

Repositories are built only with ``RepoBuilder`` (technical identity per git call). Secret values are unique
markers, so a byte search over B's handover folder, plugin data and session files proves they did not arrive.

Runtime estimate: 3–5 min (E8 two handovers incl. a gateway turn with a terminal tool call, E9 one pull).
"""

from __future__ import annotations

import json
import os
import re
from pathlib import Path

import pytest

from . import _lab

pytestmark = pytest.mark.skipif(not _lab.enabled(), reason=_lab.SKIP_REASON)

_SKIP_DIRS = {"installs", "tools", "uv", ".pycache", "__pycache__", "logs"}


def _find_bytes(root: Path, needles: list[bytes]) -> list[str]:
    """Files under ``root`` (skipping Hermes' managed runtime) that contain one of ``needles``."""
    hits = []
    for dirpath, dirnames, filenames in os.walk(root):
        dirnames[:] = [d for d in dirnames if d not in _SKIP_DIRS]
        for name in filenames:
            path = Path(dirpath) / name
            try:
                if path.is_symlink() or path.stat().st_size > 64 * 1024 * 1024:
                    continue
                data = path.read_bytes()
            except OSError:
                continue
            hits += [f"{path}: {n.decode()}" for n in needles if n in data]
    return hits


def _stash_names(path: Path) -> dict[str, list[str]]:
    rb = _lab.Lab.repo(path)
    out = {}
    for commit, _msg in rb.stash_list():
        names = []
        for ref in (commit, f"{commit}^3"):
            res = rb.run("ls-tree", "-r", "--name-only", ref, check=False)
            names += res.stdout.decode(errors="replace").split()
        out[commit] = names
    return out


@pytest.mark.e2e_timeout(900)
def test_e08_secrets_denylist(lab, ev):
    """E8 (R23, R25, SEC 5.x): an untracked ``.env``, a staged ``.env.local``, a stashed ``.env`` (``stash -u``), an
    ``id_ed25519`` outside the workspace that the transcript mentions, and a private key inside ``notes.txt`` are
    absent on B after a pull with ``--yes``, and each is listed (job status and note); ``.env.example`` arrives.
    The ``--include-secret`` half is ``test_e08_include_secret_transfers_exactly_one_file``.

    Deviation: the key lives in ``lab/ws-e2e/a/keys`` and that folder is A's
    ``export_roots`` for the pull: in this lab ``$HOME`` (``lab/a/home``) lies inside HERMES_HOME, where nothing is
    ever an extra, and mentions outside ``$HOME``/``export_roots`` are dropped by the collector (DESIGN §8.2)."""
    tag = _lab.uid(8)
    secrets = {
        "env": f"API_TOKEN=E08-ENV-{tag}",
        "env_stash": f"API_TOKEN=E08-STASH-{tag}",
        "env_local": f"DB_PASSWORD=E08-LOCAL-{tag}",
        "notes": f"-----BEGIN OPENSSH PRIVATE KEY-----\nE08-NOTES-{tag}\n-----END OPENSSH PRIVATE KEY-----\n",
        "key": f"-----BEGIN OPENSSH PRIVATE KEY-----\nE08-KEY-{tag}\n-----END OPENSSH PRIVATE KEY-----\n",
    }
    key_path = lab.ws("a") / "keys" / "id_ed25519"
    key_path.parent.mkdir(parents=True, exist_ok=True)
    key_path.write_text(secrets["key"], encoding="utf-8")
    key_path.chmod(0o600)
    cwd = lab.ws("a") / "e08proj"
    rb = lab.make_repo(cwd, dirty=False, ignored=False)
    rb.write(".env", secrets["env_stash"] + "\n")
    rb.write("stashed-note.txt", "stashed together with .env\n")
    rb.stash("wip with env", untracked=True)
    rb.write(".env", secrets["env"] + "\n")
    rb.write(".env.local", secrets["env_local"] + "\n")
    rb.add(".env.local")
    rb.write("notes.txt", secrets["notes"])
    rb.write(".env.example", "API_TOKEN=\n")
    sid = lab.new_session("a", cwd)
    tools = [{"name": "terminal", "arguments": {"command": f"ls -la {key_path}"}}]
    token = f"E08-TOOLS-{tag}"
    rule = {"id": token, "when": {"kind": "main", "last_user_contains": token}, "reply": {"tool_calls": tools}}
    with lab.llm_rules([rule]):
        turn = lab.turn("a", sid, f"check the deploy key {token}", timeout=300)
    assert turn.ok, turn.brief()
    with lab.db("a", write=True) as db:
        db.set_session_title(sid, f"e08 secrets {lab.run_id}")

    with lab.preserved("a", restart=("serve-a",)):
        lab.setting("a", "export_roots", json.dumps([str(key_path.parent)]))
        lab.restart("serve-a")
        res = lab.handover("b", "take", _lab.SERVER, sid, "--yes", timeout=600)
    ev.cli("take", res)
    assert res.rc == 0, res.brief()
    status = lab.job_status("b", res.job_id)
    ev.write("job-b.json", status)
    job = status["job"]
    assert job.get("state") == "done", job
    sid_b = str((job.get("ack") or {}).get("session_id") or "")
    dest = Path(str(lab.row("b", sid_b).get("cwd") or ""))
    assert dest.is_dir(), dest
    assert (dest / ".env.example").is_file()
    for rel in (".env", ".env.local", "notes.txt"):
        assert not (dest / rel).exists(), rel
    index = lab.repo(dest).index_entries().decode(errors="replace")
    assert ".env.local" not in index, index
    stash = _stash_names(dest)
    ev.write("stash-b.json", stash)
    assert all(".env" not in names for names in stash.values()), stash
    needles = [v.splitlines()[1 if v.startswith("-----") else 0].encode() for v in secrets.values()]
    leaks = _find_bytes(lab.handover_root("b"), needles) + _find_bytes(lab.data_dir("b"), needles)
    leaks += _find_bytes(lab.home("b") / "cache", needles)
    ev.write("leaks-b.json", leaks)
    assert not leaks, leaks
    listed = " ".join(
        str(x) for key in ("exclusions", "excluded_secrets", "withheld_stash") for x in (job.get(key) or [])
    )
    note = str((lab.arrival("b", sid_b) or {}).get("note") or "")
    ev.write("listed.json", {"status": listed, "note": note})
    for name in (".env", ".env.local", "notes.txt", "id_ed25519"):
        assert re.search(rf"(^|[/\s'\"]){re.escape(name)}($|[\s'\",)])", listed), f"{name} not listed in status"
        assert name in note, f"{name} is not named in the note"
    assert "stash" in (listed + note).lower(), "the withheld stash entry is not listed"


@pytest.mark.e2e_timeout(600)
def test_e08_include_secret_transfers_exactly_one_file(lab, ev):
    """E8, second half (R25, X07): a push from A with ``--include-secret <repo>/.env --yes`` transfers exactly that one
    secret file; the private key in ``notes.txt`` stays behind.

    Deviation: the confirmation is given with ``--yes`` (the CLI's confirmation); answering the ``ask_choice`` dialog
    would need a Desktop client."""
    tag = _lab.uid(8)
    secrets = {
        "env": f"API_TOKEN=E08-ENV-{tag}",
        "notes": f"-----BEGIN OPENSSH PRIVATE KEY-----\nE08-NOTES-{tag}\n-----END OPENSSH PRIVATE KEY-----\n",
    }
    cwd2 = lab.ws("a") / "e08bproj"
    rb2 = lab.make_repo(cwd2, dirty=False, ignored=False)
    rb2.write(".env", secrets["env"] + "\n")
    rb2.write("notes.txt", secrets["notes"])
    src2 = lab.new_session("a", cwd2)
    assert lab.turn("a", src2, "second secrets session").ok
    with lab.db("a", write=True) as db:
        db.set_session_title(src2, f"e08 include {lab.run_id}")
    pushed = lab.handover(
        "a", "to", _lab.LAPTOP_GW, "--session", src2, "--include-secret", str(cwd2 / ".env"), "--yes", timeout=600
    )
    ev.cli("push-include-secret", pushed)
    assert pushed.rc == 0, pushed.brief()
    status2 = lab.job_status("a", pushed.job_id)
    ev.write("job-a-push.json", status2)
    sid_b2 = str((status2["job"].get("ack") or {}).get("session_id") or "")
    dest2 = Path(str(lab.row("b", sid_b2).get("cwd") or ""))
    assert (dest2 / ".env").read_text(encoding="utf-8").strip() == secrets["env"]
    assert not (dest2 / "notes.txt").exists()
    secretish = [
        p.name for p in dest2.rglob("*") if p.is_file() and ".git" not in p.parts and p.name in {".env", "notes.txt"}
    ]
    assert secretish == [".env"], secretish


@pytest.mark.e2e_timeout(600)
def test_e09_conflict_with_uncommitted_changes(lab, ev):
    """E9 (BRIEF D10, DESIGN §9.7): B already has a repository at the mapped path (``~/hermes-handover/<name>``)
    with a local edit and an untracked file; after the pull that folder is byte-identical (``.git`` included) and the
    handed-over repository lives in ``<dest>-handover-<YYYYmmdd-HHMMSS>``."""
    from ctx_handover.tests.fakes.repo_builder import dir_fingerprint

    name = f"e09proj{lab.run_id}"
    existing = lab.handover_root("b") / name
    rb_b = lab.make_repo(existing, dirty=False, ignored=False, files={"local.txt": "b's own repository\n"})
    rb_b.write("README.md", "# local edit on the laptop\n")
    rb_b.write("untracked-on-b.txt", "do not touch\n")
    before = dir_fingerprint(existing)
    src = lab.make_source("a", name, turns=("conflict",))
    repo_a = _lab.repo_state(src.repo)
    res = lab.handover("b", "take", _lab.SERVER, src.sid, "--yes", timeout=600)
    ev.cli("take", res)
    status = lab.job_status("b", res.job_id) if res.job_id else {}
    ev.write("job-b.json", status)
    assert dir_fingerprint(existing) == before, "B's existing repository was modified"
    assert res.rc == 0, res.brief()
    job = status["job"]
    assert job.get("state") == "done", job
    path, _mode = _lab.dest_of(job, name)
    ev.write("dest.json", {"path": path, "ack": job.get("ack")})
    assert path and re.fullmatch(rf"{re.escape(str(existing))}-handover-\d{{8}}-\d{{6}}", path), path
    repo_b = _lab.repo_state(Path(path))
    for key in ("head", "status", "index", "refs", "tree"):
        assert repo_b[key] == repo_a[key], key
