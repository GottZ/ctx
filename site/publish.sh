#!/usr/bin/env bash
#
# ctx — Homepage bauen und veröffentlichen (https://ctx.gottz.de)
# Usage: site/publish.sh [zielverzeichnis]
#   Default: der Nextcloud-Ordner CTX/www (/mnt/storage/nextcloud/data/gz/files/CTX/www)
#
# Quelle: site/index.src.html + site/style.css + site/favicon.svg; die
# Hero-Figur rendert site/rings.py in den Platzhalter @@RINGS@@, das
# Architektur-Diagramm kommt unverändert aus docs/ (eine Quelle, kein Drift
# zur README). Ausgeliefert wird statisch von reversed
# (/compose/reversed/conf/lua/config/static_routes.lua, Host ctx.gottz.de).
#
# Liegt das Ziel in einem Nextcloud-Datenverzeichnis, bekommen die Dateien den
# Besitzer des Elternordners (Nextcloud schreibt als dieser Nutzer), und der
# Nextcloud-Dateiindex wird für den Ordner aktualisiert, damit die Dateien dort
# sichtbar sind. Beides ist für die Auslieferung nicht nötig; ein Fehlschlag
# wird gemeldet und bricht die Veröffentlichung nicht ab.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$here")"
target="${1:-/mnt/storage/nextcloud/data/gz/files/CTX/www}"

mkdir -p "$repo/.gotmp"
build="$(mktemp -d -p "$repo/.gotmp" site-build.XXXXXX)"
trap 'rm -rf "$build"' EXIT

python3 "$here/rings.py" > "$build/rings.frag"
python3 - "$here/index.src.html" "$build/rings.frag" "$build/index.html" <<'EOF'
import sys
src, frag, out = sys.argv[1:4]
html = open(src, encoding="utf-8").read()
if html.count("@@RINGS@@") != 1:
    sys.exit("index.src.html: Platzhalter @@RINGS@@ muss genau einmal vorkommen")
open(out, "w", encoding="utf-8").write(
    html.replace("@@RINGS@@", open(frag, encoding="utf-8").read()))
EOF

cp "$here/style.css" "$here/favicon.svg" "$build/"
cp "$repo/docs/how-ctx-works.svg" "$repo/docs/how-ctx-works-dark.svg" "$build/"
rm "$build/rings.frag"

mkdir -p "$target"
rsync -a --delete --chmod=D755,F644 "$build/" "$target/"

# Nextcloud-Datenverzeichnis: <daten>/<nutzer>/files/<pfad>
if [[ "$target" =~ ^(.*/data)/([^/]+)/files/(.+)$ ]]; then
    nc_path="${BASH_REMATCH[2]}/files/${BASH_REMATCH[3]}"
    chown -R --reference="$(dirname "$target")" "$target" \
        || echo "Hinweis: Besitzer von $target nicht angepasst" >&2
    docker exec -u www-data "${NEXTCLOUD_CONTAINER:-nextcloud}" \
        php occ files:scan --path="$nc_path" >/dev/null \
        || echo "Hinweis: Nextcloud-Dateiindex für $nc_path nicht aktualisiert" >&2
fi
echo "published to $target:"
ls -l "$target"
