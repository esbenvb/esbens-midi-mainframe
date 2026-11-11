#!/usr/bin/env bash
# Rebuild the user manual: screenshots of the web interface (simulated devices) and
# the PDF, docs/EMM-manual.pdf.
#
#   docs/manual/build.sh [--no-shots]
#
# --no-shots keeps the screenshots in docs/manual/img and only renders the PDF (after
# editing manual.html). Needs Go (to build emmd), Node 22 and Google Chrome; set CHROME
# to the browser binary if it is not the macOS default.
set -euo pipefail

HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
ROOT=$(cd "$HERE/../.." && pwd)
# shellcheck source=scripts/lib.sh
. "$ROOT/scripts/lib.sh"

SHOTS=1
for a in "$@"; do
  case "$a" in
    --no-shots) SHOTS=0 ;;
    -h|--help)  print_usage_comment "$0"; exit 0 ;;
    *)          die "unknown option: $a" ;;
  esac
done
have node || die "node not found (need Node 22)"

PID=""
TMP=$(mktemp -d)
cleanup() { [ -z "$PID" ] || kill "$PID" 2>/dev/null || true; rm -rf "$TMP" "$HERE/manual.build.html"; }
trap cleanup EXIT

if [ "$SHOTS" = 1 ]; then
  log "building emmd"
  (cd "$ROOT" && make build >/dev/null)
  PORT=18099
  log "starting a simulated emmd on port $PORT"
  "$ROOT/bin/emmd" --backend sim --netcfg fake --listen "127.0.0.1:$PORT" --ui-dir "$ROOT/internal/web/ui" \
    --state "$TMP/state.json" --boot-dir "" >"$TMP/emmd.log" 2>&1 &
  PID=$!
  for _ in $(seq 1 50); do curl -fs "http://127.0.0.1:$PORT/healthz" >/dev/null 2>&1 && break; sleep 0.2; done
  # The fake network stack starts in setup mode; join its pretend WiFi so the normal pages show.
  curl -fs -X POST -H 'Content-Type: application/json' -d '{"ssid":"HomeNet-5G","password":"whatever123"}' \
    "http://127.0.0.1:$PORT/api/network/wifi" >/dev/null
  for _ in $(seq 1 60); do
    curl -fs "http://127.0.0.1:$PORT/api/network" | grep -q '"setup_mode":false' && break; sleep 0.5
  done
  log "taking screenshots"
  node "$HERE/capture.mjs" shots "http://127.0.0.1:$PORT" "$HERE/img"
  kill "$PID" 2>/dev/null || true; PID=""
fi

VERSION=$(cd "$ROOT" && git describe --tags --always 2>/dev/null || echo dev)
DATE=$(date +%Y-%m-%d)
sed -e "s/{{VERSION}}/$VERSION/g" -e "s/{{DATE}}/$DATE/g" "$HERE/manual.html" >"$HERE/manual.build.html"
log "rendering the PDF"
node "$HERE/capture.mjs" pdf "$HERE/manual.build.html" "$ROOT/docs/EMM-manual.pdf" "Esben's MIDI Mainframe · user manual · $VERSION"
log "done: docs/EMM-manual.pdf"
