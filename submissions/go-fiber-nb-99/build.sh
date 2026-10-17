#!/usr/bin/env bash
# Vets, tests, and builds bin/server. GO overrides the toolchain from install.sh.
set -euo pipefail
cd "$(dirname "$0")"
GO=${GO:-/opt/go-challenge-1.27.2/bin/go}
export CGO_ENABLED=1 GOTOOLCHAIN=local
"$GO" vet ./...
"$GO" test ./...
"$GO" build -trimpath -o bin/server ./cmd/server
