#!/usr/bin/env bash
set -euo pipefail

workdir="${GT_WORKDIR:-/work}"
keys_dir="${workdir}/keys"

if [[ ! -f "${keys_dir}/client_ed25519.pub" || ! -f "${keys_dir}/reverse_ed25519.pub" ]]; then
  echo "missing test public keys in ${keys_dir}" >&2
  exit 1
fi

if ! id -u jumpuser >/dev/null 2>&1; then
  useradd -m -s /bin/bash jumpuser
fi
passwd -d jumpuser >/dev/null 2>&1 || true
usermod -U jumpuser >/dev/null 2>&1 || true

install -d -m 700 -o jumpuser -g jumpuser /home/jumpuser/.ssh
cat "${keys_dir}/client_ed25519.pub" "${keys_dir}/reverse_ed25519.pub" > /home/jumpuser/.ssh/authorized_keys
chown jumpuser:jumpuser /home/jumpuser/.ssh/authorized_keys
chmod 600 /home/jumpuser/.ssh/authorized_keys

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
GatewayPorts no
X11Forwarding no
AllowUsers jumpuser
PidFile /var/run/sshd.pid
LogLevel VERBOSE
LogLevel DEBUG3
EOF

exec /usr/sbin/sshd -D -e
