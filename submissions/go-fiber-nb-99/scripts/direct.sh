#!/usr/bin/env bash
# Run the challenge suite or benchmark against the app on this host.
#   scripts/direct.sh test
#   scripts/direct.sh benchmark [benchmark.py options]
# The app serves k6 directly, as on the challenge VPS. Runs bin/server from make
# build and an installed k6 v2; a missing k6 is downloaded to a temporary
# directory that is deleted afterwards.
set -euo pipefail
action=${1:?usage: direct.sh test|benchmark [benchmark.py options]}
shift
here=$(cd "$(dirname "$0")/.." && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d "${TMPDIR:-/tmp}/twelve-dollar-go.XXXXXX")
pids=()
child=
# shellcheck source-path=SCRIPTDIR source=lib.sh
. "$here/scripts/lib.sh"

cleanup() {
  local status=$?
  trap - EXIT
  for pid in ${pids[@]+"${pids[@]}"}; do kill "$pid" 2>/dev/null || true; done
  for pid in ${pids[@]+"${pids[@]}"}; do wait "$pid" 2>/dev/null || true; done
  rm -rf "$work"
  exit "$status"
}
trap cleanup EXIT
# Bash runs traps only after a foreground command exits, so the benchmark runs in
# the background. Ctrl-C already reaches it; SIGTERM sent to make must be forwarded.
stop() {
  if [ -n "$child" ]; then
    [ "$1" = INT ] || kill -TERM "$child" 2>/dev/null || true
    wait "$child" 2>/dev/null || true
  fi
  exit "$2"
}
trap 'stop INT 130' INT
trap 'stop TERM 143' TERM

[ -x "$here/bin/server" ] || { echo "bin/server is missing; run make build" >&2; exit 1; }
need curl
need python3

if [ ! -f "$root/seed/feed.db" ] || [ ! -f "$root/seed/tokens.json" ]; then
  need node
  need sqlite3
  bash "$root/seed/make-seed.sh"
fi
cp "$root/seed/feed.db" "$work/feed.db"

# The app holds one keep-alive connection per k6 user, so both need many open files.
raise_file_limit
port=$(free_port)
url=http://127.0.0.1:$port
pprof=127.0.0.1:$(free_port)
SQLITE_PATH=$work/feed.db JWT_SECRET=twelve-dollar-challenge HOST=127.0.0.1 PORT=$port PPROF_ADDR=$pprof \
  bash "$here/start.sh" >"$work/app.log" 2>&1 &
app_pid=$!
pids+=("$app_pid")
if ! wait_healthy "$url" "$app_pid"; then
  echo "The app did not become healthy. Last log lines:" >&2
  tail -n 40 "$work/app.log" >&2
  exit 1
fi

case "$action" in
  test)
    need jq
    need openssl
    NO_PROXY='*' no_proxy='*' bash "$root/test/test.sh" "$url"
    ;;
  benchmark)
    ensure_k6
    options=(--mode direct --url "$url" --k6 local --k6-bin "$K6_BIN"
      --tokens "$root/seed/tokens.json" --app-pid "$app_pid")
    [ "${PROFILE:-false}" = false ] || options+=(--pprof "$pprof")
    python3 "$here/tools/benchmark/benchmark.py" "${options[@]}" "$@" &
    child=$!
    wait "$child"
    ;;
  *)
    echo "Unknown action: $action" >&2
    exit 2
    ;;
esac
