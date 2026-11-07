#!/usr/bin/env bash
# A real SMTP server for the harnesses: Mailpit, STARTTLS required, with a
# throwaway certificate (tools/e2e/.mail/cert.pem, the CA the server is told
# to trust with -smtp-ca). Messages are read back over its HTTP API.
#
#   tools/e2e/mailpit.sh start   # SMTP on 127.0.0.1:1025, API on 127.0.0.1:8025
#   tools/e2e/mailpit.sh stop
#
# Removes only what it created: the container aether-mailpit.
set -eu
export MSYS_NO_PATHCONV=1
hostpath() { cygpath -w "$1" 2>/dev/null || echo "$1"; }
DIR=$(cd "$(dirname "$0")" && pwd)/.mail
case "${1:-}" in
start)
  docker rm -f aether-mailpit >/dev/null 2>&1 || true
  mkdir -p "$DIR"
  if [ ! -s "$DIR/cert.pem" ]; then
    # Made readable inside the container: on Linux the files belong to root.
    docker run --rm --entrypoint sh -v "$(hostpath "$DIR"):/out" alpine/openssl -c \
      'openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:P-256 -nodes -days 3650 -subj /CN=localhost \
        -addext "subjectAltName=DNS:localhost,DNS:host.docker.internal,IP:127.0.0.1" \
        -keyout /out/key.pem -out /out/cert.pem 2>/dev/null && chmod 644 /out/key.pem /out/cert.pem'
  fi
  docker run -d --name aether-mailpit -p 127.0.0.1:1025:1025 -p 127.0.0.1:8025:8025 \
    -v "$(hostpath "$DIR"):/certs:ro" axllent/mailpit:v1.33.0 \
    --smtp-tls-cert /certs/cert.pem --smtp-tls-key /certs/key.pem --smtp-require-starttls \
    --smtp-auth-accept-any >/dev/null
  for _ in $(seq 1 40); do curl -sf http://127.0.0.1:8025/api/v1/info >/dev/null && exit 0; sleep 0.25; done
  echo "mailpit did not start" >&2; docker logs aether-mailpit >&2; exit 1 ;;
stop)
  docker rm -f aether-mailpit >/dev/null 2>&1 || true ;;
*)
  echo "usage: $0 start|stop" >&2; exit 2 ;;
esac
