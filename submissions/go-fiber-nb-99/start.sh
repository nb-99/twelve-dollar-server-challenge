#!/usr/bin/env bash
set -euo pipefail
: "${SQLITE_PATH:?SQLITE_PATH must name a fresh copy of the seed database}"
: "${JWT_SECRET:?JWT_SECRET is required}"
exec "$(dirname "$0")/bin/server"
