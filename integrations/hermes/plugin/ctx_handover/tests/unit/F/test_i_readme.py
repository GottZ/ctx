"""Layer I (package i-readme): README statements bound to the behaviour or to the constants behind them
(``reports/L2/readme-check.md`` §4, integrator decisions 1, 4, 5 and 7 of the i-readme brief).

Each test reads the README sentence it guards and the code path or constant the sentence describes, so a change on
either side turns it red: the limit for sessions of older Hermes builds (a late ``verify_failed`` ends the handover,
the source stays free), the pairing-code lifetime, the numbers of the disclosure, the default column of the
settings table and what ``/handover setup`` learns from the gateway's ``/api/auth/providers``."""

from __future__ import annotations

import http.server
import json
import re
import threading

import pytest

from ctx_handover.engine import gitpack, jobs, pairing, policy, surfaces
from ctx_handover.engine import settings as settings_rules
from ctx_handover.engine.errors import VerifyFailed
from ctx_handover.engine.model import Options, Role
from ctx_handover.tests.conftest import PLUGIN_ROOT
from ctx_handover.tests.fakes import FakeClock
from ctx_handover.tests.fakes.surfaces_fake import FakeBackend
from ctx_handover.tests.unit.A.harness import Harness

from ._l2r_helpers import FLAT, README, near
from .test_commands import _plugin, _slash

DEFAULTS = settings_rules.DEFAULTS


def _listener(url: str) -> FakeBackend:
    backend = FakeBackend(peers=[])
    backend.config_data = {"plugins": {"entries": {"ctx_handover": {"settings": {"listen_url": url}}}}}
    return backend


# ---------------------------------------------------------------------------------------------------------
# Decision 1 (#17): a late identity backfill ends the handover with verify_failed — the source stays free
# ---------------------------------------------------------------------------------------------------------


def test_late_backfill_verify_failed_ends_the_handover_and_leaves_the_source_free(tmp_path):
    """★ README "Sessions from older Hermes builds": when the core backfills message identities on the source while
    a handover runs, the delta's expected hashes differ and the target refuses it (``VerifyFailed`` in
    ``start_apply_delta``, B ``test_legacy_backfill_between_export_and_delta_fails_safe``). The conductor does not
    restart by itself: ``delta_applying → aborting → aborted`` before the commit point, no restart counted, the
    target never commits, and the source releases its lock and leases — usable, unlocked. The README says exactly
    that: start the handover again and it begins with a fresh export.

    Red (README before i-readme): ``…and the handover starts over from a fresh export — nothing is accepted
    silently.`` (no "when you start it again", no word on the source). Mutant (conductor restarts on a delta
    ``VerifyFailed``) and mutant (abort without releasing the source): see ``reports/L2/i-readme.md``.
    """
    h = Harness(tmp_path, FakeClock())
    h.world.inject("source", "start_prepare", "action", action=lambda w: w.source.append_rows("late"))
    h.world.inject("target", "start_apply_delta", "error", error=VerifyFailed("backfilled ids", which="h_all"))
    c = h.conductor()
    c.run_until_negotiated()
    assert c.run() is None
    states = h.states()
    assert states[-3:] == ["delta_applying", "aborting", "aborted"], states
    assert "committing" not in states and states.count("exporting") == 1, states
    row = h.ledger.job(h.job_id, Role.CONDUCTOR)
    assert row["last_error"]["code"] == "verify_failed"
    assert int(row.get("restarts") or 0) == 0
    assert "start_commit" not in h.calls("target") and h.world.target.ack is None and not h.world.target.active
    src = h.world.source
    assert src.usable and not src.primary_lock and not src.veto and not src.leases
    assert src.state == "released" and "release" in src.events

    lead = "**Sessions from older Hermes builds.**"
    para = FLAT.split(lead, 1)[1].split("- **", 1)[0]
    assert "(`verify_failed`)" in para
    assert "starts over from a fresh export when you start it again" in para, para
    assert "ends before anything is committed" in para and "usable and unlocked" in para, para


# ---------------------------------------------------------------------------------------------------------
# Decision 4 (#28): the pairing code lives pairing.DEFAULT_TTL_S — the README's 10 minutes
# ---------------------------------------------------------------------------------------------------------


def test_pair_code_lifetime_is_the_pairing_default(fake_host, monkeypatch):
    """★ ``/handover pair`` without ``--ttl`` asks for a code of ``pairing.DEFAULT_TTL_S`` and prints its minutes;
    the README's "one-time code (10 minutes, single use)" and "≤ 10 min by default, at most 60" are that default and
    ``pairing.MAX_TTL_S``.

    Red (before i-readme): with ``pairing.DEFAULT_TTL_S`` set to 900 the verb still asked for 600 —
    ``commands.py`` carried its own literal ``or 600``.
    """
    plugin, backend, _ = _plugin(_listener("https://s.example/"))
    out = _slash(plugin, "pair --name laptop")
    assert backend.last("create_pair_code")["ttl_s"] == pairing.DEFAULT_TTL_S == 600
    assert f"valid {pairing.DEFAULT_TTL_S // 60} min, single use" in out, out

    with monkeypatch.context() as m:
        m.setattr(pairing, "DEFAULT_TTL_S", 900)
        out = _slash(plugin, "pair --name laptop")
    assert backend.last("create_pair_code")["ttl_s"] == 900
    assert "valid 15 min, single use" in out, out

    minutes = pairing.DEFAULT_TTL_S // 60
    assert f"a one-time code ({minutes} minutes, single use)" in FLAT
    assert f"(≤ {minutes} min by default, at most {pairing.MAX_TTL_S // 60})" in FLAT


# ---------------------------------------------------------------------------------------------------------
# Decision 5 (#57, #62, #32, #20, B6): the numbers of the README follow the code
# ---------------------------------------------------------------------------------------------------------


def _readme_numbers() -> dict[str, tuple[str, tuple[object, ...]]]:
    """README phrase built from the code's value → the values that must agree (schema default, ``Options``
    default, ``options_from`` of the default settings, module constant)."""
    opts = surfaces.options_from(surfaces.Parsed(verb="to", args={}), {})
    chars = DEFAULTS["refine_history_chars"]
    tokens = DEFAULTS["refine_side_question_max_tokens"]
    idle_min = DEFAULTS["participant_idle_timeout_min"]
    days, gib = DEFAULTS["block_cache_days"], DEFAULTS["block_cache_gb"]
    return {
        "refine-history-chars": (
            f"up to {chars:,} characters of redacted recent transcript",
            (chars, Options().refine_history_chars, opts.refine_history_chars, 60_000),
        ),
        "side-question-tokens": (
            f"skipped above {tokens:,} tokens".replace(",", " "),
            (tokens, Options().refine_side_question_max_tokens, opts.refine_side_question_max_tokens, 200_000),
        ),
        "sniff-bytes": (
            f"reads the first {policy.SNIFF_BYTES // 1024} KiB of each candidate file",
            (policy.SNIFF_BYTES, gitpack._SNIFF_BYTES, 8 * 1024),
        ),
        "block-cache-background": (
            f"a block cache in the plugin data folder (default up to {gib:g} GiB, {days} days;",
            ((days, gib), (3, 5.0)),
        ),
        "block-cache-unredacted": (
            f"The block cache keeps copies up to {days} days / {gib:g} GiB",
            ((days, gib), (3, 5.0)),
        ),
        "offer-ttl": (
            f"leaves an offer ({jobs.OFFER_TTL_S // 3600} h)",
            (jobs.OFFER_TTL_S, 2 * 3600),
        ),
        "quiet-pull-source": (
            f"`participant_idle_timeout_min` ({idle_min} minutes by default)",
            (idle_min * 60, Options().participant_idle_timeout_s, opts.participant_idle_timeout_s, 15 * 60),
        ),
    }


@pytest.mark.parametrize("key", sorted(_readme_numbers()))
def test_readme_numbers_follow_the_code(key):
    """★ The README states each number exactly as the code holds it: refine budget (60,000 characters, 200 000
    tokens), content sniff (8 KiB — ``policy`` and the blob sniff of ``gitpack``), block cache (3 days / 5 GiB, both
    places), offer lifetime (2 h), quiet pull source (15 minutes, setting ``participant_idle_timeout_min``). The
    phrase is built from the code's value, and every place that carries the value agrees with the last element
    (the number the README was written for).

    Mutants: ``settings.SCHEMA["refine_history_chars"]`` 50000, ``policy.SNIFF_BYTES`` 4 KiB, ``jobs.OFFER_TTL_S``
    3 h, README "5 GiB, 3 days" → "5 GiB, 4 days" — each red (``reports/L2/i-readme.md``)."""
    phrase, values = _readme_numbers()[key]
    assert phrase in FLAT, phrase
    assert len(set(values)) == 1, values


# ---------------------------------------------------------------------------------------------------------
# Decision 5 (#48, B4): the default column of the settings table equals the schema
# ---------------------------------------------------------------------------------------------------------


def _cell_values(cell: str) -> list[object]:
    """Defaults as the table writes them: ``auto``, ``""``, ``[]``, ``false``, numbers; comma-separated cells give
    one value per key."""
    out: list[object] = []
    for part in [p.strip() for p in cell.split(",")]:
        text = part.strip("`")
        if text == '""':
            out.append("")
        elif text == "[]":
            out.append([])
        elif text in {"true", "false"}:
            out.append(text == "true")
        elif re.fullmatch(r"-?\d+(\.\d+)?", text):
            out.append(float(text))
        else:
            out.append(text)
    return out


def _same(readme_value: object, schema_default: object) -> bool:
    if isinstance(readme_value, float):
        return (
            not isinstance(schema_default, bool)
            and isinstance(schema_default, (int, float))
            and (float(schema_default) == readme_value)
        )
    if isinstance(schema_default, list) and not isinstance(readme_value, list):
        return schema_default == [readme_value]
    return type(readme_value) is type(schema_default) and readme_value == schema_default


def _settings_rows() -> list[tuple[list[str], list[object]]]:
    table = README.split("## Settings", 1)[1].split("## Disclosure", 1)[0]
    rows = []
    for line in table.splitlines():
        cells = [c.strip() for c in line.strip().strip("|").split("|")]
        if len(cells) != 3 or cells[0] in {"Key", ""} or set(cells[0]) <= {"-"}:
            continue
        rows.append((re.findall(r"`([a-z_]+)`", cells[0]), _cell_values(cells[1])))
    return rows


def _manifest_schema() -> dict:
    import hermes_yaml

    return hermes_yaml.safe_load((PLUGIN_ROOT / "plugin.yaml").read_text(encoding="utf-8"))["config_schema"]


def test_readme_settings_table_defaults_equal_the_schema():
    """★ Every row of the README settings table: each key exists in ``settings.SCHEMA`` and in ``plugin.yaml``
    ``config_schema``; a cell with one value per key gives each key's default, a single value the default of the
    row's first key (``workspace_root``, ``allow_roots`` share ``~/hermes-handover``: every key of such a row whose
    default is that value or a list of it). Both schemas agree on each checked default.

    Mutants: README ``confirm_above_mb`` default 500 → 400; ``settings.SCHEMA["inbound"]`` default ``on`` — each
    red (``reports/L2/i-readme.md``)."""
    manifest = _manifest_schema()
    rows = _settings_rows()
    assert len(rows) >= 12, rows
    checked = 0
    for keys, values in rows:
        assert keys, values
        for key in keys:
            assert key in settings_rules.SCHEMA and key in manifest, key
        if len(values) == len(keys):
            pairs = list(zip(keys, values, strict=True))
        else:
            assert len(values) == 1, (keys, values)
            pairs = [(keys[0], values[0])] + [
                (k, values[0])
                for k in keys[1:]
                if settings_rules.SCHEMA[k]["type"] != "secret" and _same(values[0], DEFAULTS[k])
            ]
        for key, value in pairs:
            assert _same(value, DEFAULTS[key]), (key, value, DEFAULTS[key])
            assert _same(value, manifest[key].get("default")), (key, value, manifest[key].get("default"))
            checked += 1
    assert checked >= 17, checked


# ---------------------------------------------------------------------------------------------------------
# Decision 7 (#26, #49, B1): /handover setup reads this gateway's /api/auth/providers once
# ---------------------------------------------------------------------------------------------------------


class _Providers(http.server.BaseHTTPRequestHandler):
    """A dashboard stand-in answering ``GET /api/auth/providers`` in the shape of ``hermes_cli/dashboard_auth/
    routes.py:api_auth_providers`` (``{"providers": [{name, display_name, supports_password}]}``)."""

    def do_GET(self) -> None:  # http.server API name
        self.server.hits.append(self.path)  # type: ignore[attr-defined]
        status, body = self.server.answer  # type: ignore[attr-defined]
        data = json.dumps(body).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *args) -> None:  # quiet test output
        del args


BASIC = (200, {"providers": [{"name": "basic", "display_name": "Basic", "supports_password": True}]})
TOKEN_ONLY = (200, {"providers": []})
NOT_A_DASHBOARD = (404, {"detail": "Not Found"})


@pytest.mark.parametrize(
    ("answer", "expected"),
    [
        (BASIC, "Its dashboard uses a Basic login: on the other device choose peer_gate basic_login"),
        (TOKEN_ONLY, "Its dashboard signs in with a fixed token: set HERMES_DASHBOARD_SESSION_TOKEN"),
        (NOT_A_DASHBOARD, "The address does not look like a Hermes dashboard (no /api/auth/providers)."),
    ],
    ids=["basic-login", "fixed-token", "not-a-dashboard"],
)
def test_setup_reads_auth_providers_once_and_names_the_gate(fake_host, monkeypatch, answer, expected):
    """★ README §Set up step 2 and §Disclosure Network: ``/handover setup`` asks this gateway's own
    ``/api/auth/providers`` exactly once and says whether the other device needs the Basic login
    (``peer_gate basic_login``) or the fixed dashboard token; an address without that route is named as no Hermes
    dashboard. A local HTTP server stands in for the dashboard (no Hermes process).

    Mutants: ``_gate_advice`` asks twice; the ``supports_password`` branch inverted — each red
    (``reports/L2/i-readme.md``)."""
    for name in ("http_proxy", "https_proxy", "all_proxy", "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("no_proxy", "127.0.0.1")
    server = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _Providers)
    server.hits = []  # type: ignore[attr-defined]
    server.answer = answer  # type: ignore[attr-defined]
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        url = f"http://127.0.0.1:{server.server_address[1]}"
        plugin, _backend, _ = _plugin(_listener(url))
        out = _slash(plugin, "setup")
    finally:
        server.shutdown()
        server.server_close()
        thread.join(5)
    assert server.hits == ["/api/auth/providers"], server.hits  # type: ignore[attr-defined]
    assert f"Address for pairing: {url}" in out, out
    assert expected in out, out
    others = [(BASIC, "Basic login"), (TOKEN_ONLY, "fixed token"), (NOT_A_DASHBOARD, "does not look like")]
    for other, text in others:
        if other is not answer:
            assert text not in out, (text, out)

    assert near(FLAT, "`/handover setup` reads this gateway's own `/api/auth/providers` once", "Network")
    step = (
        "it tells you whether its dashboard uses a fixed token or a Basic login and which field the other device needs"
    )
    assert step in FLAT


def test_readme_names_the_pull_reclaim_limit():
    """(L3-F13, named limit): after a finished pull the target is not told about a reclaim on the source; the README
    says so in its limits section."""
    limits = README.split("## Limits", 1)[1].split("\n## ", 1)[0]
    assert "learns of a reclaim on the source only while the job runs" in limits
