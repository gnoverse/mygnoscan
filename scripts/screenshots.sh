#!/bin/sh
# Regenerate the screenshots used by README.md and docs/.
#
# Runs the real binary against a database snapshot with syncing disabled, so the
# output depends only on the fixture and not on whatever the live chain happened
# to be doing. Rerun after any UI change that alters these views.
#
#   DB=/path/to/mygnoscan.db ./scripts/screenshots.sh
#
# Requires Chrome (headless). Everything else is the project's own binary.
#
# The images committed under docs/images/ on 2026-09-25 were NOT made this way.
# They were captured headless against the live mainnet instance, because the
# only database with enough history to look like something is the 1.6 GB one on
# the server. Before that they were pearl-era captures of a top-nav UI that no
# longer exists, on the retired topaz network, which is what made the README's
# first impression a page nobody could load:
#
#   chrome --headless --hide-scrollbars --window-size=1400,900 \
#     --virtual-time-budget=15000 --screenshot=docs/images/<name>.png \
#     'https://mygnoscan.moul.p2p.team/<path>?network=mainnet'
#
# home.png is 1400x1600 rather than 900, because the hot realms and hot assets
# panels are the point of that page and both have to be in frame.
set -e

DB="${DB:-mygnoscan.db}"
OUT="${OUT:-docs/images}"
PORT="${PORT:-8899}"
# pearl, not topaz: topaz was retired months ago and its endpoints are NXDOMAIN,
# so the default selected a network the config no longer lists and the UI
# rendered an empty selector.
NETWORK="${NETWORK:-pearl}"
CONFIG="${CONFIG:-testdata/screenshots-networks.json}"
WIDTH="${WIDTH:-1400}"
HEIGHT="${HEIGHT:-900}"

CHROME="${CHROME:-/Applications/Google Chrome.app/Contents/MacOS/Google Chrome}"
[ -x "$CHROME" ] || CHROME="$(command -v chromium || command -v google-chrome || true)"
if [ -z "$CHROME" ] || [ ! -x "$CHROME" ]; then
  echo "error: no Chrome/Chromium found; set CHROME=/path/to/chrome" >&2
  exit 1
fi

if [ ! -f "$DB" ]; then
  echo "error: database not found: $DB (set DB=...)" >&2
  exit 1
fi

mkdir -p "$OUT"

echo "building..."
CGO_ENABLED=0 go build -o /tmp/mygnoscan-shots .

echo "starting server on :$PORT against $DB (sync disabled)"
/tmp/mygnoscan-shots -listen "127.0.0.1:$PORT" -db "$DB" -config "$CONFIG" -sync=false >/tmp/mygnoscan-shots.log 2>&1 &
SERVER_PID=$!
trap 'kill $SERVER_PID 2>/dev/null || true' EXIT

# Wait for it to answer rather than sleeping a guessed amount.
i=0
while [ $i -lt 50 ]; do
  if curl -fsS -o /dev/null "http://127.0.0.1:$PORT/api/version" 2>/dev/null; then break; fi
  i=$((i + 1))
  sleep 0.2
done

shot() {
  name="$1"
  path="$2"
  echo "  $name"
  # --virtual-time-budget lets the SPA finish fetching and rendering before capture.
  "$CHROME" --headless --disable-gpu --hide-scrollbars \
    --window-size="$WIDTH,$HEIGHT" \
    --virtual-time-budget=8000 \
    --screenshot="$OUT/$name.png" \
    "http://127.0.0.1:$PORT$path" >/dev/null 2>&1
}

echo "capturing to $OUT/"
shot home "/?network=$NETWORK"
shot realms "/realms?network=$NETWORK"
shot transactions "/txs?network=$NETWORK"
shot analytics "/analytics?network=$NETWORK"
shot blocks "/blocks?network=$NETWORK"

echo "done"
ls -la "$OUT"
