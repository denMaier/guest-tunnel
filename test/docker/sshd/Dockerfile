FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bash \
        ca-certificates \
        curl \
        netcat-openbsd \
        openssh-client \
        openssh-server \
        procps \
        python3 \
    && rm -rf /var/lib/apt/lists/*

RUN mkdir -p /var/run/sshd /opt/guest-tunnel-test /work \
    && ssh-keygen -A

COPY test/docker /opt/guest-tunnel-test

RUN chmod +x \
    /opt/guest-tunnel-test/vps/entrypoint.sh \
    /opt/guest-tunnel-test/home/entrypoint.sh
