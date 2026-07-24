-- =============================================================================
-- 116_link_notify.sql — NOTIFY für Link-Mutationen + Block-Attribut-Flips
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Evokoa-Clean-Room design/05 §3.1 (Welle W05.3), Entscheide E-05-2 (=NOTIFY
-- statt Poll) und E-05-7 (=spaltengefilterter Attribut-Flip-Trigger). K1-
-- Konfliktauflösung: die Design-Nummer 108 ist vorläufig, kanonisch ist 116.
--
-- Schließt die Signal-Lücke aus Inventur 05 §6: kein bestehender NOTIFY-Kanal
-- trägt Link-Writes; der Manage-link-put-Pfad
-- (handler/context_manage_issues.go, store.PutStructuralLink) ist heute
-- komplett signalfrei. Konsument ist der Graph-Cache-Rebuild-Arm (W05.2,
-- events/graph_cache.go + internal/graphcache), der auf ctx_link_write bereits
-- LISTENed — bis zu dieser Migration feuerte der Kanal nie.
--
-- ── Entwurfs-Entscheidungen (design/05 §3.1 Nr. 1–7) ─────────────────────────
--  1. FOR EACH ROW mit KONSTANTEM Payload statt FOR EACH STATEMENT: Postgres
--     dedupliziert identische (channel, payload)-Paare innerhalb einer
--     Transaktion. Ein Replace-Batch (dream/writelinks.go) oder ein ctid-Batch-
--     Prune (store/tenant.go) erzeugt damit genau EINE Notification; ein
--     0-Zeilen-Statement (deleteStaleLinks ohne Treffer) erzeugt KEINE — ein
--     statement-level Trigger feuerte dort nutzlos und triggerte ein Voll-
--     Rebuild. Payload = Tabellenname, reine Diagnose: der Konsument ist ein
--     Debounce, der nur "dirty" braucht, keine Row-Identität.
--  2. Auch DELETE — die Signal-Lücke betrifft gerade die Löscher (Replace-
--     Semantik, täglicher Cleanup, Scope-Move-Sweep, Tenant-Prune).
--  3. Idempotent per CREATE OR REPLACE + DROP TRIGGER IF EXISTS (Muster 004).
--  4. Kein Backfill nötig — der Cache ist abgeleiteter Prozess-Zustand, der
--     Bestandsdaten-Pfad ist der Boot-Full-Build (design/05 §4.3).
--  5. Block-Writes: NUR Attribut-Flips feuern, Stempel nicht. Das bestehende
--     ctx_block_write (004) bleibt unkonsumiert — Block-Writes sind
--     hochfrequent (jeder Dream-Zyklus stempelt dream_checked_at). Der neue
--     Trigger ist spaltengefiltert (AFTER UPDATE OF is_archived, scope) UND
--     WHEN-geguarded (IS DISTINCT FROM), feuert also nur bei echten
--     Sichtbarkeits-Änderungen: Guard-Auto-Archiv (M107: 65 Flips auf einmal)
--     und Scope-Moves. Damit ist die Zusage "Hint-Drift ≤ MaxStaleness"
--     (§4.4) unter der Dirty-Age-Staleness beweisbar — JEDE cache-relevante
--     Mutation erzeugt ein Dirty-Signal.
--  6. Auch TRUNCATE — heute nutzt kein Writer TRUNCATE auf den Link-Tabellen,
--     aber ein künftiger Bulk-Pfad (Tenant-Wipe) bliebe sonst bis zum Hard-
--     Intervall cache-unsichtbar: gelöschte Kanten lebten als Ghosts weit über
--     MaxStaleness. TRUNCATE ist row-level nicht abbildbar → eigener
--     statement-level Trigger, der bewusst immer feuert. Kosten null.
--  7. Migrations-Laufzeit @10M: CREATE/DROP TRIGGER ist metadata-only
--     (Millisekunden nach Lock-Erhalt), nimmt aber ACCESS EXCLUSIVE auf allen
--     drei Tabellen — hinter einem Langläufer staut die Migration ALLE
--     nachfolgenden Reader/Writer. Deshalb lock_timeout im File; Abbruch mit
--     SQLSTATE 55P03 ist der gewollte Ausgang. Deploy-Hinweis (schreibarmes
--     Fenster, gefahrloser Re-Run) in docs/operations.md §Deploy & migrations.
--
-- Tx-Hinweis (Konvention 058/059/061/091): der Runner (store/migrations.go)
-- wickelt jede Migration in eine eigene Transaktion, SET LOCAL ist damit
-- transaktions-gebunden und selbst-revertierend — kein lock_timeout-Rest auf
-- der zurückgegebenen Pool-Verbindung.
--
-- Forward-only. Keine neue Tabelle, keine Spalte → test.sh table count
-- UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

CREATE OR REPLACE FUNCTION notify_link_write() RETURNS trigger AS $$
BEGIN
    -- Konstanter Payload pro Tabelle (Nr. 1): Batch ⇒ 1 Notification,
    -- 0 Zeilen ⇒ 0 Notifications.
    PERFORM pg_notify('ctx_link_write', TG_TABLE_NAME);
    RETURN NULL;
END;
$$ LANGUAGE plpgsql;

-- Row-level auf beiden Link-Tabellen (Dedupe macht das batch-sicher):
DROP TRIGGER IF EXISTS trg_dream_link_write ON context_dream_links;
CREATE TRIGGER trg_dream_link_write
    AFTER INSERT OR UPDATE OR DELETE ON context_dream_links
    FOR EACH ROW EXECUTE FUNCTION notify_link_write();

DROP TRIGGER IF EXISTS trg_struct_link_write ON context_structural_links;
CREATE TRIGGER trg_struct_link_write
    AFTER INSERT OR UPDATE OR DELETE ON context_structural_links
    FOR EACH ROW EXECUTE FUNCTION notify_link_write();

-- TRUNCATE ist row-level nicht abbildbar → eigener statement-level Trigger
-- (feuert bewusst immer — TRUNCATE ist per Definition eine Mutation):
DROP TRIGGER IF EXISTS trg_dream_link_truncate ON context_dream_links;
CREATE TRIGGER trg_dream_link_truncate
    AFTER TRUNCATE ON context_dream_links
    FOR EACH STATEMENT EXECUTE FUNCTION notify_link_write();

DROP TRIGGER IF EXISTS trg_struct_link_truncate ON context_structural_links;
CREATE TRIGGER trg_struct_link_truncate
    AFTER TRUNCATE ON context_structural_links
    FOR EACH STATEMENT EXECUTE FUNCTION notify_link_write();

-- Block-ATTRIBUT-Flips (Sichtbarkeits-Hints im Cache): NUR is_archived/scope,
-- spaltengefiltert + WHEN-Guard — Dream-Stempel (dream_checked_at) und
-- Content-Writes feuern hier NICHT (Nr. 5):
DROP TRIGGER IF EXISTS trg_block_visattr_write ON context_blocks;
CREATE TRIGGER trg_block_visattr_write
    AFTER UPDATE OF is_archived, scope ON context_blocks
    FOR EACH ROW
    WHEN (OLD.is_archived IS DISTINCT FROM NEW.is_archived
          OR OLD.scope IS DISTINCT FROM NEW.scope)
    EXECUTE FUNCTION notify_link_write();
