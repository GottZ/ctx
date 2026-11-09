"""K0: secrets and file policy (DESIGN §10.1–§10.4, R18, R19, R23, SEC 2.2–2.4, 5.1–5.4) — real files in tmp.

Every check runs against a real directory layout: a user home with a Hermes root (``~/.hermes``), an active
profile, credential directories, a project, symlinks and hard links.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, replace
from pathlib import Path

import pytest

from ctx_handover.engine import policy
from ctx_handover.engine.errors import PolicyDenied
from ctx_handover.engine.model import Options, PlanHints
from ctx_handover.engine.policy import PolicyConfig, Verdict
from ctx_handover.tests.conftest import require_upstream

OK = Verdict(True, None)
OPENSSH_KEY = (
    "-----BEGIN OPENSSH PRIVATE KEY-----\nb3BlbnNzaC1rZXktdjEAAAAABG5vbmU=\n-----END OPENSSH PRIVATE KEY-----\n"
)


@dataclass
class Layout:
    home: Path
    root: Path
    profile: Path
    project: Path
    plugin_data: Path

    def cfg(self, **over) -> PolicyConfig:
        options = over.pop("options", Options())
        kwargs = {
            "home": self.home,
            "hermes_home": self.profile,
            "hermes_root": self.root,
            "plugin_dir": self.plugin_data,
            "case_insensitive": False,
            "target_os": "linux",
        }
        kwargs.update(over)
        return PolicyConfig.from_options(options, **kwargs)


def _write(path: Path, text: str = "hello\n") -> Path:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(text, encoding="utf-8")
    return path


@pytest.fixture
def lay(tmp_path) -> Layout:
    home = tmp_path / "u"
    root = home / ".hermes"
    profile = root / "profiles" / "work"
    project = home / "proj"
    plugin_data = profile / "plugin-data" / "ctx_handover"
    for d in (root, profile, project, plugin_data):
        d.mkdir(parents=True, exist_ok=True)
    return Layout(home, root, profile, project, plugin_data)


# --------------------------------------------------------------------------------------------------------
# Source denylist
# --------------------------------------------------------------------------------------------------------


DESIGN_NAMES = [
    ".env",
    ".env.production",
    ".envrc",
    ".netrc",
    "_netrc",
    ".pgpass",
    ".npmrc",
    ".pypirc",
    ".yarnrc",
    ".yarnrc.yml",
    "pip.conf",
    "pip.ini",
    ".git-credentials",
    ".dockercfg",
    ".vault-token",
    ".htpasswd",
    ".boto",
    ".s3cfg",
    "rclone.conf",
    ".terraformrc",
    "credentials.tfrc.json",
    "credentials",
    "credentials.json",
    ".credentials.json",
    "credentials.toml",
    "auth.json",
    "client_secret_123.json",
    "service-account-prod.json",
    "prod.tfstate",
    "prod.tfstate.backup",
    "x.tfvars",
    "kubeconfig",
    "id_rsa",
    "id_dsa",
    "id_ecdsa",
    "id_ed25519",
    "id_ecdsa_sk",
    "id_ed25519_sk",
    "server.pem",
    "tls.key",
    "cert.p12",
    "cert.pfx",
    "store.jks",
    "app.keystore",
    "vault.kdbx",
    "vpn.ovpn",
    "putty.ppk",
    ".bash_history",
    ".zsh_history",
    ".python_history",
    ".psql_history",
    ".mysql_history",
    ".node_repl_history",
]


@pytest.mark.parametrize("name", DESIGN_NAMES)
def test_policy_denylist_names(name, lay):
    """★ Every basename of DESIGN §10.1 incl. the SEC 5.1 additions (spelled out in ``DESIGN_NAMES``) is denied — in
    the workspace, as an extra and in upper case; the content is innocuous, so only the name decides.

    Red proof: mutant = remove ``"*.tfvars"`` from ``policy.SOURCE_DENY_NAMES`` → for ``x.tfvars``:
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='secret')``.
    """
    cfg = lay.cfg()
    for variant in (name, name.upper()):
        path = _write(lay.project / "sub" / variant)
        assert policy.check_source_file(path, cfg, explicit=False, item_root=lay.project) == Verdict(False, "secret")
        assert policy.check_source_file(path, cfg, explicit=True) == Verdict(False, "secret")
        path.unlink()
    assert policy.is_denied_name(f"C:\\Users\\a\\proj\\{name}", case_insensitive=False)


@pytest.mark.parametrize("pattern", policy.SOURCE_DENY_NAMES)
def test_policy_denylist_patterns_cover_design_names(pattern):
    """Every pattern of the tuple is exercised by at least one name of ``DESIGN_NAMES`` (no untested pattern)."""
    import fnmatch

    assert any(fnmatch.fnmatchcase(n.casefold(), pattern) for n in DESIGN_NAMES), pattern


@pytest.mark.parametrize(
    "name", [".env.example", ".env.sample", ".env.template", ".env.dist", "id_rsa.pub", "id_ed25519.pub", "env", "a.md"]
)
def test_policy_denylist_exceptions_allowed(name, lay):
    path = _write(lay.project / name)
    assert policy.check_source_file(path, lay.cfg(), explicit=False, item_root=lay.project) == OK


@pytest.mark.parametrize(
    "rel",
    [
        ".ssh/config",
        ".aws/config",
        ".gnupg/pubring.kbx",
        ".kube/cache/x",
        ".docker/config.json",
        ".azure/x",
        ".config/gh/hosts.yml",
        ".config/gcloud/x",
        ".password-store/x.gpg",
        ".config/op/config",
        ".config/github-copilot/apps.json",
        ".local/share/keyrings/login.keyring",
        "Library/Keychains/login.keychain-db",
        "Library/Cookies/Cookies.binarycookies",
        ".mozilla/firefox/x",
        ".config/google-chrome/Default/Cookies",
        ".config/chromium/Default/x",
        "Library/Application Support/Google/Chrome/Default/x",
        "AppData/Roaming/Microsoft/Credentials/x",
        "AppData/Roaming/Microsoft/Protect/x",
        "AppData/Local/Microsoft/Credentials/x",
        "proj/.ssh/deploy",
    ],
)
def test_policy_denylist_dirs(rel, lay):
    path = _write(lay.home / rel)
    assert policy.check_source_file(path, lay.cfg(), explicit=True) == Verdict(False, "secret")


def test_policy_denylist_dirs_casefold(lay):
    """★ Directory components are compared casefold + NFC on case-insensitive filesystems [SEC 5.2]:
    ``.SSH/id`` and ``Library/keychains`` are denied there; on a case-sensitive filesystem ``.SSH`` is just a
    directory.

    Red proof: mutant = ``_fold`` ignores ``case_insensitive`` (returns the NFC form only) →
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='secret')``.
    """
    ssh = _write(lay.home / ".SSH" / "id")
    chain = _write(lay.home / "Library" / "keychains" / "login.keychain-db")
    nfd = _write(lay.home / "Library" / "Application Support" / "Google" / "Chrome" / "x")
    ci = lay.cfg(case_insensitive=True)
    cs = lay.cfg(case_insensitive=False)
    assert policy.check_source_file(ssh, ci, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(chain, ci, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(nfd, cs, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(ssh, cs, explicit=True) == OK
    assert policy.is_denied_name(".SSH/id", case_insensitive=True)
    assert not policy.is_denied_name(".SSH/id", case_insensitive=False)
    assert policy.is_denied_name("C:\\Users\\a\\AppData\\Roaming\\Microsoft\\Credentials\\x", case_insensitive=True)


def test_policy_symlinked_item_root_into_denied_dir(lay):
    """★ ``~/proj/creds -> ~/.aws`` is denied via realpath, as an item root and for a file reached through it
    [SEC 5.3].

    Red proof: mutant = check only the lexical path (drop ``or is_denied_name(str(real), …)`` in
    ``check_source_file``) → ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False,
    reason='secret')``.
    """
    _write(lay.home / ".aws" / "config", "[default]\nregion = eu-central-1\n")
    link = lay.project / "creds"
    os.symlink(lay.home / ".aws", link)
    cfg = lay.cfg()
    assert policy.check_source_file(link, cfg, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(link / "config", cfg, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(link, cfg, explicit=False) == Verdict(False, "symlink")


def test_policy_hardlink_forces_sniff(lay):
    """A hard link with an innocent name to a key elsewhere is caught by the content sniff (``st_nlink > 1``)."""
    key = _write(lay.home / "keys" / "deploy", OPENSSH_KEY)
    alias = lay.project / "notes.md"
    os.link(key, alias)
    assert os.stat(alias).st_nlink == 2
    assert policy.check_source_file(alias, lay.cfg(), explicit=False, item_root=lay.project) == Verdict(False, "secret")


@pytest.mark.parametrize(
    "content",
    [
        OPENSSH_KEY,
        "header\n-----BEGIN EC PRIVATE KEY-----\nMHcCAQEE\n",
        "-----BEGIN ENCRYPTED PRIVATE KEY-----\n",
        "PuTTY-User-Key-File-3: ssh-ed25519\n",
        "# created\nAGE-SECRET-KEY-1QQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQQ\n",
        "[Interface]\nAddress = 10.0.0.2/32\nPrivateKey = aGVsbG8gd29ybGQgdGhpcyBpcyBub3QgYSBrZXk=\n",
    ],
    ids=["openssh", "pem-ec", "pem-pkcs8", "putty", "age", "wireguard"],
)
def test_policy_sniff_private_key(content, lay):
    """★ ``notes.txt`` holding a private key (OpenSSH, PEM, PuTTY, age, WireGuard ``PrivateKey =``) is denied by
    content; ``include_secret`` lifts exactly that path.

    Red proof: mutant = skip the sniff (``if not lifted and False:`` in ``check_source_file``) →
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='secret')``.
    """
    path = _write(lay.project / "notes.txt", content)
    assert policy.check_source_file(path, lay.cfg(), explicit=False, item_root=lay.project) == Verdict(False, "secret")
    assert policy.sniff_secret(path)
    lifted = lay.cfg(include_secret=[str(path)])
    assert policy.check_source_file(path, lifted, explicit=False, item_root=lay.project) == OK


def test_policy_sniff_only_reads_the_head(lay):
    path = _write(lay.project / "big.txt", "x" * (policy.SNIFF_BYTES + 10) + "\n" + OPENSSH_KEY)
    assert not policy.sniff_secret(path)
    assert policy.sniff_secret(lay.project) is True  # not a regular file: fail-closed


def test_policy_extra_with_token_is_proposed(lay):
    """An extra whose head changes under the core's ``redact_sensitive_text(force=True)`` is a suspected secret:
    allowed only as a proposal [SEC 5.1, DESIGN §8.7]. Uses the real core redactor through ``hostcompat``."""
    require_upstream()
    from ctx_handover.engine import hostcompat

    token = "sk-" + "A1b2C3d4" * 6
    risky = _write(lay.home / "notes" / "deploy.txt", f"export OPENAI_API_KEY={token}\nrun deploy\n")
    clean = _write(lay.home / "notes" / "todo.txt", "buy milk\nship the release\n")
    cfg = lay.cfg()
    assert hostcompat.redact(risky.read_text()) != risky.read_text()
    assert policy.check_extra(risky, cfg, explicit=False, redact=hostcompat.redact) == Verdict(True, "suspected_secret")
    assert policy.check_extra(clean, cfg, explicit=False, redact=hostcompat.redact) == OK
    assert policy.suspected_secret(risky, hostcompat.redact)
    lifted = lay.cfg(include_secret=[str(risky)])
    assert policy.check_extra(risky, lifted, explicit=False, redact=hostcompat.redact) == OK
    binary = lay.home / "notes" / "blob.bin"
    binary.write_bytes(b"\x00\x01" + token.encode())
    assert not policy.suspected_secret(binary, hostcompat.redact)


# --------------------------------------------------------------------------------------------------------
# Never releasable
# --------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    "rel",
    [
        "profiles/other/state.db",
        "profiles/other/memories/MEMORY.md",
        ".env",
        "config.yaml",
        "state.db",
        "skills/x/SKILL.md",
        "profiles/work/auth.json",
        "profiles/work/memories/USER.md",
        "profiles/work/plugin-data/ctx_handover/ledger.sqlite3",
    ],
)
def test_policy_denies_hermes_root_and_other_profiles(rel, lay):
    """★ Everything under the Hermes root — other profiles, the root ``.env``, the active profile itself — is
    denied, as given and explicitly, even with ``include_secret`` [SEC 2.4, R19].

    Red proof: mutant = ``_hermes_bases`` returns only ``cfg.hermes_home`` → ``profiles/other/state.db`` gives
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='hermes')``.
    """
    path = _write(lay.root / rel)
    cfg = lay.cfg(include_secret=[str(path)])
    assert policy.check_source_file(path, cfg, explicit=True) == Verdict(False, "hermes")
    assert policy.is_hermes_protected(path, cfg)
    link = lay.project / "innocent"
    os.symlink(path, link)
    assert policy.check_source_file(link, cfg, explicit=True) == Verdict(False, "hermes")


def test_include_secret_cannot_lift_hermes_credentials(lay, tmp_path):
    """★ ``include_secret`` on Hermes' ``auth.json``, the profile ``.env`` (identity key) and ``/etc/shadow`` has
    no effect, also when ``HERMES_HOME`` lives outside the Hermes root; it does lift an ordinary secret
    [R19, SEC 2.4, 5.4].

    Red proof: mutant = in ``check_source_file`` return ``_OK`` early when ``_lifted(cfg, lexical, real)`` (before
    the system/Hermes checks) → ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False,
    reason='hermes')``.
    """
    custom = tmp_path / "srv" / "hermes-home"
    auth = _write(custom / "auth.json", '{"providers": {}}')
    env = _write(custom / ".env", "CTX_HANDOVER_IDENTITY_KEY=abc\n")
    shadow = Path("/", "etc", "shadow")
    netrc = _write(lay.home / ".netrc", "machine example.org\n")
    cfg = lay.cfg(hermes_home=custom, include_secret=[str(auth), str(env), str(shadow), str(netrc)])
    assert policy.check_source_file(auth, cfg, explicit=True) == Verdict(False, "hermes")
    assert policy.check_source_file(env, cfg, explicit=True) == Verdict(False, "hermes")
    if shadow.exists():
        assert policy.check_source_file(shadow, cfg, explicit=True) == Verdict(False, "secret")
    assert policy.check_source_file(netrc, cfg, explicit=True) == OK


@pytest.mark.parametrize(
    "parts",
    [("etc", "shadow"), ("etc", "gshadow"), ("etc", "sudoers"), ("etc", "sudoers.d", "x"), ("etc", "ssl", "private")],
)
def test_system_paths_never_releasable(parts, lay):
    path = Path("/", *parts)
    cfg = lay.cfg(home=Path("/"), include_secret=[str(path)])
    verdict = policy.check_source_file(path, cfg, explicit=True)
    assert verdict in (Verdict(False, "secret"), Verdict(False, "missing"))
    assert policy._system_denied(path)


def test_hermes_credential_paths_cover_the_core_list():
    """The copy of the private ``PROFILE_CREDENTIAL_PATHS`` is not behind the core (drift guard)."""
    require_upstream()
    from hermes_cli.profiles import PROFILE_CREDENTIAL_PATHS

    assert set(PROFILE_CREDENTIAL_PATHS) <= set(policy.HERMES_CREDENTIAL_PATHS)


def test_session_file_areas_are_the_only_exception(lay):
    cfg = lay.cfg(document_cache=lay.profile / "cache" / "documents")
    spill = _write(lay.profile / "cache" / "spillover" / "out-1.txt")
    hook = _write(lay.profile / "hook_outputs" / "sid" / "h.txt")
    native = _write(lay.profile / "cache" / "documents" / "native-inputs" / ("a" * 64) / "img.png")
    assert not policy.is_hermes_protected(spill, cfg, session_file_kind="spillover")
    assert not policy.is_hermes_protected(hook, cfg, session_file_kind="hook_outputs")
    assert not policy.is_hermes_protected(native, cfg, session_file_kind="native_inputs")
    assert policy.is_hermes_protected(spill, cfg, session_file_kind="hook_outputs")
    assert policy.is_hermes_protected(spill, cfg)
    assert policy.is_hermes_protected(lay.profile / "auth.json", cfg, session_file_kind="spillover")
    with pytest.raises(PolicyDenied):
        policy.is_hermes_protected(spill, cfg, session_file_kind="anything")


# --------------------------------------------------------------------------------------------------------
# Other source rules
# --------------------------------------------------------------------------------------------------------


def test_source_special_files_symlinks_missing_size_and_location(lay, tmp_path):
    cfg = lay.cfg(options=Options(max_file_bytes=10))
    fifo = lay.project / "pipe"
    os.mkfifo(fifo)
    assert policy.check_source_file(fifo, cfg, explicit=True) == Verdict(False, "special")
    link = lay.project / "l"
    os.symlink(lay.project / "missing", link)
    assert policy.check_source_file(link, cfg, explicit=False) == Verdict(False, "symlink")
    assert policy.check_source_file(link, cfg, explicit=True) == Verdict(False, "missing")
    assert policy.check_source_file(lay.project / "nope", cfg, explicit=True) == Verdict(False, "missing")
    big = _write(lay.project / "big.txt", "x" * 11)
    assert policy.check_source_file(big, cfg, explicit=False, item_root=lay.project) == Verdict(False, "size")
    assert policy.check_source_file(big, cfg, explicit=True, item_root=lay.project) == OK
    outside = _write(tmp_path / "elsewhere" / "a.txt")
    assert policy.check_source_file(outside, cfg, explicit=True) == Verdict(False, "outside_home")
    exported = lay.cfg(options=Options(export_roots=(str(tmp_path / "elsewhere"),)))
    assert policy.check_source_file(outside, exported, explicit=True) == OK
    assert policy.check_source_file(outside, cfg, explicit=True, item_root=tmp_path / "elsewhere") == OK
    escaped = lay.project / "esc"
    os.symlink(outside, escaped)
    assert policy.check_source_file(escaped, cfg, explicit=True, item_root=lay.project) == Verdict(False, "symlink")
    assert policy.check_source_file(Path("relative/x"), cfg, explicit=True) == Verdict(False, "special")
    assert policy.check_source_file(lay.project, cfg, explicit=True) == OK


def test_from_options_never_reads_remote_include_secret(lay):
    """R18: ``Options.hints.include_secret`` (which a remote peer can fill) never reaches the policy."""
    netrc = _write(lay.home / ".netrc", "machine x\n")
    options = Options(hints=PlanHints(include_secret=(str(netrc), "~/.netrc")))
    cfg = lay.cfg(options=options)
    assert cfg.include_secret == frozenset()
    assert policy.check_source_file(netrc, cfg, explicit=True) == Verdict(False, "secret")
    local = lay.cfg(options=options, include_secret=["~/.netrc"])
    assert local.include_secret == frozenset({str(netrc)})


def test_policy_config_expands_roots(lay):
    cfg = lay.cfg(options=Options(allow_roots=("~/hermes-handover", "/srv/x"), export_roots=("data",)))
    assert cfg.allow_roots == (lay.home / "hermes-handover", Path("/srv/x"))
    assert cfg.export_roots == (lay.home / "data",)
    assert cfg.platform_rules().windows is False
    assert lay.cfg(target_os="windows").platform_rules().windows is True


# --------------------------------------------------------------------------------------------------------
# Target write roots and denylist
# --------------------------------------------------------------------------------------------------------

TARGET_DENIED = [
    ".ssh/authorized_keys",
    ".bashrc",
    ".config/autostart/x.desktop",
    "Library/LaunchAgents/x.plist",
    ".local/bin/x",
    ".gitconfig",
    ".hidden",
    ".hidden/deeper/file",
    "bin/tool",
    ".config/systemd/user/x.service",
    ".config/fish/config.fish",
    ".zshenv",
    ".npmrc",
    ".config/pip/pip.conf",
    "AppData/Roaming/Microsoft/Windows/Start Menu/Programs/Startup/x.lnk",
    "Library",
    "AppData",
    ".",
]


@pytest.mark.parametrize("rel", TARGET_DENIED)
def test_target_dest_denylist_under_home(rel, lay):
    """★ With ``allow_roots=["~"]`` the target denylist still refuses credential, startup, autostart, bin and
    config locations, every dot entry directly under ``$HOME``, ``$HOME`` itself and ancestors of denied places
    [R19, SEC 2.2] — also for recorded and locally typed destinations.

    Red proof: mutant = delete the dot-entry block (``if rel and rel[0].startswith(".")``) in ``_target_denied`` →
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='target_denied')`` for
    ``.hidden``.
    """
    cfg = lay.cfg(options=Options(allow_roots=("~",)))
    dest = lay.home if rel == "." else lay.home / rel
    for recorded, local in ((False, False), (True, False), (False, True)):
        assert policy.check_target_dest(dest, cfg, recorded=recorded, local_choice=local) == Verdict(
            False, "target_denied"
        )


@pytest.mark.parametrize(
    "where", ["root/skills/x", "root/profiles/other/memories/x", "plugin/blocks/x", "root", "profile/../x"]
)
def test_target_dest_hermes_root_and_plugin_data(where, lay):
    cfg = lay.cfg(options=Options(allow_roots=("~",)))
    base, _, rest = where.partition("/")
    start = {"root": lay.root, "plugin": lay.plugin_data, "profile": lay.profile}[base]
    dest = start / rest if rest else start
    assert policy.check_target_dest(dest, cfg, recorded=True, local_choice=False) == Verdict(False, "target_denied")


def test_target_dest_follows_symlinks_into_denied_places(lay):
    (lay.home / ".ssh").mkdir()
    (lay.home / "hermes-handover").mkdir()
    os.symlink(lay.home / ".ssh", lay.home / "hermes-handover" / "sneaky")
    cfg = lay.cfg()
    dest = lay.home / "hermes-handover" / "sneaky" / "authorized_keys"
    assert policy.check_target_dest(dest, cfg, recorded=False, local_choice=False) == Verdict(False, "target_denied")
    os.symlink(lay.home / "elsewhere", lay.home / "hermes-handover" / "out")
    (lay.home / "elsewhere").mkdir()
    escaped = lay.home / "hermes-handover" / "out" / "x"
    assert policy.check_target_dest(escaped, cfg, recorded=False, local_choice=False) == Verdict(
        False, "not_allowed_root"
    )


def test_target_dest_default_root_is_handover_root(lay):
    """Default ``allow_roots`` is ``~/hermes-handover``, not the home directory [R19]."""
    cfg = lay.cfg()
    assert cfg.allow_roots == (lay.home / "hermes-handover",)
    assert policy.check_target_dest(lay.home / "app", cfg, recorded=False, local_choice=False) == Verdict(
        False, "not_allowed_root"
    )
    assert policy.check_target_dest(lay.home / "hermes-handover" / "app", cfg, recorded=False, local_choice=False) == OK
    assert policy.check_target_dest(lay.home / "app", cfg, recorded=False, local_choice=True) == OK
    assert policy.check_target_dest(Path("rel/app"), cfg, recorded=True, local_choice=True) == Verdict(
        False, "not_allowed_root"
    )


def test_target_dest_recorded_path_allowed_outside_roots(lay, tmp_path):
    """A path of this host's own workspace map may lie outside ``allow_roots`` but never on the denylist."""
    cfg = lay.cfg()
    assert policy.check_target_dest(lay.home / "app", cfg, recorded=True, local_choice=False) == OK
    assert policy.check_target_dest(tmp_path / "srv" / "app", cfg, recorded=True, local_choice=False) == OK
    for rel in (".ssh/x", ".config/x", "bin/x"):
        assert policy.check_target_dest(lay.home / rel, cfg, recorded=True, local_choice=False) == Verdict(
            False, "target_denied"
        )


def test_target_dot_roots_opened_by_allow_roots(lay):
    cfg = lay.cfg(options=Options(allow_roots=("~/.work", "~/.local/handover")))
    assert policy.check_target_dest(lay.home / ".work" / "a", cfg, recorded=False, local_choice=False) == OK
    assert (
        policy.check_target_dest(lay.home / ".local" / "handover" / "a", cfg, recorded=False, local_choice=False) == OK
    )
    for rel in (".workx/a", ".local/bin/a", ".local", ".local/share/a"):
        verdict = policy.check_target_dest(lay.home / rel, cfg, recorded=False, local_choice=True)
        assert verdict == Verdict(False, "target_denied"), rel


def test_target_case_insensitive_denylist(lay):
    cfg = lay.cfg(options=Options(allow_roots=("~",)), case_insensitive=True)
    for rel in (".SSH/authorized_keys", "library/launchagents/x", "BIN/x"):
        assert policy.check_target_dest(lay.home / rel, cfg, recorded=True, local_choice=False) == Verdict(
            False, "target_denied"
        )


def test_target_hermes_file_placement(lay):
    cfg = replace(lay.cfg(), document_cache=lay.profile / "cache" / "documents")
    ok = [
        (lay.profile / "cache" / "spillover" / "a.txt", "spillover"),
        (lay.profile / "hook_outputs" / "newsid" / "h.txt", "hook_outputs"),
        (lay.profile / "cache" / "documents" / "native-inputs" / ("b" * 64) / "i.png", "native_inputs"),
    ]
    for dest, kind in ok:
        assert policy.check_target_dest(dest, cfg, recorded=False, local_choice=False, hermes_file_kind=kind) == OK
    bad = [
        (lay.profile / "cache" / "spillover" / "a.txt", "hook_outputs"),
        (lay.profile / "cache" / "spillover", "spillover"),
        (lay.profile / ".env", "spillover"),
        (lay.profile / "cache" / "spillover" / ".." / ".." / "auth.json", "spillover"),
        (lay.home / "x", "spillover"),
        (lay.profile / "cache" / "spillover" / "a.txt", "bogus"),
    ]
    for dest, kind in bad:
        verdict = policy.check_target_dest(dest, cfg, recorded=True, local_choice=True, hermes_file_kind=kind)
        assert verdict == Verdict(False, "target_denied"), (dest, kind)


# --------------------------------------------------------------------------------------------------------
# Tree entries and pathspecs
# --------------------------------------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("entry", "allowed"),
    [
        ({"type": "file", "mode": "0644", "size": 3, "path": "a"}, True),
        ({"type": "file", "mode": "100755", "size": 3, "path": "a"}, True),
        ({"type": "file", "mode": 0o644, "size": 3, "path": "a"}, True),
        ({"type": "dir", "mode": "0755", "path": "d"}, True),
        ({"type": "symlink", "path": "d/l", "target": "../a"}, True),
        ({"type": "symlink", "mode": "120777", "path": "d/e/l", "target": "../../a"}, True),
        ({"type": "fifo", "mode": "0644", "path": "p"}, False),
        ({"type": "socket", "mode": "0644", "path": "s"}, False),
        ({"type": "chardev", "mode": "0644", "path": "c"}, False),
        ({"type": "file", "mode": "4755", "path": "a"}, False),
        ({"type": "file", "mode": "2755", "path": "a"}, False),
        ({"type": "dir", "mode": "1777", "path": "d"}, False),
        ({"type": "file", "mode": "0600", "path": "a"}, False),
        ({"type": "file", "mode": "0777", "path": "a"}, False),
        ({"type": "file", "mode": "040755", "path": "a"}, False),
        ({"type": "file", "mode": "rw-r--r--", "path": "a"}, False),
        ({"type": "file", "mode": True, "path": "a"}, False),
        ({"type": "file", "path": "a"}, False),
        ({"type": "file", "mode": "0644", "size": -1, "path": "a"}, False),
        ({"type": "symlink", "path": "l", "target": "/etc/passwd"}, False),
        ({"type": "symlink", "path": "d/l", "target": "../../x"}, False),
        ({"type": "symlink", "path": "l", "target": "C:\\x"}, False),
        ({"type": "symlink", "path": "l", "target": ".git/config"}, False),
        ({"type": "symlink", "path": "l", "target": ""}, False),
        ({"type": "symlink", "path": "l"}, False),
        ("not a dict", False),
    ],
)
def test_check_tree_entry_types_and_modes(entry, allowed):
    """★ Only file/dir/symlink with 0644/0755 (no setuid, setgid, sticky) and relative in-tree link targets
    [SEC 2.3, BUILD-ORDER §6 #7].

    Red proof: mutant = drop ``perm & 0o7000 or`` and add ``0o4755, 0o2755, 0o1777`` to ``_ALLOWED_PERMS``
    (i.e. accept special bits) → ``assert True is False`` for mode ``4755``.
    """
    verdict = policy.check_tree_entry(entry)
    assert verdict.allowed is allowed
    if not allowed:
        assert verdict.reason in {"special", "symlink"}


def test_exclude_pathspecs():
    assert policy.exclude_pathspecs(["b", "a", "b", "dir with space/x"]) == (
        ":(exclude,literal)a",
        ":(exclude,literal)b",
        ":(exclude,literal)dir with space/x",
    )
    for bad in ("a\nb", "a\x00", "", None):
        with pytest.raises(PolicyDenied):
            policy.exclude_pathspecs([bad])


def test_verdict_reasons_are_declared():
    assert {"secret", "hermes", "size", "outside_home", "special", "symlink", "target_denied"} <= policy.REASONS
    assert {"not_allowed_root", "suspected_secret", "missing", "unreadable"} <= policy.REASONS


# --------------------------------------------------------------------------------------------------------
# Per-scan cache and precompiled forms (L2: faster source checks, same verdicts)
# --------------------------------------------------------------------------------------------------------


def _scan_layout(lay: Layout, tmp_path: Path) -> list[Path]:
    """Every kind of source path the policy distinguishes, built on disk (files, links, special files)."""
    key = _write(lay.home / "keys" / "deploy", OPENSSH_KEY)
    out = [
        _write(lay.project / "a.md"),
        _write(lay.project / "sub" / "deep" / "b.txt"),
        _write(lay.project / ".env", "A=1\n"),
        _write(lay.project / "sub" / "id_rsa", "x\n"),
        _write(lay.project / "notes.txt", OPENSSH_KEY),
        _write(lay.project / "big.txt", "x" * 100),
        _write(lay.project / ".SSH" / "id"),
        _write(lay.project / "Library" / "keychains" / "x"),
        _write(lay.project / "cafe\u0301" / "c.txt"),
        _write(lay.home / ".ssh" / "config"),
        _write(lay.home / ".aws" / "config", "[default]\n"),
        _write(lay.root / "config.yaml"),
        _write(lay.root / "profiles" / "other" / "auth.json", "{}"),
        _write(lay.profile / "auth.json", "{}"),
        _write(lay.profile / "memories" / "MEMORY.md"),
        _write(lay.plugin_data / "ledger.sqlite3"),
        _write(tmp_path / "elsewhere" / "a.txt"),
        lay.project,
        lay.project / "sub",
        lay.project / "missing",
        lay.home / ".aws",
    ]
    hard = lay.project / "hard.md"
    os.link(key, hard)
    fifo = lay.project / "pipe"
    os.mkfifo(fifo)
    links = {
        "link-aws": lay.home / ".aws",
        "link-file": lay.project / "a.md",
        "dangling": lay.project / "nope",
        "esc": tmp_path / "elsewhere" / "a.txt",
        "hermes-link": lay.root / "config.yaml",
        "profile-link": lay.profile,
    }
    for name, target in links.items():
        os.symlink(target, lay.project / name)
        out.append(lay.project / name)
    out += [hard, fifo, lay.project / "link-aws" / "config", lay.project / "profile-link" / "auth.json"]
    out += [lay.project / "sub" / ".." / ".env", Path("relative/x"), Path("//srv/share/x")]
    return out


def test_policy_scan_cache_gives_the_same_verdicts(lay, tmp_path):
    """One ``SourceCheckCache`` shared by all checks of a scan (in either order) gives exactly the verdicts of
    uncached calls — for every source rule: system/Hermes paths, credential stores through links, symlinks,
    special files, names and directories (case-sensitive and case-insensitive), locations, size, the sniff
    (hard link included) and ``include_secret``; also for ``check_extra`` and ``is_hermes_protected``."""
    paths = _scan_layout(lay, tmp_path)
    configs = [
        lay.cfg(options=Options(max_file_bytes=64)),
        lay.cfg(case_insensitive=True),
        lay.cfg(include_secret=[str(lay.project / "notes.txt"), str(lay.project / ".env")]),
        lay.cfg(options=Options(export_roots=(str(tmp_path / "elsewhere"),))),
    ]
    cases = [(p, e, r) for p in paths for e in (False, True) for r in (None, lay.project)]
    reasons: set[str | None] = set()

    def redact(text: str) -> str:
        return text.replace("region", "***")

    for cfg in configs:
        expected = [policy.check_source_file(p, cfg, explicit=e, item_root=r) for p, e, r in cases]
        reasons |= {v.reason for v in expected}
        forward = policy.SourceCheckCache()
        got = [policy.check_source_file(p, cfg, explicit=e, item_root=r, cache=forward) for p, e, r in cases]
        assert got == expected
        backward = policy.SourceCheckCache()
        rev = [policy.check_source_file(p, cfg, explicit=e, item_root=r, cache=backward) for p, e, r in cases[::-1]]
        assert rev[::-1] == expected
        extras = [policy.check_extra(p, cfg, explicit=e, redact=redact) for p, e, _ in cases[::2]]
        assert extras == [
            policy.check_extra(p, cfg, explicit=e, redact=redact, cache=forward) for p, e, _ in cases[::2]
        ]
        absolute = [p for p in paths if p.is_absolute() and not str(p).startswith("//")]
        assert [policy.is_hermes_protected(p, cfg) for p in absolute] == [
            policy.is_hermes_protected(p, cfg, cache=forward) for p in absolute
        ]
    assert reasons >= {None, "secret", "hermes", "symlink", "special", "missing", "outside_home", "size"}


def test_policy_cache_does_not_outlive_its_scan(lay):
    """★ Calls without a cache, and a fresh cache per scan, see a parent directory that was swapped for a link
    into ``~/.aws`` after an earlier scan: the file is denied by its realpath (no cache survives a scan).

    Red proof: mutant = calls without ``cache`` share one process-wide ``SourceCheckCache`` (``_memo`` falls
    back to a module-level instance) → the later call still uses the old realpath of ``proj/sub``:
    ``assert Verdict(allowed=True, reason=None) == Verdict(allowed=False, reason='secret')``.
    """
    _write(lay.home / ".aws" / "notes.txt", "region = eu-central-1\n")
    sub = lay.project / "sub"
    note = _write(sub / "notes.txt", "region = eu-central-1\n")
    cfg = lay.cfg()
    earlier_scan = policy.SourceCheckCache()
    assert policy.check_source_file(note, cfg, explicit=False, item_root=lay.project) == OK
    assert policy.check_source_file(note, cfg, explicit=False, item_root=lay.project, cache=earlier_scan) == OK
    note.unlink()
    sub.rmdir()
    os.symlink(lay.home / ".aws", sub)
    secret = Verdict(False, "secret")
    assert policy.check_source_file(note, cfg, explicit=False, item_root=lay.project) == secret
    next_scan = policy.SourceCheckCache()
    assert policy.check_source_file(note, cfg, explicit=False, item_root=lay.project, cache=next_scan) == secret


def test_policy_cache_rebinds_to_another_config(lay, tmp_path):
    """★ A cache used with a second ``PolicyConfig`` judges by that configuration's bases: a file in the second
    configuration's ``HERMES_HOME`` is ``hermes`` there, although the same cache allowed it under the first.

    Red proof: mutant = ``SourceCheckCache._bind`` keeps the first configuration → ``assert Verdict(allowed=True,
    reason=None) == Verdict(allowed=False, reason='hermes')``.
    """
    other = tmp_path / "srv" / "hermes-other"
    note = _write(other / "notes.txt")
    first = lay.cfg()
    second = lay.cfg(hermes_home=other)
    cache = policy.SourceCheckCache()
    assert policy.check_source_file(note, first, explicit=True, item_root=other, cache=cache) == OK
    assert policy.check_source_file(note, second, explicit=True, item_root=other, cache=cache) == Verdict(
        False, "hermes"
    )
    assert policy.check_source_file(note, first, explicit=True, item_root=other, cache=cache) == OK


def test_policy_device_prefix_before_fs_with_warm_cache(lay, monkeypatch):
    """A warm cache does not move any filesystem call before the device-prefix rejection [SEC 2.2]."""
    cfg = lay.cfg()
    cache = policy.SourceCheckCache()
    assert policy.check_source_file(_write(lay.project / "a.md"), cfg, explicit=False, cache=cache) == OK

    def no_fs(*a, **kw):
        raise AssertionError("filesystem touched")

    with monkeypatch.context() as m:
        for name in ("lstat", "stat", "open"):
            m.setattr(os, name, no_fs)
        m.setattr(os.path, "realpath", no_fs)
        verdicts = [policy.check_source_file(Path(p), cfg, explicit=True, cache=cache) for p in ("//srv/x", "\\??\\C:")]
    assert verdicts == [Verdict(False, "special")] * 2


def test_policy_sniff_literal_precheck_is_exact():
    """The substring precheck in front of the sniff regex never changes its answer: every alternative contains
    its literal, and on generated heads ``_has_key_marker`` equals ``bool(_SNIFF_RE.search(head))``.

    Red proof: mutant = the precheck omits the last literal (``_SNIFF_MARKERS[:-1]``) → ``AssertionError`` on a
    head with a WireGuard ``PrivateKey =`` line. Heads: every sequence of up to three pieces.
    """
    import itertools

    for literal, pattern in policy._SNIFF_MARKERS:
        assert literal in pattern
    pieces = [
        b"-----BEGIN ",
        b"OPENSSH ",
        b"PRIVATE KEY-----",
        b"PuTTY-User-Key-File-",
        b"AGE-SECRET-KEY-1",
        b"PrivateKey",
        b" = ",
        b"=",
        b"\n",
        b"\t",
        b" ",
        b"x",
        b"-----",
        b"Private",
    ]
    heads = [b"", b"PrivateKey=", b"\n  PrivateKey\t= x", b"x PrivateKey = y"]
    heads += [b"".join(combo) for n in (1, 2, 3) for combo in itertools.product(pieces, repeat=n)]
    for head in heads:
        assert policy._has_key_marker(head) == bool(policy._SNIFF_RE.search(head)), head


def test_policy_precompiled_denylists_match_the_linear_rules():
    """The precompiled forms (name regex, directory index, system-path prefix) decide exactly like the plain
    rules over the constant lists: ``fnmatchcase`` per pattern, a sliding window per directory sequence."""
    import fnmatch
    import itertools
    import unicodedata

    def fold(text: str, ci: bool) -> str:
        text = unicodedata.normalize("NFC", text)
        return unicodedata.normalize("NFC", text.casefold()) if ci else text

    def linear(path: str, ci: bool) -> bool:
        comps = [c for c in path.replace("\\", "/").split("/") if c not in {"", "."}]
        if comps and len(comps[0]) == 2 and comps[0][1] == ":" and path[2:3] in {"/", "\\"}:
            comps = comps[1:]
        if not comps:
            return False
        name = fold(comps[-1], True)
        if name not in policy.SOURCE_DENY_NAME_EXCEPTIONS and any(
            fnmatch.fnmatchcase(name, p) for p in policy.SOURCE_DENY_NAMES
        ):
            return True
        folded = tuple(fold(c, ci) for c in comps)
        for spec in policy.SOURCE_DENY_DIRS:
            seq = tuple(fold(c, ci) for c in spec.split("/"))
            if any(folded[i : i + len(seq)] == seq for i in range(len(folded) - len(seq) + 1)):
                return True
        return False

    def linear_system(path: Path) -> bool:
        parts = tuple(fold(p, True) for p in path.parts[1:])
        return any(
            len(parts) >= len(q) and all(fnmatch.fnmatchcase(a, b) for a, b in zip(parts, q, strict=False))
            for q in policy.SYSTEM_DENY
        )

    names = ["a.md", ".env", ".env.example", "X.TFVARS", "id_rsa.pub", "Credentials", "café", "cafe\u0301"]
    dirs = [*policy.SOURCE_DENY_DIRS, ".SSH", "library/keychains", "proj", ".config", "gh", "Library"]
    paths = []
    for a, b in itertools.product(dirs, repeat=2):
        for n in names[:4]:
            paths += [f"/home/u/{a}/{b}/{n}", f"{a}/{n}", f"C:\\Users\\u\\{a}\\{n}".replace("/", "\\")]
    paths += [f"/srv/{d}" for d in dirs] + [f"x/{n}" for n in names] + ["", "/", "C:/", "./.ssh/x"]
    for path, ci in itertools.product(paths, (False, True)):
        assert policy.is_denied_name(path, case_insensitive=ci) == linear(path, ci), (path, ci)
    systems = ["/etc/shadow", "/ETC/Sudoers.d/x", "/etc/ssl/private/k", "/etc/ssl/certs/x", "/private/etc/shadow"]
    systems += ["/etc", "/", "/home/etc/shadow", "/private/etc/gshadow", "/etc/gshadow-", "/etc/sudoers"]
    for s in systems:
        assert policy._system_denied(Path(s)) == linear_system(Path(s)), s


def test_policy_path_keys_match_under_any():
    """``_under`` on precomputed keys equals ``paths.under_any`` (NFC, casefold, ``..``, relative paths)."""
    import itertools

    from ctx_handover.engine.paths import under_any

    paths = ["/a/b", "/a/b/c", "/a/bc", "/A/B/c", "/a/b/../c", "rel/a", "/", "/a/cafe\u0301/x", "/a/café/x", "/a"]
    roots = ["/a/b", "/a", "/", "/A", "/a/café", "rel", "/a/b/..", "/a/b/"]
    for p, r, ci in itertools.product(paths, roots, (False, True)):
        fast = policy._under(policy._cand(p, ci).key, policy._root_key(r, ci))
        assert fast == under_any(Path(p), [Path(r)], case_insensitive=ci), (p, r, ci)
