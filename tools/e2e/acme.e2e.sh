#!/usr/bin/env bash
# Automatic certificates, end to end, before any real domain exists.
#
# Pebble (Let's Encrypt's own ACME test server) and its DNS test server run on
# a private Docker network; the production image obtains a certificate from it
# over the real ACME protocol, validated by real challenges against the
# container. Checks: the certificate served is issued by Pebble for the
# configured name; a forged SNI gets none; plain HTTP redirects to HTTPS on the
# named host only; a restart serves the cached certificate (no new order).
#
#   tools/e2e/acme.e2e.sh [image]          # default image aether:dev
#
# Removes only what it created: containers acme-e2e-*, network acme-e2e,
# volume acme-e2e-data.
set -u
# Git Bash on Windows rewrites container paths (/ca/...) into Windows ones;
# host paths are converted explicitly below instead (no-op on Linux).
export MSYS_NO_PATHCONV=1
hostpath() { cygpath -w "$1" 2>/dev/null || echo "$1"; }
IMAGE=${1:-aether:dev}
NET=acme-e2e
SUBNET=10.77.0.0/24
IP=10.77.0.10
NAME=meet.aether.test
fails=0
ok()   { echo "ok    $*"; }
bad()  { echo "FAIL  $*"; fails=$((fails+1)); }
cleanup() {
  docker rm -f acme-e2e-aether acme-e2e-dns pebble >/dev/null 2>&1
  docker network rm $NET >/dev/null 2>&1
  docker volume rm acme-e2e-data >/dev/null 2>&1
}
cleanup
trap cleanup EXIT

docker network create --subnet $SUBNET $NET >/dev/null
# DNS: every name resolves to the Aether container.
docker run -d --name acme-e2e-dns --network $NET ghcr.io/letsencrypt/pebble-challtestsrv \
  -defaultIPv4 $IP -defaultIPv6 "" -http01 "" -https01 "" -tlsalpn01 "" -doh "" >/dev/null
# The ACME server (its API certificate is valid for the name "pebble").
docker run -d --name pebble --network $NET -e PEBBLE_VA_NOSLEEP=1 ghcr.io/letsencrypt/pebble \
  -config test/config/pebble-config.json -dnsserver acme-e2e-dns:8053 >/dev/null
# Pebble validates HTTP-01 on 5002 and TLS-ALPN-01 on 5001, so Aether listens there.
CA=$(hostpath "$(mktemp -d)")
cid=$(docker create ghcr.io/letsencrypt/pebble) && docker cp "$cid:/test/certs/pebble.minica.pem" "$CA" >/dev/null && docker rm "$cid" >/dev/null
docker volume create acme-e2e-data >/dev/null
start_aether() {
  docker run -d --name acme-e2e-aether --network $NET --ip $IP \
    -v acme-e2e-data:/data -v "$CA:/ca:ro" \
    -e AETHER_ACME_DOMAIN=$NAME -e AETHER_ACME_EMAIL=ops@aether.test \
    -e AETHER_ACME_DIRECTORY=https://pebble:14000/dir -e AETHER_ACME_CA=/ca/pebble.minica.pem \
    -e AETHER_ADDR=:5001 -e AETHER_HTTP_ADDR=:5002 -e AETHER_RELAY=false -e AETHER_MEDIA_PORT=0 \
    -e AETHER_BACKUP_EVERY=0 \
    --read-only --tmpfs /tmp:size=16m --cap-drop ALL --security-opt no-new-privileges --user 65532:65532 \
    "$IMAGE" >/dev/null
}
start_aether
sleep 4

# One TLS handshake with SNI, from a client on the same network.
hello() { # $1 = server name; prints the leaf's subject, issuer, serial
  docker run --rm --network $NET alpine/openssl s_client -connect $IP:5001 -servername "$1" </dev/null 2>/dev/null |
    docker run --rm -i alpine/openssl x509 -noout -subject -issuer -serial 2>/dev/null
}
cert=""
for i in $(seq 1 30); do
  cert=$(hello $NAME)
  [ -n "$cert" ] && break
  [ "$(docker inspect -f '{{.State.Running}}' acme-e2e-aether 2>/dev/null)" = true ] || { echo "aether exited:"; docker logs acme-e2e-aether 2>&1 | tail -3; break; }
  sleep 2
done
echo "$cert" | sed 's/^/        /'
echo "$cert" | grep -q "subject=.*$NAME\|subject=$" && echo "$cert" | grep -qi "issuer=.*Pebble" \
  && ok "a certificate for $NAME was issued by the ACME authority" \
  || bad "no certificate from the ACME authority for $NAME"
docker run --rm --network $NET alpine/openssl s_client -connect $IP:5001 -servername $NAME </dev/null 2>/dev/null |
  docker run --rm -i alpine/openssl x509 -noout -ext subjectAltName 2>/dev/null | grep -q "DNS:$NAME" \
  && ok "it names $NAME" || bad "the certificate does not name $NAME"
serial=$(echo "$cert" | grep serial)

# A forged name gets nothing: the authority is never asked for it.
evil=$(hello evil.aether.test)
[ -z "$evil" ] && ok "a forged SNI (evil.aether.test) is refused a certificate" || bad "a forged SNI got a certificate: $evil"

# Plain HTTP: a redirect to HTTPS, only ever to the named host.
loc=$(docker run --rm --network $NET curlimages/curl -s -o /dev/null -w '%{http_code} %{redirect_url}' -H "Host: attacker.example" http://$IP:5002/room?x=1)
[ "$loc" = "301 https://$NAME/room?x=1" ] && ok "plain HTTP redirects to https://$NAME (a forged Host does not change it)" || bad "redirect was: $loc"

# Restart: the certificate comes from the cache, not a new order.
docker rm -f acme-e2e-aether >/dev/null && start_aether && sleep 4
again=""
for i in $(seq 1 15); do again=$(hello $NAME); [ -n "$again" ] && break; sleep 1; done
[ -n "$serial" ] && [ "$(echo "$again" | grep serial)" = "$serial" ] && ok "after a restart the same certificate is served from the cache" || bad "a restart changed the certificate ($serial vs $(echo "$again" | grep serial))"

# The container really is the hardened one.
docker inspect acme-e2e-aether --format '{{.HostConfig.ReadonlyRootfs}} {{.HostConfig.CapDrop}} {{.Config.User}}' | grep -q "true \[ALL\] 65532:65532" \
  && ok "served by the hardened container (read-only, no capabilities, uid 65532)" || bad "container not hardened"

[ $fails -eq 0 ] && echo "all passed" || {
  echo "$fails FAILED"
  echo "-- aether"; docker logs acme-e2e-aether 2>&1 | grep -v "missing certificate" | tail -12
  echo "-- pebble"; docker logs pebble 2>&1 | tail -20
}
exit $fails
