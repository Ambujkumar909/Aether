#!/usr/bin/env bash
# Browser harnesses on Linux (CI), each against a fresh process of the server
# on one throwaway database: sign-in and sign-up are rate-limited per process.
#
#   BIN=/tmp/aether tools/e2e/ci-browser.sh console layout messaging
set -u
BIN=${BIN:-/tmp/aether}
WORK=$(mktemp -d)
fails=0
# Email on, through a real SMTP server, as in production.
bash tools/e2e/mailpit.sh start
MAIL=(-smtp-addr 127.0.0.1:1025 -smtp-from noreply@aether.test -smtp-user aether -smtp-password e2e
  -smtp-ca tools/e2e/.mail/cert.pem -public-url https://localhost:9443)
for n in "$@"; do
  "$BIN" -addr :9443 -tls -secure-cookies -db "$WORK/e2e.db" -cert "$WORK/cert.pem" -key "$WORK/key.pem" \
    -web web -media-port 8811 "${MAIL[@]}" >"$WORK/srv-$n.log" 2>&1 &
  pid=$!
  for _ in $(seq 1 80); do curl -skf https://localhost:9443/healthz >/dev/null && break; sleep 0.25; done
  t0=$(date +%s)
  if node "tools/e2e/$n.e2e.mjs" https://localhost:9443 >"$WORK/log-$n.txt" 2>&1; then
    echo "ok    $n ($(grep -c '^ *ok' "$WORK/log-$n.txt") checks, $(( $(date +%s) - t0 ))s)"
  else
    echo "FAIL  $n"
    grep -E '^ *FAIL' "$WORK/log-$n.txt" | head -12
    tail -4 "$WORK/log-$n.txt"
    tail -5 "$WORK/srv-$n.log"
    fails=$((fails + 1))
  fi
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
done
echo "$fails of $# harnesses failed"
exit "$fails"
