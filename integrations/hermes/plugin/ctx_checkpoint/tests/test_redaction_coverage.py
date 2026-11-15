"""Plugin 0.11.0: what the checkpoint redaction removes and what it keeps (#288).

Two corpora, both measured over the whole chain a checkpoint part goes through: Hermes'
pre-scrub before the provider (``MemoryManager`` → ``redact_for_egress``), then the plugin's
``render_direct_evidence`` (bound keys, the host redactor in its strict mode, the plugin's own
rules). Every secret value below is artificial (``EXAMPLE`` markers, the documented test card
and IBAN, the classic ``hunter2`` / ``correct horse battery staple``).

* ``SECRET_FORMS``: 20 forms that must leave no fragment behind (0.10.0 let 18 of them through
  unchanged and one partly).
* ``KNOWN_LIMITS``: forms no rule recognizes; pinned so that a change which starts to catch
  them also updates the limits the READMEs name.
* ``BENIGN_LINES``: code and prose without a credential. The plugin's own rules must not
  change any of them; only the host redactor may (``HOST_PRICE`` names the lines it changes).

Like ``test_settings_and_status.py`` these run against the real Hermes host on ``PYTHONPATH``
with an isolated ``HERMES_HOME``; config.yaml and .env are written as files, the ctx server is a
recording dispatch. No test opens a network connection.
"""

from __future__ import annotations

import importlib
import importlib.util
import json
import logging
import os
import sys
import time
from pathlib import Path

import pytest
from agent.redact import redact_for_egress, redact_sensitive_text

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_redaction", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider
cp = importlib.import_module(f"{_SPEC.name}.memory.checkpoint")

# A minted ctx key is 32 random bytes as hex (go/internal/store/api_keys.go): 64 characters.
CTX_KEY = "e1a2" + "0123456789abcdef" * 3 + "fedcba987654"
assert len(CTX_KEY) == 64

# name -> (text, fragments that must not survive the chain)
SECRET_FORMS = {
    "s3-presigned": (
        "Lade es von https://bucket.s3.eu-central-1.amazonaws.com/backup.tar?X-Amz-Algorithm=AWS4-HMAC-SHA256"
        "&X-Amz-Expires=604800&X-Amz-Signature=" + "0123456789abcdef" * 4,
        ["0123456789abcdef" * 4],
    ),
    "azure-sas": (
        "https://acct.blob.core.windows.net/c/file.zip?sv=2022-11-02&se=2027-01-01&sp=r"
        "&sig=RVhBTVBMRS1zaWduYXR1cmUtZXhhbXBsZQ%3D%3D",
        ["RVhBTVBMRS1zaWduYXR1cmUtZXhhbXBsZQ"],
    ),
    "gcs-signed": (
        "https://storage.googleapis.com/b/o?X-Goog-Expires=3600&X-Goog-Signature=" + "abcdef0123456789" * 2,
        ["abcdef0123456789" * 2],
    ),
    "discord-webhook": (
        "poste nach https://discord.com/api/webhooks/123456789012345678/EXAMPLE-webhook_token-0123456789abcdefABCDEF",
        ["EXAMPLE-webhook_token-0123456789abcdefABCDEF"],
    ),
    "slack-webhook": (
        "https://hooks.slack.com/services/T00000000/B00000000/EXAMPLEexampleEXAMPLE0123",
        ["EXAMPLEexampleEXAMPLE0123"],
    ),
    "oauth-code": (
        "https://app.example.com/callback?code=4/0EXAMPLE-oauth-code-0123456789&state=xyz",
        ["0EXAMPLE-oauth-code-0123456789"],
    ),
    "magic-link": ("https://app.example.com/login?token=mlk_EXAMPLE0123456789", ["EXAMPLE0123456789"]),
    "prose-de": ("mein Passwort ist hunter2", ["hunter2"]),
    "prose-en": ("the password is hunter2", ["hunter2"]),
    "prose-ssh": ("ssh root@10.0.0.5 mit Passwort Sommer2026!", ["Sommer2026"]),
    "kennwort-kv": ("Kennwort: Sommer2026!", ["Sommer2026"]),
    "passwort-kv": ("Passwort=Sommer2026!", ["Sommer2026"]),
    "passphrase-kv": ("passphrase: correct horse battery staple", ["correct horse", "battery staple"]),
    "password-spaces": ("password: correct horse battery staple", ["horse battery", "battery staple"]),
    "password-quoted": ('password: "correct horse battery staple"', ["correct horse", "battery staple"]),
    "base64-creds": ("creds: dXNlcjpleGFtcGxlLXBhc3N3b3Jk", ["dXNlcjpleGFtcGxlLXBhc3N3b3Jk"]),
    "cred-env": ("MY_SERVICE_CRED=EXAMPLEcred0123456789", ["EXAMPLEcred0123456789"]),
    "pin": ("PIN 4711", ["4711"]),
    "recovery-codes": ("Recovery-Codes: 1a2b-3c4d 5e6f-7g8h 9i0j-1k2l", ["1a2b-3c4d", "5e6f-7g8h", "9i0j-1k2l"]),
    "seed-phrase": (
        "seed phrase: abandon ability able about above absent absorb abstract absurd abuse access accident",
        ["abandon ability", "absorb abstract", "access accident"],
    ),
}
# Forms assistants write that the audit did not list: labels in markdown emphasis, a colon
# after the verb, PIN-Code, the verb form after a token name.
FURTHER_FORMS = {
    "md-bold-colon-inside": ("**Password:** Sommer2026!", ["Sommer2026"]),
    "md-bold-colon-outside": ("**Passwort**: Sommer2026!", ["Sommer2026"]),
    "md-list-api-key": ("- **API key**: EXAMPLEkey0123456789", ["EXAMPLEkey0123456789"]),
    "verb-colon": ("Passwort lautet: Sommer2026!", ["Sommer2026"]),
    "pin-code": ("PIN-Code 4711", ["4711"]),
    "token-verb": ("the token is EXAMPLE0123456789", ["EXAMPLE0123456789"]),
    "one-time-code": ("token: 839201", ["839201"]),
    "go-assignment": ('password := "Sommer2026!"', ["Sommer2026"]),
}
# Values of letters only (0.10.0 masked every value behind a credential name; Hermes masks such a
# run only at the start of a line, or from 20 characters or two letter cases on). Artificial,
# random-looking, no word.
LETTER_VALUES = {
    "lower16": "xqzvbnmkjhgfdwrt",
    "upper12": "QZXVBNMKJHGF",
    "mixed24": "QzXvBnMkJhGfDwRtPlKsMnBv",
    "one-letter-humps": "eumlJasBAbygiNTvQusfAiHK",
    "dashes": "bxqz-kvwm-tzpr-hjnd",
    "mixed-separators": "AbCd_EfGh.IjKl/MnOp",
    "dotted": "Qzxv.Bnmk.Jhgf",
    "base64url": "xQ-zV_bN-mK_jH-gF-dW_rT",
}
LETTER_FORMS = {
    "token:": "token: {v}",
    "secret=": "secret={v}",
    "api_key:": "api_key: {v}",
    "client_secret=": "client_secret={v}",
    "password:": "password: {v}",
    "Passwort:": "Passwort: {v}",
    "Authorization:": "Authorization: {v}",
    "json": '{{"api_key": "{v}"}}',
    "SECRET_KEY=": "SECRET_KEY={v}",
    "MY_SERVICE_TOKEN=": "MY_SERVICE_TOKEN={v}",
    "mid token:": "set token: {v} in the file",
    "mid secret=": "run it with secret={v} once",
    "mid api_key:": "the config says api_key: {v} today",
    "mid password:": "login with password: {v} please",
    "mid Authorization:": "send Authorization: {v} along",
    "mid json": 'payload {{"token": "{v}"}} sent',
}
# Unnamed numbers that only a checksum could tell apart from any other number.
KNOWN_LIMITS = {
    "card-number": ("Karte 4111 1111 1111 1111 exp 12/29", "4111 1111 1111 1111"),
    "iban": ("IBAN DE89 3704 0044 0532 0130 00", "DE89 3704 0044 0532 0130 00"),
}
# The 15 lines of the audit (0.10.0 changed all 15), then lines next to every new rule.
ISSUE_BENIGN = [
    "api_key: Optional[str] = None",
    "token = self.token",
    "password: str,",
    "the token: it expires after an hour",
    "def login(user: str, password: str) -> bool:",
    "secret = os.environ['X']",
    "max_tokens: 4096, token: int = 0",
    "if token == expected:",
    "Authorization: see the docs",
    "Das Passwort: bitte nie im Chat nennen. password: niemals teilen",
    "token: str | None = None  # the bearer",
    "kwargs = dict(api_key=api_key, token=token)",
    "| token: | description |",
    "refresh_token=refresh_token,",
    "const secret: string = cfg.secret;",
]
NEIGHBOUR_BENIGN = [
    "the password is required",
    "das Passwort ist falsch",
    "Passwort vergessen? Dann hilft der Reset-Link.",
    "Open the Password Manager and search for it",
    "pin 3 of the GPIO header is ground",
    "PIN-Eingabe am Terminal",
    "the token count is 4096",
    "if password == confirm:",
    "passphrase = self.passphrase",
    "passphrase: str",
    "seed: 42",
    "Recovery is done; the codes were rotated.",
    "the authorization flow is documented",
    "Authorization header is required",
    "Use Bearer authentication for the API",
    "token: u64,",
    "secret: ${VAULT_SECRET}",
    "password: <your password>",
    "https://example.org/search?q=ctx&page=2",
    "https://discord.com/channels/123456789012345678/876543210987654321",
    "https://hooks.slack.com/services",
    "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855  empty.txt",
    "commit 64fc9d9177dcf232f6c6d0c2e0cb2ea34cbaa03c",
    "X-Context-Key: your-api-key-here",
    "**Password** Manager settings",
    "the token is valid for an hour",
    "the API key is required",
    "**Note:** the password is required",
    "the password is 12 characters long",
    "Passwort ist 8 Zeichen lang",
    "the password was 2 weeks old",
    "PIN 3 of the CPU socket",
    "token = self.token2",
    "token: Int64 = 0",
    "token: Uint8Array",
    "token = base64.b64encode(raw)",
    "secret = sha256(data).hexdigest()",
    "token := get_token()",
    "token: Bearer",
    "secret = config.sharedSecretName",
    "password: required",
    "api_key: string",
    "set token: accessTokenProviderFactory in the file",
    "use secret: environmentVariableName here",
    "login with password: required please",
    "the config says api_key: string today",
    "pass token: TokenizerConfig on",
    "see secret: kubernetes.io/service-account-token now",
    "with api_key: OpenAIApiKey set",
    "set token: refresh_token_handler there",
    "the token is expired",
]
BENIGN_LINES = ISSUE_BENIGN + NEIGHBOUR_BENIGN


def _host_prescrub(text: str) -> str:
    """Hermes' own scrub before ``on_pre_compress`` (agent/memory_manager.py ``_scrub``)."""
    return redact_for_egress(text)


def _part_text(text: str, secret=()) -> str:
    """The transcript text of a checkpoint part for one user message."""
    rendered = cp.render_direct_evidence([{"role": "user", "content": _host_prescrub(text)}], secret)
    return rendered.split("\n\n", 1)[1].strip() if rendered else ""


def _host_strict(text: str) -> str:
    return redact_sensitive_text(redact_for_egress(text), force=True, redact_url_credentials=True)


# -- secret forms ---------------------------------------------------------------------------


@pytest.mark.parametrize("name", sorted(SECRET_FORMS) + sorted(FURTHER_FORMS))
def test_secret_form_leaves_no_fragment_in_the_part(name):
    text, fragments = {**SECRET_FORMS, **FURTHER_FORMS}[name]
    out = _part_text(text)
    assert out, "the line must still be archived"
    for fragment in fragments:
        assert fragment not in out, f"{name}: {fragment!r} survived in {out!r}"


def test_secret_form_count():
    leaked = [name for name, (text, frags) in SECRET_FORMS.items() if any(f in _part_text(text) for f in frags)]
    assert leaked == [], f"{len(leaked)} of {len(SECRET_FORMS)} secret forms leave a fragment: {leaked}"


@pytest.mark.parametrize("form", sorted(LETTER_FORMS))
@pytest.mark.parametrize("value_name", sorted(LETTER_VALUES))
def test_a_letters_only_value_behind_a_name_is_masked(form, value_name):
    """0.10.0 masked these; the plugin's shape rule keeps doing so where Hermes does not."""
    value = LETTER_VALUES[value_name]
    out = _part_text(LETTER_FORMS[form].format(v=value))
    pieces = {value[i : i + 6] for i in range(len(value) - 5)}
    assert not [piece for piece in pieces if piece in out], out


def test_letter_runs_and_identifiers_are_told_apart_at_the_measured_rates():
    """Both directions of the shape rule on samples: seeded random letter runs (12 to 64 letters,
    three letter cases) behind a mid-line ``token:``, and the long identifiers of the stdlib."""
    import random
    import re
    import string

    rng = random.Random(288)  # noqa: S311 - a seeded test sample, not a secret
    runs = [
        "".join(rng.choice(alphabet) for _ in range(length))
        for length in (12, 16, 20, 24, 32, 40, 64)
        for alphabet in (string.ascii_lowercase, string.ascii_uppercase, string.ascii_letters)
        for _ in range(40)
    ]
    kept = [run for run in runs if run[:6] in _part_text(f"set token: {run} in the file")]
    assert len(kept) / len(runs) <= 0.15, f"{len(kept)} of {len(runs)} random runs kept"
    names = sorted(
        {
            name
            for module in (os, re, json, logging, string)
            for name in dir(module)
            if re.fullmatch(r"[A-Za-z_]{12,}", name)
        }
    )
    masked = [name for name in names if cp._opaque_letters(name)]
    assert len(names) >= 40 and len(masked) / len(names) <= 0.05, masked


@pytest.mark.parametrize("name", sorted(KNOWN_LIMITS))
def test_known_limit_still_passes_and_is_named_in_the_readmes(name):
    text, value = KNOWN_LIMITS[name]
    assert value in _part_text(text), f"{name} is caught now: drop it from the limits the READMEs name"
    readme = (PLUGIN_DIR / "README.md").read_text(encoding="utf-8")
    engine = (PLUGIN_DIR / "memory" / "README.md").read_text(encoding="utf-8")
    for doc in (readme, engine):
        assert "card numbers" in doc and "IBAN" in doc


# -- benign lines ---------------------------------------------------------------------------


@pytest.mark.parametrize("line", BENIGN_LINES)
def test_plugin_rules_keep_a_benign_line(line):
    """The plugin's own rules add no change to what the host redactor left."""
    host = _host_strict(line)
    assert cp._redact_checkpoint_secrets(host) == host
    assert _part_text(line) == host.strip()


def test_benign_line_count():
    changed = [line for line in BENIGN_LINES if cp._redact_checkpoint_secrets(_host_strict(line)) != _host_strict(line)]
    assert changed == [], f"plugin rules changed {len(changed)} of {len(BENIGN_LINES)} benign lines: {changed}"


def test_host_mask_remnants_are_removed():
    """The host keeps head and tail of a token (``8f3a9c...9c2e``) or a password's tail after a
    character it stops at (``***&3``); the plugin's rules remove those remnants."""
    for remnant in [
        "token: 8f3a9c...9c2e",
        "password=***&3",
        "curl -H 'Authorization: Bearer abcDEF...iJKL'",
        "send it with Bearer abcDEF...iJKL",
    ]:
        out = cp._redact_checkpoint_secrets(remnant)
        assert "8f3a9c" not in out and "&3" not in out and "abcDEF" not in out, out
        assert "[REDACTED]" in out
    for text, value in [
        ("token: 8f3a9c1e7b2d4f6a8c0e1b3d5f7a9c2e", "8f3a9c1e7b2d4f6a8c0e1b3d5f7a9c2e"),
        ("password=Tr0ub4dor&3", "Tr0ub4dor&3"),
        ("curl -H 'Authorization: Bearer abcDEF123456ghiJKL'", "abcDEF123456ghiJKL"),
    ]:
        out = _part_text(text)
        assert value[:6] not in out and value[-4:] not in out, out


# -- the ctx key on the MCP transport ------------------------------------------------------


@pytest.fixture
def hermes_home(tmp_path, monkeypatch):
    home = tmp_path / "hermes-home"
    home.mkdir()
    monkeypatch.setenv("HERMES_HOME", str(home))
    for name in ("CTX_API_KEY", "MCP_CTX_API_KEY", "CTX_KEY"):
        monkeypatch.delenv(name, raising=False)
    return home


def _write(home, config, env=""):
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")
    if env:
        (home / ".env").write_text(env, encoding="utf-8")


def _recording_dispatch():
    calls = []

    def dispatch(name, args, **kwargs):
        calls.append((name, json.loads(json.dumps(args))))
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"id": f"019f6000-0000-7000-8000-{len(calls):012d}"})

    return dispatch, calls


KEY_TRANSCRIPT = [
    {"role": "user", "content": f"my ctx key is {CTX_KEY}"},
    {"role": "assistant", "content": f"curl -H 'X-Context-Key: {CTX_KEY}' https://ctx.example.test/api/search"},
    {"role": "user", "content": json.dumps({"headers": {"X-Context-Key": CTX_KEY}})},
]


def _checkpoint(home):
    from agent.memory_manager import MemoryManager

    dispatch, calls = _recording_dispatch()
    provider = Provider(dispatch=dispatch)
    provider.initialize("root-288", platform="cli", hermes_home=str(home))
    manager = MemoryManager()
    manager.add_provider(provider)
    manager.on_pre_compress(list(KEY_TRANSCRIPT), evidence_messages=list(KEY_TRANSCRIPT), require_checkpoint=True)
    stores = [args for name, args in calls if name.endswith("__store")]
    assert stores, "a checkpoint chain was written"
    return provider, stores


@pytest.mark.parametrize(
    ("entry", "env"),
    [
        pytest.param(
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${CTX_API_KEY}"}},
            f"CTX_API_KEY={CTX_KEY}\n",
            id="setup-entry",
        ),
        pytest.param(
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${MCP_CTX_API_KEY}"}},
            f"MCP_CTX_API_KEY={CTX_KEY}\n",
            id="own-variable",
        ),
        pytest.param(
            {"url": "https://ctx.example.test/mcp", "headers": {"X-Context-Key": "${env:MCP_CTX_API_KEY}"}},
            f"MCP_CTX_API_KEY={CTX_KEY}\n",
            id="env-ref-x-context-key",
        ),
        pytest.param(
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": f"Bearer {CTX_KEY}"}},
            "",
            id="literal-bearer",
        ),
        pytest.param(
            {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${ MCP_CTX_API_KEY }"}},
            f"MCP_CTX_API_KEY={CTX_KEY}\n",
            id="spaced-reference",
        ),
    ],
)
def test_mcp_checkpoint_parts_never_carry_the_ctx_key(hermes_home, entry, env):
    _write(hermes_home, {"mcp_servers": {"ctx": entry}, "memory": {"provider": "ctx_checkpoint"}}, env)
    _provider, stores = _checkpoint(hermes_home)
    for args in stores:
        assert CTX_KEY not in json.dumps(args), "the ctx key reached a store call"
    parts = [a for a in stores if "Compaction source evidence" in str(a.get("content"))]
    assert parts and all("[REDACTED]" in a["content"] for a in parts)
    assert all("bound-ctx-key" in a["metadata"]["ctx_plugin"]["redaction"] for a in parts)


def test_mcp_key_named_in_a_header_is_masked_without_a_resolvable_entry(hermes_home):
    """An entry that signs in with OAuth has no key to resolve; the header forms are named and
    still masked by the plugin's rules. A bare key without a name is a documented limit."""
    _write(hermes_home, {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "auth": "oauth"}}})
    _provider, stores = _checkpoint(hermes_home)
    parts = "\n".join(a["content"] for a in stores if "Compaction source evidence" in str(a.get("content")))
    assert f"X-Context-Key: {CTX_KEY}" not in parts
    assert f'"X-Context-Key": "{CTX_KEY}"' not in parts
    assert f"my ctx key is {CTX_KEY}" in parts  # the limit: an unnamed hex value is kept


def test_failed_checkpoint_error_in_the_outcome_file_never_carries_the_key(hermes_home):
    """The outcome file keeps the redacted error text of a failed checkpoint; the key of an
    entry that names its own variable is masked there as well."""
    from agent.memory_manager import MemoryManager

    _write(
        hermes_home,
        {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${K}"}}}},
        f"K={CTX_KEY}\n",
    )

    def dispatch(name, args, **kwargs):
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"error": f"store refused for key {CTX_KEY}"})

    provider = Provider(dispatch=dispatch)
    provider.initialize("root-288", platform="cli", hermes_home=str(hermes_home))
    manager = MemoryManager()
    manager.add_provider(provider)
    with pytest.raises(Exception):  # noqa: B017 - the host passes the checkpoint failure up as it is
        manager.on_pre_compress(list(KEY_TRANSCRIPT), evidence_messages=list(KEY_TRANSCRIPT), require_checkpoint=True)
    state = list((hermes_home / "ctx_state").rglob("*"))
    assert any(path.is_file() and path.stat().st_size for path in state), "an outcome was recorded"
    for path in state:
        if path.is_file():
            assert CTX_KEY not in path.read_text(encoding="utf-8", errors="replace"), path


def test_a_rebinding_keeps_the_previous_entry_key_for_redaction(hermes_home):
    """A write scheduled before a re-initialization redacts when its thread runs: the key the
    entry signed in with before is still known after the entry's key changed."""
    entry = {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${K}"}}
    other = "c0de" + "13579bdf02468ace" * 3 + "13579bdf0246"
    _write(hermes_home, {"mcp_servers": {"ctx": entry}}, f"K={CTX_KEY}\n")
    provider = Provider(dispatch=_recording_dispatch()[0])
    provider.initialize("root-288", platform="cli", hermes_home=str(hermes_home))
    _write(hermes_home, {"mcp_servers": {"ctx": entry}}, f"K={other}\n")
    provider.initialize("root-288b", platform="cli", hermes_home=str(hermes_home))
    assert set(provider._bound_secrets()) == {CTX_KEY, other}
    assert CTX_KEY not in provider._redacted(f"the old key {CTX_KEY}")


def test_only_key_headers_are_bound(hermes_home):
    """Other headers of the entry (``Accept``, a user agent) are not credentials: their values
    stay in the transcript."""
    accept = "application/vnd.example-long+json"
    _write(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {
                    "url": "https://ctx.example.test/mcp",
                    "headers": {"Accept": accept, "X-Context-Key": "${MCP_CTX_API_KEY}"},
                }
            }
        },
        f"MCP_CTX_API_KEY={CTX_KEY}\n",
    )
    provider = Provider(dispatch=_recording_dispatch()[0])
    provider.initialize("root-288", platform="cli", hermes_home=str(hermes_home))
    assert provider._bound_secrets() == (CTX_KEY,)
    assert accept in provider._redacted(f"send {accept} as Accept")


def test_an_unresolved_reference_binds_nothing(hermes_home):
    """A header whose reference does not resolve (an unset variable, a source Hermes does not expand
    in config.yaml) binds nothing: its literal text is no credential."""
    _write(
        hermes_home,
        {
            "mcp_servers": {
                "ctx": {
                    "url": "https://ctx.example.test/mcp",
                    "headers": {"Authorization": "Bearer ${vault:ctx_key}", "X-Context-Key": "${UNSET_EXAMPLE_KEY}"},
                }
            }
        },
    )
    provider = Provider(dispatch=_recording_dispatch()[0])
    provider.initialize("root-288", platform="cli", hermes_home=str(hermes_home))
    assert provider._bound_secrets() == ()
    line = "the entry reads Bearer ${vault:ctx_key}"
    assert provider._redacted(line) == line


def test_short_bound_values_are_not_replaced():
    """A bound value under MIN_BOUND_SECRET_CHARS would erase a common word everywhere."""
    assert cp.redact_text("the abc of it", ("abc",)) == "the abc of it"
    assert cp.bound_secret_values(["abc", "", None, CTX_KEY]) == (CTX_KEY,)


def test_resolved_key_stays_out_of_repr_logs_and_state(hermes_home, caplog):
    _write(
        hermes_home,
        {"mcp_servers": {"ctx": {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${X}"}}}},
        f"X={CTX_KEY}\n",
    )
    caplog.set_level(logging.DEBUG)
    provider, _stores = _checkpoint(hermes_home)
    assert CTX_KEY not in repr(provider._chain())
    assert CTX_KEY not in caplog.text
    for path in hermes_home.rglob("*"):
        if path.is_file() and path.name != ".env":
            assert CTX_KEY not in path.read_text(encoding="utf-8", errors="replace"), path


# -- determinism and cost -------------------------------------------------------------------


def test_redaction_is_deterministic_for_the_same_input():
    text = "\n".join([t for t, _ in SECRET_FORMS.values()] + BENIGN_LINES + [f"key {CTX_KEY}"])
    other = "ctx-key-EXAMPLE-0123456789"
    first = cp.redact_text(text, (CTX_KEY, other))
    assert cp.redact_text(text, (other, CTX_KEY)) == first
    assert cp.redact_text(text, [CTX_KEY, other, CTX_KEY]) == first
    assert CTX_KEY not in first and other not in first


def test_after_a_host_masked_password_word_the_rest_of_the_line_goes():
    """Hermes masks only the first word of a spaced password value (``password: *** horse battery
    staple``). The plugin takes the rest of the line with it — also where the line was prose
    (``password: 8 characters minimum``): the trade-off the READMEs name."""
    assert _part_text("password: correct horse battery staple") == "password: [REDACTED]"
    assert _part_text("password: 8 characters minimum") == "password: [REDACTED]"


def test_bound_values_given_as_an_iterator_cover_every_message():
    messages = [{"role": "user", "content": f"key {CTX_KEY}"}, {"role": "assistant", "content": f"again {CTX_KEY}"}]
    rendered = cp.render_direct_evidence(messages, iter([CTX_KEY]))
    assert CTX_KEY not in rendered and rendered.count("[REDACTED]") == 2


def test_the_part_header_promises_the_key_only_where_the_entry_names_it():
    header = cp.part_header("root", "session", "0" * 64, 1, 1)
    assert "credentials are intentionally excluded" not in header
    assert "this profile's ctx key where its server entry names it" in header.replace("\n", " ")


def test_a_key_inside_a_longer_bound_value_is_masked_whole():
    """Longer values first: a shorter bound value that is part of a longer one cannot leave a
    piece of the longer one behind."""
    long_value = "Zz9" + CTX_KEY
    out = cp.redact_text(f"value {long_value}", (CTX_KEY, long_value))
    assert "Zz9" not in out and CTX_KEY not in out


@pytest.mark.parametrize(
    "unit",
    [
        "password: ",
        "token=" + "a1" * 8 + " ",
        "Passwort ist ",
        "passphrase: x ",
        "https://discord.com/api/webhooks/1/",
        "*** " * 3,
        "a" * 50,
        "x-context-key:" + "=" * 20,
        "token:|",
        "passphrase: <x> ",
        "token: '<a",
        "token=$",
        "creds=self.x(",
        "mnemonic: $",
    ],
)
def test_rules_stay_linear_on_pathological_input(unit):
    """Four times a pathological input takes about four times as long (no rescans)."""

    def timed(n):
        text = unit * n
        best = float("inf")
        for _ in range(3):
            start = time.perf_counter()
            cp._redact_checkpoint_secrets(text)
            best = min(best, time.perf_counter() - start)
        return best

    timed(500)  # warm the regex cache
    small, large = timed(10_000), timed(40_000)
    # Four times the input: linear takes about 4x, quadratic 16x; 8 separates them with room for noise.
    assert large < max(small, 0.005) * 8, f"{unit!r}: {small:.4f}s -> {large:.4f}s"


# -- error texts of the chain, the facts, the mirror and the digests (#301) ----------------


def _texts_of(exc):
    """The text of an exception and of every exception chained to it."""
    seen = []
    while exc is not None and exc not in seen:
        seen.append(exc)
        exc = exc.__cause__ or (None if exc.__suppress_context__ else exc.__context__)
    return " | ".join(f"{type(e).__name__}: {e}" for e in seen)


def _log_text(caplog):
    return "\n".join(f"{r.getMessage()} {r.exc_text or ''}" for r in caplog.records)


def _key_echo_home(home, *, memory=False):
    entry = {"url": "https://ctx.example.test/mcp", "headers": {"Authorization": "Bearer ${K}"}}
    _write(
        home,
        {
            "mcp_servers": {"ctx": entry},
            "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"memory": memory}},
        },
        f"K={CTX_KEY}\n",
    )


def _key_echo_dispatch(*, refuse, text):
    """A ctx stand-in whose refused stores repeat the key they were called with (artificial)."""

    def dispatch(name, args, **kwargs):
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        if name.endswith("__store") and refuse(args):
            return json.dumps({"error": text})
        return json.dumps({"id": "019f6000-0000-7000-8000-000000000301"})

    return dispatch


@pytest.mark.parametrize("required", [True, False], ids=["required", "best-effort"])
def test_a_failed_chain_raises_and_logs_its_error_without_the_key(hermes_home, caplog, required):
    _key_echo_home(hermes_home)
    notices = []
    dispatch = _key_echo_dispatch(
        refuse=lambda args: True, text=f"Unknown tool: mcp__ctx__store (refused for key {CTX_KEY})"
    )
    provider = Provider(dispatch=dispatch)
    provider.initialize("root-301", platform="cli", hermes_home=str(hermes_home), warning_callback=notices.append)
    with caplog.at_level(logging.DEBUG):
        if required:
            with pytest.raises(_MODULE.CtxCheckpointError) as raised:
                provider.on_pre_compress(list(KEY_TRANSCRIPT), require_checkpoint=True)
            assert CTX_KEY not in _texts_of(raised.value)
            assert "Unknown tool: mcp__ctx__store" in str(raised.value)
        else:
            assert provider.on_pre_compress(list(KEY_TRANSCRIPT)) == ""
    logged = _log_text(caplog)
    assert "chain failed" in logged
    assert CTX_KEY not in logged
    # The cause hints of the user notice still read the redacted text (#285).
    assert notices and "is not connected in this Hermes" in notices[-1]


def test_a_refused_fact_store_logs_its_error_without_the_key(hermes_home, caplog):
    _key_echo_home(hermes_home, memory=True)
    dispatch = _key_echo_dispatch(
        refuse=lambda args: args.get("category") == "learnings", text=f"store refused for key {CTX_KEY}"
    )
    provider = Provider(dispatch=dispatch)
    provider.initialize("root-301-facts", platform="cli", hermes_home=str(hermes_home))
    fact = "Der Proxy auf INFRA-WWW terminiert TLS fuer alle Dienste mit Traefik."
    with caplog.at_level(logging.DEBUG):
        frame = provider.on_pre_compress([{"role": "assistant", "content": fact}], require_checkpoint=True)
    assert "ctx checkpoint head:" in frame
    logged = _log_text(caplog)
    assert "refused for key [REDACTED]" in logged
    assert CTX_KEY not in logged and CTX_KEY not in frame


def test_a_key_cut_off_at_the_end_of_an_error_text_is_masked():
    cut = f"mcp store: refused for key {CTX_KEY[:20]}"
    out = cp.redact_error_text(cut, (CTX_KEY,))
    assert CTX_KEY[:12] not in out and out.endswith("[REDACTED]")
    assert cp.redact_error_text("ctx checkpoint budget 60s exceeded before manifest", (CTX_KEY,)) == (
        "ctx checkpoint budget 60s exceeded before manifest"
    )
