"""Plugin 0.16.1 (GottZ/ctx#311): no output of the plugin repeats the user info, query or fragment of a
configured ctx URL — the REST base URL or the URL of the ctx MCP server entry.

Before, on ``transport: rest``, ``hermes memory status`` and ``hermes ctx_checkpoint status`` printed the base URL as
written (and the CLI the ``ctx_base_url`` of ctx.json once more), the memory block of the system prompt named it, the
REST client's error for a base URL with a password repeated that password (urllib reads it as the port), and an error
text naming the MCP entry's URL (a transport error) reached the outcome file, the user notice and the agent tool's
answer with its fragment and its query values. The values below are artificial; no test opens a network connection
(the REST client's opener and the connection test are stand-ins).
"""

import importlib
import importlib.util
import json
import sys
import types
from pathlib import Path

import pytest

PLUGIN_DIR = Path(__file__).resolve().parents[1]
_SPEC = importlib.util.spec_from_file_location(
    "ctx_checkpoint_url_parts_hidden", PLUGIN_DIR / "__init__.py", submodule_search_locations=[str(PLUGIN_DIR)]
)
assert _SPEC is not None and _SPEC.loader is not None
_MODULE = importlib.util.module_from_spec(_SPEC)
sys.modules[_SPEC.name] = _MODULE
_SPEC.loader.exec_module(_MODULE)
Provider = _MODULE.CtxCheckpointMemoryProvider

USER, PASSWORD, QUERY_VALUE, FRAGMENT = "alice311", "s3cretPw311", "QKEY311value", "FRAG311tok"
MARKS = (USER, PASSWORD, QUERY_VALUE, FRAGMENT)
REST_URL = f"https://{USER}:{PASSWORD}@ctx.example.test:8443/ctx?sig={QUERY_VALUE}#{FRAGMENT}"
REST_SHOWN = "https://ctx.example.test:8443/ctx"
MCP_URL = f"https://{USER}:{PASSWORD}@ctx.example.test/mcp?sig={QUERY_VALUE}#{FRAGMENT}"
REST_KEY = "b311" + "fedcba9876543210" * 3 + "0123456789ab"  # artificial: CTX_KEY


def _submodule(name):
    return importlib.import_module(f"{_SPEC.name}.{name}")


def _assert_clean(text):
    for mark in MARKS:
        assert mark not in str(text), text


@pytest.fixture
def home(_isolated_hermes_home, monkeypatch):
    for name in ("CTX_API_KEY", "CTX_KEY", "CTX_BASE_URL"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setattr(_MODULE, "_hygiene_warning_emitted", False)
    return _isolated_hermes_home


@pytest.fixture
def rest(home, monkeypatch):
    """``transport: rest`` with the base URL in ctx.json and a stand-in for the REST client's opener."""
    monkeypatch.setenv("CTX_KEY", REST_KEY)
    (home / "ctx.json").write_text(json.dumps({"ctx_base_url": REST_URL}), encoding="utf-8")
    section = {"transport": "rest", "memory": True}
    config = {"memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": section}}
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")
    opened = []

    class Answer:
        def __init__(self, body):
            self.body = body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

        def read(self):
            return self.body

    def urlopen(request, *, timeout):
        opened.append(request.full_url)
        return Answer(json.dumps({"success": True, "home_scope": "s311", "label": "l311", "role": "r"}).encode())

    monkeypatch.setattr(_submodule("memory.client"), "_urlopen", urlopen)
    return types.SimpleNamespace(home=home, section=section, opened=opened)


# -- display_url -----------------------------------------------------------------------------


@pytest.mark.parametrize(
    ("url", "shown"),
    [
        (MCP_URL, "https://ctx.example.test/mcp"),
        ("https://[::1]:8443/mcp?key=x", "https://[::1]:8443/mcp"),
        ("http://127.0.0.1:8080/mcp", "http://127.0.0.1:8080/mcp"),
        ("https://al%40ice:pa%3Ass@ctx.example.test/mcp", "https://ctx.example.test/mcp"),
        ("https://ctx.example.test", "https://ctx.example.test"),
        ("${CTX_URL}?key=x#y", "${CTX_URL}"),
        ("${SCHEME}://alice:pw@ctx.example.test/mcp?key=x", "${SCHEME}://ctx.example.test/mcp"),
        ("${CTX_URL}/a@b", "(unreadable URL)"),
        ("user:pass@ctx.example.test/mcp?key=x", "(unreadable URL)"),
        ("ctx.example.test:8443/mcp", "(unreadable URL)"),
        ("https://alice/x:pw@ctx.example.test/mcp", "(unreadable URL)"),
        ("https://ctx.example.test/a@b/mcp", "(unreadable URL)"),
        # A ``?`` or ``#`` in the user info ends the host early: user and password head look like host and port.
        ("https://alice311:1234#pw@ctx.example.test/mcp", "(unreadable URL)"),
        ("https://alice311#pw@ctx.example.test/mcp", "(unreadable URL)"),
        ("https://alice311:1234?pw@ctx.example.test/mcp", "(unreadable URL)"),
        ("${SCHEME}://alice311:pw?x@ctx.example.test/mcp", "(unreadable URL)"),
        ("https://ctx.example.test/mcp?mail=a@b", "(unreadable URL)"),
        ("https://[::1/mcp", "(unreadable URL)"),
        ("https://ctx.example.test:99999/mcp", "(unreadable URL)"),
        ("https://user:pw@:8443/mcp", "(unreadable URL)"),
        ("", ""),
        (None, ""),
    ],
)
def test_display_url_never_repeats_user_info_query_or_fragment(url, shown):
    assert _submodule("memory.status").display_url(url) == shown


# -- transport: rest ---------------------------------------------------------------------------


class _Connected:
    """A REST client that answers whoami, whatever its base URL."""

    def __init__(self, base_url, api_key, **kwargs):
        pass

    def whoami(self):
        return {"success": True, "home_scope": "s311", "label": "l311", "role": "r"}


def test_memory_status_on_rest_shows_the_base_url_without_its_credentials(rest, monkeypatch):
    monkeypatch.setattr(_submodule("memory.provider"), "CtxClient", _Connected)

    status = Provider(config=rest.section).get_status_config({})

    assert status["ok"] is True and status["base_url"] == REST_SHOWN
    assert status["summary"] == f"✓ Connected · {REST_SHOWN} · scope: s311 · label: l311"
    _assert_clean(json.dumps(status))


def test_memory_status_on_rest_names_the_refusal_and_the_base_url_without_its_credentials(rest):
    status = Provider(config=rest.section).get_status_config({})

    client = _submodule("memory.client")
    assert status["ok"] is False and status["base_url"] == REST_SHOWN
    assert status["summary"].startswith("✗ CTX base URL refused") and status["summary"].endswith(f" · {REST_SHOWN}")
    _assert_clean(json.dumps(status))
    assert rest.opened == [] and client.BASE_URL_REFUSED[:60] in status["summary"]


def test_the_system_prompt_names_the_server_without_its_credentials(rest, monkeypatch):
    provider = Provider(config=rest.section)
    monkeypatch.setattr(provider, "_connect", lambda: None)
    provider.initialize("root-311", platform="cli", hermes_home=str(rest.home))
    provider._active, provider._base_url, provider._client = True, REST_URL, object()

    block = provider.system_prompt_block()

    assert f"Server: {REST_SHOWN} · scope:" in block
    _assert_clean(block)


def test_hermes_ctx_checkpoint_status_on_rest_prints_no_credentials(rest, capsys):
    cli = _submodule("memory.cli")

    rc = cli.cmd_status(types.SimpleNamespace(), rest.home, cli._load(rest.home))

    out = capsys.readouterr().out
    assert rc == 1 and f"base URL: {REST_SHOWN}" in out and f"ctx_base_url: {REST_SHOWN}" in out
    _assert_clean(out)


@pytest.mark.parametrize(
    "base",
    [
        f"https://{USER}:{PASSWORD}@127.0.0.1/ctx",
        f"https://{USER}@127.0.0.1:9/ctx",
        f"https://127.0.0.1:9/ctx?sig={QUERY_VALUE}",
        f"https://127.0.0.1:9/ctx#{FRAGMENT}",
        # A ``/`` in the user info ends the host before the ``@``: the netloc holds no ``@``.
        f"https://{USER}:1234/{PASSWORD}@127.0.0.1/ctx",
        f"https://{USER}:ab/{PASSWORD}@127.0.0.1/ctx",
    ],
)
def test_the_rest_client_sends_nothing_for_a_base_url_with_credentials(base, monkeypatch):
    client = _submodule("memory.client")
    monkeypatch.setattr(client, "_urlopen", lambda *a, **k: pytest.fail("a request was sent"))

    with pytest.raises(client.CtxApiError) as raised:
        client.CtxClient(base, REST_KEY).whoami()

    assert str(raised.value) == client.BASE_URL_REFUSED
    _assert_clean(str(raised.value))


def test_the_rest_client_still_sends_to_a_plain_base_url(rest):
    client = _submodule("memory.client")

    assert client.CtxClient("https://ctx.example.test:8443/", REST_KEY).whoami()["success"] is True
    assert rest.opened == ["https://ctx.example.test:8443/api/whoami"]


# -- texts that name the MCP entry's URL -------------------------------------------------------

# Hermes' text for a transport failure, with httpx naming the URL it failed on (httpx keeps user info, query and
# fragment in it) and Hermes' own masking of ``key=``-like values applied.
TRANSPORT_ERROR = (
    "MCP call failed: HTTPStatusError: Client error '503 Service Unavailable' for url "
    f"'https://{USER}:{PASSWORD}@ctx.example.test/mcp?key=[REDACTED]&sig={QUERY_VALUE}#{FRAGMENT}'"
)


def test_a_transport_error_naming_the_url_leaves_no_credential_in_any_output(home, monkeypatch):
    """The error of a checkpoint store reaches the outcome file, the user notice and the log; the same error of an
    agent tool reaches the model."""
    config = {
        "mcp_servers": {"ctx": {"url": MCP_URL, "auth": "oauth"}},
        "memory": {"provider": "ctx_checkpoint", "ctx_checkpoint": {"memory": True}},
    }
    (home / "config.yaml").write_text(json.dumps(config), encoding="utf-8")

    def dispatch(name, args, **kwargs):
        if name.endswith("__search"):
            return json.dumps({"result": "[]"})
        return json.dumps({"error": TRANSPORT_ERROR})

    notices = []
    provider = Provider(config={"memory": True}, dispatch=dispatch)
    provider.initialize("root-311", platform="cli", hermes_home=str(home), warning_callback=notices.append)
    with pytest.raises(_MODULE.CtxCheckpointError) as raised:
        provider.on_pre_compress([{"role": "user", "content": "keep this"}], require_checkpoint=True)
    tool = provider.handle_tool_call("ctx_store", {"category": "c", "title": "t", "content": "x"})

    state = (home / "ctx_state" / "checkpoint-status.state").read_text(encoding="utf-8")
    outputs = [str(raised.value), state, *notices, tool]
    for text in outputs:
        _assert_clean(text)
    assert "for url 'https://ctx.example.test/mcp'" in tool and "for url 'https://ctx.example.test/mcp'" in state


def test_an_error_text_without_such_a_url_is_unchanged():
    checkpoint = _submodule("memory.checkpoint")
    text = "connection error: <urlopen error [Errno 111] Connection refused> at https://ctx.example.test/mcp, retry"

    assert checkpoint.redact_error_text(text) == text


@pytest.mark.parametrize(
    ("error", "kept"),
    [
        (f"connect failed for url '{MCP_URL}'", "for url 'https://ctx.example.test/mcp'"),
        (f"server rejected query {f'sig={QUERY_VALUE}'} and fragment {FRAGMENT}", "server rejected query"),
        (f"nonnumeric port: '{PASSWORD}@ctx.example.test'", "nonnumeric port"),
    ],
)
def test_the_connection_test_masks_the_entry_url_in_a_probe_error(home, monkeypatch, error, kept):
    status = _submodule("memory.status")

    def probe(name, entry, timeout):
        raise RuntimeError(error)

    monkeypatch.setattr(status, "probe_server", probe)
    test = status.run_connection_test("ctx", {"url": MCP_URL})

    assert test.ok is False and kept in test.detail
    _assert_clean(test.detail)


@pytest.mark.parametrize(
    ("url", "error"),
    [
        (
            f"{USER}:{PASSWORD}@ctx.example.test/mcp",
            f"Invalid MCP URL for 'ctx': scheme must be http or https, got '{USER}' "
            f"('{USER}:{PASSWORD}@ctx.example.test/mcp')",
        ),
        (
            f"https://{USER}:1234#{PASSWORD}@ctx.example.test/mcp",
            f"connect failed: host '{USER}' port 1234 (https://{USER}:1234#{PASSWORD}@ctx.example.test/mcp)",
        ),
    ],
    ids=["no scheme", "fragment mark in the user info"],
)
def test_the_connection_test_withholds_a_probe_error_for_a_url_it_cannot_take_apart(home, monkeypatch, url, error):
    """Hermes names a URL it refuses, and for ``user:pass@host`` the user name stands in its text as the scheme; with a
    ``#`` in the user info a client takes the user name for the host."""
    status = _submodule("memory.status")

    def probe(name, entry, timeout):
        raise RuntimeError(error)

    monkeypatch.setattr(status, "probe_server", probe)
    test = status.run_connection_test("ctx", {"url": url})

    assert test.ok is False and test.detail.endswith(status.URL_DETAILS_WITHHELD)
    assert "mcp_servers.ctx ((unreadable URL))" in test.detail
    _assert_clean(test.detail)


def test_setup_refuses_a_url_without_scheme_without_repeating_it(home):
    with pytest.raises(ValueError) as raised:
        _MODULE._normalize_setup_values({"ctx_url": f"{USER}:{PASSWORD}@ctx.example.test/mcp?sig={QUERY_VALUE}"}, {})
    assert "must start with https:// or http://" in str(raised.value)
    _assert_clean(str(raised.value))
