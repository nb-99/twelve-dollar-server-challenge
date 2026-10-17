#!/usr/bin/env bash
# Installs Go and a C compiler for the cgo SQLite driver. Runs once as root.
set -euo pipefail
case "$(uname -m)" in
  x86_64)
    GO_ARCH=amd64
    GO_SHA256=ecbadb99091a3f46e31f5f934b068b1864eafa7995211b39eaddf76996045fe5
    ;;
  aarch64|arm64)
    GO_ARCH=arm64
    GO_SHA256=94f3e30b8e374bc285e7dadc11e0865726b9bc6e85b841ccceaabc0214c6b7c8
    ;;
  *) echo "Unsupported Linux architecture: $(uname -m)" >&2; exit 1 ;;
esac
apt-get update
apt-get install -y --no-install-recommends build-essential ca-certificates curl tar gzip

GO_DIR=/opt/go-challenge-1.27.2
if [ ! -f "$GO_DIR/.installed" ]; then
  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  curl --fail --location --retry 3 --retry-all-errors --connect-timeout 30 --max-time 300 \
    "https://go.dev/dl/go1.27.2.linux-${GO_ARCH}.tar.gz" -o "$TMP/go.tar.gz"
  echo "$GO_SHA256  $TMP/go.tar.gz" | sha256sum -c -
  mkdir -p "$TMP/go" "$GO_DIR"
  tar -xzf "$TMP/go.tar.gz" -C "$TMP/go" --strip-components=1 --no-same-owner
  cp -a "$TMP/go/." "$GO_DIR/"
  touch "$GO_DIR/.installed"
fi
