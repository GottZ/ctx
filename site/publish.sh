#!/usr/bin/env bash
#
# ctx — Homepage bauen und veröffentlichen (https://ctx.gottz.de)
# Usage: site/publish.sh [zielverzeichnis]   (Default: /var/www/gottz.ctx)
#
# Quelle: site/index.src.html + site/style.css + site/favicon.svg; die
# Hero-Figur rendert site/rings.py in den Platzhalter @@RINGS@@, das
# Architektur-Diagramm kommt unverändert aus docs/ (eine Quelle, kein Drift
# zur README). Ausgeliefert wird statisch von reversed
# (/compose/reversed/conf/lua/config/static_routes.lua, Host ctx.gottz.de).

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$here")"
target="${1:-/var/www/gottz.ctx}"

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
echo "published to $target:"
ls -l "$target"
