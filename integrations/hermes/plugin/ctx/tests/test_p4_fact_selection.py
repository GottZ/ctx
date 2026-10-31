"""P4 tests: fact selection from the end, cross-compaction dedupe, new noise filters.

Fixtures from conftest (``make_provider``/FakeClient) plus a small stateful fake that
remembers chain blocks so the server-side ``fact_hashes`` path can be exercised.
"""

from __future__ import annotations

import json
import os
import stat
from typing import ClassVar

import pytest
from conftest import join_writes

from ctx_plugin.config import (
    FactLedger,
    extract_salient_facts,
    fact_keys,
    fact_noise_reason,
    is_user_request,
    load_ctx_config,
)

FILLER = (
    " Details: HOST-MON (192.0.2.50) liefert alle Prometheus-Targets, Grafana-Dashboards sind aktuell,"
    " Alertmanager-Routen verifiziert, keine offenen Findings im Wazuh-Report, Backups vollständig."
)


def _long(prefix: str, n: int = 900) -> str:
    text = prefix
    while len(text) < n:
        text += FILLER
    return text[:n]


def _turns(*answers, user="weiter"):
    """user/assistant pairs: each answer is a turn-closing assistant reply."""
    msgs = []
    for a in answers:
        msgs.append({"role": "user", "content": user})
        msgs.append({"role": "assistant", "content": a})
    return msgs


# -- noise filters: the three observed slip-throughs + new prefixes (negative) --------

DROP_CASES = [
    (
        "[Message from agent 'default'] AUFTRAG: Prüfe die Backups auf HOST-DB und melde das Ergebnis per Mail.",
        "compaction-marker",
    ),
    ("Hi! 👋 Ich bin w4-gameserver-admin, dein Assistent für die DayZ-Server. Was kann ich für dich tun?", "greeting"),
    (
        "HOST-MON erreichbar. Jetzt: (a) Prometheus-Targets prüfen, (b) Grafana-Datasource testen, (c) Alerts.",
        "narration",
    ),
    ("Hallo zusammen, ich bin der neue Sysadmin-Bot und kümmere mich ab heute um eure Server und Backups.", "greeting"),
    (
        "On Tue, 2026-09-08 at 02:09 +0200, hermes@example.com wrote:\n> No home channel is set for Email.\n> Reply",
        "quoted-mail",
    ),
    (
        "Am Di., 8. Sep. 2026 um 02:09 Uhr schrieb Hermes <hermes@example.com>:\n> Cron-Ergebnis\n> zweite Zeile",
        "quoted-mail",
    ),
    (
        "ok\n\nOn Tue, 2026-09-08 at 02:09 +0200, hermes@example.com wrote:\n> Soll ich das Update einspielen?\n> x",
        "quoted-mail",
    ),
    (
        "> zitierte Zeile eins aus der Mail\n> zitierte Zeile zwei aus der Mail\n> drei\nPasst so, danke dir.",
        "quoted-mail",
    ),
    (
        "[IMPORTANT: Background process proc_ab6c8e9960e7 completed normally (exit code 0). Command: make",
        "compaction-marker",
    ),
    (
        "[ASYNC DELEGATION BATCH COMPLETE — deleg_cb83dad7] A background fan-out unit you dispatched earlier",
        "compaction-marker",
    ),
    (
        "[Background process proc_ad3 finished with exit code 0] Output: build complete, 3 artifacts written",
        "compaction-marker",
    ),
    (
        "[OUT-OF-BAND USER MESSAGE — a direct message from the user] bitte auch den zweiten Host prüfen",
        "compaction-marker",
    ),
    (
        "You've reached the maximum number of tool-calling iterations allowed. Please provide a final response.",
        "compaction-marker",
    ),
    (
        "/steer @image:/home/thel/.config/Hermes/composer-images/image_6ad2be.png [screenshot] schau mal hier",
        "slash-command",
    ),
    ("Verstanden. Ich kümmere mich direkt darum und melde mich, sobald der Rollout durch ist.", "self-talk"),
    ("Alles klar, dann machen wir das so wie besprochen und ich setze die Änderung heute Abend um.", "self-talk"),
    ("Letzte Registry-Checks (Traefik, oauth2-proxy, Nautobot), danach dispatch an die Subagents:", "announcement"),
    (
        "Der Benchmark läuft noch auf der DGX, aktuell 34/89 Trials abgeschlossen, Rest folgt morgen früh.",
        "transient-status",
    ),
    (
        "Step 2 läuft jetzt im freien OpenCode-Modell; danach prüfe ich den Working Tree und die Tests selbst.",
        "transient-status",
    ),
    (
        "Milestone 1 ist gestartet. Die drei Voranalysen wurden bewertet und im Qualitätsledger erfasst.",
        "transient-status",
    ),
    ("Okay, the user wants me to search through all credential-containing files and document them.", "self-talk"),
    ("The user wants me to go through all chats from the last 3 weeks and document all credentials.", "self-talk"),
    (
        "hier die münze\n@image:/home/thel/.config/Hermes/composer-images/image_82de33.png\n[screenshot]",
        "attachment-only",
    ),
]


@pytest.mark.parametrize("text,reason", DROP_CASES)
def test_noise_filter_drops(text, reason):
    assert fact_noise_reason(text) == reason


STATUS_0957 = _long(
    "**Stand 09:57 — Wartungsfenster durch, alles grün, ein Befund gefixt.** UUA-Reboots liefen exakt nach "
    "Plan (alle 15 Docker-Hosts). Der Re-Scan läuft noch für zwei Images, Ergebnis kommt nach.",
    1200,
)

KEEP_CASES = [
    STATUS_0957,
    _long(
        "Verstanden. Analyse der Ursache: Der Traefik-Router für chat.example.com zeigte auf den alten "
        "Service-Port 8080, Open WebUI lauscht seit dem Update auf 3000.",
        900,
    ),
    _long("HOST-MON erreichbar. Jetzt: (a) Prometheus-Targets, (b) Datasource, (c) Alerts — Ergebnis: ", 700),
    "PostgreSQL 17 auf HOST-DB repliziert jetzt synchron nach HOST-DB2; Failover-Test am 12.10. bestanden.",
    "Hallo-Welt-Container entfernt: hello-world:latest belegte 13 kB auf allen 15 Docker-Hosts, jetzt gelöscht.",
    _long("Ja, das Update ist eingespielt: Nextcloud 31.0.4 läuft auf HOST-NC, Apps aktualisiert, Cron ok.", 200)
    + "\n\nOn Tue, 2026-09-08 at 02:09 +0200, hermes@example.com wrote:\n> Soll ich das Update einspielen?",
    "[FLUCHT] Producer-Aktion nötig: Szene 4 braucht zwei zusätzliche Statisten, Drehbeginn bleibt 14:00 Uhr.",
]


@pytest.mark.parametrize("text", KEEP_CASES)
def test_noise_filter_keeps(text):
    assert fact_noise_reason(text) == ""


def test_stand_0957_status_report_is_selected():
    facts = extract_salient_facts(_turns(STATUS_0957))
    assert facts == [STATUS_0957]


# -- 600-char limit for transient / acknowledgement / narration ----------------------


def _pad(prefix: str, n: int) -> str:
    text = prefix
    while len(text) < n:
        text += " Ergebnis protokolliert."
    return text[:n].rstrip() + "x" * (n - len(text[:n].rstrip()))


@pytest.mark.parametrize(
    "prefix",
    [
        "Der Benchmark läuft noch auf der DGX, aktuell 34/89 Trials abgeschlossen.",
        "Verstanden. Die Ursache war ein falscher Service-Port im Traefik-Router.",
        "HOST-MON erreichbar. Jetzt: Grafana-Dashboard importieren und verlinken.",
    ],
)
def test_length_limit_600(prefix):
    at_limit = _pad(prefix, 600)
    over = _pad(prefix, 601)
    assert len(at_limit) == 600 and len(over) == 601
    assert fact_noise_reason(at_limit) != ""
    assert fact_noise_reason(over) == ""


# -- user turns ------------------------------------------------------------------------


def test_user_requests_are_not_facts_but_declarative_user_turns_are():
    request = "kannst du bitte die Backups auf HOST-DB prüfen und mir das Ergebnis per Mail schicken?"
    task = "Prüfe die Zertifikate aller Traefik-Router und erneuere die, die in weniger als 14 Tagen ablaufen."
    statement = "Die Stacks auf der DGX können bei Bedarf wieder gestartet werden, mit der GPU bin ich fertig."
    assert is_user_request(request) and is_user_request(task) and not is_user_request(statement)
    msgs = [
        {"role": "user", "content": request},
        {"role": "user", "content": task},
        {"role": "user", "content": statement},
    ]
    assert extract_salient_facts(msgs, facts_from_user=False) == [statement]  # declarative only
    assert extract_salient_facts(msgs) == []  # default: none
    assert extract_salient_facts(msgs, facts_from_user="none") == []
    assert extract_salient_facts(msgs, facts_from_user=True) == [request, task, statement]


@pytest.mark.parametrize(
    "raw,expected",
    [
        (None, "none"),
        ("true", True),
        (True, True),
        ("none", "none"),
        ("NEVER", "none"),
        ("off", False),
    ],
)
def test_facts_from_user_config(tmp_path, raw, expected):
    if raw is not None:
        (tmp_path / "ctx.json").write_text(json.dumps({"facts_from_user": raw}), encoding="utf-8")
    assert load_ctx_config(str(tmp_path))["facts_from_user"] == expected


def test_provider_passes_facts_from_user(make_provider):
    statement = "Die Stacks auf der DGX können bei Bedarf wieder gestartet werden, mit der GPU bin ich fertig."
    p, client = make_provider(config={"checkpoint_chain": False, "facts_from_user": "none"})
    p.on_pre_compress([{"role": "user", "content": statement}])
    join_writes(p)
    assert client.calls == []
    p2, client2 = make_provider(config={"checkpoint_chain": False, "facts_from_user": False})
    p2.on_pre_compress([{"role": "user", "content": statement}])
    join_writes(p2)
    assert [c["content"] for c in client2.calls] == [statement]
    p2.shutdown()
    p.shutdown()


def test_default_none_user_turns_produce_no_facts(make_provider):
    """P4.1: default ``facts_from_user`` = "none" — no ctx.json key → user turns never become facts."""
    statement = "Die Stacks auf der DGX können bei Bedarf wieder gestartet werden, mit der GPU bin ich fertig."
    answer = "Der Reverse-Proxy auf HOST-WWW terminiert TLS für alle Dienste, Zertifikate kommen von Traefik."
    p, client = make_provider(config={"checkpoint_chain": False})
    assert p._config["facts_from_user"] == "none"
    p.on_pre_compress([{"role": "user", "content": statement}, {"role": "assistant", "content": answer}])
    join_writes(p)
    assert [c["content"] for c in client.calls] == [answer]
    p.shutdown()


# -- selection from the end ----------------------------------------------------------


def test_prefers_end_and_returns_chronological_order():
    answers = [
        f"Ergebnis {i:02d}: Host HOST-{i:02d} ist gepatcht, Kernel 6.12 aktiv und alle Dienste laufen."
        for i in range(10)
    ]
    facts = extract_salient_facts(_turns(*answers))
    assert facts == answers[5:]


def test_mid_turn_short_assistant_prose_is_skipped_but_long_report_kept():
    mid = "Registry-Checks für Grafana und Keycloak sind durch, Ergebnisse gesammelt für den Report hier."
    long_mid = _long("Zwischenbericht Registry: Grafana 12.2 und Keycloak 26.4 sind die aktuellen Tags. ", 700)
    final = "Fertig: 14 von 15 Images aktuell, nur mautrix-whatsapp hängt eine Minor-Version hinterher."
    msgs = [
        {"role": "user", "content": "weiter"},
        {"role": "assistant", "content": mid},
        {"role": "assistant", "content": long_mid},
        {"role": "assistant", "content": final},
    ]
    assert extract_salient_facts(msgs) == [long_mid.rstrip(), final]


def test_skip_keys_do_not_consume_budget():
    answers = [
        f"Ergebnis {i:02d}: Host HOST-{i:02d} ist gepatcht, Kernel 6.12 aktiv und alle Dienste laufen."
        for i in range(8)
    ]
    known = [k for a in answers[5:] for k in fact_keys(a)]
    assert extract_salient_facts(_turns(*answers), skip_keys=known) == answers[:5]


def test_fact_keys_normalize_whitespace_case_and_share_title_key():
    a = "Traefik  v3.5 läuft auf HOST-PROXY,\nMiddleware sso-auth aktiv für alle Admin-Routen und Dashboards."
    b = "traefik v3.5 läuft auf host-proxy, middleware sso-auth aktiv für alle admin-routen und dashboards."
    assert fact_keys(a) == fact_keys(b)
    c = a + " Zusatz: Rate-Limit 100/min."
    assert fact_keys(a)[0] != fact_keys(c)[0]  # different content
    assert fact_keys(a)[1] == fact_keys(c)[1]  # same title → same upsert key → deduped


def test_within_run_title_dedupe():
    a = "Traefik v3.5 läuft auf HOST-PROXY, Middleware sso-auth aktiv für alle Admin-Routen und Dashboards."
    b = a + " Nachtrag: auch für Grafana."
    assert extract_salient_facts(_turns(a, b)) == [b]


# -- ledger ----------------------------------------------------------------------------


def test_ledger_persists_atomically_and_tolerates_corruption(tmp_path):
    led = FactLedger(str(tmp_path), "root/with:odd chars")
    led.add(["c1", "t1", "c1"])
    assert led.path.parent.name == "ctx_state" and led.path.exists()
    assert stat.S_IMODE(os.stat(led.path).st_mode) == 0o600
    again = FactLedger(str(tmp_path), "root/with:odd chars")
    assert again.keys() == ["c1", "t1"]
    again.discard(["c1"])
    assert FactLedger(str(tmp_path), "root/with:odd chars").keys() == ["t1"]
    assert FactLedger(str(tmp_path), "other-root").keys() == []
    led.path.write_text("{not json", encoding="utf-8")
    assert FactLedger(str(tmp_path), "root/with:odd chars").keys() == []
    assert not [p for p in led.path.parent.iterdir() if p.name.startswith(".ledger-")]


def test_ledger_is_capped(tmp_path):
    led = FactLedger(str(tmp_path), "r", max_keys=5)
    led.add([f"k{i}" for i in range(9)])
    assert FactLedger(str(tmp_path), "r", max_keys=5).keys() == ["k4", "k5", "k6", "k7", "k8"]


# -- provider: 3 consecutive compactions of one session --------------------------------

HEAD = [
    {"role": "user", "content": "Wir richten heute das Monitoring für alle Docker-Hosts neu ein, Ziel ist HOST-MON."},
    {
        "role": "assistant",
        "content": "Ausgangslage: 15 Docker-Hosts, Prometheus 3.5 auf HOST-MON, Grafana 12.2, "
        "Alertmanager mit Mail-Route an thelonius@example.com.",
    },
]


def _segment(n: int, size: int = 4):
    return _turns(
        *[
            f"Segment {n} Ergebnis {i}: Host docker-{n}{i} exportiert jetzt node_exporter-Metriken, "
            f"Target in Prometheus grün, Dashboard verlinkt."
            for i in range(size)
        ]
    )


def _compactions():
    """Like the host: protected head carried over, earlier tail repeated, new tail appended."""
    s1 = HEAD + _segment(1)
    s2 = HEAD + _segment(1)[-2:] + _segment(2)
    s3 = HEAD + _segment(2)[-2:] + _segment(3)
    return [s1, s2, s3]


def _fact_titles(client):
    return [c["title"] for c in client.calls if c["category"] == "learnings"]


@pytest.mark.parametrize("chain", [True, False])
def test_three_compactions_store_no_duplicate_facts(make_provider, chain):
    p, client = make_provider(config={"checkpoint_chain": chain})
    for i, msgs in enumerate(_compactions()):
        if i:
            p.on_session_switch(f"sess-1234-c{i}", parent_session_id="sess-1234")  # compaction keeps root
        p.on_pre_compress(msgs)
        join_writes(p)
    titles = _fact_titles(client)
    assert titles and len(titles) == len(set(titles))
    contents = [c["content"] for c in client.calls if c["category"] == "learnings"]
    assert len(contents) == len(set(contents))
    assert not any("Ausgangslage" in c for c in contents[5:])  # head not re-extracted later
    # every compaction contributed new facts (tail), none were repeats
    assert sum("Segment 3" in c for c in contents) >= 1
    p.shutdown()


def test_ledger_survives_provider_restart(make_provider):
    s1, s2, _ = _compactions()
    p, client = make_provider()
    p.on_pre_compress(s1)
    join_writes(p)
    first = set(_fact_titles(client))
    p.shutdown()
    p2, client2 = make_provider()  # same tmp HERMES_HOME + same root "sess-1234"
    p2.on_pre_compress(s1 + s2[len(HEAD) :])
    join_writes(p2)
    assert not (set(_fact_titles(client2)) & first)
    p2.shutdown()


def test_reset_starts_new_ledger(make_provider):
    p, client = make_provider()
    msgs = _compactions()[0]
    p.on_pre_compress(msgs)
    join_writes(p)
    n = len(_fact_titles(client))
    p.on_session_switch("sess-new", reset=True)
    p.on_pre_compress(msgs)
    join_writes(p)
    assert len(_fact_titles(client)) == 2 * n
    p.shutdown()


def test_failed_store_releases_claim_for_retry(make_provider):
    p, client = make_provider(config={"checkpoint_chain": False})
    client.fail_on = {0, 1, 2, 3, 4}
    msgs = _compactions()[0]
    p.on_pre_compress(msgs)
    join_writes(p)
    assert p._fact_ledger().keys() == []
    client.fail_on = set()
    p.on_pre_compress(msgs)
    join_writes(p)
    assert len(p._fact_ledger().keys()) == 2 * len(extract_salient_facts(msgs))
    p.shutdown()


def test_required_checkpoint_records_and_dedupes(make_provider):
    p, client = make_provider(config={"checkpoint_chain": False, "facts_from_user": False})
    msgs = _compactions()[0]
    r1 = p.on_pre_compress(msgs, require_checkpoint=True)
    n1 = len(_fact_titles(client))
    r2 = p.on_pre_compress(msgs, require_checkpoint=True)
    r3 = p.on_pre_compress(msgs, require_checkpoint=True)
    assert "stored 5" in r1 and n1 == 5
    assert "stored 1" in r2  # the one candidate left over (declarative user turn)
    assert "0 salient facts" in r3  # nothing new → nothing stored, receipt still non-empty
    assert len(_fact_titles(client)) == len(set(_fact_titles(client))) == 6
    p.shutdown()


# -- server-side fact_hashes in head/manifest metadata ---------------------------------


class StatefulFake:
    """Chain-aware fake: remembers checkpoint blocks by title (head upsert, metadata)."""

    shared_blocks: ClassVar[dict] = {}

    def __init__(self, *a, **k):
        self.calls = []
        self.blocks = StatefulFake.shared_blocks
        self.n = 0

    def whoami(self):
        return {"success": True, "home_scope": "shared", "label": "test"}

    def store(self, category, title, content, sensitivity=None, tags=None, **k):
        self.calls.append({"category": category, "title": title, "content": content, **k})
        if category == "compaction-checkpoints":
            bid = self.blocks.get(title, {}).get("id") or f"019f6000-0000-7000-8000-{len(self.blocks) + 1:012d}"
            self.blocks[title] = {"id": bid, "metadata": k.get("metadata") or {}}
            return {"success": True, "block": {"id": bid}}
        self.n += 1
        return {"success": True, "block": {"id": f"fact-{self.n}"}}

    def search(self, text, **k):
        rows = [{"id": b["id"], "title": t, "type": "checkpoint"} for t, b in self.blocks.items() if text in t]
        return {"success": True, "results": rows}

    def get_block(self, block_id, **k):
        for t, b in self.blocks.items():
            if b["id"] == block_id:
                return {"success": True, "block": {"id": block_id, "title": t, "metadata": b["metadata"]}}
        return {"success": False, "error": "not found"}

    def query(self, text, **k):
        return {"success": True, "sources": []}


def test_fact_hashes_in_chain_metadata_dedupe_across_hosts(monkeypatch, tmp_path):
    import ctx_plugin.provider as provider_mod
    from ctx_plugin.provider import CtxMemoryProvider

    StatefulFake.shared_blocks = {}
    monkeypatch.setattr(provider_mod, "CtxClient", StatefulFake)
    monkeypatch.setenv("CTX_KEY", "k")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    s1, _s2, _ = _compactions()

    def _prov(name):
        home = tmp_path / name / "profiles" / "x"
        home.mkdir(parents=True)
        p = CtxMemoryProvider()
        p.initialize("root-xyz", hermes_home=str(home), agent_context="primary")
        return p

    a = _prov("hostA")
    a.on_pre_compress(s1)
    join_writes(a)
    facts_a = [c["content"] for c in a._client.calls if c["category"] == "learnings"]
    head = next(b for t, b in StatefulFake.shared_blocks.items() if t.startswith("Compaction checkpoint head"))
    manifest = next(b for t, b in StatefulFake.shared_blocks.items() if t.endswith(" manifest"))
    expected = {k for f in facts_a for k in fact_keys(f)}
    assert set(head["metadata"]["fact_hashes"]) == expected
    assert set(manifest["metadata"]["fact_hashes"]) == expected

    b = _prov("hostB")  # empty local ledger, same root → server head carries the hashes
    b.on_pre_compress(s1)
    join_writes(b)
    facts_b = [c["content"] for c in b._client.calls if c["category"] == "learnings"]
    assert not (set(facts_b) & set(facts_a))
    head = next(b_ for t, b_ in StatefulFake.shared_blocks.items() if t.startswith("Compaction checkpoint head"))
    assert expected <= set(head["metadata"]["fact_hashes"])  # cumulative, not replaced
    a.shutdown()
    b.shutdown()


def test_facts_off_keeps_previous_fact_hashes_in_head(monkeypatch, tmp_path):
    import ctx_plugin.provider as provider_mod
    from ctx_plugin.provider import CtxMemoryProvider

    StatefulFake.shared_blocks = {}
    monkeypatch.setattr(provider_mod, "CtxClient", StatefulFake)
    monkeypatch.setenv("CTX_KEY", "k")
    monkeypatch.setenv("CTX_BASE_URL", "http://ctx.invalid")
    home = tmp_path / "profiles" / "dev"
    home.mkdir(parents=True)
    p = CtxMemoryProvider()
    p.initialize("root-dev", hermes_home=str(home), agent_context="primary")
    p.on_pre_compress(_compactions()[0])
    join_writes(p)
    before = next(b for t, b in StatefulFake.shared_blocks.items() if "head" in t)["metadata"]["fact_hashes"]
    p._config["auto_compress_extract"] = False
    p.on_pre_compress(_compactions()[1])
    after = next(b for t, b in StatefulFake.shared_blocks.items() if "head" in t)["metadata"]["fact_hashes"]
    assert before and after == before
    p.shutdown()
