FROM golang:1.26-bookworm AS builder

WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/guest-tunnel ./cmd/guest-tunnel \
    && CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -o /out/fido2-agent ./cmd/fido2-agent

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        autossh \
        bash \
        ca-certificates \
        curl \
        fail2ban \
        netcat-openbsd \
        openssh-client \
        openssh-server \
        procps \
        python3 \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /var/run/sshd /opt/guest-tunnel-test /work /etc/guest-tunnel /usr/local/libexec/guest-tunnel-test \
    && ssh-keygen -A

COPY --from=builder /out/guest-tunnel /usr/local/bin/guest-tunnel
COPY --from=builder /out/fido2-agent /usr/local/bin/fido2-agent
COPY test/docker /opt/guest-tunnel-test

RUN cp /opt/guest-tunnel-test/systemctl-shim.sh /usr/local/libexec/guest-tunnel-test/systemctl \
    && chmod +x \
    /opt/guest-tunnel-test/entrypoint.sh \
    /opt/guest-tunnel-test/client/smoke-test.sh \
    /opt/guest-tunnel-test/systemctl-shim.sh \
    /usr/local/libexec/guest-tunnel-test/systemctl
