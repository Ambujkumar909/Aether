#!/usr/bin/env bash
# rccheck — measure what remote-control input actually costs on the wire.
#
# The Go tests count payload bytes. Payload bytes are not what the user pays
# for: a 5-byte mouse move riding its own TCP segment costs 5 bytes of payload
# and roughly a hundred bytes of link, IP, TCP and WebSocket framing. This runs
# a real control session through a real WebSocket while tshark captures, and
# reports every layer, so the claim can be checked rather than believed.
#
#   tools/rccheck.sh [port] [seconds] [batch|nobatch]
set -euo pipefail

PORT="${1:-8901}"
SECONDS_RUN="${2:-30}"
MODE="${3:-batch}"
WINDOW="${4:-15ms}"
EXTRA="${5:-}"
IFACE='\Device\NPF_Loopback'

TSHARK="${TSHARK:-/c/Program Files/Wireshark/tshark.exe}"
[ -x "$TSHARK" ] || TSHARK="$(command -v tshark)"
[ -n "$TSHARK" ] || { echo "tshark not found; set TSHARK" >&2; exit 1; }

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ENGINE="$HERE/../engine"
OUT="$(mktemp -d)"
PCAP="$OUT/rc.pcap"
trap 'rm -rf "$OUT"' EXIT

BATCH_FLAG="-batch=true"
[ "$MODE" = "nobatch" ] && BATCH_FLAG="-batch=false"

echo "capturing on $IFACE, tcp port $PORT, ${SECONDS_RUN}s, mode=$MODE window=$WINDOW"

"$TSHARK" -i "$IFACE" -f "tcp port $PORT" -w "$PCAP" -q >/dev/null 2>&1 &
TSHARK_PID=$!
sleep 2

( cd "$ENGINE" && go run ./cmd/rcbench -port "$PORT" -seconds "$SECONDS_RUN" -window "$WINDOW" $BATCH_FLAG $EXTRA ) \
  > "$OUT/app.json"

sleep 1
kill "$TSHARK_PID" 2>/dev/null || true
wait "$TSHARK_PID" 2>/dev/null || true

# Per-packet layer sizes, read from the capture rather than assumed.
"$TSHARK" -r "$PCAP" -T fields \
  -e frame.len -e ip.hdr_len -e ipv6.plen -e tcp.hdr_len -e tcp.len \
  -e websocket.payload_length \
  -E separator=, > "$OUT/fields.csv" 2>/dev/null

python "$HERE/rcreport.py" "$OUT/fields.csv" "$OUT/app.json" "$SECONDS_RUN" "$MODE window=$WINDOW"
