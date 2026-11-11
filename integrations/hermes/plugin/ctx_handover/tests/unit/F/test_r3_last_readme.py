"""The README texts proposed in review round 3, each bound to the code behind it.

* **R2-M3 / L3-F26** (Use, workspace paragraph): a working folder git carries nothing from but placeholders is a
  scratch folder; files git ignores never travel with a repository; a file named in a folder another repository ignores
  is an item of its own — and the limit: a file ignored only by its own name still points to its repository.
* **R2-M2** (Use, summary paragraph): the repositories that stay here include every clone git does not ignore, also
  one the session never touched (the production probe's ``nested_repos``).
* **M10** (Use, note paragraph): hook outputs arrive in the received session's own folder, same names; the note
  names the folder per old folder.
* **H5 rest** (Rights): owner homes by the name service, Windows homes behind WSL's drive mounts, localised OneDrive
  documents folders — and the three named limits.
* **L9** (Unredacted, Reads): a hard link to a known credential file is left out as that file; the limits (other
  places, the scan bound ``policy.LINKED_SCAN_MAX``); Reads names the owner lookup and the identity walk.

Each test reads the README sentence and the constant, signature or behaviour behind it (L28: no free-standing
substrings). Red before the fix: none of these sentences was in the README.
"""

from __future__ import annotations

import inspect
import os
import re
from dataclasses import replace
from pathlib import Path

from ctx_handover.engine import collector, gitops, gitpack, hermes_files, local_target, note, plan, policy
from ctx_handover.engine.gitops import Git, GitProbe
from ctx_handover.engine.model import HermesPaths, ItemKind, Options
from ctx_handover.engine.policy import PolicyConfig
from ctx_handover.tests.fakes.planning_fake import static_discover
from ctx_handover.tests.fakes.repo_builder import RepoBuilder
from ctx_handover.tests.unit.D.conftest import lay as d_lay  # noqa: F401  (fixture)
from ctx_handover.tests.unit.D.conftest import (
    transcript,  # noqa: F401  (fixture)
    write,
)
from ctx_handover.tests.unit.F._docs import docs_text
from ctx_handover.tests.unit.K0.test_r3_kd_hardlinks import HERMES, OK, SECRET, _link, _write
from ctx_handover.tests.unit.K0.test_r3_kd_hardlinks import lay as k0_lay  # noqa: F401  (fixture)

PLUGIN_ROOT = Path(policy.__file__).resolve().parents[1]
FLAT = " ".join(docs_text().split())


def _between(start: str, end: str) -> str:
    return FLAT.split(start, 1)[1].split(end, 1)[0]


def _use() -> str:
    return _between("## Use", "## Settings")


# ---------------------------------------------------------------------------------------------------------
# Use: the workspace (R2-M3, L3-F26)
# ---------------------------------------------------------------------------------------------------------


def test_workspace_paragraph_names_the_placeholder_rule(tmp_path):
    """★ R2-M3: a working folder git carries nothing from but placeholders (``collector.PLACEHOLDER_NAMES``) while
    other files lie there travels alone, as a file tree — asked through the production probe's ``carried_files``,
    which answers ``(".gitkeep",)`` for ``tmp/*`` with ``!tmp/.gitkeep``."""
    use = _use()
    sentence = (
        "unless git ignores the working folder there (a scratch or build folder inside a checkout) or carries nothing "
        "from it but placeholder files such as `.gitkeep` while other files lie there (`tmp/*` with `!tmp/.gitkeep`): "
        "then only the working folder travels, as a file tree, and the repository around it stays here."
    )
    assert sentence in use, use[:600]
    assert ".gitkeep" in collector.PLACEHOLDER_NAMES and "file tree" in collector.CONTENT_IGNORED_REASON
    assert "carried_files" in inspect.getsource(collector._Classifier.placeholder_only)
    repo = RepoBuilder.init(tmp_path / "r")
    repo.write(".gitignore", "tmp/*\n!tmp/.gitkeep\n")
    repo.write("tmp/.gitkeep", "")
    repo.write("src/a.py", "a\n")
    repo.commit_all("seed")
    write(repo.root / "tmp" / "notes.md", "work\n")
    assert GitProbe(Git(), ceiling=tmp_path).carried_files(str(repo.root / "tmp"), collector.MAX_CARRIED_PROBE) == (
        ".gitkeep",
    )


def test_workspace_paragraph_says_ignored_files_stay_with_the_repository():
    """★ L3-F26 (observation): what git ignores inside a repository that travels stays here, also when the
    conversation names it — the pack lists untracked files with ``--exclude-standard`` and never asks for ignored
    ones."""
    use = _use()
    assert (
        "Files git ignores do not travel with a repository (build outputs, caches), also when the conversation names "
        "them." in use
    )
    scan = inspect.getsource(gitpack._scan_untracked)
    assert '"--exclude-standard"' in scan and "--ignored" not in inspect.getsource(gitpack)


def _collect(t, lay):
    hp = HermesPaths(home=str(lay.hermes_home), document_cache=str(lay.hermes_home / "cache" / "documents"))
    probe = GitProbe(Git(), ceiling=lay.home.parent)
    return collector.collect(
        t.rows(), t.session_row(), hp, lay.cfg(), probe, source_os="linux", discover=static_discover([])
    )


def test_workspace_paragraph_names_mentions_in_ignored_places_and_the_limit(d_lay, transcript):  # noqa: F811
    """★ L3-F26 with the production probe: a file named in a folder another repository ignores is an extra of its
    own (that repository is no candidate); the named limit — a file ignored only by its own pattern in a folder git
    does not ignore (``*.log`` in ``src/``) — still makes the repository the candidate, and the pack would not carry
    the file. If the limit is lifted one day, this test turns red and the README sentence goes."""
    use = _use()
    assert (
        "A file the conversation names in a folder git ignores in another repository is an item of its own outside the "
        "project, never the repository around it; a file git ignores only by its name, in a folder it does not ignore "
        "(`*.log` in `src/`), still points the plan to that repository, which does not carry the file." in use
    )
    root = d_lay.home / "work" / "other"
    other = RepoBuilder.init(root)
    other.write(".gitignore", "build/\n*.log\n")
    other.write("src/app.py", "print('x')\n")
    other.commit_all("other")
    write(root / "build" / "out.txt", "build output\n")
    write(root / "src" / "run.log", "a log\n")
    real = os.path.realpath

    t = transcript(d_lay.project)
    t.terminal(f"tail {root / 'build' / 'out.txt'}")
    by_path = {c.path: c for c in _collect(t, d_lay)}
    assert real(root) not in by_path, sorted(by_path)
    assert by_path[real(root / "build" / "out.txt")].kind is ItemKind.EXTRA_FILE

    t = transcript(d_lay.project)
    t.terminal(f"tail {root / 'src' / 'run.log'}")
    by_path = {c.path: c for c in _collect(t, d_lay)}
    assert by_path[real(root)].kind is ItemKind.GIT_REPO, sorted(by_path)
    assert real(root / "src" / "run.log") not in by_path


# ---------------------------------------------------------------------------------------------------------
# Use: repositories that stay here (R2-M2), hook outputs (M10)
# ---------------------------------------------------------------------------------------------------------


def test_summary_names_every_repository_that_stays_here():
    """★ R2-M2: the summary's ``repos_left`` holds each initialised submodule and each clone git does not ignore, also
    one the session never touched — the collector asks the production probe's ``nested_repos`` (git's own list of the
    clones the pack leaves out); behaviour: ``E/test_r3_kd_gitprobe.py::test_collector_with_the_production_probe``."""
    use = _use()
    assert (
        "the repositories inside the workspace that stay here (each initialised submodule and each clone git does not "
        "ignore, also one the session never touched)" in use
    )
    assert callable(getattr(gitops.GitProbe, "nested_repos", None))
    assert "nested_repositories(" in inspect.getsource(gitops.GitProbe.nested_repos)
    assert "nested_repos" in inspect.getsource(collector._Classifier.nested)
    assert "cls.nested(" in inspect.getsource(collector.collect)
    left = inspect.getsource(plan.repos_left)
    assert "WHY_SUBMODULE" in left and "WHY_NESTED_REPO" in left


def test_note_paragraph_says_where_hook_outputs_are():
    """★ M10: hook outputs arrive in ``<HERMES_HOME>/hook_outputs/<received session id>/`` under their names
    (``hermes_files.target_path`` = ``hook_outputs_dir`` + name); the target maps every old folder to the folder it
    placed the files in (``LocalTarget._hook_folders`` in the prepare step) and the note's ``HOOK_LINE`` names it per
    old folder."""
    use = _use()
    assert (
        "Hook-output files arrive in the received session's own folder (`hook_outputs/<session id>/` in the Hermes "
        "home there, same file names); the conversation still names the old folders, so the note lists, for each old "
        "folder, the folder its files are in now." in use
    )
    target = HermesPaths(home="/srv/h/.hermes", document_cache="/srv/h/.hermes/cache/documents")
    placed = hermes_files.target_path("hook_outputs", "1f2e.txt", "0" * 64, target, "20261109_tip")
    assert placed == Path("/srv/h/.hermes/hook_outputs/20261109_tip/1f2e.txt")
    assert placed.parent == hermes_files.hook_outputs_dir(target, "20261109_tip")
    assert "same file names" in note.HOOK_LINE and "{old}" in note.HOOK_LINE and "{here}" in note.HOOK_LINE
    assert "self._hook_folders(placed, items)" in inspect.getsource(local_target.LocalTarget._prepare)
    assert 'Path(entry["target"]).parent' in inspect.getsource(local_target.LocalTarget._hook_folders)


# ---------------------------------------------------------------------------------------------------------
# Rights (H5 rest)
# ---------------------------------------------------------------------------------------------------------


def _target_cfg(allow: tuple[str, ...]) -> PolicyConfig:
    hermes = Path("/home/u/.hermes")
    return PolicyConfig.from_options(
        replace(Options(), allow_roots=allow),
        home=Path("/home/u"),
        hermes_home=hermes,
        hermes_root=hermes,
        plugin_dir=hermes / "plugin-data" / "ctx_handover",
        case_insensitive=False,
        target_os="linux",
    )


def _dest(path: str, allow: tuple[str, ...]) -> bool:
    return policy.check_target_dest(Path(path), _target_cfg(allow), recorded=False, local_choice=False).allowed


def test_rights_name_owner_homes_wsl_homes_and_localised_onedrive():
    """★ H5 rest: Rights names the owner homes (``_uid_home``: ``pwd.getpwuid``), the Windows homes behind WSL's drive
    mounts (``WSL_HOME_PARENTS``) with the Windows rules and the drive's system folders (``TARGET_SYSTEM_DENY_WSL``),
    and OneDrive's localised documents folder (``ONEDRIVE_DOCUMENTS_NAMES``) — checked on a Linux target."""
    rights = _between("**Rights:**", "**Unredacted:**")
    for part in (
        "the homes of the password database and of the owners of the folders on the way",
        "and the Windows homes behind WSL's drive mounts, `/mnt/<drive>/Users/*` and `/<drive>/Users/*`)",
        "`ProgramData`, and the same folders of each WSL drive)",
        "on Windows and on those WSL drives also not in the PowerShell profile folders (in `Documents` and in "
        "OneDrive's documents folder, also under its localised name such as `OneDrive/Dokumente`) or `WindowsApps`",
    ):
        assert part in rights, part
    assert "getpwuid" in inspect.getsource(policy._uid_home)
    assert policy.WSL_HOME_PARENTS == ("mnt/[a-z]/Users", "[a-z]/Users")
    assert {s.rsplit("/", 1)[-1] for s in policy.TARGET_SYSTEM_DENY_WSL} == set(policy.TARGET_SYSTEM_DENY_WINDOWS)
    assert "Dokumente" in policy.ONEDRIVE_DOCUMENTS_NAMES
    for path, allow in (
        ("/mnt/c/Users/u/.ssh", ("/mnt/c/Users/u",)),
        ("/c/Users/u/Documents", ("/c/Users/u",)),
        ("/mnt/c/Users/u/OneDrive/Dokumente/PowerShell", ("/mnt/c/Users/u",)),
        ("/mnt/d/ProgramData/x", ("/mnt/d",)),
    ):
        assert not _dest(path, allow), path


def test_rights_name_the_limits_of_the_home_recognition():
    """★ The three named limits hold as written: a WSL drive mounted elsewhere (``/media/c``) and a documents-folder
    name outside ``ONEDRIVE_DOCUMENTS_NAMES`` (Esperanto) are not recognised; an owner unknown to the name service
    gives no home (``_uid_home`` None). If one is lifted, this test turns red and the README sentence goes."""
    rights = _between("**Rights:**", "**Unredacted:**")
    assert (
        "A home whose owner the name service does not know, a WSL drive mounted elsewhere than `/mnt/<drive>` or "
        "`/<drive>`, and a OneDrive documents folder named in a language outside the plugin's list "
        "(`policy.ONEDRIVE_DOCUMENTS_NAMES`) are not recognised." in rights
    )
    assert _dest("/media/c/Users/u/Documents/WindowsPowerShell", ("/media/c/Users/u",))
    assert "Dokumentoj" not in policy.ONEDRIVE_DOCUMENTS_NAMES
    assert _dest("/mnt/c/Users/u/OneDrive/Dokumentoj/PowerShell", ("/mnt/c/Users/u",))
    assert policy._uid_home(2**31 - 7) is None


# ---------------------------------------------------------------------------------------------------------
# Unredacted, Reads (L9)
# ---------------------------------------------------------------------------------------------------------


def test_unredacted_names_the_hard_link_check(k0_lay):  # noqa: F811
    """★ L9: a hard link to a known credential file is left out as that file — ``--include-secret`` on its path
    releases one of the home, never a Hermes store — and one elsewhere is caught only by name and content (here a
    hard link between ``proj/.env`` and ``proj/notes.txt``: no identity known, no marker, allowed)."""
    unredacted = _between("**Unredacted:**", "**Not moved:**")
    assert (
        "A hard link to a credential file the plugin knows (in your home's secrets folders such as `~/.ssh`, `~/.aws` "
        "and the browser profiles, a file with a secret name at the top of your home or in `~/.config` or one folder "
        "below it, Hermes' credential stores) is left out as that file would be: `--include-secret` on its path "
        "releases one of your home, nothing releases Hermes'." in unredacted
    )
    # R3-M2: beyond the bound a linked file is no longer allowed — the sentence moved to
    # ``test_r4_f_readme.py::test_unredacted_names_the_bound_of_the_hard_link_check``
    assert (
        "A hard link to a credential file elsewhere (another account's home, any other place) is caught only by the "
        "name and content checks." in unredacted
    )
    assert {".ssh", ".aws", ".mozilla"} <= set(policy.SOURCE_DENY_DIRS)
    walk = inspect.getsource(policy._credential_identities)
    assert walk.rindex("_BROWSER_DENY_DIRS") > walk.rindex("HERMES_CREDENTIAL_PATHS")  # browser profiles last
    creds = _write(k0_lay.home / ".aws" / "credentials", "[default]\ntoken = abc\n")
    alias = _link(creds, k0_lay.project / "notes.txt")
    assert k0_lay.check(alias) == SECRET
    assert k0_lay.check(alias, k0_lay.cfg(include_secret=(str(alias),))) == OK
    env = _write(k0_lay.profile / ".env", "OPENAI_API_KEY=abc\n")
    hermes_alias = _link(env, k0_lay.project / "env.txt")
    assert k0_lay.check(hermes_alias, k0_lay.cfg(include_secret=(str(hermes_alias),))) == HERMES
    elsewhere = _write(k0_lay.project / "app" / ".env", "API_TOKEN=abc\n")
    assert k0_lay.check(_link(elsewhere, k0_lay.project / "app" / "notes.txt")) == OK


def test_reads_name_the_owner_lookup_and_the_identity_walk():
    """★ Reads discloses the two reads behind H5 rest and L9: the name-service lookup of each folder owner's home
    (``pwd.getpwuid``) and, when a file to send has more than one link, names and identities — never contents — of
    the entries at the credential places, at most ``LINKED_SCAN_MAX`` per scan."""
    reads = _between("**Reads:**", "**Writes:**")
    assert (
        "the home of the owner of each folder on the way to a destination from the name service (`getpwuid`)" in reads
    )
    assert (
        "when a file to send has more than one link, the names and file identities — not the contents — of the "
        "entries in the credential places of the hard-link check under **Unredacted** "
        f"(at most {policy.LINKED_SCAN_MAX} per scan)" in reads
    )
    walk = inspect.getsource(policy._IdentityWalk)
    assert "scandir" in walk and "stat(" in walk and not re.search(r"\bopen\(", walk)
    check = inspect.getsource(policy.check_source_file)
    assert check.index("st.st_nlink > 1") < check.index("_head(")  # the identity first, a read only after it


def test_ctx_record_links_every_root_id():
    """README ctx (optional): the head search uses the title ctx_checkpoint files the head under
    (``ctxrecords.head_token``, like ``checkpoint.head_title_for``): timestamp ids and UUIDs as they are, a longer id
    or one with other characters cut and marked with a hash — so the record finds the head for every id."""
    import pytest

    from ctx_handover.engine import ctxrecords

    checkpoint = pytest.importorskip("ctx_checkpoint.memory.checkpoint")
    for root in ("20261031_120000_abcdef", "95b87f05-c9be-4364-bae0-04db0eae6cc7", "local-" + "a" * 64, "team/alice:1"):
        assert checkpoint.head_title_for(root) == ctxrecords.HEAD_TITLE.format(root=ctxrecords.head_token(root))
    ctx = _between("**ctx (optional):**", "**Core modules used:**")
    assert "with a `search` for the checkpoint head before each `store` (to link the record to it)" in ctx
    assert "70-character `local-<sha256>` ids" in ctx and "the record finds the head for every id" in ctx
