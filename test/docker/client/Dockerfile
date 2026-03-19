FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/guest-tunnel ./cmd/guest-tunnel \
    && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/fido2-agent ./cmd/fido2-agent

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        netcat-openbsd \
        openssh-client \
        procps \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /out/guest-tunnel /usr/local/bin/guest-tunnel
COPY --from=builder /out/fido2-agent /usr/local/bin/fido2-agent
COPY test/docker /opt/guest-tunnel-test

RUN chmod +x /opt/guest-tunnel-test/client/smoke-test.sh
