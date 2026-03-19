#!/usr/bin/env bash
set -euo pipefail

workdir="${GT_WORKDIR:-/work}"
keys_dir="${workdir}/keys"
vps_host="${GT_VPS_HOST:-vps}"

if [[ ! -f "${keys_dir}/client_ed25519.pub" || ! -f "${keys_dir}/reverse_ed25519" ]]; then
  echo "missing test keys in ${keys_dir}" >&2
  exit 1
fi

if ! id -u tunneluser >/dev/null 2>&1; then
  useradd -m -s /bin/bash tunneluser
fi
passwd -d tunneluser >/dev/null 2>&1 || true
usermod -U tunneluser >/dev/null 2>&1 || true

install -d -m 700 -o tunneluser -g tunneluser /home/tunneluser/.ssh
cp "${keys_dir}/client_ed25519.pub" /home/tunneluser/.ssh/authorized_keys
chown tunneluser:tunneluser /home/tunneluser/.ssh/authorized_keys
chmod 600 /home/tunneluser/.ssh/authorized_keys

cat >/etc/ssh/sshd_config <<'EOF'
Port 22
ListenAddress 0.0.0.0
HostKey /etc/ssh/ssh_host_ed25519_key
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PermitRootLogin no
PubkeyAuthentication yes
AllowTcpForwarding yes
X11Forwarding no
AllowUsers tunneluser
PidFile /var/run/sshd.pid
LogLevel VERBOSE
LogLevel DEBUG3
EOF

mkdir -p /opt/guest-tunnel-test/home/www
cp /opt/guest-tunnel-test/home/index.html /opt/guest-tunnel-test/home/www/index.html
python3 -m http.server 8080 --bind 127.0.0.1 --directory /opt/guest-tunnel-test/home/www >/tmp/home-http.log 2>&1 &

start_reverse_tunnel() {
  while true; do
    ssh \
      -i "${keys_dir}/reverse_ed25519" \
      -o BatchMode=yes \
      -o ExitOnForwardFailure=yes \
      -o ServerAliveInterval=5 \
      -o ServerAliveCountMax=3 \
      -o StrictHostKeyChecking=no \
      -o UserKnownHostsFile=/dev/null \
      -N \
      -R 2222:localhost:22 \
      "jumpuser@${vps_host}" || true
    sleep 1
  done
}

if [[ "${GT_START_REVERSE_TUNNEL:-1}" == "1" ]]; then
  start_reverse_tunnel >/tmp/reverse-tunnel.log 2>&1 &
fi

exec /usr/sbin/sshd -D -e
