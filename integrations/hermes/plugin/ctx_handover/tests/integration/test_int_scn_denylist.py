"""Mandatory scenario 8 (layer 2, D10, R19, R23, R25, DESIGN §10.1, TESTPLAN E8): the secrets denylist on a real
push. The laptop's repository holds an untracked ``.env``, a staged ``.env.local``, a private key in the untracked
``notes.txt``, a stash entry that took an untracked ``.env`` along (``stash -u``) next to a harmless stash entry, and
the harmless ``.env.example``; the transcript read an SSH key ``~/keys/id_ed25519``, a file with an API token
(``~/docs/settings.txt``, a suspected secret) and Hermes' own ``auth.json``.

Visible results: no secret byte arrives anywhere on the server (work tree, index, stash, extras, Hermes home, block
store), ``.env.example`` and the harmless stash entry do; the exclusions are named in the target's note and in the
job status. With a local ``--include-secret`` (confirmed) exactly the named files travel — but Hermes' own
credential file never, and a suspected secret only through that single release (never through a blanket yes)."""

from __future__ import annotations

import os
import re
from pathlib import Path

from ctx_handover.engine import jobs
from ctx_handover.engine.model import Mode, Options, SessionRef
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.integration._int3_helpers import arrival, run_job
from ctx_handover.tests.integration.conftest import builder

KEY = (
    "-----BEGIN OPENSSH PRIVATE KEY-----\n"
    "b3BlbnNzaC1rZXktdjEAAAAABG5vbmUAAAAEbm9uZQAAAAAAAAABAAAAMwAAAAtzc2gtZW\n"
    "-----END OPENSSH PRIVATE KEY-----\n"
)


def _secrets(tag: str) -> dict[str, bytes]:
    """Distinct secret contents per scenario run (searched for byte-wise on the target)."""
    return {
        ".env": f"API_TOKEN=ctxh-int3-{tag}-dotenv-0f3a9c\n".encode(),
        ".env.local": f"DB_PASSWORD=ctxh-int3-{tag}-local-77e1b2\n".encode(),
        "stash .env": f"STASHED_TOKEN=ctxh-int3-{tag}-stash-5d20aa\n".encode(),
        "notes.txt": f"remember the deploy key\n{KEY}# ctxh-int3-{tag}-notes\n".encode(),
        "id_ed25519": f"{KEY}# ctxh-int3-{tag}-sshkey\n".encode(),
        "settings.txt": f"OPENAI_API_KEY=sk-proj-Int3{tag}AbCdEfGhIjKlMnOpQrStUvWxYz0123\n".encode(),
        "auth.json": f'{{"providers": {{"x": {{"api_key": "ctxh-int3-{tag}-hermes-auth"}}}}}}\n'.encode(),
    }


def _setup(laptop, tag: str) -> tuple[str, RepoBuilder, dict[str, bytes], dict[str, Path]]:
    s = _secrets(tag)
    r = RepoBuilder.init(laptop.home / f"proj-{tag}")
    r.write(".gitignore", "build/\n")
    r.write("README.md", "# app\n")
    r.write("src/app.py", "print('app')\n")
    r.add_all()
    r.commit("first")
    r.write("src/app.py", "print('harmless stash')\n")
    r.stash("harmless")
    r.write(".env", s["stash .env"])
    r.stash("secret in stash", untracked=True)
    r.write(".env", s[".env"])
    r.write(".env.local", s[".env.local"])
    r.add(".env.local")
    r.write("notes.txt", s["notes.txt"])
    r.write(".env.example", "API_TOKEN=\n")
    extras = {
        "id_ed25519": laptop.home / "keys" / "id_ed25519",
        "settings.txt": laptop.home / "docs" / "settings.txt",
        "auth.json": laptop.hermes_home / "auth.json",
    }
    for name, path in extras.items():
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_bytes(s[name])
    b = builder(laptop)
    sid = b.create(title=f"Secrets {tag}", cwd=str(r.root))
    b.user(sid, "check the keys and the settings")
    calls = [
        builder(laptop).tool_call("read_file", {"path": str(p)}, call_id=f"c{i}") for i, p in enumerate(extras.values())
    ]
    b.assistant(sid, "reading them", tool_calls=calls)
    for i, name in enumerate(extras):
        b.tool(sid, f"c{i}", f"contents of {name}", name="read_file")
    b.assistant(sid, "done")
    return sid, r, s, extras


def _files_with(root: Path, needle: bytes) -> list[str]:
    hits = []
    for dirpath, _dirs, files in os.walk(root):
        for name in files:
            path = os.path.join(dirpath, name)
            try:
                if not os.path.islink(path) and needle in Path(path).read_bytes():
                    hits.append(path)
            except OSError:
                continue
    return hits


def _status(laptop, job_id: str) -> dict:
    with laptop.active():
        return jobs.runner_for(None).status(job_id)["job"]


def _named(text: str, name: str) -> bool:
    return re.search(rf"(^|[/\s'\"(]){re.escape(name)}($|[\s,;)'\"])", text, re.MULTILINE) is not None


def test_int_scn_denylist_stay_behind_and_are_listed(online):
    """★ Scenario 8a: ``--yes`` (blanket confirmation) — every secret stays behind and is named.

    Red at ``a8fec516``: nothing leaks, but the pack's exclusions (untracked secrets,
    withheld stash entry) and the dropped suspected secret are named neither in the note nor in the job status."""
    server, laptop, _listener, peer = online
    sid, repo, s, _extras = _setup(laptop, "a")
    harmless = repo.stash_list()[1]
    res = run_job(laptop, Mode.PUSH, peer, SessionRef(id=sid), options=Options(refine="off", yes=True))
    assert res.rc == 0, res.why()
    dest = server.home / "hermes-handover" / repo.root.name
    leaked = {name: _files_with(server.root, data) for name, data in s.items()}
    assert {k: v for k, v in leaked.items() if v} == {}, "secret bytes arrived on the server"
    assert (dest / ".env.example").read_bytes() == b"API_TOKEN=\n"
    assert b".env.local" not in RepoBuilder(dest).index_entries(), "the staged secret is in the target index"
    assert RepoBuilder(dest).stash_list() == [harmless], "stash: the harmless entry must arrive, the secret one not"

    note = (arrival(server, res.job_id) or {}).get("note") or ""
    for name in (".env", ".env.local", "notes.txt", "id_ed25519", "settings.txt"):
        assert _named(note, name), (f"{name} is not named in the note", note)
    assert "stash entry withheld" in note, note
    status = _status(laptop, res.job_id)
    listed = " ".join(str(e.get("path")) for e in status["exclusions"])
    for name in (".env", ".env.local", "notes.txt", "id_ed25519", "settings.txt"):
        assert _named(listed, name), (f"{name} is not listed in the job status", status["exclusions"])
    assert status["withheld_stash"], status


def test_int_scn_include_secret_releases_exactly_the_named_files(online):
    """Scenario 8b: a local ``--include-secret`` for ``.env``, the suspected ``settings.txt`` and Hermes' own
    ``auth.json``, confirmed with *yes*: ``.env`` and ``settings.txt`` arrive byte-identical, ``auth.json`` never
    (R19), every other secret stays behind."""
    server, laptop, _listener, peer = online
    sid, repo, s, extras = _setup(laptop, "b")
    asked: list[dict] = []

    def confirm(info: dict) -> str:
        asked.append(info)
        return "yes"

    released = (str(repo.root / ".env"), str(extras["settings.txt"]), str(extras["auth.json"]))
    res = run_job(
        laptop,
        Mode.PUSH,
        peer,
        SessionRef(id=sid),
        options=Options(refine="off", yes=False),
        include_secret=released,
        confirm=confirm,
    )
    assert asked and "include_secret" in asked[0]["reasons"], "the local --include-secret was not confirmed"
    assert res.rc == 0, res.why()
    dest = server.home / "hermes-handover" / repo.root.name
    assert (dest / ".env").read_bytes() == s[".env"]
    assert _files_with(server.root, s["settings.txt"]), "the released suspected secret did not arrive"
    assert _files_with(server.root, s["auth.json"]) == [], "Hermes' own credential file left the machine (R19)"
    for name in (".env.local", "stash .env", "notes.txt", "id_ed25519"):
        assert _files_with(server.root, s[name]) == [], f"{name} arrived although it was not released"
