"""P4.2 tests: harness warnings, reasoning/plan narration and short step notices.

Positive cases are the live slip-throughs of 2026-10-24 (sysadmin, agent.log) and the
classes left open in the P4 report; negative cases are real result facts that must stay.
"""

from __future__ import annotations

import pytest

from ctx_plugin.config import extract_salient_facts, fact_noise_reason

FILLER = (
    " Details: HOST-MON (192.0.2.50) liefert alle Prometheus-Targets, Grafana-Dashboards sind aktuell,"
    " Alertmanager-Routen verifiziert, keine offenen Findings im Wazuh-Report, Backups vollständig."
)


def _long(prefix: str, n: int = 900) -> str:
    text = prefix
    while len(text) < n:
        text += FILLER
    return text[:n]


# Live 2026-10-24 (sysadmin), verbatim.
STREAM_STALLED = (
    "⚠ Stream stalled mid tool-call (terminal); the action was not executed. Ask me to retry if you want to continue."
)
# Live 2026-10-24 (sysadmin): head verbatim, the real message is ~30 kB of reasoning.
GIT_PUSH_LEAK = (
    "The git push was successful. Now I need to:\n1. Update the CTX block with the monitoring config\n"
    "2. Report the results in the requested format (3 blocks: Ergebnis, Beleg, DoD)\n\n"
    "The user's request was very specific:\n- FORMAT: genau 3 Blöcke: 1) Ergebnis ... 2) Beleg ... 3) DoD ...\n\n"
    'Wait, the user\'s criteria says "Target zeigt health=up in /api/v1/targets". But from the API output, '
    'the cryptobob-health target is showing as "down" because the probe is failing.'
)
assert len(GIT_PUSH_LEAK) <= 600  # the long variant is tested separately below

# Real result facts (sysadmin 2026-10-18, stored live 2026-10-24) — must be kept.
RESULT_PM_MC2 = (
    "Ergebnis\nVM-MC2, VMID 131, läuft: Ubuntu 24.04 LTS, 4 vCPU/8192 MB, VLAN42, 192.0.2.241/24, "
    "Gateway192.0.2.1; onboot und QEMU-Agent aktiv.\nOS64 GiB auf zp_data; neue Backup-Disk100 GiB auf "
    "zp_tier3, per UUID unter /srv/vm-backup gemountet und nach Reboot verifiziert.\nSSH: hermes@192.0.2.241; "
    "sudo getestet. Root-/Passwort-SSH deaktiviert, UFW-Regeln wie beauftragt aktiv.\nSecurity-Updates installiert, "
    "keine höheren noble-security-Versionen offen; Kernel6.8.0-147-generic, kein weiterer Rebootbedarf. Kein Docker, "
    "Minecraft oder Panel installiert.\n\nBeleg (Quelle+Zeit)\nLive-SSH/QGA/Nautobot, 2026-10-18 12:20–12:21 UTC: "
    "neue Boot-ID, beide Mounts samt Persistenzmarkern vorhanden; findmnt --verify fehlerfrei, dpkg --audit leer."
)
RESULT_OPENJDK = (
    "Ergebnis\nOpenJDK25 installiert: java -version = 25.0.4.1; Paket openjdk-25-jre-headless=25.0.4.1+1-1~24.04.4 "
    "aus offiziellen Ubuntu-Repositories.\nprometheus-node-exporter=1.7.0-1ubuntu0.3; Unit "
    "prometheus-node-exporter.service active/enabled, Benutzer prometheus, Listenadresse192.0.2.241:9100.\n"
    "Collector auf CPU/RAM/Disk/Systemd plus gefordertes uname begrenzt; alle sechs Collector erfolgreich."
)

DROP_CASES = [
    # harness / stream warnings
    (STREAM_STALLED, "harness-warning"),
    (
        "⚠️ Stream stalled mid tool-call (write_file); the action was not executed. Ask me to retry if you want "
        "to continue.",
        "harness-warning",
    ),
    ("Your request was not processed. Send it again if you still want me to carry it out.", "compaction-marker"),
    (
        "This turn did not complete. Some actions may already have run; verify their effects before resending.",
        "harness-warning",
    ),
    (
        "Operation interrupted: waiting for model response (12.3s elapsed). Der letzte Schritt wurde nicht beendet.",
        "compaction-marker",
    ),
    (
        "The action was not executed because the stream stalled. Ask me to retry if you want to continue.",
        "harness-warning",
    ),
    (
        "Der Deploy auf HOST-WWW ist vorbereitet. ⚠ Stream stalled mid tool-call (terminal); "
        "ask me to retry if you want to continue.",
        "harness-warning",
    ),
    # English reasoning / plan narration
    (GIT_PUSH_LEAK, "reasoning-leak"),
    (
        "There's no session_search tool available. Let me check if there's a tool_search for this. Actually, the "
        'user wants me to go through "chats" - this could mean past sessions or files on disk.',
        "reasoning-leak",
    ),
    (
        "Now I need to update the Traefik router for the new backend and then reload the container on HOST-RP.",
        "self-talk",
    ),
    ("Next I'll verify the Prometheus targets on HOST-MON and compare them with the Nautobot inventory.", "self-talk"),
    ("I'll now run the migration against the staging database and check the row counts afterwards.", "self-talk"),
    (
        "Deploy passed the postcheck. Next I'll run the e2e verification against the beta portal and the API.",
        "reasoning-leak",
    ),
    (
        "The user (via default) wants me to add two people to a Keycloak group that grants them trading access.",
        "self-talk",
    ),
    (
        "The user sent a message about reading call.yml and cloud.yml configs. Let me analyze what I found so far.",
        "self-talk",
    ),
    # German plan narration
    ("Jetzt muss ich noch die Compose-Datei auf HOST-WWW anpassen und den Container neu starten.", "reasoning-leak"),
    ("Als Nächstes prüfe ich die Grundlagen für Collector, Digest und CVE-Report auf HOST-MON.", "self-talk"),
    (
        "Remote-Helfer läuft, der Index-Guard greift. Als Nächstes prüfe ich die Grundlagen für Collector, Digest "
        "und CVE-Report.",
        "narration",
    ),
    (
        "Backup ist durch und verifiziert. Jetzt muss ich die Rollback-Tags setzen und das Register aktualisieren.",
        "reasoning-leak",
    ),
    # short step progress
    (
        "Step 2 läuft; M4A bleibt bis zur unabhängigen Prüfung der PostgreSQL- und Immutability-Pfade offen.",
        "step-progress",
    ),
    (
        "Schritt 3/5 läuft: Snapshots der VMs 129 und 131 werden auf PBS geschrieben, Dauer ca. 10 Minuten.",
        "step-progress",
    ),
    ("Schritt 2: Konfiguration von Traefik und Keycloak auf HOST-RP prüfen und anpassen.", "step-progress"),
]


@pytest.mark.parametrize("text,reason", DROP_CASES)
def test_p42_noise_filter_drops(text, reason):
    assert fact_noise_reason(text) == reason


def test_p42_reasoning_leak_dropped_at_any_length():
    """The live git-push leak is ~30 kB: the head rule has no length limit."""
    text = _long(GIT_PUSH_LEAK + "\n\n", 30_000)
    assert len(text) > 600
    assert fact_noise_reason(text) == "reasoning-leak"
    text = _long("There's no session_search tool available. Let me check if there's a tool_search for this. ", 1027)
    assert fact_noise_reason(text) == "reasoning-leak"


KEEP_CASES = [
    RESULT_PM_MC2,
    RESULT_OPENJDK,
    # long result reports mentioning a next step / plan words beyond the head stay
    _long(
        "**Live-Status, letzte Abfrage ca. 08:24 CEST:** Controller aktiv, DGX-Benchmark läuft, 29,9 GiB frei; "
        "Qwen3.8 Terminal-Bench 31/62 abgeschlossen, keine Fehler seit 06:10, Logs rotiert, Alerts ruhig. "
        "Als Nächstes prüfe ich den Retry-Lauf und melde mich mit den Zahlen.",
        935,
    ),
    _long(
        "Jetzt ist das Bild vollständig. **Was vorhin los war:** Um 12:07 hat der Gateway-Watchdog den Prozess "
        "neu gestartet, weil der Healthcheck 3x fehlschlug; Ursache war ein voller /tmp.",
        900,
    ),
    _long(
        "Route passt. Jetzt der Stand:\n\n**Erledigt (live verifiziert):** T-01..T-07, T-11..T-13 — beide VMs laufen.",
        900,
    ),
    # English final answer that introduces the result with "Let me" (not an investigation verb)
    _long(
        "You're right - I'm stuck in a loop again. Let me stop immediately and give you what you need:\n\n"
        "## LiteLLM is Running ✅\n\nThe LiteLLM proxy IS running on HOST-LLM with the database attached.",
        700,
    ),
    # (> 600 chars: short messages with "let me" anywhere are narration since P4)
    _long(
        "Done. Let me summarize: the backup job on HOST-DB now runs at 02:00 and the restore test passed (12 tables).",
        800,
    ),
    # "Step"/"Schritt" inside a result, or a long step report
    "Alle fünf Schritte sind erledigt: Step 2 (Backup) und Step 4 (Restore-Test) waren grün, 12 Tabellen geprüft.",
    _long("Schritt 3 von 5 abgeschlossen: Snapshots von VM 129 und 131 liegen auf PBS (verifiziert, 2× OK).", 400),
    # technical statements about "the user" that are not reasoning leaks
    "The user hermes has no sudo rights on HOST-WWW; only root is in the docker group (verified via id/getent).",
]


@pytest.mark.parametrize("text", KEEP_CASES)
def test_p42_noise_filter_keeps(text):
    assert fact_noise_reason(text) == ""


def test_p42_live_compaction_keeps_results_drops_leaks():
    """Replays the shape of the 2026-10-24 sysadmin compaction: closing answers with the two
    slip-throughs between real results — only the results become facts."""
    msgs = [
        {"role": "user", "content": "weiter"},
        {"role": "assistant", "content": RESULT_PM_MC2},
        {"role": "user", "content": "weiter"},
        {"role": "assistant", "content": STREAM_STALLED},
        {"role": "user", "content": "retry"},
        {"role": "assistant", "content": RESULT_OPENJDK},
        {"role": "user", "content": "weiter"},
        {"role": "assistant", "content": _long(GIT_PUSH_LEAK + "\n\n", 5000)},
        {
            "role": "user",
            "content": "Your request was not processed. Send it again if you still want me to carry it out.",
        },
        {
            "role": "assistant",
            "content": "Your request was not processed. Send it again if you still want me to carry it out.",
        },
    ]
    facts = extract_salient_facts(msgs, facts_from_user=True)
    assert facts == [RESULT_PM_MC2, RESULT_OPENJDK]
