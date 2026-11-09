# Aether: one server for meetings, calls and chat.
#
#   docker build -t aether .                 # the image
#   docker build --target test .             # the Go suite, on Linux, under -race
#   docker compose up -d                     # see compose.yaml and deploy/README.md
#
# The runtime image is distroless: no shell, no package manager, no root. The
# binary is static (pure-Go SQLite), and the web client is files served as-is.

# The toolchain go.mod pins (toolchain go1.26.10): the image builds with the
# same compiler, patched for every advisory govulncheck knows.
ARG GO_IMAGE=golang:1.26.10-bookworm

FROM ${GO_IMAGE} AS deps
WORKDIR /src/engine
COPY engine/go.mod engine/go.sum ./
RUN go mod download && go mod verify

FROM deps AS source
COPY engine/ ./

# The whole Go suite under the race detector, on Linux. A failing test fails
# the build of this target; the release image does not depend on it, so CI
# builds it explicitly.
FROM source AS test
ENV CGO_ENABLED=1
RUN go vet ./... && go test ./... -race -count=1

FROM source AS build
# The version every build carries. Unset, it is the build's UTC time
# (2026.1109.1735): dotted numbers that always grow, so a Windows app built
# into one image sees the next image as newer. A "dev" version would never
# update itself (cmd/aether-desktop/version.go), so no image ships one.
ARG VERSION=dev
RUN V="${VERSION}"; [ "$V" = dev ] && V="$(date -u +%Y.%m%d.%H%M)"; mkdir -p /out && echo "$V" > /out/version \
    && CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$V" -o /out/aether ./cmd/aether
# The Windows app, cross-compiled, with the manifest the app updates from
# (cmd/aether-desktop/update.go); served by the server at /desktop/.
RUN V="$(cat /out/version)" && mkdir -p /out/desktop \
    && GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath \
       -ldflags "-s -w -H windowsgui -X main.version=$V" -o /out/desktop/aether-desktop.exe ./cmd/aether-desktop \
    && cd /out/desktop \
    && printf '{"version":"%s","file":"aether-desktop.exe","sha256":"%s","size":%s}\n' \
       "$V" "$(sha256sum aether-desktop.exe | cut -d' ' -f1)" "$(stat -c %s aether-desktop.exe)" > latest.json
# The web client without what only development uses: node_modules, the unit
# tests and the package file.
COPY web/ /out/web/
RUN rm -rf /out/web/node_modules /out/web/package.json /out/web/package-lock.json \
    && find /out/web -name '*.test.mjs' -delete \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot AS runtime
COPY --from=build /out/aether /app/aether
COPY --from=build /out/web /app/web
COPY --from=build /out/desktop /app/desktop
COPY THIRD_PARTY_NOTICES.md /app/THIRD_PARTY_NOTICES.md
COPY --from=build --chown=65532:65532 /out/data /data
WORKDIR /app
USER 65532:65532

# (Docker's linter flags AETHER_KEY as a possible secret: it is a file path,
# the TLS key's location on the data volume, never a key.)
# Defaults for a container: unprivileged ports (compose maps 443 and 80 onto
# them), state on /data, the relay on a fixed range a firewall can open, and
# its TCP/443 path off because the web server holds 443. Every one can be
# overridden with an AETHER_ variable (see deploy/env.example).
ENV AETHER_ADDR=:8443 \
    AETHER_HTTP_ADDR=:8080 \
    AETHER_WEB=/app/web \
    AETHER_DB=/data/aether.db \
    AETHER_CERT=/data/tls/cert.pem \
    AETHER_KEY=/data/tls/key.pem \
    AETHER_TLS=true \
    AETHER_MEDIA_PORT=8801 \
    AETHER_RELAY_PORTS=49160-49359 \
    AETHER_RELAY_ALT_TCP=0 \
    AETHER_METRICS_ADDR=:9464 \
    AETHER_DESKTOP_DIR=/app/desktop

VOLUME ["/data"]
EXPOSE 8443/tcp 8080/tcp 8801/tcp 8801/udp 3478/tcp 3478/udp 49160-49359/udp

HEALTHCHECK --interval=30s --timeout=6s --start-period=20s --retries=3 \
    CMD ["/app/aether", "-healthcheck"]

ENTRYPOINT ["/app/aether"]
