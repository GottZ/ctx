-- =============================================================================
-- 131_dream_last_inert.sql — Inert-Ausgang des letzten Dream-Zyklus am Block
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Settings-UI Back-off-Editor (Kurven-Welle 4). Kontext: dream.go
-- SetDreamCooldown / BackoffConfig.
--
-- WARUM EINE SPALTE: Der Back-off-Stempel (dream_cooldown_until) wird beim
-- Zyklus-Abschluss aus der DANN aktiven Policy berechnet — inert (kein Link
-- geschrieben) startet den Block InertOffset Stufen weiter oben auf derselben
-- Kurve. Bisher war der Inert-Ausgang nur im Stempel selbst kodiert und ging
-- danach verloren. Die manage action dream-backoff-restamp rechnet nach einem
-- Policy-Save ALLE Stempel unter der neuen Kurve neu (checked_at + Kurve(n));
-- ohne persistierten Inert-Ausgang müsste sie Inert-Blöcke auf die
-- Active-Basis ziehen — beim Default-Offset 7 (exp 1.6) ein Faktor ~29 zu
-- früh, und die Dream-Queue flutet nach jedem Save mit reifen Inert-Blöcken.
--
-- BACKFILL: bewusst keiner. Historische Stempel tragen die Inert-Info nicht
-- rekonstruierbar (die Policy-Generation des Stempels ist unbekannt); der
-- Default false liest Bestandsblöcke beim ersten Restamp einmalig auf der
-- Active-Basis — Back-off verzögert nur, verliert nie —, und ab dem nächsten
-- abgeschlossenen Zyklus schreibt SetDreamCooldown den echten Ausgang. Die
-- Spalte konvergiert organisch mit dem normalen Dream-Betrieb.
--
-- ADD COLUMN mit konstantem DEFAULT ist in PostgreSQL metadata-only (kein
-- Table-Rewrite), auch bei 1M+ Zeilen.

ALTER TABLE context_blocks
    ADD COLUMN IF NOT EXISTS dream_last_inert boolean NOT NULL DEFAULT false;

COMMENT ON COLUMN context_blocks.dream_last_inert IS
    'Inert-Ausgang des letzten abgeschlossenen Dream-Zyklus (kein Link geschrieben). Gesetzt von SetDreamCooldown; gelesen von dream-backoff-restamp, damit eine Policy-Neubewertung Inert-Blöcke auf der Inert-Stufe der Kurve hält.';
