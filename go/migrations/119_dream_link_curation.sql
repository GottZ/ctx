-- =============================================================================
-- 119_dream_link_curation.sql — user governance over dream links (pin + rationale)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Review-Befund 2026-07-26: dream-review war eine reine Read-Fläche — kein
-- benutzergetriebener Einzel-Link-Widerruf, kein Operator-Urteil überlebte den
-- Replace-Sweep, keine dauerhafte Begründung pro Link. Zwei additive Spalten
-- schließen das (Konsument: store.DreamLinkResolve hinter manage
-- dream-link-resolve, Muster GuardResolveBatch):
--
--   pinned    — vom Menschen bestätigter Link. Der Dream-Replace-Sweep
--               (dream/writelinks.go deleteStaleLinks) überspringt gepinnte
--               Zeilen (WHERE NOT pinned): ein Operator-Urteil überlebt damit
--               jeden späteren LLM-Zyklus, der den Link nicht mehr re-emittiert.
--               Der Supersedes-Revert-Mechanismus bleibt für ungepinnte Links
--               unverändert.
--   rationale — dauerhafte Begründung pro Link. Bleibt beim automatischen
--               Schreiben NULL: der benchmarkte V5-Eval-Prompt
--               (dream/evaluate.go) emittiert nur {target_id, type, confidence}
--               — kein Begründungs-Feld im Response-JSON, und Prompt-Änderungen
--               sind eine eigene eval-gestützte Welle (Session 24/25).
--               Gesetzt wird sie vom Operator beim confirm-Resolve.
--
-- ADD COLUMN mit konstantem DEFAULT ist seit PG11 metadata-only (kein Table-
-- Rewrite @10M Links); lock_timeout schützt die ACCESS-EXCLUSIVE-Acquisition
-- hinter einem Langläufer (Muster 116; SQLSTATE 55P03 = gewollter Abbruch,
-- gefahrloser Re-Run). Idempotent per IF NOT EXISTS. Kein Index nötig: der
-- einzige pinned-Prädikat-Pfad (deleteStaleLinks) läuft über den PK-Präfix
-- source_block_id.
--
-- Tx-Hinweis (Konvention 058/059/061/091/116): der Runner (store/migrations.go)
-- wickelt jede Migration in eine eigene Transaktion, SET LOCAL ist damit
-- selbst-revertierend.
--
-- Forward-only. Keine neue Tabelle → test.sh table count UNCHANGED.
-- =============================================================================

SET LOCAL lock_timeout = '2s';

ALTER TABLE context_dream_links
    ADD COLUMN IF NOT EXISTS pinned BOOLEAN NOT NULL DEFAULT false;

ALTER TABLE context_dream_links
    ADD COLUMN IF NOT EXISTS rationale TEXT;
