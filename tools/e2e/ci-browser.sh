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
  -smtp-ca tools/e2e/.mail/cert.pem -public-url https://localhost:9443
  -sso-issuer http://127.0.0.1:9460 -sso-client-id e2e-idp -sso-client-secret e2e-idp-secret
  -sso-name "Example Org" -sso-domains example.test
  -signup-check-mail=false -signup-block-domains tools/e2e/blocked-domains.txt -signup-per-address 20
  -admin-email admin@ci.e2e.test -admin-password "ci-$(date +%s%N)")
# The first administrator, as the native runner makes one: harnesses that
# act as an administrator (recording, twostep) sign in with these.
export AETHER_ADMIN_EMAIL=admin@ci.e2e.test AETHER_ADMIN_PASSWORD=${MAIL[-1]}
# The organisation's identity provider, faked (tools/e2e/fakeidp.mjs).
node tools/e2e/fakeidp.mjs 9460 >"$WORK/idp.log" 2>&1 &
# A push service, faked (tools/e2e/fakepush.mjs): where the server posts notices.
node tools/e2e/fakepush.mjs 9470 >"$WORK/push.log" 2>&1 &
fpush=$!
for _ in $(seq 1 40); do curl -sf http://127.0.0.1:9470/health >/dev/null && break; sleep 0.25; done
idp=$!
trap 'kill $idp $fpush 2>/dev/null' EXIT
for _ in $(seq 1 40); do curl -sf http://127.0.0.1:9460/keys >/dev/null && break; sleep 0.25; done
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
    # One annotation per failing harness: readable on the run page and over
    # the public API, where job logs are not.
    if [ -n "${GITHUB_ACTIONS:-}" ]; then
      msg=$( { grep -E '^ *FAIL' "$WORK/log-$n.txt" | head -6; tail -3 "$WORK/log-$n.txt"; tail -3 "$WORK/srv-$n.log"; } | tr '\n' '|' | cut -c1-900)
      echo "::error title=harness $n::$msg"
    fi
    tail -4 "$WORK/log-$n.txt"
    tail -5 "$WORK/srv-$n.log"
    fails=$((fails + 1))
  fi
  kill "$pid" 2>/dev/null
  wait "$pid" 2>/dev/null
done
echo "$fails of $# harnesses failed"
exit "$fails"
