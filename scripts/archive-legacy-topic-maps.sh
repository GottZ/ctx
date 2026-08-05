#!/usr/bin/env bash
# =============================================================================
# archive-legacy-topic-maps.sh — geführter Betriebs-Schritt der Welle W-H
# Part of ctx by GottZ (https://github.com/GottZ/ctx)
# =============================================================================
# Archiviert die beiden toten Zeilen-Maps `topic-map-work` und `topic-map-hth`
# (design/02 §4.6/§7 "W-H", User-Entscheid E2-02 A). KEIN DELETE — die Blöcke
# bleiben über `ctx get <id>` lesbar, und genau das ist der Rollback-Pfad.
#
# DEFAULT IST DRY-RUN. Der Lauf passiert in einer Transaktion, die zurückgerollt
# wird; die Ausgabe ist trotzdem die echte, gepinnte Vorher-Liste (ids,
# Content-Länge, SHA-256, fertiger Rollback-Einzeiler je Zeile). Erst `--apply`
# committet.
#
#   bash scripts/archive-legacy-topic-maps.sh            # Dry-Run, ändert nichts
#   bash scripts/archive-legacy-topic-maps.sh --apply    # committet
#
# Zwei Gürtel gegen "die falsche Map erwischt": eine explizite Titel-Allowlist
# in der SQL, und ein Produzenten-Guard (ein Scope mit AKTIVEM home_scope-Key
# wird übersprungen — dort schreibt weiterhin jemand hin). Die Map des aktiven
# Tenants steht in keiner der beiden Listen.
#
# ⚠ Live-Doktrin dieses Projekts: das ist ein Schritt NACH dem Deploy, mit
# ausdrücklicher Freigabe. Der Bau-Pfad führt ihn nie gegen eine echte Datenbank
# aus — die Gates laufen gegen Testcontainer.
# =============================================================================
set -euo pipefail

APPLY=false
for arg in "$@"; do
  case "$arg" in
    --apply) APPLY=true ;;
    -h|--help) sed -n '2,30p' "$0"; exit 0 ;;
    *) echo "unbekanntes Argument: $arg" >&2; exit 2 ;;
  esac
done

SQL_FILE="$(cd "$(dirname "$0")" && pwd)/archive-legacy-topic-maps.sql"
[[ -f "$SQL_FILE" ]] || { echo "SQL fehlt: $SQL_FILE" >&2; exit 1; }

# Verbindung wie in CLAUDE.md dokumentiert: der Context-Store läuft im
# db-Container. PSQL kann überschrieben werden, wenn die DB woanders liegt.
PSQL="${CTX_PSQL_CMD:-docker exec -i -e PGPASSWORD=${CONTEXT_DB_PASSWORD:-} n8n-db-1 psql -U ${CONTEXT_DB_USER:-admin} -d ${CONTEXT_DB:-context_store}}"

if $APPLY; then
  echo "== W-H: ARCHIVIERE (committet) =="
  WRAP="BEGIN; $(cat "$SQL_FILE") COMMIT;"
else
  echo "== W-H: DRY-RUN (Transaktion wird zurückgerollt, es ändert sich nichts) =="
  WRAP="BEGIN; $(cat "$SQL_FILE") ROLLBACK;"
fi

# ON_ERROR_STOP, damit ein Fehler nicht in einem halb offenen Block endet.
echo "$WRAP" | $PSQL -v ON_ERROR_STOP=1 --pset=pager=off

cat <<'NOTE'

-- Vorher-Zustand ist oben gepinnt (id · Länge · sha256).
-- Rollback: die Spalte rollback_sql jeder Zeile ist der fertige Einzeiler, z. B.
--   UPDATE context_blocks SET is_archived = false WHERE id = '<id>';
-- Der Block war nie weg: `ctx get <id>` liest ihn auch archiviert.
NOTE
