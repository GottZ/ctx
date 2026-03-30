#!/usr/bin/env bash
set -euo pipefail

VERSION="${1:-dev}"
COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
DATE=$(date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT} -X main.date=${DATE}"

PLATFORMS=(
  "linux/amd64"
  "linux/arm64"
  "darwin/amd64"
  "darwin/arm64"
  "windows/amd64"
)

OUTDIR="dist"
rm -rf "$OUTDIR"
mkdir -p "$OUTDIR"

for platform in "${PLATFORMS[@]}"; do
  GOOS="${platform%/*}"
  GOARCH="${platform#*/}"
  output="${OUTDIR}/ctx-${GOOS}-${GOARCH}"
  [[ "$GOOS" == "windows" ]] && output="${output}.exe"

  echo "Building ${GOOS}/${GOARCH}..."
  GOOS="$GOOS" GOARCH="$GOARCH" CGO_ENABLED=0 \
    go build -ldflags "$LDFLAGS" -o "$output" ./cmd/ctx/

  echo "  → ${output} ($(du -h "$output" | cut -f1))"
done

echo ""
echo "Done. Binaries in ${OUTDIR}/"
ls -lh "$OUTDIR/"
