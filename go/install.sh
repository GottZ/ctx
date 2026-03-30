#!/usr/bin/env bash
# ctx installer — detects OS/arch, builds or downloads, installs to /usr/local/bin
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"
REPO="github.com/GottZ/ctx"

# Detect platform
OS=$(uname -s | tr '[:upper:]' '[:lower:]')
ARCH=$(uname -m)
case "$ARCH" in
  x86_64)  ARCH="amd64" ;;
  aarch64|arm64) ARCH="arm64" ;;
  *) echo "Unsupported architecture: $ARCH"; exit 1 ;;
esac

echo "Platform: ${OS}/${ARCH}"
echo "Install dir: ${INSTALL_DIR}"

# Method 1: go install (if Go is available)
if command -v go &>/dev/null; then
  echo "Go found — building from source..."
  GOBIN="$INSTALL_DIR" go install "${REPO}/cmd/ctx@latest"
  echo "Installed: $(ctx version 2>/dev/null || echo 'ctx')"
  exit 0
fi

# Method 2: pre-built binary from dist/
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
BINARY="${SCRIPT_DIR}/dist/ctx-${OS}-${ARCH}"
[[ "$OS" == "windows" ]] && BINARY="${BINARY}.exe"

if [[ -f "$BINARY" ]]; then
  echo "Installing pre-built binary..."
  cp "$BINARY" "${INSTALL_DIR}/ctx"
  chmod +x "${INSTALL_DIR}/ctx"
  echo "Installed: $(ctx version 2>/dev/null || echo 'ctx')"
  exit 0
fi

echo "Error: No Go compiler and no pre-built binary for ${OS}/${ARCH}."
echo "Either install Go (https://go.dev/dl/) or run ./build.sh first."
exit 1
