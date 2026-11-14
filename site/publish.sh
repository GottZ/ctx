#!/usr/bin/env bash
#
# ctx: build the project homepage (https://ctx.gottz.de) and sync it into a folder.
#
# Usage: site/publish.sh [--force] [target-directory]
#
# The target is, in this order: the argument; the environment variable
# CTX_SITE_TARGET; a line CTX_SITE_TARGET=<path> in the untracked .env at the
# repository root (only that line is read, the file is never sourced). With
# none of them set the script stops before it builds anything. The repository
# holds no target path.
#
# Sources: site/index.src.html, site/style.css and site/favicon.svg. site/rings.py
# renders the hero figure into the placeholder @@RINGS@@. The architecture
# diagram comes unchanged from docs/, the plugin pictures unchanged from
# integrations/hermes/assets/: one source each, no copies in the repository.
#
# The folder is served as static files behind a Content-Security-Policy that
# allows only same-origin styles and images (default-src 'none'; style-src 'self';
# img-src 'self'): no scripts, no web fonts, no inline styles, nothing from
# another origin.
#
# The sync runs rsync --delete. The script always refuses a relative target, /,
# your home directory, the repository or a folder above it, and a folder inside
# the repository that holds tracked files. An existing folder that is not empty
# and holds no index.html (a folder this script did not fill) it accepts only
# with --force.
#
# If the target lies in a Nextcloud data directory (<data>/<user>/files/<path>,
# where <data> holds Nextcloud's marker file .ncdata or .ocdata), the files get
# the owner of the target's parent folder (Nextcloud writes as that user), and
# `occ files:scan --path=<user>/files/<path>` runs in the Nextcloud container
# (name: $NEXTCLOUD_CONTAINER, default "nextcloud") so Nextcloud knows about the
# change. Neither is needed to serve the site; a failure is reported and does
# not stop the publish.

set -euo pipefail

here="$(cd "$(dirname "$0")" && pwd)"
repo="$(dirname "$here")"

die() { echo "publish.sh: $*" >&2; exit 2; }

force=0
arg=""
while (($#)); do
    case "$1" in
        --force) force=1 ;;
        -h|--help) sed -n '3,/^$/s/^# \{0,1\}//p' "$0"; exit 0 ;;
        --)
            shift
            (($# <= 1)) || die "more than one target given"
            if (($#)); then
                [[ -z "$arg" ]] || die "more than one target given"
                arg="$1"
            fi
            break ;;
        -*) die "unknown option: $1" ;;
        *) [[ -z "$arg" ]] || die "more than one target given"; arg="$1" ;;
    esac
    shift
done

target="$arg"
source_of_target="argument"
if [[ -z "$target" ]]; then
    target="${CTX_SITE_TARGET:-}"
    source_of_target="environment variable CTX_SITE_TARGET"
fi
if [[ -z "$target" && -f "$repo/.env" ]]; then
    # Only the line CTX_SITE_TARGET=... (optionally after "export"), the last one
    # wins. The value is split like a shell word, without running anything: quotes
    # and a trailing comment are dropped, and it has to be exactly one path.
    if ! target="$(python3 - "$repo/.env" <<'EOF'
import re
import shlex
import sys

path = sys.argv[1]
value = None
with open(path, encoding="utf-8", errors="replace") as f:
    for line in f:
        m = re.match(r"\s*(?:export\s+)?CTX_SITE_TARGET=(.*)$", line.rstrip("\r\n"))
        if m:
            value = m.group(1)
if value is None:
    sys.exit(0)
try:
    words = shlex.split(value, comments=True)
except ValueError as e:
    sys.exit(f"publish.sh: cannot read CTX_SITE_TARGET in {path}: {e}")
if len(words) > 1:
    sys.exit(f"publish.sh: CTX_SITE_TARGET in {path} must be one path, got {words!r}")
print(words[0] if words else "")
EOF
)"; then
        exit 2
    fi
    source_of_target="CTX_SITE_TARGET in $repo/.env"
fi
[[ -n "$target" ]] || die "no target directory. Pass it as the first argument, set CTX_SITE_TARGET, or add a line CTX_SITE_TARGET=/absolute/path to $repo/.env."

# Guard the rsync --delete below against a wrong target.
[[ "$target" == /* ]] || die "target must be an absolute path, got '$target' (from the $source_of_target)"
resolve() { python3 -c 'import os, sys; print(os.path.realpath(sys.argv[1]))' "$1"; }
real_target="$(resolve "$target")"
real_repo="$(resolve "$repo")"
real_home="$(resolve "${HOME:-/nonexistent}")"
[[ "$real_target" != "/" ]] || die "refusing to sync into / (target '$target')"
[[ "$real_target" != "$real_home" ]] || die "refusing to sync into the home directory $real_home (target '$target')"
case "$real_repo/" in
    "$real_target"/*) die "refusing to sync into the repository or a folder above it: $real_target" ;;
esac
case "$real_target/" in
    "$real_repo"/*)
        tracked="$(git -C "$real_repo" ls-files -- "$real_target" 2>/dev/null)" \
            || die "target $real_target is inside the repository, and git cannot say what it tracks there"
        [[ -z "$tracked" ]] || die "refusing to sync into a repository folder with tracked files: $real_target (e.g. ${tracked%%$'\n'*})"
        ;;
esac
if [[ -e "$real_target" && ! -d "$real_target" ]]; then
    die "target exists and is not a directory: $real_target"
fi
if [[ -d "$real_target" && ! -e "$real_target/index.html" && -n "$(ls -A "$real_target")" ]]; then
    ((force)) || die "target $real_target is not empty and holds no index.html; rsync --delete would remove its files. Pass --force if that is what you want."
fi

mkdir -p "$repo/.gotmp"
build="$(mktemp -d -p "$repo/.gotmp" site-build.XXXXXX)"
trap 'rm -rf "$build"' EXIT

python3 "$here/rings.py" > "$build/rings.frag"
python3 - "$here/index.src.html" "$build/rings.frag" "$build/index.html" <<'EOF'
import sys
src, frag, out = sys.argv[1:4]
html = open(src, encoding="utf-8").read()
if html.count("@@RINGS@@") != 1:
    sys.exit("index.src.html: the placeholder @@RINGS@@ must occur exactly once")
open(out, "w", encoding="utf-8").write(
    html.replace("@@RINGS@@", open(frag, encoding="utf-8").read()))
EOF

cp "$here/style.css" "$here/favicon.svg" "$build/"
cp "$repo/docs/how-ctx-works.svg" "$repo/docs/how-ctx-works-dark.svg" "$build/"
cp "$repo/integrations/hermes/assets/ctx_discord_policy.webp" \
   "$repo/integrations/hermes/assets/ctx_handover_video.jpg" "$build/"
rm "$build/rings.frag"

mkdir -p "$real_target"
rsync -a --delete --chmod=D755,F644 "$build/" "$real_target/"

# Nextcloud data directory: the nearest folder above the target that holds
# .ncdata or .ocdata, with the target at <user>/files/<path> below it. Walked on
# the path as given, only normalised (a symlink above the data directory must
# not hide it).
nc_probe="$(python3 -c 'import os, sys; print(os.path.normpath(sys.argv[1]))' "$target")"
nc_path=""
dir="$nc_probe"
while [[ "$dir" != "/" ]]; do
    dir="$(dirname "$dir")"
    if [[ -e "$dir/.ncdata" || -e "$dir/.ocdata" ]]; then
        rel="${nc_probe#"$dir"/}"
        [[ "$rel" =~ ^[^/]+/files/.+$ ]] && nc_path="$rel"
        break
    fi
done
if [[ -n "$nc_path" ]]; then
    chown -R --reference="$(dirname "$real_target")" "$real_target" \
        || echo "note: owner of $real_target not changed" >&2
    docker exec -u www-data "${NEXTCLOUD_CONTAINER:-nextcloud}" \
        php occ files:scan --path="$nc_path" >/dev/null \
        || echo "note: Nextcloud file index for $nc_path not updated" >&2
fi
echo "published to $real_target:"
ls -l "$real_target"
