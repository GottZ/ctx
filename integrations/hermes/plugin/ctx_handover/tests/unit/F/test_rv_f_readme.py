"""Review round rv-f — README statements against the code (``review-code/REVIEW.md`` §2 M33, L22–L28, §6-T, M12,
M6; hand-overs of rv-b (Bot Chat), rv-k0 (target denylist), rv-e (LFS), i-r36 (reason j), R33a (host isolation)).

Every test reads the README sentence it guards and the constant or signature behind it, so a change on either side
turns it red (L28: no free-standing substrings)."""

from __future__ import annotations

import inspect
import re

from ctx_handover.engine import (
    commands,
    crypto,
    ctxrecords,
    envfacts,
    gitapply,
    lock,
    policy,
    settings,
    surfaces,
)
from ctx_handover.engine import plan as planning
from ctx_handover.engine.model import EnvFacts, Options

from .test_surfaces import PLUGIN_ROOT

README = (PLUGIN_ROOT / "README.md").read_text(encoding="utf-8")
FLAT = re.sub(r"\s+", " ", README)
MANIFEST = (PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8")


def _section(start: str, end: str) -> str:
    return FLAT.split(start, 1)[1].split(end, 1)[0]


def _limits() -> str:
    return _section("## Limits", "## What the lock")


def _bullet(text: str, lead: str) -> str:
    return text.split(lead, 1)[1].split(" - ", 1)[0]


def test_rv_f_l24_no_byte_exact_claim():
    """★ L24: the hash compares normalised rows (``session_hash``), so neither README nor manifest says "byte-exact";
    both say row by row with a hash.

    Red before the fix: "byte-exact" in README:5 and plugin.yaml:3."""
    for text in (README, MANIFEST):
        assert not re.search(r"byte[- ]exact", text, re.IGNORECASE)
    assert "row by row" in FLAT and "row by row" in MANIFEST
    assert "ctxh-norm-v1" in (PLUGIN_ROOT / "engine" / "session_hash.py").read_text(encoding="utf-8")


def test_rv_f_l27_no_ctx_blob_relay_is_a_limit():
    """★ L27 (BRIEF §5 non-goal): the limits say that no handover is relayed through ctx blobs.

    Red before the fix: no "blob" in the limits."""
    limits = _limits()
    assert "**No relay through ctx.**" in limits
    assert "ctx blobs" in _bullet(limits, "**No relay through ctx.**")


def test_rv_f_r33a_host_isolation_named_exactly():
    """★ R33a: under ``plugins.isolation: host`` the routes are reachable through ``hermes serve`` and not through the
    gateway's own listener; a ``--yolo`` gateway behind a separate ``hermes serve`` is invisible to the plugin, so the
    approvals refusal does not apply and ``allow_remote_continue`` should stay off — the doctor line keeps naming the
    same ways (``commands.HOST_ISOLATION_DETAIL``).

    Red before the fix: the listener was only "not verified", no ``--yolo`` sentence."""
    host = _bullet(_limits(), "Under `plugins.isolation: host`")
    assert "reachable only through `hermes serve`" in host, host
    assert "the gateway's own listener does not serve them" in host, host
    assert "`hermes serve` forwards the ctx_handover routes there" in host
    # r3-f (H2 rest, REVIEW-2 §1/§2): the ``--yolo`` limit holds under every isolation, so it is its own Limits point
    # now ("Continuation and approvals", ``test_r3_f_h2.py``) instead of a sentence inside this paragraph
    yolo = _bullet(_limits(), "**Continuation and approvals.**")
    assert "--yolo" not in host and "`--yolo`" in yolo, (host, yolo)
    assert "the continuation is not refused" in yolo and "leave `allow_remote_continue` off" in yolo, yolo
    assert "`hermes serve`" in commands.HOST_ISOLATION_DETAIL and "own listener" in commands.HOST_ISOLATION_DETAIL


def test_rv_f_m6_lock_guarantee_5_names_its_start_and_retry():
    """★ M6: guarantee (5) says how long the first message after a start waits for the plugin, and that a ledger that
    cannot be opened is retried with growing pauses (bound to ``commands.HOOK_BUILD_WAIT_S``/``HOOK_RETRY_MAX_S``).

    Red before the fix: no such sentence (and no such constants)."""
    lock_section = _section("## What the lock on the source guarantees", "## Set up")
    wait = f"{commands.HOOK_BUILD_WAIT_S:g} seconds"
    cap = "a minute" if commands.HOOK_RETRY_MAX_S == 60 else f"{commands.HOOK_RETRY_MAX_S:g} seconds"
    assert f"waits up to {wait}" in lock_section, lock_section
    assert f"growing pauses of at most {cap}" in lock_section, lock_section
    assert "turns in between are not refused" in lock_section


def test_rv_f_bot_chat_exception_of_mark_4():
    """★ rv-b hand-over (M9 rest): Hermes forbids renaming the canonical Bot Chat, so mark (4) does not apply to it;
    a Bot Chat replaced meanwhile comes back as ``Bot Chat (before handover)`` (the name ``lock`` uses).

    Red before the fix: no Bot Chat sentence."""
    lock_section = _section("## What the lock on the source guarantees", "## Set up")
    # r3-f (M9, REVIEW-2 §4): "keeps its title" was wrong — archiving releases the name (real SessionDB, title None
    # while handed over; ``test_r3_f_m9.py``); the sentence now says so
    assert "*Bot Chat*" in lock_section and "loses its title while it is handed over" in lock_section, lock_section
    assert "`Bot Chat (before handover)`" in lock_section
    assert "(before handover)" in inspect.getsource(lock)


def test_rv_f_m33_writes_name_return_trip_and_local_choice():
    """★ M33: a destination lies under ``allow_roots`` unless this machine recorded it (return trip) or the person here
    typed it (``--dest``/``--map``) — the two exceptions of ``policy.check_target_dest`` — and the target denylist
    applies to every destination.

    Red before the fix: "writes land only under `allow_roots`" (README:182)."""
    params = inspect.signature(policy.check_target_dest).parameters
    assert {"recorded", "local_choice"} <= set(params)
    assert "writes land only under `allow_roots`" not in FLAT
    table = _section("## Settings", "## Disclosure")
    row = table.split("| `workspace_root`, `allow_roots` |", 1)[1].split(" | ", 2)[1]
    assert "the folder it came from (return trip" in row and "`--dest`/`--map` typed here" in row, row
    assert "the denylist under **Rights** applies to every destination" in row, row
    rights = _section("**Rights:**", "**Unredacted:**")
    # r3-f (M33 rest, REVIEW-2 §5): Rights also names the third ``recorded`` case (an earlier delivery of the same
    # peer), so the list is no longer "… and a returning repository"; ``test_r3_f_disclosure.py`` binds all three
    assert "a returning repository in the folder this machine handed it away from" in rights, rights


def test_rv_f_k0_target_denylist_named_from_policy():
    """★ rv-k0 hand-over (H5): the README lists where no destination may lie — every account home (the
    ``HOME_PARENTS`` folders, ``/root``, the password database), the system places (a subset of
    ``TARGET_SYSTEM_DENY``; Windows: ``TARGET_SYSTEM_DENY_WINDOWS``), PowerShell profiles (also under OneDrive) and
    ``WindowsApps`` (``TARGET_DENY_WINDOWS``), and that ``~/Documents`` itself is no destination on Windows.

    Red before the fix: none of it in the README."""
    rights = _section("**Rights:**", "**Unredacted:**")
    for parent in policy.HOME_PARENTS:
        assert f"`/{parent}/*`" in rights, parent
    assert f"`/{policy.FIXED_HOMES[0]}`" in rights
    assert "password database" in rights
    listed = re.findall(r"`/([a-z]+(?:/[a-z]+)?)`", rights.split("system places", 1)[1].split(";", 1)[0])
    listed = [x for x in listed if x not in {p.lower() for p in policy.HOME_PARENTS}]
    assert listed and set(listed) <= {s.lower() for s in policy.TARGET_SYSTEM_DENY}, listed
    for name in policy.TARGET_SYSTEM_DENY_WINDOWS:
        assert f"`{name}`" in rights, name
    windows = " ".join(policy.TARGET_DENY_WINDOWS)
    for word in ("PowerShell", "OneDrive", "WindowsApps"):
        assert word in windows and word in rights, word
    assert "`~/Documents` itself is no destination" in rights


def test_rv_f_l22_unredacted_names_every_session_file_kind():
    """★ L22: every kind of Hermes session file the plugin carries (``policy.HERMES_FILE_KINDS``) is in the
    Unredacted sentence — terminal output and attached images included.

    Red before the fix: only "transcripts, spillover and hook-output files"."""
    words = {
        "spillover": "spillover",
        "hook_outputs": "hook-output",
        "terminal_output": "terminal-output",
        "native_inputs": "attached images",
    }
    assert set(words) == set(policy.HERMES_FILE_KINDS)
    unredacted = _section("**Unredacted:**", "**Not moved:**").split(" go to the peer as they are", 1)[0]
    for kind, word in words.items():
        assert word in unredacted, (kind, unredacted)


def test_rv_f_l23_fallback_names_the_fail_closed_keys():
    """★ L23: a value of the wrong type counts as the default — except the ``FAIL_CLOSED_KEYS``, which count as
    ``off``.

    Red before the fix: "counts as that key's default" without the exception."""
    sentence = FLAT.split("A value of the wrong type or outside its choices", 1)[1].split(". ", 1)[0]
    assert settings.FAIL_CLOSED_KEYS
    for key in settings.FAIL_CLOSED_KEYS:
        assert f"for `{key}` as `off`" in sentence, sentence


def test_rv_f_l25_commands_name_the_tool_list_and_tool_probe():
    """★ L25: the Commands disclosure gives the size of ``envfacts.TOOL_ALLOWLIST``, the ``tool_probe`` setting and the
    scratch folder in the system temp directory the probes run in.

    Red before the fix: "about 30 developer tools" only."""
    cmds = _section("**Commands:**", "**Background work:**")
    assert f"a fixed list of {len(envfacts.TOOL_ALLOWLIST)} developer tools" in cmds, cmds
    assert "`tool_probe`" in cmds and "tool_probe" in Options.__dataclass_fields__
    assert "scratch folder in the system temp directory" in cmds
    assert envfacts.SCRATCH_PREFIX in cmds


def test_rv_f_l26_disclosure_names_facts_reads_search_and_thread():
    """★ L26: the environment facts each side sends (fields of ``EnvFacts``), the reads of ``/etc/os-release`` and
    ``/proc/self/status``, the ctx ``search`` before each ``store`` and the ``ctxh-maintenance`` thread.

    Red before the fix: none of them disclosed."""
    network = _section("**Network:**", "**Inbound:**")
    for field, words in {
        "hostname": "host name",
        "user": "user name",
        "home": "home folder",
        "timezone": "time zone",
        "disk_free": "free disk space",
        "tools": "tool versions and paths",
    }.items():
        assert field in EnvFacts.__dataclass_fields__, field
        assert words in network, (field, network)
    reads = _section("**Reads:**", "**Writes:**")
    assert "`/etc/os-release`" in reads and "`/proc/self/status`" in reads
    ctx = _section("**ctx (optional):**", "**Core modules used:**")
    assert "a `search` for the checkpoint head before each `store`" in ctx
    assert '"search"' in inspect.getsource(ctxrecords._checkpoint_head)
    background = _section("**Background work:**", "**Stored secrets:**")
    assert "`ctxh-maintenance`" in background
    assert "ctxh-maintenance" in (PLUGIN_ROOT / "engine" / "jobs.py").read_text(encoding="utf-8")


def test_rv_f_m29_ctx_wait_is_bound():
    """M29: the ctx paragraph says how long a job start waits for ctx at most (``ctxrecords.FLUSH_BUDGET_S``)."""
    ctx = _section("**ctx (optional):**", "**Core modules used:**")
    assert f"waits at most {ctxrecords.FLUSH_BUDGET_S:g} seconds for ctx" in ctx, ctx


def test_rv_f_l28_bindings_to_constants_and_parser():
    """★ L28 (g7 mutants M1/M3/M5): the identity variable is ``crypto.IDENTITY_ENV``; the refine budget is
    ``Options.refine_history_chars``; the cost sentence and the two "never" sentences stand; the command list is the
    parser's verb set exactly, not a substring hit.

    Red before the fix: ``logs`` dropped from the list or ``IDENTITY_ENV`` renamed stayed green."""
    secrets = _section("**Stored secrets:**", "**Model use")
    writes = _section("**Writes:**", "**Commands:**")
    assert f"`{crypto.IDENTITY_ENV}`" in writes and "an Ed25519 private key in `.env`" in secrets
    model = _section("**Model use", "**ctx (optional):**")
    assert f"up to {Options().refine_history_chars:,} characters" in model
    assert "Inference costs are yours." in model
    unredacted = _section("**Unredacted:**", "**Not moved:**")
    assert "Hermes' own credentials never" in unredacted
    assert "never writes into Hermes plugin, skill, memory or config directories" in unredacted
    listed = FLAT.split("All commands: `/handover ", 1)[1].split("` —", 1)[0]
    assert {v.strip() for v in listed.split("|")} == set(surfaces.VERBS), listed


def test_rv_f_trust_unsigned_is_disclosed():
    """★ §6-T: ``import --trust-unsigned`` (an option of the grammar) is in the README with what such an import does
    not get: no continuation, no return trip to the exporter, origin "unverified bundle".

    Red before the fix: no ``--trust-unsigned`` in the README."""
    assert "--trust-unsigned" in surfaces.USAGE["import"]
    rights = _section("**Rights:**", "**Not moved:**")
    sentence = rights.split("`import --trust-unsigned`", 1)[1].split(". ", 2)
    text = " ".join(sentence[:2])
    assert "never starts a continuation" in text and "no return trip" in text, text
    assert "unverified bundle" in text, text


def test_rv_f_m12_context_window_limit():
    """★ M12 (named limit): the size question (R32) needs the context window of the model there, which a plugin learns
    only from ``context_length`` in that gateway's ``config.yaml``; without it nothing is asked.

    Red before the fix: no such limit."""
    limits = _limits()
    assert "**The size check needs the context window there.**" in limits
    bullet = _bullet(limits, "**The size check needs the context window there.**")
    assert "`model.context_length`" in bullet and "nothing is asked" in bullet, bullet


def test_rv_f_reasons_j_and_k_in_the_readme():
    """★ i-r36 hand-over (R36a) and reason (k) of D: the confirmation reasons name a working folder that holds none of
    the repository's tracked files and a repository inside the workspace that stays here; ``--yes`` confirms
    neither.

    Red before the fix: "the workspace is the whole repository around the working folder" (no R36a condition), no
    (k)."""
    use = _section("## Use", "## Settings")
    assert "around a working folder that holds none of its tracked files" in use
    assert "a repository inside the workspace (a submodule or another clone) stays here" in use
    assert {planning.CONFIRM_WORKSPACE_DIFFERS, planning.CONFIRM_REPOS_LEFT} <= planning.YES_EXEMPT
    assert re.search(r"`--yes` does not confirm[^.]*repository inside the workspace", use), use


def test_rv_f_lfs_line():
    """★ rv-e hand-over (H8): LFS files are checked out from the shipped objects (``git lfs checkout``, no network);
    without git-lfs there they stay pointers and the final message warns (``gitapply._NO_LFS``).

    Red before the fix: no such line."""
    use = _section("## Use", "## Settings")
    assert "`git lfs checkout`" in use and "never from a network" in use and "stay pointers" in use
    assert "stay pointers" in gitapply._NO_LFS


def test_rv_f_h1_per_peer_credentials_disclosed():
    """★ H1, F part (rv-c §5): README and manifest name the per-peer variables a pairing writes (``pairing``'s
    prefixes), the address binding, the credential-free first request and the limit behind a dashboard sign-in.

    Red before the fix: no ``CTX_HANDOVER_PEER_TOKEN_<peer id>`` anywhere."""
    from ctx_handover.engine import pairing

    names = (
        f"`{pairing.PEER_TOKEN_PREFIX}<peer id>`",
        f"`{pairing.PEER_PASSWORD_PREFIX}<peer id>`",
        f"`{pairing.PEER_HEADER_PREFIX}<n>_<peer id>`",
    )
    writes = _section("**Writes:**", "**Commands:**")
    for name in names:
        assert name in writes, name
    for name in names:
        assert name.strip("`") in MANIFEST, name
    meaning = _section("**What the peer credential means:**", "**Rights:**")
    assert "goes only to the address it was stored for (scheme, host and port)" in meaning
    assert "The first request of a pairing carries no credential" in meaning
    assert "check that address before you paste it" in meaning
    reads = _section("**Reads:**", "**Writes:**")
    assert "through Hermes' `get_secret`" in reads
    assert "store_credentials" in inspect.getsource(pairing) and "credential_owner" in inspect.getsource(pairing)


def test_rv_f_m34_env_reads_disclosed():
    """★ M34 (F part, after rv-c/rv-k0): the Reads disclosure names the identity key read through ``get_secret`` with
    the ``.env`` file as the fallback (``crypto.Identity.load``) and the provider-key presence check
    (``hostcompat.provider_usable``: ``read_secret`` or ``read_env_fresh``).

    Red before the fix: no ``.env`` read named except its file mode."""
    from ctx_handover.engine import hostcompat

    reads = _section("**Reads:**", "**Writes:**")
    assert f"`{crypto.IDENTITY_ENV}` through `get_secret`" in reads, reads
    # r3-f (M34 rest, REVIEW-2 §5): the ``.env`` is also read when ``get_secret`` returns a stale key (g7 probe
    # ``test_g7_identity_env.py``); the old wording "only when `get_secret` does not return it" was wrong
    assert "the profile `.env` file itself only when `get_secret` returns no key or one that does not match" in reads
    assert "whether the provider of the session's model has a key here" in reads
    usable = inspect.getsource(hostcompat.provider_usable)
    assert "read_secret(var)" in usable and "read_env_fresh(var)" in usable
    load = inspect.getsource(crypto.Identity.load)
    assert "read_secret(IDENTITY_ENV)" in load and "read_env_fresh(IDENTITY_ENV)" in load


def test_rv_f_m37_readme_says_what_the_summary_names():
    """★ M37/H15/M12: the README says the summary names each outside item with its size (``plan.PRESEND_PATHS_MAX``
    largest, then a count), the repositories that stay here and an unknown context window; the M12 limit says the
    summary names it.

    Red before the fix: no such sentence."""
    use = _section("## Use", "## Settings")
    assert (
        f"every item outside the project with its size (the {planning.PRESEND_PATHS_MAX} largest, then how many more)"
        in use
    )
    assert "the repositories inside the workspace that stay here" in use
    assert "whether the context window of the model there is unknown" in use
    assert "a submodule travels only as the commit the project records" in use
    bullet = _bullet(_limits(), "**The size check needs the context window there.**")
    assert "the summary says that the context window there is unknown" in bullet
    assert "there unknown" in surfaces.context_unknown_text({"model": "m"})


def test_rv_f_retention_and_background_threads():
    """★ rv-a hand-over (H4, M6, M29): the Background-work disclosure names the ``ctxh-janitor`` and ``ctxh-outbox``
    threads (names from ``jobs``) and what the retention removes when (``ledger.JOURNAL_KEEP_S`` = 30 days).

    Red before the fix: neither thread nor a retention sentence."""
    from ctx_handover.engine import ledger

    background = _section("**Background work:**", "**Stored secrets:**")
    source = (PLUGIN_ROOT / "engine" / "jobs.py").read_text(encoding="utf-8")
    for thread in ("ctxh-janitor", "ctxh-outbox", "ctxh-maintenance"):
        assert f"`{thread}`" in background and f'"{thread}"' in source, thread
    days = int(ledger.JOURNAL_KEEP_S // 86400)
    assert "**Retention:** a source's staging goes when its handover ends" in background
    assert f"go after {days} days" in background and f"offers {days} days after they expired" in background
    assert "`/handover gc`" in background
