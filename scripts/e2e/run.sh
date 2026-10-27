#!/usr/bin/env bash
set -euo pipefail

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$HERE/../.." && pwd)"
readonly HERE ROOT
readonly PORT="${GLD_E2E_PORT:-18419}"
readonly CDP_PORT="${GLD_E2E_CDP_PORT:-19229}"
readonly MAC_CHROME="/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"

usage() {
  cat <<EOF
Usage: $(basename "$0")   (no arguments)
Runs the spec 010 manual trigger e2e (AC8, AC11): builds gld, drives headless Chrome over CDP.
Env: GLD_E2E_CHROME (browser binary), GLD_E2E_PORT (default 18419), GLD_E2E_CDP_PORT (default 19229).
EOF
}

die() {
  echo "error: $*" >&2
  exit 1
}

case "$#:${1:-}" in
  0:) ;;
  1:-h | 1:--help) usage; exit 0 ;;
  *) usage >&2; exit 2 ;;
esac

for c in go node curl; do
  command -v "$c" >/dev/null || die "'$c' not found in PATH"
done
[[ "$(node -p 'process.versions.node.split(".")[0]')" -ge 22 ]] || die "node >= 22 required (global WebSocket), found $(node --version)"

find_chrome() {
  local c
  if [[ -n "${GLD_E2E_CHROME:-}" ]]; then
    command -v "$GLD_E2E_CHROME" || die "GLD_E2E_CHROME='$GLD_E2E_CHROME' is not an executable"
    return
  fi
  for c in google-chrome chromium chromium-browser; do
    command -v "$c" && return
  done
  [[ -x "$MAC_CHROME" ]] && { echo "$MAC_CHROME"; return; }
  die "no Chrome found (google-chrome, chromium, chromium-browser); set GLD_E2E_CHROME"
}
chrome="$(find_chrome)"
readonly chrome

port_free() {
  local rc=0
  curl -s -o /dev/null --max-time 1 "http://127.0.0.1:$1/" || rc=$?
  # 7 = connection refused; any other outcome means something listens there.
  [[ "$rc" -eq 7 ]]
}
for p in "$PORT" "$CDP_PORT"; do
  port_free "$p" || die "port $p already in use (set GLD_E2E_PORT / GLD_E2E_CDP_PORT)"
done

tmp="$(mktemp -d)"
readonly tmp

# Kills a recorded PID only while its command line still matches the expected glob: PIDs get
# reused, so it is checked again before every signal and every poll.
stop() {
  local f="$1" pattern="$2" pid
  [[ -f "$f" ]] || return 0
  pid="$(cat "$f")"
  alive() {
    local cmd
    cmd="$(ps -o command= -p "$pid")" || return 1
    # shellcheck disable=SC2053 # the pattern is a glob on purpose
    [[ "$cmd" == $pattern ]]
  }
  if ! alive; then
    if ps -p "$pid" >/dev/null; then
      echo "warning: pid $pid ($(basename "$f")) is now '$(ps -o command= -p "$pid")', not killed" >&2
    fi
    return 0
  fi
  local sig
  for sig in TERM KILL; do
    # KILL: Chrome may still be flushing its profile; rm -rf would race it and leak the temp dir.
    alive && kill -s "$sig" "$pid" 2>/dev/null || true
    for _ in 1 2 3 4 5 6 7 8 9 10; do
      alive || return 0
      sleep 0.3
    done
  done
  echo "warning: pid $pid ($(basename "$f")) still alive" >&2
}

cleanup() {
  stop "$tmp/sleep.pid" "sleep 60"
  stop "$tmp/server.pid" "$tmp/gld *"
  stop "$tmp/chrome.pid" "*--user-data-dir=$tmp/chrome *"
  rm -rf "$tmp"
  if [[ -e "$tmp" ]]; then echo "warning: temp dir left: $tmp" >&2; fi
}
trap cleanup EXIT

dump_logs() {
  echo "--- server log" >&2
  cat "$tmp/server.log" >&2
  echo "--- chrome log" >&2
  cat "$tmp/chrome.log" >&2
}

(cd "$ROOT" && go build -o "$tmp/gld" .) || die "go build failed"

mkdir -p "$tmp/docs" "$tmp/config" "$tmp/home" "$tmp/chrome"
printf '# Test\n\nThe quick brown fox jumps over the lazy dog.\n' >"$tmp/docs/test.md"
# Keeps the test's settings.json away from the user's real config; macOS ignores XDG, hence HOME.
export XDG_CONFIG_HOME="$tmp/config"

HOME="$tmp/home" "$tmp/gld" -b=false -H 127.0.0.1 -p "$PORT" "$tmp/docs/test.md" >"$tmp/server.log" 2>&1 &
echo "$!" >"$tmp/server.pid"
"$chrome" --headless=new --remote-debugging-port="$CDP_PORT" --user-data-dir="$tmp/chrome" \
  --no-first-run --no-default-browser-check --use-mock-keychain --password-store=basic \
  about:blank >"$tmp/chrome.log" 2>&1 &
echo "$!" >"$tmp/chrome.pid"

ready=0
for _ in $(seq 50); do
  if [[ "$(curl -sf "http://127.0.0.1:$CDP_PORT/json/list")" == *'"type": "page"'* ]] && curl -sf "http://127.0.0.1:$PORT/" >/dev/null; then
    ready=1
    break
  fi
  sleep 0.2
done
if [[ "$ready" -ne 1 ]]; then
  dump_logs
  die "server or chrome not ready after 10 s"
fi

if ! node "$HERE/manual-trigger.js" "$PORT" "$CDP_PORT" "$tmp"; then
  dump_logs
  exit 1
fi
