#!/usr/bin/env bash
set -euo pipefail

role="${GT_ROLE:-}"
workdir="${GT_WORKDIR:-/work}"
keys_dir="${workdir}/keys"
config_path="/etc/guest-tunnel/config.yml"
vps_host="${GT_VPS_HOST:-vps}"
tunnel_port="${GT_TUNNEL_PORT:-2222}"

require_file() {
  if [[ ! -f "$1" ]]; then
    echo "missing required file: $1" >&2
    exit 1
  fi
}

write_config() {
  local pubkey=""
  if [[ -f "${keys_dir}/client_ed25519.pub" ]]; then
    pubkey="$(cat "${keys_dir}/client_ed25519.pub")"
  fi

  cat >"${config_path}" <<EOF
vps_host: ${vps_host}
vps_user: jumpuser
vps_port: 22
home_user: tunneluser
tunnel_port: ${tunnel_port}
socks_port: 1080
socks_bind: 127.0.0.1
laptop_pubkey: ${pubkey}
ssh_daemon: openssh
skip_test: true
EOF
}

prepare_test_systemctl() {
  export PATH="/usr/local/libexec/guest-tunnel-test:${PATH}"
}

setup_vps() {
  require_file "${keys_dir}/client_ed25519.pub"
  require_file "${keys_dir}/reverse_ed25519.pub"

  write_config
  prepare_test_systemctl

  guest-tunnel --mode=server --config "${config_path}"

  install -d -m 700 -o jumpuser -g jumpuser /home/jumpuser/.ssh
  auth_keys="/home/jumpuser/.ssh/authorized_keys"
  touch "${auth_keys}"
  chmod 600 "${auth_keys}"
  chown jumpuser:jumpuser "${auth_keys}"
  if ! grep -Fq "$(cat "${keys_dir}/reverse_ed25519.pub")" "${auth_keys}"; then
    cat "${keys_dir}/reverse_ed25519.pub" >> "${auth_keys}"
  fi
  chown jumpuser:jumpuser "${auth_keys}"

  exec /usr/sbin/sshd -D -e
}

setup_home() {
  require_file "${keys_dir}/client_ed25519.pub"
  require_file "${keys_dir}/reverse_ed25519"
  require_file "${keys_dir}/reverse_ed25519.pub"

  write_config
  prepare_test_systemctl

  # Pre-install the test reverse tunnel key so guest-tunnel sees it
  # and skips key generation (keeps key consistent with VPS authorized_keys)
  install -d -m 700 -o root -g root /home/tunneluser/.ssh
  install -m 600 -o root -g root "${keys_dir}/reverse_ed25519" /home/tunneluser/.ssh/tunnel_ed25519
  install -m 644 -o root -g root "${keys_dir}/reverse_ed25519.pub" /home/tunneluser/.ssh/tunnel_ed25519.pub

  # Run the actual home setup — creates tunneluser, installs keys,
  # writes reverse-tunnel.service, starts autossh via systemctl shim
  guest-tunnel --mode=home --config "${config_path}"

  # Start HTTP server for smoke test (tunnel endpoint)
  mkdir -p /opt/guest-tunnel-test/home/www
  cp /opt/guest-tunnel-test/home/index.html /opt/guest-tunnel-test/home/www/index.html
  python3 -m http.server 8080 --bind 127.0.0.1 --directory /opt/guest-tunnel-test/home/www >/tmp/home-http.log 2>&1 &

  exec /usr/sbin/sshd -D -e
}

setup_client() {
  require_file "${keys_dir}/client_ed25519"
  install -d -m 700 /root/.ssh
  install -m 600 "${keys_dir}/client_ed25519" /root/.ssh/id_ed25519
  install -m 644 "${keys_dir}/client_ed25519.pub" /root/.ssh/id_ed25519.pub
  exec bash -lc "sleep infinity"
}

case "${role}" in
  vps)
    setup_vps
    ;;
  home)
    setup_home
    ;;
  client)
    setup_client
    ;;
  *)
    echo "unknown or missing GT_ROLE: ${role}" >&2
    exit 1
    ;;
esac
