#!/usr/bin/env bash
# ctx installer — detects OS/arch, builds or downloads, installs to /usr/local/bin
set -euo pipefail

INSTALL_DIR="${INSTALL_DIR:-/usr/local/bin}"

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

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"

# Method 1: build from this clone (if Go is available). `go install
# github.com/GottZ/ctx/cmd/ctx@latest` does not resolve: the Go module lives in go/, not at
# the repository root (issue #258).
if command -v go &>/dev/null; then
  echo "Go found — building from this clone..."
  TMP_BIN="$(mktemp "${TMPDIR:-/tmp}/ctx-build.XXXXXX")"
  trap 'rm -f "$TMP_BIN"' EXIT
  go -C "$SCRIPT_DIR" build -o "$TMP_BIN" ./cmd/ctx
  install -m 0755 "$TMP_BIN" "${INSTALL_DIR}/ctx"
  echo "Installed: $("${INSTALL_DIR}/ctx" version 2>/dev/null || echo 'ctx')"
  exit 0
fi

# Method 2: pre-built binary from dist/
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
