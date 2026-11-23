# Obscura on Railway: the official Obscura image plus a small token gateway.
#
# Obscura 0.2.7 (h4ckf0r0day/obscura, Apache-2.0), official Docker Hub image,
# pinned by version and digest. Bump both together (see CHANGELOG.md).
# The gateway (./gateway, Go standard library only) listens on $PORT, checks
# OBSCURA_TOKEN, rewrites CDP discovery to the public wss:// address, serves
# MCP at /mcp and /healthz, and supervises both Obscura processes.

FROM golang:1.26.11-alpine3.24@sha256:ecb67f987aa1a57794c91d19ade7ae0209f2f0f5496aaef9e6ae85ddf92d02ab AS gateway
WORKDIR /src
COPY gateway/ ./
ENV CGO_ENABLED=0 GOFLAGS=-trimpath
RUN go vet ./... && go build -ldflags "-s -w -X main.version=0.2.7" -o /obscura-gateway .

FROM h4ckf0r0day/obscura:0.2.7@sha256:e70c2abc925f2194c9338762075432c69328032cae220d77393f1495e90c601a
COPY --from=gateway /obscura-gateway /obscura-gateway
# The gateway starts as root only to own the Railway volume at /data; it runs
# both Obscura processes as the image's unprivileged user (uid 65532).
USER 0
ENV PORT=8080 \
    RUST_LOG=warn,usvg=error \
    MALLOC_ARENA_MAX=2
EXPOSE 8080
ENTRYPOINT ["/obscura-gateway"]
