-- =============================================================================
-- archive-legacy-topic-maps.sql — Ablösung Stufe 2 (Welle W-H)
-- Part of ctx by GottZ (https://github.com/GottZ/ctx)
-- =============================================================================
-- Achse 02 / Welle W-H (plan-cluster-topicmap design/02 §4.6/§7 "W-H";
-- User-Entscheid E2-02 A, DECISIONS.md: "is_archived mit gepinntem
-- Vorher-Zustand + Rollback").
--
-- Zwei Zeilen-Maps sind seit 2026-03 tot und stehen trotzdem in jedem
-- `ctx search index`-Ergebnis:
--
--   topic-map-work   (scope work)
--   topic-map-hth    (scope work — der Titel sagt 'hth', der Scope sagt 'work';
--                     eine BP-4-Altlast von vor der T12/T38-Scope-Klammer, die
--                     obendrein einen FALSCHEN Bootstrap-Pfad anbietet)
--
-- KEIN DELETE, ausdrücklich. `is_archived = true` nimmt die Blöcke aus der
-- Trefferliste, lässt sie über `ctx get <id>` aber lesbar — genau das ist der
-- Rollback-Pfad, und ein DELETE machte ihn unmöglich. Die letzte Spalte dieser
-- Ausgabe ist der Rückweg als fertiger Einzeiler.
--
-- NICHT DIE MAP DES AKTIVEN TENANTS. Zwei Gürtel:
--   (1) eine explizite Titel-Allowlist (genau die zwei oben), und
--   (2) ein PRODUZENTEN-GUARD: ein Kandidat wird übersprungen, solange ein
--       AKTIVER API-Key diesen Scope als home_scope trägt. Dann gibt es nämlich
--       weiterhin einen Digest-/Wurzel-Map-Lauf, der dorthin schreibt, und
--       Archivieren führe einen Kampf gegen einen lebenden Erzeuger. Das ist
--       die sachliche Fassung des E2-Arguments "kein Tenant hat homeScope=work"
--       — als Bedingung statt als Annahme.
-- Der In-Band-Stub (W-E, digest.mode=stub) trägt die Übergangszeit für
-- topic-map-private; dieser Schritt fasst ihn nicht an.
--
-- BETRIEBS-SCHRITT, KEIN DEPLOY-SCHRITT. Nicht in der Migrationskette, weil er
-- Daten anfasst und ein Mensch die gepinnte Vorher-Ausgabe sehen soll, bevor
-- er committet. Aufruf über archive-legacy-topic-maps.sh (Default: Dry-Run in
-- einer zurückgerollten Transaktion).
--
-- Die Ausgabe ist der PIN: id, Titel, Scope, Content-Länge und der
-- SHA-256 des Inhalts VOR der Archivierung — genug, um nachzuweisen, dass
-- danach derselbe Block dasteht und nur sein Flag anders ist.
-- =============================================================================

WITH targets AS (
    SELECT b.id,
           b.title,
           b.scope,
           length(b.content)                                   AS content_length,
           encode(sha256(convert_to(b.content, 'UTF8')), 'hex') AS content_sha256
      FROM context_blocks b
     WHERE b.category = 'index'
       AND b.title IN ('topic-map-work', 'topic-map-hth')
       AND NOT b.is_archived
       AND NOT EXISTS (
             SELECT 1 FROM context_api_keys k
              WHERE k.active AND k.home_scope = b.scope)
), archived AS (
    UPDATE context_blocks b
       SET is_archived = true
      FROM targets t
     WHERE b.id = t.id
    RETURNING b.id
)
SELECT t.id::text,
       t.title,
       t.scope,
       t.content_length,
       t.content_sha256,
       (SELECT count(*) FROM archived)::int AS archived_n,
       'UPDATE context_blocks SET is_archived = false WHERE id = ''' || t.id::text || ''';' AS rollback_sql
  FROM targets t
 ORDER BY t.title;
